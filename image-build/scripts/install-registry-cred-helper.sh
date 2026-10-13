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

# Install a Docker credential helper that hands out a fresh access token
# for the target registry host each time a push needs one.
#
# Why: the multi-step Cloud Build configs run for 60-80 minutes. In
# ptone/scion image build a979045d, a push 61 minutes in failed with
# "failed to fetch oauth token ... 401 Unauthorized". An access token lasts
# at most 60 minutes, and a cached one from the metadata server can have
# much less left. With credHelpers set, the docker CLI in each step asks
# this helper for a credential at push time, so pushes stop depending on
# when a credential was obtained earlier in the build.
#
# Cloud Build shares $HOME (/builder/home) across steps. The helper goes in
# $HOME/bin, and $HOME/.docker/config.json gets a credHelpers entry for the
# registry host. Steps that push must have $HOME/bin on PATH (see the
# cloudbuild-*.yaml push steps).
#
# Usage:
#   install-registry-cred-helper.sh <registry>           # write helper + config (needs python3)
#   install-registry-cred-helper.sh --verify <registry>  # check the helper returns a credential
#
# The --verify mode runs in the same image as the push steps
# (gcr.io/cloud-builders/docker), so a missing tool or unreachable metadata
# server fails the build in seconds instead of at the first push. It never
# prints the credential.

set -euo pipefail

HELPER_NAME="gcemeta"

verify=false
if [[ "${1:-}" == "--verify" ]]; then
  verify=true
  shift
fi

REGISTRY="${1:?Usage: install-registry-cred-helper.sh [--verify] <registry>}"
reg_host="${REGISTRY%%/*}"
bin_dir="${HOME:?HOME must be set}/bin"
helper="${bin_dir}/docker-credential-${HELPER_NAME}"
config_dir="${DOCKER_CONFIG:-${HOME}/.docker}"
config="${config_dir}/config.json"

if [[ "${verify}" == true ]]; then
  if [[ ! -x "${helper}" ]]; then
    echo "ERROR: credential helper not installed: ${helper}" >&2
    exit 1
  fi
  if ! printf '%s' "${reg_host}" | "${helper}" get | grep -q '"Secret":"[^"]'; then
    echo "ERROR: credential helper returned no credential for ${reg_host}" >&2
    exit 1
  fi
  echo "Registry credential helper OK for ${reg_host}"
  exit 0
fi

mkdir -p "${bin_dir}" "${config_dir}"

# The helper is POSIX sh + curl + sed so it runs in gcr.io/cloud-builders/docker.
# The metadata server returns a token with at least a few minutes left,
# which is plenty for buildkit to exchange it for a registry token.
cat >"${helper}" <<'EOF'
#!/bin/sh
# Docker credential helper: returns the build service account's current
# access token from the GCE metadata server. Installed by
# image-build/scripts/install-registry-cred-helper.sh.
set -eu
case "${1:-}" in
  get)
    server=$(cat)
    resp=$(curl -sSf -m 10 -H 'Metadata-Flavor: Google' \
      'http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token')
    token=$(printf '%s' "${resp}" | sed -n 's/.*"access_token" *: *"\([^"]*\)".*/\1/p')
    if [ -z "${token}" ]; then
      echo "docker-credential-gcemeta: no access_token in metadata server response" >&2
      exit 1
    fi
    printf '{"ServerURL":"%s","Username":"oauth2accesstoken","Secret":"%s"}\n' "${server}" "${token}"
    ;;
  store|erase)
    cat >/dev/null
    ;;
  list)
    echo '{}'
    ;;
  *)
    echo "docker-credential-gcemeta: unsupported action '${1:-}'" >&2
    exit 1
    ;;
esac
EOF
chmod 0755 "${helper}"

# Merge rather than overwrite: keep any auths the build environment already
# put in config.json. credHelpers takes precedence over auths for this host.
python3 - "${config}" "${reg_host}" "${HELPER_NAME}" <<'EOF'
import json
import os
import sys

path, host, helper = sys.argv[1:4]
try:
    with open(path) as f:
        cfg = json.load(f)
except FileNotFoundError:
    cfg = {}
cfg.setdefault("credHelpers", {})[host] = helper
tmp = path + ".tmp"
with open(tmp, "w") as f:
    json.dump(cfg, f, indent=2)
os.chmod(tmp, 0o600)
os.replace(tmp, path)
EOF

echo "Installed docker-credential-${HELPER_NAME} for ${reg_host} (${helper})"
