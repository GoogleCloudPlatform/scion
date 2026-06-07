# Agent State & Container Lifecycle Fixes

## Status
**Preliminary draft / survey** | branch `scion/state-fixes` | June 2026

This is an initial survey + scoped-work draft covering three related problems in how
agent state is represented and kept current across lifecycle transitions, and how that
ties to the underlying container lifecycle. It is intentionally incomplete — open
questions are flagged inline and consolidated at the end.

## Decisions (from user Q&A)

- **Q1 — Target runtime.** Docker is the primary runtime today and the place to fix
  things first (multiple integration environments available for repro). **Other runtimes
  must not be allowed without NFS** — gate them on NFS being configured. The design must
  still *plan* for all deploy modes and runtimes, but Docker is the proving ground.
  - Implication for Part 1: on Docker the home + workspace are host bind-mounts that
    survive container recreation, AND the resume-flag flow is correct end-to-end
    (suspend writes `phase=suspended`; `GetSavedPhase` reads it; `effectiveResume=true`;
    `claude --continue` is emitted). So the Docker resume failure is NOT home-loss — it
    needs a live reproduction in an integration env to pin the true cause (candidates:
    `--continue` not matching the prior session by cwd, flag/quoting interference in the
    tmux wrapper, or a non-obvious symptom).

- **Q2 — Resume success criterion.** Resume must be a true **harness continuation of
  the last conversation**, using the harness-specific resume flag as implemented in the
  harness config adapter (Claude `--continue`, etc.). "Container back with files intact
  but a fresh session" is NOT acceptable.

## Test environment

- VM `scion-integration` (project `deploy-demo-test`, zone `us-central1-a`); hub at
  `https://integration.projects.scion-ai.dev` (Caddy → localhost:8080). Built from
  `scripts/starter-hub`. Currently running branch `postgres/wave-b-integration` on a
  Postgres DB.
- Access is proxied by the `state-fix-instance-manager` agent (this workstream lacks
  compute perms on the project). Deploy loop: push branch → instance-manager pulls on
  VM, `go build -o scion ./cmd/scion`, swap binary, restart hub.
- **Branch base — DECIDED:** state-fixes is based on `main` (currently zero code delta).
  `postgres/wave-b-integration` was an unrelated project and is being replaced on the VM.
  Workflow: push `scion/state-fixes` → redeploy on the integration VM → retest. The VM's
  Postgres DB from the wave-b work is reset as needed for a clean main-based deploy.

## Background: how state works today

- **State model** (`pkg/agent/state/state.go`): two orthogonal axes.
  - `Phase` (infrastructure lifecycle): `created → provisioning → cloning → starting →
    running → {suspended} → stopping → stopped`, plus terminal `error`.
  - `Activity` (what a running agent is doing): `working, thinking, executing,
    waiting_for_input, blocked, completed, limits_exceeded, stalled, offline, crashed`.
  - Source of truth: in-container `agent-info.json` (written by hook handlers), relayed
    to the Hub via heartbeat; Hub DB is authoritative once stopped.
- **Suspend/resume** (`.design/suspend-resume-design.md`, `cmd/suspend.go`,
  `cmd/resume.go`, `cmd/common.go`): suspend = `docker stop` + phase=`suspended`.
  Resume = `RunAgent(resume=true)` → `mgr.Start` which **deletes the stopped container
  and creates a new one** (`pkg/agent/run.go:101`), passing the harness resume flag.
- **Crash/exit handling** (`cmd/sciontool/commands/init.go:802-869`,
  `pkg/sciontool/supervisor/supervisor.go`): `sciontool init` supervises a child,
  captures its exit code, and on non-zero maps to phase=`stopped` + activity=`crashed`.
- **Stall detection** (`pkg/hub/server.go`, `MarkStalledAgents`): a scheduler marks an
  agent `stalled` when `last_activity_event` is older than `StalledThreshold` (default
  5m) AND heartbeat is recent (<2m). `blocked` agents are exempt. No action is taken
  beyond setting the status.

---

## Part 1 — Resume does not correctly restart the container with the resume flag

### What we found
The resume flag **is** plumbed end-to-end:
`cmd/resume.go` → `RunAgent(resume=true)` → `effectiveResume` (`cmd/common.go:459`) →
`api.StartOptions.Resume` → `runtime.RunConfig.Resume` (`pkg/agent/run.go:889`) →
`config.Harness.GetCommand(task, resume, args)` (`pkg/runtime/common.go:428`) →
harness adds `--continue` (Claude) / `--resume` (Gemini).

