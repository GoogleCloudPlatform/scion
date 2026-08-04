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
"""LHH REPL runner for Scion — uses build_runner() for full capabilities.

This module is the Scion-facing entry point for the Long-Horizon Harness (LHH).
It calls LHH's build_runner() — the same function the FastAPI app uses — to
get a fully configured Runner with session, memory, and artifact services
resolved from the environment. It then enters an input() loop for Scion to
deliver messages via tmux send-keys.

Key design decisions:
  - Loads provisioner env vars BEFORE importing horizon (see _load_provisioner_env)
    because horizon/agent.py calls google.auth.default() at import time.
  - Uses build_runner() for full LHH capabilities (NOT InMemoryRunner).
  - Injects Scion callbacks into LHH agent callback chains for status reporting.
  - Reports status via write_agent_status() to ~/agent-info.json.
  - Crash reporting to /proc/1/fd/2 (container logs).

Usage (standalone):
    python3 /opt/lhh/lhh_scion_repl.py --input "what is 2+2"

Usage (with resume):
    python3 /opt/lhh/lhh_scion_repl.py --resume
"""

from __future__ import annotations

import argparse
import asyncio
import json
import logging
import os
import shutil
import subprocess
import sys
import tempfile
import traceback
from pathlib import Path

logger = logging.getLogger(__name__)

APP_NAME = "horizon"
USER_ID = os.environ.get("LHH_USER_ID", "scion_user")
SESSION_FILE = Path.home() / ".lhh" / "last_session_id"


# ── crash reporting ──────────────────────────────────────────


def _log_to_init(message: str) -> None:
    """Write directly to PID 1 (sciontool) stderr so it appears in container logs."""
    try:
        with open("/proc/1/fd/2", "w") as f:
            f.write(message + "\n")
            f.flush()
    except Exception:
        pass


def _report_crash(message: str) -> None:
    """Write crash details to container logs, agent-info.json, and stderr."""
    print(message, file=sys.stderr, flush=True)
    _log_to_init(message)
    try:
        info_path = os.path.join(
            os.environ.get("HOME", "/home/scion"), "agent-info.json"
        )
        with open(info_path, "w") as f:
            json.dump({"activity": "error", "error": message}, f)
    except Exception:
        pass


# ── sciontool status bridge ──────────────────────────────────

# Sticky activities that should not be overwritten by transient updates.
_STICKY_ACTIVITIES = frozenset(
    {"waiting_for_input", "blocked", "completed", "limits_exceeded"}
)


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
    """Atomic write to ~/agent-info.json. Same pattern as
    examples/adk_scion_agent/sciontool.py.

    Respects sticky activity semantics — will not overwrite waiting_for_input,
    blocked, completed, or limits_exceeded.
    """
    try:
        current = _read_current_activity()
        if current in _STICKY_ACTIVITIES:
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
            try:
                os.unlink(tmp_path)
            except OSError:
                pass
            raise
    except Exception:
        logger.warning("Failed to write agent activity %s", activity, exc_info=True)


def run_sciontool_status(status_type: str, message: str) -> None:
    """Invoke sciontool status <type> <message> for sticky states."""
    binary = shutil.which("sciontool")
    if not binary:
        return
    try:
        subprocess.run(
            [binary, "status", status_type, message],
            capture_output=True,
            text=True,
            timeout=10,
        )
    except Exception:
        logger.warning(
            "Failed to run sciontool status %s", status_type, exc_info=True
        )


# ── provisioner env loading ──────────────────────────────────


def _load_provisioner_env() -> None:
    """Load env vars from provisioner outputs BEFORE importing horizon.

    This is CRITICAL because horizon/agent.py calls google.auth.default()
    at import time. The env vars (GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_REGION,
    GOOGLE_APPLICATION_CREDENTIALS, etc.) must be set before any horizon.*
    modules are imported.
    """
    env_json = Path.home() / ".scion" / "harness" / "outputs" / "env.json"
    if not env_json.exists():
        return
    try:
        with open(env_json, "r", encoding="utf-8") as f:
            env_data = json.load(f)
        if isinstance(env_data, dict):
            for key, value in env_data.items():
                if isinstance(value, str) and key not in os.environ:
                    os.environ[key] = value
    except Exception as e:
        logger.warning("Failed to load provisioner env: %s", e)


# ── session persistence ──────────────────────────────────────


def save_session_id(session_id: str) -> None:
    """Persist the current session ID for --resume."""
    SESSION_FILE.parent.mkdir(parents=True, exist_ok=True)
    SESSION_FILE.write_text(session_id)


