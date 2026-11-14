# Hub stops the run on limits_exceeded (ptone/scion#7928)

**Date:** 2026-11-14

- A status report or broker heartbeat that moves the agent's current run (report run ID equals
  the row's) to activity `limits_exceeded` triggers `stopRunOnLimitsExceeded`
  (`pkg/hub/limits_exceeded_stop.go`): one background `DispatchAgentStop` per run per hub
  instance, then phase stopped unless the row already records the run's end. A failed dispatch
  forgets the run so a later report retries.
- The run intent is deliberately unchanged (as after a container's own exit): swapping it to
  stopped would stop `evaluateHalt` from halting a later platform disruption of the run
  (`TestHalted_SelfReportedLimitsThenRuntimeReasonHalts`).
- `stopCurrentRun` is the reusable system stop for the hub-enforced max_duration phase.
