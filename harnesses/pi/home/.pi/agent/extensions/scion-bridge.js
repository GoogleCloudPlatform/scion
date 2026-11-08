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

// Scion bridge extension for pi.
//
// Loaded by pi from ~/.pi/agent/extensions/ (user-level, so it loads
// regardless of project trust). It forwards pi lifecycle events to
// `sciontool hook --dialect=pi` (see dialect.yaml) and signals
// `task_completed` when a run has fully settled.
//
// Every call is best-effort and synchronous with a timeout (5 s for hook
// events, 15 s for status, which waits on the Hub); a failing
// or missing sciontool never throws into pi. Arguments are passed as an argv
// array (never through a shell). No per-token work is done, so a streaming
// response spawns no processes.
//
// Environment:
//   SCION_PI_NO_AUTO_COMPLETE=1  do not signal task_completed on settle
//   SCION_PI_PROJECT_TRUST       never (default) | ask | always; enforced via
//                                the project_trust event, which pi consults
//                                before saved trust.json decisions
//   SCION_HOOK_DEBUG=1           log bridge failures to stderr

import { execFileSync } from 'node:child_process';

// Per-event `sciontool hook` calls may also call the Hub, but sciontool bounds
// those calls with a 3 s Hub budget, so 5 s covers them. `sciontool status`
// sends the status to the Hub with its own 5 s deadline, so its process gets
// a longer budget: killing it at 5 s could cut off the Hub update for
// task_completed and the coordinator would never be notified.
export const HOOK_TIMEOUT_MS = 5000;
export const STATUS_TIMEOUT_MS = 15000;
const MAX_TOOL_INPUT_BYTES = 8192;
const MAX_ERROR_CHARS = 200;
const MAX_SUMMARY_CHARS = 200;

export function timeoutFor(args) {
  return args[0] === 'status' ? STATUS_TIMEOUT_MS : HOOK_TIMEOUT_MS;
}

// makeRunner builds the sciontool runner; `exec` is injectable for tests.
export function makeRunner(exec = execFileSync) {
  return (args, input) =>
    exec('sciontool', args, {
      input,
      stdio: ['pipe', 'ignore', 'ignore'],
      timeout: timeoutFor(args),
    });
}

const defaultRun = makeRunner();

