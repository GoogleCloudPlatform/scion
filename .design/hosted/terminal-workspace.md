# Persistent terminal workspace

Status: Proposed. This document scopes implementation; it does not enable the feature.
Date: 2026-09-19. Code inspected at `080ff7c`.
Revised after design discussion: authorization on attach is sufficient; no
product session cap or memory budget is required. Accepted interaction decisions:
first terminal tab owns the hub singleton, browser activation is best effort with
no extension, terminal opens use the single-pane layout, multi-pane placements
are protected, and rail-to-pane placement plus graph/chat header actions are in scope.

## Recommendation

Add **Terminals** as a peer mode to Dashboard and Chat. Designate one owner tab
for a hub/account within a browser profile. Opening an agent terminal from any
participating tab requests one retained session in that owner. Subsequent opens
select that session and request activation of its tab/window. Moving to another
mode, hiding a pane, or changing the layout keeps
its WebSocket, xterm instance, parsed screen, and scrollback alive. Explicitly
closing a session releases its attach; the agent continues running.

Use a session list on the left and four fixed layouts on the right: single pane,
two side by side, two stacked, and four in a 2×2 grid. Deliver persistence with
one visible terminal first, then tiling.
The initial implementation can use the existing PTY protocol and broker stream
infrastructure. A new browser transport multiplexing protocol is unnecessary.

The retention guarantee applies to the owner document's lifetime. Reloads, tab
closure, browser suspension, network failures, and server restarts can interrupt
connections. Retention removes navigation-induced reconnects; it does not make a
WebSocket durable.

## Evidence from the current implementation

Paths below are relative to the repository root. Function names are stable
reference points for implementation; behavior was inspected, not benchmarked.

| Area | Current behavior | Consequence |
| --- | --- | --- |
| `web/src/client/main.ts`, `renderRoute()` | Removes the previous page even within a reused shell; clears `#app` when changing shell type | Both page and shell navigation destroy terminal DOM |
| `web/src/components/pages/terminal.ts`, `disconnectedCallback()` / `cleanup()` | Sends tmux detach, closes WebSocket and SSE, disposes xterm and observers | A cached agent ID or route alone cannot retain an attach or screen |
| Same file, `loadAgentInfo()` / `initTerminal()` / `connectWebSocket()` | Fetches metadata, imports xterm/addons/CSS, waits for layout, performs PTY auth preflight, then opens WebSocket | Reopening repeats work; the contribution of each step to latency still needs measurement |
| Same file, `handleReconnect()` | Calls full cleanup and reloads metadata | Reconnect currently discards local terminal state |
| Same file, `connectSSE()` | Opens an `SSEClient` per terminal | Retaining many copies would multiply event connections |
| Same file, resize, input, and upload handlers | Always fits on resize, focuses on socket open, installs window-level drop prevention, loads OSC 52 clipboard support | Hidden and unfocused terminals need explicit interaction rules |
| `web/src/components/shared/header.ts`, `renderModeSwitch()` | Implements a Dashboard/Chat switch gated by native chat | Add a third mode independently of whether Chat is enabled |
| `web/src/components/shared/chat/chat-members.ts`, `openTerminalPopout()` | Opens a named browser window per agent, with in-app fallback | Existing window reuse is separate from reuse within the main SPA |
| `web/src/client/state.ts`, `setScope()` | Clears route-scoped agent state and reconnects SSE on scope changes | Persistent terminal metadata/subscriptions cannot depend solely on the current route scope |
| `pkg/hub/pty_handlers.go`, `handleAgentPTY()` / `PTYSession.Run()` | Authorizes attach, upgrades one browser socket, opens a broker PTY stream, and pings the browser | Existing transport supports multiple independent retained attaches |
| `pkg/runtimebroker/controlchannel.go`, `handlePTYStream()`; `pkg/runtimebroker/pty_handlers.go`, `StreamPTYHandler` | Resolves the agent and runs a tmux attach through runtime exec | Every retained socket also retains a broker stream and runtime attach resources |

