# Agent observability: first integration milestone

Status: Phase 1 implemented locally; later phases and reviewed deployment evidence pending.
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
  incoming `service.name` or `service.instance.id`;
- canonical `SCION_PROJECT_ID` wins over the legacy environment alias, whose
  name is obtained from `pkg/projectcompat`;
- resource and instrumentation-scope identity, scope name/version/attributes,
  and resource/scope schema URLs are preserved;
- event include/exclude policy applies to span names and normalized log
  `event.name`; exclusion wins, restrictive includes reject unnamed logs, and
  event lists do not filter metrics;
- unsafe producer-controlled span, span-event, log-event, and metric names are
  rejected rather than forwarded with possible user content;
- field policy recursively covers resource, scope, span, span-event, link, log
  record, structured log body, metric-point, and exemplar attributes;
- unstructured log bodies use the virtual field `log.body`, and span status
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
| `tool_output` | `gen_ai.completion`, `gen_ai.output.messages`, `output.value`, `tool.output`, `tool.call.result`, `gen_ai.tool.call.result` |
| `session_id` | `session.id`, `gen_ai.conversation.id` |

This is field-based protection, not arbitrary secret detection. Safe event
names and trace/span correlation identifiers are retained. Phase 1 tests use
capturing fake destinations and explicitly labeled synthetic native-shaped
fixtures; they do not claim installed native-harness behavior.

## Later-phase intended contracts (not yet implemented)

### Phase 2: metric stream correctness

Replace latest-wins batching with bounded, type-aware stream handling that
preserves resource, scope, instrument, unit, kind, temporality, monotonicity,
typed point attributes, timestamps, and cumulative start epochs. Normalized
hook deltas, native cumulative/delta sums, gauges, and explicit histograms must
handle replay, reset, overlap, and unsupported inputs explicitly. Cross-process
session reconciliation and durable exactly-once delivery remain deferred.

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

Phase 1 has not been deployed and has no post-change Cloud acceptance evidence.
The verified Cloud observations above describe the defective baseline only.
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
