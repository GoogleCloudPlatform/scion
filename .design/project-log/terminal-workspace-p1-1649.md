# P1.4 / #1649: retained terminal metadata and SSE aggregation

Implemented against accepted base `0704d3b89b3842d4e1377fd2a96f15d1dae789e3`.

## Ownership and contract

`TerminalSessionRegistry.metadata` owns a separate `TerminalMetadata` map for the
registry's authenticated Hub origin/base path/account. Registry `open` retains one
entry synchronously; repeated opens preserve the original initializer and do not
change metadata ownership. Session close releases its entry even if renderer
cleanup throws. The existing coordinator close-loop therefore releases the last
subscription without needing a new lifecycle hook. Additive `registry.dispose()`
closes every session, disposes metadata, then reports aggregate cleanup errors;
it permanently rejects further opens. Retained route/mode changes never call it.

Consumers use `metadata.get(agentUUID)`,
`metadata.subscribe(agentUUID, listener)` (immediate notification, unsubscribe),
and `metadata.refresh(agentUUID): Promise<void>`. Values contain an `agent`
snapshot, `availability` (`loading`, `ready`, `deleted`, `unavailable`) and `error`.
Here `ready` means synchronized metadata, not a successful PTY attach or running
agent. `TerminalSession.state.connection` remains the transport authority;
`state.agent` is its attach-time metadata, not the live workspace view. Metadata
lifetime hooks `retain`, `release`, `seed`, `dispose` are for the registry owner.

The pane subscribes internally when `pane.open(registry, agentUUID)` succeeds.
Metadata-fetch errors remain warnings and cannot remove the host during an
independently authorized PTY setup. Capture-auth refreshes this same map; transport/resize notifications never copy
attach-time metadata back over it. Name, phase/activity and exposed ports update
from the map; deleted/unavailable entries keep identity and display the metadata
error. Port links are suppressed while unavailable. A metadata retry is available
without reconnecting an otherwise healthy PTY. Existing pane duplicate/rebind
rejection, visibility and resource identity contracts are preserved.

## Ordering, batching and failures

Union changes coalesce in a microtask. One SSE client serves ordinary workspace
sizes; encoded query strings are batched at 1800 characters (about 35 UUID subjects)
for request-line/proxy headroom. This is not a session limit or eviction policy.
The inspected Hub caps individual patterns at 256 characters and has no
subject-count limit. Workspace subscriptions never call route `state.setScope`.

Initial/reconnect authoritative snapshots start only after the SSE client reports
browser `onopen`. Up to four metadata GETs run concurrently across batches;
deltas are compacted from readiness, including for entries waiting in the queue,
and overlaid on each snapshot. Deletion wins over an in-flight snapshot. The
separate PTY attach metadata GET still serves availability/preflight and may seed
a provisional `loading` view before SSE opens; it cannot replace an existing
central value. Old epochs, removed entries and aborted requests cannot publish.

A failed handshake performs bounded diagnostics once per failed batch lifetime.
Only HTTP 403/404 exclude subjects; their sessions remain labeled in the map and
registry. Accessible neighbors then reconnect and take authoritative snapshots
only after genuine stream readiness. Network/5xx failures remain in the union;
401 is labeled authentication expiry. A readable GET with persistently rejected
SSE is not proof of subscription authorization: that batch stops and exposes an
explicit retry state. `refresh` on a disconnected/excluded entry retries its
subscription, including reconsidering exclusion. No attach retry/input replay,
idle eviction, cap, or live permission-revocation feature was added.

SSEClient keeps default route/Chat endpoint behavior and adds an optional trusted
endpoint, scoped auth probe, a failed-handshake notification, and immediate gap
notification on server-directed reconnect. Stale auth-probe results cannot redirect
after disconnect. Existing clients need no changes.

## Required backend dependencies

Inspection found `handleSSE` flushing headers before `Subscribe`, so browser open
alone is not a readiness guarantee at the pinned base. Manager assigned #1671 to
move subscription registration before the first flush with a regression test.
Also, the pinned `authorizeSSESubjects` authenticates but checks only project/user
subjects; exact and wildcard agent selectors bypass resource authorization. This
is an explicit feature prerequisite (#1672), not a client-enforced security claim.
The manager assigned both backend changes and their reviews separately. This leaf
changes no Hub files. Integration acceptance requires both fixes and security
review. Exact source evidence is delivered in the shared developer report assets.

## P1.5 handoff

No coordinator/pane binding bridge was added. `pane.open` still creates and binds
its session; a coordinator-created entry cannot subsequently be bound by calling
it. P1.5 must choose one authoritative creation layer, either exposing an explicit
owner-only pane binding/initializer contract or making coordinator creation delegate
to pane creation. Repeated coordinator selections must reuse identical ownership.
Do not bypass the duplicate-pane guard, pre-open outside the coordinator's ownership
gate, or create another registry. Prove first/repeated pending/connected opens use
one initializer, pane and attach, including cancellation during initialization.
Metadata requires no additional SSE client in that bridge: the existing registry
open/close path owns it automatically, and the pane consumes it internally.

## Verification and limits

Focused tests cover retained unions, route-scope independence, microtask coalescing,
readiness and queued snapshot buffering, missed-event reconciliation, URL batching,
last-session/account teardown, stale responses, capture refresh precedence, mixed
403/404 neighbors, 401/5xx failures, diagnostic cancellation and explicit retry.
Two sequential production panes verify one live aggregate source and status
preservation through resize; the unit runner returned a real xterm module for a
second concurrent dynamic import, so concurrent union behavior is tested at the
registry/metadata boundary instead. Four headless Chromium production-pane cases
pass with real xterm/addons/Shoelace and fake network/clipboard boundaries, including
capture-auth and uploads. No real Hub, active agent, desktop focus or backend auth
runtime result is claimed here.

Production/type/build and supplemental typed checks are recorded with exact
commands in `reports/p1-1649-developer.md` on the external shared volume. Root lint
is not green (809 errors / 2100 warnings); exact-base comparison proves the same
four SSE-client and five pane production errors. The new metadata test adds the
known root test/parser-project mismatch; supplemental typed checks cover it.
Cumulative `make ci-full` remains the manager/integration owner's gate. Headed
focus remains user-deferred to #1662 and lifecycle freeze to P3.3.

Delivery is a verified Git bundle plus manifest/report on the externally mounted
scratchpad, as explicitly required by the brief; no remote push, main rebase,
backend change, child agent or sibling implementation is authorized here.