The deployed frontend is a Lit SPA served by Go, as documented in
`web/AGENTS.md`. References to Koa/SSR in the older frontend design and terminal
source comments do not describe this request's runtime path.

## User experience and scope

The shared header exposes Dashboard, Chat when enabled, and Terminals. Terminals
shows an open-session count. Returning to Dashboard or Chat restores that mode's
last route in this tab. A session may belong to any accessible project; show its
project beside its agent name to disambiguate repeated names.

```text
Dashboard   Chat   Terminals (6)     Layout: 1 | 2 side by side | 2 stacked | 4
┌──────────────────────┬──────────────────────┬──────────────────────┐
│ Open terminals       │ agent-a · project-x  │ agent-b · project-x  │
│ ● agent-a  project-x │ terminal             │ terminal             │
│ ● agent-b  project-x ├──────────────────────┼──────────────────────┤
│ ● agent-c  project-y │ agent-c · project-y  │ agent-d · project-y  │
│ ● agent-d  project-y │ terminal             │ terminal             │
│ ● agent-e  project-z │                      │                      │
│ ○ agent-f  project-z │                      │                      │
└──────────────────────┴──────────────────────┴──────────────────────┘
```

The diagram shows four visible panes and six retained sessions. Connection state
and agent phase/activity are distinct indicators: a running agent can have a
disconnected terminal, and a completed task can leave an attachable agent.

| Action | Proposed behavior |
| --- | --- |
| Open terminal from list, detail, tree/graph, or chat membership | Route to the owner tab, ensure one session in the rail, set the single-pane slot to that agent, activate the single-pane layout, and request browser focus |
| Open an already connected agent | Reuse its session in the single-pane slot; do not preflight or attach again, even if it also belongs to a multi-pane arrangement |
| Open an already connecting agent | Show the same pending session in single-pane mode; share its connection attempt |
| Open a disconnected session | Show it in single-pane mode with its Reconnect action; do not create a second session |
| Click a session in the rail | Use the same single-pane selection action; preserve all multi-pane arrangements |
| Drag a rail entry onto a pane | Explicitly assign that session to the target slot in the displayed layout, without creating another attach |
| Add a new session | Add it to the rail and replace the single-pane slot, switching to that layout; do not fill or replace any multi-pane slots |
| Choose a layout | Restore that layout's saved assignments; multi-pane layouts initially have empty slots for explicit placement |
| Maximize a pane | Temporarily show only that pane; restore prior placement on exit |
| Close session | Remove it from the rail, detach its client, dispose local resources; do not stop the agent |
| Switch to Chat/Dashboard/Profile | Hide terminal workspace and retain connections |
| Agent stops or is deleted | Preserve a labeled disconnected/unavailable entry until the user closes it; disable inappropriate actions |
| Narrow viewport | Show one pane and a collapsible session list; preserve the desktop placement for return |
| Open in graph / Open in chat | Navigate to the pane's agent context in the owner SPA; retain every attach and layout |

Protection applies to entire multi-pane arrangements. No pinning system is
required: ordinary terminal opens and rail clicks can modify only the single-pane
slot. Explicit drag/drop or its keyboard equivalent modifies the chosen layout.
Closing a session removes its references from every layout and leaves those slots
empty; no other terminal is automatically substituted.

Use explicit Close and Maximize controls; do not overload one control with both
hide and detach. Keyboard users can reach the list and pane toolbar without
keystrokes being sent to an agent. Announce connection changes without reading
every terminal output update. No broadcast input: only one pane receives human
keyboard/paste input at a time.

Initial scope includes rail-to-pane drag/drop but excludes arbitrary split trees,
draggable docking between windows, synchronized
typing, shared terminal layouts across users, and
stored terminal transcripts. Fixed CSS Grid layouts are sufficient for this scope.
Separate agent/shell tiles for the same agent are also excluded: retain the
existing Agent/Shell control within that agent's single attach.

## Technical design

### Lifetime and ownership

In the owner tab, create two stable children under the SPA root during bootstrap:

