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

# Shared helpers for the Scion GKE Continuous Delivery pipeline (deploy/cd/).
# Compatible with Bash 3.2+ and shellcheck -x --source-path=SCRIPTDIR.

log_info() {
  printf '[scion-cd] %s\n' "$*"
}

log_warn() {
  printf '[scion-cd] WARNING: %s\n' "$*" >&2
}

die() {
  printf '[scion-cd] ERROR: %s\n' "$*" >&2
  exit 1
}

# Emit a POSIX-sh-safe KEY='value' assignment line (escaping any single quote).
format_env_kv() {
  local key="$1" val="$2"
  local sq="'"
  local esc="'\\''"
  local escaped="${val//${sq}/${esc}}"
  printf "%s='%s'\n" "${key}" "${escaped}"
}

# Validate boolean substitution ('true' or 'false').
validate_bool_flag() {
  local name="$1" val="$2"
  case "${val}" in
    true|false) return 0 ;;
    *) die "${name} must be 'true' or 'false' (got '${val}')" ;;
  esac
}

# Validate common hub & state bucket inputs shared across CD pipeline steps.
# Mirrors deploy/terraform/configurations/hub-gke/variables.tf anti-clobber rules.
validate_cd_common_inputs() {
  local bucket="${_STATE_BUCKET:-}"
  local prefix="${_SHARED_PREFIX:-}"
  local hub="${_HUB_NAME:-}"

  [[ -n "${bucket}" ]] || die "_STATE_BUCKET is required (e.g. '<project>-<prefix>-tfstate')"
  [[ "${bucket}" =~ ^[a-z0-9][a-z0-9._-]{1,220}[a-z0-9]$ ]] \
    || die "_STATE_BUCKET '${bucket}' is not a valid GCS bucket name"

  [[ -n "${prefix}" ]] || die "_SHARED_PREFIX is required (must match shared-infra name_prefix)"
  [[ "${prefix}" =~ ^[a-z][a-z0-9]{1,7}$ ]] \
    || die "_SHARED_PREFIX '${prefix}' must match ^[a-z][a-z0-9]{1,7}$ (2-8 lowercase alphanumeric chars, no hyphens)"

  [[ -n "${hub}" ]] || die "_HUB_NAME is required (e.g. '${prefix}-gke-1')"
  [[ "${hub}" =~ ^[a-z][a-z0-9-]{2,15}$ ]] \
    || die "_HUB_NAME '${hub}' must match ^[a-z][a-z0-9-]{2,15}$"
  case "${hub}" in
    "${prefix}-"*) ;;
    *) die "_HUB_NAME ('${hub}') must start with '${prefix}-' (anti-clobber rule in configurations/hub-gke)" ;;
  esac
}

# Compute 'sha256:<64hex>' digest of file $1 (portable across Linux and macOS).
sha256_file() {
  local path="$1"
  python3 - "${path}" <<'PY'
import hashlib
import sys

h = hashlib.sha256()
with open(sys.argv[1], "rb") as f:
    while True:
        chunk = f.read(65536)
        if not chunk:
            break
        h.update(chunk)
print("sha256:" + h.hexdigest())
PY
}

# Validate Artifact Registry Docker repository path (<location>-docker.pkg.dev/<project>/<repo>).
# When $2 (<shared_prefix>) is non-empty, also verifies the repository name matches
# '<shared_prefix>-scion' (the repo configurations/hub-gke resolves via shared-lookup).
validate_registry_path() {
  local reg="${1%/}"
  local expected_prefix="${2:-}"
  [[ -n "${reg}" ]] || die "_REGISTRY is required (e.g. 'us-central1-docker.pkg.dev/<project>/<prefix>-scion')"
  local ar_re='^(us|europe|asia|[a-z]+-[a-z]+[0-9]{1,2})-docker\.pkg\.dev/[a-z][a-z0-9-]*[a-z0-9]/[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$'
  [[ "${reg}" =~ ${ar_re} ]] \
    || die "_REGISTRY '${reg}' must be an Artifact Registry Docker path (<location>-docker.pkg.dev/<project>/<repo>)"

  if [[ -n "${expected_prefix}" ]]; then
    local want_repo="${expected_prefix}-scion"
    local repo_tail="${reg#*docker.pkg.dev/*/}"
    [[ "${repo_tail}" == "${want_repo}" ]] \
      || die "_REGISTRY '${reg}' repository '${repo_tail}' does not match '${want_repo}' expected by configurations/hub-gke (shared_prefix='${expected_prefix}')"
  fi
}

