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

### Fix plan (Part 1b) — phase-overwrite race (found during verification of 80c1579)

The threading fix is correct but its precondition fails: the hub sets `phase=suspended`
*after* dispatching the stop, then the dying container's async sciontool `/status` report
(and/or a broker heartbeat) reports `stopped`/`crashed` and overwrites `suspended` back to
`stopped` before the start handler reads it — so `resume := phase==suspended` is false.
The existing regression guards are ordinal-based and only cover *active* phases; both
`suspended` and `stopped` are ordinal 0, so the transition slips through.

Make `suspended` sticky against async status updates (explicit lifecycle start/stop bypass
these guards, so they can still leave suspended):
1. `pkg/hub/handlers.go` `guardAgentPhaseTransition` (~2988): if current phase is
   `suspended`, drop `status.Phase` and `status.Activity` from async `/status` reports.
2. `pkg/hub/handlers.go` broker-heartbeat path (~6345): treat `suspended` like a sticky
   phase — do not let a heartbeat-reported phase/terminal-activity revert it.
Add unit tests (suspended stickiness for both paths).

NOTE: a related but broader issue (stale reports from the OLD container landing AFTER a
resume and falsely setting `crashed` — the "false crash" side finding) is tracked under
Part 2 / task #4; not fixed here.

---

## Part 2 — Crashes never produce an error state

### What we found (corrected after deeper survey)

**Correction:** `sciontool init` IS the supervisor on ALL runtimes, including local Docker
— the agent image sets `ENTRYPOINT ["sciontool","init","--"]`
(`image-build/scion-base/Dockerfile:101`), so `docker run … sh -c "tmux …"` actually runs
`sciontool init -- sh -c "tmux …"`. The earlier "local Docker has no supervisor" claim was
wrong. This makes the fix unified across runtimes.

