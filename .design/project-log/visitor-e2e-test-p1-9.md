# End-to-end test of project visitors across two hubs (phase 1, item 9)

**Date:** 2026-10-18
**Stacked on:** the federated user reply reading item (the bridge's
visitor reply poller), which builds on the A2A bridge item for federated
users
**Tracked in:** upstream issue 4603

## What changed

- **New test:** `TestFederatedVisitorEndToEnd` in
  `extras/scion-a2a-bridge/integration/federated_visitor_e2e_test.go`. It
  uses only test code and exported pkg/hub and bridge API. No production
  file changed.
- **Topology, all in one process:**
  - Hub A runs with its OIDC provider on and `user_token_audiences` set to
    hub B.
  - Hub B trusts hub A as an `issuer_type: hub` issuer with
    `accept_user_tokens` and `allow_visitor_grants`. Hub B runs in `dev`
    mode, because the federation authenticator accepts an `http` issuer
    URL (hub A's loopback listener) only in `dev` and `workstation` modes.
  - Both hubs have the `federation.project_visitors` experiment on, run on
    SQLite stores, and listen on `httptest` loopback ports. The test sets
    `HOME` to a temporary directory, so hub B's project directory
    (`$HOME/.scion/projects/<slug>`) is never created in the real home,
    and clears inherited `SCION_SERVER_*` and `SCION_SEED_*` variables.
  - **Operational settings:** `fvStartHub` installs DB-backed operational
    settings on each hub, built as `cmd/server_foreground.go`
    `initOperationalSettings` builds them (`NewOperationalSettings`,
    `Refresh`, `SetOperationalSettings`), without the snapshot apply, so
    the federation configuration the test sets stays in force. `hub.New`
    leaves them unset, and without them the conversation envelope switch
    reads as off. Step 3 depends on that switch (see "Agent replies").
  - The A2A bridge (`Auth.Scheme: federation`) sits in front of hub B. It
    is a spoke of hub B's fan-out event bus, with the broker proxy as its
    host callbacks, the same wiring `cmd/server_foreground.go` uses for a
    broker plugin.
  - The client is plain JSON-RPC, decoded with a2a-go types.
- **The agent:** hub B's dispatcher is a test type. It records each
  delivered message and answers an error for every other dispatch. The
  test replies as the agent through `POST
  /api/v1/agents/{id}/outbound-message`, with an agent credential that
  hub B signed and recorded.
- **Steps:**
  1. Alice obtains a user access credential from hub A with a session
     credential.
  2. Hub B's owner grants her by email. The grant is pending.
  3. Her first task through the bridge is delivered and completes with the
     agent's reply. The grant is then active, and one `visitor_linked`
     audit record exists. Her DM with the agent, read the way the bridge
     reads it (the federation header carries her credential), holds the
     reply; the same read with only the bridge's bearer credential answers
     404. The agent row does not name her account, she holds no direct
     project role other than her visitor grant, and the hub's membership
     rule (`CheckEffectiveMembership`, which also counts group-derived
     bindings) finds her a non-member. The creator's owner role and
     membership are the positive control. The bridge lists the agent's
     card.
  4. With `accept_user_tokens` off (applied through `ApplySnapshot`), the
     bridge answers HTTP 401 and nothing is delivered. Turned back on, the
     next task completes, and the grant is unchanged.
  5. After the grant is revoked, the next task is refused, and her DM
     read answers 403.
  6. A visitor rule for `partner.example` admits Bob on his first request,
     bound by the rule. After the rule is removed, his next task is
     refused.
- **Uniform refusal:** the refusals in steps 5 and 6 must be identical, in
  task state and message text, to the answer for an agent the bridge does
  not expose. That answer is taken in the same run. After every refusal
  the test also checks hub B's store for the refused text, which does not
  depend on delivery timing.
- **Waits:** every wait is a `select` on the delivery channel or the A2A
  answer, bounded by a 60 s context. There are no sleeps.

## Notes

- **CI:** the test runs in the Extras Modules CI matrix (`go test ./...`
  for the bridge module) and in the A2A integration job (`-v`, `-race`,
  `-count=3`). The test never skips, because that job fails on skipped
  tests. It is not added to `testdata/acceptance_layers.json`, the census
  of the GE suite.
- **Fan-out routing:** the fan-out bus sends a message **without** a
  channel to every spoke. A message **with** a channel goes only to the
  in-process bus and to the spoke whose routing key matches the channel
  (`pkg/eventbus/fanout.go`). The bridge spoke has no `ChannelID`.
- **Subscriptions:** the bridge's `BrokerServer.Publish` does not filter
  by subscription, and neither does the plugin RPC adapter. For messages
  that reach the bridge spoke, its topic subscriptions are therefore
  bookkeeping, and a test cannot observe a missing subscription.
- **Agent replies:** an agent's direct reply to a user always carries a
  channel. With no channel in the request and no reply affinity, hub B
  files it under `web`, so the reply never reaches the bridge spoke. The
  bridge's visitor reply poller reads it instead from the visitor's own DM
  history (`GET /api/v1/chat/conversations/<dm key>/messages`, with the
  user access credential in the federation header). That read needs the
  conversation envelope switch, which is why the test installs the
  operational settings. Step 3 passes; with the settings install removed,
  it ends on the bridge's send timeout.
- **Runs:** the test has been compiled and run in a scheduled build
  window: all steps pass, and the run without the operational settings
  install fails at step 3. CI on the pull request is the evidence for the
  full suite.
  Superseded by `fed-p1-9-merge-forward-6cff673.md`, which records the
  merge forward and the gate on the current base.
