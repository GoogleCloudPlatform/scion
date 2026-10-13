---
title: "Tutorial: Git-Based Projects"
description: Set up a GitHub repository as a Hub-managed project and dispatch agents to work on it remotely.
---

**What you will learn**: How to create a project from a GitHub repository, configure authentication with a GitHub App or Personal Access Token, and start agents that clone and work on your code — all without a local checkout.

## Per-agent Clones vs. Worktree-per-agent

Every agent runs through a Hub, so every git project is a Hub project. The project's workspace sharing mode decides how agents get the code:

| | Per-agent clone (default for Hub-managed projects) | Worktree of your checkout (repository linked to the local workstation Hub) | Worktree of the broker's clone (Hub-managed, chosen at creation) |
| :--- | :--- | :--- | :--- |
| **Where code lives** | Inside the agent container, cloned at startup | `.scion/agents/<name>/workspace` in your repository | On the Runtime Broker's host, a worktree per agent over one shared base clone |
| **Git credentials** | Requires a GitHub App or `GITHUB_TOKEN` (private repositories) | None; nothing is fetched | The broker fetches the base clone from the project's clone URL; see [Workspaces & Sharing Modes](/scion/local/workspaces-and-sharing/) |
| **Isolation** | Full clone per agent, completely independent | Worktree per agent, shared `.git` in your repository | Worktree per agent, shared `.git` in the base clone |
| **Merging work** | Agent pushes to remote; merge via Pull Request | Merge the agent's branch locally | Agent pushes to remote; merge via Pull Request |
| **Applies to** | Projects created from a URL, repositories linked to a remote Hub, and repositories linked to a workstation Hub before local worktrees existed | New links to the local workstation Hub (git 2.48+) | Projects created on the Hub with `--workspace-mode worktree-per-agent` (git 2.48+ on the broker) |

