# Secrets Management Design

**Status:** Phase 1 & 2 Implemented
**Updated:** 2026-02-11

## 1. Overview

This document specifies the design for secrets management in Scion. Secrets are sensitive values—API keys, credentials, certificates, configuration files—that agents need at runtime. They are stored centrally in the Hub (or an external secrets backend) and projected into agent containers at provisioning time.

The current system provides basic environment-variable-style secrets (key-value pairs injected as `ResolvedEnv` in the `CreateAgent` dispatch). This design extends secrets into a first-class concept with a typed interface, pluggable storage backends, and runtime-specific projection strategies.

### 1.1 Goals

- **Typed secrets**: Distinguish between environment variables, opaque variables (non-env key-value), and file-system secrets.
- **Pluggable storage**: Define an abstract storage interface with a primary GCP Secret Manager implementation.
- **Runtime-aware projection**: Each runtime (Docker, Apple, Kubernetes, Cloud Run) projects secrets using its native capabilities.
- **Least-privilege**: Secrets are write-only in the Hub API, decrypted only at provisioning time, and scoped to the narrowest context required.
- **Clear separation from env vars**: The existing plain-text `EnvVar` system remains as-is for non-sensitive configuration. Secrets are a distinct system with encrypted storage and controlled projection. The existing `Secret` store model (which was not in use) is upgraded in place to support the new typed secret model.

### 1.2 Non-Goals (This Iteration)

- Secret rotation policies and automated key cycling.
- Agent-initiated secret access at runtime (secrets are injected at start, not fetched on demand).
- Audit logging of secret access events (tracked as a future enhancement).
- Multi-cloud secret backends (AWS Secrets Manager, HashiCorp Vault) beyond GCP.

---

## 2. Secret Model

### 2.1 Secret Types

A secret has a **type** that determines how it is projected into the agent container:

| Type | Description | Target Semantics |
|------|-------------|------------------|
| `environment` | Injected as a container environment variable | Target is the environment variable name (e.g., `ANTHROPIC_API_KEY`) |
| `variable` | An opaque key-value pair stored but not automatically injected as an env var | Target is a logical name; consumed by templates or tooling |
| `file` | Projected as a file on the container filesystem | Target is the absolute file path (e.g., `/home/scion/.config/credentials.json`) |

### 2.2 Go Interface Definition

```go
package secret

// Type represents the kind of secret and how it is projected.
type Type string

const (
    TypeEnvironment Type = "environment"
    TypeVariable    Type = "variable"
    TypeFile        Type = "file"
)

// Secret is the core interface for a typed secret.
type Secret interface {
    // Name returns the unique identifier for this secret within its scope.
    Name() string

    // Type returns the secret type (environment, variable, or file).
    Type() Type

    // Target returns the projection target.
    // For environment secrets: the environment variable name.
    // For variable secrets: a logical key name.
    // For file secrets: the absolute container file path.
    Target() string

    // Value returns the secret's plaintext value as bytes.
    // For environment and variable secrets, this is the UTF-8 string value.
    // For file secrets, this is the raw file content.
    Value() []byte

    // Scope returns the scope at which this secret is defined.
    Scope() Scope

    // ScopeID returns the ID of the scoped entity (user ID, grove ID, or broker ID).
    ScopeID() string

    // Version returns the secret version, incremented on each update.
    Version() int

    // Description returns an optional human-readable description.
    Description() string
}

// Scope identifies the level at which a secret is defined.
type Scope string

const (
    ScopeUser          Scope = "user"
    ScopeGrove         Scope = "grove"
    ScopeRuntimeBroker Scope = "runtime_broker"
    // ScopeAgent is reserved for future use. Agent-scoped secrets would be
    // tied to a specific agent instance. The current design accommodates this
    // as a future addition without breaking changes to the resolution logic
    // (it would slot in as the highest-priority scope).
    // ScopeAgent Scope = "agent"
)
```

### 2.3 Concrete Implementation

