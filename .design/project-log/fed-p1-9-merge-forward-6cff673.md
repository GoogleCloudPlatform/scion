# Federated project access, item 9: merge forward onto main 6cff673

Item 9 (`TestFederatedVisitorEndToEnd`, the end-to-end test of project visitors across two hubs) merged forward onto the new 10V head and main, and re-gated on that base. The first merge was onto main `8ed09b1` (sections "Merge" to "Leftover-process check"). The second, onto 10V `aad2bdb2` and main `6cff6732`, is under "Merge onto item 10 on main" at the end. Earlier evidence for item 9, gathered on the old 10V head `db6de1e185e5`, is superseded by this entry.

- Item 9 head before the merge: `2f32e457d5628a2e59656a6b78188b5ca4e0bc5d`.
- 10V merged: `ecec559cc432b7bc056d7be770f1c62e017f0077` (on main `9e87b08984a6`, containing item 10 `2a335a1e`).
- Main merged: `8ed09b1142ec62d349b2b2a522fc6fc1cb77ae7e`.
- Merged head: see the branch tip. This entry is the last commit and changes no code. The first gate ran on `caec903a1a4f` (window 1) and `7d08c54507d5` (window 2), as listed under "Gate". The gate on the current base ran on `b8b1ae6a052f` (window 3), as listed under "Merge onto item 10 on main".

## Merge

Two merges, in order, each with `git merge --no-edit`: 10V `ecec559c`, then main `8ed09b11`.

- **10V `ecec559c`:** no conflicts. Item 9 changes only its test file and its project log, and 10V touches neither.
- **Main `8ed09b11`:** no conflicts. Main is one commit past 10V's base (upstream PR 4740). That commit changes `pkg/hub/project_workspace_*` and one docs-site reference page, and nothing under `extras/scion-a2a-bridge`.
- `git diff origin/main...HEAD` touches only `extras/scion-a2a-bridge/` and `.design/project-log/`. Nothing under `pkg/`, so no hub shards were run.
- `git diff origin/scion/mmr-fed-p1-10v...HEAD` holds item 9's two files, main's 7 files from upstream PR 4740, and this entry.

## Test adaptations

