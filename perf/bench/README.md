# Large-project performance harness (ptone/scion#2393, #2374, #2367)

Reproducible harness for measuring agent-list/graph performance at 25, 100,
and 500 agents in a single project: a local SQLite hub, seeded directly
(bypassing HTTP) with realistic data, and Go/Playwright benchmark tools that
separate API time, browser rendering, and live-update (SSE) responsiveness.

This harness makes **no changes to hub or web source**. It is pure tooling,
so it captures a BASELINE against unmodified `origin/main` before any of
#2367's other workstreams land.

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
go build -o /tmp/scion-bin ./cmd/scion/
go build -o /tmp/seed-bin ./perf/bench/seed/
go build -o /tmp/apibench-bin ./perf/bench/apibench/

cd web
npm install
npx playwright install chromium   # add --with-deps only if you have root/sudo
npm run build                     # produces web/dist/client, used via --web-assets-dir
cd ..
```

**Do not add `-buildvcs=false` to the `apibench`/`seed` build commands.**
`apibench`'s report records its own build commit via
`runtime/debug.ReadBuildInfo()`'s `vcs.revision`/`vcs.modified` build
settings -- `go build`'s default auto-stamping, which requires building
*without* that flag. If your checkout cannot be VCS-stamped at all (e.g. no
`.git`, or `go build` prints a VCS-status error), the tool degrades to an
explicit `harnessCommitSource` explaining why rather than guessing -- it
does not fall back to reading git state from the caller's current working
directory, which would silently record whatever OTHER checkout happened to
be there instead.

**Also build from a regular clone/checkout, not any form of `git worktree`
checkout.** This applies to `./cmd/scion` (the hub binary under test) as
much as to `apibench`/`seed`. Go's VCS auto-stamping only recognizes a
`.git` *directory*; a worktree's `.git` is a file pointing back at another
checkout's metadata. There are two different failure modes, not one, and
the second is worse:

- A worktree **outside** any other git checkout gets **no VCS stamp at
  all** -- a plain repo and a shallow clone both stamp correctly; this form
  of worktree does not. A hub built this way reports `hubScionVersion` as
  `"unknown"` even when it was built from a known, pinned commit.
- A worktree **nested inside another checkout** -- including a gitignored
  one, such as **this very repo's own `.claude/worktrees/<name>`**
  (`.claude/` is in `.gitignore`, so this is plausibly the common case on
  a fleet that creates worktrees there, not an edge case) -- gets silently
  stamped with the commit of the **enclosing** checkout instead, and can
  report `vcs.modified=false` ("clean") even though the nested worktree's
  own tree is completely different: the stamped revision is the enclosing
  checkout's `HEAD`, not the worktree's own. This is the same class of
  failure as trusting `git rev-parse HEAD` run in the caller's cwd -- a
  confidently wrong commit reported as fact, not an absent one -- just via
  a different mechanism.

Neither Go nor this harness can detect either case after the fact from
inside the running binary: once a revision and a modified flag are
reported, there is no remaining signal that distinguishes a correct stamp
from a wrong one. If you need the build identity recorded reliably:

1. Build from a plain `git clone`/checkout (not any `git worktree`). For
   the **hub binary specifically** (`./cmd/scion`), you can alternatively
   stamp its version explicitly via
   `-ldflags "-X github.com/GoogleCloudPlatform/scion/pkg/version.Commit=$(git rev-parse HEAD)"`.
   This `-ldflags` option is NOT available to `apibench`/`seed` --
   `harnessBuildInfo` reads only Go's own `vcs.revision` build setting,
   never `pkg/version.Commit`, so an `-ldflags`-stamped `apibench`/`seed`
   binary gets no benefit from this; those two must be built from a plain
   clone.
2. **Independently verify** before trusting a capture:
   `go version -m /tmp/apibench-bin | grep vcs.revision` and compare it
   against `git rev-parse HEAD` run in the checkout you actually intended
   to build from. A mismatch means you built from a worktree -- nested or
   not -- and the stamp does not mean what it looks like it means. This
   specific check does NOT work for a hub binary built with the `-ldflags`
   option above from inside a worktree -- `go version -m` only ever shows
   Go's own auto-stamp (the wrong, enclosing-checkout commit, or nothing),
   never the `-ldflags` value, which only shows up at runtime via
   `pkg/version.Short()`. For an `-ldflags`-stamped hub, verify its
   `GET /health` `scionVersion` field against `git rev-parse HEAD` instead
   -- the auto-stamp check only applies to binaries relying on Go's own VCS
   stamping (`apibench`, `seed`, and a hub built without `-ldflags`). A
   match is necessary but not sufficient in one edge case -- if a nested
   worktree's own `HEAD` happens to equal the enclosing checkout's `HEAD`
   (e.g. right after creating the worktree, before committing anything new
   in either), step 2 will show a match even though `vcs.modified` still
   reflects the ENCLOSING tree's dirty state, not the worktree's own. A
   mismatch is always diagnostic; a match is not quite a full guarantee in
   that specific case.

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

`env -i` alone is **not sufficient**: it clears process environment
variables, but Application Default Credentials are discovered via the GCE
metadata server (`169.254.169.254`), not the environment, so a hub launched
with only `env -i` still logs `GCP token generator configured` / `GCP
service account minting configured` / `Policy Troubleshooter checker
configured` and holds live connections to the metadata server and Google
APIs -- confirmed via `ss -tnp`. A throwaway bench hub must not be able to
mint tokens for a real service account.

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
3. While the hub is running, `lsof -a -nP -p <hub-pid> -i` and confirm the
   only rows are the hub's own listening socket on 127.0.0.1 plus any
   loopback client connections -- **no row with a non-loopback remote
   address**.
   **Do not drop the `-a`.** `lsof -p <pid> -i` (no `-a`)
   ORs its selectors instead of ANDing them: it is "every internet socket on
   the host, **plus** every file descriptor PID holds", not "PID's internet
   sockets". On a shared host this prints dozens of rows belonging to other
   processes regardless of what the hub itself holds, so "prints no rows at
   all" can never be the output of a working check and proves nothing about
   isolation either way -- confirmed by running it against an idle,
   socketless shell and getting 32 rows of other processes' sockets. `-a`
   makes lsof AND its selectors (PID **and** `-i`), which is what this check
   actually needs. Alternatively, with no `lsof` available: join
   `/proc/<pid>/fd` socket inodes against `/proc/<pid>/net/tcp` directly (no
   root required for a process you own).

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
or `?query`-suffixed `--db` value before looking at the filesystem, so
there is no DSN-form bypass. There is no override flag -- use a fresh path
or remove the file first.

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
empty reply to the client. Line numbers below are as of `73ebd022`, the
commit this harness's BASELINE measurements were captured against; they may
have shifted on upstream `main` since, which does not affect the baseline --
re-check them against whatever commit you are actually measuring:

- `pkg/config/hub_config.go:851` -- `HubServerConfig.WriteTimeout`, default
  `60 * time.Second`, wired into the standalone Hub API's `http.Server` at
  `pkg/hub/server.go:4518-4519`.
- `pkg/hub/web.go:2913` -- hard-coded `WriteTimeout: 60 * time.Second` on
  the combined hub+web `http.Server` (`WebServer.Start`).

At 500 agents, some fraction of `project-agents-list`/`project-list`/
`project-graph-embedded` requests take long enough to cross this ceiling
(see `measurements.md` for how often, which varies run to run with host
load -- the api-side client can occasionally still receive a
slow-but-complete response past 60s, so this is a real but not
deterministic cliff). Above it, the client gets a connection reset with
no body, `project-detail.ts`'s `loadData()` `Promise.all` rejects, and the
page falls to its error/empty state -- which renders almost no agent cards.
**A naive "did the expected element count ever appear" check cannot tell
that apart from "still rendering a huge DOM"**; `large-project-bench.mjs`
watches the actual network outcome of the load-bearing API request
(`page.on('response'/'requestfailed')`) to classify each run as `populated`
/ `load-failed(<reason>)` / `loaded-not-rendered` / `still-loading`
instead, and records exactly when that outcome was observed
(`networkObservedAtMs`).

**"60 seconds" is the configured server write deadline, not the
client-observed threshold.** A browser capture's own `networkObservedAtMs`
data at 500 agents:

| View / run | Outcome | `networkObservedAtMs` |
| --- | --- | ---: |
| `project-grid` run 2 | populated (200) | 116,515 |
| `standalone-graph` run 4 | populated (200) | 119,622 |
| `project-list` run 4 | `load-failed(network:ERR_EMPTY_RESPONSE)` | 156,190 |
| `standalone-graph` run 3 | `load-failed(network:ERR_EMPTY_RESPONSE)` | 161,755 |

The empty replies this capture observed arrived at roughly 156-162s from
navigation, not near 60s; loads whose response arrived at 116-120s
succeeded. This does not disprove `WriteTimeout` as the mechanism -- under
Go's HTTP/1.1 semantics the write deadline is armed once request headers
are read and a timed-out write fails at handler-completion time, which can
land well after the deadline itself if the handler is still running -- but
a 120s *success* means the server-side clock (whatever it is actually
measuring from) started well after this capture's navigation, or something
else is also in play. **That is an open question discussed in
`measurements.md`**; do not describe a specific wall-clock number as "the
60s cliff" without citing the `networkObservedAtMs` timing an actual
capture recorded, since it has not landed near 60s in any capture so far.

See `measurements.md`'s baseline section for the full per-run data at 500
agents on unmodified main.

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
  mislabeled it as "what the standalone graph page loads", which it is not.
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
sampled once at the start and again at the end of the run (a 500-agent run
takes long enough for load to swing within one report) -- see "Choosing
regression budgets" below for why this is recorded, and for what it is not
sufficient for. The report also records the effective
`--runs`/`--warmup`/`--timeout-seconds`/`--want-perf-trace` settings;
`harnessCommit` (the binary's own build-time VCS revision, see "One-time
setup" above for how this is obtained), `harnessCommitDirty` (`*bool`, nil
when the commit itself is unknown, so "clean" and "dirty state unknown"
are never indistinguishable), and `harnessCommitSource`; and
`hubScionVersion` (read from the hub's own `GET /health`) -- so a report
file is self-describing on both sides (harness AND hub) without having to
match either to a commit by timestamp. There is deliberately no
`hubVersion` field: `/health`'s `version` is a hard-coded placeholder in
hub source (`pkg/hub/handlers_health.go:86`), constant regardless of which
hub commit is actually running, and a field that always reads the same
value no matter what is measured would be worse than no field at all.

**`harnessCommitDirty`/the `.mjs` script's own dirty check count untracked
files as dirty.** This is intentional, not an oversight: Go's own VCS
auto-stamping determines `vcs.modified` the same way, via plain `git status
--porcelain` with no `--untracked-files=no` (see the Go toolchain's
`cmd/go/internal/vcs/vcs.go`), confirmed empirically too (a tree with
everything tracked committed and one new untracked file present still
stamps `vcs.modified=true`). A stray untracked file anywhere in the repo --
a log, a scratch note -- will mark a report dirty even with zero tracked
changes; that is consistent with what the Go-side stamp on the hub binary
itself would also report, not a bug to route around.

**Always pass `--notes`** describing conditions the report fields do not
capture on their own: how many other hub instances were co-resident during
this run (every capture to date has run 25/100/500 concurrently --
`MachineInfo.LoadAvg*` reflects the whole host, not this hub alone), and
the per-size timeout values in effect if they were raised above the
defaults shown in `EffectiveSettings`. A blank `notes` field in a raw
report is a gap for whoever reads it later, not a neutral default.

When the hub runs with request performance tracing on (add
`SCION_SERVER_HUB_PERFTRACE=true` to the isolated launch's `env -i` list, or
set `server.hub.perf_trace: true`), add `--want-perf-trace` to additionally
capture the `X-Scion-Perf-*` response headers per attempt: endpoint class,
phase times (microseconds) and counts, authorization store calls and times,
decision-audit counts, and DB pool waits. The hub sends those headers only
to unscoped local platform admins, so for the seeded member caller also
pass `--hub-perf-log <hub log file>` (redirect the hub's output to a file):
after the run, apibench joins each attempt to the hub's `perf_trace` log
line by request ID and stores the same fields, plus the `serialize` phase,
with `perfTraceSource: "hub-log"`. See `pkg/hub/perftrace.go` for the phase
definitions. On a baseline run against
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
fresh ("cold") browser context; runs 1..N-1 share one ("warm") context**,
reported separately
(`medianNavToPopulatedMsCold`/`...Warm`, `coldRunCount`/`warmRunCount`) as
well as combined. Cold is necessarily `n=1` per scenario per capture, and
the data collected so far does not show a consistent cold penalty -- at 25
agents both graph views were *faster* cold, and at 500 agents the grid was
faster cold too. Report the split because a reader may care about it, not
because this harness has established that one is reliably slower.

Every run is classified `populated` / `load-failed(<reason>)` /
`loaded-not-rendered` / `still-loading` (see "A real product finding"
above; `loaded-not-rendered` is a 2xx response that still never reaches the
expected rendered count, a render failure distinct from "nothing observed
at all"). Only `populated` runs contribute to the reported median/min/max/
stddev for navigation time, DOM element count (recursively counted through
every open shadow root, since this Lit app keeps almost all of its
structure there), and long-task totals from a `PerformanceObserver`
injected before navigation. `successCount`/`failureCount` and a
per-outcome tally are reported alongside, so "5/5 populated" and "1/5
populated, 4 load-failed" are never conflated into the same median. Each
run also records when the load-bearing request's network outcome was
observed, in elapsed ms from navigation start (`networkObservedAtMs`), so
a WriteTimeout attribution is a measurement, not an inference from a run's
total wall-clock time. The report is written incrementally (after every
scenario and every burst run), so a Chromium crash mid-benchmark loses at
most the in-flight run, not the whole report.

For the two graph scenarios, a populated run also performs a short
pan/zoom/hover interaction sequence (hover over up to 5 nodes, wheel-zoom
in and out, drag-pan) and reports the long-task cost specifically
attributable to it (`graphInteraction`) -- this was previously listed as a
#2393 acceptance gap "not attempted here for lack of time"; no source
change was needed, so it is implemented now.

**`graphInteraction.interactionMs` is mostly fixed harness overhead, not
UI latency.** The sequence contains roughly 750ms of fixed
`waitForTimeout` calls (five 50ms hover pauses, a 100ms post-zoom pause, a
100ms post-drag-start pause, a 300ms settle pause) plus the wall-clock cost
of around 30 Playwright round-trips (two 10-step drags and five hovers)
that scale with Playwright/CDP overhead, not agent count. **Read the
long-task delta (`graphInteraction.longTasks`), not `interactionMs`,** as
the measurement of actual UI cost -- it is the field that scales with agent
count (near-zero at 25 agents, up to ~733ms at 500 for
`standalone-graph`) and the one `measurements.md` bases its conclusions on.

It then runs the SSE burst-update scenario `--burst-runs` times (default:
same as `--runs`; pass `--burst-only` to skip the four view scenarios above
and go straight to the burst, e.g. for a targeted re-measurement after a
burst-logic-only fix), all within one browser context. Each run: picks up
to `--burst-count` (default 15) agents that are **not** currently
`suspended` (see below), computes a target phase per agent via
`pickBurstTarget`, fires real `phase` updates via the REST API as the
seeded owner while checking every POST's status, polls each *accepted*
agent's own rendered `<scion-status-badge>` until it shows the new value --
ground truth for "the live update reached the DOM" -- then **restores
every updated agent to its pre-burst phase AND activity** and waits, up to
a bound, to confirm the restore in the DOM. This does NOT block the next
run -- the wait is bounded and the next run starts regardless, but if the
restore was not fully confirmed in that window, the next run is marked
`invalid` rather than treated as having started from a known-good state.
See point 3 below for the exact mechanism.

Settle time is measured **per agent, independently, starting the instant
THAT agent's own POST resolves** -- anchoring on each agent's own POST
completion but only beginning to *observe* after every agent's POST had
returned would inflate a fast agent's recorded settle by the spread
between POST completions -- 0.24-1.40s observed in one capture, the same
magnitude as the reported medians. Server-side application is confirmed
independently via a follow-up GET per agent, so "the server never applied
this update" and "the UI did not settle" are reported as distinct,
non-overlapping counts.

**Three independent guards protect against a lost SSE update being
miscounted as settled**, because offsetting `pickBurstTarget`'s rotation by
run index alone is not enough -- the function's "never repeats" guarantee
does not hold against the real caller, which always passes the constant
pre-burst phase as `currentPhase`, not the previous run's own target (see
`pickBurstTarget`'s doc comment in `lib.mjs` for the exact mechanism and
`lib.test.mjs` for a test that holds `currentPhase` fixed, as the real
caller does, and fails against an index-only implementation):

1. `pickBurstTarget` now excludes both the agent's current phase AND the
   actual phase it was targeted with on its own previous run (tracked per
   agent-id across the whole scenario), not just the current phase.
2. Immediately before posting each run's updates, every target's badge is
   read; any agent already showing the phase about to be requested is
   excluded from that run's settle tracking (`preStaleExcludedCount`) --
   this closes the hole regardless of whether (1) or the restore-wait is
   itself correct.
3. The restore-wait's RESULT has an effect on the next run (described
   precisely here, not as "gates", to avoid implying it blocks -- see the
   bounded-wait-then-proceed behavior described above this numbered list).
   If the previous run's restore was not fully confirmed in the DOM within
   the bound (`restoreFullyConfirmed` is false), the next run is marked
   `invalid` and excluded from the scenario's settle statistics
   (`invalidRunCount`), rather than merely recording the gap and silently
   proceeding as if nothing had happened. The restore-wait's expected
   value is computed the way the UI actually renders it
   (`displayStatusLabel` in `lib.mjs`: activity instead of phase for a
   `running` agent with non-empty activity) -- comparing against the
   literal phase could never match for those agents no matter how long it
   waited.

The pre-stale exclusion (guard 2) and the invalid-run exclusion (guard 3)
are implemented as pure functions in `lib.mjs` (`computePreStaleIds`,
`summarizeBurstScenario`) with their own `lib.test.mjs` coverage -- not
only inline in `large-project-bench.mjs`, where they would only be
exercised indirectly against a fake hub and DOM.

**What the reported statistics mean, precisely.** `medianSettleMs` is the
median OF THE PER-RUN MEDIANS (one sample per valid run -- a median of
medians, not a median over every individual agent's settle time). The true
per-agent range across all valid runs (the min of each run's own min and
the max of each run's own max) is reported as
`perAgentMinSettleMs`/`perAgentMaxSettleMs` -- deliberately NOT named
`minSettleMs`/`maxSettleMs`, to avoid the kind of silently-redefined field
name that the `postFanOutMs`/`burstWallClockMs` split below exists to
prevent. The range of the five per-run medians itself is available
separately, under its own name: `runMedianMinMs`/`runMedianMaxMs`.

**Each poll has a SAMPLING INTERVAL, not a "floor".** The first poll
happens immediately after the POST resolves, so values well under 170ms
are common. The raw reports keep no per-agent samples, only per-run
minimums: per-run minimums as low as 11-12ms were observed (25 and 100
agents). What is true: each sample can LAG the true DOM update by up to
one poll interval -- a 100ms sleep plus a deep shadow-DOM badge read,
which runs 14ms alone but ~68ms median (p90 98ms) with `--burst-count`
concurrent pollers sharing one page. Differences smaller than that
combined interval (roughly 170-200ms) cannot be used to rank hub speed,
even though individual samples will themselves often read below it.
`postFanOutMs` (POST-only completion spread) and `burstWallClockMs` (the
whole per-agent POST-plus-poll sequence) are reported separately as two
distinctly-named fields, rather than overloading one name for both
meanings now that polling happens inside the same per-agent `Promise.all`.
`postFanOutMs` only reflects agents whose POST was accepted; a rejected
POST records no `postCompletedAt` and is excluded from that spread, so a
run with any rejection reports a narrower `postFanOutMs` than the full
POST attempt actually took.

**Settle times are environment-sensitive:** a lower-load, single-hub
reproduction measured about 4x lower settle times at 25 agents (39ms vs
164ms) but only about 2x lower at 500 agents (320ms vs 619ms) than a
three-co-resident-hub capture under higher load -- "roughly 4x" is not a
single ratio that holds across agent counts. The browser benchmark's
report records `os.loadavg()` at the start and end of the run, matching
`apibench`'s convention, so a reader can tell environment noise from an
actual difference.

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
  against a given database -- what looked like "SSE settle flakiness" was
  entirely this, not SSE. The burst scenario now skips currently-suspended
  agents as sources, never targets `suspended`, checks every POST's
  status, and restores state (including waiting for the restore -- see
  above) afterward.
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
else is running. Repeated apibench reruns under otherwise identical
isolated conditions varied by more than 2x run to run purely from this --
host-load noise, not anything this harness's own changes caused.
Environment noise on this scale would swamp any regression budget chosen
from data captured here.

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
