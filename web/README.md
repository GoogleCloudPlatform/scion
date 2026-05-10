# Scion Web Frontend

Browser-based dashboard for managing Scion agents and projects.

## Architecture

The web frontend is a client-side single-page application (SPA) built with Lit web components. Node.js is used only at build time (Vite compiles and bundles client assets). At runtime, the Go `scion` binary serves the compiled assets and handles all server-side concerns (OAuth, sessions, SSE, API routing) via the `--enable-web` flag.

Key architectural points:

- **Single binary** — the Go server serves static assets, handles OAuth, manages sessions, and delivers real-time events via SSE. No Node.js runtime or NATS dependency.
- **SPA shell** — the Go server returns a minimal HTML page; Lit components render entirely client-side.
- **SSE for real-time updates** — an in-process `ChannelEventPublisher` (Go channels) delivers events to the browser. No external message broker required.

## Prerequisites

- Node.js 20.x or later (build-time only)
- npm 10.x or later
- Go 1.22+ (for running the server)

## Getting Started

### Install Dependencies

```bash
cd web
npm install
```

### Build Client Assets

```bash
cd web
npm run build
```

Build output goes to `web/dist/client/`.

## Running the Server

All `scion server start` commands should be run from the **repository root** (the directory containing `cmd/`, `pkg/`, `web/`, etc.). The `--enable-web` flag activates the web frontend alongside the Hub API.

### Development Mode (Dev Auth)

The simplest way to run the web server locally. Dev auth bypasses OAuth entirely — the server auto-creates a development user session with admin privileges.

```bash
scion server start --enable-hub --enable-web --dev-auth \
  --web-assets-dir ./web/dist/client
```

This starts both the **Hub API** and **Web frontend** on `:8080` (combined mode).
In combined mode (`--enable-hub --enable-web`), the Hub API is mounted on the web
server's port and the standalone Hub listener is not started.

Visit `http://localhost:8080`. You'll be automatically logged in as a dev user (`dev@localhost`, admin role).

The `--web-assets-dir` flag tells the server to load assets from disk instead of the embedded copy. This lets you rebuild client assets (`npm run build` in `web/`) and refresh the browser without restarting the Go server.

The `--dev-auth` flag:
- Generates a dev token and prints it to the console
- Sets `SCION_DEV_TOKEN` and `SCION_AUTH_TOKEN` environment variables for the process
- Auto-populates a session for browser requests so no login flow is needed

### Development with Vite HMR

For active frontend development, run the Vite dev server alongside the Go server for hot module replacement:

```bash
# Terminal 1: Go server (API + SSE + auth)
scion server start --enable-hub --enable-web --dev-auth \
  --web-assets-dir ./web/dist/client

# Terminal 2: Vite dev server (HMR)
cd web && npm run dev
```

The Vite dev server runs on `:3000`. You can configure Vite's proxy to forward `/api/*`, `/auth/*`, and `/events` to the Go server, then work from `http://localhost:3000` for full HMR support.

### Testing with OAuth (Google/GitHub)

To test the full OAuth login flow, you need OAuth client credentials for Google and/or GitHub. These are configured via environment variables:

```bash
# Google OAuth (web flow)
export SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENTID="your-google-client-id"
export SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENTSECRET="your-google-client-secret"

# GitHub OAuth (web flow)
export SCION_SERVER_OAUTH_WEB_GITHUB_CLIENTID="your-github-client-id"
export SCION_SERVER_OAUTH_WEB_GITHUB_CLIENTSECRET="your-github-client-secret"
```

Then start the server with a session secret and base URL:

```bash
scion server start --enable-hub --enable-web \
  --session-secret "$(openssl rand -hex 32)" \
  --base-url http://localhost:8080 \
  --web-assets-dir ./web/dist/client
```

The OAuth callback URL to register with your provider is `{base-url}/auth/callback/{provider}` (e.g., `http://localhost:8080/auth/callback/google`).

**Restricting access by email domain:**

Set authorized domains in your `settings.yaml` under `auth.authorized_domains` to restrict which email domains can log in.

**Bootstrapping admin users:**

```bash
scion server start --enable-hub --enable-web \
  --admin-emails "admin@example.com,other@example.com" \
  ...
```

### Production

