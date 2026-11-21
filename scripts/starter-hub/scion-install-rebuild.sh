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

# scripts/starter-hub/scion-install-rebuild.sh - Root-owned helper that installs
# the staged starter-hub binary (/home/scion/scion/scion.rebuild) to
# /usr/local/bin/scion.
#
# Installed by gce-start-hub.sh --full at:
#   /usr/local/libexec/scion/scion-install-rebuild.sh (0755, root:root)
#
# Invariant: scion-hub.service runs as User=scion and must never run as root.
# Because the unprivileged scion user controls /home/scion/scion, this helper
# opens the source without following symlinks and verifies on the open file
# descriptor that it is a regular file owned by scion before copying it
# atomically to /usr/local/bin/scion (0755).
#
# Environment overrides (SCION_REBUILD_SRC, SCION_REBUILD_DEST,
# SCION_REBUILD_OWNER) are for non-root tests only and are ignored when
# invoked via sudo as root.

set -euo pipefail

if [[ $# -ne 0 ]]; then
    echo "scion-install-rebuild: unexpected arguments" >&2
    exit 2
fi

DEFAULT_SRC="/home/scion/scion/scion.rebuild"
DEFAULT_DEST="/usr/local/bin/scion"
DEFAULT_OWNER="scion"

if [[ "$(id -u)" -eq 0 && -n "${SUDO_USER:-}" ]]; then
    SRC="${DEFAULT_SRC}"
    DEST="${DEFAULT_DEST}"
    EXPECTED_OWNER="${DEFAULT_OWNER}"
else
    SRC="${SCION_REBUILD_SRC:-${DEFAULT_SRC}}"
    DEST="${SCION_REBUILD_DEST:-${DEFAULT_DEST}}"
    EXPECTED_OWNER="${SCION_REBUILD_OWNER:-${DEFAULT_OWNER}}"
fi

python3 - "${SRC}" "${DEST}" "${EXPECTED_OWNER}" <<'PY'
import errno
import os
import pwd
import shutil
import stat
import sys
import tempfile

src, dest, owner = sys.argv[1:4]


def die(msg: str) -> None:
    print(f"scion-install-rebuild: {msg}", file=sys.stderr)
    sys.exit(1)


try:
    expected_uid = pwd.getpwnam(owner).pw_uid
except KeyError:
    die(f"unknown expected owner user: {owner}")

flags = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK
try:
    fd = os.open(src, flags)
except OSError as exc:
    if exc.errno == errno.ELOOP:
        die(f"source path must not be a symlink: {src}")
    if exc.errno == errno.ENOENT:
        die(f"source file not found: {src}")
    if exc.errno == errno.EISDIR:
        die(f"source path must be a regular file: {src}")
    die(f"cannot open source {src}: {exc.strerror}")

tmp_path = None
try:
    st = os.fstat(fd)
    if not stat.S_ISREG(st.st_mode):
        os.close(fd)
        fd = -1
        die(f"source path must be a regular file: {src}")
    if st.st_uid != expected_uid:
        os.close(fd)
        fd = -1
        die(
            f"source file must be owned by {owner} (uid {expected_uid}), "
            f"got uid {st.st_uid}: {src}"
        )

    dest_dir = os.path.dirname(dest) or "."
    with os.fdopen(fd, "rb") as src_file:
        fd = -1
        with tempfile.NamedTemporaryFile(
            dir=dest_dir, prefix=".scion.rebuild.", delete=False
        ) as tmp_file:
            tmp_path = tmp_file.name
            shutil.copyfileobj(src_file, tmp_file)
            tmp_file.flush()
            os.fsync(tmp_file.fileno())
            os.fchmod(tmp_file.fileno(), 0o755)
    os.replace(tmp_path, dest)
    tmp_path = None
except SystemExit:
    raise
except Exception as exc:
    die(f"failed to install {src} to {dest}: {exc}")
finally:
    if fd >= 0:
        os.close(fd)
    if tmp_path is not None:
        try:
            os.unlink(tmp_path)
        except OSError:
            pass
PY
