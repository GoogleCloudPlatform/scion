# Terminal workspace P1.6: shared header and retained-session rail

Implemented against accepted base `24e78ba090c0681f1edcf0935c0ff4508b5caee7`.

The retained terminal root now renders the single-pane workspace surface: shared
header, keyboard-accessible session rail, empty/status states, and one stable
pane host per retained session. The rail shows agent name plus project identity
from the registry metadata map, so repeated agent names remain distinguishable.
Connection state comes from `TerminalSession.state.connection`; agent metadata
availability and phase/activity remain separate. Deleted/unavailable metadata
and disconnected transports stay visible until explicit Close.

The registry exposes a collection-level `subscribe()` hook so the workspace can
update its rail without polling, creating another registry, or opening a second
SSE/EventSource path. The workspace still creates panes only through the accepted
owner bridge: `TerminalCoordinator` owns the registry, invokes
`create(registry, agentId)`, and `pane.open(registry, UUID)` synchronously
registers/binds the session. Rail selection dispatches `/terminals/:agentId`,
which returns through the same coordinator path and reuses the exact retained
session/pane/socket. Close calls only `session.close()` for that entry; it does
not stop/delete the agent or close peers. Reconnect is explicit and calls the
retained session connect path plus metadata refresh, with no input replay.

The shared header now presents Dashboard, enabled Chat, and Terminals as peer
modes. Terminals remains available when native Chat is disabled. The header
receives the retained-session count through a document event and restores the
last Dashboard and Chat routes for this tab; returning to Terminals shows the
retained single selection. `/terminals` and `/terminals/:agentId` have page-title
coverage. Existing Shoelace icons already include `terminal`, `x-circle`, and
`arrow-clockwise`; the production build reruns the icon copy task.

The phase deliberately does not migrate all terminal entry points, add tiling,
drag/drop placement, graph/chat pane actions, hidden-input policy, or backend
behavior. Those remain P1.7, Phase 2, P1.8, and separate backend scopes.

Validation uses the existing terminal-workspace Playwright fixture with the real
production router, coordinator, registry, pane, and xterm, while HTTP, PTY
WebSocket, and EventSource are fixture-controlled. Added cases cover rail
selection without a new attach, repeated names across projects, close selected
with peer survival, disconnected/reconnect retention, empty/count/header states,
Chat-disabled Terminals, and last Dashboard/Chat route restoration. No live Hub,
broker, runtime, or active agent was probed.

Known limits: desktop foreground focus, live authorization/runtime behavior,
broad entry-point migration, and detailed hidden input/drop/clipboard policy are
not claimed by this phase. Full ESLint remains baseline-red; exact counts are
reported in the developer handoff.

## R2 accessibility acceptance

R1 follow-up replaced the rail's `listbox`/`option` structure with an explicit
`list`/`listitem` rail containing separately named Show/Reconnect/Close buttons,
avoiding nested interactive controls inside ARIA options while preserving the
same accessible names and arrow-key focus behavior. Selection is exposed on the
Show button with `aria-current="page"` and styled with `data-selected`.

Rail refreshes are now microtask-batched for synchronous session/metadata
notifications, count publication happens through `refresh()`, and the header and
workspace share the terminal-count event from one module. Header last-path memory
is intentionally document-level and is grouped/commented for the two header
instances. Pending Reconnect controls are disabled while a session is loading or
connecting.

Browser coverage now includes real Chromium ArrowDown/ArrowUp/Home/End rail
navigation, proof that no `role="option"` remains, metadata-only refresh focus
preservation across an accessible-name change, and the existing deployment
base-path route/history case.
