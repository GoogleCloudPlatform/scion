# Agent observability: first integration milestone

Status: Phase 1 accepted with live evidence; Phase 2 deployed but its live gate remains open after a session-count Cloud series collision. Phases 3–5 remain pending.
Updated: 2026-09-19.

## Outcome and evidence labels

This milestone makes agent telemetry trustworthy from an installed harness,
through the container-local OpenTelemetry receiver, to Cloud Trace, Cloud
Logging, and Cloud Monitoring. The intended end state is:

```text
native harness OTLP ─┐
hook SDK OTLP ───────┼─> loopback receiver -> policy/identity boundary
init SDK OTLP ───────┘                         -> bounded forwarding pipeline
                                                    ├─> Cloud Trace
                                                    ├─> Cloud Logging
                                                    └─> Cloud Monitoring
```

The sections below deliberately distinguish observed evidence from implemented
code and future contracts. Local tests are not deployment or Cloud evidence.

## Verified pre-implementation baseline evidence

The source baseline is `0c07fdee3eefac5a21f93878211bf507b2e5c6a7`.
Local capture and a disposable workload on `scion-gteam` demonstrated that the
installed pipeline could reach all three Google Cloud destinations, while also
demonstrating these defects:

- ten independent normalized tool hooks became one Monitoring increment;
- metric resources and scopes collapsed into one output identity;
- configured field policy did not cover native-shaped resource, span, log, or
  body fields;
- excluded normalized prompt spans and logs bypassed receiver policy;
- normalized hook traces and init lifecycle metrics had direct Cloud paths.

Native span-name exclusion did work. The native-shaped baseline inputs were
synthetic fixtures. They do not prove native emission by any installed harness.
The inspected Claude 2.1.220 agent lacked native telemetry enablement, while
Codex 0.144.1 targeted `cloudtrace.googleapis.com:443` directly for traces/logs
and used `metrics_exporter=statsig`. Harness provisioning remains Phase 4.

## Phase 1 implemented behavior

Phase 1 establishes the common ingress boundary in
`pkg/sciontool/telemetry`:

- hook and init SDK providers send traces, logs, and metrics only to the
  configured `127.0.0.1` gRPC receiver; producer providers do not consume Cloud
  credentials or external destinations;
- receiver ingress adds authoritative `scion.agent.id`, `scion.project.id`,
  `scion.harness`, and available agent-slug/broker identity without replacing
  incoming `service.name` or `service.instance.id`. Producer Scion identity
  keys, including duplicate, typed, and legacy broker/project aliases, are
  stripped after field processing, then one authoritative value is appended
  for each identity available from the receiver environment. When an env value
  is absent, no producer-provided substitute is trusted. Configured redaction
  cannot suppress authoritative identity;
- canonical `SCION_PROJECT_ID` wins over the legacy environment alias, whose
  name is obtained from `pkg/projectcompat`;
- resource and instrumentation-scope identity, scope name/version/attributes,
  and resource/scope schema URLs reach generic OTLP destinations. The native
  Cloud Trace/Monitoring adapters retain processed scope attributes and both
  schema URLs in supported SDK metadata. Cloud Logging has no equivalent
  scope/schema fields: its adapter writes processed scope name/version/attributes
  and schema URLs under the reserved `scion_otel_metadata` payload key (attribute
  values stringified); it does not claim backend-native scope fields;
- event include/exclude policy applies to span names and normalized log
  `event.name`; exclusion wins, restrictive includes reject unnamed logs, and
  event lists do not filter metrics. Pinned Codex `codex.user_prompt` and
  `codex.tool_result`, and Gemini `gemini_cli.user_prompt`, normalize to the
  corresponding `agent.user.prompt` and `agent.tool.result` policy names;
  contradictory event-name fields/duplicates reject the entire request;
- unsafe producer-controlled span, span-event, log-event, and metric names are
  rejected with bounded InvalidArgument (gRPC) or HTTP 400 responses rather
  than silently dropped. The entire request is validated before cloning or
  forwarding; a 32-level/4096-value AnyValue budget prevents unbounded
  recursion, while local counters track rejected spans/log records/metric
  points. Configured filtering is distinct from invalid-input rejection;
- field policy recursively covers resource, scope, span, span-event, link, log
  record, structured log body, metric-point, and exemplar attributes;
- unstructured log bodies and unkeyed scalars/bytes in structured body arrays
  use the virtual field `log.body`, while keyed body values use their own field
  names. Span status
  text uses `span.status.message`; both are redacted by default;