The real, universal gap:
1. **The supervised child is `sh -c "tmux new-session -d …"`, not the harness.** Because
   the tmux session is detached (`-d`) and the command chain ends with `attach-session`,
   the supervised `sh` exits with tmux/attach's status — never the harness pane's. So
   `result.code == 0` even when the harness exits non-zero → `isCrash` is false
   (`init.go:814`) → the crash path is essentially never taken. This is why crashes are
   never surfaced. (`pkg/runtime/common.go:444`, `pkg/sciontool/supervisor/supervisor.go`
   captures the child=sh exit code faithfully — it's just the wrong process.)
2. **"crash" ≠ "error" today.** Even when `isCrash` fires, it sets phase=`stopped` +
   activity=`crashed` (`init.go:831`), not `PhaseError`. `PhaseError` is currently set
   only on provisioning/clone failures (`pkg/agent/list.go:174`), never on a running-agent
   crash. (**OPEN Q4** — see below.)
3. **False crash observed:** the hub showed `activity=crashed`, `exit code -1` while the
   agent ran fine. Source not yet found in code (no `-1` literal; `runtimebroker/
   handlers.go:1611` hardcodes `ExitCode:0 // TODO`). Needs live log evidence from a real
   crash test on the VM. Likely a container-exit inspection or a stale report from a prior
   container instance landing after a (re)start.

### Q4 DECISION (hybrid)
Crash target state is hybrid: clean exit (code 0) → `stopped`; limits → `stopped` +
`limits_exceeded`; unexpected non-zero exit → **`PhaseError`** (restartable — `start`
clears it and runs a fresh session). To avoid state-validation conflicts (`crashed` is only
valid on `stopped`), represent a crash as `Phase=error`, activity cleared, with
`message="Agent crashed with exit code N"` and the exit code recorded. `PhaseError` is
already protected by `preserveTerminalPhase`, so it won't be reverted by async updates.

### Fix plan (Part 2)
- **Recover the harness's real exit code from tmux** (the core fix, all runtimes). Cleanest
  option: wrap the harness inside the tmux window so it writes its exit code to a known
  file — e.g. `tmux new-session -d -s scion -n agent 'sh -c "<harness>; echo $? >
  ~/.scion/agent-exit-code"'`. After the supervised `sh` returns, `sciontool init` reads
  that file and uses it as `finalCode` for the `isCrash` decision (fall back to the
  supervised code if the file is missing). Localized to `pkg/runtime/common.go` (tmux
  command) + `cmd/sciontool/commands/init.go` (read the file). Apply the same wrapping in
  the k8s tmux command (`pkg/runtime/k8s_runtime.go:901`).
- **Target state (pending Q4):** wire the chosen crash representation consistently through
  init.go → Hub status → DB → DisplayStatus.
- **False crash:** find and fix the path that sets crashed/-1 without a real harness exit
  (attribute crash reports to a specific container instance so stale reports are ignored).
- **Distinguish exit kinds:** clean exit (0) → stopped; limits → stopped+limits_exceeded;
  unexpected non-zero → the Q4 target. (Q5 about local-Docker parity is now moot — same
  path.)

### VM crash-evidence findings (instance-manager, commit a3c8ece)
- Process tree confirmed: `sciontool(PID1) → sh → tmux-client → tmux-server → claude`.
  The harness is a tmux **grandchild**, so its exit code is structurally invisible to the
  supervisor — confirms the exit-code-file fix is the right bridge.
- **`-1` source identified:** when killed by signal, **tmux-server is reaped as a zombie
  with exit code -1** by sciontool's zombie reaper. That's the spurious `-1` seen earlier.
  The fix (read a real exit-code file + use Docker `State.ExitCode`) avoids surfacing the
  reaper's -1.
- **Hard crash (SIGKILL claude → container exit 137):** the container DOES exit (session
  collapses → sh exits). But the hub ended up `phase=stopped`, **`activity=stalled`**
  (stale — a stopped agent should never be `stalled`), `message="Agent crashed with exit
  code 137"`. The message came from the **broker heartbeat inspecting Docker `Exited(137)`**
  — because sciontool's own status/shutdown report **401'd** (see below). So even today's
  partial crash signal comes from the broker, not sciontool.
- Implications for the fix:
  - The **broker-heartbeat path** that derives state from Docker `Exited(code)` must set
    the crash target (Q4) + `crashed` activity when `ExitCode != 0`, since it's the path
    that works even when sciontool can't report. Find where Docker exited-status is mapped
    to phase and enhance it.
  - On transition to stopped/error on crash, **clear a stale `stalled` activity** (replace
    with `crashed`). The `stalled` overwrite is a sticky-activity bug.

### Resume 401 — ROOT CAUSE CONFIRMED
`DispatchAgentStart` mints a valid agent JWT and places it in
`resolvedEnv["SCION_AUTH_TOKEN"]` (`pkg/hub/httpdispatcher.go:1086`). The broker's
`startAgent` passes `ResolvedEnv` into `buildStartContext` but does **not** set
`AgentToken` (`pkg/runtimebroker/handlers.go:1169`). In `buildStartContext`
(`pkg/runtimebroker/start_context.go:192-221`): step 1 copies the valid token from
`resolvedEnv` into `env`, then step 3 — because `in.AgentToken == ""` — takes the `else`
branch and **overwrites** `env["SCION_AUTH_TOKEN"]` with the broker's OWN
`os.Getenv("SCION_AUTH_TOKEN")` (a dev token that is not a valid 3-part JWT) → 401. The
CreateAgent path sets `req.AgentToken` (`handlers.go:592`), so initial start works; the
resume/start path doesn't, so it's clobbered. In production (broker has no
`SCION_AUTH_TOKEN`) the resolvedEnv token survives — so it manifests only under dev-auth,
but the start-vs-create asymmetry is a real latent bug.

**Recommended fix (minimal, provisioning-time):** in `buildStartContext`, only apply the
broker's dev `SCION_AUTH_TOKEN` when `env` does NOT already have one — i.e. never clobber a
hub-resolved token with the dev fallback. (Optionally also set `AgentToken` from
`resolvedEnv["SCION_AUTH_TOKEN"]` in the broker start path for parity with create.) This is
cleaner than a post-resume re-inject; the existing reset-auth/SIGUSR2 path
(`handlers.go:1617`) stays for genuine hub-disruption recovery.

### Separate bug found: resumed containers get a malformed hub token (401)
Resumed containers logged persistent `401 invalid agent token: ... compact JWS format must
have three parts` on every sciontool status/heartbeat call. The harness ran fine (Part 1
works), but the resumed agent cannot report status/heartbeat → broken observability, and it
exacerbates crash invisibility. May be a dev-auth-mode artifact on the VM or a real resume
token-provisioning gap. Tracked as its own task; needs isolation (does it occur with real
auth, or only dev-auth?).

---

## Part 3 — Auto-suspend (hibernate) stalled agents to reclaim resources

### DECISIONS (user)
- **Q6 home persistence: DEFER sync for now**, presume GCS later. Key realization: on
  Docker the agent home is a host bind-mount that survives container removal, so reclaiming
  the container and later resuming works WITHOUT any sync — the now-fixed suspend/resume
  handles it. GCS sync only matters for runtimes with ephemeral home (k8s/Cloud Run), which
  are gated on NFS anyway. So Part 3 on Docker needs no home-persistence work.
- **Q7 policy: hardwired** — auto-suspend after an ADDITIONAL 5 min of being stalled (≈10
  min total inactivity = StalledThreshold + 5m). Not configurable yet.
- **Deploy tip (user):** `make container-binaries` + `export SCION_DEV_BINARIES=<.build/
  container>` makes the hub bind-mount dev `scion`+`sciontool` into agent containers
  (`pkg/runtime/common.go:358`), so sciontool changes can be side-loaded WITHOUT an image
  rebuild. There's also an admin maintenance action that runs the rebuild.

### Implementation plan (Part 3, minimal)
- Add a recurring scheduler handler (mirror `agentStalledDetectionHandler` in
  `pkg/hub/server.go`): find agents with `activity==stalled` whose `last_activity_event` is
  older than `StalledThreshold + 5m`, heartbeat still recent (alive/resumable), and whose
  harness supports resume; auto-suspend them.
- Factor the suspend core out of `handleAgentLifecycle` (case `AgentActionSuspend`,
  ~handlers.go:3052) into a reusable internal `suspendAgent(ctx, agent)` (validate resume
  capability, set phase=suspended, syncWorkspaceOnStop, DispatchAgentStop) called by both
  the HTTP handler and the scheduler.
- Guardrails: only `running`+`stalled` agents; skip harnesses without resume support (can't
  hibernate what we can't resume — leave them stalled); `blocked`/`waiting_for_input` are
  already not `stalled`.
- Hardwired `autoSuspendStalledGrace = 5 * time.Minute`.

### Original survey notes

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
