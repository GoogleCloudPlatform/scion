# Federated project access, item 10: merge forward onto main 6066222

A2A bridge reply polling for visitors, merged forward from merge base `67d812af1c80` onto main `60fc1bda6b2f69f6bfde70c918dd1783952843c2` (856 commits later) and re-gated on that base, then given the round 1 review fixes and merged again onto main `6066222f398dde594c6e8eae8b94a73194ff98ba` (see "Round 1 fixes").

- Item branch head before the merge: `b22652b158e282f70fb501cc5c100ac2ab8ad1ee`.
- Merged head: see the branch tip. This entry is the last commit and changes no code. The round 1 gate ran on the head just before it. In the "Gate" section below (base 60fc1bd), the full bridge module run and the mutants ran on the merge plus the test fix; the hub shards, `make ci` and `make build` ran one commit later, which adds only `TestReplyWaitKey_SameCallerOnly` (run on its own in the gate).

## Merge

- The earlier federation items this branch was stacked on are on main already: the visitor agent list and read routes (upstream PR 3908) and federated users in the A2A bridge (upstream PR 4021). Their copies on this branch were dropped in favour of main's. Item 10 adds nothing that main already had.
- Item 10's own change is bridge-only. After the merge, `git diff origin/main...HEAD` touches 22 files plus this entry: `extras/scion-a2a-bridge/internal/bridge/` (reply poller, executor, bridge, broker, caller, config, stream, and their tests), the bridge README, the sample config, `integration/reply_poll_e2e_test.go`, and `.design/project-log/a2a-bridge-reply-polling.md`. Nothing under `pkg/` or `docs-site/` differs from main.
- Conflicts: 12 files. The 7 outside item 10 (`docs-site` pages, `pkg/hub/federation_visitor_agents_test.go`, `federation_visitor_messaging.go`, `handlers_agent_messaging.go`, `handlers_projects_core.go`, `extras/.../federated_user.go`) take main's version. The 5 bridge files were resolved by re-applying item 10's own diff (item 7 head to item 10 head) onto main's copy. 20 of the 22 files match that mechanical re-application exactly; the two hand-resolved ones:
  - `executor.go`: main added the launch wait (asynchronous create, upstream PR 3801) before task registration. Main's launch-wait blocks stay; item 10's same-caller check follows them, still before the task is registered.
  - `federated_user_test.go`: both sides added fields to the hub test double; both kept.
- One semantic conflict the merge did not flag: main's federated-user launch test read `activeTaskEntry.federatedUserID`, which item 10 replaced with `userCaller` and `callerUserID` (the same account ID for a federated user caller). The test now reads the item 10 fields.

## Gate

Every go command ran with a private `GOCACHE` and `GOMODCACHE` and `-buildvcs=false`.

| Check | Command | Result |
|---|---|---|
| A2A bridge module | `go test -count=1 ./...` in `extras/scion-a2a-bridge` | pass (cmd, integration, internal/bridge, internal/state) |
| pkg/hub shard 1 (`federation_visitor_agents_test`, `federation_visitor_messaging_test`) | `go test -p 1 -overlay <hubshard overlay> -tags hubshard,hubshard_1 -count=1 -timeout 30m ./pkg/hub` (GOGC=25) | pass |
| pkg/hub shard 3 (`route_classification_test`) | same, `hubshard_3` | pass |
| pkg/hub shard 4 (`test_admin_census_test`) | same, `hubshard_4` | pass |
| `request_path_sites_test` | `go test -count=1 ./pkg/util/logging/` | pass |
| New same-caller key test | `go test -count=1 -run '^(TestReplyWaitKey_SameCallerOnly\|TestReplyPoll_SameCallerConcurrency)$' ./internal/bridge/` | pass |
| `make ci` | `make ci` | fmt-check, lint, check-custom pass; test-fast fails in one test outside item 10 (`TestHarnessProvisionPython/hermes`, `./harnesses`): the container's inherited `SCION_MODEL` reaches the Python provision test; make stops before `build` |
| `./harnesses` rerun | `env -u SCION_MODEL go test -count=1 -run '^TestHarnessProvisionPython$' ./harnesses/` | pass |
| scion build | `make build` | pass |

