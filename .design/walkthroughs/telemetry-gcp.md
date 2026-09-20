# Telemetry GCP evidence runbook

**Status:** Phase 4 R5 accepted only for Claude Code 2.1.273 logs first; Phase 5 source verification and deployment gate pending. This procedure requires an approved, isolated fixture. It does not authorize changing active agents, shared settings, or infrastructure.

## Pin and record the fixture

Before starting, record UTC start and end, integration commit, built `scion` and `sciontool` SHA256, container image digest, installed harness name/version, receiver configuration, Cloud project/log name, exact Scion agent ID, project ID, harness label, and a unique nonsecret marker. Keep the marker and any sensitive controls out of credentials. Preserve an inventory of running and total container IDs and hashes of the shared binaries/configuration. Use a task-owned fixture and short-lived credential; never copy real user content into an evidence fixture.

For the accepted Phase 4 R5 example, the commit was `ed8cb776e2659aae36b877f4d4d0888e43cceee4`, installed harness was Claude Code 2.1.273, and the Cloud Logging destination was `projects/deploy-demo-test/logs/scion-agents`. The restricted evidence and exact query results are in the task scratch area documented by `phase4-r5-native-live-acceptance.md`. These values identify that historical fixture, not a reusable test identity.

## Effective configuration checks on isolated fixtures

Use only an approved task-owned project, home, agent name and exact fixture container ID. Set `TASK_EVIDENCE_DIR` to a mode `0700` task-owned scratch directory before collecting any environment or raw telemetry. Keep the global settings file in that task home and project settings in that task project; record their hashes, the selected template, and the explicit environment passed to the fixture. Do not edit the operator's home, a shared project, or an active agent. The owner starts and removes each dedicated fixture by its exact ID after checking that it has stopped.

For an enabled fixture, set `schema_version: "1"` and `telemetry.enabled: true`, `telemetry.cloud.enabled: true`, `telemetry.cloud.provider: gcp` in the task settings. Read the exact container's environment with `docker inspect "$FIXTURE_CONTAINER_ID" --format '{{range .Config.Env}}{{println .}}{{end}}' > "$TASK_EVIDENCE_DIR/fixture-env.txt"` into the restricted evidence directory; this file may contain credentials and must not be printed or shared. Confirm `SCION_TELEMETRY_ENABLED=true`, `SCION_TELEMETRY_CLOUD_ENABLED=true`, `SCION_TELEMETRY_CLOUD_PROVIDER=gcp`, the expected filter controls, and the harness's effective loopback OTLP endpoint/port. For Claude, confirm `CLAUDE_CODE_ENABLE_TELEMETRY=1` and loopback `OTEL_EXPORTER_OTLP_ENDPOINT`; Claude native GCP metrics must be disabled while hook metrics remain available. Confirm the receiver actually listens only on loopback at that port before interpreting missing source records.

For a separate disabled fixture, put `telemetry.enabled: false` in its task project settings. Confirm `SCION_TELEMETRY_ENABLED=false`, no native harness telemetry enablement, and no receiver/listener or export diagnostics for that fixture. For precedence, use distinct nonsecret endpoint sentinels at global and project levels, then an explicit fixture environment override. The expected order is global < project < template < explicit environment; inspect the exact fixture's final environment and receiver configuration at each step. A setting present in a file is not evidence that it became effective. Restore or remove only the task-owned files and stopped fixture IDs after recording results.

## Source to receiver to Cloud counts

1. Capture the fixture's native OTLP source requests at the loopback receiver with exact timestamps, signal type, instrumentation scope and event names. Keep raw payloads restricted. Record SHA256 and a value-free parsed summary. Record normalized hook subprocess inputs separately; synthetic native-shaped requests are local test fixtures and must never be described as installed-vendor emission.
2. Capture receiver final diagnostics for spans, metrics and logs: Accepted, Filtered, Rejected, Delivered, Failed, SDK errors, Dropped, Unconfirmed, and queue bytes/records/entries. On clean Stop, require every accepted permitted record to be delivered with zero residual and zero terminal uncertainty. If the backend fails, report accepted versus delivered and terminal uncertainty separately; successful ingress is not Cloud proof.
3. Query Cloud Logging within a fixed, recorded visibility window using the exact fixture agent ID, project ID, harness label, log name and UTC time interval. Save the exact filter, query time, response SHA256, unique insert IDs, event names, scope and resource fields. Query twice if the first readback is incomplete, within the predeclared deadline. Match unique Cloud rows to permitted source and receiver counts. Cloud Trace and Monitoring require their own backend readback before making a Cloud delivery claim for those signals.

