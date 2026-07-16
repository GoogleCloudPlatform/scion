---
title: Harness Authentication
description: Configuring LLM credentials for Scion agents to access model providers.
---

Scion automatically handles discovering and injecting LLM credentials into agent containers so that the underlying harnesses (Claude, Gemini, etc.) can authenticate with their respective model providers (Anthropic, Google, OpenAI).

> **Note**: This documentation covers how harnesses gain access to LLM models, as well as how agents authenticate to Git repositories.

## Local vs. Hub Deployment

Authentication setup depends heavily on how you are running Scion:

- **Local (Solo) Mode**: Scion running locally will automatically scan your host machine's environment variables and well-known credential file paths (like `~/.config/gcloud/application_default_credentials.json`).
- **Hub (Hosted) Mode**: For agents dispatched by a Scion Hub to remote brokers, the agent's environment is strictly isolated from the broker's host machine. You must provide credentials explicitly via Hub Secrets or profile settings, which are then securely injected into the agent container at launch.

---

## The Container-Script Provisioning Model

All harnesses are provisioned by a **container-script** (`provision.py`) that runs inside the
agent container, backed by the shared `scion_harness` Python library. Credential resolution is a
two-part collaboration:

1. **Host-side gather (Go)**: Before the container starts, Scion collects candidate credentials from environment variables and well-known file paths. In Hub mode this includes only the secrets and variables explicitly injected into the agent; direct Hub-secret access from inside the agent is blocked.
2. **In-container select (`provision.py`)**: The harness's provisioner evaluates the staged candidates against the harness's declared auth methods (the `auth:` block in its `config.yaml`), selects one, and writes the harness-native configuration (e.g. `~/.claude.json`, `~/.gemini/settings.json`, `~/.hermes/.env`).

The selection logic lives in each bundle's `provision.py` via `scion_harness.AuthSpec` /
`ctx.select_auth(...)`. The order of methods in that spec — not the `config.yaml` `autodetect`
map — determines which credential wins when several are present.

### Source precedence

For each credential key, the resolution order is:

1. **Staged candidate / secret file** — credentials the Hub or CLI explicitly staged for the agent.
2. **Environment variable** — a matching variable in the agent's process environment.
3. **Well-known file** — a native credential file at its conventional path (e.g. `~/.config/gcloud/application_default_credentials.json`).

Staged candidates are matched across *all* keys before the process-environment fallback fires, so
a user-provided secret is never shadowed by a stale container environment variable.

**Vertex AI / GCP metadata** is not a "source" gathered this way — it is an auth *type*
(`vertex-ai`) selected when a GCP service account is assigned to the agent. At runtime, tokens are
served by Scion's in-container GCP metadata server.

:::note[Harness-specific ordering]
Each harness declares its own precedence among *methods*. For example, Hermes selects the first
present of `ANTHROPIC_API_KEY` > `OPENAI_API_KEY` > `GOOGLE_API_KEY`; Copilot uses
`COPILOT_GITHUB_TOKEN` > `GH_TOKEN` > `GITHUB_TOKEN`; Claude uses `ANTHROPIC_API_KEY` >
`CLAUDE_CODE_OAUTH_TOKEN` > auth-file > `vertex-ai`. See the per-harness sections in
[Supported Agent Harnesses](/scion/supported-harnesses/).
:::

---

## The Three-Stage Auth Workflow

Beyond a plain API key, most harnesses authenticate through an interactive login. Scion supports
this with a **no-auth start → authenticate → capture** workflow that lets you bring an agent up
with no credentials, log in interactively, and then persist the resulting credentials so future
agents start pre-authenticated.

1. **No-auth start** — When no usable credentials are found, provisioning does **not** fail. The
   agent still starts and drops to an interactive shell, with a graceful warning explaining that it
   is running in no-auth mode. See [No-Auth Fallback](#no-auth-fallback).
2. **Authenticate** — Inside the agent (via `scion attach` or the terminal page), you run the
   harness's native login command (e.g. `claude setup-token`, `codex login --device-auth`,
   `copilot login`, `agy`). Each bundle's no-auth message tells you exactly which command to run.
