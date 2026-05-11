---
title: Working with Templates & Harnesses
---

Scion separates the **role** of an agent (what it does) from its **execution mechanics** (how it runs). This is achieved through two complementary concepts: **Templates** and **Harness-Configs**.

## Core Concepts

### 1. Templates (The "Role")
A template defines the agent's purpose, personality, and instructions. It is **harness-agnostic**, meaning a "Code Reviewer" template can theoretically run on Claude, Gemini, or any other LLM.

A template typically contains:
- `scion-agent.yaml`: The agent definition (resources, env vars).
- `agents.md`: Operational instructions for the agent.
- `system-prompt.md`: The core persona and role definition.
- `home/`: Optional portable configuration files (e.g., linter configs).

### 2. Harness-Configs (The "Mechanics")
A harness-config defines the runtime environment and tool-specific settings. It includes the base files required by the underlying tool (e.g., `.claude.json` for Claude, `.gemini/settings.json` for Gemini).

Harness-configs live in `~/.scion/harness-configs/` and contain:
- `config.yaml`: Runtime parameters (container image, model, auth).
- `home/`: Base files that are copied to the agent's home directory.

### 3. Composition
When you create an agent, Scion composes the final environment by layering:
1.  **Harness-Config Base Layer**: The foundation (e.g., Gemini CLI settings).
2.  **Template Overlay**: The role definition (prompts, instructions).
3.  **Profile/Runtime Overrides**: User-specific tweaks.

## Creating an Agent

To create an agent, you specify both a template and a harness-config.

```bash
# Explicitly specify both
scion create my-agent --template code-reviewer --harness-config gemini

# Use the template's default harness-config (if defined)
scion create my-agent --template code-reviewer

# Use the system default harness-config (from settings.yaml)
scion create my-agent --template code-reviewer
```

### Resolution Order
Scion determines which harness-config to use in this order:
1.  CLI flag: `--harness-config`
2.  Template default: `default_harness_config` in `scion-agent.yaml`
3.  System default: `default_harness_config` in global `settings.yaml`

## Managing Templates

### Template Bootstrapping

Local agent templates are automatically bootstrapped into the Hub database during server startup. This ensures that all defined templates are consistently available across the system, allowing for seamless deployment without manual importing steps.

### Template Traceability

To provide clear visibility into the exact configuration version associated with each agent, Scion captures and displays template IDs and hashes. When you view an agent's details in the CLI (`scion list`, `scion info`) or the Web UI, you can see the precise template version that was used to provision it.

### Structure of a Template
A typical template directory looks like this:

```text
my-template/
├── scion-agent.yaml      # Configuration
├── agents.md             # Instructions
├── system-prompt.md      # Persona
└── home/                 # Portable files (optional)
    └── .config/
        └── my-tool.conf
```

**`scion-agent.yaml` Example:**

```yaml
schema_version: "1"
name: code-reviewer
description: "Thorough code review agent"

agent_instructions: agents.md
system_prompt: system-prompt.md
default_harness_config: gemini

env:
  REVIEW_STRICTNESS: high

resources:
  requests:
    cpu: "500m"
    memory: "512Mi"
```

### Template Commands

```bash
# List available templates
scion templates list

# Create a new template
scion templates create my-new-role

# Clone an existing template
scion templates clone code-reviewer my-custom-reviewer

# Import definitions from Claude or Gemini sub-agents
scion templates import .claude/agents/code-reviewer.md

# Delete a template
scion templates delete my-old-template
```

## Managing Harness-Configs

Harness-configs are directories stored in `~/.scion/harness-configs/` (global) or `.scion/harness-configs/` (project-level).

### Customizing a Harness-Config
To change the default model or add custom hooks for a specific harness, edit the files directly in the harness-config directory.

**Example: Changing the Gemini model**
Edit `~/.scion/harness-configs/gemini/config.yaml`:

```yaml
harness: gemini
model: gemini-1.5-pro
# ...
```

**Example: Adding a persistent CLI flag**
Edit `~/.scion/harness-configs/gemini/home/.gemini/settings.json`.

### Creating Variants
You can create custom variants of harness-configs by copying the directory.

```bash
cp -r ~/.scion/harness-configs/gemini ~/.scion/harness-configs/gemini-experimental
```

Now you can use this variant:
```bash
scion create test-agent --harness-config gemini-experimental
```

### Resetting Defaults
If you mess up a harness-config, you can restore the factory defaults:

```bash
scion harness-config reset gemini
```

## Harness Skills

Templates and harness-configs can define **skills** — reusable instruction snippets that are automatically merged and mounted into the appropriate harness-specific directory during agent provisioning.

When an agent is created, Scion collects skills from both the template and the harness-config, then mounts them into the correct location for the harness:

| Harness | Skills Directory |
| :--- | :--- |
| Claude | `.claude/skills/` |
| Gemini | `.gemini/skills/` |

This allows you to package domain-specific expertise (e.g., coding standards, review checklists) as portable skill files that any template can reference.

### Defining Skills in a Template

Add skill files to the template's `home/` directory under the harness-specific path:

```text
my-template/
├── scion-agent.yaml
├── agents.md
└── home/
    └── .claude/
        └── skills/
            └── code-standards.md
```

Skills from both the template and the harness-config are merged at provisioning time, so you can define common skills in the harness-config and role-specific skills in individual templates.

### The `team-creation` Skill

Scion includes a specialized built-in skill called `team-creation`. This skill is designed for generating coordinated multi-agent template sets. It simplifies the creation of orchestrator-worker patterns by providing best-practice guidance for agent-to-agent communication and template structure, allowing an agent to quickly scaffold a team of specialized sub-agents.

## Template Importing (Hub Integration)

When connected to a Hub, you can import templates into your projects. This process is now a direct, high-performance server-side operation that provides immediate feedback, replacing the older container-based sync mechanism.

### Direct Server-Side Import

You can initiate a template import directly from the Web UI using the **Load Templates** button on the project details page, or programmatically via the Hub API:

```
POST /api/v1/projects/{projectId}/import-templates
```

*(Note: The legacy `sync-templates` endpoint has been removed.)*

### Supported Sources and Deep Paths

The import system supports a wide variety of sources, allowing you to pull templates from specific locations:

- **Git Repositories:** Standard HTTPS clones are supported.
- **Deep GitHub Paths:** You can import from specific subdirectories within a repository (e.g., `https://github.com/my-org/templates/tree/main/path/to/templates`). This relies on GitHub's tarball API via HTTPS for high reliability in restricted environments without requiring `svn` or `git` binaries.
- **Archives:** Direct import from `.zip` or `.tar.gz` archive URLs.
- **Rclone URIs:** Support for importing from cloud storage via rclone URIs (e.g., `:gcs:my-bucket/templates`).

### Native Scion Templates

The import process automatically detects and performs direct "copy" imports of native Scion templates (those containing a `scion-agent.yaml`), bypassing any conversion steps needed for generic templates.

### Automatic Sync on Hub Startup

Local templates are automatically bootstrapped into the Hub during server startup. This ensures all defined templates are consistently available across distributed brokers without manual intervention.

### Web UI Management

Once imported, templates can be directly managed via the Scion Web UI. The dashboard provides comprehensive **template file browsing, inline editing (with Markdown preview), and upload capabilities**, allowing you to refine agent roles and instructions without leaving the browser.