The hub shards ran through `go test` with the `hack/hubshard` overlay rather than `make test-hub-shard`, so each compiled once inside the process-session check; the make target's test-list comparison was not run. No PostgreSQL was available for this gate, so the database-backed bridge tests (those that read `TEST_DATABASE_URL`, including `pgstore_test.go`, `pgstore_crossprocess_test.go` and `TestReplyPoller_StandaloneAppendFence`) skipped here. They ran in round 1 (below).

### Mutants

Fresh one-site changes at the item 10 sites as they stand after the merge. Each was applied, run against the narrowest test, and reverted; none was committed.

| # | Site | Change | Test | Result |
|---|---|---|---|---|
| M01 | `reply_poller.go` `pollOnce` | page loop `<` to `<=` (one extra page) | `TestReplyPoller_Paging`, `TestReplyPoll_Selection` | caught |
| M02 | `reply_poller.go` `run` | backoff cap removed | `TestReplyPoller_BackoffSchedule` | caught |
| M03 | `reply_poller.go` `pollCapped` | in-flight read cap bypassed | `TestReplyPoller_InFlightCap` | caught |
| M04 | `executor.go` | same-caller check skipped | `TestReplyPoll_SameCallerConcurrency` | caught |
| M05 | `reply_poller.go` `replyWaitKey` | key ignores the caller (always the admin user) | `TestReplyPoll_SameCallerConcurrency` | survived; then caught by the new `TestReplyWaitKey_SameCallerOnly` |
| M06 | `reply_poller.go` `selectReply` | reply sender not checked against the agent | `TestSelectReply_AnchorByID`, `TestReplyPoll_Selection` | caught |
| M07 | `reply_poller.go` `readOutcome` | 403 retried instead of ending the call | `TestReplyPoll_LocalRefusalsAndStatusMapping` | caught |
| M08 | `reply_poller.go` `mayAppend` | append fence ignores the done flag (done flag only; the standalone fence body is covered by the PostgreSQL run and MF1, MF2 in "Round 1 fixes") | `TestReplyPoller_AppendFence` | caught |
| M09 | `bridge.go` `HandleBrokerMessage` | pushed copy not dropped while a poller is live | `TestHandleBrokerMessage_DropsPushWhilePollerLive`, `TestExecutor_PolledWaitIgnoresAppendedPushEvent` | caught |
| M10 | `bridge.go` `pushAcceptReason` | user caller's own topic not required | `TestCorrelateToTask_PluginAcceptanceTable` | caught |
| M11 | `executor.go` reader selection | federated user given a reply poller | `TestExecutor_FederatedUserWaitsOnPushPathWithPollingOn`, `TestExecutor_MemberPollerAndFederatedUserPushWaitOnOneAgent` | caught |
| M12 | `reply_poller.go` `classifyReplyRow` | input-needed without a thread taken as a question | `TestReplyPoll_Selection`, `TestSelectReply_AnchorByID` | caught |
| M13 | `config.go` `IsEnabled` | `reply_poll.enabled: false` ignored | `TestReplyPoll_RollbackSwitch` | caught |
| M14 | `executor.go` preflight | preflight refusal does not end the call before the send | `TestReplyPoll_LocalRefusalsAndStatusMapping` | caught |

The visitor route, route authorization manifest and store claim sites named for this item before the merge are main's code now (upstream PR 3908 and 4021) and are not item 10 changes, so they are not in this table; their tests ran in the shards above.

### Leftover-process check

The bridge module and each hub shard ran as `SIDFILE=<file> setsid --wait run-in-session.sh <go test args>`, where the script records `$$` (the new session ID) and execs `go test`. A small checker reads only `/proc/[0-9]*/stat`, takes the session field, and lists the processes in that session; it exits 2 with "could not check" when `/proc` cannot be read. It ran once while `go test` was running and once after `setsid --wait` returned.

| Run | Members during | Members after |
|---|---|---|
| A2A bridge module | 1 | 0 |
| pkg/hub shard 1 | 2 | 0 |
| pkg/hub shard 3 | 2 | 0 |
| pkg/hub shard 4 | 2 | 0 |

