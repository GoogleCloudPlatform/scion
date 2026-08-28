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
  and memory budget. See §9.1 for measured agent counts at each Instance size.
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

- `hub_id` cannot derive from `K_SERVICE` and falls back to hostname. That fallback is
  now measured, and it is stable: the hostname inside an Instance is always `localhost`,
  so `hub_id` is always `49960de5880e` — the first twelve hex digits of
  `SHA-256("localhost")` — on every Instance in every project. **Do not set
  `server.hub.hub_id` in the deploy.** An earlier revision of this document instructed
  otherwise, on the assumption that the hostname was unstable; no deploy ever followed
  the instruction, and the measurement says it would have bought nothing. `hub_id` is not
  inert — it seeds the hub's signing key, so a change to it invalidates live JWTs — but
  across four startups the signing keys differed every time while `hub_id` stayed
  constant. Key material is regenerated per start on this tier regardless of `hub_id`,
  because nothing persists it. Pinning `hub_id` therefore cannot stabilise anything that
  is not already stable.

  This conclusion is conditional, and here is the condition that overturns it: today
  every Instance in every project shares one `hub_id`, which is harmless only because
  the value is never used to tell two hubs apart. Give this tier a persistent secret
  backend (GCS, Secret Manager) so keys survive a restart, or run more than one Instance
  against shared state, and `hub_id` uniqueness starts to matter. At that point setting
  it explicitly — to the Instance name, which is operator-chosen and already unique
  within a project — becomes the correct design.
- The logging paths conclude "not on Cloud Run" and stand up their own Cloud Logging
  client, but an Instance's stdout is already captured. Likely duplicate ingestion.

**The same principle governs the deploy path, for a different reason.** Creating this
tier's Instance requires the `gcloud beta run instances` command, which speaks the v1
API — the only surface that carries `sandboxLauncher`. The v2 API will happily create
an Instance without it, so the constraint is not "v2 cannot create Instances"; it is
that a `sandboxLauncher`-less Instance is a different artifact, and one whose scion
server cannot start. That command is not on every Cloud SDK: it is **absent at
575.0.0**, where the `instances` noun is alpha-only, and **present at 582.0.0**.
Versions 576–581 are unmeasured, so **this design states no version floor**. Writing
one down would publish a number nobody has checked.

Three consequences for tooling:

- **Probe for the noun; do not compare version strings.** The deploy script must
  refuse early with a message that names the missing command. A hardcoded floor would
  reject working installations anywhere in the unmeasured range — a gate that rejects
  a good install is worse than the error it replaces.
- **`gcloud`'s own advice on failure is a wrong fix.** It suggests
  `gcloud alpha run instances`. The alpha surface uses `create` rather than `deploy`
  and has no `--sandbox-launcher`, so following it produces an Instance whose scion
  server crashes on startup. The diagnostic has to say so, because the platform's
  suggestion is actively misleading.
- **Where the deploy leaves `gcloud` and speaks REST directly, it inherits a narrower
  credential contract — and must not assume otherwise.** IAP configuration has no
  `gcloud` flag, so it is applied by a hand-authenticated REST PATCH. That PATCH
  rejected credential types the `gcloud` step immediately before it had accepted,
  returning `401 ACCESS_TOKEN_TYPE_UNSUPPORTED`. The operator experience is the
  pathological one: the deploy authenticates, does real work, and *then* fails on
  credentials — so the error arrives after the point where the operator has concluded
  their auth is fine.

  **The general form is the same argument this section makes about `K_SERVICE`: a signal
  that answers a nearby question is not the same as one that answers yours.** "`gcloud`
  authenticated successfully" answers *can gcloud use this credential*, not *can the REST
  endpoint use this credential*. Any hand-rolled call must validate the credential
  against the surface that will consume it, as early as the deploy can do so — the
  preflight, not the point of use.

**This section describes runtime autodetection only, and that is not the whole of what
an agent runs on.** The profile layer above it makes its own selection and does not learn
what autodetect decided — see §4.7, which is the same argument as this section's, one
layer up, and which cost a §1 blocker to find.

### 4.4 The runtime is named `cloudrun-sandbox`

`cloudrun-instances` is taken by a real implementation of the opposite topology
(§7.1), not by a placeholder. Reusing the name would conflate the two. The existing
stubs are the sibling's landing pad and must not be repurposed.

