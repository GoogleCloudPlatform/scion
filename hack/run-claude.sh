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

set -e

# hack/run-claude.sh - Launch the main scion-claude container for testing
# This script is intended for local testing of the Claude image with persistent 
# home directory and a specific workspace.

# Full image path from pkg/config/embeds/default_settings.json
IMAGE=${1:-us-central1-docker.pkg.dev/ptone-misc/public-docker/scion-claude:latest}
WORKSPACE="/Users/ptone/src/claude/testing-workspace"

echo "=== Launching Claude container ==="
echo "Image:   $IMAGE"
echo "Mount:   $HOME -> /home/scion"
echo "Workdir: $WORKSPACE"

# Ensure the workspace directory exists on the host
mkdir -p "$WORKSPACE"

# Build the command array for consistent echoing and execution
RUN_CMD=(
  container run -it
  --rm
  -v "${HOME}:/home/scion"
  -v "${WORKSPACE}:${WORKSPACE}"
  -w "${WORKSPACE}"
  "$IMAGE"
  claude
)

echo "Executing: ${RUN_CMD[*]}"

# Use 'container' (Apple Virtualization Framework CLI) instead of 'docker'
"${RUN_CMD[@]}"

