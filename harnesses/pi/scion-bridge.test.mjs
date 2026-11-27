// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Tests for the pi scion-bridge extension. Run: node --test scion-bridge.test.mjs
// Lives beside config.yaml (not under home/.pi/agent/extensions/) so pi never
// tries to load it as an extension.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import scionBridge, {
  createHandlers,
  trustPolicy,
  makeRunner,
  timeoutFor,
  HOOK_TIMEOUT_MS,
  STATUS_TIMEOUT_MS,
} from './home/.pi/agent/extensions/scion-bridge.js';

function harness(env = {}, { failing = false } = {}) {
  const calls = [];
  const run = (argv, stdin) => {
    calls.push({ argv, stdin: stdin === undefined ? undefined : JSON.parse(stdin) });
    if (failing) throw new Error('sciontool missing');
  };
  return { calls, h: createHandlers({ run, env }) };
}

const asst = (text, extra = {}) => ({
  role: 'assistant',
  content: [{ type: 'text', text }],
  stopReason: 'stop',
  ...extra,
});

test('session_start forwards the reason via the pi dialect', () => {
  const { calls, h } = harness();
  h.session_start({ type: 'session_start', reason: 'startup' });
  assert.deepEqual(calls[0].argv, ['hook', '--dialect=pi']);
  assert.deepEqual(calls[0].stdin, { hook_event_name: 'session_start', reason: 'startup' });
});

test('input counts human input but never forwards the prompt text', () => {
  const { calls, h } = harness();
  h.input({ type: 'input', text: 'secret prompt', source: 'interactive' });
  h.input({ type: 'input', text: 'x', source: 'rpc' });
  h.input({ type: 'input', text: 'x', source: 'extension' });
  assert.equal(calls.length, 2);
  assert.deepEqual(calls[0].stdin, { hook_event_name: 'input' });
  assert.ok(!JSON.stringify(calls).includes('secret prompt'));
});

test('tool events carry name, bounded input and success', () => {
  const { calls, h } = harness();
  h.tool_execution_start({ toolName: 'bash', args: { command: 'ls' } });
  h.tool_execution_start({ toolName: 'write', args: { content: 'x'.repeat(20000) } });
  h.tool_execution_end({ toolName: 'bash', isError: true });
  assert.deepEqual(calls[0].stdin.tool_input, { command: 'ls' });
  assert.equal(calls[1].stdin.tool_input, undefined);
  assert.deepEqual(calls[2].stdin, { hook_event_name: 'tool_execution_end', tool_name: 'bash', success: false });
});

test('settled, not aborted, signals task_completed with the final answer', () => {
  const { calls, h } = harness();
  h.agent_end({ messages: [asst('first'), asst('All done.\n  Two lines.')] });
  h.agent_settled({ type: 'agent_settled', aborted: false });
  const last = calls[calls.length - 1];
  assert.deepEqual(last.argv, ['status', 'task_completed', '--', 'All done. Two lines.']);
});

test('summary is truncated and passed as one argv element', () => {
  const { calls, h } = harness();
  h.agent_end({ messages: [asst('$(touch /tmp/x); `id` ' + 'y'.repeat(500))] });
  h.agent_settled({ aborted: false });
  const argv = calls[calls.length - 1].argv;
  assert.equal(argv.length, 4);
  assert.equal(argv[2], '--');
  assert.ok(argv[3].length <= 200);
  assert.ok(argv[3].startsWith('$(touch /tmp/x);'));
});

test('aborted run does not signal task_completed', () => {
  const { calls, h } = harness();
  h.agent_end({ messages: [asst('partial', { stopReason: 'aborted' })] });
  h.agent_settled({ aborted: true });
  assert.ok(!calls.some((c) => c.argv[0] === 'status'));
});

