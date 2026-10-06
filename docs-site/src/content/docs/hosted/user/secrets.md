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

1.  **User Scope** (Highest Priority): Personal secrets or variables for a specific user. Applied to all agents owned by that user.
2.  **Project Scope**: Project-level secrets or variables. Available to all agents running in a specific Project.
3.  **Hub Scope**: Platform-wide settings or secrets configured by Hub administrators.
4.  **Broker Scope** (Lowest Priority): Infrastructure-level secrets or variables. Available only to agents running on a specific Runtime Broker (e.g., for hardware-specific config).

**Resolution Priority:** When multiple scopes define the same secret key, the more specific scope wins. The precedence order is:
```text
runtime_broker  <  hub  <  project  <  user
```
Therefore, user-scoped settings have the highest priority and will override project, hub, and broker-scoped variables or secrets of the same name. Template `env` blocks and CLI `--env` flags are layered on top of resolved secrets.

### Reserved Names

Names beginning with `SCION_` or `GCE_METADATA_` (case-sensitive prefix match) are reserved for Scion's own control-plane variables. The Hub rejects with a validation error any environment variable, or `environment`-type secret, whose key or target uses one of these prefixes. `file` and `variable` secrets are not affected.

Values stored under a reserved name before this check existed are not deleted, but they are never used: the Hub drops them at dispatch and the Runtime Broker drops them again before injection. If a variable or secret collides with a value the runtime sets itself, the runtime's value wins.

`SCION_METADATA_MODE` is always set by the Hub. It comes from the agent's GCP identity configuration and defaults to `block`. A Runtime Broker only accepts an elevated mode (anything other than `block`) when the Hub's dispatch includes its source marker. Otherwise the Runtime Broker downgrades it, so a stored value or an older Hub cannot turn on metadata access.

---
## Injection Modes

Both environment variables and secrets support **Injection Modes**, which control how they are delivered to the agent container:

- **As Needed (Default)**: The variable or secret is only injected if it is explicitly requested in the agent's template (`scion-agent.yaml`) or harness configuration. This is the recommended mode for most credentials to minimize the attack surface.
- **Always**: The variable or secret is injected into *every* agent started within that scope, regardless of whether it is explicitly requested.

:::note[GITHUB_TOKEN and the Always mode]
When you enter a GitHub token while creating a git-backed project in the web UI, the Hub saves it as a project-scoped `GITHUB_TOKEN` secret with `always` injection mode. The token has to be present in the first environment-resolution pass so that `sciontool init` can clone the repository in **Clone-per-agent** and **Worktree-per-agent** sharing modes. On startup, the Hub also runs a one-time migration that moves any existing `GITHUB_TOKEN` secret from `as_needed` to `always`. Secrets you set after that migration keep the mode you give them, so pass `--always` when you set `GITHUB_TOKEN` yourself.
:::

You can set the injection mode via the CLI using the `--always` flag:

```bash
# Set a variable to be always injected in a project
scion hub env set --project --always LOG_LEVEL=debug

# Set a secret to be always injected for a user
scion hub secret set --always MY_GLOBAL_TOKEN secret-value
```

### Propagation to Descendant Agents (Progeny)

When an agent creates child/sub-agents (referred to as **progeny**), they do not inherit the parent agent's user-scoped configuration or secrets unless progeny propagation is enabled. This preserves a strict security and least-privilege boundary across agent ancestry chains.

You can enable progeny propagation in two ways:

1. **Per-secret**: Use the `--allow-progeny` flag when setting an individual secret or environment variable.
2. **Hub-wide default**: Hub administrators can configure a hub-wide default so that newly created user-scoped secrets default to progeny-enabled. Individual secrets can still override this default. When no hub-wide default is set, `AllowProgeny` defaults to `false`.

To explicitly enable propagation on a specific secret or variable, use the `--allow-progeny` flag:

* **User-Scoped Secrets**: Can be marked for progeny propagation at any time.
  ```bash
  scion hub secret set --allow-progeny MY_PERSONAL_TOKEN token-value
  ```