**The sibling has since landed as a real PR, which settles this.** Upstream
`GoogleCloudPlatform/scion#1302`, "Cloud Run Instances runtime", opened 2026-08-26,
dispatches *each agent as its own Cloud Run service* via
`pkg/runtime/cloudrun_runtime.go` and `pkg/runtime/cloudrun/iap_exec.go`. This tier
dispatches *all agents as sandboxes inside one Instance* via
`pkg/runtime/cloudrun_sandbox_runtime.go`. Verified by intersecting the two file
lists: neither touches the other's runtime implementation.

The two are complements, not competitors, and the difference is the durability trade
in §5. A per-agent service survives its neighbours and scales independently; a
sandbox on a shared Instance is cheaper, starts faster, and shares one filesystem
(§3.2) — and dies with the Instance.

The two changesets do share three files — `pkg/runtime/factory.go`,
`cmd/server_foreground.go`, `pkg/config/settings_v1.go`. `factory.go` is the one that
matters: both register a runtime, so whichever lands second reconciles the
registration. That is expected, and it is the ordinary cost of the two runtimes being
genuinely separate rather than one overloaded implementation.

**Outcome, 2026-08-27 — the prediction held, and my first attempt to correct it was
itself wrong.** Both entries stay, because the sequence is the lesson.

At 00:31 I measured the rebase surface and found `factory.go` untouched by any of the
eight upstream commits the branch was behind. I concluded the predicted conflict had
been overtaken by landing order, wrote that here, and told the rebase developer
explicitly not to go looking for it.

That measurement was accurate and the conclusion drawn from it was not. GoogleCloudPlatform/scion#1302 — the
Instances runtime, the *other* half of the pair this section is about — merged
upstream as `83ee4bd9` roughly twenty minutes after I measured. Re-measured at 00:49,
the branch was behind by eleven rather than eight, and the conflict surface was
exactly the three files named above: `factory.go`, `cmd/server_foreground.go`,
`pkg/config/settings_v1.go`. The original reasoning was right on all three.

Two things are worth carrying forward. **A conflict-surface measurement is a snapshot
with a short half-life**, and an active upstream invalidates one faster than a rebase
takes to run; the measurement needs re-taking immediately before resolution, not once
at brief-writing time. And the `factory.go` conflict has a shape that a clean
auto-merge hides: both sides register `cloudrun-instances`, so git resolving it
without complaint can yield a duplicated `case` — a compile error, not a merge marker.
The branch appeared `MERGEABLE` throughout.

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

### 4.6 The deploy runs on the operator's machine, and that machine is not ours

Every other decision in this document concerns software we build and run. This one
concerns software we build and *someone else* runs, on hardware we have never seen.
§1.3 begins *"an operator with a GCP project runs one deploy command"* — so
`scripts/single-node/deploy.sh` executes on a laptop, and the tier is only as portable
as that script.

**This section exists because its absence caused a §1 blocker.** The deploy script used
`${var,,}`, a bash 4.0 parameter expansion, at two sites. macOS ships **bash 3.2.57** —
the last GPLv2 release — and has since 2007. The script died on line 286 the first time
it was run on a Mac. Five review rounds, 42 Go tests, 62 shellcheck files and a live
end-to-end deploy all passed beforehand, because **every one of them ran on Linux with
GNU userland and bash 5.** Nothing was wrong with the review; the review had no way to
see it. **Nobody wrote the requirement down, so nobody asked, so nothing tested it.**

**The decision: the supported operator environment is stock macOS and mainstream Linux,
with no prerequisite installation step.** "Install a newer bash first" is a second
command, and §1.3 says *one*. This is load-bearing in the strict sense — it constrains
every line of the deploy script permanently, and it is expensive to reverse once
operators rely on it.

What that commits us to, measured on an arm64 Darwin 25.6.0 machine on 2026-08-28:

| Constraint | Measured | Consequence for the deploy script |
|---|---|---|
| `bash` 3.2.57(1) | Both `/bin/bash` and the `PATH` bash | No `${v,,}`/`${v^^}`, no `mapfile`/`readarray`, no `local -n`, no `[[ -v ]]`, no `wait -n`, no `coproc`. **`declare -A` is worse than absent — see below.** `printf -v` *is* available |
| `=~` quoted right-hand side | Trap confirmed present | From 3.2 on, quoting the pattern makes it match **literally**. The RHS must stay unquoted, and this is a security-relevant line — it feeds a host-shape assertion |
| BSD `sed` | Rejects the GNU-style `--help` extractor | Assume BSD `sed`; no GNU-only addressing |
| BSD `grep` 2.6.0-FreeBSD | — | No `-P` |
| `awk` 20200816 (BWK) | — | Not `gawk`; no `gensub` |
| `mktemp` with no template | Works | Not the portability hazard it was assumed to be |

