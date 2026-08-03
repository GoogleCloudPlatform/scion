---
title: Secret & Environment Management
description: Managing environment variables and secrets via the Scion Hub.
---

Scion's hosted architecture provides a centralized way to manage configuration and sensitive data across your team. Instead of sharing `.env` files or hardcoding credentials, you can use the Scion Hub to store and inject environment variables and secrets into your agents.

## Variables vs. Secrets

Scion distinguishes between regular environment variables and secure secrets:

| Feature | Environment Variables (`env`) | Secrets (`secret`) |
| :--- | :--- | :--- |
| **Visibility** | Read/Write (via API and CLI) | Write-only (cannot be read back) |
| **Storage** | Plaintext in database | Encrypted at rest / Externally stored |
| **Use Case** | API URLs, log levels, feature flags | API keys, passwords, private keys |
| **Injection** | Environment variables only | Environment, files, or JSON variables |

---

## Scoping

Both variables and secrets can be scoped to different levels. Scion resolves these hierarchically when an agent starts:

1.  **User Scope**: Personal secrets for a specific user. Applied to all agents owned by that user.
2.  **Project Scope**: Project-level secrets. Available to all agents running in a specific Project.
3.  **Broker Scope**: Infrastructure-level secrets. Available only to agents running on a specific Runtime Broker (e.g., for hardware-specific config).

**Resolution Priority:** When multiple scopes define the same secret key, the more specific scope wins. Broker scope has the highest priority, followed by Project, then User, then Hub. Template `env` blocks and CLI `--env` flags are layered on top of resolved secrets.

---
## Injection Modes

Both environment variables and secrets support **Injection Modes**, which control how they are delivered to the agent container:

- **As Needed (Default)**: The variable or secret is only injected if it is explicitly requested in the agent's template (`scion-agent.yaml`) or harness configuration. This is the recommended mode for most credentials to minimize the attack surface.
- **Always**: The variable or secret is injected into *every* agent started within that scope, regardless of whether it is explicitly requested.

You can set the injection mode via the CLI using the `--always` flag:

```bash
# Set a variable to be always injected in a project
scion hub env set --project --always LOG_LEVEL=debug

# Set a secret to be always injected for a user
scion hub secret set --always MY_GLOBAL_TOKEN secret-value
```

---

## Managing Environment Variables

Use the `scion hub env` command suite to manage non-sensitive configuration.

### Setting Variables
```bash
# Set a user-scoped variable
scion hub env set API_URL=https://api.example.com

# Set a project-scoped variable (inferred from current directory)
scion hub env set --project LOG_LEVEL=debug

# Set a variable only for a specific broker
scion hub env set --broker=my-gpu-node CUDA_VISIBLE_DEVICES=0
```

## Managing Secrets

Secrets are write-only. Once set, their values cannot be retrieved via the CLI or API; they are only decrypted and injected into the agent container at runtime.

### Setting Secrets
Secrets can be set manually via the CLI or Web Dashboard, or gathered interactively during agent creation.

```bash
# Set a user-scoped secret
scion hub secret set ANTHROPIC_API_KEY sk-ant-api01-...

# Set a project-scoped secret
scion hub secret set --project DB_PASSWORD my-secure-password
```

:::tip[Graceful Raw vs. Base64 Fallback]
To prevent silent integration failures (such as when the web UI sends raw plaintext but the underlying REST endpoint accepts base64), all four of Scion's secret-write API handlers (Hub, User, Project, and Broker scopes) feature a **graceful fallback mechanism**.

When writing a secret, the Hub checks if the payload is a valid base64-encoded string. If it is, the Hub decodes it back to raw bytes before encrypting. If it is not valid base64 (or if decoding fails), the Hub gracefully falls back to treating the payload as raw plaintext. This ensures that both base64-encoded binary payloads (e.g. key files) and raw plaintext API keys are accepted reliably.
:::

**Interactive Secrets-Gather:**
If a template requires specific secrets (defined in `scion-agent.yaml`), Scion utilizes an interactive `secrets-gather` pipeline during agent creation. It will automatically prompt you to securely input any missing values and store them in the backend, ensuring sensitive credentials are never written to plain text configuration files.

