---
name: team-creation
description: >-
  Create or extend scion agent team templates from a high-level description of roles and workflow.
  Use when the user describes a multi-agent team, panel, crew, pipeline, or any scenario
  requiring coordinated LLM agents with distinct roles. Also use when adding new roles to an
  existing team, modifying workflows, or restructuring agent coordination. Produces ready-to-use
  template directories in .scion/templates/ by default, or in a custom path if specified.
---

# Scion Team Creation & Extension Skill

You create and extend **scion agent templates** — the blueprints that define specialized agent roles. Given a description of a team (roles, responsibilities, workflow), you produce template directories that can be used to start agents with `scion start <name> --type <template>`.

When extending an existing team, review the current templates to understand the established patterns, naming, and workflow before making changes.

## Template Directory Structure

Each template lives in `.scion/templates/<template-name>/` and contains:

```
.scion/templates/<template-name>/
├── scion-agent.yaml       # REQUIRED: template metadata and config
├── agents.md              # REQUIRED: agent behavioral instructions
├── system-prompt.md       # REQUIRED: role persona and expertise framing
└── skills/                # OPTIONAL: harness skills (agentskills.io standard)
    └── <skill-name>/
        └── SKILL.md
```

Custom templates automatically **inherit** from the built-in `default` template, which provides shell configs, git setup, and base scion infrastructure. You do NOT need to create a `home/` directory or duplicate any infrastructure files.

### Template Naming

- Directory names use **kebab-case**: `panel-judge`, `code-reviewer`, `team-lead`
- Names should be descriptive of the role, not the project

## Required File Formats

### scion-agent.yaml

Every template MUST have this file. Minimal valid config:

```yaml
schema_version: "1"
description: "Short description of this agent role"
agent_instructions: agents.md
system_prompt: system-prompt.md
```

Optional fields you may include when the role requires them:

```yaml
max_turns: 50              # Maximum conversation turns
max_duration: "30m"        # Maximum wall-clock time (e.g. "1h", "30m")

env:
  ROLE: "reviewer"

services:
  - name: chromium
    command: ["chromium", "--headless", "--no-sandbox", "--remote-debugging-port=9222"]
    restart: always
    ready_check:
      type: tcp
      target: "localhost:9222"
      timeout: "10s"
```

### agents.md (Agent Instructions)

This file contains the behavioral instructions injected into the agent's context. 

The primary contents should focus on task and job work describing what the agent should do, how it should behave, and any constraints on its work.

Do **not** include instructions on how to use the scion CLI (starting agents, messaging, `scion look`, etc.) — every agent automatically receives base CLI operating instructions from the platform.

### system-prompt.md (Optional)

Frames the agent's persona, expertise, and perspective — *who* the agent is, while `agents.md` shapes *what* it does. Example:

```markdown
# Expert Code Reviewer

You are a senior software engineer with deep expertise in code quality,
security, and maintainability. You approach code review methodically,
prioritizing correctness and clarity over style preferences.
```

If the role doesn't need a distinct persona, omit this file and remove the `system_prompt` line from `scion-agent.yaml`.

This file can contain broad direction around skillsets.

## Harness Skills