test('provider error is reported on agent-end and suppresses task_completed', () => {
  const { calls, h } = harness();
  h.agent_end({ messages: [asst('', { stopReason: 'error', errorMessage: '401 invalid x-api-key' })] });
  h.agent_settled({ aborted: false });
  assert.equal(calls[0].stdin.error, '401 invalid x-api-key');
  assert.ok(!calls.some((c) => c.argv[0] === 'status'));
});

test('a failure does not leak into the next successful run', () => {
  const { calls, h } = harness();
  h.agent_end({ messages: [asst('', { stopReason: 'error', errorMessage: 'boom' })] });
  h.agent_settled({ aborted: false });
  h.agent_end({ messages: [asst('fine')] });
  h.agent_settled({ aborted: false });
  assert.deepEqual(calls[calls.length - 1].argv, ['status', 'task_completed', '--', 'fine']);
});

test('SCION_PI_NO_AUTO_COMPLETE=1 disables task_completed only', () => {
  const { calls, h } = harness({ SCION_PI_NO_AUTO_COMPLETE: '1' });
  h.agent_end({ messages: [asst('ok')] });
  h.agent_settled({ aborted: false });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].argv[0], 'hook');
});

test('empty final answer falls back to a default message', () => {
  const { calls, h } = harness();
  h.agent_end({ messages: [] });
  h.agent_settled({ aborted: false });
  assert.equal(calls[calls.length - 1].argv[3], 'Task completed');
});

// --- sciontool dialect fixture ----------------------------------------------------
//
// pkg/sciontool/hooks/dialects/testdata/pi/bridge-payloads.jsonl holds the
// exact stdin this bridge writes to `sciontool hook --dialect=pi` for the
// scripted session below. The pi event objects are synthetic, shaped like pi
// 1.3.0's extension event types (not captured from a live pi). The Go test
// pkg/sciontool/hooks/handlers/pi_dialect_test.go parses the same file
// through harnesses/pi/dialect.yaml, so a change to the bridge's payloads
// fails here until the fixture is regenerated, and then fails there if the
// dialect no longer maps it. Regenerate with:
//   SCION_PI_UPDATE_FIXTURE=1 node --test scion-bridge.test.mjs

const FIXTURE = join(
  dirname(fileURLToPath(import.meta.url)),
  '..', '..', 'pkg', 'sciontool', 'hooks', 'dialects', 'testdata', 'pi', 'bridge-payloads.jsonl',
);

const BRIDGE_SESSION = [
  ['session_start', { type: 'session_start', reason: 'startup' }],
  ['input', { type: 'input', text: 'list the files', source: 'interactive' }],
  ['message_end', {
    type: 'message_end',
    message: {
      role: 'assistant',
      content: [{ type: 'text', text: 'Checking files.' }],
      api: 'openai-completions',
      provider: 'mockprov',
      model: 'mock-model',
      responseModel: 'mock-model-2026',
      responseId: 'chatcmpl-mock-1',
      usage: { input: 380, output: 40, cacheRead: 120, cacheWrite: 25, reasoning: 15, totalTokens: 565 },
      stopReason: 'toolUse',
      timestamp: 1795719212534,
    },
  }],
  ['tool_execution_start', { type: 'tool_execution_start', toolCallId: 't1', toolName: 'bash', args: { command: 'ls -la', timeout: 30 } }],
  ['tool_execution_end', { type: 'tool_execution_end', toolCallId: 't1', toolName: 'bash', result: {}, isError: false }],
  ['tool_execution_start', { type: 'tool_execution_start', toolCallId: 't2', toolName: 'read', args: { path: 'missing.txt' } }],
  ['tool_execution_end', { type: 'tool_execution_end', toolCallId: 't2', toolName: 'read', result: {}, isError: true }],
  ['agent_end', { type: 'agent_end', messages: [asst('Here are the files.')] }],
  ['input', { type: 'input', text: 'again', source: 'rpc' }],
  ['message_end', {
    type: 'message_end',
    message: {
      role: 'assistant',
      content: [],
      api: 'openai-completions',
      provider: 'mockprov',
      model: 'mock-model',
      usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0 },
      stopReason: 'error',
      errorMessage: '429 rate limited: retry later',
      timestamp: 1795719213601,
    },
  }],
  ['agent_end', { type: 'agent_end', messages: [asst('', { stopReason: 'error', errorMessage: '429 rate limited: retry later' })] }],
  ['session_start', { type: 'session_start', reason: 'resume' }],
  ['session_start', { type: 'session_start', reason: 'reload' }],
];

