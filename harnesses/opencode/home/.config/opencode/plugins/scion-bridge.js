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

// Scion hook bridge plugin for OpenCode.
// Subscribes to OpenCode lifecycle events and pipes them to sciontool
// for processing through the Scion hook handler chain.

import { execSync } from 'node:child_process';

function emitHookEvent(eventName, data) {
  try {
    const payload = JSON.stringify({
      hook_event_name: eventName,
      ...data,
    });
    execSync('sciontool hook --dialect=opencode', {
      input: payload,
      stdio: ['pipe', 'ignore', 'ignore'],
      timeout: 5000,
    });
  } catch (err) {
    // Best-effort — never crash the plugin
    if (process.env.SCION_HOOK_DEBUG) {
      console.error(`[scion-bridge] ${eventName}: ${err.message}`);
    }
  }
}

// Field name assumptions below are based on the opencode plugin API as of v0.x.
// Exact shapes are defensive (optional chaining + fallbacks) because the
// @opencode-ai/plugin type definitions do not comprehensively document all
// event payloads. If field names are wrong, values will be empty/defaults,
// not errors.

export const ScionBridge = async (ctx) => {
  let lastMessageEmit = 0;

  return {
    "session.created": async (input) => {
      emitHookEvent("session.created", { session_id: input?.id });
    },
    "tool.execute.before": async (input) => {
      emitHookEvent("tool.execute.before", {
        tool_name: input?.name || input?.tool || "unknown",
        tool_input: typeof input?.args === 'string' ? input.args : JSON.stringify(input?.args || {}),
      });
    },
    "tool.execute.after": async (input, output) => {
      emitHookEvent("tool.execute.after", {
        tool_name: input?.name || input?.tool || "unknown",
        success: !output?.error,
        error: output?.error || "",
      });
    },
    "session.idle": async () => {
      emitHookEvent("session.idle");
    },
    "message.updated": async (input) => {
      const now = Date.now();
      if (now - lastMessageEmit < 500) return;
      lastMessageEmit = now;
      emitHookEvent("message.updated", {
        assistant_text: input?.content || input?.text || "",
      });
    },
    "permission.asked": async (input) => {
      emitHookEvent("permission.asked", {
        message: input?.message || "Permission requested",
      });
    },
    "permission.replied": async () => {
      emitHookEvent("permission.replied");
    },
    "session.error": async (input) => {
      emitHookEvent("session.error", {
        error: input?.error || "Unknown error",
        reason: "error",
      });
    },
  };
};
