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
# stage-vendored-git.sh <staging-root>
#
# Point the vendored Chainguard git at its own libpcre2 without imposing that
# choice on the rest of the image.
#
# WHY A VENDORED libpcre2 AT ALL
#
# Chainguard's git links against a libpcre2 whose versioned symbols Debian's
# build does not provide. Git still works against the system copy -- this is a
# warning, not a failure -- but it prints
#
#   git: /lib/x86_64-linux-gnu/libpcre2-8.so.0: no version information
#   available (required by git)
#
# to stderr on every single invocation, including plain `git --version`. Agents
# parse git output, so that noise is the actual problem being solved.
#
# WHY RUNPATH AND NOT LD_LIBRARY_PATH
#
# The first version of this set `ENV LD_LIBRARY_PATH=/usr/local/lib/scion-git`
# in the image. That applies to *every* process in the container -- node,
# python, anything an agent compiles -- forcing all of them to prefer
# Chainguard's libpcre2 over the system one. Enormous blast radius for a stderr
# warning. Setting RUNPATH on the few binaries that actually declare libpcre2
# as NEEDED confines the override to exactly those binaries.
#
# Run this in a throwaway build stage: it needs patchelf and binutils, and
# neither should reach a shipped image.

set -euo pipefail

STAGE="${1:?usage: stage-vendored-git.sh <staging-root>}"
RUNPATH_DIR=/usr/local/lib/scion-git

# A scan for "which binaries need libpcre2" that runs without readelf matches
# nothing, patches nothing, and looks exactly like success. Refuse instead.
command -v readelf >/dev/null ||
  {
    echo "FAIL: readelf is missing; the NEEDED scan would silently match nothing" >&2
    exit 1
  }
command -v patchelf >/dev/null ||
  {
    echo "FAIL: patchelf is missing" >&2
    exit 1
  }

patched=0
# -type f is load-bearing: git-core is ~151 symlinks around 4 real binaries,
# and patchelf must be pointed at the binaries, not the links to them.
while IFS= read -r f; do
  readelf -d "$f" 2>/dev/null | grep -q libpcre2 || continue
  patchelf --set-rpath "$RUNPATH_DIR" "$f"
  patched=$((patched + 1))
  echo "  RUNPATH set: ${f#"$STAGE"}"
done < <(
  printf '%s\n' "${STAGE}/usr/bin/git"
  find "${STAGE}/usr/libexec/git-core" -maxdepth 1 -type f
)

echo "stage-vendored-git: patched ${patched} binaries with RUNPATH=${RUNPATH_DIR}"

# On the Chainguard image as of git 2.55.0 this is exactly five: /usr/bin/git,
# git-http-fetch, git-http-push, git-remote-http, git-sh-i18n--envsubst. The
# assertion is ">0" rather than "==5" because upstream may legitimately change
# the split -- but zero means either the scan broke or the vendored libpcre2 is
# no longer needed, and both of those deserve a human.
[ "$patched" -gt 0 ] ||
  {
    echo "FAIL: no binary declared libpcre2 as NEEDED. Either the scan is" >&2
    echo "      broken or the vendored libpcre2 can now be dropped entirely." >&2
    exit 1
  }

readelf -d "${STAGE}/usr/bin/git" | grep -qE 'RPATH|RUNPATH' ||
  {
    echo "FAIL: /usr/bin/git has no RPATH/RUNPATH after patchelf" >&2
    exit 1
  }
