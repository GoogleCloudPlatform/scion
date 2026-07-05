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

/**
 * OpenCode plugin that bridges OpenCode events to Scion's hook/status system.
 *
 * When running inside a Scion container (SCION_AGENT_ID is set), this plugin
 * intercepts OpenCode plugin events and forwards them to `sciontool hook` so
 * that the Scion Hub can track agent status in real-time.
 *
 * Event mapping:
 *   session.created          -> session-start      (activity: working)
 *   session.idle             -> (no event)          (activity: preserved)
 *   session.error            -> session-end        (activity: stopped)
 *   session.deleted          -> session-end        (activity: stopped)
 *   tool.execute.before      -> tool-start         (activity: executing)
 *   tool.execute.after       -> tool-end           (activity: working)
 *   message.updated (user)   -> prompt-submit      (activity: thinking)
 *   message.updated (assistant) -> model-start     (activity: thinking)
 *   permission.asked         -> notification       (activity: waiting_for_input)
 *   tui.command.execute      -> prompt-submit      (activity: thinking)
 */

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const DEBOUNCE_MS = 200          // batch rapid events (tools)
const MSG_DEBOUNCE_MS = 2000     // longer debounce for streaming messages
const HEARTBEAT_INTERVAL_MS = 45_000  // 45s -- fires before 5min stalled threshold
const HOOK_TIMEOUT_MS = 5000     // max wait for sciontool hook to respond

// Sticky activities that should not be overwritten by normal events
const STICKY_ACTIVITIES = new Set([
  "waiting_for_input",
  "blocked",
  "completed",
  "limits_exceeded",
  "crashed",
])

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/** Safely parse JSON string, returning null on failure. */
function safeJSON(str) {
  try {
    return JSON.parse(str)
  } catch {
    return null
  }
}

/** Truncate a string to maxLen characters. */
function truncate(str, maxLen) {
  if (!str || str.length <= maxLen) return str
  return str.slice(0, maxLen - 3) + "..."
}