```go
// SecretEntry is the standard implementation of the Secret interface.
type SecretEntry struct {
    name        string
    secretType  Type
    target      string
    value       []byte
    scope       Scope
    scopeID     string
    version     int
    description string
}

// NewEnvironmentSecret creates a secret projected as an environment variable.
func NewEnvironmentSecret(name, envKey string, value []byte, scope Scope, scopeID string) *SecretEntry {
    return &SecretEntry{
        name:       name,
        secretType: TypeEnvironment,
        target:     envKey,
        value:      value,
        scope:      scope,
        scopeID:    scopeID,
    }
}

// NewFileSecret creates a secret projected as a file.
func NewFileSecret(name, filePath string, content []byte, scope Scope, scopeID string) *SecretEntry {
    return &SecretEntry{
        name:       name,
        secretType: TypeFile,
        target:     filePath,
        value:      content,
        scope:      scope,
        scopeID:    scopeID,
    }
}
```

### 2.4 Store Model

The `Secret` store model is upgraded in place with type, target, and audit metadata fields:

```go
// In pkg/store/models.go

type Secret struct {
    ID             string    `json:"id"`
    Key            string    `json:"key"`
    EncryptedValue string    `json:"-"`

    // Secret type and target
    SecretType string `json:"secretType"` // "environment", "variable", "file"
    Target     string `json:"target"`     // env var name, logical key, or file path

    Scope       string `json:"scope"`
    ScopeID     string `json:"scopeId"`
    Description string `json:"description,omitempty"`
    Version     int    `json:"version"`

    Created   time.Time `json:"created"`
    Updated   time.Time `json:"updated"`
    CreatedBy string    `json:"createdBy"`
    UpdatedBy string    `json:"updatedBy,omitempty"`
}
```

Default behavior: if `SecretType` is empty, the secret is treated as `TypeEnvironment` with `Target` defaulting to `Key`.

---

## 3. Storage Interface

### 3.1 Abstract Interface

The higher-level business logic interface for secret storage is `secret.SecretBackend`. This is distinct from the low-level database persistence layer (`store.SecretStore`) to avoid naming confusion (see Section 3.3).

```go
package secret

import "context"

// Filter specifies criteria for listing secrets.
type Filter struct {
    Scope   Scope  // Required
    ScopeID string // Required
    Type    Type   // Optional: filter by secret type
    Name    string // Optional: filter by exact name
}

// SecretBackend defines the abstract interface for secret storage backends.
// This is the business-logic layer that may delegate to store.SecretStore,
// GCP Secret Manager, or other backends for actual persistence.
type SecretBackend interface {
    // Get retrieves a secret by name within a scope.
    // Returns the secret with its decrypted value.
    Get(ctx context.Context, name string, scope Scope, scopeID string) (Secret, error)

    // Set creates or updates a secret.
    // The implementation is responsible for encrypting the value at rest.
    Set(ctx context.Context, s Secret) error

    // Delete removes a secret by name within a scope.
    Delete(ctx context.Context, name string, scope Scope, scopeID string) error

    // List returns secret metadata matching the filter.
    // Values are NOT populated in the returned secrets.
    List(ctx context.Context, filter Filter) ([]Secret, error)

    // Resolve returns all secrets applicable to a given agent context,
    // merging across scopes with the standard precedence:
    // user < grove < runtime_broker.
    // Returns secrets with their decrypted values.
    Resolve(ctx context.Context, userID, groveID, brokerID string) ([]Secret, error)
}
```

### 3.2 Resolution Semantics

The `Resolve` method implements the same hierarchical merge used for environment variables today:

1. **User scope** (lowest priority): Secrets defined for the agent's owner.
2. **Grove scope**: Secrets defined for the project.
3. **Runtime Broker scope** (highest priority): Secrets specific to the execution host.

Within the same scope, secrets with the same `Name` are deduplicated (last write wins). Across scopes, higher-priority scopes override lower ones when names collide.

> **Future consideration:** Agent-scoped secrets (see Section 2.2) would slot in as the highest-priority scope, overriding runtime broker scope when present.

### 3.3 Relationship to Existing Stores

The `secret.SecretBackend` interface is the higher-level business logic abstraction. The `store.SecretStore` interface is the low-level database persistence layer (SQLite/Postgres) that handles metadata and encrypted value storage. `SecretBackend` implementations may delegate to `store.SecretStore`, GCP Secret Manager, or other backends.