# Validate _AGENT_PLATFORMS (e.g. 'linux/amd64' or 'linux/amd64,linux/arm64').
validate_agent_platforms() {
  local plats="$1"
  [[ -n "${plats}" ]] || die "_AGENT_PLATFORMS must not be empty"
  local -a plat_list=()
  IFS=',' read -r -a plat_list <<< "${plats}"
  [[ ${#plat_list[@]} -gt 0 ]] || die "_AGENT_PLATFORMS must list at least one platform"
  local p
  for p in "${plat_list[@]}"; do
    case "${p}" in
      linux/amd64|linux/arm64) ;;
      *) die "_AGENT_PLATFORMS contains unsupported platform '${p}' (allowed: linux/amd64, linux/arm64)" ;;
    esac
  done
}

# Validate a release tag against Scion's deployable release image tag grammar
# (matching pkg/config/image_tag.go releaseVersionPattern so hub.image_tag_version_default
# always resolves agent images to :<TARGET_TAG> rather than falling back to :latest):
#   - vX.Y.Z, vX.Y.Z-preview.N, or vX.Y.Z-rc.N
#   - nightly-YYYYMMDD with a real calendar date
is_valid_release_tag() {
  local tag="$1"
  python3 - "${tag}" <<'PY'
import datetime
import re
import sys

tag = sys.argv[1]
nightly_m = re.match(r"^nightly-(\d{8})$", tag)
if nightly_m:
    try:
        datetime.datetime.strptime(nightly_m.group(1), "%Y%m%d")
    except ValueError:
        sys.exit(1)
    sys.exit(0)

semver_m = re.match(
    r"^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-(preview|rc)\.(0|[1-9]\d*))?$",
    tag,
)
sys.exit(0 if semver_m else 1)
PY
}

# Compare deployed vs target release refs:
#   - nightly <-> nightly: calendar date then optional .N sequence (matching pkg/version/update/compare.go)
#   - SemVer <-> SemVer (stable, preview, rc): ordered as one SemVer family (deliberately stricter than
#     pkg/version/update/compare.go's date-based preview<->stable switch rule so v0.4.0 -> v0.4.0-preview.N
#     and v0.4.0 -> v0.3.x are always classified as 'downgrade' regardless of publish timestamps)
#   - Cross-family (nightly <-> SemVer) or non-release current version: ordered by publish dates
# Prints one of: same | upgrade | switch-newer | downgrade | switch-older | unknown
# Args: <current_tag> <target_tag> [<current_date_rfc3339>] [<target_date_rfc3339>]
compare_release_refs() {
  local cur_ver="$1"
  local tgt_ver="$2"
  local cur_date="${3:-}"
  local tgt_date="${4:-}"

  python3 - "${cur_ver}" "${tgt_ver}" "${cur_date}" "${tgt_date}" <<'PY'
import datetime
import re
import sys

cur_ver, tgt_ver, cur_date_s, tgt_date_s = sys.argv[1:5]

NIGHTLY_RE = re.compile(r"^nightly-(\d{8})(?:\.(\d+))?$")
SEMVER_RE = re.compile(
    r"^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)"
    r"(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)"
    r"(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?$"
)


def parse_nightly(tag):
    m = NIGHTLY_RE.match(tag)
    if not m:
        return None
    try:
        d = datetime.datetime.strptime(m.group(1), "%Y%m%d").replace(
            tzinfo=datetime.timezone.utc
        )
    except ValueError:
        return None
    seq = int(m.group(2)) if m.group(2) is not None else 0
    return (d, seq)


def parse_semver(tag):
    m = SEMVER_RE.match(tag)
    if not m:
        return None
    major, minor, patch = int(m.group(1)), int(m.group(2)), int(m.group(3))
    prerelease = m.group(4)
    return (major, minor, patch, prerelease)


def cmp_semver(a, b):
    pa, pb = parse_semver(a), parse_semver(b)
    if pa[:3] != pb[:3]:
        return -1 if pa[:3] < pb[:3] else 1
    pre_a, pre_b = pa[3], pb[3]
    if pre_a == pre_b:
        return 0
    if pre_a is None:
        return 1
    if pre_b is None:
        return -1
    ids_a = pre_a.split(".")
    ids_b = pre_b.split(".")
    for ia, ib in zip(ids_a, ids_b):
        if ia == ib:
            continue
        na, nb = ia.isdigit(), ib.isdigit()
        if na and nb:
            return -1 if int(ia) < int(ib) else 1
        if na != nb:
            return -1 if na else 1
        return -1 if ia < ib else 1
    return -1 if len(ids_a) < len(ids_b) else 1


def parse_rfc3339(s):
    if not s:
        return None
    s = s.strip()
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    try:
        dt = datetime.datetime.fromisoformat(s)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=datetime.timezone.utc)
        return dt.astimezone(datetime.timezone.utc)
    except ValueError:
        return None


def ref_date(ver, date_s):
    dt = parse_rfc3339(date_s)
    if dt is not None:
        return dt
    n = parse_nightly(ver)
    if n is not None:
        return n[0]
    return None


tgt_n = parse_nightly(tgt_ver)
tgt_s = parse_semver(tgt_ver)
if tgt_n is None and tgt_s is None:
    print("unknown")
    sys.exit(0)

cur_n = parse_nightly(cur_ver)
cur_s = parse_semver(cur_ver)

if cur_n is not None and tgt_n is not None:
    if cur_n == tgt_n:
        print("same")
    elif cur_n < tgt_n:
        print("upgrade")
    else:
        print("downgrade")
    sys.exit(0)

if cur_s is not None and tgt_s is not None:
    # Both SemVer (covers stable<->stable, preview<->preview, and preview<->stable
    # such as v0.3.0 -> v0.4.0-preview.2 or v0.4.0-preview.4 -> v0.4.0).
    c = cmp_semver(cur_ver, tgt_ver)
    if c == 0:
        print("same")
    elif c < 0:
        print("upgrade")
    else:
        print("downgrade")
    sys.exit(0)

# Cross-family (nightly <-> semver) or non-release current version: compare publish dates.
cd = ref_date(cur_ver, cur_date_s)
td = ref_date(tgt_ver, tgt_date_s)
if cd is None or td is None:
    print("unknown")
elif td > cd:
    print("switch-newer")
else:
    print("switch-older")
PY
}

