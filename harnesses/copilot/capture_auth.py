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
"""Capture-auth for Copilot — delegates to the standard capture flow,
then captures ~/.copilot/config.json as a COPILOT_CONFIG file secret."""

import os
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import scion_harness

_COPILOT_CONFIG = os.path.join(
    os.environ.get("HOME") or os.path.expanduser("~"),
    ".copilot", "config.json",
)


def _capture_config_json(force: bool = False, scope: str = "user") -> str:
    """Capture ~/.copilot/config.json as a COPILOT_CONFIG file secret.

    Returns "captured", "held" (another agent's pending secret holds the
    name), or "failed".
    """
    if not os.path.isfile(_COPILOT_CONFIG):
        print(
            f"capture-auth: {_COPILOT_CONFIG} not found — "
            "run 'copilot login' first, then re-run this script",
            file=sys.stderr,
        )
        return "failed"

    cmd = [
        "sciontool", "secret", "set", "COPILOT_CONFIG",
        f"@{_COPILOT_CONFIG}",
        "--type", "file",
        "--target", _COPILOT_CONFIG,
        "--scope", scope,
    ]
    if scope == "user":
        cmd.append("--allow-progeny")
    if force:
        cmd.append("--force")

    try:
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
    except FileNotFoundError:
        print("capture-auth: sciontool not found in PATH", file=sys.stderr)
        return "failed"
    except subprocess.TimeoutExpired:
        print("capture-auth: sciontool timed out setting COPILOT_CONFIG", file=sys.stderr)
        return "failed"

    outcome = scion_harness.secret_set_outcome(result, scope)
    if outcome == "held":
        print(scion_harness.held_message("COPILOT_CONFIG"))
        return "held"
    if outcome == "conflict":
        print(
            'capture-auth: COPILOT_CONFIG already exists (use --force to overwrite)',
        )
        return "failed"
    if outcome == "error":
        print(f"capture-auth: failed to set COPILOT_CONFIG: {result.stderr.strip()}", file=sys.stderr)
        return "failed"

    print(scion_harness.captured_message("COPILOT_CONFIG", _COPILOT_CONFIG, outcome))
    return "captured"


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--scope", choices=["project", "user"], default="user")
    known, _ = parser.parse_known_args()

    rc = scion_harness.capture_auth_main()

    force = "--force" in sys.argv
    config = _capture_config_json(force, known.scope)

    if config == "held":
        sys.exit(scion_harness._CA_EXIT_CONFLICT)
    if rc != 0 and config == "captured":
        sys.exit(0)
    sys.exit(rc)
