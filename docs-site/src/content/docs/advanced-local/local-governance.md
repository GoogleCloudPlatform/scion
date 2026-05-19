---
title: Local Configuration
description: Configuring Scion for local development workflows.
---

**What you will learn**: How to configure Scion beyond the defaults using `settings.yaml`, including custom profiles, local runtime overrides, and advanced workspace behaviors.

When running Scion in **Solo Mode** (local-only), your configuration focuses on defining the environment in which your agents run. This guide explains how to use `settings.yaml` to customize your local workflow.

## The Settings File

Scion looks for `settings.yaml` in two places:
1.  **Global**: `~/.scion/settings.yaml` (Apply to all projects)
2.  **Project**: `.scion/settings.yaml` (Apply to the current project)

## Core Concepts

### Profiles
Profiles are the primary way to switch contexts. You might have a `local` profile for debugging and a `high-power` profile for heavy tasks. The defaults are `local` and `remote`.

```yaml
active_profile: local

profiles:
  local:
    runtime: docker
    default_template: gemini
```

You can switch profiles using the `--profile` flag:
```bash
scion start my-agent --profile local
```

### Runtimes
Runtimes define *where* the agent runs. For local development, this is usually **Docker**, **podman**, or **Apple Virtualization**.

```yaml
runtimes:
  docker:
    type: docker
    host: "unix:///var/run/docker.sock"

  # Daemonless/Rootless
  podman:
    type: podman
    
  # Apple Silicon only
  container:
    type: container
```

### Harness Configs
Harness Configs define *what* agent harness runs, with what configurations. They map a logical name (like `gemini`) to a specific container image and configuration for that harness.

```yaml
harness_configs:
  gemini:
    harness: gemini
    image: "us-central1-docker.pkg.dev/.../scion-gemini:latest"
    user: scion
    
  gemini-dev:
    harness: gemini
    image: "gemini:local-dev"
    env:
      DEBUG: "true"
```

## Common Local Customizations

### Injecting Environment Variables
You can inject host environment variables into all agents running under a specific profile.

```yaml
profiles:
  local:
    env:
      # Pass through host credentials
      GITHUB_TOKEN: "${GITHUB_TOKEN}"
      OPENAI_API_KEY: "${OPENAI_API_KEY}"
```

### Mounting Local Directories
For development, you might want to mount a local directory (like a shared library) into your agents.

```yaml
harness_configs:
  gemini-with-lib:
    harness: gemini
    volumes:
      - source: "/Users/me/code/shared-lib"
        target: "/home/scion/shared-lib"
        read_only: true
```

This is also useful if you want to mount common build caches, such as:

```
    volumes:
        - source: ${GOPATH}/pkg
          target: /home/scion/go/pkg
        - source: /Users/me/Library/Caches/go-build
          target: /home/scion/.cache/go-build
```

## Troubleshooting

- **Check Active Profile**: Run `scion config list` to see resolved settings.
- **Variable Substitution**: Environment variables in `settings.yaml` use the `${VAR}` syntax.
