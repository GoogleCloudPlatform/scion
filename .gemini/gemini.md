# Scion Project Context

## Overview
`scion` is a container-based orchestration tool designed to manage concurrent LLM-based code agents. It provides isolation, parallelism, and context management (via git worktrees) for multiple specialized agents running on local machines (Docker/macOS) or remote clusters (Kubernetes).

## Core Technologies
- **Language**: Go (Golang)
- **CLI Framework**: [Cobra](https://github.com/spf13/cobra)
- **Runtimes**:
  - **macOS**: Apple Virtualization Framework (via `container` CLI)
  - **Linux/Generic**: Docker
  - **Cloud**: Kubernetes (Experimental)
- **Harnesses**:
  - **Gemini**: Logic for interacting with Gemini CLI.
  - **Claude**: Logic for interacting with Claude Code.
  - **Generic**: A base harness for other LLM interfaces.
- **Workspace Management**: Git Worktrees for concurrent, isolated code modification.

## Project Structure
- `cmd/`: CLI command definitions (using Cobra). Each file corresponds to a `scion` subcommand.
- `pkg/`: Core logic implementation.
  - `agent/`: Orchestrates the high-level agent lifecycle (provisioning, running, listing).
  - `harness/`: Interaction logic for specific LLM agents (Gemini, Claude).
  - `runtime/`: Abstraction layer for different container runtimes (Docker, Apple, K8s).
  - `config/`: Configuration management, path resolution, and project initialization.
    - `embeds/`: **CRITICAL** - Contains the source files for agent templates (bashrc, settings, etc.) that are seeded into `.scion/` during `init`.
  - `k8s/`: Kubernetes-specific client and API types.
  - `api/`: Shared types and interfaces.
- `.design/`: Design specifications and architectural documents.

## Development Guidelines
- **Idiomatic Go**: Follow standard Go patterns and naming conventions.
- **Adding Commands**: New CLI commands must be added to `cmd/` using Cobra.
- **Updating Templates**: **DO NOT** manually update the `.scion/` folder in this repo to change default behavior. Instead:
  1. Modify the source files in `pkg/config/embeds/`.
  2. The seeding logic in `pkg/config/init.go` uses `//go:embed` to package these files.
- **Runtime Abstraction**: When adding new runtime features, ensure they implement the `Runtime` interface in `pkg/runtime/interface.go`.
- **Harness Logic**: LLM-specific interactions should be encapsulated in `pkg/harness`.

## Project use of the scion tool itself
Do not commit changes in the project's own `.scion` folder to git as part of committing progress on code and docs. These are managed and committed manually when template defaults are intentionally updated.

When working on this project, you can use `scion` to manage sub-agents for specific tasks like "refactor pkg/runtime" or "audit pkg/auth".
