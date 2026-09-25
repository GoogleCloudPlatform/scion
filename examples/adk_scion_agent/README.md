# ADK Scion Agent Example

An example [ADK (Agent Development Kit)](https://google.github.io/adk-docs/) agent that integrates with scion's lifecycle management. The agent reports its status through scion's `sciontool` so it can be orchestrated alongside other agents in a project.

## Prerequisites

- Python 3.11+
- `google-adk` from git HEAD (see `requirements.txt`; install with `pip install -r requirements.txt`). The `EnvironmentToolset` used by this agent is not yet in a released PyPI version.
- A Google AI API key or Vertex AI credentials

## Quick Start (Standalone)

```bash
# From the repository root:
cp examples/adk_scion_agent/.env.example examples/adk_scion_agent/.env
# Edit .env and set GOOGLE_API_KEY

# Interactive mode (no initial task):
cd examples
python -m adk_scion_agent

# With an initial task via --input:
python -m adk_scion_agent --input "write a hello world script"
```

The agent starts an interactive session. Type a task and the agent will work through it, using ADK's environment tools (ReadFile, WriteFile, EditFile, Execute) to interact with the workspace and `sciontool_status` to signal lifecycle events. The `--input` flag sends an initial message before entering the interactive loop.

When running outside a scion container, `sciontool` won't be on PATH — the agent works normally but status reporting is silently skipped.

## Container Image

The included `Dockerfile` builds on `scion-base` (which provides sciontool, tmux, git, and Python 3):

```bash
docker build -t scion-adk-agent examples/adk_scion_agent/
```

The image installs `google-adk` into a virtualenv and copies the agent source to `/opt/adk_scion_agent/`. The default CMD is `python -m adk_scion_agent`, which uses a custom runner that supports `--input` for task delivery.

## Deploying via Scion Template

A ready-to-use template is provided in `templates/adk/`. To deploy this agent in a project:

```bash
# Copy the template into your project's .scion directory
cp -r examples/adk_scion_agent/templates/adk .scion/templates/adk

# Copy the harness-config (or place it globally at ~/.scion/harness-configs/adk/)
cp -r examples/adk_scion_agent/templates/adk/harness-configs/adk .scion/harness-configs/adk

# Start an agent using the template
scion start my-agent --template adk
```

The template uses the **generic** harness with `args` set to `["python", "-m", "adk_scion_agent"]` and `task_flag: "--input"`. When scion starts the agent with a task, it appends `--input <task>` to the command. The generic harness passes these as the container command, and scion wraps it in a tmux session for message delivery.

## Running Inside a Scion Container

When scion launches this agent inside a container:

1. **sciontool** runs as PID 1 and supervises the agent process.
2. The agent writes transient activity updates (`thinking`, `executing`, `idle`) to `$HOME/agent-info.json` via ADK callbacks.
3. Sticky activity transitions (`waiting_for_input`, `blocked`, `completed`, `limits_exceeded`) go through `sciontool status` which also reports to the scion Hub.
4. **Message delivery** works natively: `scion message` sends text via tmux `send-keys` into ADK's `input()` loop.

### Status Lifecycle

```
User sends message
    │
    ▼
thinking          ← before_agent_callback
    │
    ├──► executing    ← before_tool_callback (WriteFile, Execute, etc.)
    │        │
    │        ▼
    │    thinking     ← after_tool_callback
    │        │
    │   (more tools...)
    │
    ▼
idle              ← after_agent_callback

If agent calls sciontool_status("task_completed", ...):
    → completed (sticky — survives subsequent transient updates)

If agent calls sciontool_status("ask_user", ...):
    → waiting_for_input (sticky — cleared when user responds)

If agent calls sciontool_status("blocked", ...):
    → blocked (sticky — cleared when user responds)
```

## Auth Bridging

Scion's Gemini harness sets `GEMINI_API_KEY`. ADK requires `GOOGLE_API_KEY`. The agent bridges this automatically at import time — if `GOOGLE_API_KEY` is unset but `GEMINI_API_KEY` is available, it copies the value over.

For Vertex AI, set `GOOGLE_GENAI_USE_VERTEXAI=true` and configure Application Default Credentials. See `.env.example` for all options.

## Tools

| Tool | Source | Purpose |
|---|---|---|
| `ReadFile` | EnvironmentToolset | Read file contents from the workspace. |
| `WriteFile` | EnvironmentToolset | Create or overwrite a file in the workspace. |
| `EditFile` | EnvironmentToolset | Make surgical text replacements in an existing file. |
| `Execute` | EnvironmentToolset | Run shell commands in the workspace directory. |
| `sciontool_status(status_type, message)` | Custom | Signal `ask_user`, `blocked`, `task_completed`, or `limits_exceeded` to scion. |

## Project Structure

```
adk_scion_agent/
├── Dockerfile         # Container image (built on scion-base)
├── __init__.py        # ADK package entry point (exports root_agent)
├── __main__.py        # python -m adk_scion_agent entrypoint
├── run.py             # Custom runner with --input flag support
├── requirements.txt   # Python dependencies (google-adk>=1.28.0)
├── agent.py           # root_agent definition, auth bridging, model config
├── tools.py           # sciontool_status tool
├── callbacks.py       # ADK callbacks → scion activity updates
├── sciontool.py       # Low-level sciontool subprocess wrapper
├── .env.example       # Environment variable template
├── README.md          # This file
└── templates/
    └── adk/
        ├── scion-agent.yaml           # Template definition
        ├── agents.md                  # Agent instructions (sciontool lifecycle)
        └── harness-configs/
            └── adk/
                └── config.yaml        # Generic harness config (image + args)
```
