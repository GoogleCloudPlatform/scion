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

# Runs one `docker buildx build ... --push` Cloud Build step and records the
# digest it pushed as the step's output, for the release driver
# (build-release-images.sh --channel-tag with --builder cloud-build).
#
# Usage (as a Cloud Build step on gcr.io/cloud-builders/docker):
#   entrypoint: 'bash'
#   args: ['/workspace/image-build/scripts/buildx-record-digest.sh',
#          '$_REGISTRY/<image>:$_TAG', 'buildx', 'build', ..., '--push', '.']
#
# <ref> must be one of the build's -t values. The script adds
# --metadata-file, runs the build, reads the pushed digest from the metadata
# file and writes exactly one line to $BUILDER_OUTPUT/output:
#   <ref> sha256:<64 hex>
# which is the line form the driver reads from results.buildStepOutputs.
# Record only the :$_TAG ref: the driver stops on a line for any ref it did
# not build (such as :$_SHORT_SHA) and on a ref named twice.
#
# Fails (non-zero, nothing written) when the arguments are not a pushing
# buildx build tagged <ref>, when BUILDER_OUTPUT is not a directory, when the
# build fails, or when the metadata file is missing, empty or does not hold
# exactly one well-formed digest for an image named <ref>.
#
# Needs only bash, grep, mktemp and docker (all in gcr.io/cloud-builders/docker).

set -euo pipefail

die() {
  echo "buildx-record-digest: $*" >&2
  exit 1
}

[ $# -ge 3 ] || die "usage: buildx-record-digest.sh <ref> buildx build <args...>"
ref="$1"
shift
if [ "$1" != "buildx" ] || [ "$2" != "build" ]; then die "arguments after <ref> must start with 'buildx build'"; fi
[[ "${ref}" =~ ^[^[:space:],]+:[^[:space:]/:,]+$ ]] || die "'${ref}' is not an image reference with a tag"

push=false
tagged=false
prev=""
for a in "$@"; do
  case "${a}" in
    --push) push=true ;;
    --metadata-file | --metadata-file=*) die "--metadata-file is set by this script, not by the caller" ;;
  esac
  if { [ "${prev}" = "-t" ] || [ "${prev}" = "--tag" ]; } && [ "${a}" = "${ref}" ]; then
    tagged=true
  fi
  prev="${a}"
done
[ "${push}" = true ] || die "no --push: a step that pushes nothing records nothing"
[ "${tagged}" = true ] || die "${ref} is not a -t value of this build"
if [ -z "${BUILDER_OUTPUT:-}" ] || [ ! -d "${BUILDER_OUTPUT}" ]; then die "BUILDER_OUTPUT is not set to a directory"; fi

meta_dir="$(mktemp -d)"
trap 'rm -rf -- "${meta_dir:?}"' EXIT
meta="${meta_dir}/metadata.json"
docker buildx build --metadata-file "${meta}" "${@:3}" || die "docker buildx build failed"

[ -s "${meta}" ] || die "buildx wrote no metadata file (or an empty one) at ${meta}"

# The metadata file is JSON; buildx writes each key once. Read the two keys
# this script needs with anchored patterns and require exactly one of each.
digests="$(grep -oE '"containerimage\.digest"[[:space:]]*:[[:space:]]*"[^"]*"' "${meta}" || true)"
[ -n "${digests}" ] || die "no containerimage.digest in ${meta}"
[ "$(printf '%s\n' "${digests}" | grep -c .)" -eq 1 ] || die "more than one containerimage.digest in ${meta}"
digest="${digests%\"}"
digest="${digest##*\"}"
[[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "containerimage.digest '${digest}' is not sha256:<64 hex>"

names="$(grep -oE '"image\.name"[[:space:]]*:[[:space:]]*"[^"]*"' "${meta}" || true)"
[ -n "${names}" ] || die "no image.name in ${meta}"
[ "$(printf '%s\n' "${names}" | grep -c .)" -eq 1 ] || die "more than one image.name in ${meta}"
names="${names%\"}"
names="${names##*\"}"
found=false
IFS=',' read -r -a name_list <<< "${names}"
for n in "${name_list[@]}"; do
  if [ "${n}" = "${ref}" ]; then found=true; fi
done
[ "${found}" = true ] || die "image.name in ${meta} does not name ${ref}"

printf '%s %s\n' "${ref}" "${digest}" > "${BUILDER_OUTPUT}/output"
echo "buildx-record-digest: recorded ${ref} ${digest}"
