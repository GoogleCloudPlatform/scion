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

event="$(printf '%s' "$payload" | sed -n 's/.*"type"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
if [ -z "$event" ]; then
  event="$(printf '%s' "$payload" | sed -n 's/.*"event"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
fi

if [ "$event" = "agent-turn-complete" ]; then
  # TODO: notify hook is disabled pending full hook support in Codex; this script is currently unused
  autoc="${SCION_CODEX_NOTIFY_AUTO_COMPLETE-true}"
  if [ "$autoc" = "false" ] || [ "$autoc" = "0" ] || [ "$autoc" = "no" ]; then
    exit 0
  fi

  title="$(printf '%s' "$payload" | sed -n 's/.*"title"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
  if [ -z "$title" ]; then
    title="Codex turn completed"
  fi

  sciontool status task_completed "$title" >/dev/null 2>&1 || true
fi
