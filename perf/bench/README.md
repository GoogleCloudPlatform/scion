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
telemetry credentials, mint tokens for a real GCP service account, or write
into your real `~/.scion`.

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
  roots), long tasks, graph pan/zoom/hover cost, and SSE burst-update settle
  time for the project grid/list/embedded-graph views and the standalone
  graph page.
- `web/e2e-perf/lib.mjs` -- the pure, Playwright-free functions from the
  above (network-outcome classification, stats, burst-target selection),
  split out so they have real unit tests: `web/e2e-perf/lib.test.mjs`, run
  with `npm run test:e2e-perf` (from `web/`).

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
`SCION_OTEL_*` (which made it hold a live connection to a local telemetry
forwarder and log an RPC error once a second), the ambient
`SCION_HUB_ENDPOINT`/`SCION_HUB_URL` pointing at the *live* hub (never
contacted in practice, but there is no reason for a throwaway benchmark
process to hold it in its environment at all), and it wrote into the real
`~/.scion/{hub-id,storage,attachments,harness-configs}` shared with every
other agent on this host.

`env -i` alone is **not sufficient** (bench-rev-2 R3): it clears process
environment variables, but Application Default Credentials are discovered
via the GCE metadata server (`169.254.169.254`), not the environment, so a
hub launched with only `env -i` still logs `GCP token generator configured`
/ `GCP service account minting configured` / `Policy Troubleshooter checker
configured` and holds live connections to the metadata server and Google
APIs -- confirmed via `ss -tnp` during bench-rev-2's review. A throwaway
bench hub must not be able to mint tokens for a real service account.

Use a scrubbed launch for every hub subprocess this harness starts, run
from **outside** any project/repo directory (e.g. `cd /tmp/scion-bench`
first -- running from inside a git checkout makes the hub warn "Server is
running from a project directory context" and use that project's
templates/settings instead of its own throwaway ones):

```sh
mkdir -p /tmp/scion-bench/home

env -i HOME=/tmp/scion-bench/home PATH="$PATH" SCION_CLI_MODE=human \
  GCE_METADATA_HOST=127.0.0.1:1 GCE_METADATA_IP=127.0.0.1:1 \
  /tmp/scion-bin server start \
  --hosted --enable-hub \
  --db /tmp/scion-bench/hub25.db \
  --session-secret bench-secret-1 \
  --port 19810 --host 127.0.0.1 \
  --foreground --no-auto-migrate
```