test('bridge hook payloads match the sciontool dialect fixture', () => {
  const lines = [];
  const h = createHandlers({
    run: (argv, stdin) => {
      if (argv[0] === 'hook') lines.push(stdin);
    },
    env: {},
  });
  for (const [ev, payload] of BRIDGE_SESSION) h[ev](payload);
  const got = lines.join('\n') + '\n';
  if (process.env.SCION_PI_UPDATE_FIXTURE === '1') writeFileSync(FIXTURE, got);
  assert.equal(readFileSync(FIXTURE, 'utf8'), got);
});

// --- timeouts ------------------------------------------------------------------

test('status calls get a longer timeout than hook calls (Hub update needs >5 s)', () => {
  assert.equal(HOOK_TIMEOUT_MS, 5000);
  assert.ok(STATUS_TIMEOUT_MS >= 10000, `status timeout ${STATUS_TIMEOUT_MS} < 10 s`);
  assert.ok(STATUS_TIMEOUT_MS > HOOK_TIMEOUT_MS);
  assert.equal(timeoutFor(['status', 'task_completed', '--', 'x']), STATUS_TIMEOUT_MS);
  assert.equal(timeoutFor(['hook', '--dialect=pi']), HOOK_TIMEOUT_MS);
});

test('the default runner passes the per-command timeout to execFileSync', () => {
  const execs = [];
  const exec = (file, args, opts) => execs.push({ file, args, opts });
  const h = createHandlers({ run: makeRunner(exec), env: {} });
  h.tool_execution_start({ toolName: 'bash', args: { command: 'ls' } });
  h.agent_end({ messages: [asst('done')] });
  h.agent_settled({ aborted: false });
  assert.deepEqual(
    execs.map((e) => [e.file, e.args[0], e.opts.timeout]),
    [
      ['sciontool', 'hook', HOOK_TIMEOUT_MS],
      ['sciontool', 'hook', HOOK_TIMEOUT_MS],
      ['sciontool', 'status', STATUS_TIMEOUT_MS],
    ],
  );
  assert.deepEqual(execs[2].args, ['status', 'task_completed', '--', 'done']);
  assert.deepEqual(execs[0].opts.stdio, ['pipe', 'ignore', 'ignore']);
  assert.equal(JSON.parse(execs[0].opts.input).hook_event_name, 'tool_execution_start');
});

test('a failing sciontool never throws into pi', () => {
  const { h } = harness({}, { failing: true });
  assert.doesNotThrow(() => {
    h.session_start({ reason: 'startup' });
    h.input({ source: 'interactive' });
    h.tool_execution_start({ toolName: 'bash', args: {} });
    h.tool_execution_end({ toolName: 'bash', isError: false });
    h.agent_end({ messages: [asst('x')] });
    h.agent_settled({ aborted: false });
    h.agent_settled(undefined);
  });
});

test('default export registers exactly the expected pi events and starts nothing', () => {
  const registered = [];
  scionBridge({ on: (name, fn) => registered.push([name, typeof fn]) });
  assert.deepEqual(
    registered.map((r) => r[0]).sort(),
    ['agent_end', 'agent_settled', 'input', 'message_end', 'project_trust', 'session_start', 'tool_execution_end', 'tool_execution_start'],
  );
  assert.ok(registered.every((r) => r[1] === 'function'));
});

// --- message_end model usage -------------------------------------------------