// Statuses the agent sets itself through its bash tool; they must not be
// overwritten by the automatic task_completed on settle. An agent-run
// task_completed counts too: the bridge's own call would replace the agent's
// summary with the tail of the chat text.
// The call must be in command position: at the start of the command or of a
// line, after ; & | (so && and ||), ( { $( or a backtick, optionally after
// then/do/else, and optionally with a path (/usr/local/bin/sciontool). A
// command that only mentions it (grep "sciontool status blocked", echo ...)
// does not count. Shell quoting is not parsed: a separator inside quotes
// still matches, e.g. echo "x; sciontool status blocked" counts.
const STICKY_STATUS_RE =
  /(?:^|[;&|\n({`])\s*(?:(?:then|do|else)\s+)?(?:[^\s;&|()`'"]*\/)?sciontool\s+status\s+(?:ask_user|blocked|limits_exceeded|task_completed)\b/;

const TRUST_DECISIONS = { never: 'no', always: 'yes', ask: 'undecided' };

// Effective project-trust policy: the env value, else (also for any invalid
// value) the safe default "never".
export function trustPolicy(env = process.env) {
  const v = String(env.SCION_PI_PROJECT_TRUST || '').trim().toLowerCase();
  return Object.hasOwn(TRUST_DECISIONS, v) ? v : 'never';
}

function lastAssistantMessage(messages) {
  if (!Array.isArray(messages)) return undefined;
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (m && m.role === 'assistant') return m;
  }
  return undefined;
}

function assistantText(message) {
  if (!message) return '';
  const c = message.content;
  if (typeof c === 'string') return c;
  if (!Array.isArray(c)) return '';
  return c
    .filter((b) => b && b.type === 'text' && typeof b.text === 'string')
    .map((b) => b.text)
    .join('\n');
}

function truncate(text, max) {
  const oneLine = String(text).replace(/\s+/g, ' ').trim();
  return oneLine.length > max ? `${oneLine.slice(0, max - 1)}…` : oneLine;
}

function boundedToolInput(args) {
  if (args === undefined) return undefined;
  try {
    return JSON.stringify(args).length <= MAX_TOOL_INPUT_BYTES ? args : undefined;
  } catch {
    return undefined;
  }
}

// createHandlers returns the pi event handlers. `run(argv, stdin)` is
// injectable for tests; it may throw, which is swallowed.
export function createHandlers({ run = defaultRun, env = process.env } = {}) {
  // The most recent run's outcome, set on agent_end and consumed on
  // agent_settled (which fires after agent_end once pi will not continue).
  let lastRunFailed = false;
  let lastMessages;
  // Set when the agent ran `sciontool status ask_user|blocked|limits_exceeded|task_completed`
  // since the last human/RPC input (reset in input()).
  let agentSetStatus = false;

  const safe = (what, fn) => {
    try {
      fn();
    } catch (err) {
      if (env.SCION_HOOK_DEBUG) {
        console.error(`[scion-bridge] ${what}: ${err && err.message}`);
      }
    }
  };

  const emit = (name, data = {}) =>
    safe(name, () =>
      run(['hook', '--dialect=pi'], JSON.stringify({ hook_event_name: name, ...data })),
    );

  return {
    session_start(event) {
      emit('session_start', { reason: event && event.reason });
    },

    input(event) {
      // Only human/RPC input is forwarded as prompt-submit; skipping
      // extension-injected input avoids extra prompt-submit/THINKING status
      // updates. (Turns for max_turns are counted at agent_end, not here.)
      // The prompt text is not forwarded.
      if (event && event.source === 'extension') return;
      agentSetStatus = false;
      emit('input');
    },

    tool_execution_start(event) {
      if (!event) return;
      const cmd = event.args && event.args.command;
      if (event.toolName === 'bash' && typeof cmd === 'string' && STICKY_STATUS_RE.test(cmd)) {
        agentSetStatus = true;
      }
      emit('tool_execution_start', {
        tool_name: event.toolName,
        tool_input: boundedToolInput(event.args),
      });
    },

    tool_execution_end(event) {
      if (!event) return;
      emit('tool_execution_end', {
        tool_name: event.toolName,
        success: !event.isError,
      });
    },

    agent_end(event) {
      lastMessages = event && event.messages;
      const last = lastAssistantMessage(lastMessages);
      lastRunFailed = Boolean(last && last.stopReason === 'error');
      const data = {};
      if (lastRunFailed) {
        data.error = truncate(last.errorMessage || 'provider error', MAX_ERROR_CHARS);
      }
      emit('agent_end', data);
    },

    project_trust() {
      return { trusted: TRUST_DECISIONS[trustPolicy(env)] };
    },

    agent_settled(event) {
      // An aborted run (Escape) or a provider error is not a completed task.
      const failed = lastRunFailed;
      lastRunFailed = false;
      if (!event || event.aborted || failed) return;
      if (env.SCION_PI_NO_AUTO_COMPLETE === '1') return;
      if (agentSetStatus) return;
      const summary = truncate(assistantText(lastAssistantMessage(lastMessages)), MAX_SUMMARY_CHARS);
      safe('task_completed', () =>
        // "--" keeps a summary that starts with "-" from being parsed as a flag.
        run(['status', 'task_completed', '--', summary || 'Task completed']),
      );
    },
  };
}

// pi calls the default export with its ExtensionAPI. Nothing is started here;
// handlers only run in response to events.
export default function scionBridge(pi) {
  const h = createHandlers();
  pi.on('session_start', (e) => h.session_start(e));
  pi.on('input', (e) => h.input(e));
  pi.on('tool_execution_start', (e) => h.tool_execution_start(e));
  pi.on('tool_execution_end', (e) => h.tool_execution_end(e));
  pi.on('agent_end', (e) => h.agent_end(e));
  pi.on('agent_settled', (e) => h.agent_settled(e));
  pi.on('project_trust', () => h.project_trust());
}