Templates can include a `skills/` directory containing reusable skill definitions that follow the [agentskills.io](https://agentskills.io/) standard. Each skill is a subdirectory with a `SKILL.md` file:

```
skills/
└── my-skill/
    └── SKILL.md
```

A `SKILL.md` file has YAML frontmatter with `name` and `description`, followed by markdown content:

```markdown
---
name: my-skill
description: >-
  Short description of what this skill does and when the agent should use it.
---

Skill instructions and reference material here.
```

During agent provisioning, Scion automatically merges skills from the template into the correct harness-specific location (e.g. `.claude/skills/` for Claude, `.gemini/skills/` for Gemini). The agent's LLM harness discovers and loads these skills at runtime.

Use skills to package domain knowledge, tool-usage patterns, or specialized workflows that a role needs. Skills are portable across harnesses and reusable across templates.

## The Orchestrator Pattern

Every team MUST have exactly one **orchestrator** (also called lead, supervisor, or coordinator). This is the agent the user starts directly — it then creates and manages the other agents.

The orchestrator's `agents.md` should focus on:

- **What templates are available** and what each role does
- **The workflow**: when to start which agents, what tasks to give them, how to handle their results
- **Communication patterns**: what information flows between roles and when
- **Completion criteria**: how the orchestrator knows the overall task is done

Workers don't communicate directly with each other — the orchestrator reads output from one and relays relevant information to others.

### Orchestrator agents.md Structure

```markdown
[status reporting boilerplate]

## Role: Team Orchestrator

You are the orchestrator for [team description]. Your job is to:
1. [high-level workflow step 1]
2. [high-level workflow step 2]

## Available Agent Roles

- `<template-1>`: [what this role does, expected input, what it produces]
- `<template-2>`: [what this role does, expected input, what it produces]

## Workflow

[Step-by-step instructions: when to create agents, what to tell them,
how to handle results, when the overall task is complete.]
```

## Creating a New Team

### 1. Analyze the Description

Identify from the user's description:
- **Roles**: What distinct agent types are needed?
- **Workflow**: How do the agents interact? Sequential pipeline, parallel work, debate, review cycle?
- **Orchestration**: Who starts whom? Who collects results?

### 2. Design the Team Structure

- Identify one role as the **orchestrator** — the entry point the user will start
- All other roles are **workers** — started and managed by the orchestrator
- Map out the communication flow

### 3. Create Templates

For each role, create the template directory with its files. Write worker templates first, then the orchestrator (since it references worker template names).

### 4. Add Skills When Appropriate

If a role needs specialized domain knowledge or tool-usage patterns that would benefit from being a discrete, reusable skill file rather than inline in `agents.md`, add it to the template's `skills/` directory.

### 5. Validate

- [ ] Every template has `scion-agent.yaml` with `schema_version: "1"`
- [ ] Every `agents.md` starts with the status reporting boilerplate
- [ ] The orchestrator references the correct template names
- [ ] Template directory names are kebab-case
- [ ] The workflow matches the user's intent
- [ ] No CLI usage instructions are duplicated in templates

## Extending an Existing Team

When adding to or modifying an existing team:

### 1. Review Current State

Read the existing templates in `.scion/templates/` to understand:
- The current roles and their responsibilities
- The orchestrator's workflow and how it references workers
- Naming conventions and patterns already established
- Any existing skills in template `skills/` directories
- Skill may be hard copied between templates if they are relevant to more than one type.

### 2. Determine the Change

- **Adding a role**: Create a new worker template, then update the orchestrator's `agents.md` to reference it — add the new template to the "Available Agent Roles" section and integrate it into the workflow.
- **Modifying a role**: Update the existing template's `agents.md` and/or `system-prompt.md`. Check if the orchestrator's workflow needs adjustment.
- **Changing workflow**: Update the orchestrator's `agents.md` workflow section. May require changes to worker instructions if handoff patterns change.
- **Adding capabilities**: Consider whether the new capability belongs as inline instructions in `agents.md` or as a discrete skill in the template's `skills/` directory.

### 3. Maintain Consistency

- Follow the naming conventions already in use
- Keep the communication patterns consistent (workers report to orchestrator)
- Update the orchestrator whenever worker templates change

## Gotchas

- **Don't duplicate CLI usage instructions**. Every agent already receives base scion CLI instructions from the platform.
- **Don't create `home/` directories** in custom templates. The default template provides all infrastructure.
- **Template names = directory names**. The `description` in `scion-agent.yaml` is cosmetic; the `--type` value is the directory name.
- **Skills are harness-portable**. Write `SKILL.md` content without assuming a specific harness — scion mounts skills into the correct location automatically.
