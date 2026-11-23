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

# scripts/starter-hub/check-install-perms.sh - Verify ownership and permissions
# of starter-hub installed files, sudoers-allowed paths, and their parent
# directories.
#
# Usage: scripts/starter-hub/check-install-perms.sh [--require-sudoers] [--scion-bin <path>]
#
# Checks that:
#   - /usr/local/bin/scion (or --scion-bin) is a regular file owned by root:root
#     with mode 0755.
#   - /etc/systemd/system/scion-hub.service is a regular file owned by root:root
#     with mode 0644 (plus any drop-ins under scion-hub.service.d/ if present).
#   - /etc/sudoers.d/scion-rebuild-server (required with --require-sudoers,
#     checked if present otherwise) is a regular file owned by root:root with
#     mode 0440.
#   - Every command and destination path in the sudoers rules (/usr/bin/install,
#     /usr/bin/systemctl, /usr/local/bin/scion, and any command paths listed in
#     /etc/sudoers.d/scion-rebuild-server) is root-owned and neither group- nor
#     world-writable.
#   - Every parent directory up to / of every checked path is root-owned and
#     neither group- nor world-writable.
#
# Exits 0 if all checks pass, 1 if any check fails.

set -euo pipefail

ROOT=""
EXPECTED_UID=0
EXPECTED_GID=0
REQUIRE_SUDOERS=false
SCION_BIN="/usr/local/bin/scion"
UNIT_PATH="/etc/systemd/system/scion-hub.service"
SUDOERS_PATH="/etc/sudoers.d/scion-rebuild-server"

