# Order-dependent LogCapture flake fix (ptone/scion#2337)

Branch `scion/broker-settings-fix-logcapture-flake`. Originally cut from `upstream-main`
(`GoogleCloudPlatform/scion` main at `e6b9ba29`); rebased onto current `upstream-main` (`a604ad4`)
during round 2 (see below). PR `ptone/scion#2366` against `ptone/scion` `main`, not draft. Part of
`ptone/scion#2061`. Priority: Now/P1/XS, ahead of all other broker-settings work, per the EM.

## Problem

`TestHandleAgentMessage_LogCapture_RawContentRedacted`'s control subtest ("non-raw message content
is not redacted") intermittently failed — but only as part of the full `pkg/hub` suite, never in
isolation or in small `-run` selections. Seen in check-run 109932791288 on ptone/scion#2334 (the
prior task's PR), and confirmed as the sole `make test-hub-sqlite` failure on bare upstream main
during that task.

## Root cause (corrected after round 1 review)

`TestAgentPortProxyThroughTunnel` (`pkg/hub/port_forward_handlers_test.go`) drives a real
`pkg/sciontool/portforward` tunnel against the test hub. The tunnel logs on connect
(`pkg/sciontool/portforward/tunnel.go`'s `log.Info(...)`) through `pkg/sciontool/log`, whose
`write()` lazily calls the package's own `Init()` the first time anything calls
`Info`/`Error`/`Debug` in the process, guarded by `write()`'s own `initialized` flag (`Init()`
itself is not idempotent — it calls `slog.SetDefault` every time it runs; the one-shot behavior
here comes entirely from `write()`'s `if !initialized { Init() }` check, not from `Init()` itself).
`Init()` ends with `slog.SetDefault(slog.New(newHandler()))`, and `slog.SetDefault` itself is what
redirects the classic `log` package's global writer (it calls `log.SetOutput` internally whenever
the new handler isn't `slog`'s own pristine, unconfigured type) — neither `Init()` nor anything else
in `pkg/sciontool/log` calls `log.SetOutput` directly. Because `port_forward_handlers_test.go` sorts
before `raw_guard_test.go`, in the default test order `TestAgentPortProxyThroughTunnel` is the first
test in the whole suite to trigger this, permanently replacing `slog.Default()` — and, via that
bridging, the classic `log` package's global writer — with sciontool's handler for the rest of the
process. No goroutine race or timing dependency is involved; the dependency is purely on test order,
and the pair reproduced deterministically (see Evidence).

`TestHandleAgentMessage_LogCapture_RawContentRedacted` (both subtests) used to call `deliverySetup(t)`
(which builds a `Server` via `New()`) *before* `captureSlog(t)`. `New()` builds `s.messageLog` via
`logging.Subsystem("hub.messages")` — `slog.Default().With(...)` — once, at construction time. In
the old order, that snapshot bound to sciontool's handler (already installed by the port-forward
test, earlier in the run), not to Go's own pristine default and not to `captureSlog`'s buffer.
`captureSlog` still correctly redirected `slog.Default()` for the *rest* of the test, but
`messageLog` was already fixed to sciontool's handler and never saw the swap: the line was written
to sciontool's stderr writer and to sciontool's own log file, never to `buf`. The redaction
assertion (`NotContains`) passed vacuously in this state; only the control assertion (`Contains`)
could observe the loss.

This is the "shared global state which leaks between tests" the brief described — a real, permanent
hijack of `slog.Default()` by an unrelated test, not a race.

## Investigation

Reproduced the failure reliably by running the full `pkg/hub` suite (`go test ./pkg/hub/`, matching
the `make test-hub-sqlite` skip list) on bare `upstream-main` in multiple separate full runs, all
failing this exact test the same way: `assert.Contains(buf.String(), secret)` failed because the
"message received for delivery" line was absent from `buf`.