So the flag reaches the harness. The likely failure modes are therefore **not** "the
flag is missing" but one or more of:

1. **Resume recreates the container instead of restarting it.** `mgr.Start` deletes the
   stopped container and `docker run`s a fresh one (`run.go:100-104`). Harness session
   continuity depends entirely on session files surviving in the agent **home**.
2. **Agent home is ephemeral on hub runtimes.** Home is a host bind-mount on Docker/
   Podman (survives), but on **Kubernetes and Cloud Run the home is in-image/in-pod and
   NOT NFS-backed** (storage survey). When the pod is deleted on resume, the harness
   session history is gone, so `--continue` starts a fresh session — looking like
   "resume didn't work."
3. **tmux wrapping.** The harness runs inside `tmux new-session` (`common.go:444`); if
   the resume args are mis-quoted or the harness re-execs with a filtered env, the
   resume flag could be dropped. Needs runtime-specific confirmation.

### CONFIRMED ROOT CAUSE (Docker, hub/broker path — repro on the integration VM, June 2026)

The resume flag is accepted at the API layer (`CreateAgentRequest.Resume`) but is **never
threaded through the hub→broker→runtime pipeline**, so `Harness.GetCommand` is called with
`resume=false` and `--continue` is never added. The resumed container runs the identical
command as a fresh start (and even re-injects the original task). Everything else is
correct: the new container reuses the same home bind-mount, the workspace/cwd is identical
(`/workspace`, encoded `-workspace`), and the prior Claude session `.jsonl` survives in
`~/.claude/projects/-workspace/` — only the flag is missing.

Trace of the gap:
- `pkg/hub/handlers.go` CreateAgent handler (~9149-9170) and wake handler (~2399) call
  `dispatcher.DispatchAgentStart(ctx, agent, task)` **without** any resume intent. No
  special handling for `suspended` agents.
- `pkg/hub/httpdispatcher.go` `DispatchAgentStart` (~966) has no resume param; calls
  `client.StartAgent(...)` (~1165) without it. `dispatch_args.go` `StartDispatchArgs` has
  only `Task`.
- `pkg/hub/broker_http_transport.go` `StartAgent` (~164) builds a payload with no
  `resume` field. `pkg/hub/brokerclient.go` interface (~47) signature lacks it.
- `pkg/runtimebroker/handlers.go` `startAgent` (~1128) has a fallback: read
  `GetSavedPhase` from disk and set `opts.Resume=true` if `suspended` (~1208-1214) — but
  this only works for local-filesystem projects, NOT hub-managed projects, so it fails on
  the deployed hub.
- `pkg/runtime/common.go:428` `GetCommand(task, config.Resume, args)` and the Claude
  harness (`pkg/harness/claude_code.go:78`, adds `--continue` when resume) are already
  correct — they just never receive `resume=true`.
- There is no `AgentActionResume` and no `/resume` HTTP route — start and resume are the
  same action (explains the `/resume` 404).

### Fix plan (Part 1)
Thread an explicit `resume bool` from the hub (source of truth) to `RunConfig.Resume`:
1. Hub computes `resume := existingAgent.Phase == PhaseSuspended` (mirrors local
   `effectiveResume`: suspended→resume, stopped→fresh) in the CreateAgent and wake paths,
   and passes it to `DispatchAgentStart`.
2. Add `resume` param through `DispatchAgentStart` → `StartDispatchArgs` →
   `BrokerClient.StartAgent` → HTTP payload (`"resume": true`).
3. Broker `startReq` gains `Resume bool`; handler sets `opts.Resume` from it (keep the
   `GetSavedPhase` read as a fallback only).
4. `opts.Resume → RunConfig.Resume → GetCommand` is already wired — no change needed.
5. On a pure resume (no new message), do **not** re-inject the original creation task
   (pass empty task so the harness just continues); a wake-with-message still passes that
   message. (Flag if this turns out larger than expected.)
Optional follow-up: add a first-class `AgentActionResume` + `/resume` route for clarity.

---

## Part 2 — Crashes never produce an error state

### What we found
Two distinct gaps:

