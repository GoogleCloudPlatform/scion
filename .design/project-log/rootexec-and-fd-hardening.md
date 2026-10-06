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
  agent-limits file, and harness exit-code file through these primitives,
  closing symlink, hard-link, and FIFO races a root-owned process is
  otherwise exposed to against a directory the workload owns.
  Shared-workspace `git config` runs as the workload identity
  (`SysProcAttr.Credential`), refusing outright when privilege drop is
  required but no usable uid/gid exists.
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
  and runtime/manager resolution used by exec, reset-auth, stop, and restart
  into a single strict pass, so an operation can no longer be dispatched to a
  different backend than the one that produced its target. The result also
  still carries the container runtime's own unmerged lifecycle `Phase`
  (never `agent.Manager`'s merged, potentially stale view) alongside the
  paired `Runtime` instance, so neither property was traded for the other.
  Every one of these handlers now classifies a lookup failure the same way:
  a runtime-listing failure (`ErrAgentListUnavailable`) gets a fixed 503 via
  `AgentLookupUnavailable`; any other real lookup error (e.g. an ambiguous
  multi-container match) gets a fixed 500, with the underlying error logged
  server-side only, never echoed into the response body; a genuine
  not-found gets a 404 from exec and reset-auth, an idempotent 202 from
  stop, and a fresh start from restart. Broker-reported per-profile attach
  capability now gates `scion attach`/`start -a`/`resume -a` before the PTY
  dial, and a runtime's declined-logs response passes through the hub with
  a fixed, generic message rather than whatever text the broker supplied.

## Attach-refusal surfacing

- `attachSupportedByBroker` no longer defaults to "supported" when the
  runtime broker's point-GET fails. It falls back to matching the same
  broker by ID in the `RuntimeBrokers().List` response. It refuses before
  dialing (fixed message, no raw server text) only when a record read by
  either path says attach is unsupported; when neither read produces the
  record, the CLI proceeds to dial and the broker's own gate (4501 close /
  501 `runtime_attach_unsupported`) stays authoritative. The LIST fallback's pagination
  has a page cap and stops on a repeated cursor, so a misbehaving response
  can't turn one attach call into an unbounded loop; the point-GET and LIST
  errors discarded from the user-facing message are debug-logged for
  diagnosability.
- Added `ClosePTYAttachUnsupported` (4501) / `attach_unsupported`, a close
  code distinct from the retriable 4503/session_not_ready it used to share
  with an actual readiness failure. Mirrored into the close-code parity
  fixture and the TypeScript client. The control-channel pre-check that
  refuses a stream before starting the tmux exec now emits
  4501/attach_unsupported; the direct-connect pre-upgrade path keeps its
  existing 501/runtime_attach_unsupported HTTP response, since no
  WebSocket has been upgraded yet at that point. A 4501 close arriving
  after the WebSocket is already upgraded now maps to an explicit
  "attach is not supported for this agent's runtime" error with a
  non-zero exit, instead of a raw close-code error string.
- The hub's "PTY session started"/"ended" log lines now carry
  `routed_broker_id` (the broker the stream is actually routed to via
  OpenStream) as its own field, distinct from the process-wide `broker_id`
  attr a combo-mode server attaches to every log line (which names the
  locally co-located broker instead).
- `PTYClient` reads stdin through an injected field, captured once at
  construction, instead of the shared `os.Stdin` package variable — a
  leaked reader goroutine touching that global under test raced a later
  reassignment. Separately: a non-TTY stdin already at EOF is treated by
  the CLI's own read loop as a clean detach, so it sends its own
  normal-closure frame and exits 0 before any broker rejection can
  arrive — a client-side close, not a broker defect.
