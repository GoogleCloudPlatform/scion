# Telemetry GCP evidence runbook

**Status:** Phase 4 R5 accepted only for Claude Code 2.1.273 logs first; Phase 5 source verification and deployment gate pending. This procedure requires an approved, isolated fixture. It does not authorize changing active agents, shared settings, or infrastructure.

## Pin and record the fixture

Before starting, record UTC start and end, integration commit, built `scion` and `sciontool` SHA256, container image digest, installed harness name/version, receiver configuration, Cloud project/log name, exact Scion agent ID, project ID, harness label, and a unique nonsecret marker. Keep the marker and any sensitive controls out of credentials. Preserve an inventory of running and total container IDs and hashes of the shared binaries/configuration. Use a task-owned fixture and short-lived credential; never copy real user content into an evidence fixture.

For the accepted Phase 4 R5 example, the commit was `ed8cb776e2659aae36b877f4d4d0888e43cceee4`, installed harness was Claude Code 2.1.273, and the Cloud Logging destination was `projects/deploy-demo-test/logs/scion-agents`. The restricted evidence and exact query results are in the task scratch area documented by `phase4-r5-native-live-acceptance.md`. These values identify that historical fixture, not a reusable test identity.

## Source to receiver to Cloud counts

1. Capture the fixture's native OTLP source requests at the loopback receiver with exact timestamps, signal type, instrumentation scope and event names. Keep raw payloads restricted. Record SHA256 and a value-free parsed summary. Record normalized hook subprocess inputs separately; synthetic native-shaped requests are local test fixtures and must never be described as installed-vendor emission.
2. Capture receiver final diagnostics for spans, metrics and logs: Accepted, Filtered, Rejected, Delivered, Failed, SDK errors, Dropped, Unconfirmed, and queue bytes/records/entries. On clean Stop, require every accepted permitted record to be delivered with zero residual and zero terminal uncertainty. If the backend fails, report accepted versus delivered and terminal uncertainty separately; successful ingress is not Cloud proof.
3. Query Cloud Logging within a fixed, recorded visibility window using the exact fixture agent ID, project ID, harness label, log name and UTC time interval. Save the exact filter, query time, response SHA256, unique insert IDs, event names, scope and resource fields. Query twice if the first readback is incomplete, within the predeclared deadline. Match unique Cloud rows to permitted source and receiver counts. Cloud Trace and Monitoring require their own backend readback before making a Cloud delivery claim for those signals.

Example read-only Logging query after setting task fixture values (save the literal expanded filter and UTC query time with the JSON):

```sh
GCP_PROJECT=your-project-id
AGENT_ID=your-fixture-agent-id
PROJECT_ID=your-fixture-project-id
HARNESS=claude
WINDOW_START=2026-09-20T00:00:00Z
WINDOW_END=2026-09-20T00:05:00Z
FILTER="logName=\"projects/${GCP_PROJECT}/logs/scion-agents\" AND timestamp>=\"${WINDOW_START}\" AND timestamp<\"${WINDOW_END}\" AND labels.\"scion.agent.id\"=\"${AGENT_ID}\" AND labels.\"scion.project.id\"=\"${PROJECT_ID}\" AND labels.\"scion.harness\"=\"${HARNESS}\""
gcloud logging read "$FILTER" --project "$GCP_PROJECT" --format=json
```

Confirm the actual backend field paths from a value-free sample before relying on the filter; record any corrected literal query. Never infer absence from a broad time-window query alone.

The accepted R5 fixture had five installed native Claude logs: one `user_prompt`, two `api_request`, two `assistant_response`. The receiver filtered the prompt and delivered seven logs total: four retained native records and three lifecycle hook records. Cloud Logging returned seven unique exact-ID rows in the fixed window. Receiver diagnostics also showed spans 3/3 and metrics 1/1 Accepted/Delivered with zero failure/residual, but no independent Trace or Monitoring backend readback was performed. A host-network model metadata-shim request count was not captured.

## Privacy controls

Use a harmless visible marker as a positive control in an allowed event. Use distinct synthetic sensitive markers for body, `message`, response, request/opaque IDs, prompt, tool input/output and span status paths. Check the receiver destination and Cloud rows for the safe marker and for absence or `[REDACTED]` replacement of each sensitive marker. Check the hashed session ID shape and cross-record consistency without storing the original ID. Test a `user_prompt` negative control under the default Claude policy, then an explicit allow configuration to prove that mandatory content and opaque-ID redaction still applies. An absent event only proves filtering after a positive control appears and the finite visibility window closes. The accepted R5 Cloud records retained redacted `assistant_response` records and did not contain the default-filtered prompt.

## Metrics and identity

Keep hook and native instrumentation scopes separate. For two or more independently launched hook processes inside one 15-second batching interval, record each input event and exact expected increments, then compare the final exported counter total. Do not infer tool duration from unpaired hook ends. For metrics, record name, type, unit, resource, scope, temporality, original source interval, exported interval, reset and retry sequence. Compare source increments with Cloud Monitoring values only after backend readback; avoid interpreting a collector observation timestamp as source time. A synthetic metric fixture establishes local semantics only. For the accepted Claude native GCP route, native exporter metrics are disabled; normalized hook metrics remain enabled. Do not claim Claude native Cloud Monitoring metrics from this route. Query two distinguishable resources/scopes to prove they do not collapse; inspect authoritative Scion identity and retained native service identity.

## Failure, cleanup and interpretation

Use a local fake destination for induced permanent/transient failure and bounded Stop tests. Record fixed-cardinality diagnostics, attempts, queue residual and the final Stop error; never inject failure into production telemetry. For live runs, capture post-run hub/broker health and exact running/total inventory, remove only the exact stopped fixture container and task credential, then compare inventory and shared binary/config hashes. Preserve restricted raw evidence under a task-owned access-controlled scratch directory and publish only nonsecret summaries and hashes.

Capability labels must be precise: **accepted** means installed emitter plus receiver and backend proof for the named version and signal; **receiver delivered** means exporter acknowledgment without independent backend readback; **synthetic** means a local OTLP-shaped fixture; **unsupported** means the installed version lacks a usable route; **untested** means no evidence was gathered. Phase 4 R5 accepts bounded Claude Code 2.1.273 GCP logs-first native emission, privacy and cleanup. Gemini 0.52 Cloud-positive native routing remains unsupported; Codex native emission is untested. Phase 5 tests and any later deployment/live gate require separate review and acceptance.
