---
name: scion
description: Manage concurrent LLM-based code agents with scion - orchestrate parallel agents with isolated workspaces
---

# Scion Agent Management Skill

Scion is a container-based orchestration tool for managing concurrent LLM-based code agents. It enables parallel execution of specialized sub-agents with isolated identities, credentials, and workspaces.

## Core Concepts

### Projects
A **project** is the grouping construct for agents, represented by the `.scion` directory. Each project can have its own project, and there's also a global project in `~/.scion/`.

### Agents
An **agent** is an isolated LLM instance running in a container with its own workspace (git worktree), credentials, and configuration.

### Templates
**Templates** are blueprints for creating agents. Common templates include:
- `gemini` - Gemini CLI-based agents
- `claude` - Claude Code-based agents
- `opencode` - OpenCode-based agents
- `codex` - Codex-based agents

### Harnesses
A **harness** is the LLM interface (Gemini CLI, Claude Code, etc.) that the agent uses.

## Command Reference

### Initialization

```bash
# Initialize a project in the current project (creates .scion directory)
scion init

# Initialize the global project
scion init --global
```

### Starting Agents

```bash
# Start an agent with a task
scion start <agent-name> <task description>

# Start with a specific template
scion start <agent-name> "task" --type claude

# Start and immediately attach to the session
scion start <agent-name> "task" --attach

# Start with a custom branch
scion start <agent-name> "task" --branch feature-branch

# Start with a custom workspace path
scion start <agent-name> "task" --workspace /path/to/workspace
```

### Listing Agents

```bash
# List agents in the current project
scion list

# List all agents across all projects
scion list --all

# Output as JSON
scion list --format json
```

Output columns:
- NAME: Agent name
- TEMPLATE: Template used (gemini, claude, etc.)
- RUNTIME: Execution runtime (docker, container, k8s)
- PROJECT: Project name
- AGENT STATUS: Agent state
- SESSION: Session status
- CONTAINER: Container status

### Interacting with Agents

```bash
# Attach to an agent's interactive session
scion attach <agent-name>

# Send a message to an agent
scion message <agent-name> "Your message here"

# Send message with interrupt (stops current work first)
scion message <agent-name> "Urgent task" --interrupt

# Broadcast message to all agents in current project
scion message --broadcast "Stop and report status"

# Broadcast to all agents across all projects
scion message --all "Global announcement"
```

### Viewing Logs

```bash
# View agent logs
scion logs <agent-name>
```

### Stopping and Resuming

```bash
# Stop an agent
scion stop <agent-name>

# Stop and remove the agent
scion stop <agent-name> --rm

# Resume a stopped agent
scion resume <agent-name>

# Resume with attach
scion resume <agent-name> --attach
```

### Deleting Agents

```bash
# Delete an agent (stops container, removes directory and worktree)
scion delete <agent-name>

# Delete but preserve the git branch
scion delete <agent-name> --preserve-branch

# Delete all stopped agents
scion delete --stopped
```

### Workspace Synchronization

```bash
# Sync workspace (direction depends on sync mode)
scion sync <agent-name>

# Sync to the agent container
scion sync to <agent-name>

# Sync from the agent container
scion sync from <agent-name>
```

### Template Management

```bash
# List available templates
scion templates list

# Show template configuration
scion templates show <template-name>

# Create a new template
scion templates create <name> --harness gemini

# Clone an existing template
scion templates clone <source> <destination>

# Delete a template
scion templates delete <name>

# Update default templates from binary
scion templates update-default
```

### Configuration

```bash
# List all effective settings
scion config list

# Get a specific setting
scion config get <key>

# Set a local setting (in current project)
scion config set <key> <value>

# Set a global setting
scion config set <key> <value> --global
```

## Common Workflows

### Parallel Task Execution

To run multiple agents in parallel on different tasks:

```bash
# Start multiple agents for parallel work
scion start coder "Implement the new API endpoint"
scion start tester "Write tests for the auth module"
scion start auditor "Review security of user input handling" --type claude

# Check status of all agents
scion list

# Attach to any agent to monitor or interact
scion attach coder
```

### Agent Collaboration Pattern

When coordinating work across agents:

1. Start agents for different subtasks
2. Use `scion list` to monitor progress
3. Use `scion message` to communicate new information
4. Use `scion attach` when human intervention is needed
5. Use `scion logs` to review work history

### Cleanup

```bash
# Delete all stopped agents at once
scion delete --stopped

# Delete specific agent, keeping its branch for review
scion delete my-agent --preserve-branch
```

## Global Flags

These flags work with most commands:

- `--project, -g <path>`: Specify a project directory
- `--global`: Use the global project (~/.scion/)
- `--profile, -p <name>`: Use a specific configuration profile
- `--format <type>`: Output format (json, plain) - currently for list only

## Tips for Agents

1. **Check existing agents first**: Before starting a new agent, use `scion list` to see what's already running.

2. **Use descriptive names**: Agent names should reflect their purpose (e.g., `refactor-auth`, `test-api`, `audit-security`).

3. **Choose appropriate templates**: Use `--type claude` for Claude Code, default is Gemini CLI.

4. **Monitor with logs**: Use `scion logs <agent>` to check progress without interrupting.

5. **Interrupt carefully**: The `--interrupt` flag on messages stops current work - use only when necessary.

6. **Preserve branches**: When deleting agents whose work might need review, use `--preserve-branch`.

7. **Use attach for complex interactions**: When an agent needs guidance, `scion attach` provides full interactive access.
