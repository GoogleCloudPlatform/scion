# Project Log: Generic enforced privilege-drop mode

**Date:** 2026-09-29

## Overview

Added an opt-in, generic privilege-drop-enforced mode, keyed only on the
`InitRunOptions.RequirePrivilegeDrop` seam. This mode is exercised entirely
by tests here; no runtime sets it in production in this tree — a runtime
consumer follows separately.

## Enforcement

- `requirePrivilegeDropOrFail` (init.go) fails `RunInit` closed
  (`exitCodePrivilegeDropRequired`) when `RequirePrivilegeDrop` is set and
  `setupHostUser`'s result shows the privilege drop did not actually
  happen, instead of silently continuing to run the harness as root.
- `setupHostUser`/`adjustScionUser` gained a `requirePrivilegeDrop`
  parameter: under it, a "scion user not found", a passwd-rewrite that
  matched nothing, or a post-adjust verify mismatch all return a fail
  value instead of the historical "report success anyway" fallback.
  `directSetUID` was split into `directSetUIDAt` (file paths as
  parameters) plus `passwdEntryExists`/`errPasswdEntryNotRewritten`, so the
  "nothing to rewrite" detection is unit-testable against a temp file.
  Every seam this subsystem needs (`runSetupHostUser`, `runDirectSetUID`,
  `scionUserLookup`, `lookupUserByID`, and the euid/capability/uid-mapped
  checks inside `setupHostUser`) is a package var with a real default, so a
  test can drive the enforcement branches from a non-root process.
- `hooks.DecideExecAsRoot` + `LifecycleManager.EnforcePrivilegeDrop`: when
  set, a hook script runs as root only if it and every directory in its
  chain up to "/" are root-owned and not group- or world-writable;
  otherwise it runs dropped to `WorkloadUID`/`WorkloadGID`. Applies to
  every lifecycle event uniformly, with no per-event exception. The
  harness-provision wrapper is always run dropped, even when classified
  `asRoot`, since being trusted, broker-delivered content proves it is
  genuine, not that what it execs is safe to run as root.
- `supervisor.Supervisor.Run` hard-errors (`ErrPrivilegeDropRequired`)
  rather than starting the child with no `Credential` when
  `Config.RequirePrivilegeDrop` is set but UID/GID do not both pass the
  credential-drop predicate — belt and suspenders against a caller that
  builds a `Config` directly, bypassing `requirePrivilegeDropOrFail`'s
  clamp one layer up.
- `hub.EnforceTokenFileOwnerChecks` gates an additional owner check
  (root or the containing directory's own owner) on top of
  `ChownTokenFile`/`readTokenFileGuarded`'s always-on regular-file/
  `Nlink==1` checks. Left off by default so a caller that hands the
  container a host-written token file whose owner isn't provably root or
  the target uid keeps working unchanged.

## Hermeticity

Added `TestMain` for `cmd/sciontool/commands`: clears every Hub/token/
agent-identity env var and `SCION_HOST_UID`/`GID`/`SCION_KEEPID_UID` for
the whole test binary, stubs `scionUserLookup`/`lookupUserByID` to refuse
by default, redirects `HOME` and the XDG base directories to a per-binary
sandbox (while re-exporting the real `GOCACHE`/`GOMODCACHE`/`GOPATH` first
so a child `go` process keeps using the real caches), stubs the reaper
start, and points `hooks.PrivateRootTmpDir` at a throwaway, self-owned
chain.
