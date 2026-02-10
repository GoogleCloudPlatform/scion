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

# hack/test_oauth.sh - Verify OAuth Discovery and Propagation

set -e

REPO_ROOT=$(pwd)
TEST_TMP=$(mktemp -d)
trap 'rm -rf "$TEST_TMP"' EXIT

echo "Using temporary directory: $TEST_TMP"

# Mock HOME
export HOME="$TEST_TMP"
GEMINI_DIR="$HOME/.gemini"
mkdir -p "$GEMINI_DIR"

# 1. Mock settings.json with OAuth selected
cat > "$GEMINI_DIR/settings.json" <<EOF
{
  "security": {
    "auth": {
      "selectedType": "oauth-personal"
    }
  }
}
EOF

# 2. Mock oauth_creds.json
echo '{"access_token": "mock-token", "refresh_token": "mock-refresh"}' > "$GEMINI_DIR/oauth_creds.json"

echo "=== Testing OAuth Discovery ==="

# Initialize a grove in the temp dir
cd "$TEST_TMP"
scion grove init

echo "=== Starting Agent ==="
scion start test-oauth-agent "hello" > start_output.log 2>&1 || true

echo "Start output:"
cat start_output.log

# Check if the agent directory was created
AGENT_DIR=".scion/agents/test-oauth-agent"
if [ -d "$AGENT_DIR" ]; then
    echo "SUCCESS: Agent directory created."
else
    echo "FAILURE: Agent directory not created."
    exit 1
fi

scion stop test-oauth-agent --rm || true

echo "Test complete."