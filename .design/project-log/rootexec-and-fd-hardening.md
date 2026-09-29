# Project Log: Root-context filesystem hardening and generic runtime seams

**Date:** 2026-09-29

## Overview

Hardened every place `sciontool init` and its supervised processes touch a
workload-writable path while running as root, and added a small set of
generic extension points that let a caller run the same init/supervisor/
broker logic without hardcoding a single runtime's assumptions.

## Hardening

- Added `pkg/sciontool/dirfd`: symlink-safe, no-follow filesystem primitives
  (parent-directory-chain resolution, atomic no-follow writes, a root-owned
  directory verifier, and a hard-link-guarded recursive chown/tree walk).
- Added `pkg/sciontool/rootexec`: a fixed-PATH command resolver and static
  guard so a root-context subprocess never resolves a bare command name
  against an inherited, workload-influenceable `PATH`.
- Routed the agent token, GitHub token, log file, scion-env file,
  agent-limits file, shared-workspace gitconfig, and harness exit-code file
  through these primitives, closing symlink, hard-link, and FIFO races a
  root-owned process is otherwise exposed to against a directory the
  workload owns.
- `sciontool init`'s host-user setup, git clone, and shared-workspace git
  configuration now resolve `git`, `iptables`, and `pgrep` by fixed path
  instead of the ambient `PATH`, and `doctor`'s git status check refuses to
  run at all against a workspace it does not own when invoked as root.
- Added a static test (`rootexec.TestNoRootContextExecUsesABareUnresolvedCommandName`)
  that walks every root-context package's `exec.Command`/`exec.CommandContext`
  call sites and fails on an unresolved bare command name, with a narrow,
  individually-justified allowlist for the sites that dispatch through an
  already-dropped credential instead.
- Hardened the harness provisioning path's `atomic_write_json` (Python) to
  create its temp file under an open, no-follow directory handle with a
  unique, unpredictable name, refusing a symlink or FIFO planted at either
  the temp or final path.
- Service startup (`pkg/sciontool/services`) now rejects a service `Name`
  that could escape the log directory, bounds its YAML config read, and
  opens its log files through the same no-follow fd helpers; a service
  whose own logs fail to open is dropped and logged without blocking the
  rest of the batch.

## Generic seams

- `InitRunOptions` (exported alongside the newly-exported `RunInit`) gives
  an in-process caller explicit control over termination-signal forwarding,
  port-forward startup, and a generic privilege-drop-hardening flag that
  toggles several of the filesystem helpers above between their historical
  path-based behaviour and the hardened, fd-based one — with no behaviour
  change for the existing `sciontool init` CLI entry point.
- Added `pkg/runtime` capability interfaces (`AttachCapableRuntime`,
  `PerProfileInstancesRuntime`) that a `Runtime` implementation may
  optionally satisfy; a runtime that implements neither keeps today's
  defaults.
- `runtimebroker.lookupAgentTarget` unifies the previously separate target
  and runtime/manager resolution used by exec, reset-auth, and stop into a
  single strict pass, so an operation can no longer be dispatched to a
  different backend than the one that produced its target. Broker-reported
  per-profile attach capability now gates `scion attach`/`start -a`/
  `resume -a` before the PTY dial, and a runtime's declined-logs response
  passes through the hub with a fixed, generic message rather than
  whatever text the broker supplied.
