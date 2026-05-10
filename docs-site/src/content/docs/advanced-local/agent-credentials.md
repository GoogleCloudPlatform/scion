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

## Authentication Approaches

Scion supports two approaches to harness authentication: the **Automatic (Implicit) Approach** and the **Explicit Path**. Both utilize Scion's unified `ResolvedAuth` pipeline, which relies on a centralized `AuthConfig` gathering and late-binding logic to ensure the correct credentials are used.

### The Automatic (Implicit) Approach

By default, when an agent starts, Scion runs a unified authentication pipeline to discover and apply credentials:

1. **Gather (`AuthConfig`)**: Scans environment variables and well-known file paths. In Hub mode, this only includes secrets and variables specifically injected into the agent.
2. **Resolve (`ResolveAuth`)**: The harness evaluates the gathered configuration and selects the best authentication method based on its internal priority order (e.g., usually preferring a direct API key over a credential file). This uses late-binding logic, so the final authentication strategy is decided right before the agent starts.
3. **Validate & Apply (`ValidateAuth`)**: Scion validates that the selected credentials are correct and configures the harness's native settings (e.g., writing to `.claude.json` or `settings.json`) to use them.

### The Explicit Path

You can override the automatic detection by explicitly forcing a specific authentication method in your agent's profile or template configuration (using the `auth_selectedType` field). You can also override this on the fly when starting an agent by using the `--harness-auth` flag (e.g., `scion start my-agent --harness-auth vertex-ai`).

When you configure the explicit path, the automatic fallback is disabled. The credentials required for your chosen method **must** be present (either gathered from the local environment or provided via Hub secrets), otherwise the agent will immediately fail to start.

The available explicit authentication types are:

- **Provider API Key** (`api-key`): Direct API key authentication.
- **Vertex Model Garden** (`vertex-ai`): Google Cloud Vertex AI using Application Default Credentials (ADC).
- **Harness specific credential file** (`auth-file`): A credential file native to the harness, such as an OAuth token file.

:::note
Scion translates these universal explicit auth types to harness-native values internally. You should always use the universal values (`api-key`, `vertex-ai`, `auth-file`) in your Scion configuration.
:::

---

## Credential Sources & Setup

The following sections detail the environment variables and files that Scion consults for each authentication method, and how to configure them locally or via the Scion Hub.

### Provider API Key (`api-key`)

This is the simplest method, relying on standard environment variables to provide a direct API key.

**Required Sources:**
- **Claude**: `ANTHROPIC_API_KEY`
- **Gemini**: `GEMINI_API_KEY` or `GOOGLE_API_KEY`
- **OpenCode/Codex**: `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, or `CODEX_API_KEY`

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

### Vertex Model Garden (`vertex-ai`)

Uses Google Cloud's Vertex AI endpoints with Application Default Credentials (ADC). Scion supports two primary ways to authenticate in Hub mode: via an assigned GCP Identity (Service Account) or an injected ADC file secret.

**Required Sources:**
- **Assigned GCP Identity** (Hub Mode): If the agent is assigned a Hub-managed GCP Service Account via metadata emulation, Vertex AI will automatically use it. This is the recommended and most secure approach.
- **ADC JSON file** (Fallback/Local): Automatically discovered at `~/.config/gcloud/application_default_credentials.json` if present locally. In Hub mode, you can upload an ADC file via the `gcloud-adc` file secret or specify the `GOOGLE_APPLICATION_CREDENTIALS` environment variable pointing to a custom credential file.
- `GOOGLE_CLOUD_PROJECT`: Your Google Cloud project ID.
- `GOOGLE_CLOUD_REGION`: The region (e.g., `us-east5`). Required for Claude, optional but recommended for Gemini.

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

Some harnesses support their own specific credential files, such as OAuth tokens.

**Required Sources:**
- **Gemini**: `~/.gemini/oauth_creds.json`
- **Codex**: `~/.codex/auth.json`
- **OpenCode**: `~/.local/share/opencode/auth.json`

**Local Setup:**
If you have run the harness's native authentication command (e.g., `gemini auth login` on your host), Scion will automatically detect the resulting credential file and mount it into the agent.

**Hub Setup:**
Similar to ADC, you can upload these specific credential files as secrets via the Web Interface or CLI:
```bash
scion hub secret set --type file \
  --target ~/.gemini/oauth_creds.json \
  GEMINI_OAUTH_CREDS @~/.gemini/oauth_creds.json
```

---

## Agent Progeny & Secret Access

When an agent creates sub-agents (progeny), Scion enforces strict controls over which secrets those child agents can access. 

By default, child agents operate under a **granular secret access** model. They do not automatically inherit all secrets from the project or their parent. Instead, they only have access to the credentials necessary to perform their specific tasks, maintaining a least-privilege security boundary across the agent ancestry chain. 

---

## Troubleshooting

### "no valid auth method found"
The harness couldn't find any usable credentials through the automatic implicit approach. Check that you have exported the correct environment variables locally, or that your Hub secrets are properly assigned and available to the agent's workspace.

### "auth type selected but..."
You have configured the **Explicit Path** (e.g., selecting `vertex-ai`) but the specific credentials required for that path (like `GOOGLE_CLOUD_PROJECT`) are missing. The explicit path disables fallback, so ensure all required sources for the chosen explicit type are provided.

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

For detailed instructions on setting this up, see [Git-Based Projects](/scion/hub-user/git-projects/).