* **User-Scoped Environment Variables**: Can only be marked for progeny propagation if their injection mode is set to `always`.
  ```bash
  scion hub env set --always --allow-progeny PERSONAL_ENV=value
  ```

#### How it Works Under the Hood
Enabling progeny propagation dynamically registers implicit access policies (e.g. `progeny-secret-access:<id>` or `progeny-envvar-access:<id>`) on the Scion Hub. When a child agent resolves its configuration, Scion walks the ancestor chain of the calling agent container. If the configuration creator is part of that ancestry tree and has enabled progeny permission, the descendant agent safely inherits the setting or secret.

---

## Managing Environment Variables

Use the `scion hub env` command suite to manage non-sensitive configuration. Each agent environment variable carries **provenance metadata** (hub-injected, user-defined, or runtime-derived) to help you audit and debug the origin of specific values.

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

Secrets are write-only from host-level CLI commands and the Web Dashboard. Once set, their values cannot be read back by users. However, authorized agents running inside their containers can securely retrieve project-scoped secrets at runtime via the Hub API or the `sciontool` utility.

### Setting Secrets
Secrets can be set manually via the CLI or Web Dashboard, or gathered interactively during agent creation.

```bash
# Set a user-scoped secret
scion hub secret set ANTHROPIC_API_KEY sk-ant-api01-...

# Set a project-scoped secret
scion hub secret set --project DB_PASSWORD my-secure-password
```

#### Streamlined Project Secrets via `scion secret`

While `scion hub secret` manages secrets at any scope (user, project, broker, hub), you can use the streamlined, top-level `scion secret` command group on your host to manage project-scoped secrets directly within your current project context:

```bash
# Set a project-scoped secret (inferred from current directory context)
scion secret set ANTHROPIC_API_KEY sk-ant-api01-...

# List all project secrets (metadata only)
scion secret list

# Get metadata for a specific project secret
scion secret get ANTHROPIC_API_KEY
```

:::tip[Graceful Raw vs. Base64 Fallback]
To prevent silent integration failures (such as when the web UI sends raw plaintext but the underlying REST endpoint accepts base64), all four of Scion's secret-write API handlers (Hub, User, Project, and Broker scopes) feature a **graceful fallback mechanism**.

When writing a secret, the Hub checks if the payload is a valid base64-encoded string. If it is, the Hub decodes it back to raw bytes before encrypting. If it is not valid base64 (or if decoding fails), the Hub gracefully falls back to treating the payload as raw plaintext. This ensures that both base64-encoded binary payloads (e.g. key files) and raw plaintext API keys are accepted reliably.

To prevent accidental base64-decoding when setting plaintext secrets that happen to look like valid base64 (e.g., specific API keys), the CLI commands `scion hub secret set` and `scion secret set` explicitly send `encoding: raw` to bypass server-side base64 validation and ensure the secret is stored exactly as typed.
:::

**Interactive Secrets-Gather:**
If a template requires specific secrets (defined in `scion-agent.yaml`), Scion utilizes an interactive `secrets-gather` pipeline during agent creation. It will automatically prompt you to securely input any missing values and store them in the backend, ensuring sensitive credentials are never written to plain text configuration files.

### Secret Types
Secrets can be projected into the agent container in three ways:

1.  **Environment** (Default): Injected as a standard environment variable.
2.  **File**: Written to a specific path on the agent's filesystem.
3.  **Variable**: Added to a JSON file at `~/.scion/secrets.json` for programmatic access by the harness.

On the Cloud Run runtimes, secrets are delivered to the agent container as environment values, with these limits:

- **File and variable secrets** are sent together in one environment value, capped at 32 KiB on Cloud Run Instances and at a 128 KiB entry on `cloudrun-sandbox`. File secrets are base64-encoded inside that base64 value and variable secrets are not, so the total room for them is about 18-24 KiB on Cloud Run Instances and about 72-96 KiB on `cloudrun-sandbox`, depending on the mix. A larger set fails agent start with an error naming the largest secret.
- **Environment secrets** are limited to a 32 KiB value each on Cloud Run Instances and a 128 KiB `KEY=VALUE` entry each on `cloudrun-sandbox`. A larger one fails agent start with an error naming it.
- **Reserved keys:** environment secrets cannot set `SCION_STAGED_SECRETS` or `SCION_OTEL_GCP_CREDENTIALS` on either runtime. On Cloud Run Instances they also cannot override `SCION_HOST_UID` or `SCION_HOST_GID`; on `cloudrun-sandbox` they also cannot override `SCION_HOST_UID`, `SCION_HOST_GID`, `SCION_WORKSPACE_PATH`, `HOME`, `USER` or `LOGNAME`. The runtime sets these itself.
- **Existing keys:** a key that is already in the agent environment takes precedence over an environment secret with the same name, and the secret is not applied.

### Updating Secret Metadata

You can update a secret's configuration metadata (like its `type`, `target` path, or injection mode) without needing to re-enter its sensitive value or create a new version in the backend. This is supported via the Web UI "Edit Settings" dialog and the CLI:

```bash
# Change an existing secret's type to file and specify a target path
scion hub secret update MY_SECRET --type file --target ~/.my-secret
```

This metadata-only update (PATCH) guarantees that path traversal protections and scope validations are applied correctly without re-writing the encrypted payload.

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

### Agent Runtime Secret Retrieval

While environment and file-based injection deliver secrets at agent startup, Scion also supports **dynamic runtime secret retrieval** from inside the agent container. This enables harnesses or scripts to request project-scoped secrets programmatically as-needed, reducing initial environment exposure.

This runtime retrieval is accessible either via the `sciontool` helper utility or directly through the Hub API.

#### Using `sciontool`
From inside an agent container, use the `sciontool secret` command suite:

*   **List Available Secrets**: Lists metadata (keys, types, and injection targets) for all secrets in the agent's project. Sensitive values are omitted.
    ```bash
    sciontool secret list
    ```
    *Output:*
    ```text
    KEY              TYPE         TARGET
    ---              ----         ------
    MY_API_KEY       environment  MY_API_KEY
    CLAUDE_AUTH      file         ~/.claude/.credentials.json
    ```

*   **Retrieve a Secret Value**: Decodes and outputs the raw bytes of a specific secret to stdout (ideal for piping or scripting).
    ```bash
    sciontool secret get MY_API_KEY
    ```
    *Example script usage:*
    ```bash
    export API_KEY=$(sciontool secret get MY_API_KEY)
    ```

*   **Set a Secret**: You can write/update secrets from inside the container to persist credentials discovered or generated at runtime. Secrets can be scoped to either the **project** (visible to all agents in the project) or the **user** (personal secrets visible only to your own agents):
    ```bash
    # Set a project-scoped secret (default)
    sciontool secret set NEW_TOKEN "secret-value"

    # Set a user-scoped (personal) secret
    sciontool secret set MY_PERSONAL_TOKEN "token-xyz" --scope user
    ```
    *Note: `--scope` accepts `project` (default) or `user`.*

    *Hub admins can restrict agents to writing user (profile) scope only. If the "Restrict
    agent-written secrets to profile scope" setting is on (Admin > Server Config), a project-scope
    write from an agent — including one that omits `--scope` — is rejected with a 403 and a message
    telling you to retry with `--scope user`. This does not affect writes you make yourself through
    the web UI or `scion hub secret set --project`.*

#### Using the Hub API Directly
Under the hood, `sciontool` interacts with the Hub's agent-specific secrets API:

*   **`GET /api/v1/agents/{agentID}/secrets`**: Lists available secret metadata in the agent's project.
*   **`GET /api/v1/agents/{agentID}/secrets/{key}`**: Retrieves a single secret's metadata and its base64-encoded value.
*   **`PUT /api/v1/agents/{agentID}/secrets/{key}`**: Stores or updates a secret.
*   **`POST /api/v1/agent/secrets`**: Fetches several secret values in one call. There is no agent ID in the URL: the Hub identifies the agent from its token. The request body is `{"keys": ["KEY_A", "KEY_B"]}` (at most 100 keys). Each entry in the response has a `status` of `ok`, `not_found`, or `entitled_but_unavailable`. `sciontool init` calls this endpoint at startup to fetch the keys listed in `SCION_SECRET_KEYS`.

#### Security & Audit Logging
*   **Authentication**: API access is restricted to the running agent container. The agent must include its unique Hub-issued JWT (loaded from `SCION_HUB_TOKEN`) in the `Authorization: Bearer <token>` header of every request.
*   **Authorization**: Agents are strictly bounded to their own project's secrets. They can also access user-scoped (personal) secrets belonging to their originating user (the user who kicked off the agent chain), which are resolved on the Hub via the agent JWT's `OriginUserID` (the user who originally started the agent chain). Agents cannot access secrets in other projects, other users' secrets, or global Hub secrets unless explicitly shared via progeny policies (descendant access).
*   **Fail-Closed Reads**: Every runtime secret read goes through one check sequence: project permission, then an originating user who is still active and either an active member of the project (any active project role binding counts, built-in or custom, direct or through a group) or holds system-level authority for the exact `secret.use` permission, then progeny sharing. Membership alone does not grant reads: the project permission check (`project.secret_read`) still applies. A value is fetched only for the version recorded in the secret's metadata. If a value cannot be retrieved, that key is reported as unavailable in the response rather than returned as an empty value; other keys in the same request are unaffected.
*   **Audit Trail**: To ensure accountability, every runtime read and write operation is fully audited on the Hub. Each retrieval request logs one audit event (`agent_secret_read`) identifying the calling agent, the requested keys, and the outcome.

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

### Secrets Backend

Scion uses a secrets backend to store secret values securely. The recommended backend for production is **GCP Secret Manager**, while the default `local` backend stores values directly in the Hub database using AES-256-GCM encryption at rest (derived from the hub signing secret).

:::note[Encryption at Rest]
Secret values are never stored in plaintext. If using the default `local` backend, values are encrypted using AES-256-GCM (derived from the hub signing secret) before being written to the database. Legacy plaintext values migrate transparently on next read.
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
export SCION_SERVER_SECRETS_GCPPROJECTID=my-gcp-project
export SCION_SERVER_SECRETS_GCPCREDENTIALS=/path/to/service-account.json
```

#### User-Managed Replication Locations

By default, GCP Secret Manager uses automatic (global) replication. Organizations that enforce the `constraints/gcp.resourceLocations` org policy — which restricts or prohibits global resources — will see secret creation fail with this default.

To comply, set `gcp_replication_locations` to a list of GCP regions where secret replicas should be stored:

```yaml
server:
  secrets:
    backend: gcpsm
    gcp_project_id: "my-gcp-project"
    gcp_replication_locations:
      - us-east1
      - europe-west1