### Secret Types
Secrets can be projected into the agent container in three ways:

1.  **Environment** (Default): Injected as a standard environment variable.
2.  **File**: Written to a specific path on the agent's filesystem.
3.  **Variable**: Added to a JSON file at `~/.scion/secrets.json` for programmatic access by the harness.

### Mounting Files as Secrets
You can use the `@` prefix to read a secret's value from a local file. This is particularly useful for SSH keys or service account JSONs.

```bash
# Upload an SSH private key and mount it to the standard location in the agent
scion hub secret set --type file --target ~/.ssh/id_rsa SSH_KEY @~/.ssh/id_rsa
```

### Well-Known Secrets

Scion recognizes certain secret names and uses them for built-in platform features. Using the correct name causes the broker to perform additional setup automatically.

| Secret Name | Type | Target Path | Effect |
|-------------|------|-------------|--------|
| `scion-telemetry-gcp-credentials` | `file` | `~/.scion/telemetry-gcp-credentials.json` | Sets `SCION_OTEL_GCP_CREDENTIALS`, auto-enables GCP-native telemetry export, and reads `project_id` from the file if `SCION_GCP_PROJECT_ID` is not set. |

**Example — provisioning GCP telemetry credentials:**

```bash
scion hub secret set \
  --type file \
  --target ~/.scion/telemetry-gcp-credentials.json \
  scion-telemetry-gcp-credentials @/path/to/sa-key.json
```