**Row one is now measured, one construct per subprocess, on a native `macos-15` runner**
(`scripts/dev/bash32-feature-probe.sh`). It did not begin that way. The list was written
from bash release history and printed under a column headed "Measured", and it was wrong:
`printf -v` arrived in bash **3.1**, so 3.2.57 has it. **A prohibition is the one kind of
claim that is never falsified by use** — nobody trips over a rule saying they cannot do
something, so a wrong entry silently narrows what everyone writes, forever.

**`declare -A` is the entry that matters, and "unsupported" understates it.** On 3.2.57
`declare -A m` **exits 0** while printing `declare: -A: invalid option` to stderr. The
variable is created — as an *indexed* array. A later `m[key]=value` then evaluates `key`
as an arithmetic expression, which yields **0**, so every key writes to `m[0]` and the
last write wins. There is no error at the point of use. **A probe keyed on exit status
alone reports `declare -A` as supported**, which is exactly what the first version of this
probe did; the third commit exists to catch the exit-0-but-rejected class.

Two properties of the measurement are load-bearing and easy to lose. **Probe each
construct in its own subprocess:** `${v,,}` and `${v^^}` are *parse* errors, and a parse
error aborts the whole script before its first line, so a single-script probe measures one
failure and reports it as nine confirmations. **And include a control that must succeed**,
or a broken harness reports universal unsupport and looks like a thorough result.

**The general rule, which outlives the specific list: a portability fix is a semantics
change until proven otherwise.** The obvious repair for `${host,,}` is a `tr` command
substitution — and command substitution strips trailing newlines, which silently flipped
three verdicts of the host-shape assertion from REJECT to ALLOW. A portability edit to a
security-relevant line must be proven byte-identical on adversarial inputs, not merely
observed to stop erroring.

**The gate is a CI job on a native macOS runner**, not a hand-built old bash on Linux.
GitHub's macOS runners ship bash 3.2.57 natively, so the interpreter needs no artifact
to fetch, verify or compile — and the runner also supplies the BSD userland and arm64
hardware, which a compiled bash on Linux would not. The runner image is pinned to a
specific version rather than `macos-latest`, because a moving alias silently retires the
gate the day the fleet upgrades past bash 3.2.

### 4.7 The profile layer never learns what the runtime layer decided

**§4.3 is correct and was not enough.** Autodetect picks `cloudrun-sandbox` on an
Instance and nowhere else, exactly as that section describes. But autodetect is not the
only layer that decides what an agent runs on, and the layer above it — *profiles* —
was never described here. A fresh deploy pre-selected `remote (kubernetes)`, which this
tier cannot serve, so §10 step 5 was unreachable on an otherwise correct deploy.

**The mechanism is a substitution that is never written back.** `GetRuntime` resolves
the configured runtime `docker`, observes it cannot work on an Instance, and returns
`cloudrun-sandbox` instead. That substitution happens at the runtime layer and stays
there. The `local` profile still *declares* `docker`. Nothing tells the profile layer
what the runtime layer chose.

`buildInfoProfiles` then filters the profile list against the broker's actual runtime,
and drops any profile that is local-only when the broker is not. So it drops `local`
**because the declaration and the substitution disagree** — and keeps `remote`, whose
`kubernetes` survives only because it is *not local-only*. **The filter discards the one
profile this broker can serve and keeps the one it cannot.** One profile then remains, so
the UI auto-selects it, and the operator is given the broken option by default and never
sees a choice.

**This is the same error §4.3 already warns about, one layer up.** That section's
argument is that `K_SERVICE` answers a nearby question rather than yours.
`isLocalOnlyRuntime` is a negative predicate standing in for a positive one: the question
that matters is *can this broker serve this profile*, and the code asks *is this profile
local-only*. Those coincide on a workstation and diverge on every hosted tier. **A
predicate that is right by coincidence is right until the environment changes, which is
precisely what a new tier is.**

**Decided: the tier seeds its own settings**, following the multi-node tier's existing
`hub-settings-template.yaml` precedent, so a `default` profile exists that declares
`cloudrun-sandbox` explicitly. Delivery is via `InitMachine`, not the deploy script —
`deploy.sh` runs on the operator's machine (§4.6) and is the wrong place for a decision
about the server's own configuration.

