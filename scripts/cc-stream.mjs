#!/usr/bin/env node
/**
 * cc-stream.mjs — tether streaming sidecar for claude-code
 *
 * Protocol (stdin → stdout, all newline-delimited JSON):
 *   stdin:  {"prompt":"<text>","sessionId":"<id or empty>","workdir":"<path>"}
 *   stdout: {"type":"session_id","sessionId":"<id>"}
 *           {"type":"text","text":"<chunk>"}
 *           {"type":"tool_use","id":"<id>","name":"<name>","input":{...}}
 *           {"type":"result","stopReason":"<reason>"}
 *           {"type":"error","message":"<msg>"}
 *
 * Spawned once per tether session. Reads one prompt per line, streams events
 * back. Stays alive for follow-up prompts in the same session.
 */

import { createInterface } from 'readline';

// Resolve the SDK from the cloudcli installation (already on this machine).
// Falls back to locally installed package if available.
const SDK_PATHS = [
  '/usr/lib/node_modules/@cloudcli-ai/cloudcli/node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs',
];

let query;
for (const p of SDK_PATHS) {
  try {
    const mod = await import(p);
    query = mod.query;
    break;
  } catch { /* try next */ }
}

if (!query) {
  process.stdout.write(JSON.stringify({ type: 'error', message: 'claude-agent-sdk not found; install @cloudcli-ai/cloudcli' }) + '\n');
  process.exit(1);
}

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n');
}

// Keep track of session across prompts
let currentSessionId = '';

const rl = createInterface({ input: process.stdin, crlfDelay: Infinity });

for await (const line of rl) {
  const trimmed = line.trim();
  if (!trimmed) continue;

  let req;
  try {
    req = JSON.parse(trimmed);
  } catch {
    send({ type: 'error', message: 'invalid JSON on stdin: ' + trimmed.slice(0, 80) });
    continue;
  }

  const { prompt, sessionId, workdir } = req;
  if (!prompt) continue;

  // Use provided sessionId or the one captured from previous turn
  const resumeId = sessionId || currentSessionId || undefined;

  const options = {
    ...(resumeId ? { resume: resumeId } : {}),
    ...(workdir ? { cwd: workdir } : {}),
    // Always fire PreToolUse hooks so tether permission UI activates
    permissionMode: 'default',
    outputFormat: 'stream-json',
  };

  try {
    const iter = query({ prompt, options });
    let sessionEmitted = false;

    for await (const msg of iter) {
      // Capture session ID on first message
      if (msg.session_id && !sessionEmitted) {
        currentSessionId = msg.session_id;
        send({ type: 'session_id', sessionId: currentSessionId });
        sessionEmitted = true;
      }

      switch (msg.type) {
        case 'assistant': {
          // Content blocks: text and tool_use
          for (const block of msg.message?.content ?? []) {
            if (block.type === 'text' && block.text) {
              send({ type: 'text', text: block.text });
            } else if (block.type === 'tool_use') {
              send({ type: 'tool_use', id: block.id, name: block.name, input: block.input });
            }
          }
          break;
        }
        case 'result':
          send({ type: 'result', stopReason: msg.subtype ?? 'stop' });
          break;
        case 'system':
          // init event — session_id already handled above
          break;
        default:
          // forward unknown types for debugging
          break;
      }
    }

    if (!sessionEmitted) {
      // No messages at all — still emit result so Go side unblocks
      send({ type: 'result', stopReason: 'stop' });
    }
  } catch (err) {
    send({ type: 'error', message: String(err?.message ?? err) });
    send({ type: 'result', stopReason: 'error' });
  }
}
