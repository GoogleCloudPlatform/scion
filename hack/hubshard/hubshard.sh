#!/usr/bin/env bash
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

# hubshard.sh runs hack/hubshard with the given arguments, like
# go run ./hack/hubshard, but builds the tool into a temporary directory
# (removed on exit) first and runs that binary from the current directory
# (the repository root, as for go run). The make targets and CI call it.
#
# Usage: hubshard.sh [hubshard flags...]   (see README.md)
#
# Environment:
#   GOGC                for the build and the tool (default 25: the tool
#                       only parses; see shard.sh)
#   HUB_SHARD_DEADLINE  optional; see lib.sh

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=hack/hubshard/lib.sh
. "$here/lib.sh"

gogc=${GOGC:-25}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

build_hubshard "$here/../.." "$tmp/hubshard" "$gogc"
step env GOGC="$gogc" "$tmp/hubshard" "$@"
