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

Captures the Antigravity OAuth token file and stores it as a project-scoped
secret via `sciontool secret set`. Designed to run after the user authenticates
interactively inside a no-auth agent container.

AGY stores its OAuth token as a JSON file at:
  ~/.gemini/antigravity-cli/antigravity-oauth-token

This script reads the file, validates it contains a refresh_token, and stores
it as the AGY_TOKEN secret.

Exit codes:
  0 = credential captured
  1 = error
  2 = no credentials found (not an error, but nothing was stored)
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import tempfile
from typing import Any

EXIT_OK = 0
EXIT_ERROR = 1
EXIT_NO_CREDS = 2

HARNESS_BUNDLE = os.path.join(
    os.environ.get("HOME") or os.path.expanduser("~"),
    ".scion", "harness",
)


def _expand(path: str) -> str:
    return os.path.expanduser(os.path.expandvars(path))


def _load_config(bundle: str) -> list[dict[str, Any]]:
    config_path = os.path.join(bundle, "inputs", "capture-auth-config.json")
    if not os.path.isfile(config_path):
        return []
    with open(config_path, "r", encoding="utf-8") as f:
        try:
            data = json.load(f)
        except (json.JSONDecodeError, OSError):
            return []
    creds = data.get("credentials")
    if not isinstance(creds, list):
        return []
    return creds


def _validate_token_file(path: str) -> bool:
    try:
        with open(path, "r", encoding="utf-8") as f:
            obj = json.load(f)
    except (json.JSONDecodeError, OSError) as exc:
        print(f"capture-auth: token file is not valid JSON: {exc}", file=sys.stderr)
        return False
    has_refresh = (
        "refresh_token" in obj
        or (isinstance(obj.get("token"), dict) and "refresh_token" in obj["token"])
    )
    if not isinstance(obj, dict) or not has_refresh:
        print("capture-auth: token file missing refresh_token field", file=sys.stderr)
        return False
    return True


def _capture_one(entry: dict[str, Any], force: bool) -> tuple[bool, str | None]:
    key = entry.get("key", "") or entry.get("name", "")
    source = _expand(entry.get("source", ""))
    secret_type = entry.get("type", "file")
    target = entry.get("target", "")

    if not key or not source:
        return False, "invalid entry: missing key or source"

    if not os.path.isfile(source):
        return False, None

    cmd = [
        "sciontool", "secret", "set", key, f"@{source}",
        "--type", secret_type,
        "--target", target,
    ]
    if force:
        cmd.append("--force")

    try:
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
    except FileNotFoundError:
        return False, "sciontool not found in PATH"
    except subprocess.TimeoutExpired:
        return False, f"sciontool timed out for key {key}"

    if result.returncode != 0:
        stderr = result.stderr.strip()
        if "already exists" in stderr:
            return True, None
        return False, f"sciontool failed for {key}: {stderr}"

    return True, None


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
    parser = argparse.ArgumentParser(
        description="Capture Antigravity auth token and store as project secret"
    )
    parser.add_argument("--force", action="store_true", help="Overwrite existing secrets")
    parser.add_argument("--bundle", default=HARNESS_BUNDLE)
    args = parser.parse_args()

    entries = _load_config(args.bundle)

    if not entries:
        home = os.environ.get("HOME") or os.path.expanduser("~")
        token_path = os.path.join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
        entries = [{
            "key": "AGY_TOKEN",
            "source": token_path,
            "type": "file",
            "target": "~/.gemini/antigravity-cli/antigravity-oauth-token",
        }]

    seen_keys: set[str] = set()
    unique_entries = []
    for e in entries:
        k = e.get("key", "") or e.get("name", "")
        if k not in seen_keys:
            seen_keys.add(k)
            unique_entries.append(e)
    entries = unique_entries

    captured = 0
    errors = 0

    for entry in entries:
        key = entry.get("key", "") or entry.get("name", "<unknown>")
        source = entry.get("source", "")
        expanded = _expand(source) if source else ""

        if not expanded or not os.path.isfile(expanded):
            print(f"capture-auth: {key}: source not found ({source})")
            continue

        if not _validate_token_file(expanded):
            errors += 1
            continue

        ok, err = _capture_one(entry, args.force)
        if err:
            print(f"capture-auth: {key}: {err}", file=sys.stderr)
            errors += 1
        elif ok:
            print(f"capture-auth: {key}: captured from {source}")
            captured += 1

    # Keyring fallback: AGY may store tokens only in gnome-keyring, not to file
    if captured == 0 and errors == 0:
        print("capture-auth: file not found, trying gnome-keyring fallback...")
        token = _extract_from_keyring()
        if token:
            target = "~/.gemini/antigravity-cli/antigravity-oauth-token"
            fd, tmp_path = tempfile.mkstemp(prefix="agy_token_", suffix=".json")
            try:
                with os.fdopen(fd, "w") as f:
                    f.write(token)
                cmd = ["sciontool", "secret", "set", "AGY_TOKEN", f"@{tmp_path}",
                       "--type", "file", "--target", target]
                if args.force:
                    cmd.append("--force")
                result = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
                if result.returncode == 0:
                    print(f"capture-auth: AGY_TOKEN: captured from gnome-keyring")
                    captured += 1
                else:
                    print(f"capture-auth: keyring fallback failed: {result.stderr.strip()}", file=sys.stderr)
                    errors += 1
            finally:
                try:
                    os.unlink(tmp_path)
                except OSError:
                    pass

    if errors > 0 and captured == 0:
        return EXIT_ERROR

    if captured == 0:
        print("capture-auth: no credentials found to capture")
        print("  Make sure you have authenticated with: agy")
        return EXIT_NO_CREDS

    print(f"capture-auth: {captured} credential(s) captured successfully")
    return EXIT_OK


if __name__ == "__main__":
    sys.exit(main())