Example read-only Logging query after setting task fixture values. The task evidence directory must be on access-controlled durable scratch storage. Disable terminal and CI transcript capture for this command; never print raw rows or attach them to shared reports:

```sh
TASK_EVIDENCE_DIR=/path/to/restricted/task-evidence
umask 077
mkdir -p "$TASK_EVIDENCE_DIR"
chmod 700 "$TASK_EVIDENCE_DIR"
GCP_PROJECT=your-project-id
AGENT_ID=your-fixture-agent-id
PROJECT_ID=your-fixture-project-id
HARNESS=claude
WINDOW_START=2026-09-20T00:00:00Z
WINDOW_END=2026-09-20T00:05:00Z
FILTER="logName=\"projects/${GCP_PROJECT}/logs/scion-agents\" AND timestamp>=\"${WINDOW_START}\" AND timestamp<\"${WINDOW_END}\" AND labels.\"scion.agent.id\"=\"${AGENT_ID}\" AND labels.\"scion.project.id\"=\"${PROJECT_ID}\" AND labels.\"scion.harness\"=\"${HARNESS}\""
gcloud logging read "$FILTER" --project "$GCP_PROJECT" --format=json > "$TASK_EVIDENCE_DIR/cloud-rows.json"
sha256sum "$TASK_EVIDENCE_DIR/cloud-rows.json" > "$TASK_EVIDENCE_DIR/cloud-rows.sha256"
python3 - "$TASK_EVIDENCE_DIR/cloud-rows.json" <<'PY'
import collections, json, sys
rows = json.load(open(sys.argv[1]))
if not isinstance(rows, list):
    raise SystemExit("readback is not a row list")
# Replace these counts with the approved fixture plan before each new run.
expected = {"assistant_response": 2, "api_request": 2, "agent.session.end": 1,
            "agent.lifecycle.post_start": 1, "agent.lifecycle.pre_start": 1}
counts = collections.Counter(row.get("jsonPayload", {}).get("event.name", "") for row in rows)
positive = sum(counts[name] for name in expected)
unexpected = sum(count for name, count in counts.items() if name not in expected)
ids = {row.get("insertId") for row in rows if row.get("insertId")}
print("positive_records", positive, "unexpected_names", unexpected, "unique_insert_ids", len(ids))
if positive == 0 or unexpected or counts != collections.Counter(expected) or len(ids) != len(rows):
    raise SystemExit("readback does not match the approved fixture plan")
PY
```

Confirm the actual backend field paths from a value-free sample before relying on the filter; record any corrected literal query and its hash. Publish only value-free counts and hashes. Never infer absence from a broad time-window query alone. The restricted Phase 4 R5 evidence directory contains `preflight.sh`, `launch-fixture.sh`, `fixture.stderr`, `native-event-summary.txt`, `cloud-exact-id.json`, and `cleanup-fixture.sh` as pinned examples; inspect them in restricted storage before adapting a new task-owned capture. Do not run the historical launch or cleanup script against current agents.

The accepted R5 fixture had five installed native Claude logs with an empty OTLP `LogRecord.EventName` and string `event.name` attributes: one `user_prompt`, two `api_request`, two `assistant_response`. The receiver filtered the prompt and delivered seven logs total: four retained native records and three lifecycle hook records. Cloud Logging returned seven unique exact-ID rows in the fixed window. Receiver diagnostics also showed spans 3/3 and metrics 1/1 Accepted/Delivered with zero failure/residual, but no independent Trace or Monitoring backend readback was performed. A host-network model metadata-shim request count was not captured.

## Privacy controls

