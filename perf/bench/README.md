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
subprocess you start locally against a throwaway SQLite file.

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
`/tmp/scion-bin` invocations this harness makes:

```sh
export SCION_CLI_MODE=human   # only for the /tmp/scion-bin invocations below
```

Do **not** unset `SCION_HUB_ENDPOINT`/`SCION_HUB_URL` to work around a
separate "no Hub endpoint configured" guard in `cmd/root.go` -- as long as
those still point at a real (non-localhost) hub, that guard passes and
`/tmp/scion-bin server start` runs fine as a private local process.

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

See `perf/bench/seed/synthetic.go` for the exact distributions and the
rationale comments next to each one.

## Start the hub under test

```sh
SCION_CLI_MODE=human /tmp/scion-bin server start \
  --hosted --enable-hub \
  --db /tmp/scion-bench/hub25.db \
  --session-secret bench-secret-1 \
  --port 19810 --host 127.0.0.1 \
  --foreground --no-auto-migrate
```

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
- For the browser benchmark, also add `--enable-web --enable-test-login
  --web-port <port> --web-assets-dir web/dist/client` (see below); when
  `--enable-web` is set, the Hub API is served on `--web-port`, not `--port`.
- Run one hub process per agent-count/DB, on distinct ports, if you want to
  benchmark 25/100/500 without restarting between them.

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
Reports median/min/max/stddev total latency and TTFB over `--runs` timed
trials (plus `--warmup` untimed ones). Raise `--timeout-seconds` (default
60) for large agent counts -- the unmodified-`main` baseline at 500 agents
regularly exceeds 60s per request.

When run against a hub built from the `perf/2392-agent-list-instrumentation`
branch with `SCION_HUB_PERF_TRACE=1` set in the hub's environment, add
`--want-perf-trace` to additionally capture phase-timing/store-call-count
data from the response (see that branch's own docs). On a baseline run
against unmodified `main` there is no such data, and the report's
`perfTraceAvailable` field is `false` rather than silently omitted.

## Browser benchmark

Requires the hub from the previous step restarted with web + test-login:

```sh
SCION_CLI_MODE=human /tmp/scion-bin server start \
  --hosted --enable-hub --enable-web --enable-test-login \
  --db /tmp/scion-bench/hub25.db --session-secret bench-secret-1 \
  --port 19810 --web-port 18080 --host 127.0.0.1 \
  --web-assets-dir /workspace/web/dist/client \
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
(`/agents/graph?project={id}`) -- each `--runs` times, reporting per-run
navigation-to-populated time, total DOM element count (recursively counted
through every open shadow root, since this Lit app keeps almost all of its
structure there), and long-task count/total/max via a `PerformanceObserver`
injected before navigation. It then runs one SSE burst-update scenario:
loads the grid once, fires a burst of real `phase` updates (default 15
agents) via the REST API as the seeded owner, and polls each updated agent's
own rendered `<scion-status-badge>` until it shows the new value -- ground
truth for "the live update reached the DOM" -- reporting how many of the N
settled and how long the slowest one took.

Raise `--populate-timeout-ms`/`--nav-timeout-ms` (default 120000/120000) for
large agent counts; at 500 agents on unmodified `main`, the project-grid
view alone can exceed 300s (see `measurements.md`'s baseline section -- this
is a real, reproduced symptom, not a harness bug).

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
- The burst scenario deliberately never targets phase `"running"`:
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
- SSE reconnection is not instantaneous after `test-login` (the client logs
  a short exponential-backoff reconnect sequence on first connect in some
  runs). The burst scenario's settle rate varies run to run as a result
  (observed 6/15 to 15/15 settling within the timeout at 25 agents) -- this
  looks like a real, if secondary, live-update-completeness gap worth a
  separate look under #2371/#2385, not a bug in this harness.
- `--enable-test-login` must never be passed when benchmarking anything
  other than a disposable local hub: it lets any caller mint a session for
  any email/role with no password.

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
  and which view/scenario is slowest) as the portable finding.
