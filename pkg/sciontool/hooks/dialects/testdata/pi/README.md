# pi bridge payload fixture

`bridge-payloads.jsonl` is the exact stdin that `harnesses/pi/home/.pi/agent/extensions/scion-bridge.js`
writes to `sciontool hook --dialect=pi` for the scripted session in
`harnesses/pi/scion-bridge.test.mjs` (one JSON object per line).

- The pi events fed to the bridge are synthetic, shaped like pi 1.3.0's extension event types.
  They are not a capture from a live pi.
- `scion-bridge.test.mjs` asserts that the bridge still produces this file byte for byte.
  To regenerate it after an intended bridge change, run
  `SCION_PI_UPDATE_FIXTURE=1 node --test scion-bridge.test.mjs` from `harnesses/pi`.
- `pkg/sciontool/hooks/handlers/pi_dialect_test.go` parses the file through `harnesses/pi/dialect.yaml`.
  It checks event names, fields, readiness and status activity, and that every mapping is exercised.
- `cmd/sciontool/commands/harness_ready_test.go` uses the first record as pi's session-start readiness case.
