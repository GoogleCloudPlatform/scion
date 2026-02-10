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
# merge-work.sh - Convenience script to merge an agent's branch onto main
#
# Usage: ./hack/merge-work.sh <agent-name>
#
# This script attempts to merge an agent's branch. If the branches have diverged,
# it sends a message to the agent to rebase on main, then retries the merge
# in a loop until successful or max retries reached.

set -e

MAX_RETRIES=7
WAIT_SECONDS=5

if [ -z "$1" ]; then
    echo "Usage: $0 <agent-name>"
    exit 1
fi

AGENT_NAME="$1"

try_merge() {
    git merge "$AGENT_NAME" 2>&1
}

echo "Attempting to merge branch '$AGENT_NAME'..."

# First attempt
if output=$(try_merge); then
    echo "$output"
    echo "Merge successful!"
    echo "Stopping agent '$AGENT_NAME'..."
    scion stop "$AGENT_NAME"
    exit 0
fi

# Merge failed - send rebase message and start retry loop
echo "Merge failed, sending rebase request to agent..."
scion message "$AGENT_NAME" 'please rebase on main'

for i in $(seq 1 $MAX_RETRIES); do
    echo "Waiting ${WAIT_SECONDS} seconds before retry $i of $MAX_RETRIES..."
    sleep $WAIT_SECONDS

    echo "Attempting merge (retry $i)..."
    if output=$(try_merge); then
        echo "$output"
        echo "Merge successful!"
        echo "Stopping agent '$AGENT_NAME'..."
        scion stop "$AGENT_NAME"
        exit 0
    fi

    echo "Merge still not possible..."
done

echo "Error: Failed to merge after $MAX_RETRIES retries"
exit 1
