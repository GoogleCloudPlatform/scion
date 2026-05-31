# Cleanup: Align server mode vocabulary (production → hosted)

**Date:** 2026-05-31
**Issue:** #92
**Branch:** scion/cleanup-server-mode-vocab

## Summary

Renamed the `scion server` command group's non-workstation mode from "production" to "hosted" to align with the canonical vocabulary:

- **Workstation mode** — single-tenant, loopback, all components enabled
- **Hosted mode** — multi-user deployment, explicit component selection

## Changes

### Flag Rename
- `--production` → `--hosted` (both `server start` and `server install`)
- `--production` kept as a deprecated alias via `cobra.MarkDeprecated`

### Variable Rename
- `productionMode` → `hostedMode` (package-level in `cmd/`)
- `serverInstallProduction` → `serverInstallHosted`

### Help Text
- "Production mode" → "Hosted mode" throughout
- "local server" → "workstation server" in `serverStartCmd` long description
- Install descriptions: "Scion Server (Production)" → "Scion Server (Hosted)"

### Config Layer
- Comments updated: `"workstation" (default) or "hosted"` with backward-compat note
- `LoadServerMode()` normalizes legacy `"production"` value to `"hosted"`
- Config reconciliation accepts both `"hosted"` and `"production"` for backward compatibility

### Scope Boundaries
- `pkg/hub/auth.go` `AuthConfig.Mode` left unchanged — it describes authentication mode ("production"/"development"/"testing"), a different concept
- "production" in non-mode contexts (deployment environment labels, k8s namespaces, "not for production use" warnings) left unchanged

## Testing

All existing tests updated and passing. Added backward-compatibility test for legacy `mode: production` in settings.yaml.
