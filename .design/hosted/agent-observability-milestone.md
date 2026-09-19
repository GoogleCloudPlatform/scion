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

The Phase 3 developer candidate uses these per-agent limits, pending independent
review and pinned live evidence:

| Boundary | Limit and response |
| --- | --- |
| OTLP HTTP intake | Loopback only; 4 MiB body on the wire, protobuf with `application/x-protobuf` and identity encoding only. Oversize returns 413, unsupported content type or encoding 415, malformed protobuf 400. Gzip and JSON intake are not supported. The 8 MiB decoded ceiling is redundant for identity encoding but protects any future decoder. |
| OTLP gRPC intake | Loopback only; 8 MiB post-decompression message limit. Permanent oversize is `ResourceExhausted` without retry advice. |
| Intake execution | 15-second deadline without extending a shorter caller deadline; 16 shared HTTP and gRPC processing slots, acquired before body read or gRPC `RecvMsg` decompression. Transient saturation returns HTTP 429 with `Retry-After: 1` or gRPC `ResourceExhausted` with `RetryInfo`. A gRPC slot remains held through receive, decoder, typed Export, and response send even if its context is canceled. The limit counts admitted processing stacks, not all transport workers or total heap. |
| Transport resources | Each listener accepts at most 64 live connections. The 65th is closed promptly as a transport error; it has no gRPC retry status. gRPC handshake is limited to 5 seconds, headers to 16 KiB, and idle connection time to 30 seconds. Idle connections do not use the 16 active request slots. HTTP has 5-second header and 15-second read/write limits with 16 KiB headers. |
| Retained payload | 16 MiB encoded, 4096 admitted records/metric points, and 512 request entries across all signals, in-flight exports, metric dirty state, and pending retry. An entry is one nonempty admitted request; a pending metric snapshot retains ownership of its contributing entries until success or terminal disposition. Whole new requests are rejected before policy cloning or state commit on exhaustion. Empty requests use no entry. Existing metric stream state remains capped at 2048 streams. |
| Retry and cadence | Spans and logs have at most four pipeline exporter calls (initial plus three retries), exponential backoff from 100 ms capped at 5 seconds and caller-context cancellation. Metrics make one pipeline exporter call per eligible flush, with at least 15 seconds after the preceding call completes before another call on the same series. An immutable metric snapshot terminates after 20 pipeline calls or five minutes from its oldest unresolved admission, whichever comes first. A failed pending seven-point cumulative snapshot is resolved before newer ten-point state. |

The retained-byte ceiling measures protobuf encoding, not total resident memory:
compressed and decompressed intake buffers, protobuf allocations, policy clones,
stream state, Go allocator overhead, and SDK buffers add to it. Up to 16
admitted gRPC messages may each reach the 8 MiB decoded-message ceiling before
these additional allocations; the 16 MiB retained-queue budget is separate.
The gRPC server advertises at most 16 concurrent streams per connection. A
17th generated-client call on the same connection may wait locally before
sending HEADERS; its caller deadline bounds that wait, while the server's
15-second intake timer starts only for a received stream. A 17th call sent on
another connection reaches the handler and receives prompt retry advice when
all 16 processing slots are occupied. External clients without a deadline can
wait at the per-connection gate, so the 15-second server intake timer is not
an end-to-end producer deadline. The queue and decode limits do not cap total
transport workers, protobuf clones, SDK buffers, or process RSS.
The entry ceiling can reject 513 small requests before 4096 records are reached.
Synchronous trace and log exports retain their admission reservation until the
export call ends; a failed call returns an error to the producer. Metric intake
ACK means accepted local state only. Fixed-cardinality local diagnostics count
admitted, filtered, rejected, queued, attempts, failures, confirmed delivery,
drops, current depth, and last successful export separately. Generic OTLP HTTP
destination configuration fails at startup; generic OTLP gRPC and GCP native
are the supported forwarding paths. A successful Cloud Logging `Flush` is
required before its exporter reports delivery, and asynchronous client errors
remain visible locally. Disk persistence, cross-crash exactly-once delivery,
and a bound on context-ignoring SDK calls are not provided. The internal init
telemetry stop budget remains 20 seconds; a shorter external container stop
limit can still terminate before the drain completes.

The receiver admission gate closes before pipeline shutdown. A handler already
inside the pipeline keeps its budget reservation and exporter alive until it
returns. If the caller deadline expires first, Stop returns an incomplete
shutdown error, keeps resources for a later cleanup call, and reports degraded
state. A pinned Monitoring shutdown closes its client on the first call even
when it returns a deadline or close error, so that one-shot client is cleared
and its error remains visible. If a deadline prevents the Logging client from
acquiring its Log/Flush slot, the pipeline retains that client for a later
Stop. A completed Logging Close is not repeated. Later cleanup cannot erase
the first shutdown error or reopen intake. A gRPC worker still receiving
before the pipeline handler also keeps
its processing slot until it unwinds; a repeated Stop cannot claim a clean
drain while that slot remains. Forced gRPC Stop runs asynchronously because
grpc-go may serialize it behind GracefulStop; both shutdown goroutines may
remain until context-ignoring work exits. This does not bound a context-ignoring
SDK call or guarantee zero residual goroutines at deadline.

