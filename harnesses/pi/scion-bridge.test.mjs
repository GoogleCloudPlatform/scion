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
import scionBridge, { createHandlers } from './home/.pi/agent/extensions/scion-bridge.js';

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
  assert.deepEqual(last.argv, ['status', 'task_completed', 'All done. Two lines.']);
});

test('summary is truncated and argv-safe (no shell)', () => {
  const { calls, h } = harness();
  h.agent_end({ messages: [asst('$(touch /tmp/x); `id` ' + 'y'.repeat(500))] });
  h.agent_settled({ aborted: false });
  const argv = calls[calls.length - 1].argv;
  assert.equal(argv.length, 3);
  assert.ok(argv[2].length <= 200);
  assert.ok(argv[2].startsWith('$(touch /tmp/x);'));
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
  assert.deepEqual(calls[calls.length - 1].argv, ['status', 'task_completed', 'fine']);
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
  assert.equal(calls[calls.length - 1].argv[2], 'Task completed');
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
    ['agent_end', 'agent_settled', 'input', 'session_start', 'tool_execution_end', 'tool_execution_start'],
  );
  assert.ok(registered.every((r) => r[1] === 'function'));
});
