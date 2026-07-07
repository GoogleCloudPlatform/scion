---
title: Onboarding Wizard
description: A guided, browser-based walkthrough that sets up your workstation the first time you run scion server start — identity, system checks, runtime, images, harnesses, and your first project.
---

**What you will learn**: How to set up Scion in [Workstation mode](/scion/choosing-a-mode/) using the browser-based onboarding wizard — from a fresh install to your first project — without touching a config file.

The onboarding wizard is the fastest way to get the hosted experience running locally. When you start the Workstation combo server for the first time, Scion opens a guided setup in your browser that walks through machine configuration, environment checks, runtime selection, harness images, and creating your first project.

:::note[Which mode is this?]
The wizard sets up **Workstation mode** — a single-tenant Scion server (Hub + Runtime Broker + Web dashboard) running on your own machine over loopback. If you only want to run agents from the CLI with no server, see [Local mode](/scion/choosing-a-mode/) and the [Installation guide](/scion/getting-started/install/) instead.
:::

## Prerequisites

Before you start, you need a working Scion install and a container runtime. See the [Installation guide](/scion/getting-started/install/) for details. In short:

- Scion installed with its web assets. Use **Homebrew** (`brew install scion`) for a ready-to-run install; a bare `go install` does **not** embed the web UI, so the wizard would load blank.
- A container runtime — Docker, Podman, or Apple Container.
- Git 2.47 or later (the wizard flags older versions).

You do **not** need to run `scion init --machine` first — the wizard handles machine initialization for you.

## Launching the wizard

Start the Workstation server:

```bash
scion server start
```

On a machine that has not been set up yet, Scion prints the web URL and **automatically opens your browser** to the wizard:

```
http://127.0.0.1:8080/onboarding
```

If the browser does not open (for example over SSH, in a headless environment, or when `SCION_NO_BROWSER` is set), open that URL manually. The port is `8080` by default.

:::tip[You can leave and come back]
The wizard saves your progress. If you close the tab or restart setup, reopening `/onboarding` resumes past the steps you have already completed.
:::

## The steps

The wizard runs through six steps and finishes with a confirmation screen. Each step can be revisited with **Back**, and several can be skipped and completed later from the dashboard.

### 1. Welcome & identity

Enter a **display name** and **email**. This identity is attached to the agents and activity you create on this workstation. Provide at least one of the two to continue.

### 2. System check

The wizard runs diagnostics against your environment and shows each result as **pass**, **warn**, or **fail**. Use **Re-check** after fixing anything. A common warning is an out-of-date Git — Scion needs **Git 2.47+** for agent worktrees; upgrade (for example `brew install git`) and re-check. You can only advance once the checks report ready.

### 3. Container runtime

Scion detects the runtimes available on your machine and preselects the best one. Pick from **Docker**, **Podman**, or **Container (Apple Virtualization)**; runtimes that were not detected are shown but disabled.

:::caution[Apple Container needs DNS setup]
If you select **Container** (Apple Virtualization), the wizard attempts to configure container DNS, which requires `sudo`. If it cannot do so automatically, it shows the exact command to run manually, for example:

```bash
sudo container system dns create <hostname> --localhost <ip>
```
:::

### 4. Image registry

Enter the container image registry that hosts the Scion harness images (for example `us-central1-docker.pkg.dev/my-project/scion`). Images such as `<registry>/scion-claude:latest` are pulled from here in the next step. If you are not ready, choose **Skip for now** and set it later.

### 5. AI harnesses

Select the [harnesses](/scion/supported-harnesses/) you want available (Claude Code, Gemini CLI, Codex, OpenCode, and others). For each selected harness the wizard checks whether its container image is present:

- **ready** — the image is available locally.
- **available** — the image exists in the registry and can be pulled.
- **not found / error** — the image could not be located.

Use **Pull selected** to fetch any images that are not local yet (progress streams live), then **Re-check** to confirm. You can add or reconfigure harnesses later from Hub settings, or **Skip for now**.

### 6. First workspace

Create your first **project**. The wizard offers three ways to start:

- **Hub-managed project** — a workspace the Hub creates and manages for you; no git repository required.
- **Link a git repo** — connect an existing git repository for source-controlled workspaces.
- **Add local directory** — link a local directory that stays where it is and is operated on in place. The wizard validates the path and warns if it is already a git repo or already linked.

This step is optional — choose **Skip for now** to create projects later from the dashboard.

### You're all set

The final screen confirms your workstation is configured. Click **Go to Dashboard** to open the [Web Dashboard](/scion/workstation/dashboard/) at `http://127.0.0.1:8080` and start running agents.

## After onboarding

- Manage the running server with `scion server status`, `scion server restart`, and `scion server stop`. See [Workstation Server Mode](/scion/workstation/workstation-server/) for the combo server, network bridges, and lifecycle details.
- Learn your way around the [Web Dashboard](/scion/workstation/dashboard/).
- Understand the pieces you just configured in [Core Concepts](/scion/concepts/).

## See also

- [Choosing a Mode](/scion/choosing-a-mode/) — where Workstation mode fits among Local and the hosted tiers.
- [Installation](/scion/getting-started/install/) — prerequisites and install methods.
- [Workstation Server Mode](/scion/workstation/workstation-server/) — the server the wizard starts.
