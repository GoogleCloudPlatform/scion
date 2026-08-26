# P3: Run, Delete, List Implementation

**Date:** 2026-08-25
**Branch:** `scion/p3-run-delete-list`
**Base:** `scion/sn-impl-em` (integration branch with P0, P1, P2)

## Summary

Implemented the core lifecycle methods (`Run`, `Delete`, `Stop`, `List`,
`GetWorkspacePath`, `Sync`) of `CloudRunSandboxRuntime`, along with a local
state store and watcher goroutine. This is the first end-to-end agent start
path on the sandbox runtime.

## Key Design Decisions

### Single mount at /scion (§3.2a correction)

All writable sandbox paths are organized under `/scion/`:

```
/scion/agents/<slug>/home       — agent home (agent-info.json, logs)
/scion/agents/<slug>/workspace  — workspace
/scion/agents/<slug>/tmux       — TMUX_TMPDIR socket directory
/scion/shared/<name>            — shared directories
```

One `--mount type=bind,source=/scion,destination=/scion` covers everything.
This is necessary because `--rootfs /` is read-only; writes to unmounted
paths vanish into a private overlay the launcher never sees.

### Environment via /usr/bin/env (not --env flags)

The sandbox CLI declares `--env` as `string` (not `stringArray`), so
repeating the flag may overwrite rather than accumulate. Environment is
injected via `/usr/bin/env KEY=VAL ... sciontool init -- sh -c '<tmux>'`.

### §9.1b ExitCode stopgap

`List()` reports `ExitCode=nil` for all sandboxes (running or stopped).
Non-zero ExitCode is hardcoded to PhaseError at `handlers_runtime_brokers.go:719`,
and Instance teardown SIGKILLs every sandbox — reporting the real code would
put the fleet into PhaseError on every normal teardown. The state store tracks
the real exit code internally for when #1260 merges.

### Sandbox binary status

The `sandbox` binary at `/usr/local/gcp/bin/sandbox` may not exist on
current Cloud Run Instances (AC-0 spike finding). The implementation
isolates binary invocation to `runSimpleCommand` calls that can be swapped.
The architecture (state store, /scion layout, env building, sciontool init,
tmux setup) is valid regardless of the isolation mechanism.

## Files Changed

- `pkg/runtime/cloudrun_sandbox_runtime.go` — Full P3 implementation
- `pkg/runtime/cloudrun_sandbox_runtime_test.go` — Comprehensive tests
- `pkg/runtime/factory.go` — Pass config to constructor

## Deliverables

- [x] State store: JSON file, thread-safe, CRUD + reconciliation
- [x] Run: sandbox command with mounts, env, sciontool init + tmux, state recording, watcher goroutine
- [x] Delete: sandbox delete with --force escalation
- [x] Stop: graceful delete (no --force)
- [x] List: state store entries, label filter, §9.1b stopgap comment
- [x] GetWorkspacePath: returns /scion workspace path from state store
- [x] Sync: no-op (filesystem shared via bind mounts)
- [x] Constructor accepts V1CloudRunSandboxConfig
- [x] Factory passes config
- [x] C7 warning for vertex-ai/gcloud-adc auth modes
- [x] Tests: state store CRUD, sanitize, mount, env, entrypoint, layout, List, Run, Delete, Stop
- [x] All existing tests pass