Use a harmless visible marker as a positive control in an allowed event. Use distinct synthetic sensitive markers for body, `message`, response, request/opaque IDs, prompt, tool input/output and span status paths. Check the receiver destination and Cloud rows for the safe marker and for absence or `[REDACTED]` replacement of each sensitive marker. Check the hashed session ID shape and cross-record consistency without storing the original ID. Test a `user_prompt` negative control under the default Claude policy, then an explicit allow configuration to prove that mandatory content and opaque-ID redaction still applies. An absent event only proves filtering after a positive control appears and the finite visibility window closes. The accepted R5 Cloud records retained redacted `assistant_response` records and did not contain the default-filtered prompt.

## Metrics and identity

Keep hook and native instrumentation scopes separate. For two or more independently launched hook processes inside one 15-second batching interval, record each input event and exact expected increments, then compare the final exported counter total. Do not infer tool duration from unpaired hook ends. For metrics, record name, type, unit, resource, scope, temporality, original source interval, exported interval, reset and retry sequence. Compare source increments with Cloud Monitoring values only after backend readback; avoid interpreting a collector observation timestamp as source time. A synthetic metric fixture establishes local semantics only. For the accepted Claude native GCP route, native exporter metrics are disabled; normalized hook metrics remain enabled. Do not claim Claude native Cloud Monitoring metrics from this route. Query two distinguishable resources/scopes to prove they do not collapse; inspect authoritative Scion identity and retained native service identity.

## Failure, cleanup and interpretation

Use a local fake destination for induced permanent/transient failure and bounded Stop tests. Record fixed-cardinality diagnostics, attempts, queue residual and the final Stop error; never inject failure into production telemetry. For live runs, capture post-run hub/broker health and exact running/total inventory, remove only the exact stopped fixture container and task credential, then compare inventory and shared binary/config hashes. Preserve restricted raw evidence under a task-owned access-controlled scratch directory and publish only nonsecret summaries and hashes.

## Owner-only rollback gate

Before a pinned rollout, the owner records the prior and candidate source commits, executable SHA256, container image digests, service unit and launch configuration, and exact task fixture image mapping in the restricted audit directory. Preserve the currently installed executable as a uniquely named, hash-verified rollback copy before replacing it. Record pre-rollout `/healthz` status and version, database and connected-broker counts, `/login` result, web asset hash, shared configuration and collector binary hashes, and sorted running/total container inventories. The reviewed release manifest must bind the candidate executable and image digest to its commit; stop if any preflight hash differs. The Phase 4 controlled hub restore pattern is documented in the restricted `phase4-deploy-ed8cb776.sh` audit script; adapt and review it for the new exact paths and hashes rather than rerunning a historical script.

The owner triggers rollback if the pinned artifact or image hash differs, the scoped service restart or health gate fails, database/broker or public `/login` health regresses, original container inventory or shared configuration changes unexpectedly, or the bounded telemetry window shows missing positive controls, a privacy leak, failed delivery, or unexplained residuals. Stop only the exact task fixture if required. Under the reserved maintenance window, the owner verifies the saved prior executable against its recorded SHA256, stages that exact copy, atomically restores the owner-controlled service path, and restarts only the recorded service. Restore a prior task fixture image mapping only for that fixture; do not change shared agent defaults or remove unrelated containers. Record the restore action, operator, UTC time, exact paths, hashes and service result in restricted audit storage.

After restore, require the installed executable SHA256 and version to equal the recorded prior values; verify active service, `/healthz` with healthy database and at least baseline broker count, `/login`, pinned web asset hash, unchanged shared config/collector hashes, and running/total container inventories against preflight. Record exact deviations and keep the rollout blocked if any check fails. Root/owner decides any further recovery; a local source test or receiver acknowledgment cannot clear a failed live rollback gate.

Capability labels must be precise: **accepted** means installed emitter plus receiver and backend proof for the named version and signal; **receiver delivered** means exporter acknowledgment without independent backend readback; **synthetic** means a local OTLP-shaped fixture; **unsupported** means the installed version lacks a usable route; **untested** means no evidence was gathered. Phase 4 R5 accepts bounded Claude Code 2.1.273 GCP logs-first native emission, privacy and cleanup. Gemini 0.52 Cloud-positive native routing remains unsupported; Codex native emission is untested. Phase 5 tests and any later deployment/live gate require separate review and acceptance.
