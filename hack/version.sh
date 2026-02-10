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


# Determine version
# Only set VERSION if we are exactly on a tag (semver-ish)
if git describe --tags --exact-match >/dev/null 2>&1; then
    VERSION=$(git describe --tags --exact-match)
else
    VERSION=""
fi

# Determine commit hash
COMMIT=$(git rev-parse HEAD 2>/dev/null || echo "unknown")

# Determine build time
BUILD_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

# Package to inject variables into
PKG="github.com/ptone/scion-agent/pkg/version"

# Construct ldflags
LDFLAGS="-X ${PKG}.Commit=${COMMIT} -X ${PKG}.BuildTime=${BUILD_TIME}"

if [ -n "$VERSION" ]; then
    LDFLAGS="${LDFLAGS} -X ${PKG}.Version=${VERSION}"
fi

echo "${LDFLAGS}"
