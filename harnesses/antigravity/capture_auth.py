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


def main() -> int:
    rc = scion_harness.capture_auth_main()
    if rc == _CA_EXIT_OK:
        return rc

    print("capture-auth: file not found, trying gnome-keyring fallback...")
    token = _extract_from_keyring()
    if not token:
        if rc == _CA_EXIT_NO_CREDS:
            print("capture-auth: no credentials found to capture")
            print("  Make sure you have authenticated with: agy")
        return rc

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