Diagnostic units are spans, log records, or admitted metric points. `Accepted`
counts policy-processed units acknowledged by intake; `Queued` counts units
actually retained for export, including synchronous export in flight;
`Delivered` counts units in a wholly confirmed export batch. `Dropped` counts
only proven local discard, such as an accepted request with no destination.
`Unconfirmed` counts terminal admitted units whose remote outcome is not
confirmed, including permanent, partial, age, attempt-limit, and shutdown
dispositions. These fixed reason counters partition `Unconfirmed`;
`BackendRejected` is a known rejected subset from OTLP PartialSuccess, not an
additional disposition. At a quiescent instant, `Accepted = Delivered +
Dropped + Unconfirmed + retained`, in the same signal-record units. Filtered
and rejected-before-admission units are excluded. `Attempts` counts exporter
calls and `Failed` counts failed export batches; `Queued` is cumulative actual
retained record units. Concurrent snapshots of separate atomics are
informational rather than a transactional ledger.
`SDKErrors` separately counts asynchronous Cloud Logging SDK callbacks. One
failed Logger.Flush contributes one pipeline `Failed` batch outcome even if
the SDK also invokes its error callback. Log export and Flush calls are
serialized for the single client/logger so the SDK's client-wide error reset
cannot let concurrent batches consume one another's result; callback delivery
can lag Flush and is not used as a second per-batch failure signal.
The collector emits a fixed-cardinality local delivery snapshot at startup,
first error/degraded transition, no more than once per 60 seconds thereafter,
and final or incomplete shutdown. It includes state, queue bytes/records/entries
and all per-signal count/reason/last-success fields in agent stderr/agent.log;
it does not export itself through OTLP or include request/backend error text.

Metric admissions keep their own timestamps and encoded-payload reservations
through dirty, pending, retry, and in-flight states. A terminal snapshot is
reported as unconfirmed, released from the retry queue, and never attributed
later success. The cumulative stream baseline and epoch remain; genuinely
newer accepted data can therefore produce cumulative 10 after terminal 7 plus
new 3. That later success confirms only the newer admission's local delivery
accounting and does not prove whether the earlier 7 reached the backend.
Without new eligible data, terminal resolution creates no new work. Newer
data retains its original age even while it waits behind a pending snapshot.
Expiry clears a dirty stream's export marker when its last eligible admission
ages out; an unrelated fresh stream cannot carry that expired point into a
snapshot. A genuinely newer admission on the same stream keeps the marker and
may export its cumulative baseline. Exact duplicate or older points that do
not change stream state are treated as filtered, without a queue reservation.
The five-minute limit is checked on the periodic one-second expiry tick;
context-ignoring export work keeps ownership until its call returns.

The three gRPC Export methods are registered with fresh public service
descriptors so admission happens in their stream handlers before `RecvMsg`.
Generated unary OTLP clients still send one request and receive one response
using the original method names and protobuf types. gRPC server statistics and
`GetServiceInfo` intentionally classify these adapters as server-streaming;
the protobuf service definitions remain unary. The receiver has no unary or
stream interceptors to preserve; its tap installs the deadline and its stats
handler cleans up that timer. A second request message is rejected before the
typed Export handler runs, though grpc-go may decode that second message while
the first request's processing slot is held. A caller that omits END_STREAM
keeps a slot until the intake deadline or earlier caller cancellation. CPU-bound
decode or an exporter ignoring context may outlive that deadline; capacity is
released only when the handler stack actually returns.

For generic OTLP gRPC, `tls.enabled=false` requests plaintext transport, while
`tls.insecure_skip_verify=true` keeps TLS encryption and skips certificate
verification. A custom CA retains verified TLS. Plaintext cannot be combined
with a custom CA or skip-verify. Explicit nondefault generic TLS settings in
GCP-native mode fail telemetry startup because the GCP SDK transport would
otherwise ignore them. The effective direct environment setting wins over a
settings-file value; contradictory effective transport settings fail startup.
Confirmed metric exports emit a local sequence and deterministic batch digest
to correlate a whole cumulative batch with downstream evidence; an intake ACK
and a successful earlier batch do not confirm a later batch.

### Phase 4: harness wiring

Configure supported installed Claude, Gemini, and Codex versions to use the
loopback receiver for the signals they actually emit. External destinations
must not bypass the boundary. Unsupported, broken, and untested capabilities
must be reported separately.

The Phase 4 provisioners generate receiver configuration from the staged
`SCION_OTEL_GRPC_PORT` (default 4317), always targeting `127.0.0.1`. The
cloud exporter endpoint and old Codex endpoint overrides are not native
destinations. Disabled provisioners explicitly disable supported exporters.
Codex uses OTLP gRPC for logs, traces, and metrics; hook token counters retain
their separate `scion.hook.*` names. Claude configures OTLP logs and metrics;
its trace exporter is disabled because its documented telemetry options do not
establish a native trace emitter. Gemini configures its documented local OTLP
target with prompt logging off and detailed traces on. These are configured
capabilities, not evidence of emitted signals. The Phase 4 live gate still
requires inspection of exact installed Claude and Gemini versions: their image
builds currently use `@latest`, and neither binary nor Docker was available in
the developer environment. Codex 0.154.0 was installed locally; its generated
configuration was exercised, but actual emission was not observed there.
Each provisioner output declares `SCION_NATIVE_TELEMETRY_POLICY` as `enabled`
or `disabled` for the runtime's fail-closed inherited-env check.
Configuration keys were checked against the vendor references for
[Claude Code environment variables](https://code.claude.com/docs/en/env-vars),
[Gemini CLI telemetry](https://geminicli.com/docs/cli/telemetry/), and
[Codex config](https://developers.openai.com/codex/config-file/config-reference).

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
