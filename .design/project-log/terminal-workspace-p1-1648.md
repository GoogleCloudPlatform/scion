# Terminal workspace P1.3 — singleton browser ownership

Implemented `TerminalCoordinator` at the accepted P1.1 base
`6c7f6677e676e5b206d00d151f39f6ce1f035b86`. The coordinator retains an exclusive,
non-stealing Web Lock for its document/account lifetime and uses a Hub/base-path/
account-scoped BroadcastChannel to route explicit open intents. Only the lock holder
opens registry sessions. Request IDs are deduplicated before callbacks; acknowledgments
must match request, normalized agent UUID, and owner generation. Timeouts stay pending
without cancellation or a second attach. Selection and observed document focus remain
separate results.

The existing registry is unchanged. Retained UI integration supplies resource
initialization and selection adapters. Selection receives an owner lifetime abort
signal; local route cancellation still requires the later router's navigation guards.
The manager confirmed that no cross-tab per-request cancellation protocol belongs in
this leaf. Keep the coordinator when showing Chat/Dashboard; stop only for document
or account teardown, closing sessions before releasing ownership.

Validation: 17 isolated Chromium cases exercise the production coordinator/registry
with real Web Locks, BroadcastChannel and multiple pages; HTTP/WebSocket endpoints
are intercepted and rendering/selection use minimal adapters. Cases cover simultaneous
opens, duplicate/conflicting IDs, invalid/stale acknowledgments, delayed acknowledgment,
held-lock retries, scope separation, unsupported API paths, default focus observation,
mode retention, teardown cancellation and release. Full web unit suite: 59 files,
1177 tests passing. Production and supplemental test typechecks, scoped typed lint
(zero errors), production build and fixture syntax checks pass. Root lint has known
baseline debt; final counts and exact commands are in the shared developer report.
No Go code changed; cumulative `make ci-full` belongs to the manager/integration writer
and was not run for this leaf. Headed desktop focus is deferred to #1662; actual
lifecycle freeze remains P3.3. No live agents or authenticated Hub were exercised.

Integration adapter instructions and reproducible browser commands are in
`web/e2e/terminal-coordinator/README.md`. Durable candidate report, check logs, verified
Git bundle and manifest are under the externally mounted
`/scion-volumes/scratchpad/projects/terminal-workspace/` (`reports/p1-1648-*` and
`transfers/p1-1648-candidate.*`). Independent review and integration are separate
manager gates. No remote Git operations, shared registry edits, sibling implementation,
or child agents were used.
