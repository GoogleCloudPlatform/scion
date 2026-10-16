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
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import scionBridge, { createHandlers, trustPolicy } from './home/.pi/agent/extensions/scion-bridge.js';

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
    ['agent_end', 'agent_settled', 'input', 'project_trust', 'session_start', 'tool_execution_end', 'tool_execution_start'],
  );
  assert.ok(registered.every((r) => r[1] === 'function'));
});

// --- agent-set sticky status -------------------------------------------------

const bashStart = (command) => ({ toolName: 'bash', args: { command } });
const statusCalls = (calls) => calls.filter((c) => c.argv[0] === 'status');

test('settle does not overwrite an agent-set ask_user / blocked / limits_exceeded', () => {
  for (const kind of ['ask_user', 'blocked', 'limits_exceeded']) {
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

test('other bash commands (even mentioning sciontool) do not trip the guard', () => {
  const { calls, h } = harness();
  h.tool_execution_start(bashStart('ls; sciontool status task_completed "x"'));
  h.tool_execution_start({ toolName: 'write', args: { command: 'sciontool status blocked' } });
  h.agent_end({ messages: [asst('ok')] });
  h.agent_settled({ aborted: false });
  assert.equal(statusCalls(calls).length, 1);
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
