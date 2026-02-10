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

# Start a scion agent with a task
# Usage: start-agent.sh <name> <task> [--type template] [--attach]

if [ $# -lt 2 ]; then
    echo "Usage: start-agent.sh <name> <task> [--type template] [--attach]"
    exit 1
fi

NAME="$1"
shift
TASK="$1"
shift

# Pass remaining args to scion
scion start "$NAME" "$TASK" "$@"
