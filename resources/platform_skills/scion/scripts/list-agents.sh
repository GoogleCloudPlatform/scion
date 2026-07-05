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

# List scion agents with optional JSON output
# Usage: list-agents.sh [--json] [--all]

JSON_OUTPUT=""
ALL_FLAG=""

while [[ $# -gt 0 ]]; do
    case $1 in
        --json)
            JSON_OUTPUT="--format json"
            shift
            ;;
        --all|-a)
            ALL_FLAG="--all"
            shift
            ;;
        *)
            shift
            ;;
    esac
done

scion list $ALL_FLAG $JSON_OUTPUT
