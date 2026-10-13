---
title: Harness Development
description: How to add support for new LLM tools to Scion.
---

A harness is the bridge between the Scion orchestrator and a specific LLM-based tool (like Claude Code or Gemini CLI).

## Communication with Sciontool

Harnesses running inside the agent container interact with the orchestrator primarily through the `sciontool` utility. This tool is injected into every agent container at `/usr/local/bin/sciontool`.

### Reporting Agent Status

The `sciontool status` command is used by the harness to signal state changes. This ensures that the Hub and Web Dashboard have accurate information about what the agent is doing.

#### Waiting for User Input
When the harness requires human intervention (e.g., for a confirmation or a question), it should call:
```bash
sciontool status ask_user "I need clarification on the requirements."
```
This updates the agent's state to `WAITING_FOR_INPUT` and logs the message.

#### Task Completion
When the harness has finished its work (successfully or with an error), it should call:
```bash
sciontool status task_completed "Implemented the requested feature."
```
This updates the state to `COMPLETED` and triggers final telemetry collection.

### Processing Harness Hooks

If your harness supports JSON-based hook events (like Gemini CLI or Claude Code), you can pipe these events directly into `sciontool hook`. It will automatically handle status updates, telemetry, and logging.

```bash
# Process a Gemini CLI hook event
echo '{"hook_event_name": "BeforeTool", "tool_name": "shell"}' | sciontool hook --dialect=gemini
```

Supported dialects: `claude`, `gemini`.

### Unified Logging

Harnesses should write their logs to `/home/scion/agent.log`. `sciontool` automatically configures this file with appropriate permissions during initialization.

For structured logging from shell scripts, you can use `sciontool` directly:

```bash
# Log an info message
sciontool log info "Starting specialized task..."

# Log a debug message (only visible if SCION_LOG_LEVEL=debug)
sciontool log debug "Internal state: $STATE"
```

## Lifecycle Hooks

Harnesses can implement lifecycle hooks defined in `scion-agent.yaml`. These hooks are executed by `sciontool` during the agent's initialization and shutdown phases.

- `pre-start`: Run before the main harness process starts.
- `post-start`: Run in the background after the harness starts.
- `pre-stop`: Run when a stop signal is received.
- `session-end`: Run after the harness process exits.

Example configuration in `scion-agent.yaml`:
```yaml
hooks:
  pre-start: ["/home/scion/scripts/setup-db.sh"]
  session-end: ["/home/scion/scripts/cleanup.sh"]
```