Limit: a child that calls `setsid` itself leaves the session and is not seen. A search of the bridge module and the `pkg/hub` tests for `Setsid` and `Setpgid` finds one use: `integration/runner_script_test.go` sets `Setpgid`, which starts a new process group inside the same session, so it stays visible. No test or helper calls `Setsid`.

## Notes

- `TestReplyWaitKey_SameCallerOnly` (`reply_poller_test.go`) is new in this merge.
- The database-backed bridge tests ran in round 1 against a local PostgreSQL 17 (below).

## Round 1 fixes

Base: main `6066222f398dde594c6e8eae8b94a73194ff98ba`, merged with `git merge --no-edit origin/main`, no conflicts. Main changed nothing under `extras/scion-a2a-bridge` since 60fc1bd, but the bridge module builds against the repository root (`replace ../../`), and main's session generation change to hub user tokens (upstream PR 4741) removed `UserTokenService.GenerateAccessToken`. Item 10's e2e helper in `integration/reply_poll_e2e_test.go` called it, so the `integration` package stopped building. It now calls `MintAccessToken` with the user's `SessionGeneration`, as the hub's web session path does (main's in-package hub tests mint with generation 0 through a test-only shim; a freshly created user has generation 0, so both give the same token here). After the merge, `git diff origin/main...HEAD` still touches 22 files plus this entry, and nothing under `pkg/` or `docs-site/`, so no hub shards were re-run.

- **The PostgreSQL append fence had never run.** PostgreSQL 17 was installed from the distro repositories, and a private cluster ran with trust auth on a unix socket in a temporary directory. The whole bridge module ran against it with `TEST_DATABASE_URL` and `TEST_REQUIRE_DATABASE=1` (below). The four subtests of `TestReplyPoller_StandaloneAppendFence` ran and passed, and so did `TestPostgresTaskStoreBrokerHandlerDedup`. Two new mutants at `standaloneAppendFence` are killed by that test (MF1, MF2). The parent test now skips as a whole when `TEST_DATABASE_URL` is unset, using the same check as `testPostgresTaskStore`, so a run without PostgreSQL reports a skip rather than a bare pass. The CI workflow is unchanged: the PostgreSQL job still runs only `./integration`, and adding the `./internal/bridge` database tests to it is left as a follow-up.
- **Same-caller wiring.** New subtest of `TestReplyPoll_SameCallerConcurrency`: while one user caller waits, a second, distinct user caller calls the same agent. Its message is sent and it is not refused. It kills MF3, which passes `replyWaitKey(nil, ...)` at the executor call site.
- **Upstream references.** The two references item 10 added to issue 4603 in `owner/repo#N` form (`.design/project-log/a2a-bridge-reply-polling.md` and `integration/reply_poll_e2e_test.go`) now read "upstream issue 4603". The changed files have no other such reference on an item 10 line. The ones left in `bridge.go`, `federated_user_test.go` and `lifecycle_test.go` are on lines main already has, so they keep main's text.
- **File count.** The file count reads "22 files plus this entry"; a recount after the fixes gives the same.
- **Lint.** A scoped `golangci-lint` (default linters; the bridge module has no config of its own) found 6 unchecked `Close` errors in item 10's test files. They are fixed. Main uses the same unchecked pattern elsewhere in the module, and those lines are left alone.

### Round 1 gate

Every go command ran with a private `GOCACHE` and `GOMODCACHE`, `GOFLAGS=-buildvcs=false`, and the database settings above where marked.

| Check | Command | Result |
|---|---|---|
| A2A bridge module with PostgreSQL, final head | `go test -count=1 -p 1 -v -timeout 40m ./...` in `extras/scion-a2a-bridge` | pass: 950 pass, 0 fail, 1 skip (`TestPostgresMigrationHelperProcess`, the subprocess helper entry point, which skips unless started as a helper). Packages cmd, integration, internal/bridge and internal/state all ok |
| Standalone fence subtests | from the same `-v` output | all 4 ran and passed (none skipped) |
| `TestPostgresTaskStoreBrokerHandlerDedup` | from the same `-v` output | pass |
| First module run, before the e2e helper fix | same command | cmd, internal/bridge and internal/state pass (891 pass, 0 fail, 1 skip); integration did not build (the removed token API above) |
| `integration` after the fix | `go test -count=1 -p 1 -v -timeout 40m ./integration/` | pass: 59 pass, 0 skip |
| gofmt | `gofmt -l ./internal ./integration` | no files |
| golangci-lint | `GOGC=40 golangci-lint run --new-from-rev=origin/main --concurrency=1 ./internal/bridge/... ./integration/...` | 0 issues (6 before the fix above) |

`make ci` was not run in this round. The diff against main touches no root-module code, and the root lint and tests do not cover the bridge module.

### Round 1 mutants

The same procedure as above. MF1 to MF3 ran against PostgreSQL where the test needs it. M01 to M14 were not re-run: the non-test bridge code they mutate is the same as in the earlier gate.

| # | Site | Change | Test | Result |
|---|---|---|---|---|
| MF1 | `reply_poller.go` `standaloneAppendFence` | final SDK state check dropped | `TestReplyPoller_StandaloneAppendFence` (PostgreSQL) | caught: "snapshot final" subtest appends 2 events, want 0 |
| MF2 | `reply_poller.go` `standaloneAppendFence` | execution lease heartbeat check dropped (always true) | `TestReplyPoller_StandaloneAppendFence` (PostgreSQL) | caught: "lease released" and "lease never claimed" subtests append 2 events, want 0 |
| MF3 | `executor.go` wait key | `replyWaitKey(nil, ...)`: every caller keyed as the admin user | `TestReplyPoll_SameCallerConcurrency` | caught: the distinct-caller subtest's second call is refused and never sent |

### Round 1 leftover-process check

The same session check as above, now sampling every 5 s for the whole run and recording the largest count. The checker exits 2 with "could not check" when it cannot read `/proc`; no sample returned that.

| Run | Samples | Max members during | Members after |
|---|---|---|---|
| A2A bridge module with PostgreSQL, first run | 111 | 5 | 0 |
| `integration` after the fix | 13 | 7 | 0 |
| A2A bridge module with PostgreSQL, final head | 39 | 7 | 0 |

Subprocess spawners in the module's tests: `pgstore_crossprocess_test.go` (builds and runs `testdata/a2a-testserver`), `internal/state/postgres_migration_concurrency_test.go` and `integration/topology_harness_test.go` (re-run the test binary as a helper), and `integration/runner_script_test.go` (runs `scripts/run-integration-ci.sh`). None calls `Setsid`. `runner_script_test.go` sets `Setpgid`, which starts a new process group in the same session, so its children stay visible. None of the module's scripts or `testdata` sources use `setsid`, `nohup`, `disown` or `&` to put a process in the background (the one `&` in `run-integration-ci.sh` is in a comment). The limit stands: a child that calls `setsid` itself would leave the session unseen.

## Round 2 review notes

A second review approved the head above. Two points were left as they are, with reasons:

- The PostgreSQL CI job runs only `./integration`, so the `./internal/bridge` database tests, including `TestReplyPoller_StandaloneAppendFence`, do not run in CI. This gap predates item 10 and covers every database test in the package. Adding the package to that job is a follow-up, kept out of this change.
- The fence test's skip guard does not read `TEST_REQUIRE_DATABASE`. That matches its package: no test under `internal/` reads that variable, and `testPostgresTaskStore` skips the same way. The fail-closed rule in `integration/README.md` covers the `integration` suite only. If the package adopts the rule, this guard should change with it.

## Review comments on upstream PR 4746

Five inline comments on upstream PR 4746, each triaged against item 10's head `79c2d15783af`. Line numbers are those of the fixed head.

- **C1, `executor.go` reply poll switch reads `e.bridge.config` without a nil check:** declined. `New` dereferences `cfg` when it builds the Bridge (`bridge.go:200`, `bridge.go:208`), and `Execute` already reads `e.bridge.config.Hub.User` on the same path before the switch (`executor.go:146`), so a nil config never reaches it.
- **C2, `reply_poller.go` `pollOnce` uses the anchor from `GetMessage` without a nil check:** fixed. `replyReader` accepts any `hubclient.ConversationService`, so the hub client's non-nil answer cannot be relied on. An answer with neither a message nor an error is now a failed read (`errNoReadResult`, outcome `pollError`), which is retried until the deadline. Covered by `TestReplyPoll_NilReadResultIsRetried/anchor`.
- **C3, `reply_poller.go` `findDirectConversation` uses the list from `List` without a nil check:** fixed the same way: the list read returns `errNoReadResult`. The poll reports `pollError` (retried) rather than "not listed", which would end the call after the send. The preflight takes it as a non-refusal and goes on to the send, as for any other failed read. Covered by `TestReplyPoll_NilReadResultIsRetried/conversation_list` and `/preflight_conversation_list`.
- **C4, `reply_poller.go` `pollOnce` uses the page from `ListMessages` without a nil check:** fixed the same way: a page read with no result is `pollError` (retried), never "anchor not seen". The preflight's own page read discards its result, so it needs no guard. Covered by `TestReplyPoll_NilReadResultIsRetried/message_page`.
- **C5, `bridge.go` `lastTaskEventID` loops forever if a page's last event ID does not pass the cursor:** declined. Both `state.Store` implementations read `id > afterID ORDER BY id ASC` (`internal/state/postgres.go:448-449`, `internal/state/state.go:514-515`), so each non-empty page ends past the cursor. The test wrappers that embed `state.Store` (`async_create_test.go:781`, `reply_poller_bounds_test.go:378`) do not override `ReadTaskEvents`, and the package is internal, so nothing outside the module implements it.

### Review comments gate

Run on `ea39617a753c` (the fixes and the section above; the commit adding this gate changes no code). Every go command ran with a private `GOCACHE` and `GOMODCACHE`, `GOFLAGS=-buildvcs=false`. PostgreSQL 17 from the distro repositories ran as a private trust-auth cluster on a unix socket, with no password.

| Check | Command | Result |
|---|---|---|
| A2A bridge module with PostgreSQL | `TEST_DATABASE_URL=... TEST_REQUIRE_DATABASE=1 go test -count=1 -p 1 -v -timeout 40m ./...` in `extras/scion-a2a-bridge` | pass: 955 pass, 0 fail, 1 skip (`TestPostgresMigrationHelperProcess`, the subprocess helper entry point). cmd, integration, internal/bridge and internal/state ok. The 4 `TestReplyPoller_StandaloneAppendFence` subtests ran and passed, and so did the 4 `TestReplyPoll_NilReadResultIsRetried` subtests |
| go vet | `go vet ./internal/bridge/ ./integration/` | clean |
| gofmt | `gofmt -l ./internal ./integration` | no files |
| golangci-lint | `GOGC=40 golangci-lint run --new-from-rev=origin/main --concurrency=1 ./internal/bridge/... ./integration/...` | 0 issues |

Mutants: each removes one new guard, runs `go test -count=1 -run '^TestReplyPoll_NilReadResultIsRetried$'`, and is reverted. None was committed.

| # | Guard removed | Result |
|---|---|---|
| G1 | `findDirectConversation` nil list (C3) | caught: `conversation_list` subtest, nil dereference |
| G2 | `pollOnce` nil anchor (C2) | caught: `anchor` subtest, nil dereference |
| G3 | `pollOnce` nil page (C4) | caught: `message_page` subtest, nil dereference |

Leftover-process check: the module run ran as `setsid --wait` on a script that records its session ID and execs `go test`. A checker that reads only `/proc/[0-9]*/stat` sampled the session every 5 s: 54 samples, at most 7 members during the run, 0 after `setsid --wait` returned. No sample reported "could not check". The limit stands: a child that calls `setsid` itself leaves the session unseen. No test or helper in the module calls `Setsid`, `nohup` or `disown`, and none starts a process in the background with `&`. `integration/runner_script_test.go` sets `Setpgid`, which stays in the session.
