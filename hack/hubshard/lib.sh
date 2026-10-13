# shellcheck shell=bash
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

# lib.sh holds the helpers shard.sh and hubshard.sh share. Source it from
# bash; it defines functions only.
#
# HUB_SHARD_DEADLINE (optional) is a Unix time in seconds. When it is set,
# step runs every command under timeout(1) with the time left until 60
# seconds before the deadline, and fails at once if none is left. Unset
# (the default, and in CI) runs every command as is.

# step runs "$@", under timeout(1) when HUB_SHARD_DEADLINE is set.
step() {
	if [ -z "${HUB_SHARD_DEADLINE:-}" ]; then
		"$@"
		return
	fi
	case "$HUB_SHARD_DEADLINE" in
	*[!0-9]*)
		echo "ERROR: HUB_SHARD_DEADLINE must be a Unix time in seconds: $HUB_SHARD_DEADLINE" >&2
		return 2
		;;
	esac
	local left=$((HUB_SHARD_DEADLINE - $(date +%s) - 60))
	if [ "$left" -le 0 ]; then
		echo "ERROR: HUB_SHARD_DEADLINE leaves no time for: $*" >&2
		return 124
	fi
	timeout "${left}s" "$@"
}

# build_hubshard ROOT OUT GOGC builds ROOT/hack/hubshard into the file OUT
# (go run ./hack/hubshard would build it again on every call) with GOGC.
build_hubshard() {
	local root=$1 out=$2 gogc=$3
	(cd "$root" && step env GOGC="$gogc" go build -buildvcs=false -o "$out" ./hack/hubshard)
}