`GCE_METADATA_HOST`/`GCE_METADATA_IP` point the Go GCP metadata client
(`cloud.google.com/go/compute/metadata`, which both the ADC resolver and the
hub's own GCP-identity features use) at a loopback address nothing listens
on, so metadata-server lookups fail immediately instead of succeeding
against the real instance metadata service.

**Verify isolation worked**, every time, before trusting a capture:

1. Check the hub's own log for the storage-backend line -- it should say
   `/tmp/scion-bench/home/.scion/storage`, not your real home directory.
2. Confirm neither of these appears anywhere in the hub's log: the real
   service-account email (`scion-my-grove@...` or similar), or the real GCP
   project ID (`deploy-demo-test` or similar). The GCP-subsystem log lines
   themselves (`GCP token generator configured`, `Policy Troubleshooter: no
   GCP project ID available`, ...) still appear with
   `GCE_METADATA_HOST`/`GCE_METADATA_IP` set -- that is expected, since the
   hub still attempts and logs the lookup -- but their content must say
   `unknown - not running on GCE` / `connection refused` / similar, not a
   real identity. Confirm no `Cloud Logging query service initialized` or
   `rpc error: code = Unimplemented` lines appear either (those are
   `SCION_TELEMETRY_*` side effects, unrelated to GCP identity, and should
   not be reachable at all once the environment is scrubbed).
3. While the hub is running, `lsof -p <hub-pid> -i` and confirm it prints
   **no rows at all** for that PID (a healthy isolated hub holds its own
   listening socket, visible via plain `lsof -p <hub-pid>` without `-i`, but
   zero outbound/established network connections). Do not rely on
   `ss -tnp` for this without root: in a shared network namespace (as in
   this container) it lists every process's sockets, and without root it
   cannot attach the `pid=` column to tell them apart -- a connection
   `ss -tn` shows does not mean the hub holds it.

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

**The seed tool refuses to run against an existing, non-empty `--db`**:
re-seeding an already-seeded file is not supported (it fails partway
through, after already mutating hub bootstrap state, with an "already
exists" error on the owner/member user). The check normalizes a `file:`-DSN
or `?query`-suffixed `--db` value before looking at the filesystem
(bench-rev-2 R4), so there is no DSN-form bypass. There is no override
flag -- use a fresh path or remove the file first.

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

At 500 agents, some fraction of `project-agents-list`/`project-list`/
`project-graph-embedded` requests take long enough to cross this ceiling
(see `measurements.md` for how often, which varies run to run with host
load -- bench-rev-2's review found the api-side client can occasionally
still receive a slow-but-complete response past 60s, so this is a real but
not deterministic cliff; see measurements.md's N9 discussion). Above it,
the client gets a connection reset with no body, `project-detail.ts`'s
`loadData()` `Promise.all` rejects, and the page falls to its error/empty
state -- which renders almost no agent cards. **A naive "did the expected
element count ever appear" check cannot tell that apart from "still
rendering a huge DOM"**; `large-project-bench.mjs` watches the actual
network outcome of the load-bearing API request (`page.on('response'
/'requestfailed')`) to classify each run as `populated` /
`load-failed(<reason>)` / `loaded-not-rendered` / `still-loading` instead.
See `measurements.md`'s baseline section for what this looked like at 500
agents on unmodified main, and bench-rev-1/bench-rev-2's reviews for how
this was found (raw evidence at `gs://scion-xproject-exchange/slow-list/
raw/bench-rev-1/` and `.../raw/bench-rev-2/`).

## API benchmark

```sh
/tmp/apibench-bin \
  --hub http://127.0.0.1:19810 \
  --seed /tmp/scion-bench/seed-25.json \
  --runs 5 --warmup 1 \
  --out /tmp/scion-bench/api-25.json \
  --notes "machine/CPU conditions, e.g. shared broker container"
```

Runs three scenarios against the seeded project, authenticated as the
seeded member:

- `project-agents-list`: `GET /api/v1/projects/{id}/agents` -- the project
  grid/list page's request.
- `global-agents-list-unscoped`: `GET /api/v1/agents`, no query string --
  what the standalone graph page actually fetches
  (`web/src/components/pages/agent-graph.ts:115`). An earlier version of
  this tool instead measured the `projectId=`-scoped variant below and
  mislabeled it as "what the standalone graph page loads", which it is not
  (bench-rev-2 R1, related/non-blocking).
- `global-agents-list-scoped`: `GET /api/v1/agents?projectId={id}` -- not
  fetched by any page today; kept as a reference point for how much a
  server-side project filter would save over the unscoped fetch above.

A request attempt is recorded whether it succeeds or not: a client timeout,
a connection error, and a non-2xx status are all failures, kept in the
report's `attempts` list, and excluded from the median/min/max/stddev
(computed over successful attempts only, per `successCount`/`failureCount`).
A single failed attempt no longer aborts the whole run and discards every
other sample. Raise `--timeout-seconds` (default 60) for large agent
counts -- some 500-agent requests exceed 60s (see "A real product finding"
above; how often varies with host load, it is not "regularly" on every run).

`MachineInfo` in the report includes `/proc/loadavg` and `/proc/uptime`,
sampled once at the start and again at the end of the run (bench-rev-2 NB9:
a 500-agent run takes long enough for load to swing within one report) --
see "Choosing regression budgets" below for why this is recorded, and for
what it is not sufficient for. The report also records the harness's own
`git rev-parse HEAD` and the effective `--runs`/`--warmup`/
`--timeout-seconds`/`--want-perf-trace` settings (bench-rev-2 NB4), so a
report file is self-describing without having to match it to a commit by
timestamp.

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
  GCE_METADATA_HOST=127.0.0.1:1 GCE_METADATA_IP=127.0.0.1:1 \
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
(`/agents/graph?project={id}`) -- each `--runs` times: **run 0 uses a
fresh ("cold") browser context; runs 1..N-1 share one ("warm") context**
(bench-rev-2 NB3), reported separately
(`medianNavToPopulatedMsCold`/`...Warm`) as well as combined, since a cold
run is measurably slower and averaging it in without saying so is
misleading.

Every run is classified `populated` / `load-failed(<reason>)` /
`loaded-not-rendered` / `still-loading` (see "A real product finding"
above; `loaded-not-rendered` -- bench-rev-2 NB6 -- is a 2xx response that
still never reaches the expected rendered count, a render failure distinct
from "nothing observed at all"). Only `populated` runs contribute to the
reported median/min/max/stddev for navigation time, DOM element count
(recursively counted through every open shadow root, since this Lit app
keeps almost all of its structure there), and long-task totals from a
`PerformanceObserver` injected before navigation. `successCount`/
`failureCount` and a per-outcome tally are reported alongside, so "5/5
populated" and "1/5 populated, 4 load-failed" are never conflated into the
same median. Each run also records when the load-bearing request's network
outcome was observed, in elapsed ms from navigation start
(`networkObservedAtMs`, bench-rev-2 NB5), so a WriteTimeout attribution is a
measurement, not an inference from a run's total wall-clock time. The
report is written incrementally (after every scenario and every burst run),
so a Chromium crash mid-benchmark loses at most the in-flight run, not the
whole report.

For the two graph scenarios, a populated run also performs a short
pan/zoom/hover interaction sequence (hover over up to 5 nodes, wheel-zoom
in and out, drag-pan) and reports the long-task cost specifically
attributable to it (`graphInteraction`) -- this was previously listed as a
#2393 acceptance gap "not attempted here for lack of time"; no source
change was needed, so it is implemented now.

It then runs the SSE burst-update scenario `--burst-runs` times (default:
same as `--runs`), all within one browser context. Each run: picks up to
`--burst-count` (default 15) agents that are **not** currently `suspended`
(see below), fires real `phase` updates via the REST API as the seeded
owner while checking every POST's status, polls each *accepted* agent's
own rendered `<scion-status-badge>` until it shows the new value -- ground
truth for "the live update reached the DOM" -- then **restores every
updated agent to its pre-burst phase AND activity** (bench-rev-2 NB1) and
**waits for the restore to be confirmed in the DOM** before the next run
starts (bench-rev-2 R2: an earlier version fired the restore and moved on
without waiting, so a slow-to-render restore from run *i* could be
miscounted as run *i+1*'s own settle -- see `pickBurstTarget`'s doc comment
in `lib.mjs` for the full mechanism). Settle time is measured **per agent,
from that agent's own POST completion** (bench-rev-2 NB2: an earlier
version measured from a shared burst-start timestamp, which folded
roughly half of each reported "SSE settle time" into what was actually POST
latency). Server-side application is confirmed independently via a
follow-up GET per agent, so "the server never applied this update" and "the
UI did not settle" are reported as distinct, non-overlapping counts.

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
  every POST's status, and restores state (including waiting for the
  restore -- see above) afterward.
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
container (`scion-community-broker-01`) is a shared host with 16 CPUs and a
load average observed to swing from roughly 47 to 450 depending on what
else is running. Repeated apibench reruns during review, under otherwise
identical isolated conditions, varied by more than 2x run to run purely
from this -- host-load noise, not anything this harness's own changes
caused (an earlier draft of `measurements.md` incorrectly credited
environment isolation for a faster recapture; see that file's "What
changed and why" for the correction). Environment noise on this scale would
swamp any regression budget chosen from data captured here.

This data is also heavy-tailed (occasional samples several multiples of the
typical value), so a budget built from mean + k*stddev over a small number
of runs would be dominated by single outliers. Prefer a median-ratio gate
(e.g. fail if the median regresses more than X% over at least 10 runs),
with MAD or IQR for spread, over a stddev-based one.

The strongest near-term option does not need a quiet runner at all:
**host-independent counters can be budgeted and CI-gated today, on any
host.** Authorization store-call and decision counts (from #2392's perf
trace), response bytes, DOM element count, and long-task count at a fixed
agent count are all independent of machine speed. Only wall-clock budgets
need a quiet, dedicated runner with a repeated (>=10 run) baseline.
`MachineInfo.loadAvg1/5/15` (now sampled at both the start and end of an
apibench run) are recorded so a reader can sanity-check whether a given
historical run's numbers are trustworthy for wall-clock comparisons, not to
derive budgets from directly.

## #2393 acceptance-criteria gaps

Tracked here rather than silently dropped:

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
- **No regression budgets, no CI wiring** -- see "Choosing regression
  budgets" above; nothing is claimed here, so there is nothing to verify,
  but wiring real budgets into CI is real follow-up work once a
  quiet-runner baseline exists (or, for the host-independent counters
  above, no quiet runner is even needed).
- **Graph membership across cursor pages** -- not covered: this harness's
  seeded agent counts (25/100/500) are all served in a single page at the
  default `limit=500`, so cursor pagination through `/api/v1/agents` is
  never exercised. Separately, `agent-graph.ts:115` makes exactly one fetch
  and never follows a cursor at all today -- above 500 agents, the
  standalone graph is silently truncated in production, independent of
  this harness. That is a product correctness gap for #2372, not just a
  missing test.
- **List sort/filter correctness** -- not covered: this harness only
  measures default-order list/grid rendering, not sort/filter parameter
  correctness. That is more naturally a correctness test than a
  performance benchmark; flagging for a separate test, not blocking this
  harness.

Graph interaction timing (pan/zoom/hover) and warm/cold trial distinction
were also previously listed here as gaps "for lack of time"; both are
implemented now (see "Browser benchmark" above) since neither needed a
source change or genuine infrastructure -- they were follow-up work, not
constraints.

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
- All measurements in this repo's `measurements.md` were taken on a shared
  host (`scion-community-broker-01`, 16 CPUs, widely variable load -- not a
  single CPU), with (for the 100/500-agent cases) up to three hub
  subprocesses co-resident. Treat absolute numbers as this-machine,
  this-run numbers; treat the *shape* (order-of-magnitude growth from 25 to
  100 to 500 agents, and which view/scenario is slowest) as the portable
  finding -- and see "Choosing regression budgets" above before using these
  numbers to gate anything.
