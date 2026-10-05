# Design: holistic harness usage telemetry (fork issue ptone/scion#2053)

Status: Agreed design (revision 4). Phases 1, 2 and 3c (the Claude vertical slice, the canonical
usage contract, the dashboard cumulative-math fix, hook-side usage alignment, and Codex native
usage) are implemented; phases 0, 3a, 3b, 3d and 4 (native-OTel routing for copilot/grok-build,
the remaining per-harness usage rules, build provenance) are follow-up work tracked against
ptone/scion#2053.
Updated: 2026-09-29.

ptone agreed to every decision D1–D11. The opencode refinement from source investigation (§3.7) implements D9 as directed.
Base: main `d9b9e6a`.
Citations of the form `file:line` are spot-checked against the code.

---

## Glossary
- **Native telemetry:** OpenTelemetry signals (metrics, log events, spans) that a harness CLI emits *itself*, through its own built-in OTel support, sent over OTLP to sciontool's local receiver. Example: Claude Code's `api_request` log event carrying `model`, `input_tokens` and `output_tokens`. The contrast is **hook telemetry**: metrics that sciontool records on the harness's behalf when the harness invokes `sciontool hook` (for example `gen_ai.api.calls` on a model-end hook).
- **Raw (native) metric:** a harness's own metric, with its own name and attribute keys, before Scion normalization. Example: Copilot's `gen_ai.client.token.usage` histogram with label `gen_ai.token.type=input`. The contrast is a **canonical** metric (§3.2), for example `scion.usage.tokens{token_type=input, agent_id, project_id, harness, model}`.
- **GCP allowlist:** the fixed sets of resource, scope and point attribute keys that sciontool permits when exporting metrics to Cloud Monitoring on the GCP provider (`pkg/sciontool/telemetry/gcp_metric_identity.go:25-36`, from GoogleCloudPlatform/scion#1792). Any other key rejects the whole OTLP request. It is a privacy guard that keeps `session.id`, `user.*`, prompt content and similar out of Cloud labels. Example: `agent_id`, `project_id`, `harness`, `model` and `status` are allowed; Claude's raw `type` label is not. This design adds exactly one key, `token_type`.
- **Usage deriver:** the new sciontool receiver component (§3.3) that turns native per-request events into canonical counters.
- **Collector epoch / cumulative:** sciontool exports each counter to Cloud as a running total since its start time (the epoch) and re-sends it on each flush. Consumers must take increases, not sum points.

---

## 0. Decision log (ptone)

| # | Decision | Recommendation | ptone answer |
|---|---|---|---|
| D1 | Canonical usage contract | Keep `gen_ai.api.calls`. Add one token metric `scion.usage.tokens{token_type}`, which replaces `scion.hook.tokens.*`. The dashboard does not read legacy names. | ptone: **yes** (the recommendation as written). |
| D2 | Where native usage is normalized | Inside the sciontool receiver: a **usage deriver** turns per-request native *events* into canonical counters. Raw native metrics stay unadmitted on GCP (the allowlist is not widened). | ptone: **yes** (the recommendation as written). |
| D3 | Canonical identity | ~~Point labels `agent_id`/`project_id`~~ **Revised after ptone:** the canonical *stored-series* labels are `scion_agent_id` and `scion_project_id`, stamped by the sciontool **exporter** from authoritative resource identity on every metric, on both the GCP and generic-OTLP exporters (the time-series store is swappable). The dashboard filters on `metric.labels.scion_project_id` and groups on `scion_agent_id`. Presenting the slug: see D7. | ptone: filter must use the disambiguated stored label (`scion_project_id`), since "project" is overloaded (GCP project) and the store is GCP by default but swappable. Scion's DB holds no time-series points. Wants a quick, efficient way to show `agent_id` as the agent slug. |
| D4 | Double counting across hook and native sources | One usage source per harness, declared by `provision.py` via `SCION_USAGE_SOURCE=native\|hooks`. **Narrow:** the switch covers only usage (`gen_ai.api.calls` and `scion.usage.tokens`). Tool, session, turn and every other hook telemetry is unaffected. | ptone: agree, provided it is narrow to usage (a harness may use hooks for turns and similar while using native for tokens). Confirmed narrow. |
| D5 | Stale sciontool in images | No `provision.py` workarounds for old sciontool. Add a build-provenance label and a harness-only-build warning, and document that usage fixes need a `scion-base` rebuild. | ptone: **yes** (the recommendation as written). |
| D6 | Scope | **Revised:** now claude, copilot, codex, opencode and antigravity. Deferred: gemini-cli and grok-build (routing fix pending D8), muse-code verification, Hub DB token sink, session reconciliation, cost. | ptone: include antigravity and opencode; defer gemini CLI and grok. |
| D10 | Vetting gate for hook-sourced usage (follows from ptone's D9 principle) | Invert the default: `SCION_USAGE_SOURCE` unset means **no usage published** from hooks (`gen_ai.api.calls` and `scion.usage.tokens` are both skipped). A harness publishes usage only when its `provision.py` declares `native` or `hooks`, and the declaration is allowed only in the PR that checks in a captured fixture proving the mapping (one model request = one call; tokens match the CLI's own report). Consequences: gemini's unvetted `AfterModel` calls stop being published until vetted (opencode publishes none today, confirmed by source inspection); muse-code and antigravity publish nothing until their fixture PRs land. Tool, session and turn hook metrics are unaffected. | ptone: **yes** to the recommendation. |
| D7 | Presenting `scion_agent_id` as a slug | Recommend: the exporter also stamps `scion_agent_slug` (from the already-allowlisted resource key `scion.agent.slug`). It is 1:1 with the agent ID, so it adds no cardinality. The dashboard reads the slug straight off the series, with no DB join, and it survives agent purge (`PurgeDeletedAgents`). Alternative: a hub batch lookup `ListAgents(IncludeDeleted)` by ID, cached; it is cheap but loses purged agents and couples the view to the DB. | ptone: **stamp the label.** |
| D8 | grok-build routing fix, given grok is deferred | Recommend keeping it in P0. Today grok-build sends native OTel straight to the cloud endpoint, bypassing redaction and identity; it is a privacy fix of a few lines. Grok usage derivation stays deferred. | ptone: **keep the grok routing fix**, as recommended. |
| D11 | Slug display and harness CLI pinning (post-P1) | (1) No per-agent slug view: AC-1.1b slug display is N/A and the per-agent-view follow-up is dropped. The `scion_agent_slug` label stays (D7). (2) Harness CLI versions are **not pinned** in Dockerfiles, on the assumption that vendors keep backward compatibility. Fixtures record the CLI version they were captured from (file name and test). "Pinned CLI version" in §5 means "the CLI version the fixture was captured from". The P3b `opencode-ai` pin is dropped unless ptone says otherwise when P3b starts. | ptone: "per agent metrics are not needed… harnesses change versions fast, so we don't want to pin; will assume they maintain backwards compatibility." |
| D9 | Hook-sourced usage for opencode and antigravity | Both have no usable native OTel, so usage comes from their hooks. opencode: fix the bridge to emit one model-end per LLM step (`step-finish` part) with its `tokens`, replacing the debounced `message.updated` that inflates calls. antigravity: `PostInvocation` provides calls; tokens only if its payload carries usage. If a captured payload shows no usage, antigravity ships calls-only and tokens become a follow-up. | ptone: dig deeper into the opencode claim against its open source code (confirmed: step-finish is sound, and the bridge's keyed hooks never fire, so it must be rewritten onto the `event` hook; see §3.7). Antigravity understood. **Principle: publish no hook-sourced usage unless it is vetted as valid and sound data** (see D10). PR plan looks fine. |

---

## 1. Problem and goals

Today no harness gets both model calls and tokens onto the Hub metrics dashboard with correct agent and project attribution. The investigation identified four compounding root causes:

1. **Names.** The dashboard reads `gen_ai.tokens.*`, which nothing emits.
2. **Identity.** Identity is stamped on the *resource* only. The dashboard filters on the *point* label `project_id`, and only hook metrics set it.
3. **Admission.** On GCP, native usage metrics are rejected, because their attributes fall outside the privacy allowlist.
4. **Sources.** Most harnesses have no hook that carries model calls or tokens.

**New finding (spot-checked against the code).** The dashboard also **sums every raw point** (`metrics_dashboard.go:358-390` and the day and group variants). After GoogleCloudPlatform/scion#1792, sciontool exports *every* counter to Cloud Monitoring as **CUMULATIVE**:
- hook counters use a collector epoch start and re-export the running total on each flush (`metric_streams.go:777-783, 978-1022`; documented in `metrics.md` "collector observation epoch … cumulative snapshot");
- native deltas are converted to cumulative (`metric_streams.go:777-781`).

So `querySum` counts one running total once per flush. Even `gen_ai.api.calls`, the one metric the dashboard reads under a correct name, is inflated today. The fix has to change the dashboard *math*, not just its names.

**Goals**
- G1. For every in-scope harness, each model call and its tokens appear on the dashboard, in the global and project views, attributed to the right agent, project, harness and model.
- G2. There is one canonical contract (names, units, labels, temporality). Every source feeds it and the dashboard reads only it, so adding a harness means adding a derivation rule and never touching the dashboard.
- G3. Normalization happens at the pipeline choke point (the sciontool receiver), not per harness in `provision.py` and not in the dashboard.
- G4. The privacy posture from GoogleCloudPlatform/scion#1792 is unchanged: no unreviewed native attribute reaches a Cloud label.
- G5. The contract is pinned by tests on both sides: the emitter shape and the dashboard query.

## 2. Non-goals
- **gemini-cli and grok-build usage** (deferred by ptone, D6). gemini keeps hook-sourced calls; the rule is sketched in §5 for the follow-up.
- **Reconciling session counts.** The dual lifecycle and hook `agent.session.count` sources are left as they are (an open question, out of scope here). The dashboard's session and unique-agent panels keep their current *sources*; they do get the cumulative-math fix. Follow-up.
- **Hub DB session-metrics tokens** (`/metrics/summary`). Follow-up (see §8, OQ-2).
- **Cost.** `claude_code.cost.usage`, `codex.turn.cost_microusd` and similar. Follow-up; the contract leaves room for a `scion.usage.cost` sibling.
- **muse-code verification and hermes.** They keep today's behaviour.
- **Publishing images upstream**, or checking the sciontool version at runtime.
- **An external OTel Collector,** including miller79/scion#138's count connector. Scion must not depend on one.
- **Admitting raw native harness metrics to Cloud Monitoring.** On generic OTLP they are still forwarded unchanged.
- **Backfilling or relabelling historical Cloud Monitoring data.**

## 3. Proposed design

### 3.1 Overview
```
                      ┌──────────────── sciontool init process ─────────────────────────┐
harness native OTLP ─►│ receiver ─► policy (identity, redaction) ─┬─► logs/spans/metrics ─►│─► exporter
 (logs, metrics)      │                                           │     (unchanged path)   │   (GCP | OTLP)
                      │                                           └─► UsageDeriver ────┐   │
                      │   rules keyed by (SCION_HARNESS, signal, scope, name)          │   │
                      │   dedupe LRU; emits canonical increments                       ▼   │
harness hooks ───────►│ (loopback OTLP, hook scope) ────────────────► metricStreams (reserved │
 (sciontool hook)     │                                               counter, collector epoch)│
                      └────────────────────────────────────────────────────────────────────────┘
hub dashboard ─► ListTimeSeries(gen_ai.api.calls | scion.usage.tokens, metric.labels.scion_project_id)
             ─► per-series cumulative increase ─► views
```

### 3.2 Canonical usage contract (D1), load-bearing

| Metric | Kind / temporality at Cloud | Value | Unit | Point labels (exact set) | Meaning |
|---|---|---|---|---|---|
| `gen_ai.api.calls` | Sum, monotonic, CUMULATIVE (collector epoch) | int64 | `{call}` | producer: `agent_id`, `project_id`, `harness`, `model`, `status` (kept for descriptor compatibility, not canonical); exporter-stamped canonical: `scion_agent_id`, `scion_project_id`, `scion_agent_slug` | One completed model response (success), or a failed request where the source reports it (error). Sources that emit nothing for aborted or failed attempts undercount by that amount; this is documented per harness. |
| `scion.usage.tokens` | Sum, monotonic, CUMULATIVE (collector epoch) | int64 | `{token}` | producer: `harness`, `model`, `token_type`; exporter-stamped canonical: `scion_agent_id`, `scion_project_id`, `scion_agent_slug` | Tokens attributed to model requests. |

- **`token_type` is a closed enum:**
  - `input`: non-cached prompt tokens;
  - `output`: generated tokens, *including* reasoning;
  - `cache_read`;
  - `cache_write`;
  - `reasoning`: an informational subset of output, never added to totals.
  - Any other value is an admission error.
- **Invariant:** total input = `input + cache_read + cache_write`, and the per-rule mappings in §5 subtract as needed. Every rule has to document how its source's fields map to these five types.
- **`model`:** comes from the native event when present (the actual model, including sub-agent models), and falls back to `SCION_MODEL`. It is truncated to 128 characters. It must be a non-empty string; otherwise `unknown`. The same precedence applies to hook-sourced usage: a dialect maps the payload's model field onto the event (`model:` in `dialect.yaml`; antigravity's `modelName`, a display alias, and the opencode bridge's joined `providerID/modelID`), and the hook handler resolves the label through the same `telemetrycontract.ResolveModelLabel` helper as the native deriver (ptone/scion#2242). The `unknown` default applies to every hook-sourced model label, including the hook `gen_ai.api.duration` histogram, which is recorded regardless of `SCION_USAGE_SOURCE`; before this, those points omitted `model` when `SCION_MODEL` was unset. Hook spans and logs carry a `model` attribute only when the payload names one (no fallback).
- **`status`:** `success` or `error`.
- **Identity labels (`scion_agent_id`, `scion_project_id`, `scion_agent_slug`):** stamped by the sciontool exporter from the authoritative resource identity (`authoritativeIdentity()`), never from the harness payload. See §3.4. `harness` is stamped by the producer from `SCION_HARNESS`.
- **Why keep `gen_ai.api.calls`:** it is already the dashboard name and already has a Cloud descriptor with exactly this label set (from the hook handler, `telemetry.go:521-533`). Keeping it preserves history and avoids a descriptor change.
- **Why one token metric with a type label, not three:** extending it (cache_write, reasoning) needs no new descriptor, and the dashboard can group by type in one query. `scion.hook.tokens.*` has effectively no data, so retiring it costs almost nothing. The dashboard reads **only** the two canonical names. Legacy `gen_ai.tokens.*` and `scion.hook.tokens.*` are not read (see §6).
- **Descriptor safety:** `token_type` is added to `cloudPointFields` (`gcp_metric_identity.go:32-36`). `scion_agent_slug` joins the exporter-reserved labels (`gcpAgentLabel` and siblings); it is not a producer-settable key. Nothing else is added. The local `validateDescriptor` pin (`metric_streams.go:445-547`) already enforces one shape per name.

**Reserved counter handling.** `scion.usage.tokens` is added to `isHookCounter` (`metric_streams.go:565-572`) with unit `{token}`, so it gets the collector-epoch and delta-to-cumulative treatment that hook counters get. Both the hook scope and the new deriver scope (`github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry/usage`) are accepted as reserved-counter scopes. Within one sciontool process, the Cloud series identity digest includes the scope, so hook-sourced and native-sourced points land in distinct series (`scion_metric_scope_id`). The dashboard sums across series, and D4 guarantees only one source per harness.

### 3.3 UsageDeriver (D2), load-bearing

**Location.** This is a new file, `pkg/sciontool/telemetry/usage.go`, called from `Pipeline.handleLogs` (and later `handleMetrics`) **after** the policy decision accepts the request and **before** the event filter drops records. The derivation must not depend on `Filter.Include`. Today the policy applies the filter inside `processLogs`, so the deriver either runs on the identity-stamped input before filtering, or `processLogs` returns the pre-filter records alongside. That choice is left to the implementer, and the test in AC-1.4 pins the behaviour.

**Interface (illustrative).**
```go
type usageIncrement struct {
    Model     string
    Status    string            // success|error
    Calls     int64             // 0 or 1 for event rules
    Tokens    map[string]int64  // token_type -> n  (>=0)
}
type usageRule interface {
    Harness() string                       // matches SCION_HARNESS
    MatchLog(scope string, rec *logspb.LogRecord) (usageIncrement, bool)
}
// A rule whose source is metrics rather than (or in addition to) log events
// implements this separate, optional interface instead of adding a method
// to usageRule. DeriveBatch sees one whole ResourceMetrics (one received
// export request) at a time, not one metric at a time, because some
// mappings need to combine more than one sibling metric from the same
// export to produce a canonical increment -- see "Batch-correlation
// contract" below.
type metricBatchDeriver interface {
    DeriveBatch(rm *metricpb.ResourceMetrics) (increments []usageIncrement, matchedNames map[string]bool, err error)
}
type UsageDeriver struct {
    rules  []usageRule         // filtered to SCION_HARNESS at construction
    seen   *boundedLRU         // fingerprint -> struct{}; 16k entries / 15 min
    calls  metric.Int64Counter // scope .../telemetry/usage, delta, loopback like hooks
    tokens metric.Int64Counter
}
```

**Emission.** The deriver records to an in-process OTel `MeterProvider`. It is built like the hook providers (`providers.go:101-151`: loopback gRPC, delta temporality) with deriver scope, and the point labels in §3.2 are computed in-process. This reuses the existing reserved-counter path through `metricStreams` unchanged, so there is no second export path. (The alternative, injecting directly into `metricStreams`, was rejected: it would bypass the admission and diagnostics code that hook counters already exercise.)

**Batch-correlation contract (metric-sourced rules).** A metric-sourced rule may need to combine more than one sibling metric from the same export to produce one canonical increment (for example, subtracting a cached-token counter from a raw total token count). A single-metric-at-a-time view cannot express that, so such a rule implements `metricBatchDeriver.DeriveBatch` and receives one whole `ResourceMetrics` at a time. Two things follow from that scope:
- **Cumulative-only sources.** A source that cannot be made to emit delta temporality (confirmed for Copilot by a real capture: §5) needs its own cumulative-to-delta conversion inside the rule, keyed by stream identity (resource attributes + scope + metric name + point attributes + start_time) so that two concurrent processes, and a process restart, cannot cross-contaminate each other's state, and bounded so unboundedly many distinct streams cannot leak memory. A value that goes backwards for the *same* stream identity is stale (an out-of-order or retried export), not a reset: it produces no delta and never regresses the remembered high-water mark. Only a genuinely new stream identity (a new start_time) is ever treated as that stream starting over.
- **Correlation is scoped to one received export request, never across requests.** Two concurrent processes can be indistinguishable by resource attributes alone, so correlating across two separate requests risks merging their usage together. This is a strict, load-bearing scoping rule, not just an implementation detail: it depends on the receiver never batching or merging separate OTLP export requests before the deriver sees them, which holds today (`Receiver.handleHTTPMetrics`/`metricsServiceServer.Export` each invoke the metric handler exactly once per received request, and `Pipeline.handleMetrics` passes that slice straight into `UsageDeriver.ProcessResourceMetrics`, which calls `DeriveBatch` once per `ResourceMetrics`). Each `DeriveBatch` call also converts its whole batch atomically (one lock held for every conversion in it), so two concurrent calls for the same stream can never interleave at the per-metric level and mis-correlate one call's data with the other's.

See §5's copilot row for the concrete counters, key formula and edge-case handling this contract produces in practice.

**Rules are enabled only when `SCION_USAGE_SOURCE=native`** (D4) and filtered to `SCION_HARNESS`. A deriver with no matching rules is a no-op.

**Dedupe (idempotence under retries).** An OTLP client may retry a request that sciontool partly processed; for example, a log export fails and the pipeline returns an error. So each derived event is keyed by `sha256(resource-identity ‖ scope ‖ time_unix_nano ‖ event.name ‖ request_id-or-canonical-attrs)`, and a repeat within the LRU window is ignored. Metric-rule dedupe uses the same interval-fingerprint idea as `metricStreams.remember`.

**Consume semantics for metric-sourced rules** (Copilot only in this project):
- On the **GCP** provider, native metrics matched by a usage rule are **removed** from the request after derivation, before `metricStreams.add`. So they are neither rejected, which would fail the whole request and invite client retries, nor admitted.
- On **generic OTLP** they are forwarded unchanged, and the canonical counters are added alongside.
- Native metrics that match no rule keep today's behaviour exactly: rejected on GCP, forwarded on OTLP. This keeps GoogleCloudPlatform/scion#1792's "unknown fields reject on first admission" stance for everything that isn't reviewed.

**Diagnostics.** The deriver adds counters `usage_derived`, `usage_duplicate` and `usage_malformed`, which are events matched by name but with non-numeric or negative token fields. They are exposed through the existing diagnostics surface (`diagnostics.go`). Malformed events never fail the request.

**Privacy.** The deriver reads token counts, model and status only. Only the §3.2 label set leaves the process. `request_id` is used only inside the dedupe hash and is never exported.

### 3.4 Identity (D3)
*Revised per ptone (D3).* "Project" is overloaded in the store, where a GCP project exists too. Scion keeps no time-series data in its DB, and the store (Cloud Monitoring by default) must stay swappable. So identity is defined as **labels on the stored series**:
- **Canonical labels:** `scion_project_id`, `scion_agent_id` and `scion_agent_slug`. The GCP exporter already stamps the first two on *every* metric point from the post-policy resource identity (`gcp_metric_identity.go:251-256`), so hook series are covered today. This design:
  - adds `scion_agent_slug` from the resource key `scion.agent.slug`, which the policy already stamps authoritatively (D7);
  - moves the stamping into an exporter-independent step, applied by the **generic OTLP exporter** too, so any backend receives the same labels. Today generic OTLP keeps identity only on the resource.
- **Dashboard queries:** `projectFilter` becomes `metric.labels.scion_project_id` (`metrics_dashboard.go:239-241`), and unique agents group by `metric.labels.scion_agent_id`. The views display `scion_agent_slug`, falling back to the ID when the slug is absent (N/A per D11: no per-agent slug view shipped; the label itself still stays on the series).
- **Producer point labels `agent_id` and `project_id`** stay on the existing hook metrics, to avoid changing the existing `gen_ai.api.calls` / `agent.session.count` descriptors. They are non-canonical and are not read by the dashboard. The deriver emits the same set for `gen_ai.api.calls`, since one descriptor serves both sources. The new `scion.usage.tokens` omits them.
- **Side benefit:** the existing hook metrics (sessions, calls, tools) become project-filterable immediately. Today they are filtered by `project_id` but come from the resource, and the change makes the whole dashboard consistent.
- Raw native metrics never reach the dashboard, so their lack of point identity doesn't matter.
- **Open item for P1 verification:** Cloud Monitoring must accept `scion_agent_slug` being added to existing auto-created descriptors (`gen_ai.api.calls`, `agent.session.count`). If it rejects the extension, fall back to the D7 alternative (hub batch lookup) and drop the slug label.

### 3.5 Hook side (D1, D4)
- **Tokens.** `hooks/handlers/telemetry.go` records `scion.usage.tokens{token_type}` in place of `scion.hook.tokens.{input,output,cached}`. `cached` maps to `cache_read`.
- **Single source.** When `SCION_USAGE_SOURCE=native`, the hook handler **skips** `gen_ai.api.calls` and token recording only. Tool, session, turn and every other hook metric, span and log is unaffected (D4, narrow). When the variable is `hooks`, hook usage is recorded. **When it is unset, no usage is recorded (D10, vetting gate).** A harness opts in only through a fixture-backed PR. Note for old images: an old sciontool ignores the variable and keeps today's behaviour, which is acceptable because that behaviour is unchanged, not new.
- **Retired names.** The receiver rejects `scion.hook.tokens.*` on the hook scope, the same way it rejects `gen_ai.tokens.*` today (`metric_streams.go:256-258`). An old hook binary talking to a new receiver can't happen, because they are the same binary in one image.

### 3.6 Dashboard (hub)
- **Queries.** Tokens come from `scion.usage.tokens`, grouped by `metric.labels.model` and filtered by `token_type`. Calls come from `gen_ai.api.calls` (unchanged name). All project filters use `metric.labels.scion_project_id`. Agent grouping uses `scion_agent_id`, displayed via `scion_agent_slug` (N/A per D11: no per-agent slug view shipped).
- **API shape.** `DashboardSummary.TotalTokens` = input + output + cache_write + cache_read. `TokensView` keeps `Input` and `Output` and **adds** `CacheRead` and `CacheWrite`; this is additive and the JSON stays backward compatible. The web page adds a cached series in a later phase if wanted (OQ-3).
- **Cumulative math, the core fix.** Replace the "sum every point" loops with a single helper `seriesIncreases(ts) []increment`, used by `querySum`, `queryDailyTimeSeries` and `queryGroupedTimeSeries`:
  1. Fetch raw points over `[windowStart − lookback, windowEnd]` with `lookback = 24h`.
  2. For each time series, partition the points by `interval.start_time` (each start is one collector epoch; a new start is a reset). Sort by end time.
  3. Within a partition, each point's increment is `v_i − v_{i−1}`. For the first point, the increment is `v_0` if its start time is ≥ the fetch start (the epoch began in range). Otherwise it becomes the baseline and contributes 0.
  4. Attribute each increment to the day of its end time. Keep those with end in `[windowStart, windowEnd]`.
  5. Read `Int64Value`; `DoubleValue` is accepted by rounding, as defence against a future double producer. Distribution values are ignored: canonical metrics are never distributions.
  - Where the error sits: an epoch that began more than 24h before the window and wrote no point in the lookback loses at most its pre-window tail. That is acceptable, and documented in code.
  - The existing comment in `querySum` about "one point per short-lived process" is deleted; it describes pre-GoogleCloudPlatform/scion#1792 behaviour.
- **Absent metrics.** A `NotFound` for a metric type with no descriptor yet (for example, `scion.usage.tokens` before its first write) counts as zero, not as a partial failure.
- **Contract pinning.** The leaf package `pkg/telemetrycontract` holds the exported constants for metric names, label keys and `token_type` values. Both `pkg/hub` and sciontool (the deriver and the hook handler) import the **same** constants from it, so that `pkg/hub` doesn't depend on sciontool. The contract test asserts both sides use it (§7).

### 3.7 Harness provisioning (Python, reaches old images)
- **claude:** set `SCION_USAGE_SOURCE=native` when telemetry is enabled. Logs are already `otlp` to local (`claude/provision.py:369-379`), and metrics stay `none` on GCP. No other change.
- **gemini-cli:** *deferred (D6).* No change in this project; it keeps hook-sourced calls. The planned follow-up is the native `gemini_cli.api_response` rule (§5).
- **codex:** logs on, native metrics off on GCP (as claude), `SCION_USAGE_SOURCE=native`.
- **copilot:**
  - always route to the local receiver at `http://127.0.0.1:${SCION_OTEL_HTTP_PORT:-4318}` with `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf` (the receiver accepts only protobuf, `receiver.go:319-321`);
  - `COPILOT_OTEL_ENABLED=true`, `COPILOT_OTEL_EXPORTER_TYPE=otlp-http`, `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta` (kept for forward compatibility, but a real capture shows Copilot CLI 1.0.89 does not honor it and has no documented temporality override at all — every point it exports is cumulative; the deriver converts cumulative points to deltas itself, see §5's copilot row);
  - stop copying cloud endpoint, headers and CA;
  - keep the `SCION_COPILOT_OTEL_*` explicit override only as a local debugging escape, documented as bypassing redaction;
  - set `SCION_NATIVE_TELEMETRY_POLICY` so the env guard applies;
  - set `SCION_USAGE_SOURCE=native`;
  - no `OTEL_RESOURCE_ATTRIBUTES` and no forced cumulative temporality (D5).
  - Related, on the sciontool side (ptone/scion#2249): sciontool's own loopback providers ignore `OTEL_RESOURCE_ATTRIBUTES` / `OTEL_SERVICE_NAME` from their environment. The OTel SDK always merges `resource.Environment()` into a provider resource, so a gRPC client interceptor (`pkg/sciontool/telemetry/loopback_resource.go`) drops every resource key that `buildResource` does not set. This keeps hook, lifecycle, usage-derived and self-metrics inside the GCP resource allowlist.
- **grok-build:** the same local-receiver routing fix (a privacy fix: today it bypasses redaction and identity), subject to D8. `SCION_USAGE_SOURCE` stays unset. Usage is deferred.
- **opencode (hooks), revised after checking the source:** the source code shows the current bridge is largely non-functional. Only `tool.execute.before/after` are real plugin hook keys, and they are read with the wrong fields. `message.updated`, `session.idle` and the other keys are never invoked, so opencode publishes **zero** model-end events today; the earlier "inflated calls" concern turned out to be based on a misreading of the bridge.
  - **Rewrite `scion-bridge.js`** to subscribe through the generic `event` hook, filtering in JS *before* `execSync`, because `message.part.delta` fires per chunk.
  - **Usage:** on `message.part.updated` with `part.type=="step-finish"`, emit one model-end carrying `tokens.{input,output,reasoning,cache.read,cache.write}`, deduped on `(sessionID, part.messageID, part.id)`. Fork replays are excluded by counting only parts whose message was seen live. The model is joined from the parent assistant message's `providerID/modelID`, which the bridge caches from `message.updated`.
  - **Token mapping:** OpenCode stores exclusive values, so input→`input`, output→`output`, reasoning→`reasoning`, cache.read→`cache_read`, cache.write→`cache_write`. Canonical `output` includes reasoning, so the mapping emits `output + reasoning` as `output` and `reasoning` informationally. All-zero tokens with `total` undefined mean unknown: count the call, emit no tokens.
  - **Same rewrite for the other events:** route `session.idle`, `session.created`, `session.error` and `permission.*` through the event hook, and fix the tool args (`output.args`) and tool success. `session.error` is received through the event hook but emits nothing by itself: turn end is `session.idle` after a `session.status` busy (or retry), never `session.error` itself, so a recoverable error or user abort does not end a turn twice. Its error name/status is instead recorded per armed session (only those structured fields, e.g. `APIError (status 429)`, never the free-text message; not for a user abort `MessageAbortedError` or a child session, cleared if the session goes busy again) and attached as `error` to that turn's one gated `session.idle`, so the failed turn's `agent.turn.end` span and log carry error status (ptone/scion#2244).
  - ~~**Pin `opencode-ai`** in `harnesses/opencode/Dockerfile`~~ **Superseded by D11:** no pin. Record the fixture's CLI version in the test (1.18.32 was the version investigated). Confirm with ptone at P3b start.
  - **Known undercount:** calls that produce no step-finish (failed or retried attempts, aborts, title and agent generation). This is consistent with the contract's "completed model responses" meaning.
  - **Alternative rejected for now:** OpenCode's `experimental.openTelemetry` spans. They cover all provider calls, including title generation, but need OTLP/HTTP **JSON** support in the receiver (today protobuf only, `receiver.go:319-321`) plus a span-derivation rule, and they carry `session.id`. That is noted as a possible follow-up.
- **antigravity (hooks):** `PostInvocation` already maps to model-end (calls). First capture a `PostInvocation` payload: if it carries usage, map the fields in `antigravity/dialect.yaml`; if not, it ships calls-only and tokens become a follow-up. Also confirm that one `PostInvocation` equals one model request, not one agent turn. If it is per turn, the calls panel labels it as such, or antigravity is left calls-less; the PR records the outcome. Native telemetry stays disabled: `enableTelemetry` is product telemetry, not OTLP.
- **Hook token plumbing:** to carry the canonical types, the hook event data gains `CacheWriteTokens` and `ReasoningTokens` alongside `Input`/`Output`/`CachedTokens`, and `dialects/mapping.go` gains the explicit yaml fields `cache_write_tokens` and `reasoning_tokens`.
- **Why each harness declares its source in `provision.py`:** env is the existing channel, as with `SCION_NATIVE_TELEMETRY_POLICY`, and `provision.py` is host-copied, so the flag reaches existing images.
- **Safe on old images:** an old sciontool ignores `SCION_USAGE_SOURCE`, so the hook path behaves exactly as today on old images. Nothing regresses. Usage simply doesn't appear until `scion-base` is rebuilt.

## 4. Alternatives considered

| Alternative | Why rejected |
|---|---|
| **A. Teach the dashboard every native name** (the miller79/scion#137 and miller79/scion#138 approach): `claude_code.token.usage`, `gen_ai.client.token.usage`, and so on | Normalization lands in the least appropriate layer: every new harness or CLI rename becomes a hub change. It doesn't work on the default GCP path, because native metrics are rejected there and lack point identity. It needs the allowlist widened with harness-specific keys (`type`, `gen_ai.token.type`). And Claude has no call metric, so it forces an external collector. |
| **B. Widen the GCP allowlist and copy resource identity onto native points** | This reverses GoogleCloudPlatform/scion#1792's deliberate privacy stance (`agent-observability-milestone.md:128-160`). Each harness's attribute vocabulary becomes a Cloud descriptor: several per harness, unbounded over time, with label churn every time a CLI renames something. It still needs dashboard-side per-harness logic (A), and it still has no Claude call count. |
| **C. Derive from native *metrics* rather than events** | Claude has no call-count metric. Cumulative native metrics need stateful diffing, which events don't. And it requires re-enabling native metrics on GCP, where they would be rejected unless consumed. It is used only for Copilot, where events aren't confirmed. A real capture later showed Copilot's metrics are cumulative-only with no working delta override, so the deriver performs that stateful diffing itself (§3.3's batch-correlation contract) rather than requiring delta from the source. |
| **D. Parse transcripts or session files** (Claude JSONL `usage`, Gemini `usageMetadata` in hook payloads) | Transcript parsing ties us to private file formats with no stability promise, and it's per-harness code running on each hook invocation. Native OTel events are the documented, versioned contract. The hook-payload route stays open for harnesses whose hooks carry usage (muse-code), via the hook path. |
| **E. Ship an OTel Collector sidecar** (miller79/scion#138's count connector) | It adds a component per agent and still doesn't solve GCP identity or privacy. The sciontool receiver already *is* the collector. |
| **F. Producer-stamped `project_id`/`agent_id` as canonical** (the original draft) | Rejected by ptone (D3): "project" is ambiguous in a store that also has GCP projects, and every producer would have to stamp identity correctly. Exporter-stamped `scion_*` labels come from a single authoritative place. |
| **H. Resolve the agent slug by hub DB lookup** instead of a stored label | Kept as the fallback (D7). It couples the dashboard to the DB and loses purged agents. |
| **G. Two token metric families (`scion.hook.tokens.*` plus a native family)** | Summing across sources belongs in the pipeline, not the dashboard. The retired family has almost no data. |

The load-bearing choices are §3.2 (names, labels, temporality: Cloud descriptors are sticky and history is keyed on them) and §3.3's placement in sciontool (it reaches agents only through image rebuilds). The easily reversible ones: the dashboard's lookback, the dedupe window, the TotalTokens composition, and the per-harness source flag.

## 5. Per-harness table

| Harness | Source after this project | Derivation rule (`MatchLog`/`metricBatchDeriver`) | Token mapping | Phase | Status |
|---|---|---|---|---|---|
| claude | native events | log, scope `com.anthropic.claude_code.events`, `event.name=api_request` → calls+1 | `input_tokens`→input, `output_tokens`→output, `cache_read_tokens`→cache_read, `cache_creation_tokens`→cache_write; status success (`api_error`→calls+1, status error, no tokens) | 1 | in scope |
| gemini-cli | hooks (unchanged) — **deferred** | follow-up rule: log `gemini_cli.api_response` → calls+1; `gemini_cli.api_error` → calls+1 error | follow-up: input=`input_token_count − cached_content_token_count`, output=`output_token_count`, cache_read=`cached_content_token_count`, reasoning=`thoughts_token_count` | — | deferred (D6) |
| codex | native events | log `codex.sse_event` with `event.kind=response.completed` → calls+1 | input=`input_token_count − cached_token_count`, output=`output_token_count`, cache_read=`cached_token_count`, reasoning=`reasoning_token_count` | 3c | in scope, fixture-gated |
| copilot | native metrics, converted from cumulative | **Confirmed by a real capture (CLI 1.0.89, phase 3a):** `gen_ai.client.token.usage` does not exist past CLI 1.0.45; the post-1.0.45 GenAI-semconv shape is used instead. Calls: the `gen_ai.client.inference.operation.input_tokens` histogram's per-export observation count (confirmed exactly one observation per model call). Tokens: each `gen_ai.client.inference.usage.*` counter; `gen_ai.client.inference.operation.{input,output}_tokens` histograms are never read for tokens. model=`gen_ai.request.model`/`gen_ai.response.model`, confirmed always on the point, never only on the resource. **Temporality note:** every Copilot metric point is cumulative — `copilot help monitoring` documents no temporality override, and `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta` makes no observed difference. The deriver converts cumulative points to per-export deltas itself (a bounded, per-stream last-value cache keyed on resource+scope+metric+point attributes+start_time, with a restart baseline and a stale-point rule (a value going backwards under the same start_time emits 0 and never lowers the stored value; only a new start_time starts a new stream) — see `usage_copilot_metrics.go`); a delta-temporal point, from any rule, is unaffected and passes through unchanged. **Batch-correlation contract:** a real capture also shows `input_tokens` is not exclusive of cached tokens (per call, `input_tokens − cache_read.input_tokens − cache_write.input_tokens` is constant): the deriver correlates the three post-conversion deltas by stream identity minus the metric-name/token-type dimension, within a single received export request only (never across two requests — Copilot's resource carries no per-process identifier, so cross-request correlation could merge two concurrent processes' usage). A cache metric absent from a batch counts as 0; a negative result clamps to 0 and is logged, never emitted, never panics. After the bounded state evicts an entry, a still-running process's own new stream is also baselined if its start_time does not exceed the largest evicted start_time — a conservative undercount, not a bug, and counted. | `input_tokens − cache_read − cache_write`→input, `output_tokens`→output (already inclusive of reasoning), `cache_read.input_tokens`→cache_read, `cache_write.input_tokens`→cache_write, `reasoning.output_tokens`→reasoning (informational, already counted in output) | 3a | in scope, fixture-gated |
| grok-build | hooks (unchanged) | none | — | 0 (routing fix only, D8) | usage deferred (D6) |
| muse-code | hooks | hook tokens land in `scion.usage.tokens` via §3.5 | hook fields | 2 (automatic) | runtime verification is follow-up |
| antigravity | hooks | `PostInvocation` → model-end (calls), confirmed one per main-loop model request, not per turn (auxiliary calls such as title generation fire no hook: known undercount) | none: a real capture from `agy` 1.2.12 shows `PreInvocation`/`PostInvocation` never carry a usage or token field | 3d | in scope, fixture-gated; calls-only (confirmed no usage in the hook payload, D9); tokens filed as a follow-up |
| opencode | hooks (bridge rewritten onto the `event` hook) | model-end per `step-finish` part, deduped on (sessionID, messageID, part.id), fork replays excluded, model from parent message | input→input, output+reasoning→output, reasoning→reasoning, cache.read→cache_read, cache.write→cache_write (exclusive values, `session.ts:338-377`) | 3b | in scope; source-vetted, fixture-gated |
| hermes | none | — | — | — | out of scope |

**Vetting principle (ptone, D9/D10).** Hook-sourced usage is published only when it is vetted as valid and sound: a captured payload from the pinned CLI version, a documented mapping, and a check that one published call equals one provider model request and that tokens match the CLI's own accounting. Unvetted hook usage is not published at all; it is never published with a caveat.

**Fixture gate.** Every rule ships with a **captured** OTLP payload from the real CLI version pinned in that harness's image, checked in under `pkg/sciontool/telemetry/testdata/usage/<harness>-<version>.pb.json` with secrets scrubbed. If a fixture can't be captured, the rule does not ship and the harness moves to follow-up. The mappings above are from vendor docs and source inspection; the fixture is the authority.

## 6. Migration and rollout
- **Order independence.** Hub and sciontool changes may land in either order. The new dashboard reads names that the old sciontool doesn't emit (usage shows as zero, same as today), and the old dashboard ignores the new names.
- **History.** `gen_ai.api.calls` history remains readable, and the corrected math makes it *more* accurate. Tokens start from zero when the new sciontool deploys. Old `gen_ai.tokens.*` data is essentially empty, and is not read.
- **Images (D5).** All sciontool-side value (the deriver, the hook token rename, the allowlist) arrives only when an operator rebuilds `scion-base` and then the harnesses. The rollout notes and the metrics docs say so explicitly. Phase 4 makes the embedded sciontool commit visible, as an image label and in `sciontool version`, and makes `build-images.sh --target harnesses` warn when it inherits a `scion-base` it didn't build in the same run.
- **Python** changes reach existing images at agent creation. Each is safe against an old sciontool, as argued in §3.7.
- **Cloud descriptors.** `scion.usage.tokens` gets a new auto-created descriptor. `gen_ai.api.calls` keeps its descriptor (same labels). No descriptor deletion is needed.
- **Docs.** Update `docs-site/.../single-node/metrics.md` (metric table, the "dashboard gap" paragraph, the usage source section), `.design/hosted/agent-observability-milestone.md` (the contract section), `harnesses/copilot/README.md`, and add a stale note to `.design/metrics/claude-otel.md`.

## 7. Test strategy
1. **Contract package** `pkg/telemetrycontract`: the names, labels and enums as constants. It is the single source of truth.
2. **Dashboard contract test** (`pkg/hub/metrics_dashboard_contract_test.go`): a fake `ListTimeSeries` client (introduce a narrow interface over `monitoring.MetricClient`) records every request filter. The test asserts:
   - each view queries exactly the contract metric types;
   - project views filter on `metric.labels.scion_project_id` (`contract.ProjectLabel`) and agent grouping uses `scion_agent_id`;
   - grouping uses the contract label keys.
   Renaming a metric on either side without the other fails a test.
3. **Dashboard math tests:** table tests for `seriesIncreases` covering a single-epoch series with multiple flushes (the value must equal the final total, not the sum of points), a reset mid-window, an epoch starting before the lookback, day bucketing, a double value, and a missing descriptor.
4. **Deriver unit tests**, one per rule, driven by the captured fixture. They assert the canonical increments, the label set (exactly §3.2), no leaked attributes, dedupe on a replayed request, malformed-event handling, and the pre-filter behaviour (`Filter.Include` excluding `api_request` still derives).
5. **Pipeline end-to-end test in sciontool** (extends the existing receiver tests, GCP mode with a fake exporter): post the Claude fixture over OTLP/HTTP → flush → assert the exported GCP `ResourceMetrics` contain `gen_ai.api.calls` and `scion.usage.tokens` points with `scion_project_id`, `scion_agent_id`, `scion_agent_slug` and `token_type`, cumulative with a collector epoch, and pass `gcpIdentityMetrics`. The Copilot variant also asserts the raw histogram was consumed, not rejected.
6. **Cross-side golden test:** the exported point from (5) is serialized as a Cloud `TimeSeries` fixture and fed to the dashboard fake from (2). The dashboard returns the expected totals. This single test pins emitter → dashboard.
7. **Hook tests:** `telemetry_test.go` asserts `scion.usage.tokens` replaces `scion.hook.tokens.*`, and that `SCION_USAGE_SOURCE=native` suppresses calls and tokens but not tool or session metrics.
8. **Provision tests:** extend `harnesses/telemetry_provision_test.py` and `copilot/provision_test.py`:
   - copilot always local on 4318 with http/protobuf, never copying cloud headers or CA, requesting delta temporality (kept for forward compatibility; a real capture shows Copilot does not honor it and the deriver converts cumulative points to deltas itself);
   - grok-build always local;
   - claude, copilot and codex set `SCION_USAGE_SOURCE=native`;
   - codex metrics are off on GCP;
   - opencode bridge: JS unit test, or a fixture replay through `sciontool hook --dialect=opencode`.
9. **Live validation** (phase gate, manual, staging hub): after a `scion-base` rebuild, run one agent per in-scope harness for a few prompts, then check the project dashboard shows non-zero calls and tokens attributed to that agent's project, and that the numbers are plausible against the CLI's own usage output (±5%).

## 8. Open questions
- **OQ-1 (resolved by a real capture, phase 3a).** Copilot per-call semantics: does the calls source record exactly one observation per model call? Confirmed yes: `gen_ai.client.inference.operation.input_tokens`'s per-export observation count matches the session's chat-span count exactly (a 3-model-call session produced count=3), independently corroborated by `gen_ai.invoke_agent.inference_calls`'s delta sum. The old `gen_ai.client.token.usage` histogram this question originally named does not exist past CLI 1.0.45 and is not used; see §5's copilot row.
- **OQ-2.** Hub DB sink: the deriver runs in the same process as `TelemetryHandler`'s aggregator (`init.go:459-474`). Should derived increments also feed `OnSessionEnd` totals so that `/metrics/summary` shows tokens? Recommended as a follow-up, out of scope here.
- **OQ-3.** Web UI: show the cache-read and cache-write series on the tokens chart now, or leave that to the follow-up? Default: the API adds the fields and the UI is unchanged.
- **OQ-4.** Codex SessionStart/End semantics and antigravity PostInvocation granularity and usage: resolved by the fixtures. The OpenCode field names are verified from source, and a fixture still pins them.

## 9. Implementation phases
Each phase is one upstream PR, independently mergeable, and must not regress current behaviour.

**Phase 1: vertical slice, Claude end to end** (depends on nothing; **gates all of phases 2 and 3**)
- Files:
  - new `pkg/telemetrycontract/contract.go`;
  - `gcp_metric_identity.go` and `exporter.go`: a shared identity-label step (`scion_*` labels) applied by both exporters, plus `scion_agent_slug`;
  - new `pkg/sciontool/telemetry/usage.go` and `usage_test.go`, plus a Claude fixture under `testdata/usage/`;
  - `pipeline.go` (call the deriver in `handleLogs`), `providers.go` (deriver meter provider), `metric_streams.go` (`isHookCounter` and the reserved scope for `scion.usage.tokens`), `gcp_metric_identity.go` (`token_type` in the allowlist);
  - `pkg/hub/metrics_dashboard.go` (contract names, `seriesIncreases`, absent-metric handling, TokensView fields), new `metrics_dashboard_contract_test.go` and math tests;
  - `harnesses/claude/provision.py` and the test (`SCION_USAGE_SOURCE=native`);
  - docs (`metrics.md`).
- AC:
  - AC-1.1: a Claude `api_request` fixture posted to the receiver yields exactly one `gen_ai.api.calls` increment and the right `scion.usage.tokens` increments, with the exact §3.2 label set.
  - AC-1.1b: every exported metric point, on both the GCP and generic-OTLP exporters, carries `scion_project_id`, `scion_agent_id` and `scion_agent_slug` taken from authoritative identity. Producer-supplied values for these keys are rejected. The dashboard filters on `scion_project_id` and displays the slug (N/A per D11: no per-agent slug view shipped). The live gate confirms Cloud Monitoring accepts the slug label on the existing descriptors; otherwise apply the D7 fallback.
  - AC-1.2: the GCP-mode end-to-end test (§7.5) passes, and the raw Claude log records are still exported and redacted as before (the existing privacy tests are unchanged and green).
  - AC-1.3: the dashboard contract test (§7.2), the math tests (§7.3) and the golden cross-side test (§7.6) pass. The summary total for a single-epoch series with N flushes equals the last value.
  - AC-1.4: derivation happens when `Filter.Include` excludes `api_request`, and a replayed request doesn't double count.
  - AC-1.5 (validation gate): on staging, after a `scion-base` rebuild, one Claude agent's calls and tokens appear under its project on the dashboard. **Phases 2 and 3 do not start until this gate is confirmed.**
- Size: large but cohesive (about 800–1200 LOC including tests). If a reviewer asks, it can be split into two commits inside one PR: sciontool, then hub. It must not be split into two PRs, because the slice is the point.

**Phase 0: route copilot and grok-build native OTel to the local receiver** (independent; can merge any time, in parallel with phase 1)
- Files: `harnesses/copilot/provision.py`, `provision_test.py`, `README.md`; `harnesses/grok-build/provision.py` and its tests.
- AC:
  - both always target 127.0.0.1;
  - copilot uses 4318 with http/protobuf, `COPILOT_OTEL_ENABLED=true` and `otlp-http`;
  - no cloud endpoint, headers or CA are copied;
  - `SCION_NATIVE_TELEMETRY_POLICY` is set;
  - the explicit `SCION_*_OTEL_ENDPOINT` override is documented as a debugging bypass;
  - Python tests are green.
- Note: until phase 3a, Copilot's raw metrics are rejected on GCP, the same as any unknown native metric. Hook metrics are unaffected.

**Phase 2: hook-side alignment** (after the phase 1 gate)
- Files: `hooks/handlers/telemetry.go` and its test, `metric_streams.go` (reject `scion.hook.tokens.*`), docs.
- AC:
  - hooks emit `scion.usage.tokens{token_type}`;
  - `SCION_USAGE_SOURCE=native` suppresses hook calls and tokens only;
  - muse-code hook tokens, if present, reach the dashboard (unit-level check with a muse dialect fixture).

**Phase 3: per-harness usage** (after the phase 1 gate. 3a and 3c are parallel with each other and with phase 2. 3b and 3d need phase 2 for the hook token plumbing.)
- **3a, copilot:**
  - metric rule plus consume-on-GCP in `handleMetrics`;
  - provision adds `delta` temporality and `SCION_USAGE_SOURCE=native`;
  - fixture from Copilot 1.0.88+.
  - AC: the raw histogram is consumed, not rejected, on GCP; canonical increments match the fixture; OQ-1 is resolved in the PR description.
- **3b, opencode (hooks):**
  - rewrite `scion-bridge.js` onto the generic `event` hook (usage via `step-finish`, plus session, idle, error and permission events), and fix the tool args and success;
  - `opencode/dialect.yaml` token mapping; ~~pin `opencode-ai` in the Dockerfile~~ (superseded by D11: no pin; record the fixture's CLI version in the test instead);
  - a captured bus-event fixture from the pinned version, including a fork and a multi-step tool loop;
  - `SCION_USAGE_SOURCE=hooks` in opencode `provision.py` (the D10 opt-in).
  - AC:
    - N completed LLM steps give calls = N;
    - streaming deltas, repeated `message.updated` events and fork replays add 0;
    - tokens per `token_type` match the step-finish parts;
    - agent-end and session events now fire;
    - tool_input is populated.
- **3c, codex:**
  - log rule; provision sets metrics off on GCP and `SCION_USAGE_SOURCE=native`;
  - fixture.
  - AC: native calls and tokens are derived; with `SCION_USAGE_SOURCE=native` a hook model-end would not add calls (codex has none today, and the test pins it); tool hooks are unaffected.
- **3d, antigravity (hooks):**
  - capture a `PostInvocation` payload; map usage fields in `antigravity/dialect.yaml` if present;
  - confirm whether invocation granularity is per model request or per turn.
  - AC: calls appear per invocation; tokens appear if the payload has them, otherwise the PR records calls-only and files a follow-up; the granularity finding is documented in the harness README.

**Phase 4: build provenance and rollout docs** (independent; parallel with anything)
- Files: `image-build/scion-base/Dockerfile` (pass `GIT_COMMIT`, set ldflags Version/Commit, add OCI label `org.opencontainers.image.revision`), `image-build/scripts/build-images.sh` (warn on an inherited `scion-base`), and `cloudbuild*.yaml` (pass the commit through). Docs: an upgrade note that usage telemetry requires a `scion-base` rebuild.
- AC:
  - `sciontool version` inside a freshly built image prints the commit;
  - `docker inspect` shows the label;
  - a harness-only build prints the warning.

**Honest count:** 7 PRs (0, 1, 2, 3a, 3b, 3c, 3d) plus 1 optional (4). Critical path: 1 → gate → 2 → 3b/3d. Phases 0 and 4 can run any time; 3a and 3c run after the gate, in parallel with 2. Deferred: gemini-cli native rule, grok-build usage.

## 10. Acceptance criteria (project level)
1. On a GCP-provider hub with rebuilt images, the claude, copilot, codex and opencode agents (and antigravity for calls, plus tokens if available) each show non-zero model calls and tokens on both the global and the project dashboard, attributed to the correct project, model and harness, and within ±5% of the CLI's own reported usage for the same session.
2. No dashboard total is inflated by the flush count: the cumulative-math tests pass, and a live single-agent check matches.
3. No new attribute reaches Cloud Monitoring labels beyond `token_type` (producer-settable) and `scion_agent_slug` (exporter-stamped from authoritative identity). The existing privacy tests are green.
3b. The dashboard filters on `scion_project_id` and groups on `scion_agent_id`, presenting slugs (N/A per D11: no per-agent slug view shipped), and a contract test pins this.
4. Copilot and grok-build never send telemetry directly to a cloud endpoint by default.
5. The contract constants are used by the hub, the hook handler and the deriver. The contract, golden and fixture tests exist and pass.
6. On old images (pre-change sciontool), nothing regresses relative to today.
7. The docs describe the contract, the per-harness sources, and the rebuild requirement. The copilot README no longer says "No telemetry integration".
8. Follow-up issues are filed for: gemini-cli native usage rule; grok-build usage; muse-code verification; antigravity tokens (if calls-only); Hub DB token sink; session reconciliation; cost metric.
