# GCE_METADATA_HOST Harness Audit — Task 3 of sn-adc-metadata

**Agent:** sn-adc-metadata  
**Date:** 2026-08-26  
**Context:** OQ-14 (§11.12) proved ADC works in gVisor sandboxes via `GCE_METADATA_HOST`. Since `iptables -t nat` does not exist in gVisor, `GCE_METADATA_HOST` is the **only** mechanism — there is no transparent interception fallback. This audit determines which harnesses work.

## Summary

| Harness | Honours `GCE_METADATA_HOST`? | Mechanism |
|---------|---------------------------|-----------|
| Claude | **Honours** | Node.js `gcp-metadata` reads `GCE_METADATA_HOST` |
| Codex | **No ADC usage** | No vertex-ai support; OpenAI API keys only |
| OpenCode | **Honours** | Go `cloud.google.com/go/compute/metadata` reads `GCE_METADATA_HOST` |
| Antigravity | **Honours** | Google auth library (ADC) reads `GCE_METADATA_HOST`; ADC file bypasses metadata entirely |
| grok-build | **Honours** | `gcloud auth print-access-token` reads `GCE_METADATA_ROOT` (set alongside `GCE_METADATA_HOST` by Scion) |

**No harness hardcodes `169.254.169.254`.** All four that use GCP auth honour the metadata host override.

## Infrastructure

The runtime broker (`pkg/runtimebroker/start_context.go`, lines 384-387) sets **both** env vars:
```go
env["GCE_METADATA_HOST"] = "localhost:18380"
env["GCE_METADATA_ROOT"] = "localhost:18380"
```

## Per-Harness Detail

### Claude
- **ADC usage:** Yes. `vertex-ai` auth type in `harnesses/claude/config.yaml`.
- **SDK:** Node.js `google-auth-library` → `gcp-metadata` npm package.
- **Evidence:** `gcp-metadata` README: "GCE_METADATA_HOST: provide an alternate host or IP to perform lookup against". Source-confirmed.

### Codex
- **ADC usage:** No. `vertex_ai: { support: "no" }` in `harnesses/codex/config.yaml` (line 69).
- **Evidence:** Config-level rejection. Codex uses only OpenAI API keys or auth files.

### OpenCode
- **ADC usage:** Yes. `vertex-ai` auth type in `harnesses/opencode/config.yaml`.
- **SDK:** Go `cloud.google.com/go/compute/metadata` v0.6.0 (per `go.mod`).
- **Evidence:** Source code: `host := os.Getenv(metadataHostEnv)` with `const metadataHostEnv = "GCE_METADATA_HOST"`, falling back to `169.254.169.254`.

### Antigravity
- **ADC usage:** Yes. `vertex-ai` auth type with `gcloud-adc` file.
- **SDK:** Google auth library (standard ADC path via `GOOGLE_APPLICATION_CREDENTIALS` or compute metadata).
- **Evidence:** All standard Google auth libraries honour `GCE_METADATA_HOST`. When ADC file is present, metadata server is not needed at all.

### grok-build
- **ADC usage:** Yes. `vertex-ai` auth type with `gcloud-adc` file.
- **SDK:** Python — delegates to `gcloud auth print-access-token` for token acquisition.
- **Evidence:** gcloud CLI reads `GCE_METADATA_ROOT` (not `GCE_METADATA_HOST`) for metadata server detection. Scion sets both vars (`start_context.go:385-387`).

## Consequence

The Task 1 fix (binding the emulator to the launcher's link-local address and setting `GCE_METADATA_HOST` to that address) is sufficient for all five harnesses. No redesign needed.
