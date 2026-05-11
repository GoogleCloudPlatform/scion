# Scion Web Frontend - Agent Instructions

This document provides instructions for AI agents working on the Scion Web Frontend.

## Design Documents

Before making changes, review the relevant design documentation:

- **[Web Frontend Design](../.design/hosted/web-frontend-design.md)** - Architecture, technology stack, component patterns

## Architecture Overview

The web frontend is a **client-side SPA** built with Lit web components. There is no Node.js server at runtime. The Go `scion` binary serves the compiled client assets and handles all server-side concerns (OAuth, sessions, SSE real-time events, API routing) via `pkg/hub/web.go` and `pkg/hub/events.go`.

Node.js and npm are used **only at build time** to compile and bundle client assets via Vite.

## Development Workflow

### Building and Running

```bash
cd web
npm install    # First time only, or after package.json changes

# Build client assets
npm run build

# Run the Go server with dev auth (from repository root)
scion server start --enable-hub --enable-web --dev-auth \
  --web-assets-dir ./web/dist/client
```

Dev auth bypasses OAuth and auto-creates a session with admin privileges. The `--web-assets-dir` flag loads assets from disk so you can rebuild and refresh without restarting the server.

### Using Vite Dev Server

For client-side development with hot module reload:

```bash
npm run dev
```

Note: The Vite dev server only serves client assets. API calls and SSE require the Go server to be running.

### Common Commands

| Command | Purpose |
|---------|---------|
| `npm run dev` | Start Vite dev server with hot reload |
| `npm run build` | Build client assets for production |
| `npm run build:dev` | Build client assets in development mode |
| `npm run lint` | Check for linting errors |
| `npm run lint:fix` | Auto-fix linting errors |
| `npm run format` | Format code with Prettier |
| `npm run typecheck` | Run TypeScript type checking |

### Verifying Changes

After making changes, verify:

1. **Type checking passes:** `npm run typecheck`
2. **Linting passes:** `npm run lint`
3. **Client builds:** `npm run build`

## Project Structure

```
web/
├── src/
│   ├── client/              # Browser-side code
│   │   ├── main.ts          # Client entry point, routing setup
│   │   ├── state.ts         # State manager with SSE subscriptions
│   │   └── sse-client.ts    # SSE client for real-time updates
│   ├── components/          # Lit web components
│   │   ├── index.ts         # Component exports
│   │   ├── app-shell.ts     # Main application shell (sidebar, header, content)
│   │   ├── shared/          # Reusable UI components
│   │   │   ├── index.ts         # Shared component exports
│   │   │   ├── nav.ts           # Sidebar navigation
│   │   │   ├── header.ts       # Top header bar with user menu
│   │   │   ├── breadcrumb.ts   # Breadcrumb navigation
│   │   │   ├── debug-panel.ts  # Debug panel component
│   │   │   └── status-badge.ts # Status indicator badges
│   │   └── pages/           # Page components
│   │       ├── home.ts          # Dashboard page
│   │       ├── login.ts         # OAuth login page
│   │       ├── agents.ts       # Agents list page
│   │       ├── agent-detail.ts # Agent details page
│   │       ├── projects.ts       # Projects list page
│   │       ├── project-detail.ts # Project details page
│   │       ├── terminal.ts     # Terminal/session page (xterm.js)
│   │       ├── unauthorized.ts # 401/403 page
│   │       └── not-found.ts    # 404 page
│   ├── styles/              # CSS theme and utilities
│   │   ├── theme.css        # CSS custom properties, light/dark mode
│   │   └── utilities.css    # Utility classes
│   └── shared/              # Shared types between components
│       └── types.ts         # Type definitions (User, Project, Agent, etc.)
├── public/                  # Static assets
│   └── assets/              # Built client assets (CSS, JS)
├── dist/                    # Build output (gitignored)
├── vite.config.ts           # Vite build configuration
├── tsconfig.json            # TypeScript configuration
└── package.json
```

## Technology Stack

