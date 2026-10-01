# Large-project performance harness (ptone/scion#2393, #2374, #2367)

Reproducible harness for measuring agent-list/graph performance at 25, 100,
and 500 agents in a single project: a local SQLite hub, seeded directly
(bypassing HTTP) with realistic data, and Go/Playwright benchmark tools that
separate API time, browser rendering, and live-update (SSE) responsiveness,
per the root tracker's completion criteria.

This harness makes **no changes to hub or web source**. It is pure tooling,
so it can (and must, per the task ordering) capture a BASELINE against
unmodified `origin/main` before any of #2367's other workstreams land.

Do **not** point any of this at the live hub
(`community.projects.scion-ai.dev`). Everything here runs against a hub
subprocess you start locally against a throwaway SQLite file, in an
environment isolated from your own agent/shell (see "Isolate the hub
environment" below) -- the hub must not inherit your ambient cloud
telemetry credentials or write into your real `~/.scion`.

## Layout

- `perf/bench/seed/` -- Go CLI. Seeds one project + a non-admin
  project-member + N synthetic agents directly into a hub SQLite database.
- `perf/bench/apibench/` -- Go CLI. Drives repeated timed HTTP requests
  against a running hub, authenticated as the seeded member.
- `perf/bench/internal/benchout/` -- shared JSON schema for the above two.
- `web/e2e-perf/large-project-bench.mjs` -- Playwright-based Node script.
  Lives under `web/` (not here) purely so `@playwright/test` resolves from
  `web/node_modules` without a second `npm install`; see that file's header
  comment. Measures navigation-to-populated time, DOM size (including shadow
  roots), long tasks, and SSE burst-update settle time for the project
  grid/list/embedded-graph views and the standalone graph page.

## One-time setup

```sh
# From the repo root.
go build -buildvcs=false -o /tmp/scion-bin ./cmd/scion/
go build -buildvcs=false -o /tmp/seed-bin ./perf/bench/seed/
go build -buildvcs=false -o /tmp/apibench-bin ./perf/bench/apibench/

cd web
npm install
npx playwright install chromium   # add --with-deps only if you have root/sudo
npm run build                     # produces web/dist/client, used via --web-assets-dir
cd ..
```

`scion server start` (the local test-hub subprocess this harness drives) is
removed from the CLI's command tree in `SCION_CLI_MODE=agent` (see
`cmd/cli_mode.go`) -- the mode this container's ambient `scion` CLI runs in
for orchestration. That restriction is a client-side command-tree filter on
the *compiled binary*, keyed off an env var, and has nothing to do with hub
authorization; it exists to stop an agent from accidentally starting a rogue
hub server while doing unrelated work. Override it only for the private
`/tmp/scion-bin` invocations this harness makes -- see the isolated-launch
wrapper below, which sets it alongside everything else.

Do **not** unset `SCION_HUB_ENDPOINT`/`SCION_HUB_URL` in your *own* shell to
work around a separate "no Hub endpoint configured" guard in `cmd/root.go`
-- as long as those still point at a real (non-localhost) hub in the shell
you run commands from, that guard passes. The isolated launch below clears
them (along with everything else) only in the *hub subprocess's own*
environment, which is a different thing and is required -- see next section.

## Isolate the hub environment (required)

Following earlier versions of this doc verbatim, the bench hub subprocess
inherited the agent container's full environment: `SCION_TELEMETRY_*` /
`SCION_OTEL_*` (which made it hold live connections to Google Cloud Trace
and a local telemetry forwarder, and log an RPC error once a second -- real
overhead inside the numbers being measured), the ambient GCP quota project,
and `SCION_HUB_ENDPOINT`/`SCION_HUB_URL` pointing at the *live* hub (never
contacted in practice, but there is no reason for a throwaway benchmark
process to hold it in its environment at all). It also wrote into the real
`~/.scion/{hub-id,storage,attachments,harness-configs}` shared with every
other agent on this host, and warned about running from a project directory
context (`/workspace` is one).

Use a scrubbed launch for every hub subprocess this harness starts:

```sh
mkdir -p /tmp/scion-bench/home

env -i HOME=/tmp/scion-bench/home PATH="$PATH" SCION_CLI_MODE=human \
  /tmp/scion-bin server start \
  --hosted --enable-hub \
  --db /tmp/scion-bench/hub25.db \
  --session-secret bench-secret-1 \
  --port 19810 --host 127.0.0.1 \
  --foreground --no-auto-migrate
```

run from **outside** any project/repo directory (e.g. `cd /tmp/scion-bench`
first) -- run it from inside a git checkout and the hub warns "Server is
running from a project directory context" and uses that project's
templates/settings instead of its own throwaway ones.

`env -i` clears the *entire* environment and re-adds only `PATH` (needed to
find binaries the hub shells out to) and the two variables the hub itself
needs. `HOME=/tmp/scion-bench/home` gives it a private, empty
`~/.scion`-equivalent instead of writing into yours. Verify isolation
worked by checking the hub's own log for the storage-backend line -- it
should say `/tmp/scion-bench/home/.scion/storage`, not your real home
directory -- and confirm no `Cloud Logging query service initialized` /
`rpc error: code = Unimplemented` lines appear (both are `SCION_TELEMETRY_*`
side effects that should no longer be reachable).

## Seed a project

```sh
mkdir -p /tmp/scion-bench

/tmp/seed-bin \
  --db /tmp/scion-bench/hub25.db \
  --session-secret bench-secret-1 \
  --agents 25 \
  --project-slug bench-25 \
  --out /tmp/scion-bench/seed-25.json

# Repeat with --agents 100 / --agents 500 and distinct --db / --project-slug
# / --out paths. One DB per agent count keeps hub restarts independent; you
# do not need three separate --session-secret values (reusing one is fine).
```

This creates: a project-owner user, a non-admin project-member user (bound
via `project-member` role, the principal every benchmark authenticates as),
and N agents with a phase/activity mix weighted toward running/stopped/error,
an ancestry mix (none / single-level / multi-level chains against real prior
agent IDs), and an appliedConfig size split (~90% small, ~10% with an
inlined multi-KB pre-start hook script, reproducing the ~62%-of-bytes
appliedConfig skew the original investigation measured). The written JSON
records the actual realized distribution counts (seeding is randomized but
seeded via `--rand-seed`, default 42, so reruns are reproducible) plus the
owner/member bearer tokens and project ID, which every other tool reads.

**The seed tool refuses to run against an existing, non-empty `--db`** by
default: re-seeding an already-seeded file is not supported (it fails
partway through, after already mutating hub bootstrap state, with an
"already exists" error on the owner/member user). Use a fresh path, remove
the file first, or pass `--force-existing` if you specifically understand
and accept that.

See `perf/bench/seed/synthetic.go` for the exact distributions and the
rationale comments next to each one.

## Start the hub under test

Use the isolated launch from above, adding whichever components the tool
you're about to run needs:

- **API benchmark only**: `--hosted --enable-hub` is enough.
- **Browser benchmark**: also add `--enable-web --enable-test-login
  --web-port <port> --web-assets-dir web/dist/client` (a path relative to
  the repo root, or absolute if you invoke the hub from elsewhere) -- when
  `--enable-web` is set, the Hub API is served on `--web-port`, not `--port`.
  `--enable-test-login` lets the browser script sign in as the seeded
  member/owner without a password; never pass it when pointing at anything
  other than a disposable local hub.

Notes on flags, learned the hard way while building this:

- `--hosted --enable-hub` (not bare `--enable-hub`): workstation mode (the
  default) enables Hub+Broker+Web regardless of which `--enable-*` flags you
  add, and the runtime broker refuses to start without `--storage-bucket`/
  `image_registry` configured. `--hosted` first, then opt into exactly the
  components you want, avoids that entirely for a hub-only API benchmark.
- `--session-secret` must match what you passed to `perf/bench/seed`: both
  derive the same JWT signing key deterministically from it
  (`deriveSharedSigningKey` in `pkg/hub/server.go`), which is how the
  seed-minted bearer tokens validate against a hub process that never saw
  the seeding happen.
- Run one hub process per agent-count/DB, on distinct ports, if you want to
  benchmark 25/100/500 without restarting between them.

## A real product finding: the hub's 60s WriteTimeout

