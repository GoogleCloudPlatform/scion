# Secrets Gather: Required Secrets in Templates and CLI Provisioning

**Status:** Accepted
**Date:** 2026-02-22

## 1. Overview

This document addresses three related gaps in the secrets provisioning flow:

1. **Auto-upgrade of required env vars to secrets**: When a required environment variable (declared with an empty value in settings/harness config) has a matching key in the secrets store, the system should automatically provision it without user input — transparently "upgrading" the env var to its secret-backed value.

2. **Template-level `secrets` section**: Templates and settings should support a `secrets` section that declares required secret keys. During the CLI gather step, when running interactively, the user can enter secret values directly at the prompt (e.g., typing an API key) rather than needing to pre-configure them via `scion hub secret set`.

3. **Kubernetes native secret mounting**: The Kubernetes runtime should use Kubernetes Secret objects (or GCP Secret Manager via CSI driver) to mount secrets into pods, rather than passing plaintext values through the dispatch chain.

### 1.1 Relationship to Existing Designs

- **`secrets.md`**: Defines the secret model, storage backends, and runtime projection (Phases 1-3 implemented, Phase 4 — native K8s/Cloud Run — deferred). This document extends Phase 4 and adds the gather/auto-upgrade concepts.
- **`env-gather.md`**: Defines the CLI fallback flow for missing env vars. This document extends that flow to include secrets awareness.
- **`kubernetes/milestones.md`**: Milestone 5 calls for replacing env var injection with proper K8s Secrets. This document specifies how.

---

## 2. Current State

### 2.1 How required env vars work today

A key is **required** if:
- A harness declares it via `RequiredEnvKeys()` (e.g., Claude → `ANTHROPIC_API_KEY`)
- A settings profile or harness config entry declares it with an empty value (`SOME_KEY: ""`)

During the env-gather flow (hosted mode):
1. Hub resolves env vars from its scoped storage (user, grove)
2. Broker merges Hub env + local env + config overrides
3. If required keys remain unsatisfied → Broker returns HTTP 202 → CLI gathers from `os.Getenv()` → user confirms → submitted back

### 2.2 How secrets work today

Secrets are a separate system from env vars:
- Stored via `scion hub secret set` (encrypted in Hub DB or GCP Secret Manager)
- Three types: `environment`, `variable`, `file`
- Resolved at dispatch time via `SecretBackend.Resolve()` with scope hierarchy (user < grove < runtime_broker)
- Passed to Broker as `ResolvedSecrets` in `CreateAgentRequest`
- Projected by runtime: Docker/Apple use `-e` flags and bind mounts; K8s currently only handles env vars via direct `EnvVar` entries in pod spec

### 2.3 The gap

These two systems don't talk to each other during the gather flow:

- The Broker's `extractRequiredEnvKeys()` identifies keys like `ANTHROPIC_API_KEY` as "needed" even when a secret with matching key exists in the Hub's secret store. The secret is already in `ResolvedSecrets` and will be projected as an env var — but the gather logic doesn't know that.
- There is no way to declare "this template needs secret X" — only env vars can be declared as required.
- The CLI gather step can only source values from `os.Getenv()`. There is no option for the user to type in a secret value interactively.
- The K8s runtime injects secrets as plaintext env vars in the pod spec, which is visible via `kubectl describe pod`.

---

## 3. Auto-Upgrade: Required Env Vars Backed by Secrets

### 3.1 Concept

When the env-gather flow evaluates whether a required key is satisfied, it should check not only the merged env map but also the `ResolvedSecrets` list. If a required env key (e.g., `ANTHROPIC_API_KEY`) has a matching secret with `type=environment` and `target=ANTHROPIC_API_KEY`, that key is **satisfied by the secret store** — no user input needed.

This is an "auto-upgrade" because the user doesn't need to do anything different. They set a secret once via `scion hub secret set ANTHROPIC_API_KEY sk-...`, and from then on, every agent that requires `ANTHROPIC_API_KEY` gets it automatically. The value never appears in the env-gather prompt.

### 3.2 Implementation: Broker-Side

The change is in `extractRequiredEnvKeys()` and the env completeness check in `pkg/runtimebroker/handlers.go`.

Currently, the completeness check at line ~344 iterates required keys and checks `env[key]`. It should additionally check `req.ResolvedSecrets` for any secret with `Type == "environment"` whose `Target` matches the key:

```go
// Build a set of env-type secret targets for quick lookup
secretTargets := make(map[string]struct{})
for _, s := range req.ResolvedSecrets {
    if s.Type == "environment" {
        secretTargets[s.Target] = struct{}{}
    }
}

for _, key := range required {
    val, hasVal := env[key]
    if hasVal && val != "" {
        // Already in merged env (from hub env, config, etc.)
        if _, fromHub := req.ResolvedEnv[key]; fromHub {
            hubHas = append(hubHas, key)
        } else {
            brokerHas = append(brokerHas, key)
        }
    } else if _, fromSecret := secretTargets[key]; fromSecret {
        // Satisfied by a resolved secret — auto-upgraded
        hubHas = append(hubHas, key)
    } else {
        // Check broker local env...
    }
}
```

### 3.3 Reporting

The `EnvRequirementsResponse` should indicate which keys are satisfied by secrets vs. plain env vars. Two options:

**Option A: Add `secretHas` field** — A new field alongside `hubHas` and `brokerHas`:
```go
type EnvRequirementsResponse struct {
    // ...existing fields...
    SecretHas []string `json:"secretHas,omitempty"` // Keys satisfied by secrets
}
```

**Option B: Annotate `hubHas` with source** — Extend the `EnvSource` type used in the Hub's enriched response to include a `"secret"` scope value. The CLI already displays scope info; adding `"secret"` is a natural extension.

**Recommendation:** Option B. The CLI already handles `EnvSource` with scope annotations. Adding `"secret"` as a scope value provides visibility without a new field. The Hub's `buildEnvGatherResponse()` can annotate keys satisfied by secrets with `scope: "secret"`.

### 3.4 Edge case: env var AND secret with same key

If both `ResolvedEnv["ANTHROPIC_API_KEY"]` and a secret with `target=ANTHROPIC_API_KEY` exist, the secret takes precedence (it was set with `--secret` or `scion hub secret set`, implying the user wants the encrypted version). The existing runtime projection already handles this: `environment`-type secrets are injected as `-e` flags, which override `ResolvedEnv` values at container start.

No special handling needed — just report it as satisfied.

---

## 4. Template-Level `secrets` Section

### 4.1 Concept

Templates and settings profiles should support declaring required secrets. This serves two purposes:

1. **Visibility**: Template authors can document what secrets an agent needs, making requirements explicit and version-controlled.
2. **Interactive gathering**: During the CLI gather step, declared secrets that aren't pre-configured can be collected interactively — the user types the value directly.

### 4.2 Schema

#### In `scion-agent.yaml` (template config):

```yaml
schema_version: "1"
description: "Custom agent with third-party API access"
agent_instructions: agents.md
system_prompt: system-prompt.md

secrets:
  - key: THIRD_PARTY_API_KEY
    description: "API key for the third-party service"
  - key: DATABASE_PASSWORD
    description: "Password for the analytics database"
    type: environment          # default
  - key: GCP_CREDENTIALS
    description: "GCP service account credentials"
    type: file
    target: /home/scion/.config/gcloud/credentials.json
```

#### In `settings.yaml` (harness config or profile):

```yaml
harness_configs:
  claude:
    harness: claude
    secrets:
      - key: ANTHROPIC_API_KEY
        description: "Anthropic API key"

profiles:
  production:
    runtime: kubernetes
    secrets:
      - key: DATADOG_API_KEY
        description: "Datadog monitoring key"
```

### 4.3 Go Types

```go
// RequiredSecret declares a secret that must be present for the agent to function.
// Used in template configs and settings profiles.
type RequiredSecret struct {
    Key         string `json:"key" yaml:"key"`
    Description string `json:"description,omitempty" yaml:"description,omitempty"`
    Type        string `json:"type,omitempty" yaml:"type,omitempty"`       // "environment" (default), "variable", "file"
    Target      string `json:"target,omitempty" yaml:"target,omitempty"`   // Projection target (defaults to Key for env type)
}
```

This type would be added to:
- Template config parsing (wherever `scion-agent.yaml` is loaded)
- `HarnessConfigEntry` in `pkg/config/settings_v1.go`
- `V1ProfileConfig` in `pkg/config/settings_v1.go`

