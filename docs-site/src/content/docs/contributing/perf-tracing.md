---
title: Hub Performance Tracing and Benchmarks
description: Using server.hub.perf_trace and the perf/bench harness to measure agent-list performance and hold host-independent regression budgets.
---

This guide is for contributors who need to find out where the Hub spends time on an agent-list request, or who want to check that a change does not make the authorization path more expensive. It covers two tools:

- **Request performance tracing** (`server.hub.perf_trace`): a startup setting that makes the Hub write one `perf_trace` log line per request, with phase timings, authorization store-read counts, decision counts and database pool waits.
- **The `perf/bench` harness**: tools that seed a throwaway local Hub with 25, 100 or 500 agents and benchmark it through the API and in a browser.

The setting itself is documented in the [server configuration reference](/scion/reference/server-config/#request-performance-tracing). That reference is the source of truth for what the setting does; this page shows how to use it. The [perf/bench README](https://github.com/GoogleCloudPlatform/scion/blob/main/perf/bench/README.md) has the full harness details and caveats.

## Request performance tracing

### Turning it on

Tracing is **off by default** and is read **only at startup**. Changing it needs a Hub restart, and it cannot be set through the admin server-config API. Turn it on in the settings file:

```yaml
server:
  hub:
    perf_trace: true
```

or with the environment variable `SCION_SERVER_HUB_PERFTRACE=true`. When it is on, the Hub logs a warning at startup: `Request performance tracing is on (server.hub.perf_trace); per-request perf_trace lines are logged`.

With tracing off, the Hub installs no tracing middleware and no store or audit decorator, and adds no headers or log lines.

### What it records

Every Hub API request writes one log line with message `perf_trace` and `subsystem` `hub.perf-trace`. When an SSE stream opens on the web server's SSE endpoint (`/events`), it writes two log lines: one when the stream opens (`"sse_stage":"connect"`) and one when it closes (`"sse_stage":"close"`). A connection that is refused writes only the `close` line, and a request with a malformed subject list writes none.

Each line records:

| What | Keys |
| :--- | :--- |
| Endpoint class | `endpoint`: one of `agents.global.legacy`, `agents.global.sorted`, `agents.project.legacy`, `agents.project.sorted`, `agents.project.sorted_agent`, `sse.events`, `other` |
| Total time | `elapsed_us` |
| Agent-list phases | `phase_<name>_us` and `phase_<name>_n` for `list_scope_authz`, `list_db_read`, `list_read_authz`, `enrich`, `capabilities`, `messageability`, `scope_capabilities`, `serialize` |
| SSE phases | `phase_sse_expand_*`, `phase_sse_authorize_*`, `phase_sse_write_*`, and `sse_events` (the number of events written, only when nonzero) |
| Authorization store reads | `store_<Method>_n` and `store_<Method>_us` per method, plus the totals `authz_store_calls` and `authz_store_us` |
| Decisions | `audit_records`, `audit_allow`, `audit_deny`, `audit_other`, `audit_emit_us` |
| Database pool | `db_wait_count`, `db_wait_us`, `db_in_use`, `db_open` (only when the store exposes a connection pool) |
| Correlation | `method`, `request_id` |

A phase or store method appears only if the request entered or called it at least once. The `store_` methods counted are the inputs the authorization service reads: `GetEffectiveGroups`, `GetEffectiveGroupsForAgent`, `GetParentGroups`, `GetUserGroups`, `GetGroupMembership`, `GetGroupBySlug`, `ListRoleBindingsForPrincipals`, `ListRoleBindingsForPrincipal`, `GetRoleDefinition`, `GetRoleDefinitionsByIDs`, `GetRoleDefinitionByName`, `ListAccessConstraints`, `GetDelegationEdgesForDelegate`, `GetUser`, `GetUserAccessToken`, `GetAgent`, `GetProject`, `GetProjectMembership` and `GetHubSetting`. They are counted after request-local reuse, so each count is a real store round trip. Reads made in other ways (relationship progeny lookups, reads inside a store transaction) are not counted. The reference's [Counted scope](/scion/reference/server-config/#request-performance-tracing) paragraph has the details.

Every authorization decision emits one decision-audit record, so `audit_records` is the number of decisions the request made. `audit_emit_us` is only the time spent queueing the records; the database write happens later, off the request path.

The database pool figures are deltas across the whole shared pool while the request ran. They include waits caused by concurrent requests and by the audit writer, so read them as "how contended was the pool", not "how much did this request wait".

### The log line format

The Hub's default log format is one JSON object per line on standard output. Keys come in a fixed order: `endpoint` and `elapsed_us`, then phases sorted by name, then store methods sorted by name, then the totals, the audit counts, `sse_events` if any, the pool figures, `sse_stage` for SSE, `method` and `request_id`. This line is from an example run on one machine (SQLite, the seeded non-admin member listing a 25-agent project). Only the timestamp and request ID were changed:

```json
{"time":"2026-01-01T00:00:00.000000000Z","level":"INFO","msg":"perf_trace","component":"scion-hub","subsystem":"hub.perf-trace","endpoint":"agents.project.legacy","elapsed_us":557495,"phase_capabilities_us":484223,"phase_capabilities_n":1,"phase_enrich_us":43829,"phase_enrich_n":1,"phase_list_db_read_us":16435,"phase_list_db_read_n":2,"phase_list_read_authz_us":2695,"phase_list_read_authz_n":2,"phase_list_scope_authz_us":1793,"phase_list_scope_authz_n":1,"phase_scope_capabilities_us":125,"phase_scope_capabilities_n":1,"phase_serialize_us":2917,"phase_serialize_n":1,"store_GetEffectiveGroups_n":1,"store_GetEffectiveGroups_us":631,"store_GetRoleDefinitionsByIDs_n":1,"store_GetRoleDefinitionsByIDs_us":358,"store_GetUser_n":84,"store_GetUser_us":472801,"store_ListAccessConstraints_n":1,"store_ListAccessConstraints_us":303,"store_ListRoleBindingsForPrincipals_n":1,"store_ListRoleBindingsForPrincipals_us":340,"authz_store_calls":88,"authz_store_us":474434,"audit_records":255,"audit_allow":162,"audit_deny":93,"audit_other":0,"audit_emit_us":188,"db_wait_count":306,"db_wait_us":1887753,"db_in_use":1,"db_open":1,"method":"GET","request_id":"00000000-0000-4000-8000-000000000000"}
```

To pull out the lines for one endpoint class from a Hub log:

```sh
jq -c 'select(.msg == "perf_trace" and .endpoint == "agents.project.legacy")' hub.log
```

With GCP log formatting (`SCION_LOG_GCP=true`, or on Cloud Run) the message is written under `message` instead of `msg`. Change the filter to match, and note that `apibench --hub-perf-log` (below) expects the default format.

### Response headers (admin only)

An unscoped local platform admin can also get the trace in the response by sending the request header `X-Scion-Perf-Trace: 1`. The response then carries `X-Scion-Perf-Endpoint`, `X-Scion-Perf-Phases`, `X-Scion-Perf-Phase-Counts`, `X-Scion-Perf-Store-Calls`, `X-Scion-Perf-Store-Us`, `X-Scion-Perf-Decisions` and, when pool figures are available, `X-Scion-Perf-DB`. Apart from `X-Scion-Perf-Endpoint` (the endpoint class), values are comma-separated `name=integer` pairs, and durations are in microseconds. For example, `X-Scion-Perf-Decisions: count=255,allow=162,deny=93,other=0,audit_us=188`.

The headers are admin-only and presentation-only: they grant no access and change nothing about the response except adding these headers. No other caller gets them, and every caller's requests are still traced and logged. The headers are set when the response starts, so they never include `serialize`; the log line does. See the reference for [why the headers are restricted](/scion/reference/server-config/#request-performance-tracing).

To look at the headers by hand on a local Hub, with an admin token in `ADMIN_TOKEN`:

```sh
curl -s -o /dev/null -D - \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "X-Scion-Perf-Trace: 1" \
  http://127.0.0.1:19810/api/v1/agents | grep -i '^x-scion-perf-'
```

### Overhead, cardinality and privacy

- **Overhead.** Each recorded phase or store read adds two clock reads and one uncontended lock. Each request also adds a few hundred bytes for the trace, one pool-stats read at the start and one at the end, and one log line. The main cost on a busy Hub is log volume, so on production turn it on only for a short diagnosis window.
- **Cardinality.** Phase names, store method names, endpoint classes and audit outcomes are fixed sets defined in code. No request-derived value is ever used as a key, so the number of distinct keys is bounded.
- **Privacy.** A line never contains the path, query, caller identity, emails, tokens, secrets or configuration values. `request_id` is a random per-request ID for correlation only. The same value is returned to the client in the `X-Request-ID` response header.

## The perf/bench harness

The harness lives in `perf/bench/` and in `web/e2e-perf/`:

| Tool | Path | What it does |
| :--- | :--- | :--- |
| Seeder | `perf/bench/seed` | Writes one project, an owner, a non-admin project member and N synthetic agents straight into a fresh Hub SQLite database. |
| API benchmark | `perf/bench/apibench` | Sends repeated timed requests to the agent-list endpoints as the seeded member and writes a JSON report. |
| Browser benchmark | `web/e2e-perf/large-project-bench.mjs` | Uses Playwright to measure the project grid, list and graph views, and how fast SSE updates appear in the page. |
| Report schema | `perf/bench/internal/benchout/schema.go` | The JSON types both Go tools write. |

:::caution
Run all of this against a **throwaway local Hub only**. The README's [Isolate the hub environment](https://github.com/GoogleCloudPlatform/scion/blob/main/perf/bench/README.md#isolate-the-hub-environment-required) section explains why the Hub must be started with a scrubbed environment (`env -i`, metadata server pointed at a dead address) and how to check that the isolation worked. Never pass `--enable-test-login` to anything but a disposable Hub.
:::

### 1. Build the tools

From the repository root, in a regular clone rather than a `git worktree`, so that the reports can record which commit they came from (the README explains why):

```sh
go build -o /tmp/scion-bin ./cmd/scion/
go build -o /tmp/seed-bin ./perf/bench/seed/
go build -o /tmp/apibench-bin ./perf/bench/apibench/
```

Do not add `-buildvcs=false` to the `seed` and `apibench` builds. Without the VCS stamp the report's `harnessCommit` is empty.

### 2. Seed a Hub at 25, 100 and 500 agents

Each seed needs a fresh database file, because the seeder refuses a non-empty `--db`. Use the same `--session-secret` when you start the Hub: the seeded bearer tokens are signed with a key derived from it.

```sh
mkdir -p /tmp/scion-bench/home
for n in 25 100 500; do
  /tmp/seed-bin \
    --db /tmp/scion-bench/hub$n.db \
    --session-secret bench-secret-1 \
    --agents $n \
    --project-slug bench-$n \
    --out /tmp/scion-bench/seed-$n.json
done
```

`--rand-seed` (default `42`) makes the synthetic data reproducible, and that is what makes counts comparable across runs. Keep it fixed when you compare counts. The seed JSON records the distribution that was actually generated, the project ID, and the owner and member tokens that the benchmarks read.

### 3. Start the Hub with tracing on

Start one isolated Hub per database, with the tracing variable added to the scrubbed environment and the output sent to a log file. Run it from outside any repository checkout, for example from `/tmp/scion-bench`:

```sh
cd /tmp/scion-bench
env -i HOME=/tmp/scion-bench/home PATH="$PATH" SCION_CLI_MODE=human \
  GCE_METADATA_HOST=127.0.0.1:1 GCE_METADATA_IP=127.0.0.1:1 \
  SCION_SERVER_HUB_PERFTRACE=true \
  /tmp/scion-bin server start \
  --hosted --enable-hub \
  --db /tmp/scion-bench/hub25.db \
  --session-secret bench-secret-1 \
  --port 19810 --host 127.0.0.1 \
  --foreground --no-auto-migrate \
  > /tmp/scion-bench/hub25.log 2>&1
```

Check `hub25.log` for the startup warning that tracing is on before you benchmark.

### 4. Run the API benchmark

```sh
/tmp/apibench-bin \
  --hub http://127.0.0.1:19810 \
  --seed /tmp/scion-bench/seed-25.json \
  --runs 5 --warmup 1 \
  --want-perf-trace \
  --hub-perf-log /tmp/scion-bench/hub25.log \
  --out /tmp/scion-bench/api-25.json \
  --notes "one hub, otherwise idle host"
```

This runs three scenarios as the seeded member: `project-agents-list` (`GET /api/v1/projects/{id}/agents`), `global-agents-list-unscoped` (`GET /api/v1/agents`) and `global-agents-list-scoped` (`GET /api/v1/agents?projectId={id}`). Other flags: `--timeout-seconds` (default `60`; raise it at 500 agents) and `--notes`, which goes into the report. Always use `--notes` to describe anything the report cannot capture, such as other Hubs running on the same host.

- `--want-perf-trace` sends `X-Scion-Perf-Trace: 1` and records any `X-Scion-Perf-*` headers that come back. The member is not an admin, so on its own this flag gets no headers.
- `--hub-perf-log` makes `apibench` read the Hub's JSON log after the run and join each attempt to its `perf_trace` line by request ID, using the `X-Request-ID` the Hub returned. The joined data is stored in the same header-shaped form, and it includes the `serialize` phase that headers cannot carry.

The tool prints how many attempts it joined. If the `--hub-perf-log` path does not exist, `apibench` exits with `read hub perf log` before it writes the report, so check the path before a long run. With a valid path, zero joins means tracing is off, the log uses the GCP format, or the log is from a different Hub.

### 5. Join trace lines by request ID yourself

Every attempt in the report carries its `requestId`, so you can look up the raw line for any attempt:

```sh
id=$(jq -r '.scenarios[] | select(.name == "project-agents-list") | .attempts[0].requestId' /tmp/scion-bench/api-25.json)
jq -c --arg id "$id" 'select(.msg == "perf_trace" and .request_id == $id)' /tmp/scion-bench/hub25.log
```

### 6. Run the browser benchmark

The browser benchmark is `web/e2e-perf/large-project-bench.mjs`. It needs the built web assets (`cd web && npm install && npx playwright install chromium && npm run build`) and a Hub restarted with the web server and test login turned on. With `--enable-web`, the Hub API is served on `--web-port`:

```sh
cd /tmp/scion-bench
env -i HOME=/tmp/scion-bench/home PATH="$PATH" SCION_CLI_MODE=human \
  GCE_METADATA_HOST=127.0.0.1:1 GCE_METADATA_IP=127.0.0.1:1 \
  SCION_SERVER_HUB_PERFTRACE=true \
  /tmp/scion-bin server start \
  --hosted --enable-hub --enable-web --enable-test-login \
  --db /tmp/scion-bench/hub25.db --session-secret bench-secret-1 \
  --port 19810 --web-port 18080 --host 127.0.0.1 \
  --web-assets-dir /path/to/scion/web/dist/client \
  --foreground --no-auto-migrate \
  > /tmp/scion-bench/hub25-web.log 2>&1
```

```sh
cd web
node e2e-perf/large-project-bench.mjs \
  --hub http://127.0.0.1:18080 \
  --seed /tmp/scion-bench/seed-25.json \
  --out /tmp/scion-bench/browser-25.json \
  --runs 5
```

It runs the `project-grid`, `project-list`, `project-graph-embedded` and `standalone-graph` scenarios, then an SSE burst-update scenario. The project grid and list are paged, so those two scenarios time the first page rendered (one page of cards or rows, with the page size read from the pager) and then the latency of a few Next-page clicks; the report records the page size, page count and page-change times. The burst scenario picks its agents from the grid's first page. Other options are `--page-changes` (default `3`), `--burst-runs`, `--burst-count` (default `15`), `--burst-only`, `--nav-timeout-ms` and `--populate-timeout-ms` (default `120000` each), `--settle-timeout-ms` (default `30000`) and `--notes`. With tracing on, the same Hub log holds the `perf_trace` lines for the page's API requests and the `sse.events` connect and close lines. The README explains how each run is classified and what each reported statistic means.

### Reading the API report

The report types are in `perf/bench/internal/benchout/schema.go`. The fields you will use most:

- `scenarios[]`: one entry per scenario, with `name`, `endpoint`, `agentCount`, `successCount` and `failureCount`. `medianTotalMs`, `medianTtfbMs`, `minTotalMs`, `maxTotalMs` and `stddevTotalMs` are computed over successful attempts only.
- `scenarios[].attempts[]`: every attempt, including failures (`status` is `0` when no response arrived, and `error` says why), with `bytes`, `ttfbMs`, `totalMs`, `requestId` and, when trace data was found, `perfTrace` and `perfTraceSource`.
- `perfTrace`: a map keyed by the `X-Scion-Perf-*` header names. `perfTraceSource` is `headers` (admin caller) or `hub-log` (joined with `--hub-perf-log`).
- `scenarios[].perfTraceAvailable`: true only if at least one attempt has trace data. It stays false when tracing is off or nothing matched; it is never left out.
- `effectiveSettings`, `harnessCommit`, `harnessCommitDirty`, `harnessCommitSource` and `hubScionVersion`: record how the report was produced. `machine` has the host, CPU count, load averages at the start and end of the run, and your `notes`.

Seed tokens and the session secret are replaced with `REDACTED` in the report before it is written.

To print the store-call and decision counts for each attempt:

```sh
jq -r '.scenarios[] | .name as $s | .attempts[] |
  [$s, .perfTrace["X-Scion-Perf-Store-Calls"], .perfTrace["X-Scion-Perf-Decisions"]] | @tsv' \
  /tmp/scion-bench/api-25.json
```

## Using counts as regression budgets

Durations depend on the machine, its load and the database. On a shared host, repeated runs can differ by more than 2x, so a wall-clock budget needs a quiet, dedicated runner and a baseline of many repeated runs.

**The exact counts do not depend on the host.** For a fixed seed (`--agents` and `--rand-seed`), caller, endpoint and code version, these numbers are the same on every attempt after the first request to a freshly started Hub, and on every machine (`apibench`'s default `--warmup 1` covers the first request):

- authorization store calls, in total (`authz_store_calls`) and per method (`store_<Method>_n`)
- decisions (`audit_records`) and the allow and deny split
- phase counts (`phase_<name>_n`)

That makes them usable as regression budgets on any CI runner:

1. **Record a baseline.** Seed at a fixed agent count and run `apibench` with `--want-perf-trace --hub-perf-log`. Check that every attempt of a scenario reports the same counts (after the first request to a freshly started Hub, which the default `--warmup 1` covers). If they differ, something other than the code changed between attempts.
2. **Set the budget.** Use the baseline counts as an upper bound per scenario and size. Because counts repeat exactly, you need no margin for noise; any increase is a real change in the request path. Budgeting at more than one size (for example 25 and 100 agents) also catches a change in how the count grows per agent, which a single size can hide.
3. **Fail on an increase, update on a decrease.** If a change raises a count, investigate before raising the budget. If a change lowers a count on purpose, lower the budget in the same change so the improvement is held.
4. **Inside Go tests**, `pkg/hub` has a `perfTracedRequest` test helper. It serves a request on a test server with tracing on and returns a snapshot whose `AuthzStoreCalls`, `StoreCalls`, `AuditRecords` and per-phase `Count` values are the same host-independent counts. A unit test can assert budgets on them without starting a real Hub.

Keep the limits in mind. Only request-path reads are counted (see the reference's [Counted scope](/scion/reference/server-config/#request-performance-tracing)), and `audit_records` equals the decision count only while every decision emits one audit record, which is the default. As an example of scale, the run shown above made 88 authorization store calls and 255 decisions for one 25-agent project list. Those numbers come from one machine (SQLite, member caller) at one commit and are an illustration only, not targets.

## Readiness marks

The web client can write [User Timing](https://developer.mozilla.org/en-US/docs/Web/API/Performance/mark) marks that time when a page became usable, measured in the browser rather than from the outside. They are **off by default** and are controlled by the hub's `profiling.readiness_marks` operational setting (see the [setting reference](/scion/reference/admin-settings/#profiling)). Off, the client makes no `performance.mark` calls and the page shell is unchanged.

### Turning them on

The setting lives in the Hub database and changes without a restart. As a hub administrator, on a signed-in browser session (user access tokens are refused), send:

```
PUT /api/v1/admin/profiling
{"readiness_marks": true}
```

Then load the page again: the client reads the setting once per full page load, from the shell's initial data (`readinessMarks`, present only for a signed-in user while the setting is on). `GET /api/v1/profiling` returns the same value to any signed-in caller. Send `{"readiness_marks": false}` to turn them off again.

### The marks

| Name | Written when | Pages |
| :--- | :--- | :--- |
| `scion:ready:agents-data` | the first agent result for the current scope is adopted | project detail (grid, list, graph), standalone graph |
| `scion:ready:rows-grid` | the first frame that shows the grid's first page of cards, or its empty state | project detail, grid view |
| `scion:ready:rows-list` | the first frame that shows the list's first page of rows, or its empty state | project detail, list view |
| `scion:ready:graph` | the graph is laid out and fitted to the viewport | project detail graph view, standalone graph |

- A mark's `startTime` is milliseconds since the document's navigation start. Its `detail` is only `{"view": "grid" | "list" | "graph"}`: marks carry no agent names, ids or row data.
- Each mark is written **at most once per load**. A load ends on a client-side navigation, on a change of the view scope (for example opening another project) and on a project filter change on the standalone graph; the marks are then cleared so the next load writes them again. Switching between grid, list and graph inside one load does not clear them.
- The graph mark needs at least one agent: an empty graph view shows the empty state instead of a graph.

Read them in the browser console, or from a Playwright script, with:

```js
performance.getEntriesByType('mark').filter((m) => m.name.startsWith('scion:ready:'))
```

### In the browser benchmark

`web/e2e-perf/large-project-bench.mjs` reads the marks of every populated run, before any interaction or page change. Each run records `readinessMarks` (name to ms), and each scenario reports `readinessMarks` with the count and median/min/max per mark, split cold and warm.

- With `--expect-readiness-marks` (the setting is on), each run waits up to 3 seconds for its scenario's two marks: `agents-data` plus `rows-grid`, `rows-list` or `graph`. `readinessMarksMissingRunCount` counts populated runs that still lack one.
- Without the flag, the setting is expected off. `readinessMarksUnexpectedRunCount` counts populated runs that found any readiness mark. Run once without the flag against a hub with the setting off to check that off writes nothing.

The bench signs in as the seeded member, which is enough to receive the setting.