- **Session credential.** Upstream PR 4741 (already in 10V's base) removed `UserTokenService.GenerateAccessToken`. `fvHubAUser` now gets hub A's session credential from `MintAccessToken(u.ID, u.Email, u.DisplayName, u.Role, u.SessionGeneration, hub.ClientTypeWeb)`, as the reply-poll end-to-end helper does. Without this change the test does not compile.
- **allowed_domains on the federation-token path** (`pkg/hub/federation_auth.go`, upstream PR 4484). The check applies only when the issuer entry lists `allowed_domains`, and an empty list adds no constraint. Hub B's trust entry for hub A (`fvPeerIssuer`) lists none, so neither alice@corp.example nor bob@partner.example is affected. The test configuration is unchanged and so is the production check.
- **Reply polling on by default (10V).** The test already installs DB-backed operational settings in `fvStartHub`, so the conversation envelope switch is on and the bridge's visitor reply poller reads the reply from the visitor's own DM. No change was needed. Steps 3, 4 and 6 complete with the agent's reply through that path.
- **errcheck.** The scoped golangci-lint run flagged the two deferred `resp.Body.Close()` calls in the test (lines 328 and 456). Both now read `defer func() { _ = resp.Body.Close() }()`, as `reply_poll_e2e_test.go` does.
- **Issue reference.** The test's header comment and item 9's log line now read "upstream issue 4603".

No production file changed.

## Gate

Every go command ran with a private `GOCACHE` and `GOMODCACHE` and `GOFLAGS=-buildvcs=false`. PostgreSQL 17 from the distro repositories ran as a private trust-auth cluster on a unix socket, with no password. Its URL went in as `TEST_DATABASE_URL`, together with `TEST_REQUIRE_DATABASE=1`, as prefix assignments on the module run only. The cluster was stopped after window 1. All commands ran in `extras/scion-a2a-bridge`.

Window 1 ran on `caec903a1a4f`. The test comment's issue reference was edited in the working tree during the window (comment only). Window 2 ran on `7d08c54507d5`, which adds the errcheck change.

| Check | Command | Result |
|---|---|---|
| go vet | `go vet ./...` | clean |
| gofmt | `gofmt -l .` | no files |
| e2e, repeated | `go test -count=3 -v -run '^TestFederatedVisitorEndToEnd$' ./integration/` | pass, 3 of 3 |
| e2e, race | `go test -race -count=1 -v -run '^TestFederatedVisitorEndToEnd$' ./integration/` | pass |
| A2A bridge module with PostgreSQL | `go test -count=1 -p 1 -v ./...` | pass: 982 PASS lines (562 top-level tests, the rest subtests), 0 fail, 1 skip (`TestPostgresMigrationHelperProcess`: "migration helper only", the subprocess helper entry point). cmd, integration, internal/bridge and internal/state ok. `TestFederatedVisitorEndToEnd` passed in this run |
| golangci-lint, window 1 | `GOGC=40 golangci-lint run --new-from-rev=origin/main --concurrency=1 ./internal/bridge/... ./integration/...` | 2 issues (errcheck, the two lines above) |
| e2e after the errcheck change, window 2 | `go test -count=1 -v -run '^TestFederatedVisitorEndToEnd$' ./integration/` | pass |
| golangci-lint, window 2 | the command above | 0 issues |

The errcheck change touches two `defer` lines in the test only, so the module run was not repeated for it.

### Mutants

Item 9 is a test, so the mutants change the production behaviour it asserts. Each one is a one-line change in the merged code. Each was applied, followed by one `TestFederatedVisitorEndToEnd` run, then reverted. None was committed. Mutants in `pkg/hub` and `pkg/store` were built through the bridge module's test build.

| # | Site | Change | Result |
|---|---|---|---|
| M01 | `linkPendingVisitor` (`pkg/hub/federation_visitor.go`) | the grant by email is never linked on first use (`pending == nil && len(rules) == 0` becomes `len(rules) == 0`) | caught: step 3, "the bridge answered before hub B delivered the message to the agent: HTTP 403" |
| M02 | `linkPendingVisitor` | no `visitor_linked` audit record written | caught: "link audit records for alice's account: 0, want 1" |
| M03 | `Bridge.federatedUserGet` (`extras/scion-a2a-bridge/internal/bridge/federated_user.go`) | requests sent with the bridge's own credential only, without the federation header carrying the user's credential | caught: step 3, HTTP 401 "credential rejected by Scion Hub". This function also serves the bridge's credential check, so the run fails there, before any DM read |
| M04 | `handleConversationHistory` (`pkg/hub/handlers_chat_v2.go`) | DM participant check skipped | caught: "DM read without the federation header: status 200, want 404" |
| M05 | `extractHubTokenIdentity` (`pkg/hub/federation_auth.go`) | `accept_user_tokens` off ignored | caught: step 4, "hub B delivered "hello while off" ... want no delivery" |
| M06 | `ProjectMembershipService.RemoveVisitor` | revoke keeps the visitor's role binding | caught: step 5, "hub B delivered "hello after revoke" ... want no delivery" |
| M07 | `checkEffectiveMembership` (`pkg/hub/authorize_message.go`) | a role that confers no membership (project-visitor) counted as membership | caught: "membership {IsMember:true ...}, want a non-member" |
| M08 | `CompositeStore.DeleteProjectVisitorRule` (`pkg/store/entadapter`) | rule removal keeps the bindings the rule created | caught: step 6, "hub B delivered "hello after rule removal" ... want no delivery" |
| M09 | `userSenderAddress` (`pkg/hub/federation_visitor_messaging.go`) | a visitor's sender address names the email, not the account | caught: "envelope sender "user:alice@corp.example", want "user:<account id>"" |
| M10 | `admitVisitorSubRoute` (`pkg/hub/federation_visitor_messaging.go`) | the visitor sub-route message check ignores the refusal | caught: "DM read after revoke: status 200, want 403" |
| M11 | `activeVisitorRulesForEmail` (`pkg/hub/federation_visitor.go`) | visitor rules never match | caught: step 6, "domain grant: ... HTTP 403". The first version of this patch did not compile (missing return); the buildable version was run in the same window |

Not mutated: "the agent row names the visitor's account". The test creates the agent row itself through the store (`fvNewEnv`), and no production code on the test's path writes `OwnerID`, `CreatedBy` or `Ancestry`. The assertion guards against a future change that does. No single production line the test exercises can make it fail today.

### Leftover-process check

The repeated e2e run and the module run each ran as `SIDFILE=<file> setsid --wait run-in-session.sh <go test args>`. The script writes `$$` (the new session ID) to the file and execs `go test`. A checker built outside the repository reads only `/proc/[0-9]*/stat`, takes field 6 (the session) counted after the last `)`, and lists the processes in that session. It runs outside the session. When it cannot read `/proc` it exits 2 with "could not check" and never reports clean. It sampled every 5 s and ran once more after `setsid --wait` returned.

| Run | Samples during | Max members during | Members after |
|---|---|---|---|
| `TestFederatedVisitorEndToEnd -count=3` | 15 | 3 | 0 |
| A2A bridge module with PostgreSQL | 46 | 7 | 0 |

No sample reported "could not check". A search of item 9's test and its helpers (all in `federated_visitor_e2e_test.go`) for `Setsid`, `setsid`, `nohup`, `disown` and a background `&` finds nothing. The file starts no process; its one goroutine (`fvSendAsync`) runs in the test process. Across the module the only related call is `Setpgid` in `integration/runner_script_test.go`, which starts a new process group inside the same session, so the check still sees it. Limit: a child that calls `setsid` itself leaves the session and is not seen.

## Notes

- No hub shards: the diff against main stays under `extras/scion-a2a-bridge/` and `.design/project-log/`.
- `make ci` was not run. The diff touches no root-module code, and the root lint and tests do not cover the bridge module.

## Merge onto item 10 on main

Item 10 landed on main as `6cff6732222a` (upstream PR 4746, squashed), after upstream PR 4743. 10V was merged onto that main: `origin/scion/mmr-fed-p1-10v` is now `aad2bdb235d23e628d248becc234b85f50426fc7`, with parents `ecec559c` and main `6cff6732`.

- Merged into the item 9 head `ce5226aa4a14` with `git merge --no-edit`: 10V `aad2bdb2`. No conflicts. Main was at `6cff6732222a02aba081583e17b2ef9db06e33f3`, already contained through 10V, so no separate main merge was needed. Merged head `b8b1ae6a052f`.
- `git diff origin/scion/mmr-fed-p1-10v...HEAD` holds item 9's two files and this entry only.
- `git diff origin/main...HEAD` touches only `extras/scion-a2a-bridge/` and `.design/project-log/`. Besides item 9's files and logs it still holds 10V's bridge files (`README.md`, `executor.go`, `federated_user.go`, `reply_poller.go`, `reply_poller_visitor.go` and their tests), because 10V is not on main yet.
- No test change was needed.
- Mutated production sites: the merge changed none of them. Of the eight files the mutants touch, only `pkg/hub/handlers_chat_v2.go` changed (upstream PR 4743 adds two fields to `chatHistoryResponse`, far from the DM participant check). All 11 mutant patches still apply unchanged. The mutants were not re-run.

### Gate on main 6cff673

Window 3, on `b8b1ae6a052f`, with the same private caches, PostgreSQL setup and leak check as above. PostgreSQL was stopped at the end of the window.

| Check | Command | Result |
|---|---|---|
| e2e, repeated, in the leak check | `go test -count=3 -v -run '^TestFederatedVisitorEndToEnd$' ./integration/` | pass, 3 of 3 |
| go vet | `go vet ./integration/ ./internal/bridge/` | clean |
| gofmt | `gofmt -l .` | no files |
| golangci-lint | `GOGC=40 golangci-lint run --new-from-rev=origin/main --concurrency=1 ./internal/bridge/... ./integration/...` | 0 issues |
| A2A bridge module with PostgreSQL, in the leak check | `go test -count=1 -p 1 -v ./...` | pass: 982 PASS lines (562 top-level tests), 0 fail, 1 skip (`TestPostgresMigrationHelperProcess`, "migration helper only"). cmd, integration, internal/bridge and internal/state ok. `TestFederatedVisitorEndToEnd` passed in this run |

| Run | Samples during | Max members during | Members after |
|---|---|---|---|
| `TestFederatedVisitorEndToEnd -count=3` | 33 | 3 | 0 |
| A2A bridge module with PostgreSQL | 47 | 7 | 0 |

No sample reported "could not check".