test('message_end emits canonical token fields, responseModel over model, and request identity', () => {
  const { calls, h } = harness();
  // Wire shape captured from pinned @earendil-works/pi-coding-agent@1.3.0
  // against a local mock OpenAI-compatible server (scrubbed).
  h.message_end({
    type: 'message_end',
    message: {
      role: 'assistant',
      content: [{ type: 'text', text: 'Hello from mock.' }],
      api: 'openai-completions',
      provider: 'mockprov',
      model: 'mock-model',
      responseModel: 'mock-model-2026',
      responseId: 'chatcmpl-mock-1',
      id: 'msg-1',
      usage: {
        input: 380,
        output: 40,
        cacheRead: 120,
        cacheWrite: 25,
        reasoning: 15,
        totalTokens: 565,
        cost: { input: 0.00038, output: 0.00008, cacheRead: 0.000012, cacheWrite: 0.00003, total: 0.000502 },
      },
      stopReason: 'stop',
      timestamp: 1795719212534,
    },
  });
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0].argv, ['hook', '--dialect=pi']);
  assert.deepEqual(calls[0].stdin, {
    hook_event_name: 'message_end',
    message_id: 'msg-1',
    response_id: 'chatcmpl-mock-1',
    timestamp: 1795719212534,
    model: 'mockprov/mock-model-2026',
    input_tokens: 380,
    output_tokens: 40,
    cached_tokens: 120,
    cache_write_tokens: 25,
    reasoning_tokens: 15,
  });
});

test('message_end preserves measured zeros and omits unknown, missing, negative, or non-finite token fields', () => {
  const { calls, h } = harness();
  h.message_end({
    type: 'message_end',
    message: {
      role: 'assistant',
      provider: 'anthropic',
      model: 'claude-sonnet-4-5',
      usage: { input: 100, output: 0, cacheRead: undefined, cacheWrite: null, reasoning: Number.NaN },
      stopReason: 'stop',
      timestamp: 1795719212500,
    },
  });
  h.message_end({
    type: 'message_end',
    message: {
      role: 'assistant',
      model: 'anthropic/claude-sonnet-4-5',
      provider: 'anthropic',
      stopReason: 'stop',
      timestamp: 1795719212600,
    },
  });
  h.message_end({
    type: 'message_end',
    message: {
      role: 'assistant',
      provider: 'anthropic',
      model: 'claude-sonnet-4-5',
      usage: { input: 100, output: 20, cacheRead: 0, cacheWrite: 0, reasoning: -1 },
      stopReason: 'stop',
      timestamp: 1795719212700,
    },
  });
  assert.equal(calls.length, 3);
  assert.deepEqual(calls[0].stdin, {
    hook_event_name: 'message_end',
    timestamp: 1795719212500,
    model: 'anthropic/claude-sonnet-4-5',
    input_tokens: 100,
    output_tokens: 0,
  });
  assert.ok(!Object.hasOwn(calls[0].stdin, 'cached_tokens'));
  assert.ok(!Object.hasOwn(calls[0].stdin, 'cache_write_tokens'));
  assert.ok(!Object.hasOwn(calls[0].stdin, 'reasoning_tokens'));
  // Already provider-prefixed model is not double-prefixed; absent usage emits no token keys.
  assert.deepEqual(calls[1].stdin, {
    hook_event_name: 'message_end',
    timestamp: 1795719212600,
    model: 'anthropic/claude-sonnet-4-5',
  });
  // Measured cache zeros are preserved; negative values are omitted.
  assert.deepEqual(calls[2].stdin, {
    hook_event_name: 'message_end',
    timestamp: 1795719212700,
    model: 'anthropic/claude-sonnet-4-5',
    input_tokens: 100,
    output_tokens: 20,
    cached_tokens: 0,
    cache_write_tokens: 0,
  });
  assert.ok(!Object.hasOwn(calls[2].stdin, 'reasoning_tokens'));
});