An initial investigation pass mis-identified this as a timing race (a leftover background goroutine
racing on the log-capture buffer) and proposed a different fix; **that diagnosis was wrong** and was
corrected in round 1 of independent review (`broker-settings-rev-fix-logcapture-1`,
`reviews/broker-settings-rev-fix-logcapture-1.md`). The reviewer traced the actual mechanism
described above and confirmed it with a deterministic two-test repro
(`-run '^(TestAgentPortProxyThroughTunnel|TestHandleAgentMessage_LogCapture_RawContentRedacted)$'`,
5/5 fail on base, 5/5 pass on the reordered branch) and an instrumented diagnostic showing
`slog.Default()` was sciontool's `*log.slogHandler` at `New()` time, not Go's pristine
`*defaultHandler`. Round 2 review (`broker-settings-rev-fix-logcapture-2`,
`reviews/broker-settings-rev-fix-logcapture-2.md`) independently re-verified R2's fix three ways
(deterministically, under `-race`, and with a whole-suite `slog.Default()` instrumentation) and
found it correct, but found the branch had fallen behind upstream (see Round 2 below).

## Fix (test-only, root cause)

Three parts, all test-only:

1. **Stop the leak at its source (R2, required by round 1).** In `TestAgentPortProxyThroughTunnel`
   (`pkg/hub/port_forward_handlers_test.go`), before starting the tunnel manager: save
   `slog.Default()`, `log.Writer()` and `log.Flags()`; call `sciontoollog.Init()` (exported) under
   that save; restore all three immediately. `pkg/sciontool/log`'s lazy path (`write()`) only calls
   `Init()` when its own `initialized` flag is false, so triggering `Init()` ourselves first — and
   restoring the process-wide globals it mutates right after — means the tunnel's own later
   `log.Info` call on connect just writes directly, without touching `slog.Default()` again.
   Production `pkg/sciontool/log` is unchanged.
2. **The `RawContentRedacted` reorder** (`captureSlog(t)` before `deliverySetup(t)` in both
   subtests of `TestHandleAgentMessage_LogCapture_RawContentRedacted`) — originally added by this
   PR, confirmed correct by round 1 review as defense in depth — **landed upstream independently**
   via GoogleCloudPlatform/scion#2138 (`2957bb65`) while round 1/2 review was in progress. After
   rebasing onto current `upstream-main` (round 2, R3 below), this PR's own copy of that hunk is
   gone (the commit that introduced it, `fdd21234`, became empty against the new base and was
   dropped by the rebase); the victim test itself is now fixed by upstream, not by this PR.
3. **O1 (round 1, Recommended, applied in this PR).** The same capture-first reorder applied to the
   file's two other capture-after-construct siblings, which upstream's GoogleCloudPlatform/scion#2138 did not touch:
   `TestHandleAgentMessage_LogCapture_RejectedRawSecretNotExposed` and
   `TestHandleProjectBroadcast_RawRejected`. Both were silently vacuous in the same way, currently
   masked only by file-name sort order (`p` before `r`) keeping the hijack from ever firing before
   them in the default run order — a shuffle run could still expose them. Unlike the victim test,
   both siblings assert only `NotContains` with no positive control, so passing or failing them
   cannot, by itself, prove the leak is present or absent; they matter because R2 makes them
   correct-by-construction instead of accidentally-correct-by-sort-order.

No assertion was loosened, no subtest skipped (`t.Skip`) or deleted, and no retry/sleep was added.

**What this PR now delivers, after the rebase:** the R2 fix (which closes the leak for *every*
later test in the suite, not just the tests this PR happens to touch) and the two O1 sibling
reorders. The `RawContentRedacted` reorder itself is no longer this PR's contribution — it is
upstream's, via GoogleCloudPlatform/scion#2138 — though this PR's original diagnosis and fix attempt is what first
identified that ordering as necessary.

## Round 1 review disposition (`broker-settings-rev-fix-logcapture-1`, REQUEST CHANGES)

Full report: `reviews/broker-settings-rev-fix-logcapture-1.md`.

