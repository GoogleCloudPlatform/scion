# Single-Node Hosted on Cloud Run Instances + Sandboxes

## Status

**Implemented, verified end to end 2026-08-25.** Pending review. Depends on four
upstream fixes that are filed but not yet landed (§9.2).

## 1. Overview

Scion's tier spine is Local → Workstation → **Single-node hosted** → HA hosted. The
single-node hosted tier needs one long-lived node running the hub, the broker, and N
agent containers, with a filesystem all three can see. Until now it had no
first-class hosting substrate.

This design provides one, using two Cloud Run capabilities:

- **Cloud Run Instances** — individually addressable long-lived compute with its own
  `run.app` URL and built-in SSH. *Acts as the VM.*
- **Cloud Run Sandboxes** — a `sandbox` CLI invoked *from inside* a container to
  launch nested, isolated workloads. *Acts as the Docker runtime.*

### 1.1 The core framing

**The Cloud Run Instance is the node.** This is not a new tier and not a new
configuration axis. It is the existing single-node hosted tier with two component
substitutions:

| Single-node hosted (generic) | This design |
|---|---|
| A VM | A Cloud Run Instance |
| Docker daemon | `sandbox` CLI |
| Combo server (hub + embedded broker, one process) | unchanged |
| Embedded SQLite | unchanged |
| Bind-mounted workspaces on local disk | unchanged (Instance's ephemeral disk) |

Everything Scion already knows about the single-node tier continues to apply. What is
added is **one new `Runtime` implementation** and the packaging around it.

### 1.2 Goals

- **G1.** A single container image ("omni image") that, deployed to one Cloud Run
  Instance, yields a working Scion hub able to launch agents.
- **G2.** Agents run in Cloud Run Sandboxes, driven through the existing tmux control
  plane, with attach, exec, and logs working from the hub UI.
- **G3.** No new abstraction layer. Implement `runtime.Runtime`; change nothing about
  how `pkg/agent` constructs a launch.
- **G4.** Deployment is one command against a GCP project — no registry setup, no
  NFS, no Filestore, no Kubernetes.
- **G5.** Cheap and fast to stand up and tear down, explicitly trading durability for
  this (§5).

### 1.3 Success criteria

An operator with a GCP project runs one deploy command, opens the resulting `run.app`
URL, logs in, creates a project, starts a Claude agent, attaches to its terminal from
the browser, and watches it commit to a git remote.

This is the only measure of done for the tier. It is restated as the acceptance
criteria in §10.

## 2. Non-Goals

- **HA, failover, or multi-node.** One Instance. A single point of failure by design.
- **Durability of agent workspaces.** Workspaces live on ephemeral storage and are
  lost on redeploy (§5).
- **Per-agent resource isolation guarantees.** All agents share the Instance's CPU
  and memory budget.
- **Replacing the Cloudflare Tunnel design.** That targets ingress for self-hosted
  nodes. A Cloud Run Instance has its own `run.app` ingress and needs no tunnel. The
  two are siblings, not competitors.
- **Templated Sandboxes.** Not yet available. Their absence is what forces the omni
  image (§4.1); §8.3 records what changes when they ship.
- **Agent-per-Cloud-Run-Instance.** That is a different topology and a separate
  runtime already in flight (§7.1).

## 3. Architecture

### 3.1 Topology

```
┌─ Cloud Run Instance ─────────────────────────────────────────────┐
│  omni image, single container                                    │
│                                                                  │
│  PID 1: scion server start --foreground --enable-hub             │
│    ├── hub (HTTP, :8080)  ──────────────► run.app ingress (IAP)  │
│    ├── embedded broker (in-process)                              │
│    └── runtime = "cloudrun-sandbox"                              │
│                                                                  │
│  /home/scion/.scion/projects/<slug>/     (ephemeral disk)        │
│      agents/<name>/home/                 ── bind ──┐             │
│      workspace/                          ── bind ──┤             │
│                                                     │            │
│  $ sandbox run --rootfs / --write --allow-egress  ◄──┘           │
│         │                                                        │
│         ├── sandbox: scion--proj--agent-1                        │
│         │     PID 1 = sciontool init -- sh -c "tmux new-session…"│
│         ├── sandbox: scion--proj--agent-2                        │
│         └── …                                                    │
└──────────────────────────────────────────────────────────────────┘
```

The hub, the broker, and every agent share one filesystem and one image.

### 3.2 Why the shared filesystem matters

The existing launch path in `pkg/agent` assumes the process constructing a launch can
see the files the agent will use — templates, harness configs, the workspace, the
agent home. Every runtime that breaks that assumption has to reintroduce it with
remote staging. Sandboxes launched with `--rootfs /` inherit the launcher's
filesystem by construction, so this design gets the assumption for free. It is the
single largest reason the tier is small.

## 4. Load-bearing design decisions

These are the decisions that are costly to reverse. Everything else in the
implementation is mechanical.

### 4.1 The omni image, built by chaining harnesses

Sandboxes cannot currently select a per-agent image: `--rootfs /` inherits the
launcher's filesystem, and per-sandbox images are the Templated Sandboxes feature,
which is not yet available. Every harness an operator might start must therefore be
present in the one image the Instance runs.

**Build it by chaining the existing harness Dockerfiles, not by transcribing them.**

```
scion-base -> claude -> codex -> opencode -> antigravity -> grok-build -> omni
```

All harness Dockerfiles are already `ARG BASE_IMAGE` / `FROM ${BASE_IMAGE}`, so they
stack. `image-build/scripts/lib/targets.sh` already models chained builds. The omni
image is therefore a **target definition, not a Dockerfile**, and the harness
definitions stay single-source.

**Rationale.** The first implementation hand-transcribed the five harness install
steps into one Dockerfile. That is a maintenance fork of five files with no drift
detection: when a harness bumps a pinned version, omni keeps the old one and nothing
fails. The defect class is the worst available here — an agent behaves differently on
this tier than on every other tier, with no error, no failing test, and no diff to
point at.

**Known cost.** `USER` state is order-dependent when stacking; `grok-build`
deliberately ends as root. Pin `USER` explicitly at the end of the chain.

Gate the chain on `OMNI_BUILD`, following the precedent `THICK_BUILD` set, so every
existing build path stays byte-identical.

### 4.2 tmux stays inside the sandbox; `sandbox exec` is the transport

The natural first design is to bind-mount the tmux socket out of the sandbox so the
launcher can drive it. That does not work, and it is also unnecessary.

**Nothing needs to cross the boundary except a command.** The tmux client and server
both live inside the sandbox, the socket never leaves, and `sandbox exec` carries each
operation in.

| Operation | Command from the launcher |
|---|---|
| send task | `sandbox exec <id> -- tmux send-keys -t scion:agent …` |
| liveness | `sandbox exec <id> -- tmux has-session -t scion` |
| logs / `scion look` | `sandbox exec <id> -- tmux capture-pane -p -t scion:agent` |

The first three are non-interactive; stdio is wired as pipes and pipes suffice. So
`scion message` and `scion look` work with no new mechanism.

**Interactive attach needs a launcher-side PTY.** A bare `sandbox exec <id> -- tmux
attach` reports `not a terminal`. Allocating a PTY on the launcher side does
propagate into the sandbox, and the tmux UI renders correctly. This was verified by
two independent wrappers agreeing, so it is a property of the boundary rather than of
one tool.

**Consequence:** there is no `TMUX_TMPDIR` and no socket path in the runtime's state
store. Both were vestiges of the bind-mount design. A persisted field holding a
plausible-looking wrong path is worse than an absent one, because the next reader
will trust it.

### 4.3 Runtime selection probes the capability, not the product

**`K_SERVICE` is not set on Cloud Run Instances.** This is the most consequential
platform fact in the design and it is counter-intuitive: an Instance is a Cloud Run
resource that does not announce itself as one. Five sites in the codebase use
`K_SERVICE` as a proxy for "am I on Cloud Run"; on an Instance every one evaluates
false. The practical effect is that autodetect falls through to `docker` and produces
a runtime that fails on first use with daemon errors.

`CLOUD_RUN_INSTANCE` *is* set, but it answers a different question. It says *where we
are running*; selection needs to know *what we can launch*. On a Cloud Run Instance
there are two defensible answers — this design, and the agent-per-Instance sibling
(§7.1) — and `CLOUD_RUN_INSTANCE` is true for both, so it cannot discriminate.

**Use both, in order:** `CLOUD_RUN_INSTANCE` to establish the environment, then a
probe for the launcher binary to pick the runtime within it.

```go
// pkg/runtime (illustrative)
func SandboxLauncherAvailable() bool {
    _, err := os.Stat("/usr/local/gcp/bin/sandbox")
    return err == nil
}
```

Binary present → `cloudrun-sandbox`; absent → fall through to the sibling. This
degrades correctly if sandboxes ever ship on another platform.

Two related deploy-time consequences of the missing `K_SERVICE`:

- `hub_id` cannot derive from `K_SERVICE` and falls back to hostname. **Set
  `server.hub.hub_id` explicitly in the deploy**; hostname stability across redeploys
  is unverified.
- The logging paths conclude "not on Cloud Run" and stand up their own Cloud Logging
  client, but an Instance's stdout is already captured. Likely duplicate ingestion.

### 4.4 The runtime is named `cloudrun-sandbox`

`cloudrun-instances` is taken by a real implementation of the opposite topology
(§7.1), not by a placeholder. Reusing the name would conflate the two. The existing
stubs are the sibling's landing pad and must not be repurposed.

### 4.5 Every address handed into a sandbox needs an explicit decision

A sandbox is not on the launcher's network, and it is not on the public internet's
side of our own perimeter. Two defects of exactly this shape were found and fixed,
and the rule generalises from them:

- **Hub endpoint.** Agents were handed the hub's own public, IAP-fronted URL. The hub
  is a link-local hop away on the same Instance, but the agent would egress to the
  internet, arrive at the IAP edge, and present credentials it does not have — so IAP
  answered `302 accounts.google.com`, correctly. At the edge, "hostile internet" and
  "our own agent on our own Instance" are the same request.
- **Metadata server.** `GCE_METADATA_HOST` pointed at `localhost`. Loopback does not
  work from inside a gVisor sandbox; only the launcher's link-local address does.

Both are the same bug: an address that is correct for a co-located process and wrong
for a sandboxed one.

**Consequence for the metadata emulator:** it binds to the launcher's link-local
address, discovered at startup, with a guard that refuses to bind `0.0.0.0`. This is
what makes ADC work inside a sandbox.

## 5. Durability — Tier 0, pure ephemeral

Workspaces and the SQLite control plane live on the Instance's ephemeral filesystem.
A redeploy loses both.

This is a deliberate trade for G5, not an oversight. The tier is aimed at cheap,
fast, disposable deployments. Operators who need durability want the GCE VM baseline
(§7.4) or the HA hosted tier.

The state store the runtime keeps on disk is preview-stage and no state file is
expected to outlive a redeploy, so schema changes in it carry no migration cost.

## 6. Security and the auth perimeter

### 6.1 One gate, and it must be designed for

| Gate | Status | Why |
|---|---|---|
| Cloud Run invoker IAM | **Must be OFF** | IAP's `x-serverless-authorization` carries a `services`-path audience the Instance invoker check rejects, producing 401. `invokerIamDisabled: true` is mandatory. |
| **IAP** | **The sole network perimeter** | Edge-enforced, verified on genuine Instances. |
| Hub session auth | Unchanged | The application-layer gate, behind the perimeter. |

**With the invoker check off, IAP is load-bearing and alone.** If `iapEnabled` is
false — set by accident, by a `gcloud` command that omits it, or by a PATCH that
drops the field — the Instance is open to the internet with only hub session auth in
front of it. Turning IAP on did not remove this footgun; it *relocated* it. Because
the open configuration is now the supported one, the deploy command must gate on it
rather than merely warn.

### 6.2 The hub work was already done

Auditing before designing showed the hub's IAP verification already exists and
already accepts the Instance's audience. This tier is substantially a *configuration*
phase.

| Component | State | Work required |
|---|---|---|
| `hub.IAPAuthenticator` | Verifies ES256 via JWKS, checks `iss`, mandatory `aud`, `exp`/`iat` with 30 s skew | None |
| Wiring behind `auth.mode=proxy`, `auth.proxy.provider=iap` | Exists; fails closed on empty audience | None |
| `isSupportedIAPAudience` | The Instance audience form passes unchanged | Add a test pinning this, so nobody "tidies" it |
| `iapAudienceToCloudRunURL` | Derives the legacy `run.app` URL format, which Instances use | Fall back to `SCION_SERVER_BASE_URL` if the format changes |

The expensive-looking half was built for the GKE tier. What remained was the deploy
command, admin bootstrap, and guard rails.

### 6.3 Dev auth must refuse non-loopback

Dev auth bound to a non-loopback interface on a publicly reachable Instance is a
critical exposure. The server refuses to start in that configuration. This fix is
valuable independently of this tier and should land on its own.

## 7. Alternatives considered

### 7.1 Agent-per-Cloud-Run-Instance — a sibling, not a road not taken

This is a second runtime actively being built, and it will remain a distinct
supported runtime.

| | `cloudrun-instances` (sibling) | `cloudrun-sandbox` (this design) |
|---|---|---|
| Unit of isolation | one Instance per agent | one sandbox per agent, all in one Instance |
| Filesystem | not shared — needs remote staging | **shared by construction** |
| Per-agent billing | yes | no — one Instance |
| Control channel | network | `exec` + tmux inside the sandbox |
| Per-agent image | yes | no, until Templated Sandboxes |

They are complementary. The sibling buys real isolation and per-agent billing at the
cost of the shared filesystem the entire launch path rests on (§3.2). This design
buys cheapness, speed, and that filesystem at the cost of shared fate. Neither
subsumes the other.

The sibling also brings prior art worth conforming to rather than duplicating: an
`ExecConnector` interface abstracting how you exec into an Instance, and Cloud Run log
streaming relevant to the `GetLogs` fallback.

### 7.2 Docker-in-Docker inside the Instance

Run a real Docker daemon and keep `DockerRuntime` unchanged. Zero new runtime code,
full template and image support.

**Rejected:** requires privileged containers, which Cloud Run does not grant. It also
forfeits the isolation sandboxes exist to provide, and reintroduces the whole
image-distribution problem the omni image deletes.

### 7.3 Sandboxes with per-agent images via a registry

Skip the omni image; pull a per-agent image for each sandbox.

**Rejected because it is not currently possible.** `--rootfs /` inherits the
launcher's filesystem by construction. Revisit when Templated Sandboxes ship (§8.3).

### 7.4 Keep the hub off Cloud Run entirely — GCE VM plus Docker

A plain VM with a persistent disk, Docker, and tunnel ingress.

**Not rejected — it remains the right answer for self-hosting.** It is strictly more
durable and more capable: real per-container limits, template images, no alpha
dependency. It is listed because it is the honest baseline this design must justify
itself against. This design wins on no VM to manage or patch, no tunnel, no registry,
and a one-command deploy. It loses on durability and per-agent resource control. Both
should exist.

## 8. Migration and rollout

### 8.1 Nothing existing changes

Every change is additive behind a runtime type that no existing configuration
selects. Docker, Podman, and Kubernetes paths are untouched. The one shared-code
change is the selection discriminator (§4.3), which only alters behaviour on a
machine that has the sandbox launcher binary.

### 8.2 Ordering

The dev-auth security fix (§6.3) must land **before** this tier, for two independent
reasons: it is a security fix valuable on its own and must precede any public deploy,
and this tier's branch currently carries a duplicate of it that disappears on rebase.

### 8.3 When Templated Sandboxes ship

Per-agent images return. The runtime gains an image-selection path, and
`ImageExists`/`PullImage` stop being no-ops. Because the omni image is confined to the
image build and the runtime's own methods, this is a change to one file plus the build
definition, not a redesign.

## 9. Known gaps

### 9.1 Within this tier

- **Ephemeral only.** No workspace or control-plane durability (§5).
- **No per-agent resource limits.** All agents share the Instance budget.
- **Image-pull failures on first deploy are hard to diagnose.** The messages come
  from the Cloud Run sandbox launcher, not from Scion, and name a cache mirror rather
  than the requested image. Routed as platform feedback.
- **Sandbox stderr can be lost**, which makes an agent that dies during provisioning
  harder to diagnose than it should be. The general Scion path is well instrumented;
  the loss is specific to this runtime's sandbox handling.

### 9.2 Upstream dependencies

Four defects found while building this tier are **not specific to it** and are filed
against the platform. Each affects other deployments; this tier is simply where they
surfaced first.

| Issue | Problem |
|---|---|
| #1273 | A hosted hub drops template and harness-config identity on agent create, and the broker then falls back to a local disk search that is always empty in hosted mode. |
| #1274 | `GitCloneConfig.Depth` is documented as `0 = full clone` and implemented as depth 1, in three independent call sites. A depth-1 workspace cannot push to any remote but origin. |
| #1275 | `noAuth: true` on agent create makes a request fail that succeeds without it. |
| #1276 | The auth preflight counts only `metadata_mode: assign` as an assigned GCP identity, so a host with ambient credentials from the real metadata server never satisfies `skipped_when_gcp_service_account_assigned`. Runtime-agnostic: affects GCE and GKE workload identity too. |

Until #1273 and #1276 land, this tier needs deploy-time workarounds. They are
stopgaps and should be removed when the fixes arrive.

## 10. Acceptance criteria

The tier is done when an operator can do all of the following, in order, on a clean
GCP project, using only the documented deploy command:

1. Run one deploy command and get a `run.app` URL.
2. Open the URL and be challenged by IAP.
3. Log in and reach the hub as an admin.
4. Create a project backed by a git remote.
5. Start a Claude agent and see it reach a running state.
6. Attach to the agent's terminal from the browser and see a live tmux session.
7. Have the agent commit and push to the git remote.
8. Redeploy, and confirm the documented durability behaviour (§5) — workspaces gone,
   no surprise partial state.

Additionally, for review:

9. A deploy with `iapEnabled: false` is refused by the deploy command, not merely
   warned about (§6.1).
10. Autodetect selects `cloudrun-sandbox` on an Instance and does not select it
    anywhere else (§4.3).
11. The omni image is produced by the chained build, and no harness version is pinned
    in two places (§4.1).
