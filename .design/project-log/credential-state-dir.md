# Credential state published outside /run/scion

**Date:** 2026-11-05
**Issue:** Addresses ptone/scion#7286

## Problem
On Cloud Run, the runtime creates `/run/scion` as the parent of an in-memory volume mount, owned by another uid with a mode that init's checks refuse. sciontool init publishes its credential state only when every directory above the file is root-owned and not writable by others, so the publish failed at every start.

## Change
`credentialStateDir` in `pkg/sciontool/hub/credential_state.go` is `/run/sciontool`. Init creates it as root with mode 0755 below the checked `/run`, using the existing create path. The checks are the same. The only reader, `ReadCredentialState` (used by `sciontool doctor`), follows the same constant.

## Notes
- Kubernetes agent pods run init as a non-root uid, so the state is not published there. This change does not affect that.
- `pkg/sciontool/hub` is in the Windows build of `./cmd/scion`. The non-unix publish file is not modified and fails closed.
- Out of scope: the publish log line says doctor will report the state "unavailable", although an ownership failure reads as untrusted.