- producer-side hook transformation was removed. Ingress clones and transforms
  a batch once, then retries the immutable processed batch. No producer flag or
  digest-shaped-value guess can bypass or repeat hashing;
- the metrics debug exporter is on the forwarding side of ingress, and hook
  debug output contains only bounded event metadata/counts, not raw event names
  or error content. Local buffers and forwarding destinations receive only
  policy-processed batches.

The finite native content aliases covered by Phase 1 are:

| Configured field | Covered attribute aliases |
| --- | --- |
| `prompt` | `gen_ai.prompt`, `gen_ai.input.messages`, `input.value` |
| `tool_input` | `tool.input`, `tool.call.arguments`, `gen_ai.tool.call.arguments` |
| `tool_output` | `gen_ai.completion`, `gen_ai.output.messages`, `output.value`, `output`, `tool.output`, `tool.call.result`, `gen_ai.tool.call.result` |
| `session_id` | `session.id`, `gen_ai.conversation.id`, `conversation.id` |

This is field-based protection, not arbitrary secret detection. Safe event
names and trace/span correlation identifiers are retained. Phase 1 tests use
capturing fake destinations and explicitly labeled synthetic native-shaped
fixtures; they do not claim installed native-harness behavior.

## Phase 2 locally implemented metric contract

The receiver now keeps independent metric streams by full post-policy resource,
scope, instrument and typed point identity. Hook counters emit deltas from each
short-lived process; the collector accumulates those increments into cumulative
streams. Native cumulative sums select newer points within an epoch, native
deltas accumulate ordered non-overlapping intervals, gauges select the newest
point, and explicit histograms retain compatible bounds, buckets, count and
sum. Overlap, conflicting writers, unsupported kinds/details, and ambiguous
resets return admission errors with bounded local rejection reasons. The
receiver caps active streams at 2048 and duplicate intervals at 64 per stream;
delivered idle streams expire after 30 minutes and start a new output epoch.
Session-end token totals no longer repeat model-end token increments. Hook
tokens use `scion.hook.tokens.input`, `.output`, and `.cached`; genuine native
`gen_ai.tokens.*` keeps a separate namespace. Old normalized hook token names
are rejected. The current Hub dashboard queries historical `gen_ai.tokens.*`,
so it excludes the new hook counters and cannot be read as complete or
native-only usage while old hook samples remain.

Generic OTLP exports all policy-processed dimensions. Cloud Monitoring uses
only these resource fields in its canonical resource digest: `service.name`,
`service.namespace`, `service.instance.id`, `scion.agent.id`,
`scion.project.id`, `scion.agent.slug`, `scion.harness`, `scion.model`,
`scion.broker.id`, `scion.broker.name`, and bounded scalar-string
`gcp.project_id` (digest only,
distinct from `scion.project.id`). The scope digest includes scope name, version, schema URL,
and only `component`, `scope.kind`, and `scope.variant` attributes. The point
digest includes only `agent_id`, `project_id`, `harness`, `model`, `tool_name`,
`status`, `operation`, `sensor`, `phase`, and `run`. Only the internal pipeline
status metric may additionally use bounded scalar-string
`scion.telemetry.provider` and `scion.telemetry.project_id`; only the internal
export-error metric may use bounded `signal` and classified `error_type`.
All three digests use
type-aware canonical encoding after policy processing and appear as fixed
`scion_metric_resource_id`, `scion_metric_scope_id`, and
`scion_metric_point_id` labels. Readable `scion_agent_id` and
`scion_project_id` labels come only from authoritative receiver resource
identity; producer point `agent_id` and `project_id` keep their existing
meaning. Service resource fields retain the Monitoring SDK's existing service
labels. Forbidden prompt, conversation/session, and payload fields are
rejected for Cloud metrics, even if policy redacted or hashed them, including
when nested inside an allowed array or map. Unknown resource, scope, and point
identity fields reject on first Cloud admission; generic OTLP retains them
after policy processing. A second full OTLP identity that would collapse to an
admitted Cloud identity is also rejected. Different input temporalities or
writer semantics targeting the same active Cloud series reject before
admission, including sums and explicit histograms. Cloud descriptor shape is
fixed per metric name among active streams: varying label sets, kind, value
type, or unit reject before state commit. Identity and descriptor registries
each cap at 2048 entries and retire an entry only after all related streams have been
delivered and idle for 30 minutes. An old remote descriptor mismatch after
local expiry remains a Cloud export error, not confirmed delivery.

