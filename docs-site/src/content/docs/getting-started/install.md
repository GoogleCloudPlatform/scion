---
title: Installation
---

**What you will learn**: How to get Scion running on your machine from scratch with zero configuration, allowing you to start your first agent immediately.

This guide covers the steps to install and configure Scion on your local machine.

## Prerequisites

### 1. Go
Scion is written in Go. You need Go 1.22 or later installed.
- [Download and install Go](https://golang.org/doc/install)

While a binary may be available from the github releases page, this is an active project and it is currently best to regularly build from source.

### 2. Container Runtime
Scion requires a container runtime to manage agents. You can use Docker, Podman, or the Apple Virtualization Framework (experimental).

#### Docker (Linux/Windows)
- Install [Docker Desktop](https://www.docker.com/products/docker-desktop) or [Docker Engine](https://docs.docker.com/engine/install/).
- Ensure the `docker` command is available in your PATH.

#### Podman (Linux/macOS)
- Install [Podman](https://podman.io/docs/installation).
- Ensure the `podman` command is available in your PATH.
- On Linux, Scion supports rootless Podman out of the box.
- On macOS, ensure `podman machine` is initialized and running.

#### Apple Virtualization (macOS only)
- Requires the [container](https://github.com/apple/container/releases) tool (an Apple tool for running OCI images in micro VMs).
- Ensure the `container` command executes.
- Start the system services `container system start`.

### 3. Git
Scion uses `git worktree` to manage agent workspaces.
- Ensure `git` is installed and available in your PATH.
- Because Scion uses a new feature for relative path worktrees, ensure that `git --version` >= 2.47.0.

For Ubuntu you can install the latest version with

```bash
add-apt-repository ppa:git-core/ppa

apt update; apt install git
```

For Debian you may need to build from source, see the [git site](https://git-scm.com/install/source), or see the Dockerfile in this repo for the base image.

---

## Scion Installation

### From Source
You can install Scion directly using `go install`:

```bash
go install github.com/GoogleCloudPlatform/scion/cmd/scion@latest
```

Ensure your `$GOPATH/bin` (typically `~/go/bin`) is in your system `$PATH`.

### From Clone
If you have the repository cloned, you can use the provided `Makefile`:

```bash
make build
# This creates a 'scion' binary in the current directory.
# You can move it to a directory in your PATH:
mv scion /usr/local/bin/
```

To verify your installation, run:

```bash
scion version
```

---

## Build container images

No publicly hosted images are currently available for Scion, but quick and easy build scripts are included.

The easiest way to get these images is to fork this repo, and then go to the "Actions" tab and select the "Build Scion Images" workflow.

You will then use your `ghcr.io/myorg` registry for the scion setting. These images are required during initialization.

See [Building Containers](/scion/advanced-local/custom-images/) for more details

## Configuration

### 1. Initialize your machine
You must first establish global settings, templates and configs for your machine

```bash
scion init --machine
```

This creates a directory at `~/.scion`

You will be prompted for the image registry where you have built and deployed the images in the previous step.

### 2. Initialize a Grove
Navigate to the root of a project where you want to use Scion and run:

```bash
scion init
```

This creates a `.scion` marker file in the directory, linking it to structures inside the global folder created on the machine initialization.


### 3. Agent Authentication (LLM Access)

Before starting an agent, you must provide credentials so the underlying LLM harness (Claude, Gemini, etc.) can authenticate with its model provider.

Scion uses a **unified authentication pipeline** that automatically discovers credentials from your environment. For a quick start, export your provider's API key:

```bash
# For Claude
export ANTHROPIC_API_KEY="your-api-key"

# For Gemini
export GEMINI_API_KEY="your-api-key"
```

Scion also supports Vertex AI (via Application Default Credentials) and OAuth token files. For advanced credential configurations, including Hub-based secret injection, see [Agent Credentials](/scion/advanced-local/agent-credentials/).

### 4. Select Runtime
Scion automatically selects the appropriate runtime based on your operating system:
- **macOS**: Defaults to `container` (Apple Virtualization Framework).
- **Linux/Windows**: Defaults to `docker` (or `podman` if Docker is missing).

If you wish to change this (e.g., to use Podman on macOS), you can manually edit `.scion/settings.yaml`:

```yaml
{
  "profiles": {
    "local": {
      "runtime": "podman"
    }
  }
}
```

---

## Shell Completions

Scion provides shell completions. These are highly recommended as they are very useful when providing proper descriptive agent names.

For setup instructions, see [Shell Completions](/scion/advanced-local/completions/).

---

## Troubleshooting

### `git worktree` errors
Ensure your project is a git repository. `scion init` and `scion start` require being inside a git repository to manage workspaces.

### Permission Denied (Docker)
Ensure your user has permissions to run Docker commands without `sudo`. On Linux, add your user to the `docker` group.

### Path Issues
If `scion` command is not found after `go install`, add the following to your shell profile (`.zshrc` or `.bashrc`):

```bash
export PATH=$PATH:$(go env GOPATH)/bin
```