test('message_end ignores non-assistant messages and non-terminal stopReasons', () => {
  const { calls, h } = harness();
  h.message_end(undefined);
  h.message_end({ type: 'message_end', message: { role: 'user', content: 'hi', timestamp: 1 } });
  h.message_end({ type: 'message_end', message: { role: 'system', content: '', timestamp: 2 } });
  h.message_end({ type: 'message_end', message: { role: 'toolResult', toolCallId: 't1', usage: { input: 5, output: 5 }, timestamp: 3 } });
  h.message_end({ type: 'message_end', message: { role: 'assistant', stopReason: 'pending', usage: { input: 5, output: 5 }, timestamp: 4 } });
  h.message_end({ type: 'message_end', message: { role: 'assistant', stopReason: 'deferred', usage: { input: 5, output: 5 }, timestamp: 5 } });
  assert.equal(calls.length, 0);
});

test('message_end deduplicates repeated deliveries on (id, responseId, timestamp) without collapsing distinct calls', () => {
  const { calls, h } = harness();
  const base = {
    role: 'assistant',
    provider: 'mockprov',
    model: 'mock-model',
    responseId: 'resp-1',
    usage: { input: 10, output: 5, cacheRead: 0, cacheWrite: 0 },
    stopReason: 'stop',
    timestamp: 1795719212000,
  };
  h.message_end({ type: 'message_end', message: base });
  h.message_end({ type: 'message_end', message: { ...base } });
  // Identical token counts on a distinct request (new responseId/timestamp) must still count.
  h.message_end({ type: 'message_end', message: { ...base, responseId: 'resp-2', timestamp: 1795719212999 } });
  assert.equal(calls.length, 2);
  assert.equal(calls[0].stdin.response_id, 'resp-1');
  assert.equal(calls[1].stdin.response_id, 'resp-2');
});

test('message_end carries bounded error for error and aborted outcomes and omits EMPTY_USAGE zeros', () => {
  const { calls, h } = harness();
  h.message_end({
    type: 'message_end',
    message: {
      role: 'assistant',
      provider: 'mockprov',
      model: 'mock-model',
      usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0 },
      stopReason: 'error',
      errorMessage: '400: ' + 'x'.repeat(300),
      timestamp: 1795719213601,
    },
  });
  h.message_end({
    type: 'message_end',
    message: {
      role: 'assistant',
      provider: 'mockprov',
      model: 'mock-model',
      usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0 },
      stopReason: 'aborted',
      timestamp: 1795719213700,
    },
  });
  assert.equal(calls.length, 2);
  assert.ok(calls[0].stdin.error.startsWith('400: '));
  assert.ok(calls[0].stdin.error.length <= 200);
  assert.ok(!Object.hasOwn(calls[0].stdin, 'input_tokens'));
  assert.equal(calls[1].stdin.error, 'aborted');
  assert.ok(!Object.hasOwn(calls[1].stdin, 'output_tokens'));
});

// --- agent-set sticky status -------------------------------------------------

const bashStart = (command) => ({ toolName: 'bash', args: { command } });
const statusCalls = (calls) => calls.filter((c) => c.argv[0] === 'status');

test('settle does not overwrite an agent-set ask_user / blocked / idle / limits_exceeded', () => {
  for (const kind of ['ask_user', 'blocked', 'idle', 'limits_exceeded']) {
    const { calls, h } = harness();
    h.tool_execution_start(bashStart(`sciontool status ${kind} "need input"`));
    h.agent_end({ messages: [asst('waiting')] });
    h.agent_settled({ aborted: false });
    assert.equal(statusCalls(calls).length, 0, kind);
  }
});

test('the sticky guard resets on the next human input', () => {
  const { calls, h } = harness();
  h.tool_execution_start(bashStart('sciontool status blocked "x"'));
  h.agent_end({ messages: [asst('a')] });
  h.agent_settled({ aborted: false });
  h.input({ source: 'interactive' });
  h.agent_end({ messages: [asst('b')] });
  h.agent_settled({ aborted: false });
  assert.deepEqual(statusCalls(calls).map((c) => c.argv[3]), ['b']);
});

