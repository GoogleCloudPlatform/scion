# Project Log: Substrate runtime backend

**Date:** 2026-09-29

## Overview

Added `substrate`, a Kubernetes-hosted actor runtime backend: agents run as
OCI containers in gVisor/microVM sandboxes on a shared worker pool, addressed
through an in-cluster control API (`third_party/ateapipb`, generated from the
upstream `ateapi` proto) and inbound router. See
`.design/kubernetes/substrate-runtime.md` for the full architecture reference
and `deploy/substrate/README.md` for the deployment manifests and operational
procedure.

## Runtime

- `pkg/runtime.SubstrateRuntime` implements the `Runtime` interface, backed
  by `pkg/runtime/substrate`'s gRPC dialer, identity, egress, and router
  client. Registered in `pkg/runtime/factory.go` behind an explicit
  `"substrate"` profile type — no auto-detect branch, since substrate is
  only ever selected explicitly.
- `pkg/config.V1SubstrateConfig` adds the schema surface
  (`pkg/config/schemas/settings-v1.schema.json`) for a substrate broker
  profile: sandbox class, egress allowlist, and trust bundle source.
- `pkg/substrateenv` and the substrate bootstrap/template files
  (`pkg/runtime/substrate_bootstrap.go`, `substrate_template.go`) build the
  actor's environment and staged files from the broker profile and the
  harness's own requirements.

## In-actor control server

- `cmd/sciontool/commands/substrate_serve.go` and `pkg/sciontool/substrate`
  add `sciontool substrate-serve`, the control server that runs inside the
  actor: it receives the bootstrap payload (env, files, start command), runs
  `RunInit` with `RequirePrivilegeDrop: true` (substrate must never run the
  harness as root — see `requirePrivilegeDropOrFail`'s doc comment), and
  exposes health/logs endpoints back through the router.
- `cmd/sciontool/commands/substrate_rootfs.go` hardens the actor's
  sudo/entrypoint path: a shared root-executable search list with init.go,
  and rootfs ownership fixups routed through the same no-follow, fd-based
  primitives the rest of the hardening work uses.
- `pkg/substratecaps` and `substrate_privilege_drop.go` /
  `substrate_enforced_hooks.go` probe the actor's actual capability set
  (`/proc/self/status`) before claiming a privilege drop is feasible, so a
  misconfigured capability set fails the bootstrap closed rather than
  starting the harness as root.
- `InitRunOptions.WorkingDir`/`ResolveWorkingDir` let substrate-serve resolve
  the harness's working directory after the workspace is prepared, instead
  of inheriting the control server's own cwd; threaded through
  `supervisor.Config.WorkingDir` into the child's `cmd.Dir` and `PWD`.

## Runtime broker integration

- The broker's restart/stop/delete paths gain a substrate-specific,
  fail-closed branch: an ambiguous or unresolvable actor lookup refuses
  rather than guessing, and a restart across a suspend/resume boundary
  dedupes against a concurrent request for the same actor.
- `GetLogs` fails closed with a fixed `ErrLogsNotSupported` sentinel when the
  actor's logs aren't reachable, rather than surfacing raw transport errors
  to the hub.

## Deployment

- `deploy/substrate/` ships the Kubernetes manifests (broker deployment,
  network policy, worker pool) and `OPERATIONS.md` for apply order and
  verification commands.
- `image-build/{omni,scion-base}/Dockerfile` add the packages and entrypoint
  wiring `sciontool substrate-serve` needs in the actor image.
