# Federated project access, item 10: merge forward onto main 60fc1bd

A2A bridge reply polling for visitors, merged forward from merge base `67d812af1c80` onto main `60fc1bda6b2f69f6bfde70c918dd1783952843c2` (856 commits later) and re-gated on the new base.

- Item branch head before the merge: `b22652b158e282f70fb501cc5c100ac2ab8ad1ee`.
- Merged head: see the branch tip. This entry is the last commit and changes no code. The full bridge module run and the mutants ran on the merge plus the test fix; the hub shards, `make ci` and `make build` ran one commit later, which adds only `TestReplyWaitKey_SameCallerOnly` (run on its own in the gate).

## Merge

- The earlier federation items this branch was stacked on are on main already: the visitor agent list and read routes (upstream PR 3908) and federated users in the A2A bridge (upstream PR 4021). Their copies on this branch were dropped in favour of main's. Item 10 adds nothing that main already had.
- Item 10's own change is bridge-only. After the merge, `git diff origin/main...HEAD` touches 22 files: `extras/scion-a2a-bridge/internal/bridge/` (reply poller, executor, bridge, broker, caller, config, stream, and their tests), the bridge README, the sample config, `integration/reply_poll_e2e_test.go`, and `.design/project-log/a2a-bridge-reply-polling.md`. Nothing under `pkg/` or `docs-site/` differs from main.
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

The hub shards ran through `go test` with the `hack/hubshard` overlay rather than `make test-hub-shard`, so each compiled once inside the process-session check; the make target's test-list comparison was not run. No PostgreSQL is available in this environment, so the database-backed bridge tests (those that read `TEST_DATABASE_URL`, including `pgstore_test.go`, `pgstore_crossprocess_test.go` and `TestReplyPoller_StandaloneAppendFence`) skip.

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
| M08 | `reply_poller.go` `mayAppend` | append fence ignores the done flag | `TestReplyPoller_AppendFence` | caught |
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
- The database-backed bridge tests remain unrun here; a run with `TEST_DATABASE_URL` is still needed for full coverage.