def load_session_id() -> str | None:
    """Load a previously saved session ID."""
    if SESSION_FILE.exists():
        return SESSION_FILE.read_text().strip() or None
    return None


# ── Scion callback injection ─────────────────────────────────


def _inject_scion_callbacks(app: object) -> None:
    """Inject Scion status callbacks into the LHH agent's callback chains.

    ADK Agent callback fields are lists; we append our callbacks so they
    run after any existing LHH callbacks.
    """
    root_agent = getattr(app, "root_agent", None)
    if root_agent is None:
        logger.warning("Could not find root_agent on app; skipping callback injection")
        return

    async def scion_before_tool(tool, args, tool_context):
        write_agent_status("executing")
        return None  # never interfere with ADK flow

    async def scion_after_tool(tool, args, tool_context, tool_response):
        write_agent_status("thinking")
        return None

    async def scion_before_agent(callback_context):
        write_agent_status("thinking")
        return None

    # ADK Agent callback fields are lists; append to them.
    before_tool = getattr(root_agent, "before_tool_callback", None)
    if isinstance(before_tool, list):
        before_tool.append(scion_before_tool)
    elif before_tool is None:
        root_agent.before_tool_callback = [scion_before_tool]

    after_tool = getattr(root_agent, "after_tool_callback", None)
    if isinstance(after_tool, list):
        after_tool.append(scion_after_tool)
    elif after_tool is None:
        root_agent.after_tool_callback = [scion_after_tool]

    before_agent = getattr(root_agent, "before_agent_callback", None)
    if isinstance(before_agent, list):
        before_agent.append(scion_before_agent)
    elif before_agent is None:
        root_agent.before_agent_callback = [scion_before_agent]


# ── main loop ────────────────────────────────────────────────


async def run(initial_message: str | None, resume: bool) -> None:
    """Main REPL loop using LHH's full build_runner()."""
    # Import horizon modules only AFTER env vars are loaded.
    from horizon.fast_api_app import build_runner  # type: ignore[import-not-found]

    runner = build_runner()

    # Inject Scion callbacks for status reporting.
    try:
        from horizon.agent import app  # type: ignore[import-not-found]

        _inject_scion_callbacks(app)
    except Exception as e:
        logger.warning("Could not inject Scion callbacks: %s", e)

    # Lazy import — google.genai.types is available after LHH installs google-genai.
    from google.genai import types  # type: ignore[import-not-found]

    if resume:
        saved_id = load_session_id()
        if saved_id:
            # Attempt to load existing session.
            session = await runner.session_service.get_session(
                app_name=APP_NAME, user_id=USER_ID, session_id=saved_id
            )
            if session is None:
                session = await runner.session_service.create_session(
                    app_name=APP_NAME, user_id=USER_ID
                )
        else:
            session = await runner.session_service.create_session(
                app_name=APP_NAME, user_id=USER_ID
            )
    else:
        session = await runner.session_service.create_session(
            app_name=APP_NAME, user_id=USER_ID
        )

    save_session_id(session.id)

    async def send(text: str) -> None:
        write_agent_status("thinking")
        content = types.Content(role="user", parts=[types.Part(text=text)])
        async for event in runner.run_async(
            user_id=USER_ID, session_id=session.id, new_message=content
        ):
            if event.content and event.content.parts:
                text_out = "".join(p.text or "" for p in event.content.parts)
                if text_out:
                    print(f"[{event.author}]: {text_out}", flush=True)
        write_agent_status("idle")

    if initial_message:
        print(f"[user]: {initial_message}", flush=True)
        await send(initial_message)

    while True:
        try:
            query = input("[user]: ")
        except (EOFError, KeyboardInterrupt):
            break
        if not query or not query.strip():
            continue
        if query.strip() == "exit":
            break
        await send(query)


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Run the LHH agent with Scion integration."
    )
    parser.add_argument(
        "--input",
        dest="initial_message",
        default=None,
        help="Initial message to send to the agent before entering interactive mode.",
    )
    parser.add_argument(
        "--resume",
        action="store_true",
        help="Resume the last session instead of creating a new one.",
    )
    args = parser.parse_args()

    # Set env vars from provisioner outputs BEFORE importing horizon.
    _load_provisioner_env()

    try:
        asyncio.run(run(args.initial_message, args.resume))
    except Exception:
        _report_crash(
            f"[lhh_scion_repl] Agent crashed:\n{traceback.format_exc()}"
        )
        sys.exit(1)


if __name__ == "__main__":
    main()