The existing `agent.tool.calls` Cloud descriptor is CUMULATIVE/INT64/{call}
with eight historical string labels, including `grove_id`. The adapter keeps
its name, kind, value type, unit, and old producer label semantics. Its five
new labels bring the descriptor union to 13; local Monitoring SDK capture
shows the expected descriptor and CreateTimeSeries shape. Actual additive
label acceptance on the existing remote descriptor remains a live rollout
gate. Admission acknowledges local state only. A pre-existing or external
descriptor mismatch is discovered during export and returns a Cloud error;
it is locally observable and is never counted as confirmed delivery.

One immutable pending snapshot is retried without re-adding newer increments.
Normal shutdown lets an in-flight metric export finish within the caller's
deadline, then waits for the next safe 15-second Cloud write slot before
flushing newer accepted metrics. `sciontool init` allows 20 seconds for this
post-child telemetry stop, up to 15 seconds longer than its former budget;
backend latency can still exceed it. A short deadline or a failed pending
snapshot with newer dirty state produces a non-nil bounded residual error
instead of a delivery claim. The first Phase 2 live gate failed when a reset
arrived before the prior epoch was exported and newer metric state remained at
shutdown. The R4 shutdown repair passed the next live causal barriers, but the
final `agent.session.count` write was rejected by Cloud because init's
cumulative counter and a hook subprocess's delta counter mapped to one Cloud
series with different start epochs. Init lifecycle metrics now use a fixed
`/lifecycle` instrumentation scope; hook metrics keep their prior scope. The
existing scope digest yields separate Cloud series without changing the metric
name, unit, descriptor, or point labels. These series count lifecycle and
harness hook events respectively. They must not be summed as a canonical
logical session count; dashboard reconciliation remains deferred. The live
correction still requires independent review and a new pinned rollout.

The init handler constructs the same token, tool, session, and API instruments
under its lifecycle scope, but its registered pre-start, post-start, pre-stop,
and session-end events currently record only `agent.session.count`. Its trace
scope and log scope are unchanged, as are hook subprocess metrics.

Ordered failed-backlog draining and bounded in-memory retry remain Phase 3;
disk durability and cross-process logical-session reconciliation are later
work. An external `docker stop` without an explicit timeout can force-kill a
Linux container after its shorter
[default 10-second grace period](https://docs.docker.com/reference/cli/docker/container/stop/),
so the 20-second internal budget is not an outer termination guarantee.

## Later-phase intended contracts (not yet implemented)

### Phase 3: bounded delivery and shutdown

Add loopback-only listeners, request/message/time limits, bounded queues and
retry state, explicit overload/permanent-failure accounting, truthful supported
egress protocols, Cloud Logging asynchronous error handling, and deadline-bound
shutdown ordering. Admission, enqueue, export attempt, and confirmed delivery
must remain distinct states.

### Phase 4: harness wiring

Configure supported installed Claude, Gemini, and Codex versions to use the
loopback receiver for the signals they actually emit. External destinations
must not bypass the boundary. Unsupported, broken, and untested capabilities
must be reported separately.

### Phase 5: integration and evidence

Exercise subprocess hooks and real installed native emitters through the actual
receiver, then record pinned artifact versions, safe synthetic privacy markers,
exact queries, backend visibility windows, rollback details, and Cloud results.
Update the deployment walkthrough only after this evidence exists.

## Deployment and Cloud evidence still pending

Phase 1 was deployed and accepted with 134 live Cloud assertions and 57 local
generic checks at `bf41c234`; see the task-local Phase 1 acceptance report.
Phase 2 was deployed for bounded live checks at `82fc67cb` and `b385857b`.
The R4 session-count Cloud collision remains an open live gate; the R5 source
scope correction has local tests only and awaits review and a pinned rollout.
The original defective baseline observations above predate Phase 1.
Before declaring the milestone delivered, a reviewed integration revision must
prove in a scoped workload:

- installed native and normalized paths through loopback;
- distinguishable resource/scope identity in each backend;
- allowed positive controls and absence of excluded/privacy markers after a
  finite visibility window;
- correct metric totals, epochs, retries, resets, and descriptor behavior;
- confirmed Logging delivery rather than local enqueue alone;
- bounded failure/recovery and final shutdown flush; and
- unchanged Hub/broker health and unrelated active agents.

Deferred work includes persistent cross-process span pairing, authoritative Hub
total reconciliation, generic OTLP HTTP egress, disk-backed durability, new
dashboards/alerts, and Scion infrastructure observability.