### 4.4 Resolution: How declared secrets interact with the gather flow

During env-gather, the Broker's `extractRequiredEnvKeys()` currently handles two sources of required keys: harness intrinsics and settings empty-value env entries. A third source is added:

**Phase 3 (template/settings secrets):** Extract keys from `secrets` sections in the resolved harness config, active profile, and template config. These keys are added to the required set.

The completeness check then works as before: for each required key, check merged env → check `ResolvedSecrets` → check broker local env → mark as "needs".

The key difference from env vars: a secret marked as "needs" can be gathered interactively (see §5) rather than only from `os.Getenv()`.

### 4.5 Merge semantics

Secret declarations from multiple sources are merged by key name (union). If the same key appears in both template and settings, the most specific declaration wins (settings profile overrides template). The `description`, `type`, and `target` from the winning source are used.

---

## 5. CLI Interactive Secret Gathering

### 5.1 Concept

When the CLI gather step identifies "needed" keys, and some of those keys are declared as secrets (either via the `secrets` section or because they match a harness's `RequiredEnvKeys()`), the CLI should offer the user the option to enter values directly.

This is specifically for interactive mode. Non-interactive mode (`--non-interactive`) continues to fail immediately if keys are missing.

### 5.2 UX Flow

```
Environment variables for agent 'researcher':
  Hub provides:    GITHUB_TOKEN (secret)
  Broker provides: (none)
  Found locally:   (none)

  Missing secrets:
    ANTHROPIC_API_KEY - Anthropic API key (required by harness)

  You can enter missing secret values now, or set them permanently:
    scion hub secret set ANTHROPIC_API_KEY <value>

  Enter value for ANTHROPIC_API_KEY (input hidden): ********

  1 secret gathered interactively.
  Continue? [Y/n]: y

Starting agent 'researcher'...
```

### 5.3 Implementation

In `gatherAndSubmitEnv()` (`cmd/common.go`), after checking `os.Getenv()` for needed keys:

1. Separate needed keys into two categories:
   - **env-only keys**: Keys that are only declared as env vars (no matching secret declaration). These must come from `os.Getenv()` or fail.
   - **secret-eligible keys**: Keys that are declared in a `secrets` section OR match a harness `RequiredEnvKeys()` entry. These can be entered interactively.

2. For secret-eligible keys still missing after `os.Getenv()`:
   - If interactive: prompt user with hidden input (using `term.ReadPassword()` or similar)
   - If non-interactive: error as today

3. Interactively gathered values are submitted as part of the `SubmitEnvRequest.Env` map. The Broker treats them the same as any other gathered env value.

### 5.4 Persistence of interactively gathered secrets

Interactively gathered secret values are **ephemeral** — used only for the current agent start. They are NOT automatically stored in the Hub's secret store. This is deliberate:

- The user may not want the value persisted
- The Hub may require a specific scope for the secret
- The value may be a temporary/one-time credential

The CLI should suggest permanent storage after a successful gather:

```
Tip: To avoid entering this value each time, store it permanently:
  scion hub secret set ANTHROPIC_API_KEY <value>
```

### 5.5 Conveying secret metadata to CLI

The `EnvGatherResponse` needs to convey which "needed" keys are secret-eligible, along with their descriptions, so the CLI can render appropriate prompts. Extend the response:

```go
type EnvGatherResponse struct {
    // ...existing fields...

    // SecretInfo provides metadata about needed keys that are declared secrets.
    // Keyed by secret key name. Only populated for keys in Needs.
    SecretInfo map[string]SecretKeyInfo `json:"secretInfo,omitempty"`
}

type SecretKeyInfo struct {
    Description string `json:"description,omitempty"` // Human-readable description
    Source      string `json:"source"`                // "harness", "template", "settings"
}
```

The Broker populates `SecretInfo` for each key in `Needs` that originated from a `secrets` declaration or a harness `RequiredEnvKeys()` call. The Hub relays this to the CLI.

---

## 6. Kubernetes Runtime: Native Secret Mounting

### 6.1 Current State

The K8s runtime (`pkg/runtime/k8s_runtime.go`) currently handles secrets as follows:

- **Environment secrets**: Injected as literal `corev1.EnvVar` entries in the pod spec (lines 224-246). Values are plaintext and visible via `kubectl describe pod`.
- **Auth injection**: Hardcoded injection of `GEMINI_API_KEY`, `ANTHROPIC_API_KEY`, `GOOGLE_API_KEY` from `config.Auth` fields (lines 234-242). This is documented as a "Temporary M1 Solution".
- **File/variable secrets**: Not handled. `ResolvedSecrets` on the `RunConfig` are not processed.

### 6.2 Design: Two-Tier Approach

The Kubernetes runtime supports two secret mounting strategies, selected by a `gke` flag in the runtime configuration. GKE with GCP Secret Manager integration is the expected production deployment, so it is the primary design. Plain K8s Secret objects are the fallback for non-GKE clusters.

```yaml
# In settings.yaml
runtimes:
  kubernetes:
    type: kubernetes
    gke: true    # Enable GCP Secret Manager CSI driver integration (default: false)
```

When `gke: true`, the runtime uses the GCP Secret Manager CSI Driver to mount secrets directly from GCP SM into pods — no plaintext values transit to etcd. When `gke: false` (or omitted), the runtime falls back to creating Kubernetes Secret objects per agent.

### 6.2.1 Primary: GCP Secret Manager CSI Driver (GKE)

When `gke: true` AND the Hub's secret backend is `gcpsm`, the runtime uses `Ref` fields from `ResolvedSecret` to create `SecretProviderClass` resources that reference secrets directly in GCP Secret Manager.

**Prerequisites:**
- GKE cluster with the Secrets Store CSI Driver addon enabled
- Workload Identity configured so the pod's service account can access GCP SM
- The `SecretProviderClass` CRD available in the cluster

**SecretProviderClass generation:**

```yaml
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: scion-agent-<name>
  labels:
    scion.agent: <name>
spec:
  provider: gcp
  parameters:
    secrets: |
      - resourceName: "projects/my-project/secrets/scion-user-abc-ANTHROPIC_API_KEY/versions/latest"
        path: "ANTHROPIC_API_KEY"
      - resourceName: "projects/my-project/secrets/scion-grove-xyz-TLS_CERT/versions/latest"
        path: "tls-cert"
  # Sync environment-type secrets to a K8s Secret for env var injection
  secretObjects:
    - secretName: scion-agent-<name>-env
      type: Opaque
      data:
        - objectName: "ANTHROPIC_API_KEY"
          key: "ANTHROPIC_API_KEY"
```

**Pod spec integration:**

```go
// CSI volume for file-type secrets
pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
    Name: "agent-secrets",
    VolumeSource: corev1.VolumeSource{
        CSI: &corev1.CSIVolumeSource{
            Driver:   "secrets-store.csi.x-k8s.io",
            ReadOnly: boolPtr(true),
            VolumeAttributes: map[string]string{
                "secretProviderClass": fmt.Sprintf("scion-agent-%s", config.Name),
            },
        },
    },
})

// Environment secrets via the synced K8s Secret
for _, s := range config.ResolvedSecrets {
    if s.Type == "environment" {
        envVars = append(envVars, corev1.EnvVar{
            Name: s.Target,
            ValueFrom: &corev1.EnvVarSource{
                SecretKeyRef: &corev1.SecretKeySelector{
                    LocalObjectReference: corev1.LocalObjectReference{
                        Name: fmt.Sprintf("scion-agent-%s-env", config.Name),
                    },
                    Key: s.Target,
                },
            },
        })
    }
}

// File secrets via subPath mounts from the CSI volume
for _, s := range config.ResolvedSecrets {
    if s.Type == "file" {
        pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
            Name:      "agent-secrets",
            MountPath: s.Target,
            SubPath:   s.Name,
            ReadOnly:  true,
        })
    }
}
```

This approach avoids storing secret values in etcd entirely. The Hub populates `ResolvedSecret.Ref` with the GCP SM resource name (e.g., `projects/my-project/secrets/scion-user-abc-API_KEY/versions/latest`), and the runtime uses these references directly — no plaintext values transit through the Broker.

### 6.2.2 Fallback: K8s Secret Objects (Non-GKE)

For non-GKE clusters or when `gke: false`, the Broker creates a Kubernetes Secret object per agent containing all resolved secrets, then references it from the pod spec. This eliminates plaintext values from the pod spec while working on any Kubernetes cluster.

**Secret Object Creation:**

Before creating the pod, the Broker creates a K8s Secret:

```go
func (r *KubernetesRuntime) createAgentSecret(ctx context.Context, namespace string, config RunConfig) (*corev1.Secret, error) {
    secretData := make(map[string][]byte)
    for _, s := range config.ResolvedSecrets {
        secretData[s.Name] = []byte(s.Value)
    }

    secret := &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{
            Name:      fmt.Sprintf("scion-agent-%s", config.Name),
            Namespace: namespace,
            Labels:    config.Labels,
            // OwnerReference is set to the Pod after creation for auto-cleanup
        },
        Data: secretData,
    }

    return r.Client.Clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
}
```

**Pod Spec Integration:**

**Environment secrets** are referenced via `secretKeyRef` instead of literal values:

```go
for _, s := range config.ResolvedSecrets {
    if s.Type == "environment" {
        envVars = append(envVars, corev1.EnvVar{
            Name: s.Target,
            ValueFrom: &corev1.EnvVarSource{
                SecretKeyRef: &corev1.SecretKeySelector{
                    LocalObjectReference: corev1.LocalObjectReference{
                        Name: fmt.Sprintf("scion-agent-%s", config.Name),
                    },
                    Key: s.Name,
                },
            },
        })
    }
}
```

**File secrets** are mounted as volumes:

```go
// Add secret volume
pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
    Name: "agent-secrets",
    VolumeSource: corev1.VolumeSource{
        Secret: &corev1.SecretVolumeSource{
            SecretName: fmt.Sprintf("scion-agent-%s", config.Name),
            Items:      fileSecretItems, // []corev1.KeyToPath
        },
    },
})

// Mount to target paths using subPath for each file secret
for _, s := range config.ResolvedSecrets {
    if s.Type == "file" {
        pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
            Name:      "agent-secrets",
            MountPath: s.Target,
            SubPath:   s.Name,
            ReadOnly:  true,
        })
    }
}
```

**Variable secrets** are written to a ConfigMap or included in the secret data and mounted at `~/.scion/secrets.json` via subPath.

### 6.2.3 Cleanup

All K8s resources created for secrets (Secret objects, SecretProviderClass CRDs) are labeled with `scion.agent=<name>`. On agent deletion, the runtime deletes all resources matching this label. This label-based approach is simple — it doesn't require OwnerReference post-creation updates and works for both the GKE (SecretProviderClass) and fallback (K8s Secret) paths.

The `Run()` method must also clean up secrets in its error path if pod creation fails after the secret/SPC was created, to avoid orphaned resources.

### 6.2.4 Removing the M1 Auth Injection

The hardcoded auth injection (lines 234-242 in `k8s_runtime.go`) should be removed once the secret mounting is in place. Auth credentials should flow through the standard `ResolvedSecrets` path. This means:

- Harness-required keys (e.g., `ANTHROPIC_API_KEY`) must be stored as secrets in the Hub
- The Broker must be configured to NOT fall back to hardcoded `config.Auth` fields
- The gather flow handles the case where no secret is configured (user enters it interactively)

This is a **breaking change** for existing K8s deployments. A migration path:
1. Deploy the new code
2. Users run `scion hub secret set ANTHROPIC_API_KEY <value>` (one-time)
3. Old auth injection code can be removed after a deprecation period

### 6.3 Strategy Comparison

| Factor | K8s Secret Objects (`gke: false`) | GCP SM CSI Driver (`gke: true`) |
|--------|----------------------------------|--------------------------------|
| **Cluster type** | Any K8s cluster | GKE with CSI driver addon |
| **Secret in etcd** | Yes (encrypted if configured) | No |
| **Setup complexity** | None | CSI driver + Workload Identity |
| **Rotation** | Manual (recreate secret) | Automatic (CSI polls SM) |
| **Cross-cluster** | Per-cluster | Shared via GCP SM |
| **Hub backend requirement** | Any | `gcpsm` (for `Ref` fields) |

### 6.4 Runtime Configuration Schema

The `gke` flag is added to `V1RuntimeConfig`:

```go
type V1RuntimeConfig struct {
    Type      string            `json:"type,omitempty" yaml:"type,omitempty" koanf:"type"`
    Host      string            `json:"host,omitempty" yaml:"host,omitempty" koanf:"host"`
    Context   string            `json:"context,omitempty" yaml:"context,omitempty" koanf:"context"`
    Namespace string            `json:"namespace,omitempty" yaml:"namespace,omitempty" koanf:"namespace"`
    Env       map[string]string `json:"env,omitempty" yaml:"env,omitempty" koanf:"env"`
    Sync      string            `json:"sync,omitempty" yaml:"sync,omitempty" koanf:"sync"`
    GKE       bool              `json:"gke,omitempty" yaml:"gke,omitempty" koanf:"gke"`
}
```

When `GKE` is true and the Hub's secret backend is `gcpsm`, the runtime uses the CSI driver path. If the Hub backend is not `gcpsm` (e.g., local/SQLite), the runtime falls back to K8s Secret objects even when `GKE` is true — the CSI driver requires GCP SM references.

This configuration should be documented in the Kubernetes runtime setup guide (`.design/kubernetes/` docs and the user-facing docs site).

---

## 7. End-to-End Flow: Secret-Aware Provisioning

Putting all three enhancements together, here is the complete provisioning flow:

```
User: scion start researcher --broker prod-gke "analyze auth module"

1. CLI → Hub: POST /groves/{id}/agents (gather_env=true)

2. Hub resolves:
   a. Env vars from storage (user, grove scopes)
   b. Secrets from SecretBackend.Resolve() → ResolvedSecrets[]
   c. Dispatches to Broker with ResolvedEnv + ResolvedSecrets

3. Broker evaluates completeness:
   a. Loads required keys from:
      - Harness RequiredEnvKeys() → [ANTHROPIC_API_KEY]
      - Settings empty-value env → [CUSTOM_KEY]
      - Template secrets section → [THIRD_PARTY_TOKEN]
   b. Checks merged env for each key
   c. Checks ResolvedSecrets for each key (auto-upgrade)
   d. Checks os.Getenv() for each key

   Result: ANTHROPIC_API_KEY satisfied by secret (auto-upgrade)
           CUSTOM_KEY satisfied by hub env
           THIRD_PARTY_TOKEN missing → needs

4. Broker → Hub → CLI: 202 with needs=[THIRD_PARTY_TOKEN],
                        secretInfo={THIRD_PARTY_TOKEN: {desc: "...", source: "template"}}

5. CLI (interactive):
   - Checks os.Getenv("THIRD_PARTY_TOKEN") → not found
   - Prompts user: "Enter value for THIRD_PARTY_TOKEN (input hidden): ********"
   - Submits gathered env back to Hub

6. Hub → Broker: FinalizeEnv with gathered values

7. Broker starts agent:
   - If gke: true → runtime creates SecretProviderClass referencing GCP SM,
     pod uses CSI volume mount + synced K8s Secret for env vars
   - If gke: false → runtime creates K8s Secret object, pod uses
     secretKeyRef for env vars and volume mounts for file secrets
   - No plaintext secrets in pod spec in either case
```

---

## 8. Open Questions

### 8.1 Should interactively gathered secrets be auto-stored? — Decided

**Decision:** Deferred to a future improvement. Interactively gathered secrets are ephemeral for the initial implementation. Users can store values permanently via `scion hub secret set`. A future iteration may offer a post-start prompt to store the value (with scope selection).

### 8.2 Secret declaration in Hub-managed templates — Decided

**Decision:** Add a `Secrets []RequiredSecret` field to the Hub's template model (`store.Template`). When the Hub dispatches agent creation, it includes the template's `secrets` declarations in the request so the Broker can evaluate completeness. This field is returned in the template API response and stored alongside other template metadata.

### 8.3 Conflict between gathered env value and resolved secret — Decided

**Decision:** The default merge behavior is correct — a broker env var set explicitly is a deliberate manual step and takes precedence. The auto-upgrade logic (§3) prevents keys satisfied by secrets from appearing in the "needs" list, so interactive conflicts shouldn't arise in practice. Broker and Hub logs should clearly indicate where each secret/env value was resolved from (scope, source type) to aid debugging when a broker env var overrides a Hub secret.

### 8.4 K8s secret cleanup on agent failure — Decided

**Decision:** The `Run()` method cleans up secrets/SPCs in its error path if pod creation fails. Additionally, `scion list` or a periodic reconciliation loop can identify orphaned resources (secrets/SPCs with `scion.agent` label but no matching pod) and clean them up.

### 8.5 GCP Secret Manager CSI driver: availability detection — Decided

**Decision:** Use the `gke: true` runtime configuration flag (§6.4) as an explicit opt-in. Don't auto-detect. The admin who configures the K8s runtime knows whether their cluster has the CSI driver. This must be documented in the Kubernetes runtime setup guide (`docs-site/src/content/docs/guides/`) and the K8s design docs (`.design/kubernetes/`).

### 8.6 File-type secrets in interactive gather — Decided

**Decision:** Deferred. File secrets are typically large (certificates, credential files) and are better handled via `scion hub secret set --type file ... @./file.pem`. The interactive gather flow only supports `environment` and `variable` type secrets. When a file-type secret is in the "needs" list, the CLI should display help text guiding the user to the `scion hub secret set` command with `--type file` and `@` syntax, rather than attempting inline file input.

---

## 9. Implementation Plan

### Phase 1: Auto-Upgrade (Required Env Var → Secret) — **Completed**

1. ~~Modify Broker `createAgent()` in `pkg/runtimebroker/handlers.go` to check `ResolvedSecrets` during env completeness evaluation~~
2. ~~Add `"secret"` as a scope value in `EnvSource` responses~~
3. ~~Update CLI display to show "Hub provides: ANTHROPIC_API_KEY (secret)"~~ (no CLI changes needed — existing `EnvSource.Scope` display handles `"secret"` automatically)
4. ~~Add tests to `handlers_envgather_test.go` and `envgather_test.go`~~

### Phase 2: Template/Settings `secrets` Section — **Implemented**

1. ~~Add `RequiredSecret` type to `pkg/api/types.go`~~
2. ~~Add `Secrets []RequiredSecret` field to `HarnessConfigEntry` and `V1ProfileConfig` in `pkg/config/settings_v1.go`~~
3. ~~Add template config parsing for `secrets` section~~
4. ~~Extend `extractRequiredEnvKeys()` to include Phase 3 (secrets declarations)~~
5. ~~Add `SecretInfo` to gather response types~~
6. ~~Update CLI gather to render secret descriptions~~

### Phase 3: CLI Interactive Secret Input — **Completed**

1. ~~Add hidden input prompt (using `golang.org/x/term`) for secret-eligible keys in `gatherAndSubmitEnv()`~~
2. ~~Distinguish secret-eligible vs env-only keys in gather logic~~
3. ~~Add guidance message about permanent storage~~
4. ~~Add help text for file-type secrets guiding users to `scion hub secret set --type file`~~
5. ~~Tests for interactive flow (mock stdin)~~

### Phase 4: K8s Native Secret Mounting

1. Add `GKE` field to `V1RuntimeConfig` in `pkg/config/settings_v1.go`
2. **GKE path:** Add `SecretProviderClass` CRD generation to `KubernetesRuntime`
3. **GKE path:** Modify `buildPod()` to use CSI volume and `secretObjects` sync for env vars
4. **Fallback path:** Add `createAgentSecret()` method for K8s Secret object creation
5. **Fallback path:** Modify `buildPod()` to use `secretKeyRef` for env secrets and volume mounts for file secrets
6. Add secret/SPC cleanup to `Delete()` method (label-based) and `Run()` error path
7. Remove M1 hardcoded auth injection (with deprecation notice)
8. Plumb `Ref` field from Hub dispatch through to runtime for GKE path
9. Add Kubernetes runtime documentation for `gke` flag and secret setup
10. Tests for pod spec generation with both strategies

---

## 10. References

- **Secrets Management Design:** `.design/hosted/secrets.md`
- **Env Gather Design:** `.design/hosted/env-gather.md`
- **Remote Env Gather Protocol:** `.design/hosted/remote-env-gather.md`
- **Kubernetes Milestones:** `.design/kubernetes/milestones.md`
- **Kubernetes Overview:** `.design/kubernetes/overview.md`
- **Hub API:** `.design/hosted/hub-api.md`
- **K8s Secrets Store CSI Driver:** https://secrets-store-csi-driver.sigs.k8s.io/
- **GCP Secret Manager:** https://cloud.google.com/secret-manager/docs
