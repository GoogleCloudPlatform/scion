// Scion bridge extension for pi.
//
// Loaded by pi from ~/.pi/agent/extensions/ (user-level, so it loads
// regardless of project trust). It forwards pi lifecycle events to
// `sciontool hook --dialect=pi` (see dialect.yaml) and signals
// `task_completed` when a run has fully settled.
//
// Every call is best-effort and synchronous with a short timeout; a failing
// or missing sciontool never throws into pi. Arguments are passed as an argv
// array (never through a shell). No per-token work is done, so a streaming
// response spawns no processes.
//
// Environment:
//   SCION_PI_NO_AUTO_COMPLETE=1  do not signal task_completed on settle
//   SCION_HOOK_DEBUG=1           log bridge failures to stderr

import { execFileSync } from 'node:child_process';

const HOOK_TIMEOUT_MS = 5000;
const MAX_TOOL_INPUT_BYTES = 8192;
const MAX_ERROR_CHARS = 200;
const MAX_SUMMARY_CHARS = 200;

function defaultRun(args, input) {
  execFileSync('sciontool', args, {
    input,
    stdio: ['pipe', 'ignore', 'ignore'],
    timeout: HOOK_TIMEOUT_MS,
  });
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
      // Only human/RPC input counts as a prompt; extension-injected input
      // would inflate max_turns accounting. The prompt text is not forwarded.
      if (event && event.source === 'extension') return;
      emit('input');
    },

    tool_execution_start(event) {
      if (!event) return;
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

    agent_settled(event) {
      // An aborted run (Escape) or a provider error is not a completed task.
      const failed = lastRunFailed;
      lastRunFailed = false;
      if (!event || event.aborted || failed) return;
      if (env.SCION_PI_NO_AUTO_COMPLETE === '1') return;
      const summary = truncate(assistantText(lastAssistantMessage(lastMessages)), MAX_SUMMARY_CHARS);
      safe('task_completed', () =>
        run(['status', 'task_completed', summary || 'Task completed']),
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
}