For production deployments, the Go binary embeds the client assets at build time. No `--web-assets-dir` flag is needed:

```bash
scion server start --enable-hub --enable-web \
  --session-secret "$SESSION_SECRET" \
  --base-url https://scion.example.com
```

The `--session-secret` should be a stable, random 32+ byte hex string. If omitted, a random secret is generated at startup (sessions won't survive restarts).

## Server Configuration Reference

### CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--enable-web` | `false` | Enable the web frontend |
| `--web-port` | `8080` | Web frontend port |
| `--web-assets-dir` | (embedded) | Path to client assets directory; overrides embedded assets |
| `--session-secret` | (auto-generated) | HMAC secret for signing session cookies |
| `--base-url` | `http://localhost:{web-port}` | Public base URL for OAuth redirects |
| `--dev-auth` | `false` | Enable dev auth (auto-generates token, bypasses OAuth) |
| `--admin-emails` | | Comma-separated emails to auto-promote to admin role |
| `--port` | `9810` | Hub API port (standalone mode only; in combined mode with `--enable-web`, Hub API is served on `--web-port`) |
| `--debug` | `false` | Enable debug logging and `/auth/debug` endpoint |

### Environment Variables

| Variable | Description |
|----------|-------------|
| `SCION_SERVER_SESSION_SECRET` | Session cookie signing secret (fallback if `--session-secret` not set) |
| `SCION_SERVER_BASE_URL` | Public URL for OAuth redirects (fallback if `--base-url` not set) |
| `SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENTID` | Google OAuth client ID (web flow) |
| `SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENTSECRET` | Google OAuth client secret (web flow) |
| `SCION_SERVER_OAUTH_WEB_GITHUB_CLIENTID` | GitHub OAuth client ID (web flow) |
| `SCION_SERVER_OAUTH_WEB_GITHUB_CLIENTSECRET` | GitHub OAuth client secret (web flow) |

### Web Server Routes

| Route | Auth | Description |
|-------|------|-------------|
| `/assets/*` | None | Static assets (Vite build output) |
| `/auth/login/:provider` | None | Initiate OAuth login (google, github) |
| `/auth/callback/:provider` | None | OAuth callback handler |
| `/auth/logout` | Session | Logout and clear session |
| `/auth/me` | Session | Current user info (JSON) |
| `/auth/debug` | Session | Debug info (only when `--debug` is set) |
| `/events?sub=...` | Session | SSE event stream |
| `/healthz` | None | Health check |
| `/*` | Session | SPA shell (catch-all) |

### SSE Subscriptions

The `/events` endpoint accepts `sub` query parameters with NATS-style subject patterns:

```bash
# Subscribe to all events in a project
curl -N "http://localhost:8080/events?sub=project.PROJECT_ID.>"

# Subscribe to multiple patterns
curl -N "http://localhost:8080/events?sub=project.abc.>&sub=broker.xyz.status"
```

Wildcards: `*` matches a single token, `>` matches the remainder.

## Available Scripts

| Script | Description |
|--------|-------------|
| `npm run dev` | Start Vite dev server with hot reload |
| `npm run build` | Build client assets for production |
| `npm run build:dev` | Build client assets in development mode |
| `npm run lint` | Run ESLint |
| `npm run lint:fix` | Run ESLint with auto-fix |
| `npm run format` | Format code with Prettier |
| `npm run format:check` | Check code formatting |
| `npm run typecheck` | Run TypeScript type checking |
| `npm run clean` | Remove node_modules, dist, and public/assets |

## Project Structure

```
web/
├── src/
│   ├── client/              # Browser-side code
│   │   ├── main.ts          # Client entry point
│   │   ├── state.ts         # State manager with SSE subscriptions
│   │   └── sse-client.ts    # SSE client for real-time updates
│   ├── components/          # Lit web components
│   │   ├── app-shell.ts     # Main application shell
│   │   ├── shared/          # Reusable UI components
│   │   └── pages/           # Page components
│   ├── styles/              # CSS theme and utilities
│   │   ├── theme.css        # CSS custom properties, light/dark mode
│   │   └── utilities.css    # Utility classes
│   └── shared/              # Shared types
│       └── types.ts         # Type definitions
├── public/                  # Static assets (built output copied here)
├── package.json
├── tsconfig.json
└── vite.config.ts
```
