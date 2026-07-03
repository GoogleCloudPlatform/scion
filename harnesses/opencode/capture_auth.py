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
"""OpenCode capture-auth script.

Scans for credential files on disk and stores them as project-scoped secrets
via `sciontool secret set`. Designed to run after the user authenticates
interactively inside a no-auth agent container.

Reads credential mappings from inputs/capture-auth-config.json (derived from
the harness config.yaml's auth.types.*.required_files declarations). This
avoids hardcoding paths or key names in the script.

Exit codes:
  0 = at least one credential captured
  1 = error
  2 = no credentials found (not an error, but nothing was stored)
  3 = secret already exists (conflict — use --force to overwrite)
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from typing import Any

EXIT_OK = 0
EXIT_ERROR = 1
EXIT_NO_CREDS = 2
EXIT_CONFLICT = 3

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


def _capture_one(
    entry: dict[str, Any], force: bool
) -> tuple[bool, str | None]:
    """Attempt to capture a single credential. Returns (success, error_msg)."""
    key = entry.get("key", "")
    source = _expand(entry.get("source", ""))
    secret_type = entry.get("type", "file")
    target = entry.get("target", "")

    if not key or not source:
        return False, f"invalid entry: missing key or source"

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
        result = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=30,
        )
    except FileNotFoundError:
        return False, "sciontool not found in PATH"
    except subprocess.TimeoutExpired:
        return False, f"sciontool timed out for key {key}"

    if result.returncode != 0:
        stderr = result.stderr.strip()
        if "already exists" in stderr.lower():
            return False, "CONFLICT"
        return False, f"sciontool failed for {key}: {stderr}"

    return True, None


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Capture auth credentials and store as project secrets"
    )
    parser.add_argument(
        "--force",
        action="store_true",
        help="Overwrite existing secrets",
    )
    parser.add_argument(
        "--bundle",
        default=HARNESS_BUNDLE,
        help="Path to harness bundle directory",
    )
    args = parser.parse_args()

    entries = _load_config(args.bundle)
    if not entries:
        print(
            "capture-auth: no credential mappings found in "
            "inputs/capture-auth-config.json",
            file=sys.stderr,
        )
        return EXIT_NO_CREDS

    captured = 0
    conflicts = 0
    errors = 0

    for entry in entries:
        key = entry.get("key", "<unknown>")
        source = entry.get("source", "")
        expanded = _expand(source) if source else ""

        if not expanded or not os.path.isfile(expanded):
            print(f"capture-auth: {key}: source not found ({source})")
            continue

        ok, err = _capture_one(entry, args.force)
        if err == "CONFLICT":
            print(f'CONFLICT: secret "{key}" already exists (use --force to overwrite)')
            conflicts += 1
        elif err:
            print(f"capture-auth: {key}: {err}", file=sys.stderr)
            errors += 1
        elif ok:
            print(f"capture-auth: {key}: captured from {source}")
            captured += 1

    if conflicts > 0:
        return EXIT_CONFLICT

    if errors > 0 and captured == 0:
        return EXIT_ERROR

    if captured == 0:
        print("capture-auth: no credentials found to capture")
        return EXIT_NO_CREDS

    print(f"capture-auth: {captured} credential(s) captured successfully")
    return EXIT_OK


if __name__ == "__main__":
    sys.exit(main())