/** Escape a string for safe embedding in a shell single-quoted argument. */
function shellEscape(str) {
  if (!str) return ""
  // Single-quote everything; internal single quotes become '\''
  return "'" + str.replace(/'/g, "'\\''") + "'"
}

/**
 * Send a normalized hook event to sciontool via stdin.
 *
 * Uses a temp file to pass JSON to sciontool, avoiding shell quoting issues
 * with here-strings and pipes. The JSON payload matches the format expected
 * by sciontool hook --dialect=opencode.
 */
async function sendHook(client, name, data = {}, logTag = "") {
  const agentId = process.env.SCION_AGENT_ID
  if (!agentId) return // Not in a Scion container

  const payload = JSON.stringify({ name, data })

  try {
    await client.app.log({
      body: {
        service: "scion-plugin",
        level: "debug",
        message: `scion hook: ${name}`,
        extra: { name, data: JSON.stringify(data).slice(0, 500), tag: logTag },
      },
    })

    // Write JSON to a temp file and pipe to sciontool -- avoids shell quoting
    // issues with here-strings (<<<) that may not work in all environments.
    const fs = await import("fs")
    const tmpPath = `/tmp/scion-hook-${Date.now()}-${Math.random().toString(36).slice(2)}.json`
    fs.writeFileSync(tmpPath, payload)

    // Fire-and-forget: don't await to avoid blocking the event handler.
    // Log errors to the app logger for visibility (not just console.log).
    const { execFile } = await import("child_process")
    execFile(
      "timeout",
      [String(HOOK_TIMEOUT_MS), "sh", "-c", `cat $1 | sciontool hook --dialect=opencode 2>/dev/null; rm -f $1`, "_", tmpPath],
      { timeout: HOOK_TIMEOUT_MS + 1000, stdio: "ignore" },
      (err) => {
        if (err) {
          client.app.log({
            body: {
              service: "scion-plugin",
              level: "error",
              message: `scion hook failed: ${name}`,
              extra: { error: String(err)?.message || String(err), name },
            },
          }).catch(() => {})
        }
      },
    )
  } catch (err) {
    await client.app.log({
      body: {
        service: "scion-plugin",
        level: "warn",
        message: `scion hook send failed: ${name}`,
        extra: { error: String(err) },
      },
    })
  }
}

/**
 * Send an activity update to sciontool hook.
 * This is a convenience wrapper that sets the activity field in the event data.
 */
async function sendActivity(client, activity, extraData = {}) {
  return sendHook(client, "_activity", { activity, ...extraData })
}

/** Check if the current activity is sticky (from local agent-info.json). */
async function isSticky(client) {
  try {
    const info = safeJSON(
      await client.$`cat ${process.env.HOME}/agent-info.json 2>/dev/null`.stdout
    )
    if (info && info.activity) {
      return STICKY_ACTIVITIES.has(info.activity)
    }
  } catch {
    // Ignore -- file may not exist yet
  }
  return false
}

/**
 * Debounce wrapper: batches rapid calls into a single execution after a
 * cooldown period. Returns a function that, when called, resets the timer.
 */
function debounce(fn, ms) {
  let timer = null
  return (...args) => {
    if (timer) clearTimeout(timer)
    timer = setTimeout(() => fn(...args), ms)
  }
}

// ---------------------------------------------------------------------------
// Heartbeat
// ---------------------------------------------------------------------------

/**
 * Send a heartbeat to keep the agent from being marked as stalled.
 *
 * Scion's stalled detection triggers after 5 minutes of no activity events.
 * We fire a lightweight heartbeat every 45 seconds to stay well under that
 * threshold. Heartbeats are suppressed when the agent is in a sticky state
 * (waiting_for_input, completed, etc.) because those are intentional pauses.
 */
async function startHeartbeat(client) {
  let timer = null

  const tick = async () => {
    const sticky = await isSticky(client)
    if (sticky) {
      // Don't heartbeat during sticky states -- the agent is intentionally paused
      return
    }

    // Send a single tool-end event to keep activity alive without the
    // thinking->working flash that model-start/model-end pairs create.
    await sendHook(client, "tool-end", {
      tool_name: "heartbeat",
      source: "opencode",
      _scion_heartbeat: true,
    })
  }

  timer = setInterval(tick, HEARTBEAT_INTERVAL_MS)

  // Store cleanup reference on the client for potential future use
  client._scionHeartbeatTimer = timer

  // Fire one immediately in case the session has been running for a while
  tick().catch(() => {})
}

/** Stop the heartbeat timer. */
function stopHeartbeat(client) {
  if (client._scionHeartbeatTimer) {
    clearInterval(client._scionHeartbeatTimer)
    client._scionHeartbeatTimer = null
  }
}

// ---------------------------------------------------------------------------
// Plugin
// ---------------------------------------------------------------------------

export const ScionStatusPlugin = async ({ project, client, $, directory, worktree }) => {
  const agentId = process.env.SCION_AGENT_ID
  const hubEndpoint = process.env.SCION_HUB_ENDPOINT || process.env.SCION_HUB_URL || ""

  // Only activate inside Scion containers
  if (!agentId) {
    return {}
  }

  // Log that the plugin is active
  await client.app.log({
    body: {
      service: "scion-plugin",
      level: "info",
      message: "Scion status plugin activated",
      extra: { agentId, hubEndpoint, directory, worktree },
    },
  })

  // Track assistant text from message.updated events for session-end forwarding
  let assistantTextParts = []

  // Debounced event senders
  const debouncedHook = debounce(sendHook, DEBOUNCE_MS)
  const debouncedMsgHook = debounce(sendHook, MSG_DEBOUNCE_MS)

  // Start heartbeat
  await startHeartbeat(client)

  return {
    // -----------------------------------------------------------------------
    // Session Lifecycle
    // -----------------------------------------------------------------------

    "session.created": async () => {
      await sendHook(client, "session-start", { source: "opencode" })
    },

    "session.deleted": async () => {
      stopHeartbeat(client)
      const assistantText = assistantTextParts.filter(Boolean).join("\n\n").slice(0, 65536)
      await sendHook(client, "session-end", {
        source: "opencode",
        assistant_text: assistantText,
      })
    },

    "session.idle": async () => {
      // Fire agent-end to create an explicit turn boundary, matching
      // Claude's Stop -> agent-end -> working pattern.
      await sendHook(client, "agent-end", { source: "opencode" })
    },

    "session.error": async ({ error }) => {
      stopHeartbeat(client)
      const errorMsg = error ? String(error) : "Unknown error"
      const assistantText = assistantTextParts.filter(Boolean).join("\n\n").slice(0, 65536)
      await sendHook(client, "session-end", {
        source: "opencode",
        error: truncate(errorMsg, 200),
        assistant_text: assistantText,
      })
    },

    // -----------------------------------------------------------------------
    // Tool Execution
    // -----------------------------------------------------------------------

    "tool.execute.before": async (input) => {
      const toolName = input?.tool || "unknown"
      await sendHook(client, "tool-start", {
        tool_name: toolName,
        source: "opencode",
      })
    },

    "tool.execute.after": async (input, output) => {
      const toolName = input?.tool || "unknown"
      const success = output?.success !== false
      await sendHook(client, "tool-end", {
        tool_name: toolName,
        success,
        source: "opencode",
      })
    },

    // -----------------------------------------------------------------------
    // Message Events
    // -----------------------------------------------------------------------

    "message.updated": async ({ event }) => {
      if (!event) return

      const role = event?.role || ""
      const content = event?.content || ""
      const contentStr = typeof content === "string" ? content : JSON.stringify(content).slice(0, 200)

      if (role === "user") {
        // User message -> prompt-submit (thinking)
        // Only fire on the first user message to avoid spamming on edits
        debouncedMsgHook(client, "prompt-submit", {
          prompt: truncate(contentStr, 100),
          source: "opencode",
        })
        // Reset assistant text buffer on new user prompt
        assistantTextParts = []
      } else if (role === "assistant") {
        // Assistant message -> agent-start (thinking) to mark turn boundary,
        // then model-start for the actual model response start
        debouncedMsgHook(client, "agent-start", { source: "opencode" })
        debouncedMsgHook(client, "model-start", {
          prompt: truncate(contentStr, 100),
          source: "opencode",
        })
        // Collect assistant text for session-end forwarding
        assistantTextParts.push(contentStr)
      }
    },

    // -----------------------------------------------------------------------
    // Permission Events
    // -----------------------------------------------------------------------

    "permission.asked": async (input) => {
      // Permission prompt -> waiting_for_input (sticky)
      const description = input?.description || input?.tool || "Permission required"
      await sendHook(client, "notification", {
        message: truncate(String(description), 100),
        source: "opencode",
      })
    },

    "permission.replied": async (input) => {
      // User replied to permission -- next tool-start will clear waiting_for_input
      await client.app.log({
        body: {
          service: "scion-plugin",
          level: "debug",
          message: "permission.replied",
          extra: { decision: input?.decision },
        },
      })
    },

    // -----------------------------------------------------------------------
    // TUI Events
    // -----------------------------------------------------------------------

    "tui.command.execute": async (input) => {
      const command = input?.command || input?.text || ""
      await sendHook(client, "prompt-submit", {
        prompt: truncate(String(command), 100),
        source: "opencode-tui",
      })
    },

    "tui.toast.show": async (input) => {
      // Toast notifications -- log but don't forward to sciontool
      await client.app.log({
        body: {
          service: "scion-plugin",
          level: "debug",
          message: "tui.toast.show",
          extra: { text: String(input?.text || "").slice(0, 200) },
        },
      })
    },

    // -----------------------------------------------------------------------
    // File Events (for observability)
    // -----------------------------------------------------------------------

    "file.edited": async (input) => {
      const filePath = input?.filePath || input?.path || ""
      await client.app.log({
        body: {
          service: "scion-plugin",
          level: "debug",
          message: "file.edited",
          extra: { filePath: String(filePath).slice(0, 200) },
        },
      })
    },

    // -----------------------------------------------------------------------
    // Shell Events
    // -----------------------------------------------------------------------

    "shell.env": async (input, output) => {
      // Shell environment hook -- could inject Scion-specific env vars
      // for now, just log
      await client.app.log({
        body: {
          service: "scion-plugin",
          level: "debug",
          message: "shell.env",
          extra: { cwd: input?.cwd },
        },
      })
    },

    // -----------------------------------------------------------------------
    // Command Events
    // -----------------------------------------------------------------------

    "command.executed": async (input) => {
      const commandName = input?.command || input?.name || ""
      await client.app.log({
        body: {
          service: "scion-plugin",
          level: "debug",
          message: "command.executed",
          extra: { command: String(commandName).slice(0, 200) },
        },
      })
    },

    // -----------------------------------------------------------------------
    // LSP Events (observability only)
    // -----------------------------------------------------------------------

    "lsp.client.diagnostics": async (input) => {
      const diagnostics = input?.diagnostics || []
      const errorCount = Array.isArray(diagnostics)
        ? diagnostics.filter((d) => d?.severity === 1).length
        : 0
      await client.app.log({
        body: {
          service: "scion-plugin",
          level: errorCount > 0 ? "warn" : "debug",
          message: "lsp.diagnostics",
          extra: { total: diagnostics.length, errors: errorCount },
        },
      })
    },

    // -----------------------------------------------------------------------
    // Todo Events
    // -----------------------------------------------------------------------

    "todo.updated": async (input) => {
      const todo = input?.todo || {}
      await client.app.log({
        body: {
          service: "scion-plugin",
          level: "debug",
          message: "todo.updated",
          extra: {
            title: String(todo?.title || "").slice(0, 100),
            status: todo?.status,
          },
        },
      })
    },
  }
}