1. **The supervised child is `tmux`, not the harness.** `sciontool init` runs
   `childArgs`, which is the tmux command (`init.go:1683` comment; `common.go:444`
   builds `tmux new-session ... '<harness>'`). tmux exits with **its own** status, not
   the harness pane's. A harness crash (non-zero exit) is masked → `result.code == 0` →
   `isCrash` is false (`init.go:814`). So the crash path is essentially never taken.
2. **Even when detected, "crash" ≠ "error".** The crash path sets phase=`stopped` +
   activity=`crashed` (`init.go:831`), not `PhaseError`. The user expects an *error
   state*. (**OPEN Q4** — should a crash be terminal `error`, or `stopped`+`crashed`?)
3. **Local Docker has no supervisor at all.** The local `docker run` CMD is
   `sh -c "tmux ..."` (`common.go:465`) with no `sciontool init` wrapping, so none of
   the crash-detection logic runs for local runs.

### Proposed scope (draft)
- Make the harness exit code observable to the supervisor. Options:
  - (a) Run the harness with `tmux ... ; echo $? > /exit-code` style capture, or set
    `set-option remain-on-exit` + poll pane dead status + `#{pane_dead_status}`.
  - (b) Have the harness invoked under a thin wrapper that writes its real exit code to
    a known file that `sciontool init` reads after tmux returns.
  - (c) Longer-term: don't wrap the supervised harness in detached tmux for exit-code
    purposes — supervise the harness directly and attach tmux as a viewer.
- Decide the target state (Q4) and wire it consistently into Hub status + DB + display.
- Decide whether local Docker runs need crash→error parity (**OPEN Q5**), since they
  lack the supervisor today.

---

## Part 3 — Auto-suspend (hibernate) stalled agents to reclaim resources

### What we found
- Stall detection already exists and is reliable (`MarkStalledAgents`), distinguishing
  `stalled` (alive but idle) from `offline` (no heartbeat) and exempting `blocked`.
- There is **no** action wired to stall today — it's purely a status.
- Auto-suspend is already named as a "Future Consideration" in the suspend/resume design.
- The blocker for hibernation is **home persistence** (same as Part 1): to reclaim the
  container we must be able to restore the agent home on resume.

### Proposed scope (draft)
- Add a configurable policy: after an agent is `stalled` for `AutoSuspendThreshold` AND
  its harness supports resume, transition it to `suspended` and reclaim the container.
- Preserve agent home before reclaiming. Storage options (**OPEN Q6**):
  - (a) Sync home → GCS (reuse `pkg/gcp/storage.go` rclone helpers), restore on wake.
  - (b) Dedicated NFS subpath for home (reuse NFS backend; needs per-agent isolation).
  - (c) Hybrid: NFS for hub clusters that already mount it, GCS otherwise.
- Wake path: on next message to a hibernated agent, restore home, resume container,
  reattach harness session. Reuse the existing Hub wake flow (`handleAgentMessage`
  Wake=true).
- Guardrails: never auto-suspend `blocked` or `waiting_for_input` agents; make threshold
  and the whole feature opt-in (**OPEN Q7**).

---

## Consolidated open questions

1. **Resume bug — which runtime?** Where have you observed resume failing — local Docker,
   Kubernetes, or Cloud Run? (Determines whether home-loss is the cause.)
2. **Resume success criterion.** When resume "works," what should the user observe — the
   harness literally continuing the prior conversation, or just the container coming back
   with working files intact?
3. **Home persistence preference.** For preserving the agent home (needed for both robust
   resume and hibernation): GCS sync, a dedicated NFS subpath, or hybrid? Any existing
   bucket/NFS share we should target?
4. **Crash target state.** Should a harness/container crash land in terminal `error`, or
   in `stopped` + `crashed`? Is `error` meant to be recoverable (restartable) or purely
   a dead-end signal?
5. **Local Docker parity.** Do crash→error and auto-suspend need to work for local Docker
   runs, or are these hub/k8s/Cloud-Run concerns only? (Local Docker has no supervisor.)
6. **Hibernation storage.** Same as Q3 but specifically for the auto-suspend flow — is GCS
   acceptable for home snapshots, including any latency on wake?
7. **Auto-suspend policy.** What idle threshold feels right (the stall threshold is 5m)?
   Should auto-suspend be global, per-template, or per-agent? Opt-in or default-on?
8. **Sequencing.** Confirm the intended order: (1) fix resume, (2) fix crash→error,
   (3) auto-suspend/hibernate — with home-persistence as shared infrastructure for 1 & 3.