# Read a GCS object into file $2.
# Returns:
#   0  object existed and was written to $2
#   1  object definitely does not exist in GCS (404 / matched no objects)
#   2  GCS lookup failed (403, bucket missing, auth/network error); error printed to stderr
gcs_cat_if_exists() {
  local uri="$1" out_file="$2"
  local err_file
  err_file="$(mktemp "${TMPDIR:-/tmp}/gcs-cat-err.XXXXXX")"
  local rc=0
  CLOUDSDK_CORE_SHOW_STRUCTURED_LOGS=never CLOUDSDK_CORE_CONSOLE_LOG_FORMAT=standard \
    gcloud storage cat "${uri}" --verbosity=error >"${out_file}" 2>"${err_file}" </dev/null || rc=$?
  if [[ ${rc} -eq 0 ]]; then
    rm -f -- "${err_file}"
    return 0
  fi

  local err
  err="$(cat "${err_file}")"
  rm -f -- "${err_file}" "${out_file}"

  # Bucket-not-found or permission/auth errors must NEVER masquerade as an absent object.
  if grep -qiE 'Bucket .* not found|bucket does not exist|HTTPError 40[13]|PERMISSION_DENIED|AccessDenied|unauthori[sz]ed|forbidden|credential' <<<"${err}"; then
    printf '%s\n' "${err}" >&2
    return 2
  fi
  if grep -qiE 'matched no objects|No URLs matched|HTTPError 404|NotFoundException: 404|Object .* not found' <<<"${err}"; then
    return 1
  fi

  printf '%s\n' "${err}" >&2
  return 2
}

# Derive raw LATEST.json URL from _REPO_URL unless MANIFEST_URL is explicitly set.
resolve_manifest_url() {
  if [[ -n "${MANIFEST_URL:-}" ]]; then
    printf '%s\n' "${MANIFEST_URL}"
    return 0
  fi
  local repo="${1:-https://github.com/GoogleCloudPlatform/scion.git}"
  repo="${repo%/}"
  repo="${repo%.git}"
  local gh_re='^https://github\.com/([^/]+/[^/]+)$'
  if [[ "${repo}" =~ ${gh_re} ]]; then
    printf 'https://raw.githubusercontent.com/%s/main/LATEST.json\n' "${BASH_REMATCH[1]}"
    return 0
  fi
  printf 'https://raw.githubusercontent.com/GoogleCloudPlatform/scion/main/LATEST.json\n'
}