```

Or via the environment variable:

```bash
export SCION_SERVER_SECRETS_GCPREPLICATIONLOCATIONS=us-east1,europe-west1
```

When this field is non-empty, Scion creates secrets with **user-managed** replication restricted to the specified regions instead of automatic global replication. When empty or omitted, the default automatic replication behavior is preserved. This field is also editable through the admin settings UI and is stored in the HA Postgres config store in database mode.

When GCP Secret Manager is configured, Scion uses a **hybrid storage** model:
- **Metadata** (name, type, scope) is stored in the Hub database.
- **Secret values** are stored in GCP Secret Manager with automatic versioning.

#### IAM Permissions and Secret Naming

Every secret name in GCP Secret Manager is prefixed with a hash derived from the hub's instance ID: `scion-<h12>-<scope>-<hash>-<name>`, where `<h12>` is the first 12 hex characters of `sha256(hub_id)`. This lets you grant a hub's service account access to only its own secrets, instead of every secret in a shared GCP project.

In a project used by a single hub, `roles/secretmanager.admin` on the whole project is simplest. In a project shared by multiple hubs (or by a hub and other workloads), grant a **conditioned** binding scoped to the hub's prefix instead:

```
role: roles/secretmanager.admin
condition: resource.name.startsWith("projects/<PROJECT_NUMBER>/secrets/scion-<h12>-")
```

Note that the condition uses the GCP **project number**, not the project ID. `<h12>` is stable for the life of the hub's instance ID; deployment tooling (e.g. Terraform) computes the same value from `hub_id` to keep the grant in sync.

:::caution[Deploy ordering]
Grant the new hub-prefixed IAM condition **before** deploying a Hub binary that writes hub-prefixed names — every secret write targets the prefixed name immediately, so writes fail with a permission error otherwise. Keep the legacy grant (conditioned or project-wide — whichever this hub previously had) in place until `--delete-legacy` (below) has been run and verified; only then remove it. Once the legacy grant is removed, `migrate-names` can no longer read legacy names at all, so run a final `--dry-run --delete-legacy` pass to confirm zero pending items **before** removing it — afterward, a clean dry-run only proves IAM is narrowed, not that migration finished.

Run `--delete-legacy` itself only after **every** replica of this hub is running a binary that includes this change — i.e. no replica is still writing legacy names. GCP Secret Manager has no conditional delete: if an older binary is still a live writer during a mixed-version rolling deploy, its write can land between `--delete-legacy`'s safety check and the actual delete, and the delete then destroys that write's only copy along with the legacy container it lived in.
:::

Secrets created before this hub-prefixed scheme existed keep resolving under their original (legacy) name until migrated — existing secrets keep resolving through their stored reference until an administrator (or, for the built-in signing keys, the hub itself at boot) rewrites it to the prefixed name. An administrator can migrate legacy names forward with `scion hub secret migrate-names` (see `--help` for `--dry-run` and `--delete-legacy`); it is safe to run repeatedly, so a plain run followed later by a separate `--delete-legacy` run is the expected two-step workflow. Until that command's `--delete-legacy` step has run for a given secret, both the legacy and least-privilege-scoped IAM grants should remain in place.

On a Cloud Run hub deployed with the hub-cloudrun Terraform module (private-IP Cloud SQL, Direct VPC egress, an explicit `hub_id`), run this command via the one-off Cloud Run job in [`docs/deploy/migrate-names-cloudrun.md`](https://github.com/GoogleCloudPlatform/scion/blob/main/docs/deploy/migrate-names-cloudrun.md); no workstation ever runs it directly against that hub's database. Hubs deployed with the manual [Deploy on GCP](/scion/hosted/ha/setup-gcp/) guide are not covered by that runbook — see that guide's "Secret Name Migration" section and [ptone/scion#2395](https://github.com/ptone/scion/issues/2395) for the tracked gap.

---

## Technical Details

### Automatic Plugin Secret Migration & Safety (Hub Integrations)

For external messaging integrations (such as Discord, Telegram, or Google Chat) that run as Hub-level message broker plugins, Scion implements an automatic, secure secret migration and stripping pipeline to keep sensitive bot tokens and API keys out of plaintext configuration files:

- **Automatic One-Shot Migration**: When starting up or activating a plugin (e.g., via the runtime activation path in `activateInstalledIntegration`), the Hub automatically scans the integration's configuration—including per-plugin external configuration files and inline config blocks. Any discovered secret keys are automatically migrated into the configured secure **Secrets Backend** (such as GCP Secret Manager).
- **Copy-Before-Strip Safety**: After migrating the secrets, Scion strips the raw values in-place from the integration's file-based and inline configurations (`stripSecretKeysInPlace`) using a secure copy-before-strip helper to avoid any risk of partial writes or configuration corruption, while deduplicating warning logs.
- **Boot and Activation Consistency**: This migration-and-strip sequence runs consistently during both the Hub's boot-up routine and dynamic runtime integration activation, ensuring that secrets are never stored or exposed in plaintext configs.

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
