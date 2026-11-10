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

:::note[On Windows?]
The Windows build (`scion-windows-amd64.zip` and `scion-windows-arm64.zip` on GitHub Releases, verified against `SHA256SUMS`) is a Hub client only: it supports connecting to a remote Hub (`hub`, `project`, `config`, `message`, `secret`, `skills`) and reaching agents over Conduit (`ssh`, `ssh-config`, `tunnel`, `vscode`), while server, Runtime Broker, and local container lifecycle commands exit with a `"not supported on Windows"` error. See [Windows](/scion/hosted/user/windows/) for how to install it and which commands it supports.
:::

## Install with Homebrew (recommended)

The easiest way to get a ready-to-run Scion — CLI plus embedded web UI — is the community
[homebrew-scion](https://github.com/homebrew-scion/homebrew-scion) tap:

```bash
brew tap homebrew-scion/scion
brew install homebrew-scion/scion/scion
```

This installs **`scion`**, the CLI with the embedded web UI, from the prebuilt binaries of
the latest preview release. The [Onboarding Wizard](/scion/getting-started/onboarding/)
suggests `ghcr.io/homebrew-scion` as the image registry. Images are published there for each
preview release, tagged with that release's tag, and a Homebrew `scion` pulls the images tagged
with its own version, so agents run images that match your binary. Chat plugins such as Telegram
and Discord are not included; their setup guides explain how to install them.

:::note[Which image tag agents use]
A release build of `scion` (`vX.Y.Z`, `vX.Y.Z-preview.N` or `vX.Y.Z-rc.N`, as Homebrew installs)
uses its own version as the tag of the bundled `scion-*` images whose tag is `latest` or absent;
development and nightly builds keep `:latest`.

- An image you name explicitly with `--image scion-claude:latest`, or a bare name such as
  `scion-claude`, is retagged the same way, to `scion-claude:<version>`.
- To keep `:latest`, set `image_tag` to `latest`: `scion config set --global image_tag latest`,
  then `scion server restart`. Any set `image_tag` takes precedence over the version default.
  While the `hub.image_tag_setting` experiment is off, a set value is not applied (image
  references keep their tags) but still suppresses the version default; with it on, a value
  other than `latest` is applied as the tag.
- To turn the version default off for the whole Hub, disable the
  `hub.image_tag_version_default` [experiment](/scion/reference/experiments/):
  `scion hub experiments disable hub.image_tag_version_default`, or switch it off under
  **Admin → Server Config → Experiments**.

See [Image tag](/scion/reference/settings-precedence/#image-tag-experimental) for the details.
:::

:::note[Older Linux distributions]
On Linux hosts whose glibc is older than 2.39 or libstdc++ older than 6.0.33 (for example Debian
12, Ubuntu 22.04 or RHEL 9), Homebrew installs its own `gcc` (and its own `glibc`, if glibc is
older than 2.39), with their dependencies, the first time you install any formula. Scion is a
static binary and does not use them. To avoid them, download `scion-linux-amd64.tar.gz` or
`scion-linux-arm64.tar.gz` from the [GitHub
releases](https://github.com/GoogleCloudPlatform/scion/releases), verify it against the
release's `SHA256SUMS`, and put `scion` on your `$PATH`.
:::

To upgrade later:

```bash
brew update && brew upgrade homebrew-scion/scion/scion
```

### Quick start

Start the Workstation combo server:

```bash
scion server start
```

Your browser opens, signed in through a single-use link, to the
[Onboarding Wizard](/scion/getting-started/onboarding/) at
`http://127.0.0.1:8080/onboarding` (run `scion server open` later for a new sign-in link), which handles the rest of setup for you — runtime detection
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

When no runtime is configured, Scion picks one automatically. On macOS it prefers Podman, then Docker, then Apple `container`. On macOS, when Podman is selected or auto-detected but not on your `PATH`, Scion also looks for it in `/opt/podman/bin`, `/usr/local/bin` and `/usr/bin` when it uses the runtime. Detection during setup, including the onboarding wizard's, looks only on your `PATH` and checks that each runtime runs; on Linux it prefers Podman, then Docker. To use another installed runtime, select it in the onboarding wizard or in your profile's `runtime` setting.

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
  automatically. With Apple container it needs a `sudo` DNS rule that must be re-created after each reboot (or installed as a LaunchDaemon); the onboarding wizard's runtime
  step shows the command. See [Apple Container DNS](/scion/local/apple-container/).
  On macOS, auto-detection prefers Podman, then Docker, and picks Apple container only when neither
  is installed, so the DNS step applies when Apple container is the only runtime or you select it
  (see [Select Runtime](#5-select-runtime)).

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

Agents run in container images. The Homebrew onboarding suggests the `ghcr.io/homebrew-scion`
registry. Images there are tagged with the release tag, and a release build of `scion` pulls the
images tagged with its own version (see
[Which image tag agents use](#install-with-homebrew-recommended)). You can also build the images
locally with the scripts in the repository.

To build the images that match your `scion` binary, run the release image driver. It clones the
release tag and builds the full image set from that release's own image specs, tagged
`:<version>`. It needs only bash, git, and Docker or Podman, and runs from a checkout or as a single
downloaded file:

```bash
curl -fsSLO https://raw.githubusercontent.com/GoogleCloudPlatform/scion/main/image-build/scripts/build-release-images.sh

# With no version, it builds the release `scion version` reports. These images stay in the
# local image store, named localhost/scion/<image>; nothing is pushed. A release build of scion
# uses the :<version> images. --also-tag latest also tags them :latest, the tag development
# builds (or image_tag latest) use; naming a registry prefix makes the driver ask for
# --force-latest as well.
bash build-release-images.sh --also-tag latest --force-latest -- --registry localhost/scion
scion config set --global image_registry localhost/scion

# Or build a given release and push it to your own registry:
bash build-release-images.sh v0.3.0-preview.5 --also-tag latest --force-latest -- \
  --registry ghcr.io/myorg --platform all --push
scion config set --global image_registry ghcr.io/myorg
```

The images must be available before you run the initialization command below. See
[Images that match a release](/scion/local/custom-images/#images-that-match-a-release) for how
the image tag is chosen.

To build from the tip of `main` instead, fork the repository and run the "Build Scion Images"
workflow from its "Actions" tab, then use your `ghcr.io/myorg` registry for `image_registry`.
With a release build of `scion`, also set `image_tag: latest` (or disable
`hub.image_tag_version_default`), or run the workflow with its image tag set to the version
`scion version` reports, because a release build pulls `:<version>` by default (see
[Image tag](/scion/reference/settings-precedence/#image-tag-experimental)).

See [Building Custom Images](/scion/local/custom-images/) for more details.

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

It runs in the background, adds itself to your list of hubs as `local` (`http://127.0.0.1:8080`),
and on the first run opens the [Onboarding Wizard](/scion/getting-started/onboarding/). Before any
hub is set up, agent commands fail with:

```
No hub selected. Add one with 'scion hub add <name> <url>', start a local hub with 'scion server start', or pick one with 'scion use --hub <name>'.
```

Once the local hub is set up, commands run while it is stopped (for example after
`scion server stop`) fail with:

```
Local scion server is not running. Start it with 'scion server start'.
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
- **macOS**: `podman`, then `docker`, then `container` (Apple Virtualization Framework).
- **Linux/Windows**: `podman`, then `docker`.

If you wish to change this (e.g., to use Apple `container` on macOS when Podman or Docker is also installed), you can manually edit `.scion/settings.yaml`:

```yaml
profiles:
  local:
    runtime: container
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
