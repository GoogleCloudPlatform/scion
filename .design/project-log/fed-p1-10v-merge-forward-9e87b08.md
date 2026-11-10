# Federated project access, item 10V: merge forward onto main 9e87b08

Item 10V (the A2A bridge reads a federated user's reply from that user's own direct conversation with the agent, with the user's own credential) merged forward onto the new item 10 head and main, and re-gated on that base. Earlier evidence for 10V, gathered on item 10 head `b22652b158e2`, is superseded by this entry.

- 10V head before the merge: `db6de1e185e532c65674457024a6ccdeaf9a81e6`.
- Item 10 merged: `2a335a1e0e16d7de7f3bcb0a7cb6c6454ad5ce0f` (after a first merge of `79c2d15783af`; item 10 then took the review comment fixes on upstream PR 4746).
- Main merged: `9e87b08984a635838b804d4b364fb2a7337e5764` (after a first merge of `7bab5c1a2acf`).
- Merged head: see the branch tip. This entry is the last commit and changes no code. The gate ran on `b0f734cf2528`; the one test commit after it ran as listed under "Gate".

## Merge

Merges, in order, each with `git merge --no-edit`: item 10 `79c2d157`, main `7bab5c1a`, item 10 `2a335a1e`, main `9e87b089`.

- **Item 10 `79c2d157`:** one conflict, in `federated_user_test.go`. Both sides added fields to the hub test double: 10V added `dmReadQueries`, `DMReadsBefore` and the paged answer, and item 10 (with main's launch wait) added `order`, `agentGets` and `knownUser`. Both sets were kept. The access read now records its query, its place in the request order, and accepts any known visitor credential.
- **Main `7bab5c1a`:** no conflicts. Main changed nothing under `extras/scion-a2a-bridge` since item 10's base.
- **Item 10 `2a335a1e`:** one conflict, in `reply_poller.go`. Item 10 added a nil-page guard to `pollOnce`'s page loop, and 10V had moved that loop into `scanPages` with a fetch function per reader. The guard now sits in the local caller's fetch closure and returns `errNoReadResult`. `scanPages` maps that error with `readOutcome` to `pollError`, the same outcome item 10's guard gives.
- **Main `9e87b089`:** no conflicts. Again nothing under the bridge changed.
- `git diff origin/main...HEAD` touches only `extras/scion-a2a-bridge/` and `.design/project-log/`. Nothing under `pkg/`, so no hub shards were run.
- `git diff origin/scion/mmr-fed-p1-10...HEAD` holds 10V's 8 files plus main's changes since item 10's base (outside the bridge) and this entry.

### 10V's diff before and after

10V's own diff (old item 10 head to 10V head) compared file by file with the merged diff (new item 10 head to merged head), on changed lines only:

| File | Difference |
|---|---|
| `executor.go`, `federated_user.go`, `reply_routing_visitor_test.go` | none |
| `reply_poller.go` | item 10's nil-page guard moved into the fetch closure (above) |
| `reply_poller_visitor.go` | the null page handling (below) |
| `federated_user_test.go` | snapshot field alignment; the hub double fields merged above; `dmNullReads`; the launch wait test changes (below) |
| `reply_poll_visitor_test.go` | new tests `TestReplyPoll_VisitorNullPageIsRetried` and `TestReplyPoll_VisitorAgentKeyNotDerivedSendsNothing` |
| `a2a-bridge-federated-user-reply-poll.md` | "Tracked in" now reads "upstream issue 4603" |

## Changes in this merge

- **Launch wait tests.** Main's launch wait (asynchronous create, upstream PR 3801), merged into item 10, brought `TestExecutor_FederatedUserLaunchingAgentAllowedWaits` and `TestExecutor_FederatedUsersLaunchingAgentTasksKeptPerAccount`. Both assume a federated user waits on the push path: one access read before the send and a pushed reply. With 10V a federated user polls its own conversation, so both failed on the first merged head (time out waiting for a pushed reply). They now run with `fedUserPushOnly`, as 10V's other push path tests do. A new test, `TestExecutor_FederatedUserLaunchingAgentAllowedWaitsPolled`, covers the polled case: the conversation read before the launch wait, the preflight read after the wait and before the send, every read with the caller's key and credential, and the reply read from the conversation. The executor code is unchanged: item 10's launch check calls `recheckFederatedUserAccess`, which under 10V is the visitor reader's `check`.
- **Read with no result.** Item 10 now takes a conversation read that returns neither a result nor an error as a failed read (`errNoReadResult`, `pollError`, retried). The visitor reader decodes the hub's answer itself (`reply_poller_visitor.go`, `page`). An empty body already fails to decode and is `pollError`, but a 200 answer of JSON `null` decoded without error into an empty page, read as "anchor not seen". `page` now decodes into a pointer and returns `errNoReadResult` for `null`, which `visitorReadOutcome` maps to `pollError`, never to the refusal. A page with no messages is still an empty page, the hub's answer for a conversation with no messages. The access read (`check`) decodes nothing and is unchanged. Covered by `TestReplyPoll_VisitorNullPageIsRetried`.
- **Agent ID that gives no key.** The executor refuses a federated user whose conversation key cannot be derived before anything is sent. No test reached that refusal: the caller's account ID is checked earlier, so only a non-canonical agent ID from the hub gets there. `TestReplyPoll_VisitorAgentKeyNotDerivedSendsNothing` covers it (see V15).

## Gate

Every go command ran with a private `GOCACHE` and `GOMODCACHE` and `GOFLAGS=-buildvcs=false`. PostgreSQL 17 from the distro repositories ran as a private trust-auth cluster on a unix socket, with no password, passed as `TEST_DATABASE_URL` with `TEST_REQUIRE_DATABASE=1`.

| Check | Command | Result |
|---|---|---|
| A2A bridge module with PostgreSQL | `go test -count=1 -p 1 -v -timeout 40m ./...` in `extras/scion-a2a-bridge` | pass: 979 pass, 0 fail, 1 skip (`TestPostgresMigrationHelperProcess`, the subprocess helper entry point, which skips unless started as a helper). cmd, integration, internal/bridge and internal/state ok |
| Launch wait tests | from the same `-v` output | `TestExecutor_FederatedUserLaunchingAgentAllowedWaits`, `...AllowedWaitsPolled` and `TestExecutor_FederatedUsersLaunchingAgentTasksKeptPerAccount` pass |
| Item 10 database and guard tests | from the same `-v` output | the 4 `TestReplyPoller_StandaloneAppendFence` subtests and the 4 `TestReplyPoll_NilReadResultIsRetried` subtests ran and passed |
| go vet | `go vet ./internal/bridge/ ./integration/` | clean |
| gofmt | `gofmt -l ./internal ./integration` | no files |
| golangci-lint | `GOGC=40 golangci-lint run --new-from-rev=origin/main --concurrency=1 ./internal/bridge/... ./integration/...` | 0 issues |
| First merged head (before the launch test fix) | the module command above | 970 pass, 2 fail (the two launch wait tests above), 1 skip; superseded |
| New key test, after the module run | `go test -count=1 -v -run '^(TestReplyPoll_VisitorAgentKeyNotDerivedSendsNothing\|TestVisitorDMReader_KeyNotDerivedIsRefusal\|TestReplyPoll_Visitor.*)$' ./internal/bridge/`, then `go vet` and the scoped `golangci-lint` on that package | 14 pass, 0 fail; vet clean; 0 issues. The commit that adds `TestReplyPoll_VisitorAgentKeyNotDerivedSendsNothing` changes tests only, so the module run above was not repeated for it |

### Mutants

Fresh one-site changes at 10V's decisions in the merged code. Each was applied, run against the narrowest tests, and reverted; none was committed. None of these tests needs PostgreSQL.

| # | Site | Change | Tests run | Result |
|---|---|---|---|---|
| V01 | `newVisitorDMReader` | key derived from a different account ID | `TestReplyPoll_VisitorReturnsReplyFromOwnConversation`, `TestExecutor_FederatedUserPollsOwnConversationWithPollingOn` | caught (both) |
| V02 | `newVisitorDMReader` | no refusal when no key can be derived | `TestVisitorDMReader_KeyNotDerivedIsRefusal`, `TestFederatedUser*` | caught: `TestVisitorDMReader_KeyNotDerivedIsRefusal` |
| V03 | `visitorDMReader.page` | page read without the caller's credential | `TestReplyPoll_Visitor*` | caught: 10 tests, among them `...ReturnsReplyFromOwnConversation`, `...UnavailableReadIsRetried` |
| V04 | `visitorReadOutcome` | 401/403/404 retried instead of the uniform refusal | `TestReplyPoll_Visitor*` | caught: `...PreflightRefusalSendsNothing`, `...ReadRefusalEndsCall` |
| V05 | `visitorReadOutcome` | other failures taken as the refusal | `TestReplyPoll_Visitor*` | caught: `...UnavailableReadIsRetried`, `...PreflightUnavailableProceeds`, `...NullPageIsRetried` |
| V06 | `visitorDMReader.page` | cursor not forwarded | `TestReplyPoll_Visitor*` | caught: `...FollowsPageCursor`, `...TooManyPages` |
| V07 | `scanPages` | page cap `<` to `<=` | `TestReplyPoll_Visitor*` | caught: `...TooManyPages` |
| V08 | `visitorDMReader.check` | access read takes a full page, not one message | `TestReplyPoll_Visitor*`, `TestFederatedUser*`, `TestExecutor_FederatedUser*` | caught: `...ReturnsReplyFromOwnConversation` and 2 more |
| V09 | `visitorReplyPreflight` | preflight refusal does not end the call | `TestReplyPoll_Visitor*` | caught: `...PreflightRefusalSendsNothing` |
| V10 | `recheckFederatedUserAccess` | re-check before forwarding skipped | `TestReplyPoll_Visitor*`, `TestFederatedUser*` | caught: `...RevokedBeforeForwardStopsReply`, `...ReturnsReplyFromOwnConversation` |
| V11 | executor poller construction | visitor poller built only when a local reader also exists | `TestReplyPoll_Visitor*`, `TestExecutor_FederatedUser*`, `TestExecutor_MemberPollerAndFederatedUserPollerOnOneAgent` | caught: the test binary panics on the nil local reader |
| V12 | executor wait predicate | `pollOnlyEvent` not set for a federated user | `TestReplyPoll_VisitorIgnoresPushedReply`, `TestExecutor_FederatedUserPolledWaitIgnoresAppendedPushEvent` | caught: `TestExecutor_FederatedUserPolledWaitIgnoresAppendedPushEvent` |
| V13 | executor `polling` | visitor reader left out of `polling` | `TestReplyPoll_Visitor*`, `TestExecutor_FederatedUser*`, `TestExecutor_MemberPollerAndFederatedUserPollerOnOneAgent` | caught: 15 tests, among them `TestExecutor_FederatedUserLaunchingAgentAllowedWaitsPolled` |
| V14 | `pollOutcome.terminalMessage` | `pollRefused` not terminal | `TestReplyPoll_Visitor*` | caught: `...PreflightRefusalSendsNothing` and more |
| V15 | executor | no refusal when the reader cannot be built (no key) | `TestVisitorDMReader_KeyNotDerivedIsRefusal`, `TestFederatedUser*`, `TestExecutor_FederatedUser*`, `TestReplyPoll_Visitor*` | survived the first run (no test reached the refusal); then caught by the new `TestReplyPoll_VisitorAgentKeyNotDerivedSendsNothing`: "1 sends reached the hub, want 0" |
| V16 | `visitorDMReader.page` | null page taken as an empty page | `TestReplyPoll_VisitorNullPageIsRetried` | caught |
| V17 | local fetch closure (merge resolution) | nil page guard removed | `TestReplyPoll_NilReadResultIsRetried` | caught: `message_page` subtest |

### Leftover-process check

The module run ran as `SIDFILE=<file> setsid --wait run-in-session.sh <go test args>`. The script records `$$` (the new session ID) and execs `go test`. A checker built outside the repository reads only `/proc/[0-9]*/stat`, takes field 6 (the session) counted after the last `)`, and lists the processes in that session. It runs outside the session, and it exits 2 with "could not check" when it cannot read `/proc`. It sampled every 5 s.

| Run | Samples | Max members during | Members after |
|---|---|---|---|
| A2A bridge module with PostgreSQL, gated head | 60 | 7 | 0 |
| A2A bridge module with PostgreSQL, first merged head | 135 | 7 | 0 |

No sample reported "could not check". Subprocess spawners in the module's tests: `pgstore_crossprocess_test.go` (builds and runs `testdata/a2a-testserver`), `internal/state/postgres_migration_concurrency_test.go` and `integration/topology_harness_test.go` (re-run the test binary as a helper), and `integration/runner_script_test.go` (runs `scripts/run-integration-ci.sh`, with `Setpgid`, a new process group in the same session, so it stays visible). A search of the module's Go sources, scripts and `testdata` for `Setsid`, `setsid`, `nohup`, `disown` and a background `&` finds only that `Setpgid` and an `&` inside a comment in `run-integration-ci.sh`. Limit: a child that calls `setsid` itself leaves the session and is not seen.

## Notes

- No hub shards: the diff against main stays under `extras/scion-a2a-bridge/` and `.design/project-log/`.
- `make ci` was not run. The diff touches no root-module code, and the root lint and tests do not cover the bridge module.
