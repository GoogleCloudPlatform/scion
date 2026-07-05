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

# Get status of a specific agent or all agents
# Usage: agent-status.sh [agent-name]
# Returns JSON with agent information

if [ -n "$1" ]; then
    # Get specific agent status
    scion list --format json | jq --arg name "$1" '.[] | select(.name == $name or .Name == $name)'
else
    # Get all agents as JSON
    scion list --format json
fi