```
                     ┌──────────────────────┐
                     │ secret.SecretBackend │  (business logic interface)
                     └──────────┬───────────┘
                                │
                ┌───────────────┼───────────────┐
                │               │               │
        ┌───────────┐   ┌─────────────┐   ┌──────────┐
        │ SQLite     │   │ GCP Secret  │   │ Vault    │
        │ (existing) │   │ Manager     │   │ (future) │
        └───────────┘   └─────────────┘   └──────────┘
```

---

## 4. GCP Secret Manager Implementation

### 4.1 Overview

The primary production implementation uses [Google Cloud Secret Manager](https://cloud.google.com/secret-manager) for encrypted secret storage. This provides:

- Envelope encryption with Google-managed keys (or customer-managed via Cloud KMS).
- Automatic versioning of secret values.
- IAM-based access control.
- Audit logging via Cloud Audit Logs.

### 4.2 Secret Naming Convention

> **Superseded.** This section predates both the hub-instance-ID namespacing in
> `secret-id-hub-refactor.md` and the hub-prefix layer added on top of it by
> `ptone/scion#2152`. The naming scheme actually implemented today is:
>
> ```
> scion-{h12}-{scope}-{sha256(hubID:scopeID)[:12]}-{name}
> ```
>
> where `h12` is the first 12 hex characters of `sha256(hubID)` — see
> `pkg/secret/gcpbackend.go` (`secretNamePrefix`, `gcpSecretName`) and
> `docs-site/src/content/docs/hosted/user/secrets.md` for the operator-facing
> IAM-grant guidance. The rest of this section is kept for historical context
> (the collision-avoidance rationale below still applies to the scope/name
> segment) but the concrete name format and examples are out of date.

GCP Secret Manager has a flat namespace per project. Scion secrets are mapped using a hashed naming convention to avoid collisions and stay within the 255-character GCP SM secret ID limit:

```
scion-{scope}-{sha256(scopeID)[:12]}-{name}
```

The `scopeID` is hashed using SHA-256, truncated to the first 12 hex characters (48 bits, birthday collision threshold ~16 million). This prevents collisions when:
- Different scope IDs sanitize to the same string (e.g., `abc-def` vs `abc@def`)
- The default scope ID `"default"` is used for user-scoped secrets
- Long UUIDs combined with long names would exceed the 255-char limit

The full scope ID is preserved in GCP labels (see Section 4.4) for discoverability and cross-referencing.

Examples (historical; see the superseded note above for the current format):
- `scion-user-a1b2c3d4e5f6-ANTHROPIC_API_KEY`
- `scion-grove-f9e8d7c6b5a4-DB_PASSWORD`
- `scion-runtime_broker-1a2b3c4d5e6f-TLS_CERT`

### 4.3 Implementation Sketch

```go
package gcpsecrets

import (
    "context"
    "fmt"

    secretmanager "cloud.google.com/go/secretmanager/apiv1"
    smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"

    "github.com/GoogleCloudPlatform/scion/pkg/secret"
)

// GCPStore implements secret.SecretBackend using GCP Secret Manager.
type GCPStore struct {
    client    *secretmanager.Client
    projectID string
}

func NewGCPStore(ctx context.Context, projectID string) (*GCPStore, error) {
    client, err := secretmanager.NewClient(ctx)
    if err != nil {
        return nil, fmt.Errorf("create secret manager client: %w", err)
    }
    return &GCPStore{client: client, projectID: projectID}, nil
}

func (s *GCPStore) secretPath(name string, scope secret.Scope, scopeID string) string {
    secretName := fmt.Sprintf("scion-%s-%s-%s", scope, scopeID, name)
    return fmt.Sprintf("projects/%s/secrets/%s", s.projectID, secretName)
}

func (s *GCPStore) Get(ctx context.Context, name string, scope secret.Scope, scopeID string) (secret.Secret, error) {
    path := s.secretPath(name, scope, scopeID) + "/versions/latest"
    result, err := s.client.AccessSecretVersion(ctx, &smpb.AccessSecretVersionRequest{
        Name: path,
    })
    if err != nil {
        return nil, fmt.Errorf("access secret %s: %w", name, err)
    }
    // Retrieve metadata to determine type/target (stored as labels on the secret)
    // ...
    return secret.NewEnvironmentSecret(name, name, result.Payload.Data, scope, scopeID), nil
}

// Set, Delete, List, Resolve methods follow similar patterns...
```

### 4.4 Metadata Storage

The **Hub database is the primary metadata store** for all secret metadata (name, type, target, scope, version, audit fields). GCP Secret Manager stores the encrypted secret values and may additionally carry supplementary labels for cross-referencing. These labels should match the native Scion types stored in the Hub database.

GCP Secret Manager supports labels on secret resources. Scion stores the following supplementary metadata as labels:

| Label Key | Value | Purpose |
|-----------|-------|---------|
| `scion-scope` | `user`, `grove`, `runtime_broker` | Scope identification |
| `scion-scope-id` | Full scope ID (sanitized) | Scoped entity ID (full value, since the secret name uses a hash) |
| `scion-type` | `environment`, `variable`, `file` | Secret type |
| `scion-name` | Secret key name (sanitized) | Original secret name for discoverability |
| `scion-target` | Projection target (sanitized) | Projection target (env var name, file path, etc.) |

These labels are maintained for operational convenience (e.g., GCP Console visibility, cross-referencing with hashed secret names) but the Hub database remains the authoritative source for secret metadata.

### 4.5 Hybrid Storage (Default)

The default storage architecture uses a hybrid approach:

- **Secret metadata** (name, type, target, scope, version, audit fields) stored in the Hub database.
- **Secret values** stored in GCP Secret Manager, referenced by a `secretRef` field in the Hub database record.
- The `secret.SecretBackend` implementation joins metadata from the database with values from GCP SM.

This keeps the Hub database as the metadata authority, enabling simple listing and visibility in the web UI and CLI without requiring GCP Secret Manager list API calls, while delegating value encryption to GCP.

```
┌─────────────┐          ┌───────────────────┐
│ Hub Database │◀────────│ secret.SecretBackend │
│ (metadata)   │         │ (joins both)        │
└─────────────┘          └─────────┬──────────┘
                                   │
                          ┌────────▼────────┐
                          │ GCP Secret Mgr  │
                          │ (encrypted vals) │
                          └─────────────────┘
```

---

## 5. Runtime Projection

Each runtime projects secrets differently based on its capabilities. The projection logic runs during agent provisioning, after the Hub resolves secrets and dispatches the `CreateAgent` command.

### 5.1 Resolved Secrets in CreateAgent

The `CreateAgentRequest` type is extended to include typed secrets alongside the existing `ResolvedEnv`:

```go
// In pkg/runtimebroker/types.go

type CreateAgentRequest struct {
    // ... existing fields ...

    ResolvedEnv map[string]string `json:"resolvedEnv,omitempty"`

    // NEW: Typed secrets resolved by the Hub.
    // Includes environment, variable, and file secrets.
    ResolvedSecrets []ResolvedSecret `json:"resolvedSecrets,omitempty"`
}

// ResolvedSecret is a secret resolved by the Hub for runtime projection.
type ResolvedSecret struct {
    Name   string `json:"name"`
    Type   string `json:"type"`            // "environment", "variable", "file"
    Target string `json:"target"`          // env var name or file path
    Value  string `json:"value,omitempty"` // plaintext value (base64-encoded for file type)
    Source string `json:"source"`          // scope that provided this secret (for diagnostics)
    Ref    string `json:"ref,omitempty"`   // GCP SM reference for K8s/Cloud Run (e.g., "projects/my-proj/secrets/scion-user-abc-API_KEY/versions/latest")
}
```

For Docker and Apple runtimes, `Value` is populated with the plaintext secret value. For Kubernetes and Cloud Run, `Ref` is populated with a GCP Secret Manager reference instead, allowing the runtime to resolve values natively without plaintext transit through the Hub.

### 5.2 Docker Runtime

Docker is the most straightforward runtime for secret projection.

#### Environment Secrets
Passed as `-e` flags on the `docker run` command line, merged into the existing environment injection in `buildCommonRunArgs()`:

```go
for _, s := range resolvedSecrets {
    if s.Type == "environment" {
        addArg("-e", fmt.Sprintf("%s=%s", s.Target, s.Value))
    }
}
```

#### File Secrets

File secrets are stored in a dedicated `secrets/` directory within the agent's provisioning directory, separate from the agent's home directory. This prevents secrets from being exposed inside the container's home mount.

**Host directory layout:**
```
.scion/agents/
  <agentName>/
    secrets/       ← secret files stored here (host-only)
      secretA
      secretB
    home/          ← bind-mounted as /home/scion
    workspace/     ← bind-mounted as /workspace
```

Each secret file is written to the `secrets/` directory on the host, then bind-mounted directly to its target path inside the container:

```go
for _, s := range resolvedSecrets {
    if s.Type == "file" {
        // Write secret to the agent's secrets directory (NOT home)
        hostPath := filepath.Join(agentDir, "secrets", s.Name)
        os.MkdirAll(filepath.Dir(hostPath), 0700)
        os.WriteFile(hostPath, []byte(s.Value), 0600)

        // Bind-mount to the secret's target path inside the container
        addArg("--mount", fmt.Sprintf(
            "type=bind,source=%s,target=%s,readonly",
            hostPath, s.Target,
        ))
    }
}
```

This respects the secret's declared target path (which may be anywhere in the container filesystem) without exposing secret files inside the home directory mount.

#### Variable Secrets
Variable-type secrets are not automatically injected. They are stored in a metadata file within the home directory that tooling (e.g., `sciontool`) can read:

```
/home/scion/.scion/secrets.json
```

### 5.3 Apple Container Runtime

The Apple Virtualization Framework (`container` CLI) supports bind mounts of directories but has limited support for individual file mounts. This requires a different approach for file-type secrets than Docker.

#### Environment Secrets
Same as Docker—passed via `-e` flags. The Apple runtime reuses `buildCommonRunArgs()`.

#### File Secrets — Init Script Injection via sciontool

Since the Apple container runtime cannot bind-mount individual files to arbitrary target paths, file secrets use a copy-on-init approach managed by `sciontool`:

1. **Provisioning**: The broker writes secret files into the agent's `secrets/` directory on the host (same layout as Docker). Since this directory is separate from the home directory, secrets are available to the provisioner but not directly exposed inside the container.

2. **Secret map**: The provisioner writes a `secret-map.json` file alongside the secrets, describing where each secret file should be placed inside the container:

```json
{
    "secrets": [
        {
            "name": "gcp-credentials",
            "source": "secrets/gcp-credentials",
            "target": "/home/scion/.config/gcloud/credentials.json",
            "mode": "0600"
        },
        {
            "name": "tls-cert",
            "source": "secrets/tls-cert",
            "target": "/etc/ssl/certs/agent.pem",
            "mode": "0644"
        }
    ]
}
```

3. **Container start**: The `secrets/` directory is bind-mounted into the container at a well-known staging path (e.g., `/run/scion-secrets/`). On startup, `sciontool` reads `secret-map.json` and copies each secret file from the staging path to its declared target path, creating parent directories as needed. The entrypoint does not need variable arguments—the logic is fixed and data-driven by the map file.

4. **Cleanup**: After copying, `sciontool` removes the staging mount contents from the container filesystem.

This approach supports arbitrary target paths without modifying the container entrypoint arguments, and keeps the init logic fixed and deterministic.

#### Variable Secrets
Same as Docker—written to `~/.scion/secrets.json`.

### 5.4 Kubernetes Runtime

Kubernetes has native support for both environment variable and file-based secret projection via the Kubernetes Secrets API.

#### GCP Secret Manager Integration

For GCP-hosted Kubernetes clusters (GKE), secrets should be projected using **GCP Secret Manager CSI Driver** or **Workload Identity** rather than duplicating values into Kubernetes Secret objects. This avoids storing plaintext values in etcd.

##### Option 1: SecretProviderClass (CSI Driver)

```yaml
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: agent-secrets-<agentId>
spec:
  provider: gcp
  parameters:
    secrets: |
      - resourceName: "projects/<project>/secrets/scion-user-<userId>-API_KEY/versions/latest"
        path: "api-key"
      - resourceName: "projects/<project>/secrets/scion-grove-<groveId>-TLS_CERT/versions/latest"
        path: "tls-cert"
```

Mounted as a volume in the agent Pod:

```yaml
volumes:
  - name: secrets
    csi:
      driver: secrets-store.csi.x-k8s.io
      readOnly: true
      volumeAttributes:
        secretProviderClass: agent-secrets-<agentId>
```

##### Option 2: External Secrets Operator

For non-GKE clusters, the [External Secrets Operator](https://external-secrets.io/) can sync GCP Secret Manager secrets into Kubernetes Secret objects:

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: agent-<agentId>
spec:
  refreshInterval: 0  # One-time fetch
  secretStoreRef:
    name: gcp-secret-store
    kind: ClusterSecretStore
  target:
    name: agent-<agentId>-secrets
  data:
    - secretKey: ANTHROPIC_API_KEY
      remoteRef:
        key: scion-user-<userId>-ANTHROPIC_API_KEY
```

#### Environment Secrets
Projected via `envFrom` or individual `env` entries in the Pod spec referencing the Kubernetes Secret:

```yaml
env:
  - name: ANTHROPIC_API_KEY
    valueFrom:
      secretKeyRef:
        name: agent-<agentId>-secrets
        key: ANTHROPIC_API_KEY
```

#### File Secrets
Mounted as volumes from the Kubernetes Secret or CSI driver:

```yaml
volumeMounts:
  - name: secrets
    mountPath: /home/scion/.config/credentials.json
    subPath: credentials.json
    readOnly: true
```

### 5.5 Cloud Run (Future)

Cloud Run supports native GCP Secret Manager integration:

```yaml
env:
  - name: API_KEY
    valueFrom:
      secretKeyRef:
        secret: scion-user-abc-API_KEY
        version: latest
volumes:
  - name: secrets
    secret:
      secret: scion-grove-xyz-TLS_CERT
      items:
        - path: tls-cert.pem
          version: latest
```

This is the cleanest integration since Cloud Run natively resolves GCP Secret Manager references without any CSI driver or operator.

### 5.6 Projection Summary

| Capability | Docker | Apple | Kubernetes | Cloud Run |
|------------|--------|-------|------------|-----------|
| **Env secrets** | `-e` flag | `-e` flag | `envFrom` / `env.valueFrom` | `env.valueFrom` |
| **File secrets** | Bind mount to target path | Copy-on-init via sciontool | Volume mount / CSI | Volume mount |
| **Variable secrets** | `secrets.json` | `secrets.json` | ConfigMap or `secrets.json` | `secrets.json` |
| **GCP SM native** | No (values passed) | No (values passed) | Yes (CSI / ESO) | Yes (native) |
| **Secret in etcd/disk** | Host `secrets/` dir | Host `secrets/` dir | Optional (CSI avoids) | Never |

---

## 6. Hub API Changes

### 6.1 Extended Secret Endpoints

The existing secret API endpoints (Section 7.3 of `hub-api.md`) are extended to support the new type and target fields.

#### Set Secret (Updated)

```
PUT /api/v1/secrets/{key}
```

**Request Body:**
```json
{
  "value": "string",
  "scope": "user",
  "scopeId": "string",
  "description": "string",
  "type": "environment",
  "target": "ANTHROPIC_API_KEY"
}
```

New fields:
- `type` (optional, default: `"environment"`): One of `environment`, `variable`, `file`.
- `target` (optional, defaults to `key`): The projection target.

For `file` type secrets, `value` should be base64-encoded.

#### Get Secret Metadata (Updated)

```json
{
  "id": "uuid",
  "key": "my-api-key",
  "type": "environment",
  "target": "ANTHROPIC_API_KEY",
  "scope": "user",
  "scopeId": "user-abc",
  "description": "Anthropic API key for Claude agents",
  "version": 3,
  "created": "2026-01-24T10:00:00Z",
  "updated": "2026-02-11T14:30:00Z",
  "createdBy": "user-abc",
  "updatedBy": "user-abc"
}
```

### 6.2 Resolved Secrets Endpoint (Internal)

A new internal endpoint for the Hub to resolve typed secrets during agent creation:

```
GET /api/v1/agents/{agentId}/resolved-secrets
```

**Response:**
```json
{
  "secrets": [
    {
      "name": "anthropic-key",
      "type": "environment",
      "target": "ANTHROPIC_API_KEY",
      "value": "sk-...",
      "source": "user"
    },
    {
      "name": "gcp-credentials",
      "type": "file",
      "target": "/home/scion/.config/gcloud/credentials.json",
      "value": "base64-encoded-content",
      "source": "grove"
    }
  ]
}
```

### 6.3 CLI Changes

The `scion hub secret set` command is extended:

```bash
# Environment secret (default)
scion hub secret set API_KEY sk-ant-...

# Explicit type
scion hub secret set --type=environment --target=ANTHROPIC_API_KEY api-key sk-ant-...

# File secret
scion hub secret set --type=file --target=/home/scion/.config/creds.json gcp-creds @./service-account.json

# Variable secret
scion hub secret set --type=variable config-value '{"setting": true}'
```

The `@` prefix for values reads from a local file (similar to `curl -d @file`).

The `scion hub env --secret` flag is a CLI convenience that translates the operation into a secret resource creation (environment-type secret). This keeps the env var system cleanly separated—`--secret` does not set an `EnvVar` record with `Secret: true`, but instead creates a proper `Secret` resource with `type=environment`.

```bash
# These two commands are equivalent:
scion hub env --secret ANTHROPIC_API_KEY sk-ant-...
scion hub secret set --type=environment --target=ANTHROPIC_API_KEY ANTHROPIC_API_KEY sk-ant-...
```

---

## 7. Security Considerations

### 7.1 Value Transmission

- Secret values are transmitted over TLS between Hub and Runtime Brokers.
- For Docker and Apple runtimes, decrypted values traverse the control channel (WebSocket over TLS) and are present on the broker host filesystem (in the agent's `secrets/` directory) and in the container process environment.
- For Kubernetes and Cloud Run, the GCP Secret Manager CSI driver or native integration avoids transmitting plaintext values through the Hub at all—only secret references are passed.

### 7.2 Value at Rest

| Component | Encryption | Notes |
|-----------|-----------|-------|
| Hub database | Application-level encryption (existing) | `EncryptedValue` field, encrypted before storage |
| GCP Secret Manager | Envelope encryption (Google KMS) | Automatic, configurable CMEK |
| Docker host filesystem | Dependent on host disk encryption | Files in agent `secrets/` directory |
| Kubernetes etcd | K8s encryption-at-rest config | Avoidable with CSI driver |

### 7.3 Value Lifecycle

1. **Write**: User sets secret via CLI/API → Hub encrypts and stores.
2. **Resolve**: Hub dispatches agent creation → decrypts and includes in `ResolvedSecrets`.
3. **Project**: Runtime Broker projects secrets into the container (env, file, etc.).
4. **Cleanup**: On agent deletion, broker removes provisioning directory (including secret files).

### 7.4 Logging

- Secret values MUST NOT appear in logs at any tier (Hub, Broker, Agent).
- The `ResolvedSecrets` field should be redacted in request/response logging.
- File contents for file-type secrets should never be logged.

### 7.5 File Secret Size Limits

File-type secrets are limited to **64 KiB** to match GCP Secret Manager's per-version limit. This is sufficient for certificates, credential files, and small configuration files. Larger files should use the existing template/workspace mechanisms.

---

## 8. Migration Path

### 8.1 Phase 1: Type-Aware Store Model — **Implemented**

1. ~~Add `SecretType` and `Target` columns to the secrets table (nullable, defaulting to `"environment"` and `Key` respectively).~~ Done (migration V13).
2. ~~Add `CreatedBy` and `UpdatedBy` audit columns to the secrets table.~~ Already present; no migration needed.
3. ~~Update `store.SecretStore` interface and SQLite implementation.~~ Done (`SecretFilter.Type`, CRUD updates).
4. ~~Update Hub API handlers to accept and return the new fields.~~ Done (type validation, file path validation, 64 KiB limit).
5. ~~Update CLI `hub secret set/get` commands.~~ Done (`--type`, `--target`, `@file` syntax, TYPE column in list).
6. ~~Implement `scion hub env --secret` as a convenience that creates a secret resource.~~ Done (redirects to Secret API).

### 8.2 Phase 2: Runtime Projection — **Implemented**

1. ~~Add `ResolvedSecrets` to `CreateAgentRequest`.~~ Done (`api.ResolvedSecret`, wired through dispatch chain).
2. ~~Implement projection logic in the Runtime Broker's `CreateAgent` handler.~~ Done (passthrough to `RunConfig`).
3. ~~Docker: env injection and bind-mount file secrets to target paths.~~ Done.
4. ~~Apple: env injection and copy-on-init via `sciontool` with `secret-map.json`.~~ Done (secrets staging volume + secret-map.json).

### 8.3 Phase 3: GCP Secret Manager Backend

1. ~~Implement `secret.SecretBackend` using hybrid storage (Hub DB metadata + GCP SM values).~~ Done (`pkg/secret/` package with `SecretBackend` interface, `LocalBackend`, and `GCPBackend`).
2. ~~Configuration to select backend (SQLite-encrypted vs hybrid GCP SM).~~ Done (`SecretsConfig` in `pkg/config/hub_config.go`, env vars `SCION_SERVER_SECRETS_*`).
3. ~~Migration tooling to move existing secrets from SQLite to GCP SM.~~ Done (`scion hub secret migrate` command with `--project`, `--credentials`, `--dry-run` flags).

### 8.4 Phase 4: Native K8s/Cloud Run Integration

1. K8s runtime generates `SecretProviderClass` or `ExternalSecret` resources.
2. Cloud Run runtime uses native secret references in service config.
3. These runtimes receive secret *references* rather than plaintext values from the Hub.

---

## 9. Decisions & Future Considerations

This section records decisions made during design review and topics deferred to future iterations.

### 9.1 Secret Reference vs. Value in Dispatch — Decided

**Decision:** Use plaintext dispatch for Docker and Apple runtimes. Use reference dispatch for Kubernetes and Cloud Run where native GCP SM integration is available. The `ResolvedSecret` type includes an optional `Ref` field (see Section 5.1) to carry GCP Secret Manager references.

### 9.2 File Secret Size Limits — Decided

**Decision:** Enforce a 64 KiB limit for file secrets to match GCP Secret Manager limits. Larger files should use the existing template/workspace mechanisms. See Section 7.5.

### 9.3 Apple Container File Mounts — Decided

**Decision:** Use the copy-on-init approach via `sciontool` (Section 5.3). Secret files are placed in the provisioning directory and a `secret-map.json` file drives the init-time copy to target paths. This supports arbitrary target paths without requiring variable entrypoint arguments.

### 9.4 Required Secrets in Templates — Decided

**Decision:** Templates can declare required secrets using the existing pattern of declaring an env key with no value. At resolve time, the system determines whether the value comes from the `EnvVar` store (plain text) or the `Secret` store (encrypted), and fails fast with a clear error if a required key has no value in any applicable scope.

### 9.5 Env Var / Secret Separation — Decided

**Decision:** Env vars and secrets are two distinct systems:

- **Env vars** (`EnvVar` model): Stored in the Hub database in plain text. No secret-manager integration. Used for non-sensitive configuration.
- **Secrets** (`Secret` model): Stored with encrypted values (Hub DB or GCP SM). Used for sensitive values.

The `EnvVar.Secret` bool flag is **not** used as the implementation for secret environment variables. Instead, `scion hub env --secret` is a CLI-only convenience that translates the command into an operation on a `Secret` resource with `type=environment`. This keeps the two systems cleanly separated.

### 9.6 Secret Versioning and Rollback — Deferred

Always use the latest version for now. Version pinning adds complexity to the resolution logic and agent creation flow. GCP SM's native versioning can be exposed later if needed.

### 9.7 Cross-Grove Secret Sharing — Deferred

User-scope secrets already solve cross-grove sharing. Document that user-scope is the recommended approach. Organization/team-scope secrets can be added as a future enhancement when user and groups functionality is implemented.

### 9.8 Agent-Scoped Secrets — Deferred

Agent-scoped secrets (tied to a specific agent instance) are a possible future addition. The current design accommodates this by reserving `ScopeAgent` in the scope enum (see Section 2.2). Implementation is deferred to a future iteration.

---

## 10. References

- **Hosted Architecture:** `.design/hosted/hosted-architecture.md` Section 4.4
- **Hub API:** `.design/hosted/hub-api.md` Section 7
- **Runtime Broker API:** `.design/hosted/runtime-broker-api.md` Section 5
- **GCP Secret Manager:** https://cloud.google.com/secret-manager/docs
- **K8s Secrets Store CSI Driver:** https://secrets-store-csi-driver.sigs.k8s.io/
- **External Secrets Operator:** https://external-secrets.io/