const settledStatusCalls = (cmd, tool = 'bash') => {
  const { calls, h } = harness();
  h.tool_execution_start({ toolName: tool, args: { command: cmd } });
  h.agent_end({ messages: [asst('chat text tail')] });
  h.agent_settled({ aborted: false });
  return statusCalls(calls).length;
};

test('a status call in command position (plain or compound) suppresses the automatic task_completed', () => {
  for (const cmd of [
    'sciontool status task_completed "Fixed the bug"',
    'make test && sciontool status task_completed "done"',
    'cd /workspace; sciontool status blocked "waiting on CI"',
    'git push && sciontool status idle "nothing assigned"',
    'false || sciontool status ask_user "which branch?"',
    'go test ./... | tee log.txt & sciontool status limits_exceeded "x"',
    'make build\nsciontool status task_completed "built"',
    'if make test; then sciontool status task_completed "green"; fi',
    '/usr/local/bin/sciontool status blocked "y"',
    'out=$(sciontool status task_completed "z")',
    '(sciontool status task_completed "subshell")',
    '{ sciontool status blocked "group"; }',
    'echo `sciontool status task_completed "backtick"`',
    'for i in 1; do sciontool status ask_user "loop"; done',
    'if false; then :; else sciontool status blocked "else"; fi',
    '  sciontool  status  task_completed  "spaced"',
  ]) {
    assert.equal(settledStatusCalls(cmd), 0, cmd);
  }
});

test('commands that only mention a status call, other sciontool commands and non-bash tools do not trip the guard', () => {
  for (const [cmd, tool] of [
    ['grep -rn "sciontool status task_completed" .', 'bash'],
    ["echo 'then run sciontool status blocked'", 'bash'],
    ['cat notes.md | grep "sciontool status ask_user"', 'bash'],
    ['rg sciontool\\s+status\\s+limits_exceeded harnesses/', 'bash'],
    ['grep -n "sciontool status" README.md', 'bash'],
    ['sciontool hook --dialect=pi', 'bash'],
    ['sciontool status --help', 'bash'],
    ['mysciontool status blocked "x"', 'bash'],
    ['sciontool status blocked', 'write'],
    ['sciontool status task_completed "x"', 'write'],
  ]) {
    assert.equal(settledStatusCalls(cmd, tool), 1, `${tool}: ${cmd}`);
  }
});

// Known limits of text matching, pinned so a change is deliberate (README
// "Lifecycle signals" documents them).
test('known limits: quoted separators and heredoc lines count; wrapped calls do not', () => {
  for (const cmd of [
    'git commit -m "fix (sciontool status blocked handling)"',
    'echo "x; sciontool status blocked"',
    "cat <<'EOF' > notes.md\nsciontool status task_completed \"later\"\nEOF",
  ]) {
    assert.equal(settledStatusCalls(cmd), 0, `false positive: ${cmd}`);
  }
  for (const cmd of [
    'env X=1 sciontool status blocked "x"',
    'timeout 10 sciontool status blocked "x"',
    'sudo sciontool status blocked "x"',
    "bash -c 'sciontool status blocked x'",
  ]) {
    assert.equal(settledStatusCalls(cmd), 1, `false negative: ${cmd}`);
  }
});

// --- project trust -----------------------------------------------------------

test('project_trust handler enforces the policy; default and invalid are never', () => {
  const decide = (env) => createHandlers({ run: () => {}, env }).project_trust();
  assert.deepEqual(decide({}), { trusted: 'no' });
  assert.deepEqual(decide({ SCION_PI_PROJECT_TRUST: '' }), { trusted: 'no' });
  assert.deepEqual(decide({ SCION_PI_PROJECT_TRUST: 'bogus' }), { trusted: 'no' });
  assert.deepEqual(decide({ SCION_PI_PROJECT_TRUST: 'never' }), { trusted: 'no' });
  assert.deepEqual(decide({ SCION_PI_PROJECT_TRUST: ' ALWAYS ' }), { trusted: 'yes' });
  assert.deepEqual(decide({ SCION_PI_PROJECT_TRUST: 'ask' }), { trusted: 'undecided' });
  assert.equal(trustPolicy({ SCION_PI_PROJECT_TRUST: 'toString' }), 'never');
});

