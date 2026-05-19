#!/bin/bash
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

#
# SSE Event Delivery Test (curl-based)
#
# Verifies that SSE events are published and delivered correctly by
# listening to the /events endpoint with curl while triggering state
# changes via the API. No browser needed — pure server-side validation.
#
# Prerequisites:
#   - scion server running with --enable-hub --enable-web --dev-auth
#
# Usage:
#   TOKEN=<dev-token> PROJECT_ID=<uuid> ./sse-curl-test.sh
#
# The script will:
#   1. Open an SSE connection to /events for the given project
#   2. Create an agent via the API
#   3. Update the agent status to running
#   4. Delete the agent
#   5. Print all SSE events received
#
set -euo pipefail

TOKEN="${TOKEN:?Set TOKEN to the dev auth token}"
PROJECT_ID="${PROJECT_ID:?Set PROJECT_ID to the project UUID}"
BASE="${BASE:-http://localhost:8080}"

# Get a session cookie (SSE endpoint requires session auth, not Bearer)
COOKIE=$(curl -s -c - -H "Authorization: Bearer ${TOKEN}" \
  "${BASE}/api/v1/projects" 2>/dev/null | grep scion_sess | awk '{print $NF}')

if [ -z "$COOKIE" ]; then
  echo "ERROR: Could not obtain session cookie. Is the server running?"
  exit 1
fi

echo "=== Starting SSE listener ==="
SSE_OUTPUT=$(mktemp)
timeout 20 curl -sN -b "scion_sess=${COOKIE}" \
  "${BASE}/events?sub=project.${PROJECT_ID}.>" > "$SSE_OUTPUT" 2>&1 &
SSE_PID=$!
sleep 2

echo "=== Creating agent ==="
AGENT_NAME="sse-test-$(date +%s)"
CREATE_RESP=$(curl -s -X POST -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d "{\"name\": \"${AGENT_NAME}\", \"projectId\": \"${PROJECT_ID}\", \"provisionOnly\": true}" \
  "${BASE}/api/v1/agents")
AGENT_ID=$(echo "$CREATE_RESP" | python3 -c "import sys, json; print(json.load(sys.stdin).get('agent', {}).get('id', ''))" 2>/dev/null || echo "")
echo "Agent ID: ${AGENT_ID}"

if [ -z "$AGENT_ID" ]; then
  echo "ERROR: Agent creation failed: $CREATE_RESP"
  kill $SSE_PID 2>/dev/null
  rm -f "$SSE_OUTPUT"
  exit 1
fi

sleep 2

echo "=== Updating agent status to running ==="
# Note: status endpoint uses POST, not PATCH
curl -s -X POST -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"status": "running"}' \
  "${BASE}/api/v1/agents/${AGENT_ID}/status" > /dev/null

sleep 2

echo "=== Deleting agent ==="
curl -s -X DELETE -H "Authorization: Bearer ${TOKEN}" \
  "${BASE}/api/v1/agents/${AGENT_ID}" > /dev/null

sleep 3

# Stop SSE listener
kill $SSE_PID 2>/dev/null || true

echo ""
echo "=== SSE Events Received ==="
cat "$SSE_OUTPUT"
echo ""
echo "=== END ==="

# Validate expected events
EVENTS=$(cat "$SSE_OUTPUT")
PASS=true

if echo "$EVENTS" | grep -q "agent.created"; then
  echo "[PASS] agent.created event received"
else
  echo "[FAIL] agent.created event NOT received"
  PASS=false
fi

if echo "$EVENTS" | grep -q "agent.status"; then
  echo "[PASS] agent.status event received"
else
  echo "[FAIL] agent.status event NOT received"
  PASS=false
fi

if echo "$EVENTS" | grep -q "agent.deleted"; then
  echo "[PASS] agent.deleted event received"
else
  echo "[FAIL] agent.deleted event NOT received"
  PASS=false
fi

if echo "$EVENTS" | grep -q '"event: update"' || echo "$EVENTS" | grep -q "event: update"; then
  echo "[PASS] Events use 'update' event type"
else
  echo "[FAIL] Events do not use 'update' event type"
  PASS=false
fi

rm -f "$SSE_OUTPUT"

if [ "$PASS" = true ]; then
  echo ""
  echo "All checks passed."
else
  echo ""
  echo "Some checks failed."
  exit 1
fi
