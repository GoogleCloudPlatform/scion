# Terminal workspace P1.5: stable roots and routes

The SPA now installs a document-lived route outlet and creates a retained terminal
root when the first enabled terminal route is requested. Ordinary app, Chat,
profile, and standalone shell replacement is confined to the outlet. The
terminal root keeps each pane node and its xterm instance mounted while another
mode is visible.

The terminal coordinator still owns the sole session registry and obtains its
Web Lock before creating a session. Its optional owner-only `create(registry,
agentId)` adapter delegates first creation to `pane.open`; the previous
initializer path remains for isolated consumers. Existing registry entries
skip creation and selection reuses the exact session/pane. The coordinator
checks that the adapter returns the requested session from its registry.
An attempted second pane for a registered agent still throws.

The `web.terminal_workspace` feature flag is captured after bootstrap settings
load. Enabled `/agents/:agentId/terminal` routes replace history with
`/terminals/:agentId`; `/terminals` and direct agent loads use the coordinator.
The flag-off path retains the shared disposable page adapter. Later in-document
flag changes cannot switch an active workspace to disposable behavior.
Navigation IDs are assigned before asynchronous work, so a superseded local
terminal request cannot reveal its pane. A remote user's request remains an
independent owner intent. Router listeners are installed before the first
asynchronous terminal render, and same-URL terminal navigation still selects.

The local Chromium fixture runs the production `main.ts` router, coordinator,
registry, pane, and xterm. It intercepts agent HTTP and PTY WebSocket traffic
and supplies a fixture EventSource; no live Hub, broker, or agent is used. Tests
verify one initializer, pane, xterm, and attach through repeated pending and
connected opens; duplicate-pane rejection; close before initialization; mode
and history retention; secondary-tab ownership; superseded navigation;
flag-off disposal; and document-lifetime flag stability. A separate Vite base
fixture verifies `/tw/` direct legacy load, history replacement, Back, and one
attach. Unsupported coordination is also verified to produce no local pane or
attach. These browser checks do not establish desktop foreground focus, live
authorization, or native runtime behavior.

The feature deliberately leaves header/rail layout and broad entry-point
wiring to P1.6/P1.7, and detailed hidden input/drop policy to P1.8. The
minimal retained root exposes a status message when a second tab routes its
request to an owner.