Both the standalone Hub API server and the combined hub+web server cap how
long a handler may run before its connection is forcibly closed with an
empty reply to the client:

- `pkg/config/hub_config.go:851` -- `HubServerConfig.WriteTimeout`, default
  `60 * time.Second`, wired into the standalone Hub API's `http.Server` at
  `pkg/hub/server.go:4518-4519`.
- `pkg/hub/web.go:2913` -- hard-coded `WriteTimeout: 60 * time.Second` on
  the combined hub+web `http.Server` (`WebServer.Start`).

At 500 agents, `project-agents-list`'s API median is already close to this
ceiling and its tail exceeds it (see `measurements.md`). Above it, the
client gets a connection reset with no body, `project-detail.ts`'s
`loadData()` `Promise.all` rejects, and the page falls to its error/empty
state -- which renders almost no agent cards. **A naive "did the expected
element count ever appear" check cannot tell that apart from "still
rendering a huge DOM"**; `large-project-bench.mjs` watches the actual
network outcome of the load-bearing API request (`page.on('response'
/'requestfailed')`) to classify each run as `populated` /
`load-failed(<reason>)` / `still-loading` instead. See `measurements.md`'s
baseline section for what this looked like at 500 agents on unmodified
main, and bench-rev-1's review for how this was found (raw evidence at
`gs://scion-xproject-exchange/slow-list/raw/bench-rev-1/`).

## API benchmark

```sh
/tmp/apibench-bin \
  --hub http://127.0.0.1:19810 \
  --seed /tmp/scion-bench/seed-25.json \
  --runs 5 --warmup 1 \
  --out /tmp/scion-bench/api-25.json \
  --notes "machine/CPU conditions, e.g. shared broker container"
```