Once set, every agent that starts will have the credential file mounted at `~/.scion/telemetry-gcp-credentials.json` and GCP-native telemetry will be enabled automatically — no additional environment variable configuration required. See [Metrics & OpenTelemetry](/scion/hosted/single-node/metrics/#4-gcp-credentials-for-agent-containers-non-adc-environments) for the full setup guide.

---

## GitHub Multi-Repo Credentials

When Scion resolves `gh://` URIs in template skill lists, it authenticates with the default `GITHUB_TOKEN` — typically a GitHub App installation token scoped to the project's own repository. This works for skills in public repos and the workspace repo itself, but **fails with 404** when a `gh://` URI references a skill in a different private repository.

To solve this, Scion supports **convention-based project secrets** that automatically provide the right credential for each `gh://` URI based on the GitHub owner and repository name. No template changes are needed — the resolver derives a secret name from the URI and looks it up in your project secrets.

### Naming Convention

Create a project secret with one of these naming patterns:

| Pattern | Scope | Example |
| :--- | :--- | :--- |
| `GH_{OWNER}__{REPO}` | One specific repo | `GH_ACME_CORP__PRIVATE_SKILLS` |
| `GH_{OWNER}` | All repos under an owner/org | `GH_ACME_CORP` |

**Normalization rules:** uppercase the name, replace hyphens (`-`) and dots (`.`) with underscores (`_`). The double underscore (`__`) separates owner from repo. *Note: Because of this normalization, names that differ only by hyphens, dots, or underscores (e.g., `acme-corp` and `acme_corp`) will resolve to the same secret name.*

**Examples:**

| GitHub Repository | Secret Name |
| :--- | :--- |
| `acme-corp/private-skills` | `GH_ACME_CORP__PRIVATE_SKILLS` |
| `my-org/my.special.repo` | `GH_MY_ORG__MY_SPECIAL_REPO` |
| All repos under `acme-corp` | `GH_ACME_CORP` |

### Setup

```bash
# Repo-specific credential (fine-grained PAT or classic PAT with repo access)
scion hub secret set --project GH_ACME_CORP__PRIVATE_SKILLS github_pat_...

# Or cover all repos under an owner with one token
scion hub secret set --project GH_ACME_CORP github_pat_...
```

Once set, template URIs resolve automatically — no `?token=` annotation needed:

```yaml
skills:
  - uri: "gh://acme-corp/private-skills/my-skill"  # auto-uses GH_ACME_CORP__PRIVATE_SKILLS
```

An explicit `?token=SECRET_NAME` parameter on the URI still works as an override when disambiguation is needed.

### Credential Resolution Order

When resolving a `gh://owner/repo/...` URI, Scion checks credentials in this order:

| Priority | Source | Description |
| :--- | :--- | :--- |
| 1 | `?token=SECRET_NAME` on the URI | Explicit override — bypasses convention lookup |
| 2 | `GH_{OWNER}__{REPO}` | Repo-specific convention secret |
| 3 | `GH_{OWNER}` | Owner-level convention secret |
| 4 | Default `GITHUB_TOKEN` | App token, environment, or provision secret cascade |
| 5 | Unauthenticated | No credential found; works for public repos only |

The first match wins. If no convention secret exists, behavior is identical to the default single-token resolution.

*Note: Credentials resolved for private `gh://` URIs are preserved end-to-end through the entire download sequence, preventing unauthenticated fallback or 404 errors during multi-file resolution.*

### Injection Mode Behavior

Convention-keyed GitHub secrets support the standard injection modes:

- **As Needed** (recommended): The credential is used at provision time to fetch skills and templates but is **not** exposed inside the agent container. This is the minimum-privilege posture.

  ```bash
  scion hub secret set --project GH_ACME_CORP__PRIVATE_SKILLS github_pat_...
  ```

- **Always**: The credential is also injected into the agent container as an environment variable (`GH_ACME_CORP__PRIVATE_SKILLS`). Use this when agents need runtime access to the same private repo.

  ```bash
  scion hub secret set --project --always GH_ACME_CORP__PRIVATE_SKILLS github_pat_...
  ```

For more on injection modes, see [Injection Modes](#injection-modes) above.

:::note[Implementation Reference]
This feature was introduced in [`GoogleCloudPlatform/scion` PR #919](https://github.com/GoogleCloudPlatform/scion/pull/919). For the full auth layer reference (PAT, GitHub App bot, `gh` CLI token, runtime fallbacks), see the `github-auth-fallback` agent skill.
:::

---

## Administrator Configuration (Hub)

To use secrets in production, the Hub must be configured with a production-grade secrets backend.

### Secrets Backend (Required)

Scion requires a secrets backend to store secret values. The recommended backend is **GCP Secret Manager**.

:::caution[No Plaintext Storage]
The Hub does not store secret values in its database. Attempting to create or update secrets without a configured backend (e.g., using the default `local` backend) will return an error. You must configure GCP Secret Manager to use secret management features.
:::

#### Configuring GCP Secret Manager

Set the backend in your `settings.yaml`:

```yaml
server:
  secrets:
    backend: gcpsm
    gcp_project_id: "my-gcp-project"
    gcp_credentials: "/path/to/service-account.json"  # Optional if using ADC
```

Or via environment variables:

```bash
export SCION_SERVER_SECRETS_BACKEND=gcpsm
export SCION_SERVER_SECRETS_GCP_PROJECT_ID=my-gcp-project
export SCION_SERVER_SECRETS_GCP_CREDENTIALS=/path/to/service-account.json
```

When GCP Secret Manager is configured, Scion uses a **hybrid storage** model:
- **Metadata** (name, type, scope) is stored in the Hub database.
- **Secret values** are stored in GCP Secret Manager with automatic versioning.

---

## Technical Details

### Resolution Hierarchy
When an agent starts, the Runtime Broker requests a "Resolved Environment" from the Hub. The Hub merges secret values in this order (last one wins for the same key):
1. Hub Secrets (global defaults)
2. User Secrets
3. Project Secrets
4. Broker Secrets
5. Template `env` block
6. CLI `--env` flags

### Security
Secrets are transmitted over TLS between the Hub and Runtime Brokers. They are only decrypted by the Hub during the dispatch process and sent over an encrypted channel to the Runtime Broker. The Broker then injects them directly into the container's memory space. Brokers never persist agent secrets to disk.

For a detailed overview of the security architecture, see the [Security Architecture Reference](/scion/reference/security/).
