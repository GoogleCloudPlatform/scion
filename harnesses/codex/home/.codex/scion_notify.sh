#!/bin/sh
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.


set -eu

payload="${1-}"
if [ -z "$payload" ]; then
  payload="$(cat)"
fi

if [ -z "$payload" ]; then
  exit 0
fi

if ! command -v sciontool >/dev/null 2>&1; then
  exit 0
fi

json_get() {
  key="$1"
  if command -v python3 >/dev/null 2>&1; then
    printf '%s' "$payload" | KEY="$key" python3 -c '
import json
import os
import sys

try:
    data = json.load(sys.stdin)
except Exception:
    raise SystemExit(0)

value = data.get(os.environ.get("KEY", ""))
if isinstance(value, str):
    print(value)
' 2>/dev/null || true
    return
  fi
  printf '%s' "$payload" | sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -n1
}

event="$(json_get type)"
if [ -z "$event" ]; then
  event="$(json_get event)"
fi
if [ -z "$event" ]; then
  event="$(json_get hook_event_name)"
fi

if [ "$event" = "agent-turn-complete" ]; then
  autoc="${SCION_CODEX_NOTIFY_AUTO_COMPLETE-true}"
  if [ "$autoc" = "false" ] || [ "$autoc" = "0" ] || [ "$autoc" = "no" ]; then
    exit 0
  fi

  title="$(json_get title)"
  if [ -z "$title" ]; then
    title="$(json_get last-assistant-message)"
  fi
  if [ -z "$title" ]; then
    title="Codex turn completed"
  fi

  sciontool status task_completed "$title" >/dev/null 2>&1 || true
  exit 0
fi

case "$event" in
  UserPromptSubmit|PreToolUse|PostToolUse|Stop|SubagentStop|SessionStart|SessionEnd)
    printf '%s' "$payload" | sciontool hook --dialect=codex >/dev/null 2>&1 || true
    ;;
esac
