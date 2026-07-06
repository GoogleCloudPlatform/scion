#!/usr/bin/env python3
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
"""Antigravity capture-auth script.

Extends the standard capture-auth flow with keyring support: when no token
file is found on disk, falls back to extracting AGY_TOKEN from gnome-keyring.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import scion_harness

_CA_EXIT_OK = 0
_CA_EXIT_ERROR = 1
_CA_EXIT_NO_CREDS = 2
_CA_EXIT_CONFLICT = 3


def _setup_dbus() -> bool:
    """Set DBUS_SESSION_BUS_ADDRESS from saved env file if not already set."""
    if os.environ.get("DBUS_SESSION_BUS_ADDRESS"):
        return True
    home = os.environ.get("HOME") or os.path.expanduser("~")
    dbus_env_file = os.path.join(home, ".scion", "harness", ".dbus-env")
    try:
        with open(dbus_env_file, "r") as f:
            for line in f:
                line = line.strip()
                if line.startswith("DBUS_SESSION_BUS_ADDRESS="):
                    addr = line[len("DBUS_SESSION_BUS_ADDRESS="):]
                    os.environ["DBUS_SESSION_BUS_ADDRESS"] = addr
                    return True
    except OSError:
        pass
    return False


def _extract_from_keyring() -> str | None:
    """Extract the AGY OAuth token from gnome-keyring via secret-tool."""
    if not _setup_dbus():
        print("capture-auth: DBUS session not available for keyring access", file=sys.stderr)
        return None
    try:
        result = subprocess.run(
            ["secret-tool", "lookup", "service", "gemini", "username", "antigravity"],
            capture_output=True, text=True, timeout=10,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
        print(f"capture-auth: secret-tool error: {exc}", file=sys.stderr)
        return None
    if result.returncode != 0 or not result.stdout.strip():
        return None
    return result.stdout.strip()


def _validate_refresh_token(token_raw: str) -> bool:
    """Validate that a token JSON blob contains a refresh_token."""
    try:
        obj = json.loads(token_raw)
    except json.JSONDecodeError:
        return False
    if not isinstance(obj, dict):
        return False
    if "refresh_token" in obj:
        return True
    inner = obj.get("token")
    if isinstance(inner, dict) and "refresh_token" in inner:
        return True
    return False


_AGY_TOKEN_PATH = "~/.gemini/antigravity-cli/antigravity-oauth-token"


def _try_capture_token_file(force: bool) -> int | None:
    """Attempt to capture AGY_TOKEN directly from the known disk path.

    Returns an exit code on definitive result, or None to continue to keyring.
    """
    expanded = scion_harness.expand_path(_AGY_TOKEN_PATH)
    if not os.path.isfile(expanded):
        return None
    try:
        with open(expanded, "r", encoding="utf-8") as f:
            raw = f.read().rstrip("\r\n")
    except OSError:
        return None
    if not raw:
        return None
    if not _validate_refresh_token(raw):
        print("capture-auth: AGY_TOKEN file does not contain refresh_token", file=sys.stderr)
        return _CA_EXIT_ERROR
    cmd = ["sciontool", "secret", "set", "AGY_TOKEN", f"@{expanded}",
           "--type", "file", "--target", _AGY_TOKEN_PATH]
    if force:
        cmd.append("--force")
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
    except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
        print(f"capture-auth: sciontool error: {exc}", file=sys.stderr)
        return _CA_EXIT_ERROR
    if result.returncode == 0:
        print(f"capture-auth: AGY_TOKEN: captured from {_AGY_TOKEN_PATH}")
        return _CA_EXIT_OK
    if "already exists" in result.stderr.lower():
        print('CONFLICT: secret "AGY_TOKEN" already exists (use --force to overwrite)')
        return _CA_EXIT_CONFLICT
    print(f"capture-auth: sciontool failed: {result.stderr.strip()}", file=sys.stderr)
    return _CA_EXIT_ERROR


def main() -> int:
    rc = scion_harness.capture_auth_main()
    if rc == _CA_EXIT_OK:
        return rc

    # Only fall back to keyring when no credential files were found on disk.
    # CONFLICT (secret already exists) and ERROR should propagate as-is.
    if rc != _CA_EXIT_NO_CREDS:
        return rc

    # Synthesized default: try the well-known AGY_TOKEN path on disk even if
    # capture-auth-config.json didn't list it (e.g. missing config, staging bug).
    force = "--force" in sys.argv
    disk_rc = _try_capture_token_file(force)
    if disk_rc is not None:
        return disk_rc

    print("capture-auth: file not found, trying gnome-keyring fallback...")
    token = _extract_from_keyring()
    if not token:
        print("capture-auth: no credentials found to capture")
        print("  Make sure you have authenticated with: agy")
        return _CA_EXIT_NO_CREDS

    if not _validate_refresh_token(token):
        print("capture-auth: keyring token does not contain refresh_token", file=sys.stderr)
        return _CA_EXIT_ERROR

    target = "~/.gemini/antigravity-cli/antigravity-oauth-token"
    fd, tmp_path = tempfile.mkstemp(prefix="agy_token_", suffix=".json")
    try:
        with os.fdopen(fd, "w") as f:
            f.write(token)
        force = "--force" in sys.argv
        cmd = ["sciontool", "secret", "set", "AGY_TOKEN", f"@{tmp_path}",
               "--type", "file", "--target", target]
        if force:
            cmd.append("--force")
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
        if result.returncode == 0:
            print("capture-auth: AGY_TOKEN: captured from gnome-keyring")
            return _CA_EXIT_OK
        if "already exists" in result.stderr.lower():
            print('CONFLICT: secret "AGY_TOKEN" already exists (use --force to overwrite)')
            return _CA_EXIT_CONFLICT
        print(f"capture-auth: keyring fallback failed: {result.stderr.strip()}", file=sys.stderr)
        return _CA_EXIT_ERROR
    finally:
        try:
            os.unlink(tmp_path)
        except OSError:
            pass


if __name__ == "__main__":
    sys.exit(main())