Runs two scenarios against the seeded project, authenticated as the seeded
member: `project-agents-list` (`GET /api/v1/projects/{id}/agents`, the
project grid/list page's request) and `global-agents-list` (`GET
/api/v1/agents?projectId={id}`, what the standalone graph page fetches).

A request attempt is recorded whether it succeeds or not: a client timeout,
a connection error, and a non-2xx status are all failures, kept in the
report's `attempts` list, and excluded from the median/min/max/stddev
(computed over successful attempts only, per `successCount`/`failureCount`).
A single failed attempt no longer aborts the whole run and discards every
other sample. Raise `--timeout-seconds` (default 60) for large agent counts
-- the unmodified-`main` baseline at 500 agents regularly exceeds 60s per
request (see "A real product finding" above).

`MachineInfo` in the report includes `/proc/loadavg` and `/proc/uptime`
automatically (Linux only) -- see "Choosing regression budgets" below for
why, and for what this is not sufficient for.

When run against a hub built from the `perf/2392-agent-list-instrumentation`
branch (not yet merged) with `SCION_HUB_PERF_TRACE=1` set in the hub's
environment, add `--want-perf-trace` to additionally capture phase-timing/
store-call-count data from the response headers. On a baseline run against
unmodified `main`, or if the flag was passed but no trace headers actually
came back, the report's `perfTraceAvailable` field is `false` for that
scenario, not silently omitted or wrongly true.

The report's embedded seed metadata has its bearer tokens and session
secret redacted before it is ever written to disk (`REDACTED` in place of
each) -- there is no manual redaction step to remember before uploading a
report.

## Browser benchmark

Requires the hub from the previous step restarted with web + test-login
(see "Isolate the hub environment" and "Start the hub under test" above):

```sh
env -i HOME=/tmp/scion-bench/home PATH="$PATH" SCION_CLI_MODE=human \
  /tmp/scion-bin server start \
  --hosted --enable-hub --enable-web --enable-test-login \
  --db /tmp/scion-bench/hub25.db --session-secret bench-secret-1 \
  --port 19810 --web-port 18080 --host 127.0.0.1 \
  --web-assets-dir web/dist/client \
  --foreground --no-auto-migrate
```

```sh
cd web
node e2e-perf/large-project-bench.mjs \
  --hub http://127.0.0.1:18080 \
  --seed /tmp/scion-bench/seed-25.json \
  --out /tmp/scion-bench/browser-25.json \
  --runs 5
```

Runs four scenarios -- `project-grid`, `project-list`,
`project-graph-embedded` (all three via `/projects/{id}`, matching the
original investigation's view-toggle table), and `standalone-graph`
(`/agents/graph?project={id}`) -- each `--runs` times. Every run is
classified `populated` / `load-failed(<reason>)` / `still-loading` (see "A
real product finding" above); only `populated` runs contribute to the
reported median/min/max/stddev for navigation time, DOM element count
(recursively counted through every open shadow root, since this Lit app
keeps almost all of its structure there), and long-task totals from a
`PerformanceObserver` injected before navigation. `successCount`/
`failureCount` and a per-outcome tally are reported alongside, so "5/5
populated" and "1/5 populated, 4 load-failed" are never conflated into the
same median. The report is written incrementally (after every scenario and
every burst run), so a Chromium crash mid-benchmark loses at most the
in-flight run, not the whole report.

It then runs the SSE burst-update scenario `--burst-runs` times (default:
same as `--runs`). Each run: loads the grid once (skipping the burst
entirely, with a recorded reason, if the grid itself did not populate),
picks up to `--burst-count` (default 15) agents that are **not** currently
`suspended` (see below), fires real `phase` updates via the REST API as the
seeded owner while checking every POST's status, polls each *accepted*
agent's own rendered `<scion-status-badge>` until it shows the new
value -- ground truth for "the live update reached the DOM" -- and then
**restores every updated agent to its pre-burst phase** before the next
run. Server-side application is confirmed independently via a follow-up GET
per agent, so "the server never applied this update" and "the UI did not
settle" are reported as distinct, non-overlapping counts.

Raise `--populate-timeout-ms`/`--nav-timeout-ms` (default 120000/120000) for
large agent counts; at 500 agents on unmodified `main`, some views exceed
even that (see "A real product finding" above and `measurements.md`'s
baseline section).

**Known limitations, so a future reader doesn't have to rediscover them:**

- The project-detail URL must carry the project **UUID**, not slug:
  `GET /api/v1/projects/{id}` (`pkg/hub/handlers_projects_core.go`'s
  `getProject`) does a raw-ID store lookup with no slug resolution, unlike
  the agent-list endpoints' `projectId` query parameter.
- `store.AgentStatusUpdate` (`pkg/store/store.go`) has no top-level `status`
  field, only `phase`/`activity`/etc. `web/test-scripts/
  realtime-lifecycle-test.js`'s `{"status": "running"}` body decodes
  successfully (unknown JSON keys are ignored) but changes nothing -- easy to
  mistake for "the live update never arrived." Use `phase`.
- `updateAgentStatus`'s Guard 0
  (`pkg/hub/handlers_agent_lifecycle.go`) silently drops any phase/activity
  update sent to an agent that is *currently* `suspended`, and still
  returns 200. An earlier version of this script both targeted `suspended`
  as a burst destination and never checked POST status, which made about 4
  of every 15 targeted agents permanently un-updatable after the first run
  against a given database -- what looked like "SSE settle flakiness"
  (originally reported as 6/15 and 12/15 in `measurements.md`'s superseded
  numbers) was entirely this, not SSE. The burst scenario now skips
  currently-suspended agents as sources, never targets `suspended`, checks
  every POST's status, and restores state afterward.
- The burst scenario also deliberately never targets phase `"running"`:
  `getAgentDisplayStatus` (`web/src/shared/types.ts`) renders a running
  agent's *activity* instead of the literal phase whenever activity is
  non-empty, which makes "did it reach running" ambiguous from the outside
  without also pinning activity.
- A plain `MutationObserver` on `document.body` (even with `subtree: true`)
  does **not** see mutations inside shadow roots -- it does not cross shadow
  boundaries at all. An earlier version of this script tried exactly that as
  a secondary "DOM mutation count" signal for the burst scenario and it
  always read zero, even for updates independently confirmed to have
  rendered. The badge-polling approach above is ground truth instead.
- This container's `/dev/shm` is small enough that Chromium can crash
  (`page.evaluate: Target crashed`) without `--disable-dev-shm-usage`,
  which is now always passed.
- `--enable-test-login` must never be passed when benchmarking anything
  other than a disposable local hub: it lets any caller mint a session for
  any email/role with no password.

## Choosing regression budgets

Not implemented by this harness, and deliberately not guessed at: this
container (`scion-community-broker-01`) is a shared, variably-loaded host.
`apibench`'s reruns during bench-rev-1's review saw medians roughly half
the original baseline's under a load average of ~100-450 (16 CPUs) and one
sample over 60s -- environment noise on this scale would swamp any
regression budget chosen from data captured here.

Recommendation for whoever picks budgets: capture a repeated (>=10 run)
baseline on a quiet, dedicated runner (not this container, and not while
anything else is benchmarking), and derive budgets from that run's own
variance (e.g. median + a multiple of its stddev), separately for API time,
browser rendering, and live-update settle time, at each of 25/100/500
agents. `MachineInfo.loadAvg1/5/15` and `uptimeSeconds` (recorded
automatically by `apibench`) are there so a reader can sanity-check whether
a given historical run's numbers are trustworthy for that purpose, not to
derive budgets from directly.

## #2393 acceptance-criteria gaps

Tracked here rather than silently dropped; each either follow-up work or
out of scope for this PR:

- **Graph interaction timing** (pan/zoom/hover render cost) -- not
  implemented. Would need to drive synthetic pointer events against
  `agent-tree-view.ts` and re-run its layout; a reasonable follow-up, not
  attempted here for lack of time.
- **In-app readiness marks** (data arrival / visible rows / graph ready)
  -- infeasible without web source changes: there are currently no such
  marks anywhere in the app (confirmed by grepping for
  `performance.mark`/custom ready events), so exposing them requires
  instrumenting `web/src/components/pages/project-detail.ts` and
  `agent-tree-view.ts` themselves. This harness only measures from the
  outside, per the brief's "no hub or web source changes" constraint.
- **Large-file-list dataset** -- infeasible in this PR: `perf/bench/seed`
  only seeds agents/projects/users, not file-browser data sources. Adding
  realistic file trees is a separate, non-trivial seeding surface.
- **Warm vs cold trial distinction** -- not implemented: all runs in a
  scenario currently share one browser context (and thus HTTP cache).
  Feasible as a follow-up (first run in a fresh context = cold, subsequent
  runs reuse it = warm) but not done here for time.
- **No regression budgets, no CI wiring** -- see "Choosing regression
  budgets" above; nothing is claimed here, so there is nothing to verify,
  but wiring real budgets into CI is real follow-up work once a
  quiet-runner baseline exists.
- **Graph membership across cursor pages** -- not covered: this harness's
  seeded agent counts (25/100/500) are all served in a single page at the
  default `limit=500`, so cursor pagination through `/api/v1/agents` is
  never exercised. Follow-up: seed >500 agents and confirm the graph drains
  every cursor page.
- **List sort/filter correctness** -- not covered: this harness only
  measures default-order list/grid rendering, not sort/filter parameter
  correctness. That is more naturally a correctness test than a
  performance benchmark; flagging for a separate test, not blocking this
  harness.

## Notes on measurement fidelity

- Direct-to-store seeding (bypassing the HTTP agent-create path) does not
  create delegation-edge rows the way a real agent creation does
  (`pkg/hub/handlers_test.go` calls this out for the same reason). This does
  not affect the agent-list/graph endpoints measured here, which do not read
  delegation edges.
- A background scheduler job (`Scheduler: marked stale agents as offline`)
  can flip a handful of seeded agents' phase shortly after hub startup if
  their seeded `LastSeen` is old enough to look stalled. This is expected,
  harmless drift from the seeded distribution recorded in `seed-*.json`, not
  a harness bug -- re-run `perf/bench/seed` for a fresh, undrifted DB if you
  need the exact seeded counts to hold.
- All measurements in this repo's `measurements.md` were taken on a single
  shared/contended CPU inside the `scion-community-broker-01` agent
  container, with (for the 100/500-agent cases) up to three hub subprocesses
  co-resident. Treat absolute numbers as this-machine, this-run numbers;
  treat the *shape* (order-of-magnitude growth from 25 to 100 to 500 agents,
  and which view/scenario is slowest) as the portable finding -- and see
  "Choosing regression budgets" above before using these numbers to gate
  anything.
