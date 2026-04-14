#!/usr/bin/env node

const readline = require('readline');

const allowedSet = new Set(
  (process.env.ALLOW_PUBKEYS || '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
);

const ratePerMinute = Number(process.env.RATE_LIMIT_PER_MIN || '60');
const requireAuth = (process.env.REQUIRE_AUTH || 'true').toLowerCase() !== 'false';

const buckets = new Map();

function takeToken(identity, nowMs) {
  const key = identity || 'anon';
  const refillPerMs = ratePerMinute / 60000;
  const maxTokens = Math.max(1, ratePerMinute);

  let bucket = buckets.get(key);
  if (!bucket) {
    bucket = { tokens: maxTokens, updatedAt: nowMs };
    buckets.set(key, bucket);
  }

  const elapsed = Math.max(0, nowMs - bucket.updatedAt);
  bucket.tokens = Math.min(maxTokens, bucket.tokens + elapsed * refillPerMs);
  bucket.updatedAt = nowMs;

  if (bucket.tokens >= 1) {
    bucket.tokens -= 1;
    return true;
  }

  return false;
}

const rl = readline.createInterface({
  input: process.stdin,
  output: process.stdout,
  terminal: false,
});

rl.on('line', (line) => {
  let req;
  try {
    req = JSON.parse(line);
  } catch (err) {
    console.error(`invalid request json: ${err.message}`);
    return;
  }

  const res = { id: req.id };

  if (req.type !== 'new') {
    res.action = 'reject';
    res.msg = 'blocked: unsupported request type';
    console.log(JSON.stringify(res));
    return;
  }

  const authed = req.authed;
  if (requireAuth && !authed) {
    res.action = 'reject';
    res.msg = 'auth-required: authenticate with NIP-42 first';
    console.log(JSON.stringify(res));
    return;
  }

  if (authed && !allowedSet.has(authed)) {
    res.action = 'reject';
    res.msg = 'blocked: pubkey is not allowed';
    console.log(JSON.stringify(res));
    return;
  }

  const identity = authed || req.sourceInfo || 'unknown';
  if (!takeToken(identity, Date.now())) {
    res.action = 'reject';
    res.msg = 'rate-limited: too many requests';
    console.log(JSON.stringify(res));
    return;
  }

  res.action = 'accept';
  console.log(JSON.stringify(res));
});
