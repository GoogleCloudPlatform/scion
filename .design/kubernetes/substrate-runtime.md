# Substrate Runtime

This is the in-repo design reference for the `substrate` runtime backend, as
built. It documents what the code in `pkg/runtime` (the `SubstrateRuntime`),
`pkg/runtime/substrate` (the ateapi dialer and router client),
`pkg/sciontool/substrate` and `cmd/sciontool/commands` (the in-actor
`sciontool substrate-serve` control server), `pkg/substratecaps`,
`pkg/substrateenv`, `pkg/config` (`V1SubstrateConfig`), and
`deploy/substrate/` actually rely on. Section numbers below are stable
citation anchors (`substrate-runtime.md §N`); when a section is split further,
the subsection number (`§N.M`) is also stable. Extend this document by adding
sections, not by renumbering existing ones.

Deployment-manifest specifics (placeholders, RBAC, apply order, verification
commands) live in `deploy/substrate/README.md`, not here; this document is
cited from there for the parts that are actually code behavior rather than
operational procedure.

## 1. Architecture and topology

Substrate is a Kubernetes-hosted actor runtime: **actors** (OCI containers in
gVisor or microVM sandboxes) run on a shared pool of **worker** pods, at most
one actor per worker at a time, and can be suspended to object storage and
resumed onto any free worker. scion maps one agent onto one actor.

```
 hub ──control channel──> scion runtime broker (in-cluster pod)
                              │ gRPC (ServiceAccount token)   │ HTTP, ate-target-actor header
                              ▼                                ▼
                     api.ate-system (ateapi Control)    atenet-router ──> actor: sciontool substrate-serve :80
                                                                            └─ tmux ─ harness (claude, ...)
 actor egress (HTTP(S) only, per-actor EgressPolicy) ──> hub, git host, model API, telemetry
```

- **The broker is the existing scion runtime broker binary**, with the
  `substrate` runtime compiled in, deployed as a Kubernetes Deployment inside
  the Substrate cluster (`deploy/substrate/`). It authenticates to ateapi
  with an in-cluster ServiceAccount token obtained via the Kubernetes
  TokenRequest API (audience `api.ate-system.svc` by default), not a
  projected volume (`pkg/runtime/substrate/dialer.go`).