See [Linked git projects](/scion/local/workspace/#linked-git-projects) for the checks, cautions and how to re-link an older project.

**Key takeaway**: A project created on the Hub from a URL, or a repository linked to a remote Hub, uses **HTTPS clone-based provisioning** by default; the Hub enforces one workspace strategy across all brokers and users. A project can instead select worktree-per-agent mode at creation, which gives agents a git worktree each over a host-side shared clone, gated on the broker's git version being 2.48 or later. A repository linked to the local workstation Hub gets worktrees of your own checkout; see [Linked git projects](/scion/local/workspace/#linked-git-projects).

For the full technical details on workspace strategies, see [About Workspaces](/scion/local/workspace/).

---

## Prerequisites

- A running Scion Hub that you are connected to (see [Connecting to Hub](/scion/hosted/user/hosted-user/))
- A GitHub repository (public or private) you want agents to work on
- A GitHub account with permission to create fine-grained Personal Access Tokens

---

## Step 1: Set Up Authentication

Scion provides two ways to authenticate with GitHub for your projects:

1. **GitHub App Integration (Recommended)**: An automated, secure way to provide agents with short-lived installation tokens. If your Hub Administrator has configured this, authentication is automatic.
2. **Personal Access Tokens (PATs)**: Manual token management using a `GITHUB_TOKEN` secret. Use this if the GitHub App is not configured or for repositories outside its scope.

:::caution
Do not put a token in the repository URL. The Hub stores a project's git remote without credentials, and refuses a `scion.dev/clone-url` label that contains a username, password, token, query string, or fragment (`400`). If an existing project relied on a token embedded in its clone URL, move that token to a `GITHUB_TOKEN` project secret or use the GitHub App.
:::

### Option A: GitHub App Integration

When creating a project, Scion automatically associates it with the corresponding GitHub App installation. The system maintains a background refresh loop for installation tokens, and the `sciontool` credential helper provides fresh tokens to `git` on-demand inside the agent container. No manual setup is required.

### Option B: Personal Access Tokens

Scion uses a GitHub **fine-grained Personal Access Token (PAT)** to clone repositories over HTTPS inside agent containers. Fine-grained PATs are preferred over classic tokens because they can be scoped to specific repositories.

### Generate the Token

1. Go to **GitHub → Settings → Developer settings → Personal access tokens → Fine-grained tokens**.
2. Click **Generate new token**.
3. Configure:
   - **Token name**: Something descriptive, e.g. `scion-agents-acme-backend`
   - **Expiration**: Choose an appropriate lifetime
   - **Repository access**: Select **Only select repositories** and pick the repo(s) your agents will work on
   - **Permissions**:
     - **Contents**: **Read and write** (required for clone and push)
     - **Pull requests**: **Read and write** (if you want agents to create PRs)
     - **Metadata**: **Read-only** (automatically selected)
4. Click **Generate token** and copy the value (it starts with `github_pat_`).

:::caution[Copy it now]
GitHub only shows the token value once. If you lose it, you'll need to regenerate it.
:::

---

## Step 2: Create a Git Project

Create a project on the Hub from your repository URL:

```bash
scion hub project create https://github.com/acme/backend.git
```

Scion will auto-detect the default branch and derive a slug from the repository name:

```
Project created:
  ID:     a1b2c3d4-e5f6-5789-abcd-ef0123456789
  Slug:   acme-backend
  Remote: github.com/acme/backend
  Branch: main
```

:::note[Project ID Format]
Project IDs are always randomly generated UUIDs (v4). A git remote is associated metadata, not identity, so the same repository can back multiple projects, and re-creating a project from the same URL produces a new ID.
:::

### Optional Flags

```bash
# Specify a branch (useful for long-lived feature branches)
scion hub project create https://github.com/acme/backend.git --branch develop

# Override the auto-generated slug
scion hub project create https://github.com/acme/backend.git --slug my-backend
```

:::note[Multiple projects per remote]
Project IDs are always randomly generated UUIDs, so a git remote is associated metadata rather than identity: creating a project from the same git URL twice yields two distinct projects, each with its own ID. Stale project links are still automatically detected and synchronized during hub-link sync operations.
:::

:::tip[Web Dashboard alternative]
In the dashboard's New Project dialog, pick **Git Repository** as the workspace type, or set **Start from** to a project template and enter your repository as the **Git Remote URL** override. The new project keeps the template's settings and uses your repository. See [Web Dashboard: Projects](/scion/workstation/dashboard/#projects).
:::

---

## Step 3: Configure Project Settings, Templates & Limits

Once your project is created, you can configure its settings via the Web Dashboard. This allows you to manage templates, set resource limits, and configure runtime brokers.

1. Navigate to the Web Dashboard and select your newly created project.
2. Configure templates by clicking the **Load Templates** button on the project detail page. This initiates a direct, server-side import of templates from a specified Git repository URL, deep path, or archive. Once imported, you can use the web interface to browse and edit template files.
3. Click the **Settings** gear icon to configure:
   - **General**: Update the project description or default branch.
   - **Limits**: Set maximum concurrency, maximum execution duration, and workspace storage limits. These limits automatically prepopulate the agent creation form.
   - **Resources**: Manage runtime brokers and message broker plugins available to agents in this project.
   - **Secrets**: Manage project-scoped secrets, such as API keys and PATs.

---

## Step 4: Upload Your Token as a Secret (PAT only)

If you are using the Personal Access Token method, store your PAT as a project-scoped secret:

Store the GitHub PAT as a project-scoped secret so that all agents in this project can authenticate:

```bash
scion hub secret set --project=acme-backend GITHUB_TOKEN github_pat_xxxxxxxx
```

Secrets are **write-only** — the value is encrypted and can never be read back via the CLI or API. It is only decrypted at runtime and injected into the agent container as an environment variable.

### User-Scoped vs. Project-Scoped

You can also set `GITHUB_TOKEN` at the **user scope** if your token covers multiple repositories:

```bash
# User-scoped: available to all your agents across all projects
scion hub secret set GITHUB_TOKEN github_pat_xxxxxxxx
```

Project-scoped secrets take priority over user-scoped ones (see [Secret Management](/scion/hosted/user/secrets/) for the full resolution hierarchy).

### Web Dashboard Alternative

You can also upload secrets through the Web Dashboard:
1. Navigate to the project's settings page.
2. Open the **Secrets** tab.
3. Click **Add Secret**, enter `GITHUB_TOKEN` as the key, paste the token value, and save.

---

## Step 5: Start an Agent

With the project and token in place, start an agent targeting the project:

```bash
scion start my-agent --project acme-backend "add input validation to the /users endpoint"
```

You'll see the agent go through the clone-based startup:

```
Using hub, cloning repo https://github.com/acme/backend.git
  (Hub mode uses HTTPS clone with GITHUB_TOKEN; local worktrees are not used)

Agent 'my-agent' starting on broker 'us-west-01'...
  Status: CLONING (github.com/acme/backend @ main)
  Status: STARTING
  Status: RUNNING
```

### What happens inside the container

1. The workspace is provisioned using a `git init` + `git fetch` strategy, which can handle workspaces that already contain `.scion` metadata or `.scion-volumes` directories
2. A feature branch `scion/my-agent` is created and checked out
3. Git identity is configured automatically
4. The agent's harness (Claude, Gemini, etc.) starts with the task prompt

:::note[Provisioning Strategy]
Hub-linked projects use a robust `git init` + `git fetch` approach rather than a standard `git clone`. This allows provisioning into workspaces that may already contain metadata directories, and properly clears stale artifacts before initialization.
:::

### Running multiple agents

Each agent gets its own clone and its own `scion/<agent-name>` branch, so you can run several in parallel on the same project:

```bash
scion start agent-auth    --project acme-backend "add OAuth2 support"
scion start agent-tests   --project acme-backend "increase test coverage for pkg/api"
scion start agent-docs    --project acme-backend "update the API documentation"
```

---

## Step 6: Monitor and Retrieve Work

### Check agent status

```bash
scion list --project acme-backend
```

### Attach to an agent

If an agent is waiting for input or you want to observe it:

```bash
scion attach my-agent --project acme-backend
```

### Retrieve the code

Since agents push their branches to the remote, you can pull their work from any machine:

```bash
git fetch origin scion/my-agent
git log origin/scion/my-agent
```

Or create a Pull Request directly from the branch on GitHub.

---

## Linked Projects: When Local Becomes Remote

If you link a local checkout to a **remote** Hub, the workspace strategy changes: even if the broker machine has the repository on disk, agents use clone-based provisioning, unless the project was created with worktree-per-agent mode (requires git 2.48+ on the broker). This means you need a `GITHUB_TOKEN` secret set on the project for private repositories, just like a project created from a URL.

On the **local workstation** Hub, a newly linked repository keeps local worktrees of your checkout instead. A repository linked there before local worktrees existed keeps cloning until it is re-linked. See [Linked git projects](/scion/local/workspace/#linked-git-projects).

---

## Troubleshooting

### Agent fails with "authentication required" or "clone failed"

- Verify a `GITHUB_TOKEN` is set on the project or at user scope:
  ```bash
  scion hub secret get --project acme-backend
  ```
- Ensure the token has **Contents: Read** permission at minimum.
- Check that the token has not expired.
- For fine-grained PATs, confirm the target repository is included in the token's repository access list.

:::note[Unclassified Clone Errors]
When a git clone fails for reasons unrelated to `GITHUB_TOKEN` (such as network timeout or dns issues), recent versions of Scion will display a neutral "unclassified error" label instead of pointing the blame at the token. This helps avoid confusion during debugging.
:::

### Agent clones the wrong branch

Use the `--branch` flag when creating the project to set the default:

```bash
scion hub project create https://github.com/acme/backend.git --branch develop
```

### Agent needs full git history

Shallow clones (`depth=1`) are used by default for fast startup. 

- **Full Clones on Startup:** If an agent needs full history for operations like `git log` or `git blame` from the start, you can request a complete, non-shallow clone by configuring a clone depth of `0` in your project or agent configuration (`depth: 0` or environment variable `SCION_GIT_DEPTH=0`). The underlying `GitCloneConfig` API treats a depth of `0` as an explicit request for a full clone, while omission/nil continues to default to shallow depth `1`.
- **Unshallowing Later:** Alternatively, if an agent has a shallow clone, it can unshallow itself later inside its session by running:
  ```bash
  git fetch --unshallow
  ```
