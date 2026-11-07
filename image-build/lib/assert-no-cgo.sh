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
#
# assert-no-cgo.sh BINARY
#
# Fails unless the Go binary BINARY was built with CGO_ENABLED=0, read from
# the build settings `go version -m` reports (this works on stripped and
# cross-compiled binaries too).
#
# sciontool must be built without cgo: sciontool init drops the network
# administration capability on every thread with syscall.AllThreadsSyscall,
# which returns ENOTSUP in a binary that links cgo, and init then refuses
# to start any agent with metadata interception. The scion-base image build
# and the Makefile's container-sciontool target both run this check on the
# sciontool they produce.
#
# Exit codes: 0 built with CGO_ENABLED=0, 1 not, 2 build info unreadable.
# Set GO to use a go binary other than the one on PATH.

set -euo pipefail

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
  echo "usage: $0 <go-binary>" >&2
  exit 2
fi
bin=$1

if ! info=$("${GO:-go}" version -m "$bin" 2>/dev/null); then
  echo "assert-no-cgo: cannot read Go build info from $bin" >&2
  exit 2
fi

if printf '%s\n' "$info" | grep -qE $'^\tbuild\tCGO_ENABLED=0$'; then
  echo "assert-no-cgo: $bin is built with CGO_ENABLED=0"
  exit 0
fi
echo "assert-no-cgo: $bin is not built with CGO_ENABLED=0" >&2
exit 1
