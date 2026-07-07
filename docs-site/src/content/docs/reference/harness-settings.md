---
title: Harness-Specific Settings
---

This document describes how to configure individual LLM tools and harnesses inside a Scion agent.

## Purpose
While Scion manages the orchestration and execution of containers, the tools running *inside* those containers (like the Gemini CLI or Claude Code) often have their own configuration systems.

## Locations
Each agent has a dedicated "Home" directory that is mounted into the container. Harness-specific settings are typically found in a hidden subdirectory:
- **Gemini**: `/home/gemini/.gemini/settings.json`
- **Claude**: `/home/claude/.claude.json` (or similar)
- **Opencode**: `/home/opencode/opencode.json`

## Seeding from Harness-Configs & Templates
When an agent is created, Scion composes its home directory by layering files from multiple sources:
1.  **Harness-Config**: Base settings for the specific LLM tool (from `~/.scion/harness-configs/<name>/home/`).
2.  **Template**: Role-specific prompts and configuration (from `.scion/templates/<name>/home/`).
3.  **Common Files**: Shared dotfiles like `.tmux.conf` and `.zshrc`.

This multi-layered approach allows you to define a "base" Gemini configuration once, and then overlay different "roles" (like Code Reviewer or Security Auditor) on top of it.

## Key Concepts
- **Tools**: Allowlists of local or remote functions the LLM is permitted to call.
- **Profiles**: Harness-level profiles (distinct from Scion profiles) that control model parameters.
- **Credentials**: How API keys are injected and stored within the harness-specific configuration. Auth type selection uses universal Scion types (`api-key`, `auth-file`, `vertex-ai`) set via `auth_selectedType` in the settings profile. Scion translates these to harness-native values during provisioning (e.g., `api-key` becomes `gemini-api-key` in Gemini's `settings.json`). See [Agent Credentials](/scion/local/agent-credentials/) for the full credential pipeline.