**One mechanism here is load-bearing and was measured, not assumed:** koanf loads the
embedded defaults *first* and the seeded template *after*, so the scalar `active_profile`
is **overwritten** while the `profiles` map **merges**. The fix depends entirely on that
asymmetry. It is pinned by a test that runs the real `InitMachine` against the real
settings loader and asserts the effective post-merge state; pins written against the
seeded *file* pass whether the fix is present or not.

**Deferred, and named here so it is not lost: the predicate itself is still wrong.**
Correcting it would remove the unservable option from the menu rather than merely
demoting it — an operator can still select `remote (kubernetes)` today and get a dead
agent. It is deferred because `buildInfoProfiles` is shared with the multi-node Cloud Run
tier and with workstation brokers, where offering a remote profile is a legitimate
feature. **That makes it a product decision about other tiers, not a defect fix within
this one**, and this design does not get to make it unilaterally.

## 5. Durability — Tier 0, pure ephemeral

Workspaces and the SQLite control plane live on the Instance's ephemeral filesystem.
Two events destroy that state:

- **Redeploy** — chosen by the operator, who can save work first.
- **Exceeding the agent ceiling** (ptone/scion#1303) — not chosen and, on this tier,
  not currently anticipatable. There is no per-agent memory or CPU instrument (§9.1),
  so nothing warns before the Instance is destroyed and self-recovers empty. See §9.1
  for measured agent counts at each Instance size.

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

**Verified 2026-08-27.** The deploy sets `invokerIamDisabled: true`, so the Cloud Run
invoker check is off and IAP is the sole perimeter. A six-way header × token ×
audience matrix confirmed that IAP rejects all unauthenticated and mis-audienced
requests, and that the hub is unreachable without a valid IAP assertion.

**That verification covers the steady state. It says nothing about the deploy itself,
and the deploy is not atomic.** The Instance is created first and IAP is configured
afterwards, by a separate REST PATCH. Two windows follow from that ordering, and the
paragraph above closes neither:

- **Between create and PATCH**, the Instance exists. If it is routable in that interval
  with the invoker check already off, the perimeter is absent while it is reachable.
- **If the PATCH fails**, the deploy exits non-zero having already created an Instance.
  A failed deploy that leaves a running artifact behind is worse than one that leaves
  nothing, because the operator's mental model is "it failed, so nothing happened."

**Requirement: a deploy that does not finish must not leave a reachable Instance without
IAP.** Either the Instance is not routable until the PATCH lands, or a failed PATCH tears
the Instance down. **This is stated as a requirement and is NOT yet measured** — the
window's existence and duration are unknown, and "the create probably isn't serving yet"
is an assumption, not a finding. §10 criterion 12 exists to settle it.

The order of those two clauses matters. **"Fail closed" here means fail to a state with
no Instance, not fail to an Instance with no perimeter.** §6.1's footgun is that the open
configuration is the supported one; a partial deploy is the cheapest way to reach it by
accident.

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
- **No per-agent resource limits.** All agents share the Instance budget. Measured
  agent counts on a single observation (repeatability unmeasured):

  | Instance size | Idle agents | Working agents |
  |---|---|---|
  | 4 CPU / 8 GiB (default) | 20 | 6 |
  | 8 CPU / 32 GiB (maximum) | 51 | 14 |

  No per-CPU or per-GiB scaling rule is derivable from these two points: 4× memory
  and 2× CPU bought roughly 3× idle and 2× working capacity — non-linear in both
  resources.
- **Image-pull failures on first deploy are hard to diagnose.** The messages come
  from the Cloud Run sandbox launcher, not from Scion, and name a cache mirror rather
  than the requested image. Routed as platform feedback.
- **Sandbox stderr can be lost**, which makes an agent that dies during provisioning
  harder to diagnose than it should be. The general Scion path is well instrumented;
  the loss is specific to this runtime's sandbox handling.
- **No per-agent observability.** The Instance budget is shared (above) and also
  invisible. Cloud Monitoring covers `cloud_run_revision` (Services) but not
  `cloud_run_instance`; `getStats` returns hardcoded zeros; the hub agent list is
  wrong in both directions; sandboxes are gVisor processes invisible to Cloud
  Monitoring; `sshd` is absent from the omni image. The only working signal today is
  agent create latency. This is a known gap we intend to close, staged in two parts:
  per-agent logging (ptone/scion#1310) and CPU/memory visibility
  (ptone/scion#1311).

### 9.2 Upstream dependencies

Four defects found while building this tier are **not specific to it** and are filed
against the platform. Each affects other deployments; this tier is simply where they
surfaced first.

| Issue | Problem |
|---|---|
| ptone/scion#1273 | A hosted hub drops template and harness-config identity on agent create, and the broker then falls back to a local disk search that is always empty in hosted mode. |
| ptone/scion#1274 | `GitCloneConfig.Depth` is documented as `0 = full clone` and implemented as depth 1, in three independent call sites. A depth-1 workspace cannot push to any remote but origin. |
| ptone/scion#1275 | `noAuth: true` on agent create makes a request fail that succeeds without it. |
| ptone/scion#1276 | The auth preflight counts only `metadata_mode: assign` as an assigned GCP identity, so a host with ambient credentials from the real metadata server never satisfies `skipped_when_gcp_service_account_assigned`. Runtime-agnostic: affects GCE and GKE workload identity too. |

Until ptone/scion#1273 and ptone/scion#1276 land, this tier needs deploy-time workarounds. They are
stopgaps and should be removed when the fixes arrive.

**Update, 2026-08-27 — three of the four have landed upstream.**

| Issue | Fix | Landed as |
|---|---|---|
| ptone/scion#1273 | resolve implicit `default` template when none is specified | `fc523ecd` (PR GoogleCloudPlatform/scion#1305) |
| ptone/scion#1275 | skip env-gather when `noAuth` is true | `6edf6ed0` (PR GoogleCloudPlatform/scion#1304) |
| ptone/scion#1276 | auth preflight recognises passthrough GCP identity mode | `a30368aa` (PR GoogleCloudPlatform/scion#1306) |

So **the deploy-time stopgaps for ptone/scion#1273 and ptone/scion#1276 are now obsolete and should be
deleted.** They were operator settings rather than code, which is why this tier never
had to carry a workaround for them and never blocked on them — the §1 walkthrough was
completed end to end on 2026-08-25 with all four open.

**ptone/scion#1274 remains open** and is the one with a live consequence: a depth-1 workspace
cannot push to any remote but `origin`. That constrains §1's final step to
origin-only pushes. It is a real limitation of the tier as shipped, not a
theoretical one.

A fifth defect was filed after this section was first written — **ptone/scion#1281**, session-end
telemetry rejected with a 400 because `SessionID` is dropped in `Finalize()`, so
`exit_code` is never persisted. Also open, also not blocking.

A sixth, the `WebServer` access-settings split-brain, was **fixed upstream by GoogleCloudPlatform/scion#1300**
(`AccessSettingsProvider`) before it was ever filed from here. All browser login paths
now read live settings, so tightening access mode in the admin UI reaches browser
logins. Verified by reading the merged code, **not yet exercised on a live deployment** —
that retest is still outstanding.

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
    in two places (§4.1). **Verified — run.** The chained build produces the omni
    image and the result is verified by digest.
12. **A deploy interrupted or failed between Instance creation and IAP configuration
    leaves no Instance reachable without IAP** (§6.1). Verify by inducing the failure,
    not by reading the ordering. The check is whether an Instance exists *and answers*
    after a failed run — an Instance that exists but was never routable satisfies this;
    one that answers without an IAP challenge does not, for any length of time.
13. **The deploy script runs to completion on stock macOS with bash 3.2 and BSD
    userland** (§4.6), with no prerequisite installation step. Enforced by CI on a
    pinned native macOS runner. **Read the interpreter version the job prints before
    trusting a green** — a gate that runs the suite under the wrong shell passes for
    the wrong reason, and this one is checking for the absence of a runtime error, which
    is the failure mode most easily faked by not executing the line at all.

14. **A fresh deploy pre-selects a runtime profile the tier can actually serve** (§4.7),
    and the operator reaches a running agent without choosing one. Verify on a deploy
    that has never had its settings touched — the defect this replaces was invisible to
    every existing test because the tests asserted the seeded file rather than the
    effective settings after the merge.

**On 12, 13 and 14 the same caution applies, and it is the lesson of this tier so far:
all three are negative criteria.** "No unprotected Instance", "no bash-4 construct" and
"no unservable profile" are each satisfied by a check that never ran. None should be
recorded as met until it has been observed *failing* against a deliberately broken
input — for 14 specifically, against a reverted fix, because a profile test that passes
with the fix removed is the exact defect that was found and removed twice during its
review.