```text
#app
  route-outlet               current app/chat/profile/standalone shell
  scion-terminal-workspace   retained host, created on first terminal open
    shared header + session rail
    stable pane collection   one xterm host per retained session
```

Change `renderRoute()` to replace only the route outlet. On terminal routes,
activate the workspace and hide or clear the ordinary outlet; on other routes,
hide the workspace. Never clear `#app` after installing these roots. The terminal
workspace is a peer route presentation, with an independent document lifetime;
adding a `terminal` value to `ShellType` alone would still trigger teardown.

Render pane elements with stable session keys. Change CSS grid placement and
visibility rather than recreating terminals or moving them between disposable
page containers. All pane elements stay under the retained host until closed.
Do not depend on disconnect/reconnect callbacks preserving xterm through DOM
reparenting. Scope toast and page-title handling to the visible mode so retained
components cannot duplicate notifications or overwrite Chat's title.

Proposed modules (new names, not existing APIs):

| Module | Responsibility |
| --- | --- |
| `web/src/client/terminal-sessions.ts` | Registry, idempotent open, connection attempts, close/reconnect, subscriptions, and state notifications |
| `web/src/client/terminal-coordinator.ts` | Browser-context owner discovery, exclusive ownership, open requests and acknowledgments, focus adapter |
| `web/src/components/terminal/terminal-workspace.ts` | Rail, saved assignments for each layout, active pane, drag/drop placement, mode header |
| `web/src/components/terminal/terminal-pane.ts` | Stable xterm host, existing toolbar capabilities, fit, focus, upload UI |
| `web/src/client/main.ts` | Stable roots, terminal route resolution, auth lifecycle cleanup |

The registry owns session lifetime and transport. Each session owns one pane's
xterm resources, with UI visibility separate from connection state. A page's
disconnect callback no longer decides when to dispose the session. Disposal is
idempotent and occurs on explicit Close, account teardown, or document teardown.

Suggested internal state:

```typescript
type TerminalConnectionState =
  | 'loading' | 'connecting' | 'connected'
  | 'disconnected' | 'unavailable' | 'closing';

interface TerminalSessionState {
  key: string; // hub origin + authenticated user ID + agent UUID
  agentId: string;
  projectId: string;
  agentName: string;
  connection: TerminalConnectionState;
  generation: number;
  lastSize: { cols: number; rows: number } | null;
  error: string | null;
}
```

Keep WebSockets, AbortControllers, xterm objects, and pending promises private to
the session implementation, outside serializable view state. Use agent UUIDs,
never agent names/slugs, for client deduplication. Register a pending entry before
any await. Abort fetches and invalidate the generation on close/reconnect;
callbacks from an old generation must not write to or reconnect the new one.
An async import that completes after close must immediately dispose anything it
created. Repeated reconnect clicks share one attempt.

### Reuse across browser windows and tabs

The proposed scope is one workspace per hub/account in one browser profile and
storage partition. Tabs using a different scheme/host/port, another Chrome
profile, or an incognito context are separate. Identify the deployment with the
origin plus application base path (or a stable Hub identifier if available), and
include the signed-in user ID. Multiple URLs for the same Hub cannot coordinate
through origin-local browser APIs without additional integration.

