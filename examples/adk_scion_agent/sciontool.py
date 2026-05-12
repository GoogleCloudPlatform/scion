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

"""Low-level wrapper for sciontool — scion's container-side status management CLI.

This module provides two mechanisms for reporting agent status:

1. write_agent_status() — writes directly to $HOME/agent-info.json for transient
   states (thinking, executing, idle). Uses atomic rename to prevent corruption.

2. run_status() — invokes `sciontool status <type> <message>` for sticky states
   (ask_user → waiting_for_input, task_completed → completed). The CLI handles
   hub reporting and logging in addition to the local file update.

All functions degrade gracefully when running outside a scion container (i.e.,
when sciontool is not on PATH). Failures are logged but never raised.
"""

import json
import logging
import os
import shutil
import subprocess
import tempfile
from pathlib import Path

logger = logging.getLogger(__name__)

_sciontool_path: str | None = None
_sciontool_searched: bool = False

# Sticky activities that should not be overwritten by transient updates.
# These match the lowercase activity values used by scion's StatusHandler.
_STICKY_ACTIVITIES = frozenset({"waiting_for_input", "blocked", "completed", "limits_exceeded"})


def _find_sciontool() -> str | None:
    """Locate the sciontool binary, caching the result."""
    global _sciontool_path, _sciontool_searched
    if not _sciontool_searched:
        _sciontool_path = shutil.which("sciontool")
        _sciontool_searched = True
        if _sciontool_path:
            logger.debug("Found sciontool at %s", _sciontool_path)
        else:
            logger.debug("sciontool not found on PATH — status reporting disabled")
    return _sciontool_path


def _agent_info_path() -> Path:
    """Return the path to agent-info.json."""
    return Path(os.environ.get("HOME", "/home/scion")) / "agent-info.json"


def _read_current_activity() -> str | None:
    """Read the current activity from agent-info.json, or None if unavailable."""
    try:
        path = _agent_info_path()
        if path.exists():
            data = json.loads(path.read_text())
            return data.get("activity")
    except Exception:
        pass
    return None


def write_agent_status(activity: str) -> None:
    """Write a transient activity to agent-info.json via atomic rename.

    Respects sticky activity semantics — will not overwrite waiting_for_input,
    blocked, completed, or limits_exceeded.

    Args:
        activity: One of "thinking", "executing", "working" (or other transient
            activities). Values are lowercase to match scion's StatusHandler.
    """
    try:
        current = _read_current_activity()
        if current in _STICKY_ACTIVITIES:
            logger.debug(
                "Skipping transient activity %s — current sticky activity is %s",
                activity,
                current,
            )
            return

        info_path = _agent_info_path()

        # Preserve existing fields in the file.
        existing: dict = {}
        try:
            if info_path.exists():
                existing = json.loads(info_path.read_text())
        except Exception:
            pass

        existing["activity"] = activity
        # Clean up legacy fields if present.
        existing.pop("status", None)
        existing.pop("sessionStatus", None)

        # Atomic write: write to temp file in the same directory, then rename.
        fd, tmp_path = tempfile.mkstemp(
            dir=str(info_path.parent), suffix=".tmp", prefix="agent-info-"
        )
        try:
            with os.fdopen(fd, "w") as f:
                json.dump(existing, f)
            os.rename(tmp_path, str(info_path))
        except Exception:
            # Clean up temp file on failure.
            try:
                os.unlink(tmp_path)
            except OSError:
                pass
            raise

        logger.debug("Updated agent-info.json activity to %s", activity)
    except Exception:
        logger.warning("Failed to write agent activity %s", activity, exc_info=True)


def run_status(status_type: str, message: str) -> None:
    """Invoke `sciontool status <type> <message>` for sticky state transitions.

    This is used for states that require hub reporting and logging beyond the
    local agent-info.json update (ask_user, task_completed, limits_exceeded).

    Args:
        status_type: One of "ask_user", "blocked", "task_completed",
            "limits_exceeded".
        message: A human-readable message describing the status change.
    """
    binary = _find_sciontool()
    if not binary:
        logger.debug(
            "sciontool not available — skipping status %s: %s", status_type, message
        )
        return

    try:
        result = subprocess.run(
            [binary, "status", status_type, message],
            capture_output=True,
            text=True,
            timeout=10,
        )
        if result.returncode != 0:
            logger.warning(
                "sciontool status %s exited %d: %s",
                status_type,
                result.returncode,
                result.stderr.strip(),
            )
        else:
            logger.debug("sciontool status %s: %s", status_type, message)
    except Exception:
        logger.warning(
            "Failed to run sciontool status %s", status_type, exc_info=True
        )