- **The router is Substrate's own `atenet-router`.** Inbound requests carry
  the header `ate-target-actor: <atespace>/<actor>`
  (`pkg/runtime/substrate/router.go`'s `TargetActorHeader`) to address a
  specific actor. An inbound request to a suspended actor auto-resumes it.
- **The only new code that runs inside the actor is `sciontool substrate-serve`**
  (§5), shipped in the agent image as the template's entrypoint.
- **Why an in-cluster broker, not a remote one:** Substrate has no
  authorization on its control API (`ateapi`) or its inbound router — any
  caller that can reach them controls the whole install. Reaching them from
  outside the cluster through a LoadBalancer/Ingress would expose both
  unauthenticated surfaces beyond the cluster boundary, so the broker (and
  therefore the only holder of ateapi credentials) must run in-cluster. This
  is also why the bootstrap-nonce fallback (§5.2) additionally
  depends on a NetworkPolicy restricting router ingress to the broker
  namespace (`deploy/substrate/README.md`), rather than relying on
  authorization Substrate itself doesn't provide.
- **ateapi client:** `github.com/agent-substrate/substrate/pkg/proto/ateapipb`
  (vendored under `third_party/ateapipb/`, with its license and source SHA
  recorded there, to avoid pulling the upstream module's full transitive
  dependency set into the build).
- **Egress** from the actor is HTTP(S)-only and default-deny without an
  EgressPolicy — see §7. There is no WebSocket egress, so the hub
  port-forward tunnel and `sciontool` autoexpose are both disabled when
  `SCION_RUNTIME=substrate` (`cmd/sciontool/commands/init.go`): starting them
  would just spin retrying against a gateway that always refuses the
  upgrade. This is a known limitation (§11), not a permanent one.

## 2. Settings and profile keys

`V1SubstrateConfig` (`pkg/config/settings_v1.go`, under
`V1RuntimeConfig.Substrate`) holds every Substrate-specific setting:

| Key | Meaning |
|---|---|
| `api_endpoint` | ateapi Control gRPC endpoint, e.g. `api.ate-system.svc:443`. |
| `router_endpoint` | atenet-router inbound endpoint, e.g. `http://atenet-router.ate-system.svc:80`. |
| `token_audience` | Audience for the in-cluster ServiceAccount TokenRequest. Defaults to `api.ate-system.svc`. |
| `ca_file` / `cluster_trust_bundle` | Verify the ateapi server certificate against a PEM file or a `ClusterTrustBundle` (the router client does not use it; `router_endpoint` is plain HTTP today, §10); when both are set, `cluster_trust_bundle` wins. |
| `sandbox_class` | `gvisor` (default) or `microvm`. |
| `sandbox_config_name` | The Substrate `SandboxConfig` CRD instance actor templates reference. |
| `worker_selector` | Copied into the ActorTemplate's `workerSelector`, matched against a `WorkerPool`'s own `metadata.labels` (not a Pod label — see `deploy/substrate/README.md`). |
| `snapshot_storage` | Bucket/prefix for the ActorTemplate's `snapshotsConfig` storage, e.g. `gs://bucket/prefix/`. |
| `egress_allow` | Additional hostnames beyond the hub/git/model/telemetry hosts the runtime always adds (§7). Public FQDNs only — see `ValidateEgressAllow`. |
| `egress_trust_bundle` | Names a Substrate trust bundle to project into the actor for the sdsmint MITM gateway (§7.1). Only `egress-mitm.ate.dev` is accepted. |
| `template_ready_timeout` | Bounds how long `Run` waits for a newly created ActorTemplate to become ready. Go duration string; defaults to 10 minutes. |

**Selection is explicit only.** `pkg/runtime/factory.go`'s `substrate` case
is reachable only when a profile names it; there is no auto-detect branch
(unlike `docker`/`podman`/`cloudrun*`, which Linux auto-detection can
select). A settings
profile is free to name its `substrate` runtime entry anything (e.g.
`substrate-prod`, `substrate-nip`); `SubstrateRuntime.Name()` always reports
the literal `"substrate"` regardless, because that is the value the broker's
substrate-only code paths (e.g. the PTY-attach handling in
`pkg/runtimebroker/pty_handlers.go`) key off. Delete and stop resolution
(§9) does not key off `Name()` at all — it is project-ID-scoped uniformly,
on every runtime.

`NewSubstrateRuntime` memoizes one `*SubstrateRuntime` (and its dialed gRPC
`ClientConn`) per distinct `V1SubstrateConfig` for the life of the broker
process — see the memoization comment on `substrateRuntimesMu`
(`pkg/runtime/substrate_runtime.go`). A CA rotation
behind the same `ca_file`/`cluster_trust_bundle` value does not take effect
until the broker process restarts.

**The runtime block is operator-only.** Every key in the table above —
endpoints, CA/trust-bundle, token audience, `egress_allow`, everything
under a `type: substrate` entry in `runtimes.<name>` — must be defined in
the broker's own operator (global, `~/.scion`) settings, never in a
project's merged settings (an in-repo `.scion/settings.*`, or a hub-managed
project's own settings file). A project may only *select* an
operator-defined substrate profile by name (its own `active_profile`, or
the `profile` a create/start request names); it may never define or
override the runtime block itself. `pkg/runtime.GetRuntime` enforces this
(`ValidateOperatorOnlySubstrateProfile`) by comparing the project-merged
resolution against a `LoadGlobalSettings`-only resolution for the same
name and refusing construction outright — a clear config error, never a
silent merge or a silent fallback — on any mismatch. This exists because
the runtime block crosses the broker's trust boundary into the actor's
bootstrap payload (hub token, resolved secrets) and egress policy: letting
project-merged settings control it would let a repo redirect that payload
to an attacker endpoint or widen egress arbitrarily, the same class of risk
`resolveHubEndpointForCreate` (`pkg/runtimebroker/hubenv.go`) already
refuses for the hub endpoint.

## 3. ActorTemplate mapping

Templates are **generic and content-addressed**, never per-agent: per-agent
configuration (env, secrets, home files) is pushed after the actor starts,
via bootstrap (§5), not baked into the template. This keeps secrets out of
Substrate's Postgres and out of golden snapshots, and lets every agent on the
same image/resources/sandbox share one template.

- **Image must be digest-pinned** (`@sha256:...`). `Run` fails closed
  otherwise (`pkg/runtime/substrate_runtime.go:316`):

  > `substrate: image %q is not pinned by digest (@sha256:...); set a
  > digest image in the agent's template or pass --image (tag resolution
  > is not currently supported)`

  Tag→digest resolution is not currently supported. A substrate profile's own
  `harness_overrides` image pin is **not** sufficient by itself on a
  default install, because agent/template config
  (`finalScionCfg.Image`, `pkg/agent/run.go`'s image resolution) outranks
  it once the harness-config's own image has been copied into the agent's
  persisted config at provisioning time (`pkg/agent/provision.go`). See
  `deploy/substrate/README.md` for the operator-facing setup step this
  requires.
- **Template name** = `scion-` + the first 12 hex characters of
  `sha256(image reference (digest-pinned), sandbox class, sandbox config name, worker_selector,
  snapshot storage, effective resources, snapshot scope, the container's
  added capabilities, substrate-serve entrypoint version)`, plus
  `egress_trust_bundle` appended only when it is non-empty. Same inputs
  always produce the same name, so concurrent `Run`s for the same effective
  template converge on one `CreateActorTemplate` instead of racing to create
  distinct ones. The entrypoint version is folded in so a change to the
  `substrate-serve` wire protocol forces a new template rather than reusing
  golden state built against an older one; resources are resolved against
  `config.BuiltinDefaultResources` before hashing (a nil `*ResourceSpec`
  would otherwise hash as empty and silently reuse a stale golden template
  if the default ever changes). `egress_trust_bundle` is appended as its own
  segment, not a fixed field, precisely so that leaving it empty keeps
  existing golden templates on a plain install byte-identical and reused;
  setting it changes the hash, since `buildActorTemplate`'s output genuinely
  differs (a new `system-info` volume and Env — see §7.1).
- **Contents:** one container running the pinned image, command
  `["sciontool", "substrate-serve"]`, `Env` carrying no secrets and no
  per-agent config ever (only the fixed CA-bundle paths when
  `egress_trust_bundle` is set — see §7.1); cpu/memory limits from
  `cfg.Resources` or runtime defaults; one `durableDir` volume at
  `/workspace`; `snapshotsConfig` (`onPause`/`onCommit` = `DATA`, storage
  from `snapshot_storage`); `sandboxConfig` (the configured sandbox class and
  `SandboxConfig` name); `workerSelector` from config.
- **Creating a template boots a golden actor and snapshots it.**
  `ensureActorTemplate` calls `GetActorTemplate`, and `CreateActorTemplate`
  if missing, then polls `GetActorTemplate` until the golden snapshot is
  ready (bounded by `template_ready_timeout`, default 10 minutes) before
  `Run` proceeds to `CreateActor`. A cold template build can take on the
  order of a minute; see `deploy/substrate/README.md` ("warm the template
  before first use") for why this matters for the hub's dispatch timeout.

## 4. Runtime lifecycle

`SubstrateRuntime` implements `runtime.Runtime`:

| Method | Behaviour |
|---|---|
| `Name()` | Always `"substrate"` (§2). |
| `Run(cfg)` | Steps below. |
| `Delete(id)` | See §9. |
| `Stop(id)` | Same as `Delete`, with a future-work marker: `SuspendActor(DATA)` plus the `$HOME` durableDir layout would keep the workspace and free the worker instead, but that lands with the suspend/resume work described in §11. Faking a "stopped" actor that is still running, or a "durable stop" that discarded the workspace, would both misreport what happened, so `Stop` stays honest by making it `Delete`. |
| `List(labelFilter)` | `ListActors` across all atespaces (paginated), keeping only actors in a `scion-`-prefixed atespace (`substrateAtespacePrefix`) — `List` has no project context to scope the call. Substrate actors carry no labels of their own, so `AgentInfo` for a **record-having** actor (one this runtime instance has an in-memory record for, from its own `Run`, keyed by actor UID) reports the real agent slug as `Name`/`"scion.name"`. A **record-less** actor (no in-memory record — e.g. right after a broker restart) is reported under its raw, project-prefixed actor name instead, deliberately: it is never resolvable by a caller-supplied slug or project filter at all, so it can appear in an unfiltered listing without ever being actioned by the wrong caller (§9, §10). When a `"scion.name"` filter has no project-scoping key alongside it and more than one record-having actor shares that slug, `List` excludes all of them rather than guessing (§9). |
| `GetLogs(id)` | Returns `ErrLogsNotSupported` and makes no ateapi or Kubernetes call. Worker pods are shared across atespaces and users; reading one at the pod level would return other tenants' actor output — and any worker-level lines naming other atespaces — alongside the caller's own, so this is not currently supported. See `deploy/substrate/README.md`, "Known limitations" → "Logs", for the operator-facing `kubectl` alternative. |
| `Exec(id, argv)` | `POST /scion/v1/exec` via the router, authorized with the `control_token` minted at bootstrap (§5.3), header `ate-target-actor: <atespace>/<actor>` (§1). Returns stdout; a non-zero exit becomes an error that includes stderr. |
| `Attach`, `Sync`, `GetWorkspacePath` | Return errors stating they are not currently supported / naming the hub workspace API. Substrate has no exec/attach/TTY primitive, declared via the optional `runtime.AttachCapableRuntime` capability (`SupportsAttach() bool`, returning `false`; see `pkg/runtime/capabilities.go`). Every caller that would otherwise reach this checks that capability before dialing, but each only ever holds what it has on hand: the broker's own direct-connect PTY handler (`handleAgentAttach`, `pty_handlers.go`) and its control-channel counterpart (`handlePTYStream`, `controlchannel.go`) each already resolved a live runtime instance for the agent (`AgentLookupResult.Runtime`) and ask it directly via `scionrt.HasAttachSupport`, rejecting before any WebSocket upgrade or tmux exec. The CLI never holds a live instance at all — it asks the broker's own advertised metadata instead: `cmd/attach.go`'s `attachSupportedByBroker` reads the agent's runtime broker record from the Hub (the broker's registered/heartbeat-reported `Profiles[].Attach`), falling back to the broker-wide `Capabilities.Attach` whenever the matched profile said nothing (an older broker, or a registration producer with no live instance to ask about a non-default-type profile — see `buildBrokerProfiles`, `cmd/broker.go`) — and that broker-wide answer is final, not a second missing-⇒-supported default: an explicit broker-wide `false` is treated as real information even when the specific profile itself was silent. The accepted cost is that a non-default-type profile on a broker whose default runtime does not support attach is refused before dialing even if that specific profile's own runtime would have been fine with it — there is no live instance to ask that profile directly at registration time, and an unreadable or silent record refuses, never assumes supported. `attachUnsupportedErr` turns an unsupported answer into a fixed error ("attach is not supported for agents on the substrate runtime") and a non-zero CLI exit — the same way it already does for managed agents. This covers every CLI entry point that can reach the WebSocket dial: `scion attach` directly, and `scion start -a` / `scion resume -a`, which attach immediately after starting or resuming an agent. The web UI terminal is a separate client and still gets only a post-upgrade close today; a hub-side pre-upgrade rejection there is out of scope. |
| `ImageExists` / `PullImage` / `ImageID` / `RemoveImage` | `true` / no-op / return the digest / no-op — Substrate pulls the image itself from the template. |
| `ExecUser()` | `"scion"`. |

**`Run(cfg)` steps:**

1. Compute `atespace = "scion-" + first 12 chars of cfg.ProjectID`
   (sanitised to a valid Kubernetes short name) and `actor = cfg.Name`
   (already DNS-safe). `CreateAtespace` if missing, treating `AlreadyExists`
   as success.
2. Verify `cfg.Image` is digest-pinned (§3); fail closed otherwise.
3. Ensure the content-addressed ActorTemplate exists and its golden snapshot
   is ready (§3).
4. `CreateActor(atespace/name, template)`. An existing actor with the same
   name surfaces as `agent.ErrContainerNameInUse`.
5. `CreateActorEgressPolicy` (§7).
6. `ResumeActor`, then poll `GetActor` until `RUNNING` (bounded).
7. Poll `GET /scion/v1/healthz` via the router until `awaiting-bootstrap`
   (bounded backoff).
8. `POST /scion/v1/bootstrap`: env — see §5.3; files = the composed home +
   `ResolvedAuth.Files` + file-type secrets (§5.4); `start_cmd` built by the
   same shared helper the docker/k8s runtimes use. Generate a random 32-byte
   hex `control_token` and keep it in memory, keyed by `<atespace>/<actor>`.
9. Return `<atespace>/<actor>` as the runtime ID (the same string
   `splitSubstrateID` parses back into atespace/actor for every later call).

**Cleanup on failure:** if any step after `CreateActor` fails, `Run` deletes
the actor and the egress policy (best effort) before returning the error.

**Error hygiene:** see §6.

## 5. Bootstrap protocol and the in-actor control server

`sciontool substrate-serve` (`cmd/sciontool/commands/substrate_serve.go`,
package `pkg/sciontool/substrate/`) is the ActorTemplate's entrypoint. It
listens on `:80`, the port the router targets by default, and is compiled
into the same `sciontool` binary as every other subcommand.

### 5.1 Endpoints

- `GET /scion/v1/healthz` — no auth. Always HTTP 200 for `GET` (`405` for any
  other method); the body's `state` field
  is one of `awaiting-bootstrap`, `running`, or `init-failed` (entered when
  the in-process init exits non-zero after a successful bootstrap; the
  control server stays up regardless — see §10). Never returns data.
- `POST /scion/v1/bootstrap` — accepted once per process lifetime, only in
  `awaiting-bootstrap`. Auth: the nonce (§5.2). Effect: write the files
  (§5.4), set the env, re-run the rootfs fixup (`fixupRootfsForScion`, a
  normally-no-op safety net — traversability and home ownership must hold
  before the next step), run the synchronous privilege-drop precondition
  check, and run the **existing** `sciontool init` path in-process (the same
  `InitRunner` seam the cmd layer already has, reused rather than forked).
  The server keeps serving afterwards. Responses: `409` if already
  bootstrapped, `401` on a missing/empty bearer (or, with a real verifier, a
  wrong one), `400` for an undecodable request body, `422` for a rejected
  file path (§5.5), `413` over the size cap (§5.4), `500` if a bootstrap file
  fails to write or the privilege-drop precondition fails
  (`privilegeDropPreconditionFailedMsg`).
- `POST /scion/v1/exec` — auth: `Bearer <control_token>` from bootstrap.
  Body `{"argv":[...], "user":"scion", "timeout_s":N}` (`"root"` is refused;
  the endpoint only ever runs the agent workload), response
  `{"stdout":"...","stderr":"...","exit_code":N}`. Output is capped at 4 MiB
  per stream (truncated, with a flag on the response). Runs as the scion
  user via a direct `syscall.Credential` drop (`runExec`), never `su`.

Not currently supported: `/pty`, `/rehydrate`, `/tunnel/open`.

### 5.2 Nonce

`bootstrapNonce` is the single call site for the bootstrap request's bearer
value, so the broker only ever has one place to change how it is derived.
Two options exist:

- **Identity-derived** (not currently used): a bootstrap nonce minted via
  `MintActorJWT` for the actor's own identity, verifiable only by the broker
  and control plane. `StaticNonceVerifier` compares a presented bearer
  against a fixed expected value in constant time, and stands in for this
  once the identity-derived path is wired up.
- **Fallback, currently in use: `FirstBootstrapWinsVerifier`.** The control
  server accepts any **non-empty** bearer token (a missing or empty
  `Authorization: Bearer` header is rejected with `401` before the verifier
  is consulted — `bearerToken`), but the single-shot "first bootstrap wins"
  check (§5.1, `409` on a second attempt) is the actual guard. This is
  acceptable only together with a NetworkPolicy
  restricting router ingress to the broker namespace
  (`deploy/substrate/README.md`) — the race window is otherwise any in-cluster
  caller able to reach the router before the broker's own bootstrap call.
  **A `409` from `/bootstrap` is treated as a compromise:** the broker
  deletes the actor and fails `Run` with a clear error.

Threat model for `egress_allow` validation (§7) ties back to this: Substrate's
egress default-deny plus the actor's own EgressPolicy is what keeps an actor
off the router and other in-cluster services in the first place; an
`egress_allow` entry that reached either would defeat the fallback nonce's
NetworkPolicy assumption, which is why `V1SubstrateConfig.Validate` rejects
CIDRs, catch-alls and internal-shaped hostnames outright.

### 5.3 Bootstrap payload

```jsonc
{
  "env": {"K": "V", ...},
  "files": [{"path": "/home/scion/.claude/...", "mode": 384, "content_b64": "..."}],
  "start_cmd": "tmux new-session ... ",
  "control_token": "<random 32B hex>"
}
```

- `env` = `cfg.Harness.GetEnv()` (when a harness is set) + `GetTelemetryEnv()`
  when telemetry is enabled (the `cfg.TelemetryEnabled` gate in
  `buildBootstrapEnv`), since every other runtime includes these and
  the harness needs the model, task and telemetry env they carry to start at
  all, + `cfg.Env` + `ResolvedAuth.EnvVars` + env-type `ResolvedSecrets`, plus
  `SCION_RUNTIME=substrate` (which is what lets `sciontool` disable
  autoexpose and the port-forward tunnel — §1) and
  `SCION_HOST_UID`/`SCION_HOST_GID`, both fixed at `1000` (the actor image's
  built-in `scion` user), so `sciontool init`'s privilege-drop path has a
  target UID/GID to drop to — Substrate's workspace is never bind-mounted
  from the broker's own filesystem, so there is no host identity to mirror
  the way Docker/Podman do (the NFS backend instead uses a stable,
  node-independent identity of its own — the operator-configurable
  `server.workspace_storage.nfs.uid`/`gid` settings, default `1000:1000`, carried
  through as the `NFSUID`/`NFSGID` fields on `RunConfig` — rather than
  mirroring host identity).
- `mode` is decimal (384 decimal == 0600 octal). Auth files and file-type
  secrets always get `0600`, since neither `api.FileMapping` nor
  `api.ResolvedSecret` carries a mode; home files keep their source
  permission bits (§5.4) — except a source with permission bits `0000`,
  where the broker sends `mode: 0` and serve-side `writeBootstrapFile`
  substitutes its own `defaultFileMode` (`pkg/sciontool/substrate/server.go`,
  `0o644`) instead of writing an unreadable file. Note the two
  same-named `defaultFileMode` constants differ: the broker's
  (`pkg/runtime/substrate_bootstrap.go`) is `0o600`, serve's is `0o644`.
- `files` — see §5.4 for composition and precedence, §5.5 for path safety.
- `start_cmd` — built by the shared `buildTmuxStartCmd` helper (not
  duplicated a third time), in its `tmuxPollSession` form like Cloud
  Run/-sandbox, since substrate-serve's child has no TTY; k8s and Docker use
  the `tmuxAttachSession` form.
- `control_token` — generated fresh per bootstrap, used as the `/exec`
  bearer (§5.1); it is independent of the bootstrap nonce, which
  `bootstrapNonce` generates separately and which is checked before this
  payload is ever parsed.

`postBootstrap` sends this payload through the router with the nonce as the
bearer. Any non-2xx response is an error; `409` specifically becomes
`errBootstrapHijacked` (§5.2), and `422` becomes a `*bootstrapPathRejectedError`
carrying the same stable code and path the server returned (§5.5).

### 5.4 Home delivery

`buildBootstrapFiles` (`pkg/runtime/substrate_bootstrap.go`) assembles
`files` from three sources, **in precedence order, deduped by final path
with the later source winning**, so exactly one entry per path reaches the
wire:

1. **The broker-composed agent home** (`RunConfig.HomeDir` — harness-config
   `home/`, e.g. `.claude.json`/`.claude/settings.json`, the template home,
   and skills). `homeBootstrapFiles(cfg.HomeDir, containerHome)` walks it
   with `filepath.WalkDir`, **never following a symlink**; regular files
   only (fifos, sockets and devices are skipped and counted, never read);
   mode = the source file's permission bits `& 0o777`. `HomeDir == ""`
   leaves the output unchanged (golden-tested against the pre-existing
   auth+secret-only behavior); a `HomeDir` that doesn't exist is an error.
2. `ResolvedAuth.Files` (read from `SourcePath`).
3. File-type `ResolvedSecrets`.

**This delivery model is deliberately narrower than Docker/Podman or
cloudrun-sandbox, which bind-mount or relocate `HomeDir` directly:**

- **Copy-in, one-way, at bootstrap.** Files are read once and written into
  the actor's filesystem. Nothing the actor writes afterwards is synced back
  to the broker's `HomeDir` — there is no persistent mount or ongoing sync.
- **Additive over the image's home, not a replacement for it.** The actor
  image's own home directory is not cleared or shadowed first; bootstrap
  only adds or overwrites the specific paths it ships.

**Size cap, measured, not assumed.** The router's ingress listener for
`POST /scion/v1/bootstrap` has no request-body cap of its own (no buffer
filter or `max_request_bytes`; the 1 MiB `per_connection_buffer_limit_bytes`
default is a flow-control watermark, not a cap — the body streams through
uninspected, under a 300s route timeout). `sciontool substrate-serve`'s own
handler is therefore the binding limit: `maxBootstrapBodyBytes` = 64 MiB
(`pkg/sciontool/substrate/server.go`), enforced via `http.MaxBytesReader`
(fails the read closed — a bounded error, not unbounded buffering — reported
as `413`). `maxBootstrapFilesTotalBytes` = 16 MiB decoded, across every file
combined after dedup (`pkg/runtime/substrate_bootstrap.go`), is sized with
headroom under that: 16 MiB decoded is ~21.3 MiB once base64-encoded (the
4/3 expansion), leaving roughly 3x headroom under the 64 MiB serve-side
limit even before the surrounding JSON envelope (`env`, `start_cmd`,
`control_token`, per-file path/mode/quoting) is added. The error on overflow
names only the cap and the total size, never a path or content.

**Hygiene:** home file contents (`settings.json` can carry env) are
secret-grade — never logged or put in an error — but ride the same
plaintext broker→router hop auth/secret files already ride (§6, §10); the
exposure is not narrower for home files than for the other two sources.

**Lifecycle hook redirect.** `writeBootstrapFile` chowns every file and
directory it writes to the scion user (§8's home-ownership model) — except
a path under `$HOME/.scion/hooks/`, which it instead redirects to a
dedicated, never-chowned, root-owned directory. See §8's "Lifecycle hook
privilege enforcement" for why.

### 5.5 Path safety: the symlink guard and the agent-home containment check

`writeBootstrapFile` (serve side, `pkg/sciontool/substrate/`) requires every
bootstrap file's target to resolve inside the agent home
(`util.GetHomeDir("scion")`) — except the enforced-hooks redirect target
under `hooks.EnforcedHooksDir`, which is root-owned by design (§5.3) — then
creates parent directories and chowns what it created for the scion user.
An outside-home target (e.g. `/etc/ld.so.preload` paired with a
workload-writable `.so` it names) is a root code-execution primitive, not a
legitimate bootstrap target, and is rejected before anything is created or
written.

- `mkdirAllTracked` Lstats **every existing component** of
  `filepath.Clean(dir)`, top-down from the first component to `dir` itself —
  not just the deepest one that happens to exist. (Stopping at the first
  existing ancestor is not sufficient: it never Lstats anything above that
  point, so a symlinked component whose target already contains the
  remaining subpath would pass unnoticed.) The first symlink found anywhere
  in that walk rejects the whole file, naming only its own path. The first
  missing component starts a suffix created top-down with plain `os.Mkdir`
  (never `os.MkdirAll`, which would reopen the same hole).
- This is a plain existence check followed by a separate create, not one
  atomic operation — safe only because nothing writes concurrently during
  bootstrap: `substrate-serve` is the sole writer, and the harness has not
  started yet. It is **not** a general TOCTOU-safe guarantee.
- The leaf file itself is handled separately: `writeFileAtomicMode`'s
  `os.Rename(tmp, path)` replaces whatever directory entry sits at `path`,
  including a pre-existing symlink, rather than following it — a symlink at
  the leaf is safe by construction.
- `writeBootstrapFile` cleans the bootstrap file's `Path` once
  (`filepath.Clean`) and uses that same cleaned value for both the component
  walk and the final write target, so a `..` segment can never resolve
  differently between the two.

**The symlink guard applies to every bootstrap `Path` that resolves inside
the agent home.** A target under a *system* symlink (e.g. `/var/run ->
/run` on Debian-based images, or a merged-`/usr` layout) is rejected exactly
like a symlinked path under home. **Workaround:** use the resolved form of
the target, e.g. `/run/secrets/...` instead of `/var/run/secrets/...`. No
real target is known to need an exemption (§11).

**Three stable error codes**, carried by a `*bootstrapPathError`
(`pkg/sciontool/substrate/helpers.go`) and returned as `422`:

- `bootstrap_path_symlink` — a path component is a symlink.
- `bootstrap_path_invalid` — an empty/relative path, or a path component
  that exists but is not a directory.
- `bootstrap_path_outside_home` — the target does not resolve inside the
  agent home (and is not the enforced-hooks redirect case). Only the
  target's own leaf component is named, never its full path or content.

The first two carry the rejected file's own `path`, never an internal
ancestor or content. `postBootstrap` (broker side) parses a `422` body into
a `*bootstrapPathRejectedError` with the same code and path, and `Run` logs
both explicitly before returning the redacted error — paths are
configuration and safe to log; file content stays secret-grade and is never
part of this error on either side. No new wire fields exist: the body is
still the pre-existing unstructured string `/bootstrap` has always returned
for every rejection, just with a stable, parseable
`"<code>: bootstrap file <quoted path> rejected: <detail>"` shape for this
class of error.

### 5.6 SIGTERM and eviction

**SIGTERM must not kill the harness.** `substrate-serve` is PID 1 in the
actor. It logs SIGTERM and keeps running — it does not forward
the signal to the harness and does not exit. This matters because Substrate
propagates SIGTERM into the actor's workload when a worker is drained, with
a 30-minute grace period before SIGKILL; forwarding it would kill the agent
on every eviction instead of surviving it. Full eviction handling —
suspending the actor within that grace period rather than just surviving
the signal — is future work (§11).

### 5.7 Harness working directory

The ateapi Container spec has no `workingDir` field, and ateom does not apply
the actor image's own `WORKDIR` — unlike Docker, Podman, Kubernetes, and
Cloud Run, which all honor it. Left unhandled, the harness process tree
(the agent process, its shell, and tmux) starts with its current directory
at `/`, which fails the agent's own folder-trust check (only `/workspace` is
trusted) before it can make its first model call.

`sciontool substrate-serve` supplies `RunInit` a working-directory resolver
(`InitRunOptions.ResolveWorkingDir`) rather than a precomputed value.
`RunInit` calls it directly after the workspace has been cloned and the
post-pre-start-hook ownership fixup has run — the two steps that can change
whether a directory is searchable by the `scion` uid — and before anything
that starts a long-running component on the harness's behalf (sidecar
services, the metadata server, the hub secret fetch) or builds
`supervisor.Run`'s config, threading the result into the same optional
`WorkingDir` field every other runtime's call site leaves unset (so their
behavior is unchanged):

- **Primary candidate:** `SCION_WORKSPACE_PATH`, required absolute, default
  `/workspace`.
- **Fallback:** the actor image's `scion` service-user home directory,
  looked up directly rather than read from `$HOME` — under substrate-serve's
  own still-root process, `$HOME` is `/root` or unset, never the
  dropped-privilege user's home.
- **Fail-closed:** because the chdir happens after the privilege drop, a
  candidate is accepted only if it, and every one of its ancestors
  (including through a symlink target), stats as searchable by the target
  uid/gid — never merely by what root itself can traverse. If neither
  candidate is usable, the harness is never started, and neither are the
  sidecar services, the metadata server, or the hub secret fetch: `RunInit`
  returns `exitCodeNoUsableHarnessCwd` (18), which is logged and reported to
  the Hub through the same init-failure path other init failures use, and
  `/healthz` reflects `init-failed`. The control server
  itself stays up — Substrate does not treat a PID 1 exit as a failure
  signal (§5.1, §10) — so the actor keeps running and holding its worker
  until it is stopped. This replaces a silent fallback to an unset
  `cmd.Dir`, which would have landed the harness back in substrate-serve's
  own root-owned cwd.
- **Never `/`.** A literal `/`, or any candidate that reduces to `/` after
  cleaning or symlink resolution, is rejected outright regardless of its
  permissions.

**Effect on tmux.** `start_cmd` is always a `tmux new-session` invocation
(§5.3); invoked without an explicit `-c` flag, `tmux new-session` defaults
its initial pane's directory to the invoking client's own cwd — i.e.
`cmd.Dir` — so the agent window, the shell window opened alongside it, and a
later `scion attach` all inherit the resolved directory without any change
to the shared tmux-invocation construction other runtimes also use.

`supervisor.Run` also sets the child's `PWD` environment variable whenever
`WorkingDir` is non-empty, so a symlinked workspace's logical path — not its
resolved physical path — stays visible to the shell, tmux, and any process
that reads `PWD` rather than calling `getcwd()`.

## 6. Error hygiene

No secrets — env values, file contents, or auth/secret material — appear in
ActorTemplates, logs, or error strings, on either side of the bootstrap
hop:

- The ActorTemplate's container `Env` never carries secrets or per-agent
  config (§3); only fixed CA-bundle paths when `egress_trust_bundle` is set.
- Bootstrap file content is never logged or put in an error, on either the
  broker or the serve side (§5.4). Rejected-path errors (§5.5) name only the
  path, never content. The size-cap error (§5.4) names only the cap and the
  total size.
- `redactErr` (`pkg/sciontool/substrate/helpers.go`) is the named seam for
  this invariant on the serve side; any new error path added to the server
  must be checked against it.
- **Known residual exposure:** the broker→router hop carries the bootstrap
  payload, including home/auth/secret file contents, in plaintext in the
  current fixture (`router_endpoint: http://...`). This is out of
  scope (§10) and covered by the NetworkPolicy plus first-bootstrap-wins
  nonce (§5.2), not by transport encryption.

## 7. Egress model

`Run` creates a per-actor `EgressPolicy` (step 5, §4) with hostname rules
for:

- the operator-configured hub endpoint host (never a creator/template
  override — see the trusted-hub-endpoint note in §5 or
  `pkg/runtime/interface.go`'s `RunConfig.TrustedHubEndpoint` doc comment);
- the git clone host, and the configured telemetry endpoint host — both
  tenant-derived, and both added **only if an operator `egress_allow` entry
  covers them** (exact match, or a wildcard entry one label above): a
  well-formed public hostname is not by itself trustworthy — a service like
  nip.io/sslip.io resolves an embedded IP octet on request, the cloud
  metadata address included, and a tenant can otherwise simply point their
  git remote or telemetry endpoint at any domain they register. An operator
  who wants an actor's own git clone or telemetry endpoint to actually reach
  the network must list that host (or a covering wildcard) in
  `egress_allow` themselves; an uncovered host is dropped and logged, never
  added;
- the harness model API hosts: `api.anthropic.com` and
  `oauth2.googleapis.com`, hardcoded, plus — only when the agent's resolved
  env indicates Vertex AI auth (`CLAUDE_CODE_USE_VERTEX`,
  `ANTHROPIC_VERTEX_PROJECT_ID`, or `CLOUD_ML_REGION`) — the Vertex endpoint
  for that agent's own configured region (`<region>-aiplatform.googleapis.com`,
  or the bare `aiplatform.googleapis.com` for an empty or `"global"`
  region). **These defaults cover the Claude harness's own Vertex AI
  integration only** (`pkg/harness/container_script_harness.go`'s
  `c.entry.Harness == "claude"` vertex-ai env translation is the sole
  wiring) — a different harness with its own model-API host needs the same
  two-part treatment added for it: a validated, fixed-Google-suffix host
  via the trusted path, or the tenant-host coverage gate, whichever its
  trust shape matches. `CLOUD_ML_REGION` is validated as a single DNS
  label (`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`) before being used to build the
  Vertex host; anything else — notably `*`, which would otherwise build a
  wildcard host — fails Run closed with a config error rather than being
  silently skipped or passed through. `ANTHROPIC_VERTEX_BASE_URL` (a model
  base-URL override an agent/template can set outright, unlike the
  constructed Vertex host) goes through the tenant-host coverage gate, not
  the trusted path. There is deliberately no `*.googleapis.com` wildcard:
  that would cover every Google API (GCS, Compute, BigQuery, and hundreds
  more), not just the ones this runtime actually calls. One consequence:
  GCP's own default Cloud Trace auto-export (`cloudtrace.googleapis.com`),
  which the old wildcard happened to also cover, now fails closed like any
  other uncovered host unless an operator explicitly lists it in
  `egress_allow` — the actual configured telemetry endpoint (via
  `SCION_OTEL_ENDPOINT` or the `OTEL_EXPORTER_OTLP_*` vars) is always
  subject to the tenant-host coverage rule above regardless;
- `egress_allow` entries from settings (§2), added directly (an
  operator-supplied entry is its own coverage).

`egress_allow` is **allowlist-first and validated** (`ValidateEgressAllow`,
`pkg/config/substrate_egress.go`): only public FQDNs (optionally wildcarded
as `*.example.com`) are accepted. Rejected include: any IP address or CIDR;
catch-alls (`all`, `*`, `0.0.0.0/0`, `::/0`); a hostname whose top-level
domain isn't a real, ICANN-delegated one (`egressAllowSuffixOK`; also
catching Kubernetes-internal-shaped names and reserved zones like
`.local`/`.internal`); the special-use `arpa`/`onion` top-level domains; a
hostname that is itself a public suffix rather than a name beneath one;
hostnames ending in `.svc`, `.cluster.local`, `.internal`, `.local`,
`.localhost`, or `localhost.localdomain`; a wildcard whose remainder is
itself a public-suffix wildcard rule (e.g. `*.run.app`); single-label
hostnames; entries over 253 characters; and entries containing `[`, `]`, or
`%`. The same normalization and rejection rules apply to a tenant-derived
host before its coverage is even checked, so a git-clone or telemetry host
that fails this grammar is dropped regardless of `egress_allow`. DNS
rebinding, or a covered hostname later resolving to a private or in-cluster
address, is not closed by any client-side allowlist — only a post-resolution
check by the egress proxy itself could close that, and none is added here.

### 7.1 `egress_trust_bundle`: the sdsmint MITM gateway

Substrate's plain `atenet-egress` enforces `egress_allow` only for TLS
*passthrough* on address rules — every rule `egress_allow` emits is a
HostnameRule, enforced for HTTPS only under the **sdsmint** gateway, which
terminates every TLS connection and re-originates it with a per-SNI leaf
certificate chained to its own CA. An actor validating only public roots
rejects that certificate and every HTTPS request fails.

`egress_trust_bundle: egress-mitm.ate.dev` (the only name the vendored
Substrate version resolves) is required if and only if the cluster runs
sdsmint and the actor makes any HTTPS/TLS request. When set,
`buildActorTemplate` adds a `system-info` volume projecting the gateway's CA
to `/run/ate/trust-bundle.pem`, and sets `NODE_EXTRA_CA_CERTS`,
`GIT_SSL_CAINFO`, `SSL_CERT_FILE`, `CURL_CA_BUNDLE` (all pointing at that
file) and `SSL_CERT_DIR=/run/ate` — the shared variable names live in
`pkg/substrateenv.TrustBundleVarNames`, so `buildActorTemplate` and the
serve-side exec path (§5.1, `runExec`'s own credential-drop environment)
never drift apart. `SSL_CERT_DIR`
is exclusive for Go and Python `ssl`, and only additive for curl, git and
Node (see `deploy/substrate/README.md` for the full breakdown). Leaving
`egress_trust_bundle` empty is byte-identical to today and keeps the
template's content-address unchanged (§3); setting it on a plain
(non-sdsmint) install breaks every actor at start, since nothing backs the
named `ClusterTrustBundle`.

`runExec`'s direct credential drop carries these vars through explicitly
regardless (see §5.1); unlike the login-shell-based mechanism this replaced,
nothing here depends on a particular `su` version being present in the
image.

## 8. Privilege drop and rootfs

Substrate starts the actor process as **UID 0 with a minimal capability
set** (`AUDIT_WRITE`, `KILL`, `NET_BIND_SERVICE`) — the ateapi `Container`
spec has no `user` field, so there is no way to ask Substrate to start the
process as anything else. `sciontool init`'s privilege-drop and rootfs
assumptions were written against Docker's defaults, which don't hold here:

- **`/` is `0700` in the actor** (Substrate `bundle_linux.go`).
  `fixupRootfsForScion` (`cmd/sciontool/commands/substrate_rootfs.go`)
  chmods `/` to `0755`. It runs at `substrate-serve` startup (captured in
  the golden snapshot) and again at `/bootstrap`, and also re-chowns
  root-owned entries under the resolved `scion` user's home
  (`chownTreeRootOwned`) — image-layer files can appear owned by UID 0.
- **`checkPrivilegeDropFeasible`** (`cmd/sciontool/commands/init.go`) is
  `/bootstrap`'s synchronous precondition: it verifies every capability in
  `pkg/substratecaps.Required` is actually effective before `/bootstrap`
  ever responds `200`, so a missing capability fails fast and visibly
  instead of deep inside `RunInit` with a silently truncated log (see the
  next point).
- **Capabilities added beyond Substrate's default set**
  (`pkg/substratecaps.Required`, applied on the ActorTemplate's container
  `SecurityContext` by `buildActorTemplate`, and independently checked by
  `checkPrivilegeDropFeasible` — the two must never drift apart, which is
  why both consumers import the single dependency-free `substratecaps`
  package):

  | Capability | Reason |
  |---|---|
  | `SETUID` | The exec control-plane endpoint's own direct credential drop (`runExec`) and the supervisor's own credential drop both need it to leave root. |
  | `SETGID` | Same two consumers, for `setgroups`/`setgid`. |
  | `CHOWN` | `RunInit` unconditionally chowns the log file and the workspace/home tree from root to the scion user once a drop is expected. |
  | `DAC_OVERRIDE` | `RunInit`'s root phase writes into the scion-owned `0700` home (created by the rootfs fixup) and appends to the log after it has already been chowned to the scion user — without this, root is subject to the same permission check as any other non-owning UID, and (a second, independent effect) log writes after the chown fail silently, dropping every later init log line including the one that would have explained the failure. |

  A direct `syscall.Credential` drop grants the dropped-to process none of
  these, so nothing scion-owned ever runs with them.
- **Non-fatal, observed warnings:** gVisor `--allow-suid` is disabled;
  `RLIMIT_MEMLOCK` gives `EPERM`.

### 8.1 Lifecycle hook privilege enforcement

`sciontool init`'s lifecycle hook scripts (`pre-start`, `post-start`,
`pre-stop`, `session-end` — `pkg/sciontool/hooks/lifecycle.go`) run in the
root init process before any drop, with no ownership check, on every
runtime. On a runtime where root is only ever a container-local convention
that already maps to the unprivileged host user (`sudo` works, or the
runtime's own boundary already applies), that is not a privilege boundary.
On Substrate, `sudo` does not work and root is a real boundary: a workload
that writes an executable `~/.scion/hooks/session-end` (or plants one under
any other event directory) and exits gets it run as root, since
`post-start`/`pre-stop`/`session-end` all fire while or after the harness —
and so the workload — has control of `$HOME`.

**The rule (`InitRunOptions.RequirePrivilegeDrop` — substrate-serve only;
every other runtime keeps its own unrelated behavior unaffected):** for
every event, a hook script runs as root only if the script file *and every
directory from it up to `/`* are owned by uid 0 and are not group- or
world-writable (`pkg/sciontool/hooks.DecideExecAsRoot`, a pure function of
fstat results). Otherwise it runs dropped to the workload uid/gid, with
env/cwd handled the same way the harness child process itself gets them.
There is no per-event exception — pre-start is checked exactly like
session-end, closing the hole even for hooks that only ever fire before the
workload could plant anything, so the rule stays a single, auditable
invariant rather than a list of trusted event names.

**Root hooks after pre-start get a hardened environment and cwd, not the
workload's own HOME or working directory.** A root-eligible hook still
needs *some* environment, and the naive choice — the same `HOME=<agent
home>` a pre-start hook needs to find the harness bundle it staged there —
would, for any later event (post-start/pre-stop/session-end), point a root
process at a directory the workload has had full control of since the
harness started. A root hook that happens to invoke python, bash, git, or
pip would load workload-planted rc/site/config files and run them as root:
the same escalation class this whole mechanism exists to close, reintroduced
through the environment instead of the exec path. So a root-eligible hook at
any event *after* pre-start instead gets `HOME=/root`, `PYTHONNOUSERSITE=1`,
and a fixed, minimal `PATH`
(`pkg/sciontool/hooks.LifecycleManager.hardenedRootHookEnv`) — built from a
closed allowlist of exact variable names (`LANG`, `TERM` — display/locale
hints a hook's own output formatting might consult, never a code or config
search path; deliberately not `TZ`, and not any `SCION_*` variable by name or
by a blanket prefix, since no root-eligible hook today reads either) rather
than inheriting the rest of init's own environment, so an interpreter or
config-loader redirector variable
(`PYTHONPATH`, `BASH_ENV`, `LD_PRELOAD`, `XDG_CONFIG_HOME`, and similarly for
Node/Perl/Ruby/git) pointed at a workload-writable location by harness,
template, or operator configuration cannot reach it either — and `cmd.Dir`
is pinned to `/`, never left to inherit init's own cwd, which (nothing in
`sciontool` ever calls `Chdir`) is whatever the image sets as its `WORKDIR`,
i.e. the workload's own writable git workspace: many interpreters and tools
resolve code relative to cwd (`python3 -c`/`-m` puts `''` first on
`sys.path`, `node -e` reads `./node_modules`, `make` reads `./Makefile`,
dotenv loaders read `./.env`), so leaving it unpinned would reopen the same
class of hole through the working directory instead of the environment. A
hook that genuinely needs the workspace must `cd` there explicitly. Pre-start
is exempt from the `HOME`/cwd hardening (though it does still get
`PYTHONNOUSERSITE=1`, which costs it nothing) because its only remaining
root-eligible hook — a project/hub hook — runs once, before the workload
exists at all, and needs the agent-home `HOME` to find what was staged there
— see §11 for the invariant that exemption depends on.

The container-script harness's own pre-start provisioner is not part of this
exemption at all: `buildEnforcedCmd` recognizes it by its fixed staged
name (`pkg/harness.HarnessProvisionHookFilename`) and runs it under the
workload's own uid/gid instead, with supplementary groups cleared, regardless
of `DecideExecAsRoot`'s own root-eligible classification for it. The wrapper
script itself stays root-owned and trusted — that classification still proves
it is the genuine, broker-delivered file — but what it execs
(`sciontool harness provision`, and in turn the harness's own `provision.py`)
reads and writes `$HOME` and `/workspace`, both fully workload-controlled, so
the "before the workload exists" premise that lets other root-owned hooks
run safely does not apply here: `$HOME` and `/workspace` are exactly the
paths that premise is about, so this step must drop privilege regardless of
it. Because this drop does not depend on $HOME being fresh, it
is unconditionally safe under a resume/re-bootstrap over a persisted `$HOME`
too, unlike the project/hub hook case §11 still tracks.

**No TOCTOU.** The script is opened with `O_NOFOLLOW` at every path
component from `/` down to its own directory, then opened itself with
`O_NOFOLLOW` relative to that already-verified parent fd — never re-resolved
by path. The file `DecideExecAsRoot` inspects is therefore provably the
exact file `exec(2)` runs. A symlinked hook or a symlinked directory
anywhere in the chain is refused outright, never followed and never treated
as "does not exist" (`pkg/sciontool/hooks/exec_enforced.go`).

**Execution.** The script's own open file is opened `O_CLOEXEC` — unlike
every other fd this construction opens on the way there, this one carries a
secret-grade script (`30-project-custom` is `0700` for exactly that reason)
and must never leak into any OTHER, unrelated child this process happens to
fork while it is open. It is handed to the child through
`exec.Cmd.ExtraFiles`, which lands it as a fresh, independently-flagged
duplicate at the fixed descriptor 3 in *that* child only, and the command
execs `/proc/self/fd/3` (the fexecve(3)-equivalent for a language with no
native fexecve) — a magic symlink the kernel resolves directly to that open
file description, not through a further filesystem path lookup, so it runs
exactly the inode that was checked regardless of what (if anything) now sits
at the script's original path. Go's `ExtraFiles` dup clears the close-on-exec
flag on the duplicate independent of the source fd's own, which is what lets
it survive the child's own exec — required for a shebang script, since the
kernel hands the interpreter that same `/proc/self/fd/3` path, which the
interpreter then opens again itself.

One observable difference from the non-enforced exec path: since the script
runs via `/proc/self/fd/3`, a shebang interpreter sees `$0` as that magic
path, not the script's own location — a hook relying on `` `dirname "$0"` ``
would break on substrate only. `SCION_HOOK_PATH` is set in the hook's
environment to the real path as a workaround.

A refused entry (symlink or non-regular file) under a hooks directory other
than `hooks.EnforcedHooksDir` is logged and skipped, not treated as a hard
failure: `DecideExecAsRoot` would drop such an entry anyway, so refusing to
run it is already correct, and letting it abort every later hook for the
same event would let a workload trivially disable its own remaining
post-start/pre-stop/session-end hooks by planting one broken entry ahead of
them. A refusal under `hooks.EnforcedHooksDir` itself still hard-fails the
event — broker-delivered content is never expected to contain one, so its
presence there is treated as an anomaly worth aborting over.

**Where trusted, broker-delivered content lives.** The container-script
harness's pre-start wrapper (`20-harness-provision`) and any project/hub
pre-start hook (`30-project-custom`) are staged host-side under
`$HOME/.scion/hooks/pre-start.d/` and — for every other runtime — bind-
mounted or copied in with that ownership intact. On Substrate, everything
delivered via `POST /scion/v1/bootstrap`'s `files` array is written by
`writeBootstrapFile` and chowned to the scion user (§5.4) — which would make
these two hooks workload-owned by construction, before the ownership check
ever runs, and so always dropped even though they are broker/operator
content, not workload content. `writeBootstrapFile` closes that gap with a
path redirect (`pkg/sciontool/substrate/enforced_hooks.go`): a path under
exactly `$HOME/.scion/hooks/` is rewritten to
`<hooks.EnforcedHooksDir>/<rest>` — a dedicated directory (`/run/scion/hooks`;
a single named constant so the location can change in one place if `/run`'s
own live properties ever require it). Directories along the way are created
by `mkdirAllTracked` (unchanged, shared with every other bootstrap path):
an `Lstat`-then-`os.Mkdir` walk, refusing any existing component that is a
symlink or not a directory, *not* an `openat`/`mkdirat` sequence with
`O_NOFOLLOW` — that is safe under bootstrap's own documented trust model
(substrate-serve is the sole writer, and this all runs before the harness —
and so the workload — ever starts, so there is no concurrent writer to race
a check against a create), the same model the rest of `writeBootstrapFile`
already relies on. Every directory and file this redirect creates is
root-owned (the process is root at this point) and **never chowned**
afterward. Any other bootstrap path is unaffected. `$HOME/.scion/hooks`
itself stays registered with the lifecycle manager and subject to the same
ownership rule, so anything the workload plants there afterward is still,
correctly, dropped.

The redirect directory is cleared (symlink-safe, never following) before
each bootstrap writes into it — and, unlike every other step in this
paragraph, this one is fail-closed by consequence rather than by
construction: `handleBootstrap` treats a clear failure as a bootstrap
failure, aborting before any file is written or init starts, rather than
logging and continuing. That is what actually keeps a hook removed since a
previous bootstrap from surviving into this one — a clear that merely tried
and failed would not.

**Rootfs hardening, and where fail-closed actually rests.** `mkdirAllTracked`
only `chmod`s directories it creates; a pre-existing ancestor of
`hooks.EnforcedHooksDir` (`/`, `/run`) keeps whatever mode/owner it already
had. `fixupEnforcedHooksDirChain`
(`cmd/sciontool/commands/substrate_enforced_hooks.go`), run at the same two
call sites as `fixupRootfsForScion` (substrate-serve startup and the
`/bootstrap` fallback), strips any group/other-write bit and corrects
non-root ownership on every existing ancestor. This fixup is a proactive
correction, not the safety property itself: whether or not it runs or
succeeds, `DecideExecAsRoot` re-checks the real filesystem state at
hook-exec time and is what actually decides root vs. dropped. If a bad mode
or owner survives anyway (e.g. a read-only rootfs), the result is simply a
hook that runs dropped instead of as root — fail-closed, never the reverse.

## 9. Delete semantics

`Delete(id)`: `GetActor` first (best effort, to capture the actor's UID),
then `DeleteActorEgressPolicy` (ignoring `NotFound`), then
`DeleteActor(any_state=true)` (also tolerating `NotFound`), then drop the
in-memory `control_token` for `<atespace>/<actor>` and, when the UID was
captured, `substrateAgentRecords[uid]` too. The ActorTemplate itself is
never deleted by `Delete` — template GC is future work (§11).

**Delete is fire-and-forget.** The broker counts `DeleteActor`
being accepted (not erroring) as success; it does not confirm the actor
actually leaves `DELETING`. A stuck actor is invisible to the hub and
silently holds a worker (§10). Confirming deletion (poll until the actor
leaves `DELETING`, or report a stuck state) is future work (§11).

**Same-slug, cross-project safety.** `resolveDeleteTarget` and
`projectScopedTarget` (`pkg/runtimebroker/handlers.go`) resolve delete and
stop, respectively, to a single project-matched entry before acting, on
every runtime — not a substrate-specific mechanism:

- **`resolveDeleteTarget`** collects candidates via `Runtime.List` with the
  requested project ID included in the filter itself (across the default
  and every auxiliary runtime), so a project-scoped `List` call returns
  only that project's entries in the first place — `List`'s own
  same-slug ambiguity guard (§4) never has a reason to engage here, since
  the filter already carries a project-scoping key. The resolved entry's
  `ContainerID` (for substrate, the globally-unique `<atespace>/<actor>`
  pair) is what `Manager.DeleteTarget` acts on directly, so the
  runtime-level delete never lists by bare slug again either. No match in
  the requested project — after also checking a legacy no-project-identity
  fallback and the project's own file-only directory — is
  `errDeleteTargetNotFound`, answered as `404`, never a project-blind
  guess. `projectScopedTarget` gives `stopAgent` the same property via
  `LookupContainerID` (which uses the same project-scoped filter,
  `scopedNameFilter`), resolving to a container ID before calling
  `Manager.Stop`.
- **Record-less actors are never resolvable by slug or project filter, at
  all** — not even by a request scoped to their own project. `List` (§4)
  reports a record-less actor under its raw, project-prefixed actor name,
  never its agent slug, and assigns it no project labels. Recovering a
  record-less actor's slug and project by inverting its actor name and
  verifying the guess against its atespace cannot be made safe against
  every project-scoped caller (a lookup scoped to project B could still
  resolve to project A's sole record-less actor sharing that slug, since
  nothing available could verify which project a record-less actor
  actually belonged to strongly enough for every caller). This runtime
  does not attempt it: a record-less actor is simply unreachable by slug,
  making a wrong-actor action structurally impossible, at the cost of
  requiring an operator to re-identify it by other means. The durable fix
  is persisting agent records so they survive a broker restart (§11), not
  reconstructing identity from the actor name.
- **A delete or stop that would otherwise report idempotent success fails
  closed instead when the target project's atespace holds a record-less
  actor.** Because a record-less actor can never be resolved by slug (above),
  a project-scoped delete/stop for an absent slug ordinarily looks
  indistinguishable from "genuinely nothing here" — but on a broker that has
  just restarted, "genuinely nothing here" and "the actor you meant has no
  record" are not the same thing, and reporting the former as success would
  let the hub treat the operation as complete while the actor, its egress
  policy, and its worker are still live. `SubstrateRuntime`
  exposes a narrow, type-asserted `RecordlessActorProber` capability
  (`pkg/runtimebroker`, never added to the generic `Runtime` interface) that
  lists the record-less actors in a project's own atespace independent of
  slug matching, excluding any actor already in `ACTOR_STATE_DELETING` (see
  below). `resolveDeleteTarget` and the stop path consult it, only when the
  requesting caller supplied a project ID, at every point they would
  otherwise answer not-found/idempotent — including, on delete, **before**
  accepting a file-only target from the local-file fallback below, not only
  when that fallback also finds nothing: a persisted project directory (a
  workstation broker, or any `$HOME` that survives a restart) can otherwise
  resolve a file-only delete for an agent whose actor is still running,
  record-less, on the cluster, deleting only the files and orphaning the
  rest. At least one record-less actor turns the answer into `HTTP 409
  agent_identity_unknown` (a generic body carrying only the count; the
  runtime-specific scope, such as the atespace, goes to the broker WARN log
  under `runtime_scope`) instead; none present leaves the ordinary idempotent
  404/202 unchanged; the
  probe itself erroring is an explicit failure, never treated as either
  outcome. See `deploy/substrate/README.md`, "After a broker restart", for
  the operator-facing behavior and cleanup steps, including the fail-closed
  consequences this accepts (a same-prefix atespace collision across two
  projects — reachable on purpose, since project IDs are client-supplied at
  creation, not just a theoretical accident; every absent-slug delete in an
  affected project returning 409 until its record-less actors are cleaned
  up; and a narrow `CreateActor`-to-record-write window that can spuriously
  409 an unrelated request in the same project).
- **Stop has a second failure mode delete does not: `projectScopedTarget`
  collapses any `LookupContainerID` error (not just a genuine miss) into
  the same `""` a not-found produces**, so a transient listing failure could
  otherwise reach the idempotent 202 for a still-existing, still-recorded
  agent. Re-deriving that outcome from a second, independent list call is
  not safe: a transient failure on the first call can resolve by the time
  the second one runs, silently masking exactly the failure this exists to
  catch. Instead, on a broker with at least one `RecordlessActorProber`
  runtime registered, `stopAgent` resolves the target through a single,
  error-preserving lookup (`projectScopedTargetErr`,
  `pkg/runtimebroker/handlers.go`) and decides directly from that one call's
  own error — an ambiguous match is also treated as "could not determine",
  never as not-found. Auxiliary runtimes are scanned in a fixed, sorted
  order, and a match found on any of them is authoritative: an earlier or
  later auxiliary runtime's own list error only becomes "could not
  determine" when no auxiliary runtime produces a match at all, so the
  outcome never depends on Go's randomized map iteration order. The rule is
  5xx when the lookup cannot determine the target: a primary-list error, or
  a stage that ends with no match after any auxiliary list error. The lookup
  runs a project-scoped stage and then an unscoped fallback stage
  (`scion.name` plus entries with no project label), and an error in the
  project-scoped stage is decisive: if that stage finds no match and its
  auxiliary scan errors, the lookup returns 5xx (fail-closed) without
  running the fallback stage, even though the fallback stage might have
  matched. Any of these outcomes returns an explicit 5xx rather than falling
  back to the idempotent 202. This same lookup also determines which
  manager `stopAgent` dispatches `Stop` through — the manager whose `List`
  call actually produced the matched entry, not a manager re-resolved by a
  second, independent lookup, which scans auxiliary runtimes in a different
  (map) order and so could return a manager for a different runtime than
  the one the target container ID came from. Every runtime without that
  capability is unaffected: `hasRecordlessProber` is false and `stopAgent`
  keeps calling `projectScopedTarget`/`resolveManagerForAgent` exactly as
  before, unchanged.
- **A record-less actor already in `ACTOR_STATE_DELETING` is excluded from
  the count above.** Stop is Delete (§4's `Stop` row): it drops
  this process's own in-memory record immediately, but Delete is
  fire-and-forget (above), so the actor can stay listed, in `DELETING`, for a
  while afterward. Without this exclusion, an ordinary same-project
  stop-then-delete sequence — no restart at all — would falsely report
  "broker restarted" for as long as that actor stayed listed. Excluding it
  is safe with respect to the egress leak this whole mechanism exists to
  prevent: an actor already in `DELETING` already had its egress policy
  removed first (Delete's own ordering, above), so there is nothing left for
  the count to protect, whether or not its `DELETING` state itself ever
  resolves. The proto documents no transition out of `DELETING` back to a
  live state either: `RevertActor`, the one RPC that returns an actor to a
  pre-delete state, only accepts CRASHED/RUNNING/PAUSED actors
  (`third_party/ateapipb/ateapi.proto:46-47`), and a deleted actor simply
  stops being listed. `ActorStatus.state` is also required on every listed
  actor, so a listed actor's state is never zero-valued/unknown; DELETING
  actors are observed either to finish deleting or to stay stuck, and none
  returns to a live state.
  **It is one of two documented exceptions to the invariant stated above**
  (the other is a second substrate profile on a different ateapi endpoint,
  not probed until its first `Run`; see `deploy/substrate/README.md`): a
  pre-restart actor that was already `DELETING` when the broker restarted
  (e.g. a pre-restart `Stop` that never finished) is excluded too, so a
  delete of its slug returns the ordinary idempotent `404` and the hub drops
  its record while the actor may still be sitting in the cluster, stuck.
  This is an accepted trade-off, not an oversight — see
  `deploy/substrate/README.md`, consequence (d), including the operator
  commands to find and clear one.
- **The broker's local-file deletion fallback (`findAgentInHubManagedProjects`,
  called from `findAgentProjectDir`) is itself project-scoped**: it only
  accepts a hub-managed project directory whose own recorded project ID
  equals the requested one, so a same-named agent's files in a different
  project are never returned as the delete target. For a matched
  record-having entry this fallback is unreachable anyway, since the
  matched entry's own trusted project path is used directly.

## 10. Known limitations

- **A create that outlives the hub's dispatch timeout leaks a running,
  unbootstrapped actor** (holding a worker indefinitely). The hub's
  control-channel `RequestTimeout` is hard-coded at 120s
  (`pkg/hub/server.go`); a cold template build (§3) can exceed it. The
  broker's own dispatch context is never cancelled by a hub-side timeout
  (generic, not substrate-specific — `pkg/runtimebroker/controlchannel.go`).
  **Operator mitigation:** warm the template before first use — see
  `deploy/substrate/README.md`.
- **The broker→router hop is plaintext HTTP**, and bootstrap credentials
  (including the composed home, §5.4) cross it. Mitigated only by
  the NetworkPolicy and first-bootstrap-wins nonce (§5.2), not by transport
  encryption. See `deploy/substrate/README.md` for the fix options.
- **PID-1 exit is not observed by Substrate.** A post-bootstrap init failure
  leaves a `RUNNING` actor holding a worker; `healthz` reports
  `init-failed`, but the hub only shows an error if the direct status report
  succeeds.
- A record-less actor (no in-memory record — e.g. after a broker restart)
  remains unreachable by slug even within its own project, by design (§9);
  an operator must re-identify it by other means until its record is
  restored. A project-scoped delete/stop targeting it (or any absent slug in
  the same atespace) fails closed with `409
  agent_identity_unknown` instead of silently reporting the usual
  idempotent success — including a delete resolved only from a persisted
  project directory (file-only target), and a stop whose own container
  lookup errored rather than a genuine not-found, both of which fail
  closed with an explicit error rather than the idempotent path they would
  otherwise fall back to (§9) — the actor is not touched either way, and the
  caller is told the actor may still exist rather than being told it is
  gone. A record-less actor
  already in `ACTOR_STATE_DELETING` is one of two exceptions (the other: a
  second substrate profile on a different ateapi endpoint, not probed until
  its first `Run`): it is never counted, so an ordinary same-project
  stop-then-delete sequence with no
  restart involved stays the ordinary idempotent 404/202 (§9). The same
  exclusion also means a *pre-restart* actor stuck in `DELETING` (e.g. a
  pre-restart `Stop` that never finished) is not counted either — its
  delete returns the ordinary idempotent 404 and the hub drops its record
  while the actor may still exist, stuck in the cluster (§9,
  `deploy/substrate/README.md` consequence (d)). A process-local record of
  this process's own delete requests could not distinguish this case from a
  genuinely pre-restart actor either, since the reason the deletion never
  finished is below scion.
- Exec and Message against a record-less actor fail with an explicit
  "no control token cached" error — permanent for that specific actor, since
  bootstrap is one-shot (§5) and there is no way to re-mint or recover a
  token an earlier process already used.
- The broker's in-memory `substrateAgentRecords`/`substrateControlTokens`
  are lost on a broker restart for actors it did not create in the current
  process lifetime (§4's `List` row); a ConfigMap-backed store is
  future work.
- Stop-then-delete can leak local broker-side files.
- A forced delete can still leak an actor that doesn't confirm `DELETING`
  (§9).
- Non-home image paths remain root-owned after the rootfs fixup (§8), which
  only re-chowns under the resolved home directory.
- **A default install's `substrate` create fails closed with the
  `not pinned by digest` error (§3) against the shipped `claude`
  harness-config's `scion-claude:latest` image, and a substrate profile's
  own `harness_overrides` image pin alone does not fix it.** The
  harness-config's image is copied into the agent's persisted config at
  provisioning (`pkg/agent/provision.go`), and at start that agent/template
  config image outranks the profile override (`pkg/agent/run.go`'s image
  resolution) — see §3 and `deploy/substrate/README.md` for the required
  setup step (a digest-pinned template `image:` or `--image`).
- **The egress hostname-rule model requires the sdsmint MITM gateway**
  (§7.1); the plain gateway enforces only address rules for TLS.
- **The dialer's trust material is pinned for the broker process's
  lifetime** (§2), not re-read per agent start.
- **The every-prefix symlink guard (§5.5) is not a general TOCTOU
  guarantee** — it depends on `substrate-serve` being the sole writer during
  bootstrap.
- Build-bootstrap-files-before-create is not implemented: `buildBootstrapFiles`
  runs after the actor is already `RUNNING` (§4 step 8), so a deterministic
  configuration error (e.g. a missing `HomeDir`, §5.4) costs a full actor
  spin-up and teardown rather than failing before `CreateActor`.
- No exemption exists for a bootstrap target that must traverse a system
  symlink (§5.5); the documented workaround is the resolved path form.

## 11. Future work

- Make the default-install image gap (previous section) harder to hit by
  default — e.g. a runtime-aware harness-config seed, or resolving tags to
  digests generically — rather than requiring every substrate install to
  set a digest-pinned template `image:` by hand.
- Identity-derived bootstrap nonce (`MintActorJWT`), replacing the current
  fallback (§5.2).
- Seal the bootstrap payload end-to-end, or move the router endpoint to its
  TLS listener with a `ClusterTrustBundle` CA, closing the broker→router
  plaintext gap (§6, §10).
- `Suspender` capability (`Suspend`/`Resume`) using Substrate's FULL
  snapshot scope, plus the `$HOME` durableDir layout needed for `Stop`
  to keep the workspace instead of deleting the actor (§4's `Stop` row).
- Credential rehydration after a FULL resume (the hub revokes agent
  credentials on suspend).
- Eviction handling beyond "don't forward SIGTERM" (§5.6): watch for
  `WORKER_STATE_DRAINING`, and have the broker suspend proactively.
- An on-demand hub port-forward tunnel and `sciontool` autoexpose, so
  Substrate agents regain that path once it no longer depends on
  always-on WebSocket egress (§1) — a scion-wide refactor, not
  substrate-specific, that this runtime would opt into once it lands.
- Template GC, and a durable, ConfigMap-backed `substrateAgentRecords` store
  — keyed by actor UID or `<atespace>/<actor>`, reloaded at broker startup —
  surviving broker restarts (§4's `List` row, §9, §10). This is what would
  let `List`, and therefore delete/stop, resolve a pre-restart actor by slug
  again instead of requiring the operator cleanup in
  `deploy/substrate/README.md`.
- Control-token persistence: mint the token into a Kubernetes `Secret` at
  the same point it is cached in memory today (`Run`, §4 step 8), keyed the
  same way, and reload it at broker startup. This cannot help an actor
  bootstrapped before the persistence code ships — `sciontool
  substrate-serve`'s bootstrap endpoint is one-shot (§5), so a broker that
  never held that actor's token in the first place cannot retroactively
  acquire it — but it closes the gap for every actor started after this
  lands.
- Confirm actor deletion (poll until `DELETING` clears, or report a stuck
  state) instead of the current fire-and-forget `Delete` (§9).
- Doctor integration for the substrate profile (API reachability, auth,
  atespace create permission, WorkerPool capacity, snapshot bucket, router
  reachability).
- Re-read the dialer's trust material per handshake instead of pinning it
  for the broker process's lifetime (§2).
- Build bootstrap files before `CreateActor`, so a deterministic
  configuration error doesn't cost a full actor spin-up (§10).
- A concrete exemption mechanism for a bootstrap target that must traverse
  a system symlink, if a real case ever needs one (§5.5) — none is known
  today, and an ownership heuristic can't reliably separate a benign
  symlink from a hostile one.
- The sdsmint trust-bundle projection (§7.1) as a supported, tested
  configuration on more than one cluster.
- The pre-start `HOME=<agent home>` exception in §8.1's lifecycle hook
  privilege enforcement (`buildEnforcedCmd`, `eventName == EventPreStart`) is
  safe under this runtime's own invariants for the one root-eligible hook it still
  applies to — a project/hub hook: bootstrap is one-shot per actor process (a
  repeat gets 409, checked before any side effect), and each actor resumes
  from a golden snapshot taken before its own bootstrap ever ran, so no
  workload code has touched `$HOME` when that hook runs. `Suspender`/`Resume`
  or any other mechanism that re-runs pre-start over a `$HOME` an earlier
  bootstrap already populated would invalidate that premise — a root
  project/hub hook would then be reading a HOME the workload had already had
  a chance to write into. Revisit the exception (e.g. by having the bootstrap
  redirect's clear step report whether it found prior content, and treating
  that as "a workload may already have run here") before any such mechanism
  ships. The harness provisioner does not share this exposure: it runs
  under the workload's own uid/gid regardless of `$HOME`'s freshness (§8.1),
  so a resume/re-bootstrap over a persisted `$HOME` cannot turn it into a
  root write against workload-controlled content the way it still could for
  a project/hub hook.