3. **Capture** — You run the bundle's `capture_auth.py`, which locates the credential files the
   harness just wrote and stores them as project secrets. On the next start, the host stages those
   secrets back into the agent and provisioning selects them automatically. See
   [Capturing Credentials](#capturing-credentials-from-a-running-agent).

If you already have a plain API key (or a credential file) you can skip stages 1–3 entirely by
providing it up front — see [Credential Sources & Setup](#credential-sources--setup).

The per-harness login and capture details:

| Harness | No-auth login command | Captured secret(s) |
| :--- | :--- | :--- |
| **Claude** | `claude setup-token` | `CLAUDE_AUTH` (file `~/.claude/.credentials.json`); `CLAUDE_CODE_OAUTH_TOKEN` (extracted from the terminal scrollback after `setup-token`) |
| **Gemini** | native Gemini auth (e.g. OAuth login) | `GEMINI_OAUTH_CREDS` (file `~/.gemini/oauth_creds.json`) |
| **Codex** | `codex login --device-auth` | `CODEX_AUTH` (file `~/.codex/auth.json`) |
| **OpenCode** | `opencode auth login` | `OPENCODE_AUTH` (file `~/.local/share/opencode/auth.json`) |
| **Copilot** | `copilot login` | `COPILOT_CONFIG` (file `~/.copilot/config.json`) |
| **Hermes** | `hermes setup` | *(none — Hermes is API-key only; provide the key up front)* |
| **Antigravity** | `agy` | `AGY_TOKEN` (file `~/.gemini/antigravity-cli/antigravity-oauth-token`, with a gnome-keyring fallback) |

---

## Authentication Approaches

Scion supports two approaches to harness authentication: the **Automatic (Implicit) Approach** and the **Explicit Path**.

### The Automatic (Implicit) Approach

By default, when an agent starts, the provisioner discovers and applies credentials automatically:
it gathers the staged candidates and environment, selects the best method according to the
harness's declared priority order (usually preferring a direct API key over a credential file),
validates the result, and writes the harness's native settings. The decision is made right before
the agent starts (late-binding), so the final strategy reflects whatever credentials are actually
available at launch.

If no usable credentials are found, provisioning **falls back to no-auth** rather than failing
(see [No-Auth Fallback](#no-auth-fallback) below).

### The Explicit Path

You can override the automatic detection by explicitly forcing a specific authentication method in your agent's profile or template configuration (using the `auth_selectedType` field). You can also override this on the fly when starting an agent by using the `--harness-auth` flag (e.g., `scion start my-agent --harness-auth vertex-ai`).

When you configure the explicit path, the automatic fallback is disabled. The credentials required for your chosen method **must** be present (either gathered from the local environment or provided via Hub secrets), otherwise the agent will immediately fail to start.

The available explicit authentication types are:

- **Provider API Key** (`api-key`): Direct API key authentication.
- **OAuth Token** (`oauth-token`): A raw OAuth token injected via an environment variable (e.g. Claude's `CLAUDE_CODE_OAUTH_TOKEN`).
- **Vertex Model Garden** (`vertex-ai`): Google Cloud Vertex AI using Application Default Credentials (ADC).
- **Harness specific credential file** (`auth-file`): A credential file native to the harness, such as an OAuth credentials file.

Not every harness supports every type — see the
[auth capability matrix](/scion/supported-harnesses/#feature-capability-matrix).

:::note
Scion translates these universal explicit auth types to harness-native values internally. You should always use the universal values (`api-key`, `oauth-token`, `vertex-ai`, `auth-file`) in your Scion configuration.
:::

---

## Credential Sources & Setup

The following sections detail the environment variables and files that Scion consults for each authentication method, and how to configure them locally or via the Scion Hub.

### Provider API Key (`api-key`)

This is the simplest method, relying on standard environment variables to provide a direct API key.

**Required Sources:**
- **Claude**: `ANTHROPIC_API_KEY`
- **Gemini**: `GEMINI_API_KEY` or `GOOGLE_API_KEY`
- **OpenCode**: `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` (Anthropic preferred)
- **Codex**: `CODEX_API_KEY` or `OPENAI_API_KEY` (Codex-specific key preferred)
- **Hermes**: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, or `GOOGLE_API_KEY` (in that order)
- **Copilot**: a GitHub token via `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, or `GITHUB_TOKEN`

**Local Setup:**
```bash
export ANTHROPIC_API_KEY="sk-ant-api01-..."
scion start --harness claude my-agent
```

**Hub Setup:**
You can establish these secrets via the Scion Hub Web Interface by navigating to the **Secrets** section, or you can use the CLI:
```bash
scion hub secret set ANTHROPIC_API_KEY "sk-ant-api01-..."
scion hub secret set GEMINI_API_KEY "AIza..."
```

### OAuth Token (`oauth-token`)

Some harnesses accept a raw OAuth token through an environment variable instead of a full
credential file. This is distinct from the `auth-file` method (a file on disk) and from
Antigravity's file-based OAuth token.

**Required Sources:**
- **Claude**: `CLAUDE_CODE_OAUTH_TOKEN` (generate on your host with `claude setup-token`).

**Hub Setup:**
```bash
scion hub secret set CLAUDE_CODE_OAUTH_TOKEN "sk-ant-oat01-..."
```

This is also the token that Claude's `capture_auth.py` stores automatically after an in-agent
`claude setup-token` login (see [Capturing Credentials](#capturing-credentials-from-a-running-agent)).

### Vertex Model Garden (`vertex-ai`)

Uses Google Cloud's Vertex AI endpoints with Application Default Credentials (ADC). Scion supports two primary ways to authenticate in Hub mode: via an assigned GCP Identity (Service Account) or an injected ADC file secret. Supported by Claude, Gemini, and OpenCode (Codex, Copilot, and Hermes do not support Vertex AI).

**Required Sources:**
- **Assigned GCP Identity** (Hub Mode): If the agent is assigned a Hub-managed GCP Service Account via metadata emulation, Vertex AI will automatically use it. This is the recommended and most secure approach.
- **ADC JSON file** (Fallback/Local): Automatically discovered at `~/.config/gcloud/application_default_credentials.json` if present locally. In Hub mode, you can upload an ADC file via the `gcloud-adc` file secret or specify the `GOOGLE_APPLICATION_CREDENTIALS` environment variable pointing to a custom credential file.
- `GOOGLE_CLOUD_PROJECT`: Your Google Cloud project ID.
- `GOOGLE_CLOUD_REGION`: The region (e.g., `us-east5`). Required for Claude, optional but recommended for Gemini. `CLOUD_ML_REGION` and `GOOGLE_CLOUD_LOCATION` are accepted as alternatives.

**Local Setup:**
```bash
# Assuming ADC is already generated via `gcloud auth application-default login`
export GOOGLE_CLOUD_PROJECT="my-project"
export GOOGLE_CLOUD_REGION="us-east5"
scion start --harness claude my-agent
```

**Hub Setup:**
For Hub mode, the recommended approach is to assign a GCP Service Account to the agent at creation time.

Alternatively, to use an ADC file secret:
```bash
# 1. Upload the ADC credential file (written to ~/.config/gcloud/application_default_credentials.json in container)
scion hub secret set --type file \
  --target ~/.config/gcloud/application_default_credentials.json \
  gcloud-adc @~/.config/gcloud/application_default_credentials.json

# 2. Set the environment variables
scion hub secret set GOOGLE_CLOUD_PROJECT "my-project"
scion hub secret set GOOGLE_CLOUD_REGION "us-east5"
```

:::note
**Direct Hub secret access from agents is explicitly blocked for security.** The Hub injects secrets into the agent at startup.
The `gcloud-adc` secret automatically writes the ADC file to the well-known GCP path inside the container. Scion does **not** set the `GOOGLE_APPLICATION_CREDENTIALS` environment variable by default when using `gcloud-adc`. If you need to use `GOOGLE_APPLICATION_CREDENTIALS` as an alternative for Vertex AI or to point to a non-standard path, set it up as a standard environment variable secret alongside your file secret.
:::

### Harness specific credential file (`auth-file`)

Some harnesses authenticate with their own credential files, such as OAuth credential files. Scion
stages the file at the harness's expected path inside the container. The file is provided either by
uploading it as a **file secret**, or (locally) by having it present at its conventional path on
your host, where Scion discovers it and mounts it into the agent.

**Well-known paths & file-secret keys:**

| Harness | Container path | File-secret key |
| :--- | :--- | :--- |
| **Gemini** | `~/.gemini/oauth_creds.json` | `GEMINI_OAUTH_CREDS` |
| **Claude** | `~/.claude/.credentials.json` | `CLAUDE_AUTH` |
| **Codex** | `~/.codex/auth.json` | `CODEX_AUTH` |
| **OpenCode** | `~/.local/share/opencode/auth.json` | `OPENCODE_AUTH` |
| **Copilot** | `~/.copilot/config.json` | `COPILOT_CONFIG` |
| **Antigravity** | `~/.gemini/antigravity-cli/antigravity-oauth-token` | `AGY_TOKEN` |

**Local Setup:**
If you have run the harness's native authentication command (e.g. an OAuth login on your host), Scion will automatically detect the resulting credential file and mount it into the agent.

**Hub Setup:**
Upload the credential file as a file secret via the Web Interface or CLI, targeting the harness's expected path:
```bash
scion hub secret set --type file \
  --target ~/.gemini/oauth_creds.json \
  GEMINI_OAUTH_CREDS @~/.gemini/oauth_creds.json
```

---

## No-Auth Fallback

When automatic detection finds no usable credentials, and the harness permits it, provisioning
does **not** abort — it falls back to a **no-auth** mode so the agent still starts (typically
dropping to an interactive shell). A graceful warning is written to the agent's logs explaining
that no auth candidates were found, that it is running in no-auth mode, and which login command to
run next.

This lets you launch an agent, log in to the harness interactively (e.g. `claude setup-token`,
`copilot login`, `hermes setup`, `agy`), and then capture the resulting credentials for reuse
(see below). Every bundle declares `no_auth.behavior: drop-to-shell` along with the harness's
login command and message.

The fallback applies only to **automatic** resolution. If you selected an auth type via the
[Explicit Path](#the-explicit-path), the fallback is disabled — the required credentials must be
present or the agent fails to start with an actionable error.

## Capturing Credentials from a Running Agent

For harnesses that authenticate through an interactive login (rather than a plain API key), you
can capture the credentials an agent produced and store them as a project secret, so future
agents start pre-authenticated instead of dropping to no-auth.

After logging in interactively inside the agent (via `scion attach` or the terminal page), run the
harness bundle's capture script from inside the container:

```bash
python3 ~/.scion/harness/capture_auth.py
```

### How capture works

The host generates a capture manifest (`inputs/capture-auth-config.json`) from the harness's
`auth.types.*.required_files` declarations — every credential file the harness can authenticate
with that has a well-known path. `capture_auth.py` reads that manifest, locates each file the
harness just wrote, and stores it as a project secret by shelling out to
`sciontool secret set <key> @<file> --type <type> --target <path>`.

- **What is captured** — the harness's credential file(s). For example, Codex captures
  `~/.codex/auth.json` as the `CODEX_AUTH` file secret; Gemini captures
  `~/.gemini/oauth_creds.json` as `GEMINI_OAUTH_CREDS`. See the table in
  [The Three-Stage Auth Workflow](#the-three-stage-auth-workflow).
- **Where it goes** — into the project **secret store** (the Hub's secret store in Hub mode). This
  is why captured credentials survive container restarts: the file lives in the store, not in the
  ephemeral container, and the host re-stages it on every subsequent start.
- **Harness-specific extras** — some bundles override the generic flow:
  - **Claude** additionally scans the terminal scrollback for the `sk-ant-oat…` token printed by
    `claude setup-token` and stores it as the `CLAUDE_CODE_OAUTH_TOKEN` environment secret.
  - **Antigravity** falls back to extracting `AGY_TOKEN` from the container's gnome-keyring (via
    `secret-tool`) when no token file is found on disk.
- **API-key-only harnesses** — Hermes declares no credential files, so there is nothing for
  `capture_auth.py` to capture; provide its API key up front instead.

### Exit codes

The script's exit code distinguishes outcomes:

| Code | Meaning |
| :--- | :--- |
| `0` | One or more credentials captured successfully. |
| `2` | No credentials found to capture (nothing logged in yet, or the harness has no capturable files). |
| `3` | A conflict — a secret with that key already exists. Re-run with `--force` to overwrite it. |
| `1` | An error occurred while capturing. |

```bash
# Overwrite an existing captured secret
python3 ~/.scion/harness/capture_auth.py --force
```

:::note
There is currently no `scion auth capture` CLI wrapper; capture is performed by running
`capture_auth.py` inside the agent, which delegates to `sciontool secret set`.
:::

## Persistence Across Restarts

Once captured, credentials live in the project secret store rather than in the ephemeral agent
container. On each subsequent start:

1. The host-side gather stages the stored secret back into the agent (as a candidate or file
   secret).
2. `provision.py` finds it and selects the corresponding auth method automatically.
3. The agent starts pre-authenticated — no interactive login required.

This means the no-auth → authenticate → capture cycle is a **one-time** cost per credential.
Restarting, stopping, or resuming the agent reuses the captured secret.

### Clearing or replacing captured credentials

Captured credentials are ordinary project secrets, so you manage them with the secret commands:

```bash
# Replace a captured credential: re-run capture with --force, or overwrite the secret directly
scion hub secret set CODEX_AUTH @~/.codex/auth.json --type file --target ~/.codex/auth.json

# Remove a captured credential entirely
scion hub secret clear CODEX_AUTH
```

After clearing a captured secret, the next agent start reverts to no-auth (or whatever other
credential is available), letting you re-run the login → capture cycle.

:::caution[`scion reset-auth` is not for clearing captured credentials]
`scion reset-auth` refreshes an agent's **Hub token**, not its captured harness credentials — see
[Repairing Auth on a Running Agent](#repairing-auth-on-a-running-agent). To clear or replace a
captured harness credential, use the secret commands above.
:::

## Repairing Auth on a Running Agent

If a long-running agent's **Hub token** expires and it cannot self-refresh (for example after a Hub
signing-key rotation), you can inject a fresh token **without restarting** the agent:

```bash
scion reset-auth <agent-name>
```

This generates a new token on the Hub, pushes it into the running container, and signals the agent
to restart its token-refresh loop. It requires Hub connectivity and only works on a **running**
agent (a stopped agent gets a fresh token on its next start). The same action is available as a
**Reset Auth** button in the web UI. See [Diagnostics](#diagnostics) to identify when this is
needed.

## Diagnostics

Two diagnostic commands help troubleshoot auth and connectivity:

- **`scion doctor`** (host-side): checks host prerequisites — Git, tmux, the active container runtime (Docker/Podman daemon or Kubernetes cluster access), and related diagnostics. Supports `--format json`.
- **`sciontool doctor`** (in-container): checks the *agent's* health from inside the container — required environment variables, the Hub token (presence, format, expiry), Hub reachability, token refresh, the GCP metadata server and token acquisition, and the GitHub App token. When the token check fails it prints a remediation hint pointing you at `scion reset-auth`.

## Agent Progeny & Secret Access

When an agent creates sub-agents (progeny), Scion enforces strict controls over which secrets those child agents can access.

By default, child agents operate under a **granular secret access** model. They do not automatically inherit all secrets from the project or their parent. Instead, they only have access to the credentials necessary to perform their specific tasks, maintaining a least-privilege security boundary across the agent ancestry chain.

---

## Troubleshooting

### "no valid auth method found"
The harness couldn't find any usable credentials through the automatic implicit approach. Check that you have exported the correct environment variables locally, or that your Hub secrets are properly assigned and available to the agent's workspace. If you intend to log in interactively, the agent will have dropped to a shell in no-auth mode instead — follow the printed login command, then capture.

### "auth type selected but..."
You have configured the **Explicit Path** (e.g., selecting `vertex-ai`) but the specific credentials required for that path (like `GOOGLE_CLOUD_PROJECT`) are missing. The explicit path disables fallback, so ensure all required sources for the chosen explicit type are provided.

### "unknown auth type"
The value you passed to `--harness-auth` (or `auth_selectedType`) is not one this harness supports. Valid universal types are `api-key`, `oauth-token`, `auth-file`, and `vertex-ai`, but each harness only accepts a subset — see the [auth capability matrix](/scion/supported-harnesses/#feature-capability-matrix).

### `capture_auth.py` reports "no credentials found to capture" (exit 2)
Either you have not completed the interactive login yet, or the harness has no capturable credential file (e.g. Hermes is API-key only). Complete the login command printed in the no-auth message first, then re-run capture.

### Vertex AI not activating
For Claude, Vertex Model Garden requires **all three** variables: credentials, project, and region. If any are missing, it will not authenticate. For Gemini, both credentials and a project are required. Ensure these are set either in your local environment or as Hub secrets.

## Git Authentication

Scion agents frequently need to interact with remote Git repositories to push changes, open PRs, or sync states. Authentication with GitHub is handled securely using native GitHub App integration or Personal Access Tokens (PATs).

### GitHub App Integration (Recommended)

Scion provides deep integration with GitHub Apps to manage Git credentials automatically, eliminating the need to manage static PATs.

1. **Automated Token Refresh**: The Scion Hub maintains a background refresh loop that constantly mints valid installation tokens for your GitHub App.
2. **Credential Helper**: Scion injects `sciontool` as a Git credential helper into the agent container (`git config --global credential.helper`).
3. **On-Demand Tokens**: When the agent executes a `git clone`, `push`, or `pull`, Git asks the credential helper for a password. `sciontool` retrieves the fresh, short-lived token provided by the Hub, ensuring operations never fail due to token expiration—even for long-running agents.

This flow is automatically enabled for any project linked to a GitHub App installation.

### Personal Access Tokens (PATs)

If GitHub App integration is not available, you can use a Personal Access Token. When using a PAT:

1. You create a fine-grained PAT on GitHub.
2. You provide the PAT to the Hub as a secret named `GITHUB_TOKEN`.
3. Scion injects this token into the agent container as an environment variable (`GITHUB_TOKEN`), which Git uses for HTTPS authentication.

For detailed instructions on setting this up, see [Git-Based Projects](/scion/workstation/git-projects/).
