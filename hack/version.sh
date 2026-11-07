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


# BEGIN release-tag rule (keep identical in hack/version.sh and
# image-build/scripts/build-images.sh; test-build-images.sh compares them)
# VERSION accepts only the tag forms the release process cuts: vX.Y.Z,
# vX.Y.Z-preview.N, vX.Y.Z-rc.N and nightly-YYYYMMDD (the pattern of
# scripts/release/update-homebrew-formula.sh plus the nightly form). Any
# other tag is treated as no release tag: VERSION stays empty, so the
# binaries report "dev", and one warning goes to stderr. It never fails.
release_tag_or_empty() {
  local re='^(v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(preview|rc)\.(0|[1-9][0-9]*))?|nightly-[0-9]{8})$'
  if [[ -z "$1" ]]; then
    return 0
  elif [[ "$1" =~ $re ]]; then
    printf '%s\n' "$1"
  elif [[ "$1" =~ ^[A-Za-z0-9._-]+$ ]]; then
    printf 'Warning: tag %s is not a release tag form; VERSION is left empty and the binaries report "dev".\n' "$1" >&2
  else
    printf 'Warning: a tag with unexpected characters is not a release tag form; VERSION is left empty and the binaries report "dev".\n' >&2
  fi
}
# END release-tag rule

# Determine version
# Only set VERSION if we are exactly on a tag. Prefer v* tags: a preview or
# stable tag shares its commit with a nightly tag, and among lightweight tags
# git describe picks the first in refname order (nightly-* before v*).
# The tag found must also be a release tag form (release_tag_or_empty).
if VERSION=$(git describe --tags --exact-match --match 'v*' 2>/dev/null); then
    :
elif VERSION=$(git describe --tags --exact-match 2>/dev/null); then
    :
else
    VERSION=""
fi
VERSION=$(release_tag_or_empty "$VERSION")

# Determine commit hash
COMMIT=$(git rev-parse HEAD 2>/dev/null || echo "unknown")

# Determine build time
BUILD_TIME=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

# Package to inject variables into (accept override as first argument)
PKG="${1:-github.com/GoogleCloudPlatform/scion/pkg/version}"

# Construct ldflags
LDFLAGS="-X ${PKG}.Commit=${COMMIT} -X ${PKG}.BuildTime=${BUILD_TIME}"

if [ -n "$VERSION" ]; then
    LDFLAGS="${LDFLAGS} -X ${PKG}.Version=${VERSION}"
fi

echo "${LDFLAGS}"
