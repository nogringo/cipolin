package requestpolicy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
)

const (
	defaultRejectMessage = "blocked: rejected by request policy plugin"
	pluginErrorMessage   = "blocked: request policy plugin error"
)

type Config struct {
	Command  string
	Timeout  time.Duration
	FailOpen bool
}

type PluginEngine struct {
	mu            sync.Mutex
	cfg           Config
	logger        *log.Logger
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	stdout        *bufio.Reader
	scriptPath    string
	scriptModTime time.Time
	seq           uint64
}

type inputMessage struct {
	Type        string       `json:"type"`
	ID          string       `json:"id"`
	Filter      nostr.Filter `json:"filter"`
	ReceivedAt  int64        `json:"receivedAt"`
	SourceType  string       `json:"sourceType"`
	SourceInfo  string       `json:"sourceInfo,omitempty"`
	Authed      *string      `json:"authed,omitempty"`
	RequestType string       `json:"requestType,omitempty"`
}

type outputMessage struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Msg    string `json:"msg,omitempty"`
}

func NewPluginEngine(cfg Config, logger *log.Logger) *PluginEngine {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 1500 * time.Millisecond
	}

	if logger == nil {
		logger = log.Default()
	}

	engine := &PluginEngine{
		cfg:    cfg,
		logger: logger,
	}

	if looksLikeScriptPath(cfg.Command) {
		engine.scriptPath = cfg.Command
	}

	return engine
}

func (p *PluginEngine) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
}

// EvaluateRequest executes the configured plugin and returns whether the request must be rejected.
func (p *PluginEngine) EvaluateRequest(ctx context.Context, filter nostr.Filter, requestType string) (bool, string) {
	if p.cfg.Command == "" {
		return false, ""
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.reloadIfChangedLocked(); err != nil {
		p.logger.Printf("request policy plugin reload check failed: %v", err)
		return p.onPluginFailureLocked(err)
	}

	if err := p.ensureStartedLocked(); err != nil {
		p.logger.Printf("request policy plugin start failed: %v", err)
		return p.onPluginFailureLocked(err)
	}

	id := fmt.Sprintf("req-%d-%d", time.Now().UnixNano(), atomic.AddUint64(&p.seq, 1))
	msg := p.buildInputMessage(ctx, filter, requestType, id)

	payload, err := json.Marshal(msg)
	if err != nil {
		return true, pluginErrorMessage
	}
	payload = append(payload, '\n')

	response, err := p.requestLocked(payload)
	if err != nil {
		p.logger.Printf("request policy plugin evaluation failed: %v", err)
		p.stopLocked()
		return p.onPluginFailureLocked(err)
	}

	if response.ID != id {
		p.logger.Printf("request policy plugin returned mismatched id=%q (expected %q)", response.ID, id)
		return p.onPluginFailureLocked(errors.New("plugin returned mismatched id"))
	}

	switch response.Action {
	case "accept":
		return false, ""
	case "reject":
		if response.Msg == "" {
			return true, defaultRejectMessage
		}
		return true, response.Msg
	case "shadowReject":
		// Keep this distinct from a regular reject by omitting a response message.
		return true, ""
	default:
		p.logger.Printf("request policy plugin returned unknown action=%q", response.Action)
		return p.onPluginFailureLocked(errors.New("plugin returned unknown action"))
	}
}

func (p *PluginEngine) buildInputMessage(ctx context.Context, filter nostr.Filter, requestType, id string) inputMessage {
	sourceType, sourceInfo := sourceFromContext(ctx)

	msg := inputMessage{
		Type:        "new",
		ID:          id,
		Filter:      filter,
		ReceivedAt:  time.Now().Unix(),
		SourceType:  sourceType,
		SourceInfo:  sourceInfo,
		RequestType: requestType,
	}

	if authed, ok := khatru.GetAuthed(ctx); ok {
		authedHex := authed.Hex()
		msg.Authed = &authedHex
	}

	return msg
}

func sourceFromContext(ctx context.Context) (string, string) {
	ip := khatru.GetIP(ctx)
	if ip == "" {
		return "Unknown", ""
	}

	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "Unknown", ip
	}

	if parsed.To4() != nil {
		return "IP4", ip
	}

	return "IP6", ip
}

func (p *PluginEngine) onPluginFailureLocked(err error) (bool, string) {
	if p.cfg.FailOpen {
		return false, ""
	}

	if err != nil {
		return true, pluginErrorMessage
	}

	return true, pluginErrorMessage
}

func (p *PluginEngine) requestLocked(payload []byte) (outputMessage, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := p.stdin.Write(payload); err != nil {
			p.stopLocked()
			if err := p.ensureStartedLocked(); err != nil {
				return outputMessage{}, err
			}
			continue
		}

		line, err := p.readLineWithTimeoutLocked(p.cfg.Timeout)
		if err != nil {
			return outputMessage{}, err
		}

		line = strings.TrimSpace(line)
		if line == "" {
			return outputMessage{}, errors.New("plugin returned an empty line")
		}

		var response outputMessage
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			return outputMessage{}, fmt.Errorf("invalid plugin json: %w", err)
		}

		return response, nil
	}

	return outputMessage{}, errors.New("plugin write failed after retries")
}

func (p *PluginEngine) readLineWithTimeoutLocked(timeout time.Duration) (string, error) {
	type result struct {
		line string
		err  error
	}

	ch := make(chan result, 1)
	go func() {
		line, err := p.stdout.ReadString('\n')
		ch <- result{line: line, err: err}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			return "", res.err
		}
		return res.line, nil
	case <-time.After(timeout):
		p.stopLocked()
		return "", fmt.Errorf("plugin timed out after %s", timeout)
	}
}

func (p *PluginEngine) reloadIfChangedLocked() error {
	if p.scriptPath == "" || p.cmd == nil {
		return nil
	}

	st, err := os.Stat(p.scriptPath)
	if err != nil {
		return err
	}

	if st.ModTime().After(p.scriptModTime) {
		p.stopLocked()
		p.scriptModTime = st.ModTime()
	}

	return nil
}

func (p *PluginEngine) ensureStartedLocked() error {
	if p.cmd != nil {
		return nil
	}

	var cmd *exec.Cmd
	if looksLikeScriptPath(p.cfg.Command) {
		cmd = exec.Command(p.cfg.Command)
		st, err := os.Stat(p.cfg.Command)
		if err != nil {
			return err
		}
		p.scriptModTime = st.ModTime()
	} else {
		cmd = exec.Command("sh", "-c", p.cfg.Command)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}

	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return err
	}

	p.cmd = cmd
	p.stdin = stdin
	p.stdout = bufio.NewReader(stdout)

	go func() {
		err := cmd.Wait()
		if err != nil {
			p.logger.Printf("request policy plugin exited: %v", err)
		}
	}()

	return nil
}

func (p *PluginEngine) stopLocked() {
	if p.stdin != nil {
		_ = p.stdin.Close()
	}

	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}

	p.cmd = nil
	p.stdin = nil
	p.stdout = nil
}

func looksLikeScriptPath(command string) bool {
	return command != "" && !strings.ContainsAny(command, " \t")
}
