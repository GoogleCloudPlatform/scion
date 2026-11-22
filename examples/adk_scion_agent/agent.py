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

"""Root agent definition for the ADK scion example.

This module defines an ADK LlmAgent that integrates with scion's lifecycle
management. It bridges auth environment variables, wires up status-reporting
callbacks, and exposes an EnvironmentToolset (for filesystem and shell access)
plus sciontool_status as callable tools.

When run with `adk run`, the agent operates as an interactive coding assistant
that reports its status to scion throughout its lifecycle.
"""

import os
from pathlib import Path

from google.adk import Agent
from google.adk.environment import LocalEnvironment
from google.adk.tools.environment import EnvironmentToolset

from . import callbacks, tools

# ---------------------------------------------------------------------------
# Auth bridging
# ---------------------------------------------------------------------------
# ADK requires GOOGLE_API_KEY for Gemini API access. Scion's Gemini harness
# provides GEMINI_API_KEY instead. Bridge the gap at import time.
if not os.environ.get("GOOGLE_API_KEY") and os.environ.get("GEMINI_API_KEY"):
    os.environ["GOOGLE_API_KEY"] = os.environ["GEMINI_API_KEY"]

# ---------------------------------------------------------------------------
# Model configuration
# ---------------------------------------------------------------------------
MODEL = os.environ.get("ADK_MODEL", "gemini-2.5-flash")

# ---------------------------------------------------------------------------
# Workspace configuration
# ---------------------------------------------------------------------------
# /workspace inside scion containers, otherwise CWD.
_WORKSPACE_DIR = Path(os.environ.get("WORKSPACE_ROOT", "/workspace"))
if not _WORKSPACE_DIR.exists():
    _WORKSPACE_DIR = Path.cwd()

# ---------------------------------------------------------------------------
# Agent instruction
# ---------------------------------------------------------------------------
AGENT_INSTRUCTION = """\
You are a coding assistant running inside a scion-managed container.

Your workspace is mounted at /workspace (or the current working directory if
running outside a container). You can read, create, and modify files there
using the environment tools (ReadFile, WriteFile, EditFile, Execute).

## Available tools

### Environment tools (provided by EnvironmentToolset)

- **ReadFile**: Read file contents from the workspace.
- **WriteFile**: Create or overwrite a file in the workspace.
- **EditFile**: Make surgical text replacements in an existing file.
- **Execute**: Run shell commands in the workspace directory.

### Scion lifecycle

- **sciontool_status(status_type, message)**: Signal lifecycle events to scion.
  - `"ask_user"` — ask your question, then call this as the last action of your
    turn so scion knows you are waiting for input.
  - `"blocked"` — call when you are intentionally waiting for something
    (e.g. an external process or a child agent to complete).
  - `"task_completed"` — call when you have finished the user's task.
    Summarize what you did.
  - `"limits_exceeded"` — call if you hit a resource or turn limit.

## Workflow

1. When you receive a task, work through it step by step.
2. Use the environment tools to read, create, and modify files as needed.
3. If you need clarification, ask your question, then call
   sciontool_status("ask_user", ...) as the last action of your turn.
4. If you are waiting on an external dependency, call
   sciontool_status("blocked", ...) with a reason.
5. When the task is complete, call sciontool_status("task_completed", ...)
   with a brief summary of what you accomplished.
"""

# ---------------------------------------------------------------------------
# Agent construction
# ---------------------------------------------------------------------------
root_agent = Agent(
    model=MODEL,
    name="scion_agent",
    instruction=AGENT_INSTRUCTION,
    tools=[
        EnvironmentToolset(
            environment=LocalEnvironment(working_dir=_WORKSPACE_DIR),
        ),
        tools.sciontool_status,
    ],
    before_agent_callback=callbacks.before_agent_callback,
    before_tool_callback=callbacks.before_tool_callback,
    after_tool_callback=callbacks.after_tool_callback,
    after_agent_callback=callbacks.after_agent_callback,
)