| Finding | Disposition |
|---|---|
| R1 (Required): root-cause write-up wrong in the test comment, commit message, PR body and this log | Fixed. All four rewritten to describe `TestAgentPortProxyThroughTunnel` -> sciontool tunnel `log.Info` -> lazy `pkg/sciontool/log.Init()` -> permanent `slog.SetDefault`. The "timing race" / "cannot be narrowed" claims are removed. |
| R2 (Required): the actual polluter (`TestAgentPortProxyThroughTunnel`) was not fixed | Fixed, test-only, in `port_forward_handlers_test.go`: save/eager-init/restore around `sciontoollog.Init()`. Production `pkg/sciontool/log` untouched. Re-verified independently in round 2 (deterministic repro, `-race`, whole-suite instrumentation). |
| O1 (Recommended, same PR): apply the same reorder to `raw_guard_test.go`'s other two capture-after-construct tests | Done in this PR; survived the round 2 rebase. |
| O2 (Recommended, file separately): `handlers_broker_inbound_raw_test.go:284/345` and `credential_decoration_ac_test.go:566` capture after construction | Filed as ptone/scion#2416 by the lead. Does not overlap this PR's O1 reorders (different files). |
| O3 (Consider, file separately): `pkg/hub` tests writing through the real `$HOME` | Filed as ptone/scion#2417 by the lead. Out of scope here. The tunnel still appends to `/home/scion/agent.log` (or `$HOME/agent.log`) after the R2 fix (round 2 FYI 2) — that belongs under ptone/scion#2417, not here. |

## Round 2 review disposition (`broker-settings-rev-fix-logcapture-2`, REQUEST CHANGES)

Full report: `reviews/broker-settings-rev-fix-logcapture-2.md`. R1, R2 and O1 were confirmed closed
on the merits; the blocker was purely mergeability.

| Finding | Disposition |
|---|---|
| R3 (Required): branch conflicts with upstream main — GoogleCloudPlatform/scion#2138 already landed the `RawContentRedacted` reorder with its own comment; CI never ran past a failing Mergeability Gate | Fixed: fetched current `upstream-main` (`a604ad4`) and rebased. Resolved `raw_guard_test.go` by keeping upstream's `RawContentRedacted` body and comment and dropping this PR's own doc-comment paragraph; kept `port_forward_handlers_test.go` and both O1 sibling reorders. The commit that only touched the now-upstream `RawContentRedacted` hunk (`fdd21234`) became empty against the new base and was dropped by the rebase (`git rebase --skip`), not force-included as an empty commit. Force-pushed the rebased branch to `origin/scion/broker-settings-fix-logcapture-flake` with `--force-with-lease`, per the EM's one-time authorization for this rebase. PR body and this log updated accordingly (this section and the "Fix" section above). |
| N1 (Nit): the `port_forward_handlers_test.go` comment was too long (13 lines) and said "one-shot `Init()`", which is inaccurate — `Init()` re-runs `slog.SetDefault` every call; the one-shot guard lives in `write()` | Fixed: shortened to 2 lines, using the reviewer's suggested wording, and removed "one-shot" everywhere it appeared (the comment, this log, and the commit message). |
| N2 (Consider): use `sciontoollog.SetLogPath(t.TempDir()+"/agent.log")` instead of save/restore, since it sets `initialized=true` without ever calling `slog.SetDefault` and also stops the tunnel writing into the real `$HOME` | **Declined.** `logPath` would then point at a directory `t.Cleanup` deletes once the test ends. If the lingering tunnel goroutine's disconnect log (`tunnel.go`'s "disconnected" line) fires after that — a real possibility, since the tunnel goroutine outlives the test body until `cancel()` propagates — `write()`'s `os.OpenFile` on the now-missing `logPath` fails, and `write()` falls back to `/tmp/agent.log` *and flips `debug=true`* for the rest of the process (another, different global-state leak, self-inflicted). The current save/restore has no such failure mode. Routing the tunnel's log destination away from the real `$HOME` is exactly ptone/scion#2417's scope, not this PR's. |
| N3 (Nit): project-log accuracy — "Two changes" was followed by three items; O1 was mislabeled a "Nit" (it is Recommended); the Investigation section said no non-test `log.SetOutput` caller exists other than `pkg/sciontool/log/log.go`, but that file does not call `log.SetOutput` directly — it calls `slog.SetDefault`, which does so internally | Fixed in the "Fix" section above (now correctly says "Three parts"), in the round 1 disposition table (O1 is "Recommended"), and in the "Root cause" section's phrasing above. |