- **Components:** Lit 3.x with TypeScript decorators
- **UI Library:** Shoelace 2.x
- **Build:** Vite for client-side bundling
- **Routing:** Client-side via History API (click interception in `main.ts`)
- **Terminal:** xterm.js for terminal sessions
- **Server:** Go (`scion` binary with `--enable-web`)

## Icon Reference

All icons use the Shoelace `<sl-icon>` component, which provides [Bootstrap Icons](https://icons.getbootstrap.com/). Use these consistently when building new UI features.

**Important:** Only icons listed in the `USED_ICONS` array in `scripts/copy-shoelace-icons.mjs` are included in production builds. When you add a new `<sl-icon name="...">` reference, you **must** also add the icon name to that array, then run `npm run copy:shoelace-icons`. Icons will render in dev mode but appear blank in production if this step is missed.

### Resource Type Icons

| Resource Type | Icon Name | Usage |
|---------------|-----------|-------|
| **Agents** | `cpu` | Agent lists, detail pages, breadcrumbs, group members |
| **Projects** | `folder` | Navigation, dashboard, breadcrumbs |
| **Brokers** | `hdd-rack` | Navigation, broker lists, broker detail |
| **Users** | `people` | Navigation, user lists, user groups |
| **Groups** | `diagram-3` | Navigation, group lists, group detail |
| **Settings** | `gear` | Navigation, project settings |
| **Dashboard** | `house` | Navigation |

### Project Variant Icons

| Variant | Icon Name | Usage |
|---------|-----------|-------|
| **Git-backed project** | `diagram-3` | Project lists, project detail header |
| **Hub workspace** | `folder-fill` | Project lists, project detail header |
| **Empty state** | `folder2-open` | No-projects placeholder |

### Profile & Config Icons

| Resource Type | Icon Name | Usage |
|---------------|-----------|-------|
| **Environment Variables** | `terminal` | Profile nav, env var pages, dashboard |
| **Secrets** | `shield-lock` | Profile nav, secrets pages |

### Individual vs. Collection Icons

| Context | Icon Name | Usage |
|---------|-----------|-------|
| **Single user** | `person` | Group member lists |
| **User avatar** | `person-circle` | Header, profile nav |
| **User collection** | `people` | Navigation, admin pages |

### Common Action Icons

| Action | Icon Name | Usage |
|--------|-----------|-------|
| **Create/Add** | `plus-lg` | Create agent, add items |
| **Create project** | `folder-plus` | Create project action |
| **Back/Return** | `arrow-left-circle` | Return links |
| **Recent activity** | `clock-history` | Dashboard activity section |

## Key Patterns

### Creating Lit Components

Components use standard Lit patterns with TypeScript decorators:

```typescript
import { LitElement, html, css } from 'lit';
import { customElement, property } from 'lit/decorators.js';

@customElement('my-component')
export class MyComponent extends LitElement {
  @property({ type: String })
  myProp = 'default';

  static override styles = css`
    :host { display: block; }
  `;

  override render() {
    return html`<div>${this.myProp}</div>`;
  }
}
```

### Using Shoelace Components

```typescript
render() {
  return html`
    <sl-button variant="primary" @click=${() => this.handleClick()}>
      <sl-icon slot="prefix" name="plus-lg"></sl-icon>
      Create Agent
    </sl-button>

    <sl-badge variant="success">Running</sl-badge>
  `;
}
```

### Theme Variables

Use CSS custom properties with the `--scion-` prefix for consistent theming:

```css
:host {
  background: var(--scion-surface);
  color: var(--scion-text);
  border: 1px solid var(--scion-border);
  border-radius: var(--scion-radius);
}
```

### Dark Mode

Dark mode is handled automatically via CSS custom properties. The theme toggle in the navigation saves the preference to localStorage. Components should use the semantic color variables (e.g., `--scion-surface`, `--scion-text`) which automatically adjust for dark mode.

## Testing Real-Time (SSE) Events

Test scripts for validating real-time event delivery are in `web/test-scripts/`. These were used during the initial validation of the SSE pipeline and remain useful for regression testing.

| Script | Purpose |
|--------|---------|
| `sse-curl-test.sh` | Server-side SSE validation with curl (no browser) |
| `realtime-lifecycle-test.js` | Full browser test with Playwright screenshots |
| `screenshot-debug.js` | Debug tool for blank/broken pages |

### Quick SSE smoke test (no browser needed)

```bash
TOKEN=<dev-token> PROJECT_ID=<uuid> ./web/test-scripts/sse-curl-test.sh
```

### Full browser lifecycle test

```bash
PROJECT_ID=<uuid> TOKEN=<dev-token> node web/test-scripts/realtime-lifecycle-test.js
```

## Containerized / Sandboxed Environments

When working in a containerized or sandboxed agent environment (e.g., scion agents), keep these points in mind:

- **Vite dev server is available.** You can run `npm run dev` to start the Vite dev server for client-side development and visual inspection. API calls and SSE will not work without the Go backend.
- **Use `--dev-auth` for local testing.** When a Go server is available, `--dev-auth` bypasses OAuth and auto-creates a dev session, which is the simplest way to test the frontend end-to-end. See the README for details.
- **Go server** the golang server can be started as a background process, but OAuth flows cannot be used in a container.

### Tool pitfalls in sandboxed environments

- **No `test` script.** There is no `npm test` script. Do not run `npm test` — it will fail. Use the specific verification commands listed in [Common Commands](#common-commands) (`npm run typecheck`, `npm run lint`, `npm run build`).
- **Never use `npx tsc`.** There is a completely unrelated npm package called `tsc` (v2.x) that `npx` will download and run instead of TypeScript's compiler. Always use `npm run typecheck` which invokes the correct local TypeScript binary via the project's package.json script.
- **TypeScript may not be installed globally.** Do not assume `tsc` or `./node_modules/.bin/tsc` are available. The project's `npm run typecheck` script is the only reliable way to type-check.
- **For CSS-only changes**, if `npm run typecheck` is unavailable, `npm run build` is the next best verification — Vite will surface any import or syntax errors during bundling.

## Tips for End-to-End Web Validation

These tips were collected during validation work and are useful for agents debugging or testing the web frontend against the Go backend.

### Server startup

- **Combined mode** runs the Hub API on the web port (default 8080). When `--enable-hub` and `--enable-web` are both set, there is no separate listener on port 9810. All API routes are at `http://localhost:8080/api/v1/`.
- **Runtime broker** must be enabled (`--enable-runtime-broker`) and linked to the project as a provider before agents can be created. With the co-located broker, use `POST /api/v1/projects/{id}/providers` with `{"brokerId":"<id>"}` to link.
- **Dev token** is printed in the server startup logs. Use it as `Authorization: Bearer scion_dev_...` for API calls.
- **`--web-assets-dir`** loads assets from disk so you can rebuild the frontend (`npm run build`) and refresh the browser without restarting the Go server.

### API gotchas

- **Agent status updates** use `POST /api/v1/agents/{id}/status`, not `PATCH`. The handler only accepts POST.
- **Agent creation response** wraps the agent under an `"agent"` key: `{ "agent": { "id": "...", ... }, "warnings": [...] }`.
- **SSE endpoint** (`/events?sub=...`) requires a session cookie, not a Bearer token. To get a session cookie via curl: `curl -c - -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/projects` then pass the `scion_sess` cookie with `-b`.

### SSE event format

The SSE stream uses a single event type `update` with the subject embedded in the data payload:

```
event: update
data: {"subject":"project.xxx.agent.created","data":{"agentId":"...","projectId":"..."}}
```

The client `SSEClient` listens for `event: update` and the `StateManager` parses the `subject` field to route events. If the event type is changed or the subject is used as the SSE event type directly, the client will silently drop events.

### Browser testing with Playwright

- Use `waitUntil: 'domcontentloaded'` instead of `'networkidle'` — the SSE connection keeps the network perpetually active, so `networkidle` will time out.
- Chromium needs `--no-sandbox --disable-setuid-sandbox` flags in containerized environments.
- Console logging via `page.on('console', ...)` is essential for debugging SSE connection state — the `[SSE] Connected` log confirms the EventSource opened.
- To validate real-time updates: take a screenshot, make an API call, wait 2-3 seconds, take another screenshot. Compare visually.

### Common failure modes

- **Blank page**: Check that web assets are built (`npm run build`) and the `--web-assets-dir` flag points to `web/dist/client`. Use `screenshot-debug.js` to see console errors and 404s.
- **SSE events not updating UI**: Check the SSE event type. The client only listens for `event: update`. If the server sends events with the subject as the type, they are silently dropped.
- **Agent delete not reflected**: The `onAgentsUpdated()` handler in `project-detail.ts` must run even when the state manager's agent map is empty (after the last agent is deleted).

## Browser-Based QA with Chrome DevTools MCP

When using the Chrome DevTools MCP tools (`take_screenshot`, `take_snapshot`, `click`, `fill`, etc.) to validate the web UI, follow these guidelines.

### Starting the Go server

1. **Settings must exist first.** The server requires `image_registry` in `~/.scion/settings.json` with `schema_version`. Create this before starting:
   ```json
   {
     "schema_version": "1",
     "image_registry": "dummy.registry.io"
   }
   ```
   Without `schema_version`, the settings file is silently ignored and the server exits with `image_registry is not configured`.

2. **Run the server with `&` in a regular Bash call**, not with `run_in_background`. Background tasks can be hard to retrieve output from. Instead:
   ```bash
   /tmp/scion-test server start --enable-hub --enable-web --dev-auth \
     --web-assets-dir ./web/dist/client --foreground 2>&1 &
   sleep 4
   ss -tlnp | grep 8080  # verify it's listening
   ```

3. **Capture the dev token** from the server startup output. It appears as:
   ```
   Dev token: scion_dev_<hex>
   ```

4. **Build assets before starting the server**: Run `npm install && npm run build` in `web/` first. The server serves from `web/dist/client`.

### Browser authentication

- **Navigate directly to `http://localhost:8080`** — dev-auth mode sets a session cookie automatically on the first page load, no manual cookie setup needed.
- **For API calls via curl**, use the dev token as `Authorization: Bearer scion_dev_...` to get a `scion_sess` cookie, then pass it to subsequent requests.

### Taking screenshots and snapshots

- **Prefer `take_snapshot` (a11y tree) over `take_screenshot`** for finding element UIDs to interact with. Screenshots are for visual validation; snapshots give you the clickable UIDs.
- **Resize the viewport early.** The default headless viewport (800x600) is too small for most pages. Resize to at least 1024x800, or 1280x800 for full table views:
  ```
  resize_page(width=1280, height=800)
  ```
- **Save screenshots to `.scratch/`** with numbered, descriptive filenames (e.g., `01-initial-page.png`, `07-token-reveal.png`) to create a clear audit trail.
- **Read screenshots back** with the `Read` tool to visually inspect them — the tool renders images inline.

### Interacting with dialogs

- **Native `confirm()`/`alert()` dialogs block clicks.** When a component uses `confirm()` (e.g., revoke/delete actions), the `click` call will time out. This is expected. After the timeout error, call `handle_dialog` with `action: "accept"` (or `"dismiss"`) to proceed.
- **Shoelace `<sl-dialog>` modals** are part of the DOM and work normally with `click`/`fill` — they don't trigger the native dialog handler.

### Filling forms in Lit/Shoelace components

- **Use `fill` for `<sl-input>` fields** — it dispatches the correct input events.
- **Use `click` on `<sl-checkbox>` elements** — they toggle on click.
- **`<sl-select>` with a single option** may auto-select; verify via snapshot before filling.
- **After form submission**, take a snapshot to confirm the dialog closed and the list updated.

### Recommended QA workflow

1. Build assets (`npm run build`) and start the server
2. Resize viewport to 1280x800
3. Navigate to the target page, screenshot the initial state
4. Test the primary flow (e.g., create → reveal → list → revoke → delete)
5. Screenshot after each state transition
6. Test dark mode (click the theme toggle) and screenshot
7. Test mobile (resize to 480x800) and screenshot
8. All screenshots go in `.scratch/` for review