usage() {
    cat <<'EOF'
Usage: check-install-perms.sh [--require-sudoers] [--scion-bin <path>]

Verifies that the installed scion binary, systemd unit file, sudoers drop-in,
every command and destination path in the sudoers rules, and all of their
parent directories are root-owned and neither group- nor world-writable.

Options:
  --require-sudoers    Fail if /etc/sudoers.d/scion-rebuild-server is missing.
  --scion-bin <path>   Path to the installed scion binary (default: /usr/local/bin/scion).
  -h, --help           Show this help message.
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --require-sudoers)
            REQUIRE_SUDOERS=true
            shift
            ;;
        --scion-bin)
            if [[ $# -lt 2 ]]; then
                echo "Error: --scion-bin requires a path argument." >&2
                exit 2
            fi
            SCION_BIN="$2"
            shift 2
            ;;
        --root)
            if [[ $# -lt 2 ]]; then
                echo "Error: --root requires a directory argument." >&2
                exit 2
            fi
            ROOT="${2%/}"
            shift 2
            ;;
        --expected-uid)
            if [[ $# -lt 2 ]]; then
                echo "Error: --expected-uid requires a numeric uid." >&2
                exit 2
            fi
            EXPECTED_UID="$2"
            shift 2
            ;;
        --expected-gid)
            if [[ $# -lt 2 ]]; then
                echo "Error: --expected-gid requires a numeric gid." >&2
                exit 2
            fi
            EXPECTED_GID="$2"
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "Error: unknown option: $1" >&2
            usage >&2
            exit 2
            ;;
    esac
done

ERRORS=0
CHECKED_DIRS=$'\n'
CHECKED_FILES=$'\n'

record_error() {
    echo "ERROR: $1" >&2
    ERRORS=$((ERRORS + 1))
}

# host_path LOGICAL_PATH -- maps a logical path (e.g. /usr/local/bin/scion)
# onto the test root when --root is set.
host_path() {
    local logical="$1"
    if [[ -z "${ROOT}" ]]; then
        printf '%s' "${logical}"
    elif [[ "${logical}" == "/" ]]; then
        printf '%s' "${ROOT}"
    else
        printf '%s%s' "${ROOT}" "${logical}"
    fi
}

# stat_meta PATH [follow] -- prints "uid gid mode_octal" for PATH.
# When follow is "L", follows symlinks; otherwise inspects the path itself.
stat_meta() {
    local target="$1" follow="${2:-}"
    if [[ "${follow}" == "L" ]]; then
        stat -L -c '%u %g %a' "${target}" 2>/dev/null
    else
        stat -c '%u %g %a' "${target}" 2>/dev/null
    fi
}

# check_real_dir REAL_PATH DISPLAY_PATH -- verifies a single directory on disk
# is owned by EXPECTED_UID and neither group- nor world-writable.
check_real_dir() {
    local real="$1" display="$2"
    if [[ "${CHECKED_DIRS}" == *$'\n'"${real}"$'\n'* ]]; then
        return 0
    fi
    CHECKED_DIRS="${CHECKED_DIRS}${real}"$'\n'

    if [[ ! -e "${real}" && ! -L "${real}" ]]; then
        record_error "${display}: directory does not exist"
        return 0
    fi

    if [[ -L "${real}" ]]; then
        local link_meta link_uid
        if ! link_meta="$(stat_meta "${real}")"; then
            record_error "${display}: cannot stat symlink"
            return 0
        fi
        read -r link_uid _ _ <<< "${link_meta}"
        if [[ "${link_uid}" != "${EXPECTED_UID}" ]]; then
            record_error "${display}: symlink owner uid is ${link_uid}, expected ${EXPECTED_UID} (root)"
        fi
        local resolved
        if ! resolved="$(readlink -f "${real}" 2>/dev/null)" || [[ -z "${resolved}" ]]; then
            record_error "${display}: broken symlink"
            return 0
        fi
        check_resolved_ancestors "${resolved}" "${display}"
    fi

    if [[ ! -d "${real}" ]]; then
        record_error "${display}: expected a directory"
        return 0
    fi

    local meta uid mode
    if ! meta="$(stat_meta "${real}" L)"; then
        record_error "${display}: cannot stat directory"
        return 0
    fi
    read -r uid _ mode <<< "${meta}"

    if [[ "${uid}" != "${EXPECTED_UID}" ]]; then
        record_error "${display}: directory owner uid is ${uid}, expected ${EXPECTED_UID} (root)"
    fi
    if (( (8#${mode} & 0022) != 0 )); then
        record_error "${display}: directory is group- or world-writable (mode 0${mode})"
    fi
}

# check_ancestors LOGICAL_DIR -- walks LOGICAL_DIR and every parent up to /.
check_ancestors() {
    local dir="$1"
    while :; do
        local hp
        hp="$(host_path "${dir}")"
        check_real_dir "${hp}" "${dir}"
        if [[ "${dir}" == "/" ]]; then
            break
        fi
        dir="$(dirname "${dir}")"
    done
}

# check_resolved_ancestors REAL_PATH DISPLAY_ORIGIN -- checks a symlink target's
# ancestors (stopping at ROOT if inside ROOT, otherwise walking to /).
check_resolved_ancestors() {
    local resolved="$1" origin="$2"
    if [[ -n "${ROOT}" && ( "${resolved}" == "${ROOT}" || "${resolved}" == "${ROOT}/"* ) ]]; then
        local logical="${resolved#"${ROOT}"}"
        [[ -n "${logical}" ]] || logical="/"
        check_ancestors "${logical}"
        return 0
    fi

    local dir="${resolved}"
    while :; do
        check_real_dir "${dir}" "${dir} (resolved from ${origin})"
        if [[ "${dir}" == "/" ]]; then
            break
        fi
        dir="$(dirname "${dir}")"
    done
}

# check_installed_file LOGICAL_PATH EXACT_MODE -- verifies a file installed by
# starter-hub: must be a regular file (not a symlink), owned by root:root,
# with exact mode EXACT_MODE, and all parent directories root-owned and
# neither group- nor world-writable.
check_installed_file() {
    local logical="$1" exact_mode="$2"
    local hp parent
    hp="$(host_path "${logical}")"
    parent="$(dirname "${logical}")"

    check_ancestors "${parent}"

    if [[ ! -e "${hp}" && ! -L "${hp}" ]]; then
        record_error "${logical}: does not exist"
        return 0
    fi
    if [[ -L "${hp}" ]]; then
        record_error "${logical}: must be a regular file, not a symlink"
        return 0
    fi
    if [[ ! -f "${hp}" ]]; then
        record_error "${logical}: must be a regular file"
        return 0
    fi

    local meta uid gid mode
    if ! meta="$(stat_meta "${hp}")"; then
        record_error "${logical}: cannot stat file"
        return 0
    fi
    read -r uid gid mode <<< "${meta}"

    if [[ "${uid}" != "${EXPECTED_UID}" ]]; then
        record_error "${logical}: owner uid is ${uid}, expected ${EXPECTED_UID} (root)"
    fi
    if [[ "${gid}" != "${EXPECTED_GID}" ]]; then
        record_error "${logical}: group gid is ${gid}, expected ${EXPECTED_GID} (root)"
    fi
    if (( (8#${mode} & 0022) != 0 )); then
        record_error "${logical}: mode is 0${mode}, expected ${exact_mode} (must not be group- or world-writable)"
    elif (( 8#${mode} != 8#${exact_mode} )); then
        record_error "${logical}: mode is 0${mode}, expected ${exact_mode}"
    fi
}

# check_sudoers_path LOGICAL_PATH -- verifies a command or target path allowed
# by sudoers: must exist, be root-owned, neither group- nor world-writable,
# and have safe parent directories. Symlinks (e.g. /usr/bin -> /bin) are
# allowed only if both the symlink and its resolved target (and target's
# parent directories) satisfy the same requirements.
check_sudoers_path() {
    local logical="$1"
    if [[ "${CHECKED_FILES}" == *$'\n'"${logical}"$'\n'* ]]; then
        return 0
    fi
    CHECKED_FILES="${CHECKED_FILES}${logical}"$'\n'

    local hp parent
    hp="$(host_path "${logical}")"
    parent="$(dirname "${logical}")"

    check_ancestors "${parent}"

    if [[ ! -e "${hp}" && ! -L "${hp}" ]]; then
        record_error "${logical}: does not exist"
        return 0
    fi

    if [[ -L "${hp}" ]]; then
        local link_meta link_uid
        if ! link_meta="$(stat_meta "${hp}")"; then
            record_error "${logical}: cannot stat symlink"
            return 0
        fi
        read -r link_uid _ _ <<< "${link_meta}"
        if [[ "${link_uid}" != "${EXPECTED_UID}" ]]; then
            record_error "${logical}: symlink owner uid is ${link_uid}, expected ${EXPECTED_UID} (root)"
        fi
        local resolved
        if ! resolved="$(readlink -f "${hp}" 2>/dev/null)" || [[ -z "${resolved}" ]]; then
            record_error "${logical}: broken symlink"
            return 0
        fi
        check_resolved_ancestors "$(dirname "${resolved}")" "${logical}"
    fi

    if [[ ! -f "${hp}" ]]; then
        record_error "${logical}: expected a regular file"
        return 0
    fi

    local meta uid mode
    if ! meta="$(stat_meta "${hp}" L)"; then
        record_error "${logical}: cannot stat file"
        return 0
    fi
    read -r uid _ mode <<< "${meta}"

    if [[ "${uid}" != "${EXPECTED_UID}" ]]; then
        record_error "${logical}: owner uid is ${uid}, expected ${EXPECTED_UID} (root)"
    fi
    if (( (8#${mode} & 0022) != 0 )); then
        record_error "${logical}: mode is 0${mode} (must not be group- or world-writable)"
    fi
}

# 1. Installed scion binary (root:root, 0755) and its parent directories.
check_installed_file "${SCION_BIN}" "0755"

# 2. Installed systemd unit file (root:root, 0644) and its parent directories.
check_installed_file "${UNIT_PATH}" "0644"

# Also check any systemd drop-in overrides for scion-hub.service if present.
UNIT_DROPIN_DIR="${UNIT_PATH}.d"
if [[ -e "$(host_path "${UNIT_DROPIN_DIR}")" || -L "$(host_path "${UNIT_DROPIN_DIR}")" ]]; then
    check_ancestors "${UNIT_DROPIN_DIR}"
    if [[ -d "$(host_path "${UNIT_DROPIN_DIR}")" ]]; then
        while IFS= read -r -d '' dropin; do
            dropin_name="$(basename "${dropin}")"
            check_installed_file "${UNIT_DROPIN_DIR}/${dropin_name}" "0644"
        done < <(find "$(host_path "${UNIT_DROPIN_DIR}")" -maxdepth 1 -mindepth 1 -print0 2>/dev/null)
    fi
fi

# 3. Installed sudoers drop-in (root:root, 0440) and its parent directories.
SUDOERS_HOST="$(host_path "${SUDOERS_PATH}")"
if [[ "${REQUIRE_SUDOERS}" == "true" || -e "${SUDOERS_HOST}" || -L "${SUDOERS_HOST}" ]]; then
    check_installed_file "${SUDOERS_PATH}" "0440"
fi

# 4. Every command and destination path in the sudoers rules and its parent directories.
check_sudoers_path "/usr/bin/install"
check_sudoers_path "/usr/bin/systemctl"
check_sudoers_path "${SCION_BIN}"

# Also inspect any NOPASSWD rules present in the sudoers drop-in so any
# additional command paths are verified automatically. Supports single commands
# and comma-separated command lists on NOPASSWD lines; fails closed on
# unsupported sudoers constructs (Cmnd_Alias, line continuations, #include /
# @include directives, or non-absolute commands) that could hide unverified paths.
if [[ -f "${SUDOERS_HOST}" && -r "${SUDOERS_HOST}" ]]; then
    while IFS= read -r line || [[ -n "${line}" ]]; do
        # Strip leading whitespace.
        line="${line#"${line%%[![:space:]]*}"}"
        [[ -z "${line}" ]] && continue

        # Fail closed on include directives before skipping regular comments.
        if [[ "${line}" =~ ^[#@](include|includedir)[[:space:]] ]]; then
            record_error "${SUDOERS_PATH}: unsupported sudoers include directive cannot be verified: ${line}"
            continue
        fi
        [[ "${line}" == \#* ]] && continue

        # Fail closed on line continuations and command aliases.
        if [[ "${line}" == *\\ ]]; then
            record_error "${SUDOERS_PATH}: unsupported sudoers line continuation cannot be verified"
            continue
        fi
        if [[ "${line}" =~ ^Cmnd_Alias[[:space:]] ]]; then
            record_error "${SUDOERS_PATH}: unsupported Cmnd_Alias directive cannot be verified"
            continue
        fi

        if [[ "${line}" == *NOPASSWD:* ]]; then
            cmd_list="${line#*NOPASSWD:}"
            IFS=',' read -r -a cmd_entries <<< "${cmd_list}"
            for cmd_spec in "${cmd_entries[@]}"; do
                # Trim leading/trailing whitespace.
                cmd_spec="${cmd_spec#"${cmd_spec%%[![:space:]]*}"}"
                cmd_spec="${cmd_spec%"${cmd_spec##*[![:space:]]}"}"
                [[ -z "${cmd_spec}" ]] && continue
                # Strip optional repeated tag prefixes (e.g. NOPASSWD: / PASSWD:) on subsequent items.
                while [[ "${cmd_spec}" =~ ^(NO)?(PASSWD|EXEC|SETENV):[[:space:]]*(.*)$ ]]; do
                    cmd_spec="${BASH_REMATCH[3]}"
                done
                # First whitespace-delimited token is the command binary.
                cmd_bin="${cmd_spec%%[[:space:]]*}"
                if [[ "${cmd_bin}" == /* ]]; then
                    check_sudoers_path "${cmd_bin}"
                else
                    record_error "${SUDOERS_PATH}: cannot verify non-absolute sudoers command '${cmd_bin}'"
                    continue
                fi
                # For install rules, also check the destination path (last token).
                if [[ "${cmd_bin}" == */install ]]; then
                    dest_path="${cmd_spec##*[[:space:]]}"
                    if [[ "${dest_path}" == /* ]]; then
                        check_sudoers_path "${dest_path}"
                    fi
                fi
            done
        fi
    done < "${SUDOERS_HOST}"
fi

if (( ERRORS > 0 )); then
    echo "ERROR: install ownership/permission check failed (${ERRORS} error(s)); refusing to continue." >&2
    exit 1
fi

echo "  -> Install ownership and permissions verified"
