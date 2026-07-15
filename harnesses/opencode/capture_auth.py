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
"""Capture-auth for OpenCode — delegates to the standard capture flow,
then captures ~/.local/share/opencode/auth.json as an OPENCODE_AUTH file secret."""

import os
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import scion_harness

_OPENCODE_AUTH = os.path.join(
    os.environ.get("HOME") or os.path.expanduser("~"),
    ".local", "share", "opencode", "auth.json",
)


def _capture_auth_json(force: bool = False) -> bool:
    """Capture ~/.local/share/opencode/auth.json as an OPENCODE_AUTH file secret."""
    if not os.path.isfile(_OPENCODE_AUTH):
        print(
            f"capture-auth: {_OPENCODE_AUTH} not found — "
            "run 'opencode auth login' first, then re-run this script",
            file=sys.stderr,
        )
        return False

    cmd = [
        "sciontool", "secret", "set", "OPENCODE_AUTH",
        f"@{_OPENCODE_AUTH}",
        "--type", "file",
        "--target", _OPENCODE_AUTH,
    ]
    if force:
        cmd.append("--force")

    try:
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
    except FileNotFoundError:
        print("capture-auth: sciontool not found in PATH", file=sys.stderr)
        return False
    except subprocess.TimeoutExpired:
        print("capture-auth: sciontool timed out setting OPENCODE_AUTH", file=sys.stderr)
        return False

    if result.returncode != 0:
        stderr = result.stderr.strip()
        if "already exists" in stderr.lower():
            print(
                'capture-auth: OPENCODE_AUTH already exists (use --force to overwrite)',
            )
            return False
        print(f"capture-auth: failed to set OPENCODE_AUTH: {stderr}", file=sys.stderr)
        return False

    print(f"capture-auth: OPENCODE_AUTH: captured from {_OPENCODE_AUTH}")
    return True


if __name__ == "__main__":
    rc = scion_harness.capture_auth_main()

    force = "--force" in sys.argv
    config_ok = _capture_auth_json(force)

    if rc != 0 and config_ok:
        sys.exit(0)
    sys.exit(rc)