Use `BroadcastChannel` for owner discovery and requests such as
`open(agentId, requestId)`. It supports communication between same-origin contexts
in the same storage partition. This lets independently opened tabs participate,
without requiring an opener reference. Send identifiers and acknowledgments;
keep terminal bytes and xterm instances in the owner.
[Broadcast Channel documentation](https://developer.mozilla.org/en-US/docs/Web/API/Broadcast_Channel_API).

Use an exclusive Web Lock to elect the owner and prevent simultaneous first opens
from creating two workspaces. Only the holder may attach. Advertise an owner
instance ID and deduplicate request IDs there. A delayed acknowledgment is not
proof that the owner is gone: check ownership instead of timing out into another
attach. A frozen owner should produce a pending/unavailable indication, not a
second owner. On ownership loss, stop attaching and close local transports before
releasing authority. Reclaim after owner exit means new attaches, not transfer of
existing sockets or DOM. Treat missing secure-context/API support explicitly in
the UI rather than silently promising browser-wide deduplication.
[Web Locks documentation](https://developer.mozilla.org/en-US/docs/Web/API/Web_Locks_API).

**Finding a workspace and activating its browser tab are separate capabilities.**
The owner can select an agent and acknowledge the request without Chrome bringing
it to the foreground. `window.focus()` is a request that can be denied. A named
`window.open()` target helps with related contexts, but is not a profile-wide tab
directory; opener policies affect whether a reference can be obtained. Do not
navigate/reload an existing owner to select an agent, because that would destroy
all its attaches. Use a retained reference where available and send an in-page
selection message.
[Window focus documentation](https://developer.mozilla.org/en-US/docs/Web/API/Window/focus),
[Window open documentation](https://developer.mozilla.org/en-US/docs/Web/API/Window/open).

Do not assume a service worker solves activation. Although the documented
`WindowClient.focus()` contract describes transient user activation, the current
Chromium implementation also gates it on worker window-interaction permission.
The inspected event dispatcher grants that permission for specific events such
as notification clicks, not an ordinary message event. A button click followed
by `postMessage` to a worker is therefore not a proven general activation path.
This is a source-based assessment, not a desktop Chrome compatibility test.
[WindowClient documentation](https://developer.mozilla.org/en-US/docs/Web/API/WindowClient/focus),
[Chromium focus implementation](https://raw.githubusercontent.com/chromium/chromium/main/third_party/blink/renderer/modules/service_worker/service_worker_window_client.cc),
[Chromium event permission handling](https://raw.githubusercontent.com/chromium/chromium/main/third_party/blink/renderer/modules/service_worker/wait_until_observer.cc).

Use browser-wide ownership in the web app, with best-effort activation and
an explicit acknowledgment such as "Selected in your terminal workspace" when
foreground activation cannot be confirmed. Keep a recognizable workspace tab
title. Never report that a tab was focused merely because the owner acknowledged
the open request, and never silently create a duplicate attach as a focus fallback.

Best-effort activation is accepted. A Chrome extension is explicitly out of scope;
failure to foreground the owner must not create another attach or block rollout.

The first tab that opens Terminals claims the singleton role for this hub/account
in the browser context. It may subsequently
show Chat or Dashboard while retaining that role. Other tabs still target it.
An optional later **Use this tab for terminals** action would require an explicit
handoff: the old owner closes
its attaches and releases ownership, then the new owner reconnects from a session
manifest. This is not seamless migration. Closing the owner likewise loses live
attaches; automatic manifest restoration remains a product choice.

Prototype actual tab activation in headed Chrome, including independent tabs,
separate windows, minimized windows, an owner currently displaying Chat, opener
closure, concurrent opens, and denied-focus acknowledgment.
Headless tests cannot establish the desktop focus experience.

### Routes and entry points

Add `/terminals` for the workspace and `/terminals/:agentId` for a focused agent.
Keep `/agents/:agentId/terminal` as an alias, resolving to the retained workspace
with history replacement. Direct loads route one requested session to the owner. Browser
Back/Forward changes selection or mode and does not close retained sessions.

Centralize terminal opening so repeated selection works even when the current
URL already matches: `navigateTo()` currently returns early for the same path.
Route resolution and an explicit open action must converge on the same idempotent
registry operation. Preserve base-path handling, page titles, and stale-navigation
guards. A canceled navigation must not steal focus after its async work resolves.

Update all current entry points: `agents.ts`, `agent-detail.ts`, both list/grid
links in `project-detail.ts`, `agent-tree-view.ts`, and `chat-members.ts`. Preserve
their existing attach-capability and agent-availability gates; the Hub remains
authoritative. Change the default chat attach action to the workspace. Existing
terminal popout entry points must join singleton routing; do not preserve an
implicit second workspace. A user can move the owner tab to another Chrome
window. Per-pane popouts and explicit ownership handoff are later enhancements.
Audit router click interception for modified clicks/targets as part of this work.

Register titles and production icons. The Terminals mode must remain available
when native chat is disabled. Session IDs and layout live in memory in v1;
reload starts a new workspace, except for a directly addressed agent route.
Optional later `sessionStorage` can restore a list/layout, but cannot restore live
sockets; reconnecting restored entries should be explicit.

### Layout state and deliberate placement

Represent layout assignments separately from the session registry:

```typescript
type TerminalLayout = 'single' | 'two-columns' | 'two-rows' | 'four';
type TerminalSlot = string | null; // session key

interface TerminalLayoutState {
  active: TerminalLayout;
  single: [TerminalSlot];
  twoColumns: [TerminalSlot, TerminalSlot]; // left, right
  twoRows: [TerminalSlot, TerminalSlot]; // top, bottom
  four: [TerminalSlot, TerminalSlot, TerminalSlot, TerminalSlot]; // row major
}
```

Each preset retains its own assignments. An `open(agentId)` request, including
one received from another browser tab, sets `single[0]` and `active = 'single'`.
It never mutates `twoColumns`, `twoRows`, or `four`. Thus a user can open a fifth
agent, inspect it alone, then return to precisely the previous four-agent grid.
The old single-pane session also remains in the rail and connected.

Dropping a rail entry on a multi-pane slot is the explicit placement operation.
Use a dedicated drag handle and an application-specific drag payload containing
only a session key. Validate that it belongs to this workspace. Show the target
slot before drop, including on empty slots. If the dragged session is already in
that layout, swap its previous slot with the target; dropping onto its existing
slot is a no-op. A replaced session stays attached and available in the rail.
Assignments in other layouts remain untouched. This preserves one visible pane
per session without forcing a second xterm or WebSocket.

Provide an accessible **Place in pane…** action from the rail with the same
semantics for keyboard and touch users. Distinguish this payload from file drops:
placing a terminal must never trigger an upload, and dropping a file must never
rearrange the grid. User input continues to go only to the focused pane.

The same session may be referenced by several inactive presets. Use CSS placement
of its one stable terminal element for the active preset. Maximizing a pane and
the narrow-viewport presentation are temporary views; they must not overwrite the
saved multi-pane assignments. Session close clears all references to that key.

### Pane-header context actions

Put two labeled icon buttons in every pane header, alongside existing controls:

| Action | Existing implementation to reuse | Behavior |
| --- | --- | --- |
| **Open in graph** (`diagram-3`) | `agent-detail.ts` graph link; `agent-graph.ts` project/focus query parsing | Navigate to `/agents/graph?project=<projectId>&focus=<agentId>`, encoding both IDs |
| **Open in chat** (`chat-dots`) | `chat.ts` agent DM key construction and `client/chat-routes.ts` routing | Open the signed-in user's direct conversation with this agent in native Chat |

Graph/chat navigation happens in the owner SPA and leaves the workspace retained.
Returning through the mode switch restores the previous layout. These actions
target the pane's agent, even when another pane last had keyboard focus.

For Chat, extract/reuse `chat.ts`'s `buildDMKey` logic as a shared helper and pass
the resulting `dm:agent:<agentId>:user:<userId>` key to `chatConversationPath()`.
Avoid the bare-peer-ID shortcut here: the current Chat route infers peer kind
from loaded membership, which may be absent on a cold navigation. Navigate only
after user identity is available. The action opens a conversation and sends no
message; Chat retains its existing history/composer authorization rules. Hide
the action when native Chat is disabled. An agent may appear in multiple group
conversations; choosing a particular group is a later enhancement, while the DM
gives this icon one predictable destination.

Both icon names already appear in `web/scripts/copy-shoelace-icons.mjs`; still
verify production assets. Give each button an accessible label and tooltip.

### Hidden output, resizing, and interaction

Continue consuming and parsing PTY bytes into each retained xterm even when it
is hidden. Make scrollback depth configurable with a generous default; the prior
2,000-line proposal is withdrawn. Do not build an unbounded
second output queue or discard arbitrary terminal bytes; escape sequences and
screen state depend on their order.

On hide, blur the pane, make it inert/non-focusable, and suppress resize work.
Retain its last nonzero rows/columns. On reveal, wait for Lit layout and an
animation frame, fit against nonzero dimensions, refresh if required, and send a
resize only if dimensions changed. Keep the existing resize debounce, with a
visibility check both when scheduled and when fired. Never send a hidden pane's
zero dimensions to tmux. Initial attach should wait for measurable geometry.

On socket open, focus only if the workspace is visible and the pane is still the
user's selected target. Limit global drop prevention to the visible terminal
workspace and route uploads to the actual drop target. Check session generation
again when asynchronous clipboard reads or uploads finish. Scope OSC 52 clipboard
access to the focused, visible terminal; hidden agent output must not change or
read the system clipboard. Preserve terminal protocol responses while blocking
human input to hidden panes; do not indiscriminately disable all transport writes.

Retain existing behavior for links, selection on macOS, Shift+Enter, Agent/Shell
switching, exposed-port links, capture-auth, and file uploads. Extract these
incrementally from `terminal.ts` with tests instead of recreating a reduced
terminal component. Keep larger actions in a pane menu so four toolbars fit.

### Status subscriptions

Use one workspace-owned `SSEClient` with the union of
`agent.<id>.>` subjects for open entries. This adds a constant one event connection
alongside route state rather than one per terminal. Update the union when sessions
open/close, coalescing rapid changes. Do not set the global route scope to keep
terminal events alive: that would replace Chat/Dashboard subscriptions.

Store terminal metadata independently of `stateManager`'s route-cleared maps.
Handle status, ports, and deleted events explicitly. Subscribe before taking a
metadata snapshot; buffer events during the snapshot and apply them afterward.
On SSE reconnection, reconcile retained entries from fresh metadata, with bounded
request concurrency, because events can have been missed. Verify subject
authorization and subscription-size limits as the list grows. If a server/URL
limit requires subscription batches, handle it explicitly without evicting sessions.

A general subscription registry shared with route state could remove the extra
SSE connection later. It would broaden this change's scope into chat and dashboard
state management; the dedicated workspace subscription is the proposed first step.

### Transport, disconnects, and authorization

Reuse `/api/v1/agents/:id/pty`, its cookie authentication, and JSON/base64 data and
resize frames. Preserve the auth preflight on first attach and actual reconnect;
do not repeat it when focusing a healthy session. The current preflight returns
200 after authorization, before broker availability checks. It therefore cannot
prove that an attach will succeed or reliably explain every upgrade failure.

Unexpected socket closure retains the entry and last screen with input disabled
and a clear disconnected overlay. V1 uses explicit Reconnect, re-fetching agent
metadata and authorization. A successful new attach begins a fresh terminal
screen/buffer so a new tmux redraw is not presented as uninterrupted output;
navigation retention preserves the original buffer because it never reattaches.
Never queue/replay user input across a disconnect. Automatic retries, if added
later, need capped exponential backoff with jitter and must stop for 401/403/404
and stopped/deleted agents. Agent restart is not a reason to silently replay input.

On logout or identity change, close all sessions and remove their buffers. Clear
sessions before showing a login page after detected authentication expiry. New
attaches must always pass Hub authorization. Authorizing on attach and reconnect
is the accepted requirement. Periodic reauthorization and live permission
revocation are outside scope and are not rollout gates. Broadcast account teardown
to participating tabs so logout from a non-owner also disposes the owner workspace.

Explicit Close should preserve the current detach sequence during extraction,
followed by socket closure. Test that the agent survives and broker resources are
released. The source comment claiming a killed attach tears down the container
needs validation: the stream implementation cleans up its runtime exec process.
Do not change detach semantics based on that comment alone.

The existing tmux configuration selects windows in session `scion`; multiple
attaches to one agent can interact through shared window selection and size.
Browser-wide deduplication reduces this exposure. Isolation from other CLI
attaches' window selection and sizing is not promised. Test concurrent clients,
especially the sandbox runtime's explicit
`resize-window`, before considering per-client tmux sessions as a separate change.

### Retention policy

There is no product cap on retained sessions, no idle eviction, and no memory or
latency budget gating delivery. Four visible panes is a starting layout preset,
not a retained-session limit. Heavy browser resource use is an accepted tradeoff.
Display connected and total counts and provide explicit close actions.

Continue checking lifecycle correctness: returning to a connected terminal must
produce zero new PTY upgrades or detach sequences, and explicitly closing one
must release its transport and observers. Preserve byte ordering under sustained
output. Memory optimization and benchmark targets are follow-up work unless a
functional failure prevents normal use.

## Phased implementation and acceptance gates

| Phase | Deliverable and likely touchpoints | Gate |
| --- | --- | --- |
| 0. Browser ownership and lifecycle spike | Test cross-window focus paths, Web Lock ownership, hiding/revealing xterm, tmux resize, cleanup, and clipboard in isolated fixtures | Confirm best-effort focus and denied-focus acknowledgment; confirm explicit close leaves agent running |
| 1. Retained single-pane workspace | Session registry, browser coordinator, terminal extraction, stable router roots, one pane plus rail, third mode, alias route, all entry points, aggregated terminal SSE | Repeated opens across tabs deduplicate while pending/connected; navigation preserves socket and screen; complete existing terminal feature parity |
| 2. Tiling and context actions | Single, two-column, two-row and 2×2 presets; protected multi-pane assignments; rail drag/drop and keyboard placement; zoom; graph/chat header icons | Open requests change only the single slot; placement changes only the selected layout; context links address the correct agent; no socket/xterm recreation |
| 3. Reliability and rollout | Cross-tab account cleanup, owner loss, reconnect races, event reconciliation, concurrent-client/runtime checks | Failure matrix passes; no silent duplicate attaches or misleading focus success; session closing releases resources |

Phases are sequential at their lifecycle boundaries. Phase 1 is independently
useful; ship it behind a proposed `web.terminal_workspace` flag once its gates
pass, then enable tiling. Use the same extracted session implementation for the
legacy page and workspace while the flag exists to avoid maintaining two PTY
clients. Resolve the flag at bootstrap; a live flag change must not silently
dispose existing sessions. Retire the flag and disposable page adapter after
rollout. No database migration or CLI command is required by the initial scope.

Suggested validation includes:

| Level | Required cases |
| --- | --- |
| Vitest | Concurrent `open()` calls; owner election and request deduplication; close while fetch/import/preflight is pending; stale callbacks; repeated reconnect; idempotent disposal; per-preset assignments and swap/no-op drops; close clears references; subject union and reconciliation; agent DM links with cold membership |
| Browser with real xterm and mocked transport | Route changes preserve exact socket and pane identities; aliases/direct loads and Back/Forward; same-URL open; hidden output visible on return; nonzero sizing; focus/clipboard/drop isolation; mobile layout; chat-disabled mode; logout; existing toolbar features |
| Hub/broker integration with disposable fixtures | One runtime attach per browser session; close cleans up stream/process without stopping agent; agent stop/restart/delete; permission failure; broker/network interruption; two browser/CLI attaches; runtime resize behavior |
| Headed Chrome | Independently opened tabs and multiple/minimized windows; owner displaying another mode; denied focus; concurrent first opens; owner exit/freeze; logout from a non-owner tab |
| Layout and navigation | Open a fifth agent from another tab while a four-pane grid is selected: switch to single, then restore the unchanged grid; verify both two-pane orientations retain assignments; drag/drop and keyboard placement agree; file drops remain uploads; graph focuses the pane agent; Chat opens its DM; both retain attaches and layout |
| Retention checks | Accumulate sessions beyond twelve without eviction; sustained hidden output; repeated mode/layout switches preserve attaches; explicit close releases resources |

Reuse `terminal.test.ts` coverage for uploads and extend the repository's existing
Playwright setup for browser behavior. Do not exercise lifecycle tests against
active project agents. For implementation, run web typecheck/lint/tests/build and
`make ci-full`; scope any extra backend tests to actual protocol/lifecycle changes.

## Accepted decisions and remaining defaults

Accepted in the design discussion:

1. One owner workspace per hub/account/browser profile across accessible projects;
   activation is best effort and no Chrome extension will be built.
2. One attach per agent in the owner, with Agent/Shell switching inside its pane.
3. Single, two side by side, two stacked, and 2×2 layouts; no session cap or eviction.
4. First terminal tab owns the workspace; opening from other tabs selects it.
5. Terminal opens and rail clicks select the single slot; multi-pane presets retain
   their assignments until explicit placement or session close.
6. Authorization on attach/reconnect is sufficient; live revocation is out of scope.
7. Rail-to-pane drag/drop and pane-header Open in graph / Open in chat actions are
   included in the initial tiling phase.

Remaining implementation defaults: explicit reconnect in v1, in-memory layout
retention for the document lifetime, and Chat opening the agent DM. Reload
restoration and ownership handoff can be added later without changing the rules
above. They are not prerequisites for the initial implementation.

## Remaining design choices and multiplexer extensions

The earlier questions about foreground activation, first owner, opening into a
full grid, and layout presets are resolved above. Remaining optional extensions:

| Choice | Recommendation | Scope implication |
| --- | --- | --- |
| What survives reload/owner closure? | Save membership, names, and layout; offer Restore/Reconnect | Restores organization but creates new attaches; terminal transcripts need separate storage design |
| Detach individual panes to another monitor? | Defer to a later phase; specify move versus mirror first | Shared transport with another renderer needs explicit input/resize ownership; reconnecting is simpler but breaks seamless retention |
| More layout freedom? | Consider saved named arrangements and adjustable split ratios after fixed presets | Arbitrary split trees are a separate layout model, not a prerequisite for persistence |

Suggested feature order, beyond the base workspace:

1. **Quick switcher and jump back.** Search open agents by name/project, switch
   without using the rail, and toggle between the last two focused terminals.
   Make shortcuts configurable so they do not consume normal terminal commands.
2. **Attention queue.** Mark agents waiting for input, completed, or errored using
   existing phase/activity events; provide Next needing attention. Keep rail order
   stable while typing and never automatically move keyboard focus on an event.
3. **Saved named layouts.** Save groups such as "backend investigation" or "release"
   on top of the protected presets; no separate pinning mechanism is needed.
   Layouts reference existing sessions, so changing layouts does not detach them.
4. **Terminal search and output markers.** Find text in retained scrollback, mark
   a point before a task, and jump to new output. Search across open terminals as
   a later extension. Be clear that this searches retained output, not complete
   agent history. Holding the viewport still must continue parsing incoming bytes.
5. **Linked context panel.** Expand the accepted graph/chat header links with a
   focused-agent task/status panel and additional conversation selection.
6. **Read-only observation.** A per-pane input lock prevents accidental typing
   while monitoring a group; it is a UI control, not a permission boundary.
7. **Opt-in batch actions.** Reconnect disconnected sessions or close a chosen
   group. Broadcast keystrokes would be a separate, explicit mode with conspicuous
   recipients; for sending the same task to agents, integrate existing group
   messaging rather than assuming all harness terminals interpret text alike.

The highest-value initial additions are the quick switcher, attention queue, and
named layouts: they build on agent state and make a large terminal set
easier to work through. Per-pane popouts and multiple independent Agent/Shell
views require more transport/tmux decisions and should follow the ownership work.

Adjacent cleanup identified during inspection: update stale Koa/SSR descriptions
in `terminal.ts` and the older frontend architecture document; assess whether
route state should eventually support additive subscriptions; validate the tmux
detach comment against each runtime. These are recorded for follow-up and are
not bundled into this proposal's implementation by default.
