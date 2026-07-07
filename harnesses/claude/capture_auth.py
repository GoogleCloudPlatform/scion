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
"""Capture auth credentials for Claude Code.

Runs the generic config-based capture (scion_harness.capture_auth_main) and
also attempts Claude-specific sk-ant OAuth token extraction from tmux
scrollback.
"""

import os
import re
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import scion_harness

_TOKEN_PATTERN = re.compile(r"^(sk-ant-oat[A-Za-z0-9._-]+)$")


def _extract_oauth_from_scrollback() -> str | None:
    """Search tmux scrollback for a sk-ant OAuth token."""
    try:
        result = subprocess.run(
            ["tmux", "capture-pane", "-pS", "-200", "-t", "scion"],
            capture_output=True, text=True, timeout=5,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return None

    if result.returncode != 0:
        return None

    for line in reversed(result.stdout.splitlines()):
        m = _TOKEN_PATTERN.match(line.strip())
        if m:
            return m.group(1)
    return None


def _store_oauth_token(token: str) -> bool:
    """Store a token as CLAUDE_CODE_OAUTH_TOKEN via sciontool secret set."""
    try:
        result = subprocess.run(
            ["sciontool", "secret", "set", "CLAUDE_CODE_OAUTH_TOKEN", token],
            capture_output=True, text=True, timeout=10,
        )
        if result.returncode == 0:
            print("capture-auth: CLAUDE_CODE_OAUTH_TOKEN captured from tmux scrollback")
            return True
        print(f"capture-auth: failed to store oauth token: {result.stderr.strip()}",
              file=sys.stderr)
    except (FileNotFoundError, subprocess.TimeoutExpired) as exc:
        print(f"capture-auth: failed to store oauth token: {exc}", file=sys.stderr)
    return False


def main() -> int:
    config_rc = scion_harness.capture_auth_main()

    token = _extract_oauth_from_scrollback()
    scrollback_ok = _store_oauth_token(token) if token else False

    if config_rc == 0 or scrollback_ok:
        return 0
    return config_rc


if __name__ == "__main__":
    sys.exit(main())
