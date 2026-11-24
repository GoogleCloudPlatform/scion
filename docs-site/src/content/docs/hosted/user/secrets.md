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

1.  **Agent Scope** *(Highest Priority, secrets only, behind `hub.agent_secrets`)*: Secrets bound to a single agent, readable only by that agent's token. See [Agent-Scoped Secrets](#agent-scoped-secrets).
2.  **User Scope**: Personal secrets or variables for a specific user. Applied to all agents owned by that user.
3.  **Project Scope**: Project-level secrets or variables. Available to all agents running in a specific Project.
4.  **Hub Scope**: Platform-wide settings or secrets configured by Hub administrators. Hub-scoped secrets reach agents only as `environment` secrets: `always` secrets are delivered at start and restart, and `as_needed` secrets are filled only by the harness credential pass for keys the harness asks for; see [Hub-Scoped Secrets](#hub-scoped-secrets).
5.  **Broker Scope** (Lowest Priority): Infrastructure-level secrets or variables. Available only to agents running on a specific Runtime Broker (e.g., for hardware-specific config).

**Resolution Priority:** When multiple scopes define the same key, the more specific scope wins. The precedence order is:
```text
Environment variables:  runtime_broker  <  hub  <  project  <  user
Secrets:                runtime_broker  <  hub  <  project  <  user  <  agent
```
Therefore, agent-scoped secrets have the highest priority among secret scopes (when `hub.agent_secrets` is enabled), and user-scoped settings have the highest priority among environment-variable scopes. The scope order applies within variables and within secrets. For the same key, a variable from any scope takes precedence over an environment-type secret. Environment values in the agent-create request (from the API, the web form or `--config`) outrank all scopes when they are non-empty; an empty value yields to the scopes. A template's `env` block is a fallback, used only for keys that no other source sets. See [Settings Precedence](/scion/reference/settings-precedence/#system-a--environment-variables) for the full order.

### Scope and Sharing

A scope decides which agents receive a variable or secret, not which people can use it. The Hub applies the scopes to every agent it starts, whoever starts it:

| Scope | Agents that receive it |
| :--- | :--- |
| **Hub** | Every agent on the Hub, started by any user. For secrets, only `environment` secrets (see [Hub-Scoped Secrets](#hub-scoped-secrets)). |
| **Broker** | Every agent running on that Runtime Broker, started by any user. |
| **Project** | Every agent in that Project, whoever starts it. |
| **User** | Only agents owned by that user. An agent started by another agent (a child agent) is owned by its parent agent, so it receives user-scoped values only if they are marked for progeny, with `--allow-progeny` or the hub-wide default (see [Propagation to Descendant Agents](#propagation-to-descendant-agents-progeny)), and (when the agent's delegation chain has recorded provenance) every hop on the chain allows `secret.use`. |
| **Agent** | Only the single bound agent (`scopeId` equals the agent's UUID; secrets only, gated by `hub.agent_secrets`). Never shared with other agents or progeny (`allowProgeny` is rejected with `400`). |

Hub, Broker and Project scopes apply to child agents in the same way as to the agents users start directly.

The [injection mode](#injection-modes) does not change who can receive a value. An `always` value is injected into every agent in its scope. An `as_needed` environment value is injected into any agent in its scope whose template or harness configuration requests that key. Template choice is up to the user starting the agent, so any user who can start an agent in the scope can request the key. `file` and `variable` secrets are placed in every agent in their scope, whatever their injection mode. Hub scope is different for secrets: only `environment` secrets reach agents (`always` at start and restart, `as_needed` only through the harness credential pass; see [Hub-Scoped Secrets](#hub-scoped-secrets)).

:::caution[Hub and Broker scopes are shared]
Hub-scoped and Broker-scoped variables and secrets are shared credentials. Every user who can start an agent on the Hub, or on that Runtime Broker, receives them in their agents. These scopes do not keep a value private to one user. Put only values there that every such user may hold.

Store sensitive secrets at **User** scope, so that only your own agents receive them, or at **Project** scope. A Project-scoped secret is shared with every member of the Project: their agents receive it, and those agents can also read it at runtime (see [Agent Runtime Secret Retrieval](#agent-runtime-secret-retrieval)).
:::

### Start-Time Authorization

When the Hub starts or restarts an agent, it authorizes each Agent (when `hub.agent_secrets` is on), User, Project, Hub, and Runtime Broker variable and secret for that agent **before** applying scope precedence (`runtime_broker < hub < project < user < agent` for secrets; `runtime_broker < hub < project < user` for variables) or reading any secret value from the secret backend. For User, Project, Hub, and Runtime Broker items, the check follows the agent's recorded delegation chain: the user or agent that delegated authority to it, and so on up to a user. For Agent-scope secrets, authorization is a structural association — the secret's `scope_id` must match the dispatched agent's ID in the agent's project — and is not subject to the owner-material chain. An item the check refuses is left out before scope precedence runs, so a refused higher-scope entry yields to a permitted lower-scope entry with the same key rather than masking it, and the start itself goes ahead. The Hub log has a `Warn` line for each item left out, with its name, scope and the reason, never its value. Only the secrets that survive both authorization and scope precedence are read from the secret backend. When the secret backend cannot resolve the agent's secret metadata at all, the Hub refuses the create, start, or restart with `503` (`agent secrets could not be resolved; the agent was not started`, or `... not created` on create). When an individual secret's metadata resolves but the secret store cannot return its value (`access`, `not_found`, or `missing`), the Hub skips that secret so the dispatch proceeds without it, emits one `WARN` log line (`launch material skipped: secret store read failed`) with `agent_id`, `purpose`, `count`, and `items` (`name`, `scope`, `class` — never secret values, backend references, scope IDs, or backend error text), and returns one warning in the create, start, or restart response's `warnings` array (`secrets not delivered because the secret store could not return them: <items>; the agent was <started|restarted|created> without them`). In that response warning, project-scope secrets are named (`<name> (project)`) for every requester, agent-scope secrets are named (`<name> (agent)`) only to requesters who can edit the agent (`agent.update`) and counted (`N agent-scope secret(s)`) otherwise, user-scope secrets are named (`<name> (user)`) when the requester is the agent's owner and counted (`N user-scope secret(s)`) otherwise, and hub- and broker-scope secrets are always counted (`N hub-scope secret(s)`, `N broker-scope secret(s)`).

Agents whose delegation chain contains legacy data may not have the recorded chain this check needs. This includes agents created before the chain was recorded, and agents created later under such an agent. The Hub keeps giving those agents their Project, Hub and Runtime Broker values, and logs a rate-limited `Warn` line naming the agent, the item and the group the agent falls in. The groups and the cases that are still refused are listed under [Launch material for legacy agents](/scion/reference/delegation-provenance-adoption/#launch-material-for-legacy-agents).

### Agents created from a user access token

An agent created from a [user access token](/scion/hosted/user/personal-access-tokens/) receives your shared secrets and its launch material the way an agent created from a sign-in does. The token's scopes narrow the API actions the agent may take, but they do not narrow the delivery of material to it at start, resume and restart: Project, Hub and Runtime Broker values, the secrets sent for skill fetches, and your user-scoped secrets shared with progeny (`--allow-progeny`). Every other check still applies to each item, including the item's association with the agent, access constraints, and the user-scoped secret checks described under [How it Works Under the Hood](#how-it-works-under-the-hood). This is delivery to the agent, not secret management: a user access token itself cannot read or change User- or Hub-scope secrets.

As for any agent, launch material reaches the agent only while the user who created it is still a member of the project. A token-created agent, or one of its sub-agents, whose creator has left the project starts without it, and the `Warn` line for each item names the cause, `root user not admitted to the project`.

Reading project-scoped secrets at runtime (`scion secret get --value`, `scion secret list`, and the agent secret endpoints) is not covered: the agent token the Hub issues to such an agent does not carry `project:secret:read`, so project-scope runtime reads and lists are refused. The full agent role is not available from a token for the same reason. When `hub.agent_secrets` is enabled, an agent can still read its own **agent-scoped** secrets without `project:secret:read`.

### Your Own User Values Follow the Agent's Delegation Chain

A User-scoped variable or secret that is **not** marked for progeny is your own. The Hub delivers it at create, start, resume and restart only to an agent you own whose delegation chain is recorded and ends at you, and whose ancestry, if it has one, starts at you. Otherwise the Hub withholds it, along with your user TZ and the clone token a `NoAuth` agent would take from your `GITHUB_TOKEN`. A Project, Broker or Hub value with the same key still applies. This covers:

- an agent whose authority another user re-recorded, for example by reincarnating it with `--role` (the reincarnate response then carries a warning);
- a sub-agent whose ancestry does not start at you;
- an agent whose delegation chain is not recorded, such as an agent created by an older Hub version. There is no transition period for these agents.

Each withheld row is counted as `scion.hub.user_material.owner_chain` with `outcome=withheld`, and the Hub logs a `Warn` naming the agent, the site and the reason (at most once every 10 minutes per agent, site and reason). It never logs a key or a value.

**Restore delivery.** As the agent's owner, reincarnate it from a CLI session without `--role` (`scion reincarnate <agent>`), or delete and re-create it. Reincarnating records the agent's authority again, with you as the delegator, bounded by your own right to delegate the agent's role and by your credential's scopes.

**Find affected agents.** Run the read-only delivery census against a copy of the Hub database or a read replica: `go run ./hack/delivery-census -driver sqlite -db <hub.db>`, or `-driver postgres -db <url>`. Run it before the upgrade, on the previous version's data, and after. Its `owner material chain` section lists each affected agent with the reason, its depth, the names of the withheld variables and secrets (never their values), the names of your shared secrets that start also withholds from it, and the remedy. The output names secret rows, so treat it as operator-only: keep it out of issues, chats and other shared places.

### Reserved Names

Names beginning with `SCION_` or `GCE_METADATA_` (case-sensitive prefix match) are reserved for Scion's own control-plane variables. The Hub rejects with a validation error any environment variable, or `environment`-type secret, whose key or target uses one of these prefixes. `file` and `variable` secrets are not affected.

Values stored under a reserved name before this check existed are not deleted, but they are never used: the Hub drops them at dispatch and the Runtime Broker drops them again before injection. If a variable or secret collides with a value the runtime sets itself, the runtime's value wins.

`SCION_METADATA_MODE` is always set by the Hub. It comes from the agent's GCP identity configuration and defaults to `block`. A Runtime Broker only accepts an elevated mode (anything other than `block`) when the Hub's dispatch includes its source marker. Otherwise the Runtime Broker downgrades it, so a stored value or an older Hub cannot turn on metadata access.

### Hub-Scoped Secrets

Hub-scoped secrets reach agents only when their type is `environment`, apart from the telemetry credential file (below). Start and restart deliver a Hub-scoped `environment` secret directly only when its mode is `always`. A Hub-scoped `environment` secret with the `as_needed` mode is not delivered in the agent environment at start or restart; it is filled only by the harness credential pass for a key the harness asks for. Hub-scoped secrets of type `file` or `variable` are not delivered to any agent at create, start, restart or during credential gathering. They are left out before scope precedence, so a Runtime Broker-scoped secret with the same name applies instead.

One Hub-scoped `file` secret is delivered: the telemetry credential file, when its name is `scion-telemetry-gcp-credentials` and its target is a path whose last two parts are `.scion/telemetry-gcp-credentials.json`, such as `~/.scion/telemetry-gcp-credentials.json` (see [Well-Known Secrets](#well-known-secrets)). A Hub-scoped file secret with any other name or target is not delivered.

This applies to Hub-scoped secrets only. Hub-scoped environment variables (`scion hub env set`), and secrets at User, Project and Runtime Broker scope, are not affected. To deliver a credential that has to be a file or a variable, store it at Project or User scope.

An agent reading Hub-scoped secrets (`GET /api/v1/secrets?scope=hub`, `GET /api/v1/env?scope=hub` and their single-key routes) sees only the `environment` secrets with the `always` mode. Every other Hub-scoped secret, including the telemetry credential file, is left out of those lists, and a request for it by name answers as for a key that does not exist. Hub administrators see every Hub-scoped secret.

---
## Injection Modes

Both environment variables and secrets support **Injection Modes**, which control how they are delivered to the agent container:

- **As Needed (Default)**: The variable or secret is only injected if it is explicitly requested in the agent's template (`scion-agent.yaml`) or harness configuration (for a Hub-scoped `environment` secret, only when the harness credential pass asks for that key; see [Hub-Scoped Secrets](#hub-scoped-secrets)). This is the recommended mode for most credentials to minimize the attack surface.
- **Always**: The variable or secret is injected into *every* agent started within that scope, regardless of whether it is explicitly requested.

:::note[GITHUB_TOKEN and the Always mode]
When you enter a GitHub token while creating a git-backed project in the web UI, the Hub saves it as a project-scoped `GITHUB_TOKEN` secret with `always` injection mode. The token has to be present in the first environment-resolution pass so that `sciontool init` can clone the repository in **Clone-per-agent** and **Worktree-per-agent** sharing modes. On startup, the Hub also runs a one-time migration that moves any existing `GITHUB_TOKEN` secret from `as_needed` to `always`. Secrets you set after that migration keep the mode you give them, so after you set `GITHUB_TOKEN` yourself, set its mode to `always` (for example, `scion hub secret update --project GITHUB_TOKEN --injection-mode always`).
:::

You can set the injection mode via the CLI using the `--always` flag of `scion hub env set` or the `--injection-mode` flag of `scion hub secret update`:

```bash
# Set a variable to be always injected in a project
scion hub env set --project --always LOG_LEVEL=debug

# Set a secret to be always injected for a user
scion hub secret set MY_GLOBAL_TOKEN secret-value
scion hub secret update MY_GLOBAL_TOKEN --injection-mode always
```

### Propagation to Descendant Agents (Progeny)

When an agent creates child/sub-agents (referred to as **progeny**), they do not inherit the parent agent's user-scoped configuration or secrets unless progeny propagation is enabled. This preserves a strict security and least-privilege boundary across agent ancestry chains.

You can enable progeny propagation in two ways:

1. **Per-secret**: Use the `--allow-progeny` flag when setting or updating an individual secret.
2. **Hub-wide default**: Hub administrators can configure a hub-wide default so that newly created user-scoped secrets default to progeny-enabled. Individual secrets can still override this default. When no hub-wide default is set, `AllowProgeny` defaults to `false`.

To explicitly enable propagation on a specific secret, use the `--allow-progeny` flag:

* **User-Scoped Secrets**: Can be marked for progeny propagation at any time. When an agent saves a user-scoped secret on your behalf, it waits for your approval first, and you can turn progeny access off when approving (`scion hub secret pending approve <ID> --no-progeny` or by unticking **Also deliver to agents your agents create** in **Profile > Secrets**; see [Approving user secrets saved by your agents](#approving-user-secrets-saved-by-your-agents)).
  ```bash
  scion hub secret set --allow-progeny MY_PERSONAL_TOKEN token-value
  ```
* **User-Scoped Environment Variables**: Can only be marked for progeny propagation if their injection mode is set to `always`. Progeny propagation for environment variables cannot currently be set from the CLI: `scion hub env set` has no `--allow-progeny` flag.

#### How it Works Under the Hood
Enabling progeny propagation dynamically registers implicit access policies (e.g. `progeny-secret-access:<id>` or `progeny-envvar-access:<id>`) on the Scion Hub. When an agent starts, restarts, or resumes (including `as_needed` secret resolution) or reads secrets at runtime, Scion walks the calling agent's ancestry and delegation chain: if the secret's owner is at the root of that ancestry tree and has enabled progeny permission, and — when the agent's delegation chain carries recorded provenance — every recorded delegation hop on the chain allows `secret.use`, every agent delegator on the chain is live and holds `project:secret:read`, and the root user is active, admitted to the project, not restricted from `secret.use` by an access constraint, and the recorded author of the secret, then the agent inherits the secret (if a user-scoped secret is withheld at start by this check, a lower-precedence project, Runtime Broker, or Hub secret with the same key still applies; see [Security & Audit Logging](#security--audit-logging)). At start, restart, and resume, progeny-shared owner secrets are decided once per request on the owner-material chain, and a legacy chain with no recorded ancestry is withheld.

---

## Managing Environment Variables

Use the `scion hub env` command suite to manage non-sensitive configuration. Each agent environment variable carries **provenance metadata** (hub-injected, user-defined, or runtime-derived) to help you audit and debug the origin of specific values.

User- and Hub-level environment variables and secrets (`scope=user`, the default when `--project` and `--broker` are omitted, and `scope=hub`) require an **interactive login** (`scion hub auth login`, a web session, or workstation dev auth). Scoped user access tokens (`scion_pat_*`) are refused on all User- and Hub-scope environment variable and secret operations—whatever their boundary or scopes—with `403 Forbidden`, `details.reason: "CREDENTIAL_MANAGEMENT"` (`details.credential: "session_required"`), and the message `"user- and hub-level environment variables and secrets require an interactive login (run 'scion hub auth login'); access tokens are not accepted"`. Project (`--project`) and Runtime Broker (`--broker`) scopes continue to accept appropriately scoped tokens.

Hub-scope (`scope=hub`) environment variable and secret routes also require an unscoped local platform admin. As the one read-only exception, an interactive login holding `hub.env_vars.read` (for example through the `hub-admin` role) can list Hub-scope environment variables (`GET /api/v1/env?scope=hub`), with secret entries omitted; reading a single Hub-scope variable, writing or deleting one, and every Hub-scope secret endpoint require a super-admin on an interactive login. (Agents may still read Hub-scope environment variables and the Hub-scope secrets described under [Hub-Scoped Secrets](#hub-scoped-secrets), and a federated caller may read—never write—its own User-scope variables and secrets.)

### Setting Variables
```bash
# Set a user-scoped variable
scion hub env set API_URL=https://api.example.com

# Set a project-scoped variable (a bare --project infers the directory's linked Hub project, else the 'scion use' project; or pass --project=<name|id>)
scion hub env set --project LOG_LEVEL=debug

# Set a variable only for a specific Runtime Broker
scion hub env set --broker=my-gpu-node CUDA_VISIBLE_DEVICES=0
```

## Managing Secrets

Secrets are write-only from host-level CLI commands and the Web Dashboard. Once set, their values cannot be read back by users. However, authorized agents running inside their containers can securely retrieve project-scoped secrets at runtime via the Hub API or the `scion secret` CLI commands (and write secrets via `sciontool secret set`). As with environment variables above, User- and Hub-scope secret operations (`scion hub secret` without `--project` or `--broker`, or with `--hub-scope`) require an interactive login (`scion hub auth login`) and refuse user access tokens with `403` (`details.reason: "CREDENTIAL_MANAGEMENT"`).

### Setting Secrets
Secrets can be set manually via the CLI or Web Dashboard, or gathered interactively during agent creation.

```bash
# Set a user-scoped secret
scion hub secret set ANTHROPIC_API_KEY sk-ant-api01-...

# Set a project-scoped secret (a bare --project infers the directory's linked Hub project, else the 'scion use' project; or pass --project=<name|id>)
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

`scion secret get --value KEY` prints the secret's value instead, which only works inside an agent container (see [Agent Runtime Secret Retrieval](#agent-runtime-secret-retrieval) below).

:::tip[Graceful Raw vs. Base64 Fallback]
To prevent silent integration failures (such as when the web UI sends raw plaintext but the underlying REST endpoint accepts base64), all four of Scion's secret-write API handlers (Hub, User, Project, and Broker scopes) feature a **graceful fallback mechanism**.

When writing a secret, the Hub checks if the payload is a valid base64-encoded string. If it is, the Hub decodes it back to raw bytes before encrypting. If it is not valid base64 (or if decoding fails), the Hub gracefully falls back to treating the payload as raw plaintext. This ensures that both base64-encoded binary payloads (e.g. key files) and raw plaintext API keys are accepted reliably.

To prevent accidental base64-decoding when setting plaintext secrets that happen to look like valid base64 (e.g., specific API keys), `scion hub secret set` explicitly sends `encoding: raw` for plaintext values to bypass server-side base64 validation and ensure the secret is stored exactly as typed. `scion secret set` instead base64-encodes every value on the client (file contents from `@file` included), so the Hub decodes it back to exactly the bytes given.
:::

**Interactive Secrets-Gather:**
If a template requires specific secrets or environment variables (defined in `scion-agent.yaml`), Scion utilizes an interactive gather pipeline during agent start and resume. For secrets, it prompts you for missing values and sends them for this agent only; they are not stored (store a secret on the Hub with `scion hub secret set` to reuse it), so sensitive credentials are never written to plain text configuration files. For environment variables found in your local environment, interactive `scion start` and `scion resume` prompt before sending them (confirmed automatically by `--yes`), while non-interactive `scion start` and `scion resume` send local environment values only when `--send-env` is passed and fail without it, naming the missing keys and the `--send-env` flag. When the CLI sends gathered environment values without showing the confirmation prompt (with `--yes`, with `--send-env` in a non-interactive run, or with `--format json`), it prints one line to `stderr` naming the keys, sorted (`Sending N environment value(s) to the Hub: KEY_A, KEY_B`), never the values.

### Secret Types
Secrets can be projected into the agent container in three ways:

1.  **Environment** (Default): Injected as a standard environment variable.
2.  **File**: Written to a specific path on the agent's filesystem.
3.  **Variable**: Added to a JSON file at `~/.scion/secrets.json` for programmatic access by the harness.

On the Cloud Run runtimes, secrets are delivered to the agent container as environment values, with these limits:

- **File and variable secrets** are sent together in one environment value, capped at 32 KiB on Cloud Run Instances and at a 128 KiB entry on `cloudrun-sandbox`. File secrets are base64-encoded inside that base64 value and variable secrets are not, so the total room for them is about 18-24 KiB on Cloud Run Instances and about 72-96 KiB on `cloudrun-sandbox`, depending on the mix. On Cloud Run Instances the same value also carries the agent's Hub credential, which takes about 1-2 KiB of that room, and the harness credentials of the agent's harness configuration. A larger set fails agent start. On `cloudrun-sandbox` the error names the largest secret; on Cloud Run Instances it names only the largest part (the Hub credential, the file and variable secrets, or the harness secrets), and the Runtime Broker logs the size of the value and of each part at every start. To fix it, make the largest part smaller, for example by moving a large value to an environment secret, which has its own limit.
- **Environment secrets** are limited to a 32 KiB value each on Cloud Run Instances and a 128 KiB `KEY=VALUE` entry each on `cloudrun-sandbox`. A larger one fails agent start with an error naming it.
- **Reserved keys:** environment secrets cannot set `SCION_STAGED_SECRETS` or `SCION_OTEL_GCP_CREDENTIALS` on either runtime, and `SCION_HUB_TOKEN_STAGED` is dropped from the container environment from any source (secret or environment variable) on every runtime so only `sciontool init` sets it during its staging pass. On Cloud Run Instances environment secrets also cannot override `SCION_HOST_UID`, `SCION_HOST_GID`, `SCION_HARNESS_SECRETS_DIR` or `SCION_HARNESS_OUTPUTS_DIR`; on `cloudrun-sandbox` they also cannot override `SCION_HOST_UID`, `SCION_HOST_GID`, `SCION_WORKSPACE_PATH`, `HOME`, `USER` or `LOGNAME`. The runtime sets these itself.
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

At Hub scope, the secret is delivered only with exactly this name and type and a target path whose last two parts are `.scion/telemetry-gcp-credentials.json`, and it is not listed to agents (see [Hub-Scoped Secrets](#hub-scoped-secrets)). Once set, every agent that starts will have the credential file mounted at `~/.scion/telemetry-gcp-credentials.json` and GCP-native telemetry will be enabled automatically — no additional environment variable configuration required. See [Metrics & OpenTelemetry](/scion/hosted/single-node/metrics/#4-gcp-credentials-for-agent-containers-non-adc-environments) for the full setup guide.

---

### Agent Runtime Secret Retrieval

While environment and file-based injection deliver secrets at agent startup, Scion also supports **dynamic runtime secret retrieval** from inside the agent container. This enables harnesses or scripts to request project-scoped secrets programmatically as-needed, reducing initial environment exposure.

This runtime retrieval is accessible either via the `scion` CLI or directly through the Hub API.

#### Using the `scion` CLI inside an agent
From inside an agent container, read secrets with `scion secret`. The calls are made as the agent, with its own hub identity:

*   **List Available Secrets**: Lists metadata (keys, types, versions and update times) for all secrets in the agent's project. Values are omitted.
    ```bash
    scion secret list
    ```

*   **Retrieve a Secret Value**: Decodes and writes the raw bytes of a specific secret to stdout, with no added newline (ideal for piping or scripting).
    ```bash
    scion secret get --value MY_API_KEY
    ```
    *Example script usage:*
    ```bash
    export API_KEY=$(scion secret get --value MY_API_KEY)
    ```
    Without `--value` (or with `--metadata`), `scion secret get MY_API_KEY` prints the secret's metadata. `--format json` returns metadata only and cannot be combined with `--value`.

`sciontool secret get` has moved to `scion secret get --value`, and `sciontool secret list` to `scion secret list`; the old commands now print a message naming the `scion` command and exit `1`.

#### Writing secrets with `sciontool`

*   **Set a Secret**: You can write/update secrets from inside the container to persist credentials discovered or generated at runtime. Secrets can be scoped to either the **project** (visible to all agents in the project) or the **user** (personal secrets visible only to your own agents):
    ```bash
    # Set a project-scoped secret (default)
    sciontool secret set NEW_TOKEN "secret-value"

    # Set a user-scoped (personal) secret
    sciontool secret set MY_PERSONAL_TOKEN "token-xyz" --scope user
    ```
    *Note: `--scope` accepts `project` (default), `user`, or `agent`. `--scope agent` stores a secret private to the calling agent and needs the `hub.agent_secrets` experiment; the agent removes it with `sciontool secret delete KEY --scope agent`. See [Agent-Scoped Secrets](#agent-scoped-secrets).*

    *Hub admins can restrict agents to writing user (profile) scope only. If the "Restrict
    agent-written secrets to profile scope" setting is on (Admin > Server Config), a project-scope
    write from an agent — including one that omits `--scope` — is rejected with a 403 and a message
    telling you to retry with `--scope user`. This does not affect writes you make yourself through
    the web UI or `scion hub secret set --project`.*

#### Approving user secrets saved by your agents

A user-scoped secret that an agent saves (with `sciontool secret set --scope user`, or through
harness credential capture) is not delivered to any agent until you approve it. A user secret
reaches your agents in every project, so you decide on each one an agent saves:

*   The save is held as a **pending secret**. `sciontool` reports
    `saved, pending approval in Scion`, and capture-auth reports `captured (pending approval in Scion)`.
    `--force` has no effect at user scope.
*   If you already have a user secret with the same name, it stays in effect until you approve
    the replacement.
*   A pending secret expires 7 days after the agent first saved it. The agent that saved it can
    replace it with a new value; the expiry stays the same.
*   While a secret is pending, another agent cannot save a user secret with the same name. Its
    save is refused with `409` (`secret_pending_held_by_other_agent`), and capture-auth exits
    with code `3`. Approve or reject the pending secret first.

Review and decide with the `scion` CLI. Values are shown only by `reveal`:

```bash
# List your pending secrets: name, type, target, the agent and project that saved it,
# when it was saved and when it expires
scion hub secret pending list

# Show one, with the project secrets of the same name it would override
scion hub secret pending show <ID>

# Print its value to check it before approving (audited; it stays pending)
scion hub secret pending reveal <ID>

# Approve one: it becomes your user secret, delivered from your agents' next start
scion hub secret pending approve <ID>

# Approve without sharing it with agents that your agents create
scion hub secret pending approve <ID> --no-progeny

# Reject one: the pending secret is deleted
scion hub secret pending reject <ID>
```

Only you can list, show, reveal, approve or reject your pending secrets, from an interactive
login (`scion hub auth login`). Agents, access tokens, other users and hub admins cannot see or
decide on them.

*   An approval writes the secret only while your existing user secret of that name is the one
    the agent saw when it saved the pending secret. If you created, changed or deleted that
    secret since, the approval is refused with `409` (`secret_pending_existing_changed`) and
    nothing changes. Reject the pending secret and have the agent save it again.
*   An approval and a rejection of the same pending secret never both succeed: the second one
    answers `404`.
*   Deleting a project deletes the pending secrets its agents saved. Deleting the agent that
    saved a pending secret does not; you can still approve or reject it.
*   Approve, reject and reveal are refused (`403`) when a browser sends them from another site.
    Behind a front proxy, the proxy must pass the public `Host` header through to the Hub
    unchanged; otherwise approvals, rejections and reveals from the web UI are refused with `403`.

##### Limits, expiry, reveal and notifications

*   **Limits**: one agent can have at most 10 pending secrets, and your account at most 50 from
    all agents. A save over a limit is refused with `429` (`secret_pending_cap`, `details.cap` is
    `agent` or `owner`) and nothing is stored; `sciontool` reports that too many secrets await
    your approval. An agent saving a secret it already has pending again replaces it and does
    not count twice.
*   **Expiry**: an expired secret is gone at once on every path (it is not listed and cannot be
    approved, rejected or revealed), and an hourly cleanup deletes it and its stored value. The
    same cleanup deletes a stored pending value that no pending secret refers to any more (for
    example after a Hub restart in the middle of a save or an approval), once it is more than an
    hour old; with the GCP Secret Manager backend these are the Hub's own `pending-` secrets,
    never another secret.
*   **Show and reveal**: `scion hub secret pending show <ID>` prints its name, type, target,
    length, the agent and project that saved it, its times, what approving it would do, and the
    project secrets of the same name, in projects you can see, that it would override. The value
    is never shown there. `scion hub secret pending reveal <ID>` prints the value so you can
    check it before you approve it; the secret stays pending, and the Hub records every reveal in
    its audit log. On a terminal, control characters in the value (escape sequences, carriage
    returns) are shown as visible escapes such as `\x1b`; redirected to a file, or with `--raw`,
    the value is written exactly as stored. Names an agent chose (key, target, agent and project
    names) are shown the same way in `pending list` and `pending show`. Reveal works only from
    your own interactive login: agents, user access tokens, federated callers, other users and
    Hub admins are refused.
*   **Listing**: `scion hub secret list` (user scope) has a `STATE` column, `approved` or
    `pending`; a name with an approved secret and a pending replacement appears twice. The Hub
    API offers the same list as `GET /api/v1/secrets?scope=user&includePending=true` (field
    `items`, each with a `state`), for your own login only.
*   **Web UI (`Profile > Secrets`)**: On your **Profile > Secrets** page (`/profile/secrets`), the
    table adds a **State** column (**Approved**, **Pending** with `expires <time>`, or
    **Approved, replacement pending** when an approved secret has a pending replacement) and lists
    pending secrets alongside your approved user secrets, while the **Secrets** item in the Profile
    sidebar shows a badge with your pending secret count. Each pending row shows the writing agent,
    its project, the save time, the byte length, whether it would replace your existing secret, and
    any project secrets of the same name (`overridesProjectSecrets`) it would shadow, together with
    **Approve**, **Reject**, and **Reveal** actions:
    *   **Approve** opens a confirmation dialog showing the secret's effect (`Will be delivered to your agents in all projects`) and, when the agent requested progeny access (`allowProgeny: true`), an **Also deliver to agents your agents create** checkbox (checked by default; unticking it sends `{"allowProgeny": false}` to `POST /api/v1/pending-secrets/{id}/approve`).
    *   **Reject** calls `POST /api/v1/pending-secrets/{id}/reject` and removes the pending secret.
    *   **Reveal** opens a dialog that fetches the held value from `POST /api/v1/pending-secrets/{id}/reveal` on demand (noted as recorded in the Hub audit log), decodes it in the browser (showing UTF-8 text or a `<N> bytes (binary)` notice with **Copy value**), and clears the decoded value from memory as soon as the dialog closes.
*   **Notifications**: when an agent saves a secret for your account, you get a notification
    (`SECRET_PENDING` / kind `secret_pending`) naming the agent, its project, the secret's name and
    type, and the time, never the value. In the web UI notification tray, pending-secret
    notifications include a **Review secrets** link to `/profile/secrets`. An agent's saves within
    one hour share one notification: each later save updates it with the latest secret and how many
    of that agent's secrets await your approval (and how many of those would replace an existing
    secret), and shows it again. A save that would replace one of your approved secrets is always
    announced on its own, as a replacement, at most once per agent and secret name per hour; your
    approved value stays in effect until you approve the replacement.
*   **Start hint**: when you start an agent with `scion start` and secrets await your approval,
    the CLI prints `1 secret awaits approval (scion hub secret pending list)` or
    `N secrets await approval (scion hub secret pending list)` on your terminal only, never inside
    an agent. In the web UI, starting or creating an agent while you have pending secrets appends
    `1 secret awaits approval in Profile > Secrets.` or `N secrets await approval in Profile > Secrets.`
    to the toast notice.
*   **Audit**: every save, replacement, refused save, approval, rejection, reveal and expiry of a
    pending secret is written to the Hub's structured audit log (`secret_pending_create`,
    `_replace`, `_refused`, `_approve`, `_reject`, `_reveal`, `_expire`) with the secret's name,
    type and target, the agent, the project and who acted. The value and its length are never
    logged.

Project-scoped secrets that an agent saves are stored directly.

#### Using the Hub API Directly
Under the hood, `scion secret` and `sciontool` interact with the Hub's agent-specific secrets API:

*   **`GET /api/v1/agents/{agentID}/secrets`**: Lists available secret metadata in the agent's project.
*   **`GET /api/v1/agents/{agentID}/secrets/{key}`**: Retrieves a single secret's metadata and its base64-encoded value.
*   **`PUT /api/v1/agents/{agentID}/secrets/{key}`**: Stores or updates a project-scoped secret (`201` created, `204` updated). A user-scoped write (`"scope": "user"`) is held for your approval and answers `202` with `{"key", "scope", "state": "pending", "pendingId", "expiresAt", "replacesExisting", "message"}`, or `409` with code `secret_pending_held_by_other_agent` when another agent's pending secret holds the name.
*   **`POST /api/v1/agent/secrets`**: Fetches several secret values in one call. There is no agent ID in the URL: the Hub identifies the agent from its token. The request body is `{"keys": ["KEY_A", "KEY_B"]}` (at most 100 keys). Each entry in the response has a `status` of `ok`, `not_found`, or `entitled_but_unavailable`. `sciontool init` calls this endpoint at startup to fetch the keys listed in `SCION_SECRET_KEYS`.

#### Security & Audit Logging
*   **Authentication**: API access is restricted to the running agent container. The agent must include its unique Hub-issued JWT (loaded from `SCION_HUB_TOKEN`) in the `Authorization: Bearer <token>` header of every request.
*   **Authorization**: Agents are strictly bounded to their own project's secrets. They can also access user-scoped (personal) secrets belonging to their originating user (the user who kicked off the agent chain), which are resolved on the Hub via the agent JWT's `OriginUserID` (the user who originally started the agent chain). Agents cannot access secrets in other projects, other users' secrets, or global Hub secrets unless explicitly shared via progeny policies (descendant access).
*   **Project Secret Metadata Needs `project:secret:read`**: An agent reads project secret metadata only when its token carries `project:secret:read` (the `full` [agent role](/scion/hosted/ha/permissions/#tiered-agent-authorization-roles)). Otherwise `GET /api/v1/secrets` and `GET /api/v1/secrets/{key}` with `scope=project`, and `GET /api/v1/projects/{id}/secrets` and `GET /api/v1/projects/{id}/secrets/{key}`, answer `403`, as `GET /api/v1/agents/{agentID}/secrets` already does. Project environment variable reads leave out environment-type secrets and any plain variable that a secret with the same key overrides (a single-key read of one answers `404`). Hub-scope reads and user callers are not affected.
*   **Fail-Closed Reads & Start-Time Resolution**: Both start-time secret delivery (create, start, restart, resume, and `as_needed` resolution) and every runtime secret read go through one check sequence: project permission, then an originating user who is still active and either an active member of the project (any active project role binding counts, built-in or custom, direct or through a group, except `project-visitor`) or holds system-level authority for the exact `secret.use` permission, then progeny sharing. For an agent whose delegation chain carries recorded delegation provenance, a user-scoped secret shared with progeny is also bounded by that chain at both start time and runtime: every recorded delegation on the chain must allow secret use (`secret.use`), every agent on the chain must be live and hold `project:secret:read`, and the user at the root of the chain must be the secret's owner and recorded author, active, admitted to the project and not restricted from `secret.use` by an access constraint (if a user-scoped secret is withheld at start by this check, a lower-precedence project, broker, or hub secret with the same key still applies). An agent whose chain has no recorded provenance is withheld these secrets at start and refused them at runtime (see the next item). The agent secret list applies the same rule, so it lists a user-scoped key exactly when a read of that key would return it. Membership alone does not grant reads: the project permission check (`project.secret_read`) still applies. A value is fetched only for the version recorded in the secret's metadata. If a value cannot be retrieved, that key is reported as unavailable in the response rather than returned as an empty value; other keys in the same request are unaffected. When the Hub itself cannot resolve an agent's secrets, or cannot read its stored environment settings, create, start, restart, resume and waking an agent for a direct message answer `503` (`agent secrets could not be resolved; the agent was not started`, `... not created` for a create, or the environment settings equivalent) instead of starting the agent without them: a start leaves the agent as it was, a restart (which stops the agent first) leaves it stopped, and a failed create is rolled back. A provision-only create still succeeds, and the agent's later start fails closed. A reincarnation answers `202` and then fails at its start leg; it is recorded as failed with the same message and the previous config is restored. The message names no secret or value. Having no secrets or settings is not an error.
*   **Chains Without Recorded Provenance**: An agent whose delegation chain is not recorded (no recorded delegation of its own, the backfill's migration edge, an edge without recorded provenance, more than one active edge, or such a hop anywhere above it) cannot have that chain evaluated. Its owner's user-scoped secrets shared with progeny are withheld at create, start, resume, restart and `as_needed` resolution, and runtime reads and lists of them are refused (`chain_unrecorded`). A refused read answers exactly as a read of a key that does not exist (`404`, `secret not found`), and a refused list returns no user-scoped keys. The Hub logs one rate-limited `Warn` per agent and counts each start-time withhold as `scion.hub.user_material.owner_chain` with `outcome=withheld` and `reason=chain_unrecorded`. An agent with no stored ancestry is no exception: it keeps its owner's shared secrets at start only when its chain is recorded and ends at the owner; otherwise they are withheld and counted with the chain's reason (for example `chain_unrecorded`, `terminal_user_not_owner` or `chain_lookup_error`). The Hub's check for a template's required keys before a start, and the environment gather, treat a secret withheld this way as missing, so the gather prompts for it instead of reporting it as stored. A reincarnate that leaves the chain unrecorded above the agent warns about it in its plan. To restore the agent, its owner reincarnates it from a CLI session (`scion reincarnate <agent>`, keeping its role) or re-creates it. The `runtime read` section of `hack/delivery-census` lists the affected agents with where their chain stops and the names (never values) of the refused secrets; like the rest of its output, it is operator-only.
*   **Audit Trail**: To ensure accountability, every runtime read and write operation is fully audited on the Hub. Each retrieval request logs one audit event (`agent_secret_read`) identifying the calling agent, the requested keys, and the outcome.

#### User secrets captured by an agent before 2026-09-12

A user-scoped secret shared with progeny reaches an agent with a recorded delegation chain only when the secret's author is its owner. Before 2026-09-12, a credential an agent captured into its user's scope (for example by harness credential capture) was recorded as written by the agent (`created_by` `agent:<id>`). A later capture of the same key kept that author. The Hub withheld such a secret from every agent of its owner whose delegation chain is recorded: the agent started without it, and a runtime read was refused.

A one-shot data migration at Hub start (marker `user_secret_attribution_repair`) records the owner as the author of these secrets where that is certain. An agent could only capture into the scope of the user at the root of its own ancestry, so the migration accepts:

- a capturing agent that still exists (or is soft-deleted) and whose ancestry root is the owner;
- a capturing agent with no stored ancestry whose owner or creator is the owner;
- a capturing agent that no longer exists, when the owner's user record does.

Each repaired secret gets a mutation audit record (`user_secret_attribution_repair`, actor `system`) whose before summary keeps the original `created_by`. The secret's value, version and update time are not changed, and a value written while the migration runs (for example a credential refresh) does not stop the repair. Any other secret is left as it was, and the migration logs a warning that names it and gives the reason:

| Reason | Meaning |
|---|---|
| `agent_not_linked_to_scope_user` | The capturing agent exists, but its ancestry root is another user, or it has no ancestry and belongs to another user. |
| `agent_gone_scope_user_missing` | Neither the capturing agent nor the owner's user record exists. |
| `federated_agent_ref` | The secret was captured by a federated agent (`agent:<issuer>:<id>`), which this Hub cannot check. |
| `agent_ref_invalid` | The recorded agent ID is not a valid ID. |
| `scope_owner_empty` | The secret has no owner. |
| `row_changed` | The secret's author kept changing while the migration ran. The marker is not written, so the next Hub start runs the migration again. |

Setting a skipped secret again does not change its author, because an update keeps `created_by`. To deliver it, clear it and set it again as its owner (`scion hub secret clear`, then `scion hub secret set`). Skips other than `row_changed` do not block the marker. A store error leaves the marker unwritten, so the next start runs the migration again.

A repaired secret that is shared with progeny becomes readable at runtime (`scion secret get` inside an agent) by the owner's agents with a recorded delegation chain, in every project, as a secret written after 2026-09-12 already is. Before the repair, these secrets were never readable at runtime.

Whenever authorization withholds a user-scoped secret at start or restart, or refuses a runtime read of one, the Hub logs a warning that names the agent, the secret and the cause, never the value: `start-time user secret withheld by authorization` and `runtime user secret read withheld by authorization`. The warning is logged at most once every 10 minutes for the same agent, secret and cause, and the next one reports how many were suppressed.

---

## Agent-Scoped Secrets

:::note[Experimental]
Agent-scoped secrets require the `hub.agent_secrets` [experiment](/scion/reference/experiments/), which is off by default. While it is off, the Hub rejects the `agent` scope with `400 invalid scope: agent` and delivers no agent-scoped secrets.
:::

When `hub.agent_secrets` is enabled, Scion supports **agent-scoped secrets** (`scope=agent`, keyed by the agent's stable UUID): secrets bound to a single agent and readable only by that agent's token.

- **Access boundary:** No API returns an agent-scope value to anyone but the bound agent's token. For humans, agent-scoped secrets are write-only — there is no reveal action (unlike [pending user secrets](#approving-user-secrets-saved-by-your-agents)).
- **Descriptions are metadata:** Secret descriptions (including those supplied on secret requests) are visible to anyone holding `agent.read` on the agent. Never put secret values in a description.
- **Secrets only:** Agent scope applies to secrets only, not plain environment variables.
- **Precedence:** Agent scope sits at the top of the secret precedence ladder (`runtime_broker < hub < project < user < agent`). Non-empty environment values in the agent-create request still outrank every scope, and plain variables from any scope still beat environment-type secrets.
- **Types, targets, and injection modes:** Supports `environment` (default), `file`, and `variable` types, custom targets, and `as_needed` (default) or `always` injection modes. `file` and `variable` secrets are placed regardless of injection mode.
- **No progeny propagation:** Agent-scoped secrets never propagate to child agents; setting `allowProgeny` at agent scope is rejected with `400`.
- **Reserved names:** `SCION_*` and `GCE_METADATA_*` are rejected for `environment`-type secrets, as at every other scope.
- **Persistence and deletion:** Because agent-scoped secrets are keyed by the agent's stable UUID, they persist across stop, suspend, restart, reincarnation (including `--broker` moves), and soft delete. When the agent is **hard-deleted** or purged by retention, its local-backend secret rows are deleted in the hard-delete transaction and any GCP Secret Manager entries are deleted best-effort post-commit.
- **GCP Secret Manager naming and labels:** Agent-scoped secrets use the hub-prefixed scheme `scion-<h12>-agent-<sha256(hubID:agentID)[:12]>-<KEY>` (covered by the existing hub-prefix conditioned IAM grant) and carry the labels `scion-scope=agent`, `scion-scope-id=<agentID>`, and `scion-project-id=<projectID>` for project attribution.

### Managing Agent Secrets (Humans)

Listing or reading metadata for an agent's secrets requires `agent.read` on the agent; creating, updating, or deleting requires `agent.update` (accepted from both interactive sessions and user access tokens carrying `agent:update`).

- **CLI (`scion hub secret ... --agent <name|id>`):**
  ```bash
  # Set from stdin (recommended — avoids shell history and process listings)
  printf %s "$TOKEN" | scion hub secret set --agent my-agent GITHUB_TOKEN -

  # Set a file secret from a local file
  scion hub secret set --agent my-agent --type file --target ~/.ssh/id_ed25519 DEPLOY_KEY @~/.ssh/deploy_key

  # List, inspect metadata, update metadata, or delete
  scion hub secret list --agent my-agent
  scion hub secret get --agent my-agent GITHUB_TOKEN
  scion hub secret update --agent my-agent GITHUB_TOKEN --injection-mode always
  scion hub secret delete --agent my-agent GITHUB_TOKEN   # alias for clear
  ```
  When passing `-` to `scion hub secret set`, stdin is read up to 64 KiB; trailing CR/LF characters are trimmed for `environment` and `variable` types, while `file` secrets preserve stdin bytes verbatim.
- **Web UI:** The agent detail and edit pages include a **Secrets** tab for adding, editing metadata, and deleting agent-scoped secrets (no reveal).

### Create-Time Secrets

You can bind secrets to an agent when creating or starting it:

- **CLI (`--secrets-file <path>` on `scion create` and `scion start`):** Reads a YAML or JSON file client-side (at most **50 secrets**, up to **64 KiB** per value) and sends the values in `CreateAgentRequest.Secrets`. There is deliberately no `--secret KEY=VALUE` flag so values are never exposed in shell history or process listings. On Unix, the CLI warns to stderr if the file is group- or world-readable.
  ```yaml
  # Map shorthand (environment, as_needed)
  GITHUB_TOKEN: ghp_example
  ```
  ```yaml
  # Full form
  secrets:
    - key: DEPLOY_KEY
      valueFile: ~/.ssh/deploy_key     # or value: | or fromEnv: VAR
      type: file
      target: ~/.ssh/id_ed25519
      description: Release deploy key
    - key: OPENAI_API_KEY
      fromEnv: OPENAI_API_KEY
      injectionMode: always
  ```
  - **Fail-closed creation (`503`):** Secrets are written to the secret backend after the agent record commits and before dispatch. If any write fails, already-written secrets are best-effort deleted, the new agent is hard-deleted, and the Hub returns `503` (`agent secrets could not be stored; the agent was not created`).
  - **Existing agents:** Running `scion start <existing-agent> --secrets-file <path>` upserts the secrets at agent scope (requiring `agent.update`) before starting the agent.
  - **Older Hubs:** A Hub that predates agent-scoped secrets silently ignores the `secrets` field on create requests.
- **Web UI:** The agent create form's **Additional options** includes a **Secrets** tab (create mode only). Values go only into the create request body.

### Web Form Masking and File Loading

Both the create-form **Secrets** tab and the secret-request form use the same masked secret input:

- **Masked by default:** Values are entered through password inputs until you click **Show**. Multi-line values and exact line endings (including CRLF and leading UTF-8 BOM) are preserved verbatim in memory while masked.
- **Revealed editing and mixed line endings:** Clicking **Show** reveals a `<textarea>`. All-LF and all-CRLF values remain editable while preserving their line endings; values with mixed line endings or bare CR render read-only when revealed with the note *"This value has mixed line endings; Hide, then paste or load to replace it."* so bytes are never silently altered.
- **Load from file:** File-typed secret inputs offer **Load from file** (UTF-8 text only, up to **64 KiB**; binary files and files containing NUL bytes are rejected).
- **No browser persistence:** Values are never stored in drafts, `localStorage`, `sessionStorage`, or logs, and are cleared immediately on submit, navigation, or `pagehide`.

:::note[Operator check: browser password managers]
Before enabling `hub.agent_secrets` broadly, verify how your organization's browser password managers interact with the masked password inputs on the create form and secret-request cards (for example autofill or save-password prompts).
:::

### Agent Self-Service

Inside a running agent container (with `hub.agent_secrets` enabled):

- **List secrets (`scion secret list`):** Defaults to `--scope all` and prints a `SCOPE` column showing both `agent` and `project` secrets. Filter with `--scope agent` or `--scope project`.
- **Read a secret (`scion secret get`):** Checks `agent` scope first, then `project` scope. Pin with `--scope agent` or `--scope project`. Use `--value` to write the raw bytes to stdout inline:
  ```bash
  curl -H "Authorization: Bearer $(scion secret get --value GITHUB_TOKEN)" https://api.github.com/user
  ```
  Reading an agent's own `agent`-scope secrets does not require `project:secret:read` (so agents created from user access tokens can still read their own agent-scoped secrets).
- **Write and delete (`sciontool secret`):** Lead with stdin (`-`) rather than passing values as command-line arguments:
  ```bash
  printf '%s' "$TOKEN" | sciontool secret set MY_AGENT_TOKEN - --scope agent
  sciontool secret delete MY_AGENT_TOKEN --scope agent
  ```
  When passing `-` to `sciontool secret set`, stdin is read up to 64 KiB; a single trailing `\r\n` or `\n` is trimmed for `environment` and `variable` types, while `file` secrets preserve stdin bytes verbatim. Agent-scope writes are stored immediately (no pending approval step, and not blocked by `agent_secrets.user_scope_only`).
- **File target restriction:** Any `--scope agent` file secret written by an agent (or requested via a file-type secret request) must target a path under the agent home (`~/...`) and must not contain `..`.

### Secret Requests (Agent Asks, Human Supplies)

When an agent needs a credential it does not yet have, it can send a **secret request** (`secret-request` message) instead of asking a human to paste the value into chat.

#### Sending a secret request (agent mode only)

```bash
# Single key
scion message conv:<id> --secret-request GITHUB_TOKEN \
  --secret-description "GitHub PAT with repo scope for org foo" - <<'EOF'
I need a token to push the release branch.
EOF

# Multiple keys or typed secrets (JSON array, 1–10 entries, max 32 KiB)
scion message @alice@example.com --secret-request-file req.json
```

Each entry in `--secret-request-file` (pass `-` for stdin) is a JSON object with `key` (required, `[A-Za-z_][A-Za-z0-9_]{0,127}`), and optional `description` (up to 1,000 characters), `type` (`environment` default, `file`, or `variable`), and `target` (required for `file`, must be under `~/` without `..`).

- **Destinations:** Users or conversations only (`user:<email>`, `@<email>`, `conv:<id>`, `#<thread>`). Agents, `group[...]`, and broadcasts are refused. Cannot be combined with `--decisions`/`--decisions-file`, `--attach`, `--artifact`, or `--wake`.
- **Versioning and `--update`:** Keys compare case-insensitively, and an agent can have at most one `pending` version per key. Sending a key that is already `pending` without `--update` is refused with `409 secret_request_key_pending` (listing the version, card message ID, and a hint to pass `--update` or `scion secret request withdraw KEY`). Passing `--update` marks the pending version `replaced` and creates `v(n+1)` (if a fulfill is currently in flight, the send is refused with `409 secret_request_submitting`). Sending a key whose latest version is terminal (`fulfilled`, `withdrawn`, `dismissed`, or `expired`) creates the next version without `--update` — this is the normal way to ask for a secret rotation.

#### Inspecting and withdrawing requests (agent mode only)

```bash
scion secret request list [--pending | --fulfilled | --withdrawn | --dismissed | --expired | --all] [--json]
scion secret request withdraw GITHUB_TOKEN --reason "no longer needed"
scion secret request withdraw GITHUB_TOKEN --set --reason "switched approach"
```

Withdrawal is all-or-nothing over the named keys' `pending` versions. `--set` also withdraws every other pending key on the same card. Repeating `withdraw` on an already-withdrawn key is an idempotent no-op (`already withdrawn`).

#### Lifecycle states and expiry

| State | Meaning |
| :--- | :--- |
| `pending` | Waiting for a human responder. |
| `fulfilling` *(internal)* | Short-lived claim while the Hub writes to the secret backend; API views report `status: "pending"` with `submitting: true`. Stale claims older than **2 minutes** are automatically recovered back to `pending`. |
| `fulfilled` | Stored at agent scope (`as_needed`); the agent receives a `secret-response` message (with auto-wake) and can read the value immediately with `scion secret get --value KEY` (start-time injection applies from the next start). |
| `replaced` | Superseded by a newer version sent with `--update`; the old card links to the newer version and cannot be submitted (`409 replaced`). |
| `withdrawn` | Retired by the agent (`withdrawn_by: "agent"`) or by the Hub when the agent is hard-deleted (`withdrawn_by: "hub"`, reason `"agent deleted"`). |
| `dismissed` | Declined by an eligible responder (whole card or selected keys, optional reason up to 500 characters, no minimum age); the agent receives a `secret-response` message. |
| `expired` | Unanswered for **7 days**; swept automatically with no message sent to the agent. |

Secret request rows never store secret values and are retained as a request log after agent deletion (deleted when the project is deleted).

#### Responding (humans)

- **Authorization:** Only a signed-in human user on an **interactive web session** who is a **recipient** of the request (the DM target, or a participant of the conversation/thread) and holds **`agent.attach`** on the asking agent can fulfill or dismiss a request. User access tokens, visitors, and agents are refused (`403`). Viewing a card (`GET /api/v1/secret-requests/{messageId}`) requires being a recipient and holding `agent.read`; viewers who cannot respond see the form disabled with one of the following reasons:
  - *"you are not an eligible responder for this secret request"*
  - *"you need agent.attach permission on this agent to supply or dismiss secrets"*
  - *"all secret requests on this card are closed"*
- **Surfaces:**
  - **Native web chat:** Renders an interactive card with masked inputs, **Show/Hide**, **Load from file** (for `file` type), live state updates (`chat.secret_request.updated`), **Submit** (submits all currently pending keys on the card together), and **Dismiss** (whole card or selected keys, with optional reason).
  - **Standalone page (`/secret-requests/<messageId>`):** Renders the same form outside chat behind sign-in. A link to a replaced card displays a replacement banner linking to the current version.
  - **Non-native surfaces** (Slack, Telegram, Discord, Google Chat): Receive a Markdown fallback listing the agent, project, keys, versions, descriptions, a reminder never to paste values into chat, and the standalone link `<hub-public-url>/secret-requests/<messageId>` (the link is omitted if the Hub has no public endpoint configured).
- **Partial-failure rollback on multi-key fulfill:** If a backend write fails (`503`) partway through fulfilling a multi-key card, rollback deletes **only keys newly created in that call** (preserving any pre-existing keys during rotation) and returns the rows to `pending` so the responder can retry.

### Experiment Gate (`hub.agent_secrets`)

`hub.agent_secrets` is a server- and web-layer experiment (alpha, off by default). While it is off:
- `scope=agent` is rejected (`400 invalid scope`) on secret CRUD and agent secret routes, and agent-scope secrets are skipped by launch delivery.
- `CreateAgentRequest.Secrets` and outbound messages carrying `secret_requests` are refused with `400 experiment_disabled`.
- `/api/v1/secret-requests/{messageId}` (plus `/fulfill` and `/dismiss`) and `/api/v1/agent/secret-requests` (plus `/withdraw`) answer `404`.
- The web UI hides the agent detail/edit **Secrets** tab, the create-form **Secrets** tab, and native chat secret-request cards.

See [Experiments](/scion/reference/experiments/#server-experiments-with-notes) for how admins toggle it.

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

**Normalization rules:** uppercase the name, replace hyphens (`-`) and dots (`.`) with underscores (`_`). The double underscore (`__`) separates owner from repo. *Note: Because of this normalization, names that differ only by hyphens, dots, or underscores (e.g., `acme-corp` and `acme_corp`) will resolve to the same secret name.* A trailing `.git` on the repository name is dropped, so `acme-corp/templates.git` uses `GH_ACME_CORP__TEMPLATES`.

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

### Template and Harness-Config Imports

The same secret names authenticate remote imports into a project. This covers importing templates and harness-configs from a GitHub URL with project scope, and scanning a GitHub directory for skills. For a `https://github.com/{owner}/{repo}/...` folder URL, the Hub checks credentials in this order:

| Priority | Source | Description |
| :--- | :--- | :--- |
| 1 | `GH_{OWNER}__{REPO}` | Repo-specific convention secret |
| 2 | `GH_{OWNER}` | Owner-level convention secret |
| 3 | GitHub App installation token | Only if the Hub has the GitHub App configured and the project has an installation; if minting the token fails, the next option is tried |
| 4 | `GITHUB_TOKEN` | Project secret |
| 5 | Unauthenticated | No credential found; works for public repos only |

For example, with a `GH_ACME_CORP` project secret, a project-scope import from `https://github.com/acme-corp/templates/tree/main/.scion/templates` uses that credential.

Other scopes use the same names, read from their own credentials only:

| Import | Where the Hub or CLI looks, in order |
| :--- | :--- |
| Global scope (Hub-level) | Hub-scope `GH_{OWNER}__{REPO}`, `GH_{OWNER}`, then `GITHUB_TOKEN`. There is no GitHub App step: App installations belong to projects. |
| User scope | Your own user-scope `GH_{OWNER}__{REPO}`, `GH_{OWNER}`, then `GITHUB_TOKEN`. An import uses them only when you are the user it imports for, never for another user. |
| `scion templates import-url` | Runs on the Hub (`POST /api/v1/resources/import`) and uses the target scope's Hub secrets in the same order above (project, global, or user). No GitHub token is sent from the CLI. |
| `scion harness-config install` and `scion templates import` | The `GH_{OWNER}__{REPO}`, `GH_{OWNER}`, then `GITHUB_TOKEN` environment variables of the shell running the CLI. The CLI fetches the source itself. |

In every scope the first match wins, and with no match the fetch is unauthenticated, which works for public repos only. A global or user-scope import never reads project credentials.

Reimporting an existing template (`POST /api/v1/templates/{id}/reimport`) uses the same lookup order for the template's own scope: a project template's project credentials and App token, a global template's Hub credentials, and a user template's credentials only when you are that user. With a credential, the Hub requests the archive from `api.github.com`. When an import runs `git` instead (`git ls-remote` to resolve a branch name that contains `/`, or a sparse checkout when the archive download fails), the credential reaches `git` only through a credential helper scoped to `https://github.com`, with terminal prompts off (and `GIT_LFS_SKIP_SMUDGE=1` for the checkout); it is never put in the repository URL. `git` offers no credential of the Hub host's own (its git config, `~/.netrc`, ssh or Kerberos credentials), so without a credential it runs anonymously. The CLI's own imports work the same way.

Only GitHub folder URLs use a credential. Archive URLs (`.tar.gz`, `.tgz`, `.zip`, including `github.com/.../archive/...` links), rclone sources and other hosts are always fetched without one.

On a hosted Hub, archive URLs that redirect (for example `github.com/.../archive/...` links and release assets) are refused; give the final URL.

### Injection Mode Behavior

Convention-keyed GitHub secrets support the standard injection modes:

- **As Needed** (recommended): The credential is used at provision time to fetch skills and templates but is **not** exposed inside the agent container. This is the minimum-privilege posture. When the Hub dispatches an agent to a Runtime Broker, `provisionCredentials` includes only the `as_needed` environment-type secrets referenced by `?token=SECRET_NAME` on a `gh://` skill URI of the agent or matching a `GH_{OWNER}` / `GH_{OWNER}__{REPO}` convention name for one of the agent's `gh://` skill URIs, plus `GITHUB_TOKEN` (from the resolved environment or an `as_needed` secret) when any `gh://` skill has no explicit or convention token. Unreferenced `as_needed` secrets, `always`-injected secrets (already in the agent environment), and `file` or `variable` secrets are not sent in `provisionCredentials`.

  ```bash
  scion hub secret set --project GH_ACME_CORP__PRIVATE_SKILLS github_pat_...
  ```

- **Always**: The credential is also injected into the agent container as an environment variable (`GH_ACME_CORP__PRIVATE_SKILLS`). Use this when agents need runtime access to the same private repo.

  ```bash
  scion hub secret set --project GH_ACME_CORP__PRIVATE_SKILLS github_pat_...
  scion hub secret update --project GH_ACME_CORP__PRIVATE_SKILLS --injection-mode always
  ```

For more on injection modes, see [Injection Modes](#injection-modes) above.

:::note[Implementation Reference]
This feature was introduced in [`GoogleCloudPlatform/scion` PR #919](https://github.com/GoogleCloudPlatform/scion/pull/919). For the full auth layer reference (PAT, GitHub App bot, `gh` CLI token, runtime fallbacks), see the `github-auth-fallback` agent skill.
:::

---

## Administrator Configuration (Hub)

To use secrets in production, the Hub must be configured with a production-grade secrets backend.

### Secrets Backend

Scion uses a secrets backend to store secret values securely. The recommended backend for production is **GCP Secret Manager**, while the default `local` backend stores values directly in the Hub database using AES-256-GCM encryption at rest.

:::note[Encryption at Rest & Startup Audit]
Secret values stored in the `local` backend and Hub-stored infrastructure backups in the database (signing keys for agent/user tokens, skill downloads, artifact views, attachment delivery, and list cursors; the OIDC signing key and `oidc_signing_keyset`; and the Conduit grant key ring) are encrypted with the Hub's versioned value cipher (`enc:v1:` or `enc:v2:<kid>:`). Legacy `enc:v1:` values remain readable while their key is in the legacy v1 read set; in legacy mode without a keyring file, legacy plaintext rows can still be read and are rewritten as `enc:v1:` on the next write, whereas managed keyring mode (`server.keyring_file`) rejects unencrypted rows on runtime reads and admits them only during an explicit encryption migration.

When a database encryption policy row is present, the Hub checks at startup that its local cipher satisfies the policy (running with `server.keyring_file`, writing `enc:v2:` under an allowed writer key ID, and holding every required reader key ID) and refuses to start on any mismatch. Before synchronizing or writing any backup keys, a startup encryption audit verifies all locally stored secret payloads and the decrypted structure of Hub-scoped infrastructure backups, classifying readable payloads in `audit.by_format` and `audit.by_kid` by the key that actually authenticated them rather than the envelope label alone. Unreadable records are preserved and never overwritten: in keyring mode, an unreadable infrastructure backup keeps the Hub not ready (`/readyz` returns `503` with `reason: "backup_unreadable"`), while in legacy mode the Hub logs an `UNHEALTHY` error naming the affected record IDs and continues serving. Before retiring legacy keys or promoting a `v2` encryption policy, run [`scion server keys verify`](/scion/reference/cli/#scion-server) on the Hub host to confirm that every locally stored secret and infrastructure backup authenticates in `enc:v2:` under an allowed writer key ID.
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

To move existing database-stored secrets to GCP Secret Manager on an upgraded Hub, run the `secret-hub-id-migration` maintenance migration from **Admin → Maintenance** (or `POST /api/v1/admin/maintenance/migrations/secret-hub-id-migration/run`; see [Hub server: Migrations](/scion/hosted/single-node/hub-server/#migrations)). It decodes each secret, writes the plaintext value to Secret Manager under its hub-prefixed name, reads it back to verify it, keeps the local database values until every secret is verified, repairs values that an earlier run copied in encoded form, and reopens once automatically if it was marked completed under an earlier release. The `scion hub secret migrate` CLI command is disabled and points to this maintenance migration.

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

On a Cloud Run hub deployed with the hub-cloudrun Terraform module (private-IP Cloud SQL, Direct VPC egress, an explicit `hub_id`), run this command via the one-off Cloud Run job in [`docs/deploy/migrate-names-cloudrun.md`](https://github.com/GoogleCloudPlatform/scion/blob/main/docs/deploy/migrate-names-cloudrun.md); no workstation ever runs it directly against that hub's database. Hubs deployed with the manual [Deploy on GCP](/scion/hosted/ha/setup-gcp/) guide (public-IP Cloud SQL) use the same kind of job, described in §9 of that runbook; see that guide's [Secret Name Migration](/scion/hosted/ha/setup-gcp/#7b-secret-name-migration) section.

---

## Technical Details

### Automatic Plugin Secret Migration & Safety (Hub Integrations)

For external messaging integrations (such as Discord, Telegram, or Google Chat) that run as Hub-level message broker plugins, Scion implements an automatic, secure secret migration and stripping pipeline to keep sensitive bot tokens and API keys out of plaintext configuration files:

- **Automatic One-Shot Migration**: When starting up or activating a plugin (e.g., via the runtime activation path in `activateInstalledIntegration`), the Hub automatically scans the integration's configuration—including per-plugin external configuration files and inline config blocks. Any discovered secret keys are automatically migrated into the configured secure **Secrets Backend** (such as GCP Secret Manager).
- **Copy-Before-Strip Safety**: After migrating the secrets, Scion strips the raw values in-place from the integration's file-based and inline configurations (`stripSecretKeysInPlace`) using a secure copy-before-strip helper to avoid any risk of partial writes or configuration corruption, while deduplicating warning logs.
- **Boot and Activation Consistency**: This migration-and-strip sequence runs consistently during both the Hub's boot-up routine and dynamic runtime integration activation, ensuring that secrets are never stored or exposed in plaintext configs.
- **Redacted Wiring & Database Settings**: Integration detail responses (`GET /api/v1/admin/integrations/{name}`) omit secret keys, internal runtime keys (`bot_id`, `config_file`), and Hub wiring and database keys (`hub_url`, `broker_id`, `hmac_key`, `plugin_name`, `project_slug_map`, `database_driver`, `database_url`) from `settings`. When configuring or restarting a plugin, live Hub wiring and database values always override any cached values for those keys.

### Resolution Hierarchy
When an agent starts, the Runtime Broker requests a "Resolved Environment" from the Hub and combines it with the template `env` block. For the same key, a later entry in this list wins over an earlier one:
1. Template `env` block (a fallback: applied only to keys that no other source sets)
2. Broker Secrets
3. Hub Secrets (global defaults)
4. Project Secrets
5. User Secrets
6. Agent Secrets *(secrets only, when `hub.agent_secrets` is enabled)*
7. Environment values in the agent-create request (API, web form or `--config`), when non-empty

The scope order (items 2 to 6 for secrets; items 2 to 5 for variables) applies within variables and within secrets. For the same key, a variable from any scope takes precedence over an environment-type secret.

### Security
Secrets are transmitted over TLS between the Hub and Runtime Brokers. They are only decrypted by the Hub during the dispatch process and sent over an encrypted channel to the Runtime Broker. The Broker then injects them directly into the container's memory space. Brokers never persist agent secrets to disk.

For a detailed overview of the security architecture, see the [Security Architecture Reference](/scion/reference/security/).