test('project_trust never remembers a decision (no trust.json write)', () => {
  const r = createHandlers({ run: () => {}, env: { SCION_PI_PROJECT_TRUST: 'always' } }).project_trust();
  assert.equal(r.remember, undefined);
});

test('the extension registers a project_trust handler', () => {
  const registered = {};
  scionBridge({ on: (name, fn) => { registered[name] = fn; } });
  assert.equal(typeof registered.project_trust, 'function');
  const prev = process.env.SCION_PI_PROJECT_TRUST;
  delete process.env.SCION_PI_PROJECT_TRUST;
  try {
    assert.deepEqual(registered.project_trust({ type: 'project_trust', cwd: '/x' }, {}), { trusted: 'no' });
  } finally {
    if (prev !== undefined) process.env.SCION_PI_PROJECT_TRUST = prev;
  }
});

// --- real sciontool (opt-in: SCIONTOOL_BIN=/path/to/sciontool) ----------------

const REAL = process.env.SCIONTOOL_BIN;

function realSciontool() {
  const home = mkdtempSync(join(tmpdir(), 'pi-bridge-'));
  const env = { PATH: `${join(REAL, '..')}:/usr/bin:/bin`, HOME: home };
  const run = (argv, stdin) =>
    execFileSync(REAL, argv, { input: stdin, env, stdio: ['pipe', 'pipe', 'pipe'], timeout: 10000 });
  const activity = () => JSON.parse(readFileSync(join(home, 'agent-info.json'), 'utf8')).activity;
  const set = (...argv) => run(argv);
  return { home, run, activity, set, done: () => rmSync(home, { recursive: true, force: true }) };
}

test('real sciontool: ask_user then settle stays waiting_for_input', { skip: !REAL }, () => {
  const r = realSciontool();
  try {
    const h = createHandlers({ run: r.run, env: {} });
    h.tool_execution_start(bashStart('sciontool status ask_user "which?"'));
    r.set('status', 'ask_user', 'which?');
    h.agent_end({ messages: [asst('which?')] });
    h.agent_settled({ aborted: false });
    assert.equal(r.activity(), 'waiting_for_input');
  } finally { r.done(); }
});

test('real sciontool: blocked then settle stays blocked', { skip: !REAL }, () => {
  const r = realSciontool();
  try {
    const h = createHandlers({ run: r.run, env: {} });
    h.tool_execution_start(bashStart('sciontool status blocked "no creds"'));
    r.set('status', 'blocked', 'no creds');
    h.agent_end({ messages: [asst('blocked')] });
    h.agent_settled({ aborted: false });
    assert.equal(r.activity(), 'blocked');
  } finally { r.done(); }
});

test('real sciontool: idle then settle stays idle', { skip: !REAL }, () => {
  const r = realSciontool();
  try {
    const h = createHandlers({ run: r.run, env: {} });
    h.tool_execution_start(bashStart('sciontool status idle "nothing assigned"'));
    r.set('status', 'idle', 'nothing assigned');
    h.agent_end({ messages: [asst('idle')] });
    h.agent_settled({ aborted: false });
    assert.equal(r.activity(), 'idle');
  } finally { r.done(); }
});

test('real sciontool: dash-leading and flag-like summaries still record completed', { skip: !REAL }, () => {
  for (const text of ['- Done: fixed bug', '--- summary', '--help', '-h']) {
    const r = realSciontool();
    try {
      const h = createHandlers({ run: r.run, env: { SCION_HOOK_DEBUG: '1' } });
      r.set('status', 'blocked', 'pre');
      h.agent_end({ messages: [asst(text)] });
      h.agent_settled({ aborted: false });
      assert.equal(r.activity(), 'completed', text);
    } finally { r.done(); }
  }
});
