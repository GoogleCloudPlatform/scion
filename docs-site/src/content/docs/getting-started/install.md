---
title: Installation
---

**What you will learn**: How to get Scion running on your machine from scratch, up to the point where you can start your first agent.

This guide covers the steps to install and configure Scion on your local machine. Scion runs
agents through a hub. On your own machine that is [Workstation mode](/scion/choosing-a-mode/):
the combo server (Hub, Runtime Broker and web dashboard) on loopback, started with
`scion server start`. To use a shared hub instead, install the CLI here and then follow
[Connecting to a Hub](/scion/hosted/user/hosted-user/). If you are not sure which you want, read
[Choosing a Mode](/scion/choosing-a-mode/) first.

:::tip[Prefer a guided setup?]
For Workstation mode, the [Onboarding Wizard](/scion/getting-started/onboarding/) walks you
through the whole setup in your browser — no config files to edit. Install Scion with Homebrew
(below), then run `scion server start`.
:::

## Install with Homebrew (recommended)

The easiest way to get a ready-to-run Scion — CLI plus embedded web UI — is the community
[homebrew-scion](https://github.com/homebrew-scion/homebrew-scion) tap:

```bash
brew tap homebrew-scion/scion
brew install homebrew-scion/scion/scion
```

This installs:

- **`scion`** — the main CLI, pre-configured to use `ghcr.io/homebrew-scion` as the default
  image registry, so the harness images are pulled for you.
- **`scion-plugin-telegram`** — the Telegram broker plugin, installed automatically alongside
  the CLI. To connect it, follow [Setting Up Telegram](/scion/getting-started/telegram/).

To upgrade later:

```bash
brew update && brew upgrade homebrew-scion/scion/scion
```

### Quick start

Start the Workstation combo server:

```bash
scion server start
```

Your browser opens to the [Onboarding Wizard](/scion/getting-started/onboarding/) at
`http://127.0.0.1:8080/onboarding`, which handles the rest of setup for you — runtime detection
(Docker, Podman, or Apple Container), identity configuration, container image setup (pulling from
`ghcr.io/homebrew-scion`), and creating your first workspace.

After onboarding, start your first agent from the web UI, or from the CLI in a repository with
`scion init` and `scion start` (see the [Tutorial](/scion/getting-started/tutorial/)).

:::note[Using Homebrew?]
The onboarding wizard performs machine initialization and image setup for you, so you can skip many of
the [Prerequisites](#prerequisites) and [Configuration](#configuration) sections below — they
apply to a from-source install or a manual setup without the wizard. You will still need a container runtime - podman is recommended.
:::

---

## Prerequisites

These apply when you build [from source](#install-from-source) or configure Scion manually. The
onboarding wizard checks the runtime and Git requirements for you.

### 1. Go
Go is required only to build Scion **from source**. You need Go 1.26 or later installed (see the
`go` directive in the repository's `go.mod`). Homebrew installs a prebuilt binary, so you can
skip this if you installed with `brew`.
- [Download and install Go](https://golang.org/doc/install)

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
- Agents reach the hub from inside their containers. With Docker and Podman this works
  automatically. With Apple container it needs a one-time container DNS entry, created with
  `sudo`; the onboarding wizard offers to do it. See [Apple Container DNS](/scion/local/apple-container/).
  Auto-detection picks Apple container first on macOS; if you also have Podman or Docker, you can
  select one of them in the wizard (or in settings, see [Select Runtime](#5-select-runtime)) to skip
  the DNS step.

### 3. Git
Scion uses `git worktree` to manage agent workspaces.
- Ensure `git` is installed and available in your PATH.
- Because Scion uses a new feature for relative path worktrees, ensure that `git --version` >= 2.48.0.

For Ubuntu you can install the latest version with

```bash
add-apt-repository ppa:git-core/ppa

apt update; apt install git
```

On Debian, note that Debian 13 (trixie)'s own `apt` git is **2.47.3** — below the 2.48 floor. Check `git --version`; if it's too old, build from source (see the [git site](https://git-scm.com/install/source)) or install a newer package from a source like [git-scm.com's Linux downloads](https://git-scm.com/download/linux).

---

## Install from Source

Building from source is the alternative to [Homebrew](#install-with-homebrew-recommended) — use
it if you are contributing to Scion or prefer to build the binary yourself.

### From Source (`go install`)
You can install Scion directly using `go install`:

```bash
go install github.com/GoogleCloudPlatform/scion/cmd/scion@latest
```

:::caution[Web UI assets are not included]
`go install` builds only the Go binary. It does not build or embed the web frontend, so `scion server start` will serve a blank web UI with missing frontend assets. Use Homebrew for a ready-to-run install, or build from a clone with `make all`.
:::

Ensure your `$GOPATH/bin` (typically `~/go/bin`) is in your system `$PATH`.

### From Clone
If you have the repository cloned, you can use the provided `Makefile`:

```bash
make all
# This creates a 'scion' binary in the build directory.
# You can move it to a directory in your PATH:
sudo mv ./build/scion /usr/local/bin/
```

The `all` target builds the web frontend before compiling the Go binary, so the embedded web UI assets are present. If you prefer separate steps, run `make web` before `make build`.

To verify your installation, run:

```bash
scion version
```

---

## Build container images

No publicly hosted images are currently available for Scion, but quick and easy build scripts are included.

The easiest way to get these images is to fork this repo, and then go to the "Actions" tab and select the "Build Scion Images" workflow.

You will then use your `ghcr.io/myorg` registry for the `image_registry` setting. These images must be available in the registry before running the initialization command.

See [Building Containers](/scion/local/custom-images/) for more details

## Configuration

### 1. Initialize your machine
You must first establish global settings, templates and configs for your machine

```bash
scion init --machine
```

This creates a directory at `~/.scion`

You will be prompted for the image registry where you have built and deployed the images in the previous step.

### 2. Initialize a Project
Navigate to the root of a project where you want to use Scion and run:

```bash
scion init
```

This creates a `.scion` marker file in the directory, linking it to structures inside the global folder created on the machine initialization.
When a hub is running (next step), `init` also links the project to it; otherwise it prints a note
and the project links on its first `scion` command.

### 3. Start the local hub
Agents run through a hub. Start the workstation server once:

```bash
scion server start
```

It runs in the background, saves its endpoint (`http://127.0.0.1:8080`) in your global settings,
and on the first run opens the [Onboarding Wizard](/scion/getting-started/onboarding/). Without a
running hub, agent commands fail with:

```
No hub configured. Start the local hub with 'scion server start' (first run opens setup), or set a remote one with 'scion config set hub.endpoint <url>'.
```

To use a shared hub instead, see [Connecting to a Hub](/scion/hosted/user/hosted-user/).


### 4. Agent Authentication (LLM Access)

Before starting an agent, you must provide credentials so the underlying LLM harness (Claude, Gemini, etc.) can authenticate with its model provider.

Scion uses a **unified authentication pipeline** that automatically discovers credentials from your environment. For a quick start, export your provider's API key:

```bash
# For Claude
export ANTHROPIC_API_KEY="your-api-key"

# For Gemini
export GEMINI_API_KEY="your-api-key"
```

On the workstation server, harness login files in your home directory (for example after you sign in to a harness on this machine) are given to agents automatically. Scion also supports Vertex AI (via Application Default Credentials). For advanced credential configurations, including Hub-based secret injection, see [Agent Credentials](/scion/local/agent-credentials/).

### 5. Select Runtime
Scion automatically selects the first runtime it finds working, in this order:
- **macOS**: `container` (Apple Virtualization Framework), then `podman`, then `docker`.
- **Linux/Windows**: `podman`, then `docker`.

If you wish to change this (e.g., to use Podman on macOS), you can manually edit `.scion/settings.yaml`:

```yaml
profiles:
  local:
    runtime: podman
```

Scion accepts settings in either YAML or JSON. `scion init` writes `settings.yaml`, and YAML is preferred when multiple files are present (the loader looks for `settings.yaml`, then `settings.yml`, then `settings.json`). If you prefer JSON, name the file `.scion/settings.json` and use valid JSON syntax:

```json
{
  "profiles": {
    "local": {
      "runtime": "podman"
    }
  }
}
```

Both files validate against the [settings JSON schema](https://github.com/GoogleCloudPlatform/scion/blob/main/pkg/config/schemas/settings-v1.schema.json). See the [Configuration Overview](/scion/reference/scion-config-reference/) for the full settings ecosystem.

---

## Next steps

- **Run your first agent** — follow the [Tutorial](/scion/getting-started/tutorial/).
- **Set up Workstation mode** — use the [Onboarding Wizard](/scion/getting-started/onboarding/)
  (`scion server start`).
- **Upgrading from a release that ran agents without a hub?** See the
  [migration page](/scion/reference/local-mode-removal/).
- **Understand the pieces** — read [Core Concepts](/scion/concepts/).

---

## Shell Completions

Scion provides shell completions. These are highly recommended as they are very useful when providing proper descriptive agent names.

For setup instructions, see [Shell Completions](/scion/local/completions/).

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
