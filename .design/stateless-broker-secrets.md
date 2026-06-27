# Stateless Broker Secret Staging

**Issue:** #284  
**Status:** Phase 1 implemented  

## Problem

The existing secret staging mechanism writes file and variable secrets to the
broker's host filesystem, then bind-mounts them into agent containers. This
creates a hard dependency on broker-local storage, breaking stateless broker
deployments and HA configurations where:

- The broker process may not share a filesystem with the container daemon.
- Multiple broker replicas cannot share staged secrets across nodes.
- Ephemeral broker filesystems lose secrets between dispatches.

## Approach

Replace host-filesystem staging with a single environment variable pipeline.
The broker serializes all file and variable secrets into one base64-encoded
JSON blob (`SCION_STAGED_SECRETS`) and injects it via `-e` at container
launch. Inside the container, `sciontool init` decodes the blob and writes
secrets to their target paths before any other process reads them.

This eliminates all broker-side filesystem writes for secrets and removes the
bind-mount assumptions from Docker, Podman, and Apple container runtimes.

### What changed

| Component | Before | After |
|-----------|--------|-------|
| **Broker (common.go)** | `writeFileSecrets()` writes to host, returns bind-mount specs; `writeVariableSecrets()` writes `secrets.json` to host; `writeSecretMap()` writes Apple manifest | `serializeSecrets()` produces base64 JSON blob; `DecodeStagedSecrets()` / `WriteStagedSecrets()` for container-side use |
| **Docker runtime** | Calls `writeFileSecrets` + `writeVariableSecrets`, inserts `-v` mount flags | Calls `serializeSecrets`, injects `SCION_STAGED_SECRETS` env var |
| **Podman runtime** | Same as Docker | Same as Docker |
| **Apple runtime** | Calls `writeFileSecrets` + `writeVariableSecrets` + `writeSecretMap`, mounts secrets dir | Calls `serializeSecrets`, injects env var; no secrets dir mount |
| **sciontool init** | No secret-staging responsibility | Decodes `SCION_STAGED_SECRETS`, writes files with 0600 perms, writes `secrets.json`, unsets env var |
| **K8s runtime** | Already stateless (K8s Secrets API) | No change |

### Serialized format

```json
{
  "file_secrets": [
    {"name": "TLS_CERT", "target": "/etc/ssl/cert.pem", "value": "<base64>"}
  ],
  "variable_secrets": {
    "config": "{\"key\":\"val\"}"
  }
}
```

The outer JSON is base64-encoded for safe transport as an env var value.

### Init sequence ordering

Secret staging runs in `sciontool init` immediately after `setupHostUser()`
resolves the agent home directory and before lifecycle hooks or any child
process. This guarantees secrets are on disk before pre-start hooks, harness
provisioners, or the agent process attempt to read them.

### Security considerations

- The env var is visible in `docker inspect` and `/proc/1/environ` until
  `sciontool init` calls `os.Unsetenv`. This is a brief exposure window,
  mitigated by unsetting immediately after decode.
- File secrets are written with 0600 permissions, matching the previous
  behavior.
- Environment-type secrets continue to use direct `-e` flag injection and are
  unaffected by this change.

### Size limits

A warning is logged if the serialized blob exceeds 100KB. Container runtimes
typically cap combined environment size at ~128KB. In practice, most secret
payloads (API keys, SSH keys, TLS certificates) are well under this limit.

## Scope

### Phase 1 (this PR)

- File secrets via env var (replaces `writeFileSecrets` + `writeSecretMap`)
- Variable secrets via env var (replaces `writeVariableSecrets`)

### Phase 2 (future)

- Auth file staging via env var (refactor `applyResolvedAuth` to eliminate
  remaining host-filesystem dependency for auth files)