## Evidence

**Round 1 (bare `upstream-main` at `e6b9ba29`, before the rebase):**
- The deterministic pair
  (`-run '^(TestAgentPortProxyThroughTunnel|TestHandleAgentMessage_LogCapture_RawContentRedacted)$' -count=5`)
  failed 5/5 on the control subtest on that base. The full `pkg/hub` suite (make test-hub-sqlite
  skip list) also failed this exact test the same way in every separate full run performed across
  this task and the prior `fix-catalog-live` task.
- With the reorder *temporarily reverted* (not committed) and only the `port_forward_handlers_test.go`
  save/restore in place, the same pair passed 5/5 — proof the leak was fixed at its source,
  independent of the reorder.
- Full fix (reorder + R2), pair `-count=5`: 5/5 pass. `-shuffle` seeds 1, 5, 6, 8 (port-forward-first
  orderings): all 4 pass. `go test -p 2 -count=1 -timeout 30m ./pkg/hub/...`: only failure
  `TestCatalogHTTPEntryPoints_LiveMethodCheck` (ptone/scion#2334, not yet on upstream main from that
  base — expected).

**Round 2 (rebased onto current `upstream-main` at `a604ad4`, which already carries both
GoogleCloudPlatform/scion#2138 and the catalog fix GoogleCloudPlatform/scion#2146):**
- Main pair (`TestAgentPortProxyThroughTunnel` + `TestHandleAgentMessage_LogCapture_RawContentRedacted`),
  `-count=5`: 5/5 pass — but now passes **even with the R2 fix reverted**, because upstream's own
  GoogleCloudPlatform/scion#2138 reorder already makes this particular victim immune to the hijack. The pair alone can no
  longer demonstrate this PR's leak closure.
- Each O1 sibling paired with `TestAgentPortProxyThroughTunnel`, `-count=5`: both pass 5/5 — but
  they pass **regardless of the R2 fix** too, since both assert only `NotContains` with no positive
  control (see the "Fix" section above); their pass/fail cannot demonstrate leak presence or
  absence either.
- **Direct evidence of leak closure**, since neither test pairing above can show it: added a
  temporary (not committed) checker test asserting `fmt.Sprintf("%T", slog.Default().Handler())`
  immediately after `TestAgentPortProxyThroughTunnel`. With the R2 fix reverted: handler type is
  `*log.slogHandler` (sciontool's handler — the hijack, directly observed). With the R2 fix applied
  (the real branch): handler type is `*slog.defaultHandler` (Go's own pristine default, unchanged).
- `go test -p 2 -count=1 -timeout 30m ./pkg/hub/...` on the rebased head (1000s): **all packages
  `ok`, zero failures** — `TestCatalogHTTPEntryPoints_LiveMethodCheck` now passes too, since
  GoogleCloudPlatform/scion#2146 (the catalog fix) is on this base.
- `go vet ./pkg/hub/...`: clean. `golangci-lint run --new-from-rev=upstream-main ./pkg/hub/...`:
  0 issues.

## Follow-ups (not in this PR)

- ptone/scion#2416 — O2: `handlers_broker_inbound_raw_test.go:284/345` and
  `credential_decoration_ac_test.go:566` capture logs after constructing their server, the same
  latent vacuous-assertion pattern this PR's O1 fixed in `raw_guard_test.go`.
- ptone/scion#2417 — O3: several `pkg/hub` tests write through the real `$HOME` rather than an
  isolated temp directory, including this fix's own tunnel, which still appends to
  `/home/scion/agent.log` after the R2 fix.

## Deliverables

- PR `ptone/scion#2366` (`scion/broker-settings-fix-logcapture-flake` -> `main`, ptone/scion), not
  draft.
- This log entry, including both round 1 and round 2 disposition tables.
- `scion message` to `broker-settings-em` with the new head SHA, the evidence numbers and the
  disposition of every round 2 finding.
