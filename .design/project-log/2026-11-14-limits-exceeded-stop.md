# Hub stops the run on limits_exceeded (ptone/scion#7928)

**Date:** 2026-11-14

- Scope: runtimes the platform may restart an exited container on. Brokers advertise them in the
  `restartingRuntimes` capability (Cloud Run Instances today). Docker, Podman and Kubernetes,
  and brokers that predate the capability, get no stop. On Kubernetes a stop deletes the pod and
  its logs.
- A report or heartbeat that moves the current run (matching run ID) to `limits_exceeded` arms
  one stop per run (`pkg/hub/limits_exceeded_stop.go`). The stop is dispatched 2 minutes later
  so that sciontool's shutdown completes first (SIGTERM grace, session-end hooks, final exit-10
  report, which sets phase stopped). That self-reported stop does not cancel it. A user or hub
  stop (run intent stopped), suspend, halt, delete, reincarnation or a new run does.
- The run intent is unchanged, so `evaluateHalt` can still halt a later platform disruption.
- `stopCurrentRun` is the reusable system stop for the hub-enforced max_duration phase.
