# Scion Web Frontend Design

## Status
**Proposed**

## 1. Overview

The Scion Web Frontend provides a browser-based dashboard for managing agents, groves, and monitoring system status. It is built as a modern server-rendered web application using Lit web components with server-side rendering (SSR) via Koa and `@lit-labs/ssr`.

### Design Goals

1. **Progressive Enhancement:** Server-rendered HTML with hydration for interactive features
2. **Real-Time Updates:** Snapshot + Delta pattern for efficient state synchronization
3. **Component-Driven:** Web Awesome (Shoelace-based) component library with Lit
4. **Minimal Client Complexity:** Server handles API integration; client focuses on presentation
5. **Cloud-Native Deployment:** Optimized for Cloud Run with fast cold starts
6. **Unified Styling:** Web Awesome provides consistent theming with built-in Shoelace integration

### Technology Stack

| Layer | Technology | Purpose |
|-------|------------|---------|
| **Components** | Lit 3.x | Web component framework |
| **UI Library** | Web Awesome | Pre-built component library (Shoelace-based) |
| **Server** | Koa 2.x | Lightweight Node.js web framework |
| **SSR** | @lit-labs/ssr | Server-side rendering for Lit components |
| **Terminal** | xterm.js | PTY display in browser |
| **Real-time** | SSE + NATS | Server-Sent Events backed by NATS pub/sub (state); WebSocket for PTY only |
| **Build** | Vite | Fast builds with ES modules |
| **Deployment** | Cloud Run | Serverless container hosting |

---

## 2. Architecture

### 2.1 High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Browser                                         │
│  ┌─────────────────────────────────────────────────────────────────────┐    │
│  │                     Lit Web Components                               │    │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐               │    │
│  │  │  Agent List  │  │  Grove View  │  │  Terminal    │               │    │
│  │  │  Component   │  │  Component   │  │  Component   │               │    │
│  │  └──────────────┘  └──────────────┘  └──────────────┘               │    │
│  │                           │                                          │    │
│  │                    ┌──────┴──────┐                                   │    │
│  │                    │ SSE Client  │  ◄─── Receives deltas             │    │
│  │                    └─────────────┘                                   │    │
│  └─────────────────────────────────────────────────────────────────────┘    │
└────────────────────────────────────┬────────────────────────────────────────┘
                                     │ HTTPS / WebSocket
                                     ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Web Frontend (Koa)                                 │
│  Port: 9820                                                                  │
│                                                                              │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐         │
│  │  SSR        │  │  SSE        │  │  API        │  │  Static     │         │
│  │  Renderer   │  │  Endpoint   │  │  Proxy      │  │  Assets     │         │
│  │  (@lit/ssr) │  │  (/events)  │  │  (/api)     │  │  (/assets)  │         │
│  └──────┬──────┘  └──────┬──────┘  └──────┬──────┘  └─────────────┘         │
│         │                │                │                                  │
│         │         ┌──────┴──────┐         │                                  │
│         │         │   NATS      │         │                                  │
│         │         │   Client    │         │                                  │
│         │         └──────┬──────┘         │                                  │
│         │                │                │                                  │
│         └────────────────┼────────────────┘                                  │
│                          │                                                   │
└──────────────────────────┼───────────────────────────────────────────────────┘
                           │ HTTP + NATS
                           ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Hub API (:9810)                                    │
│                                                                              │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐                          │
│  │  REST API   │  │  WebSocket  │  │  NATS       │                          │
│  │  Endpoints  │  │  PTY/Events │  │  Publisher  │                          │
│  └─────────────┘  └─────────────┘  └─────────────┘                          │
│                          │                                                   │
│                    ┌─────┴─────┐                                             │
│                    │  SQL DB   │                                             │
│                    └───────────┘                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 2.2 Data Models

The web frontend interacts with the following core data models from the Hub API. See `hub-api.md` for complete specifications.

#### Core Resources

| Model | Description | Key Operations |
|-------|-------------|----------------|
| **Agent** | Running or stopped agent instance | List, Create, Start, Stop, Delete, Attach PTY |
| **Grove** | Project grouping of agents | List, View, Settings, Contributors |
| **Template** | Agent configuration blueprint | List, Create, Upload, Clone, Delete |
| **User** | Registered user account | Profile, Preferences |
| **Group** | Collection of users for access control | List, Create, Manage Members |
| **Policy** | Access control rules | List, Create, Evaluate |
| **EnvVar** | Environment variable (scoped) | List, Set, Delete |
| **Secret** | Write-only secret value (scoped) | List, Set, Delete |

#### Identity & Access Models

```typescript
// User identity
interface User {
  id: string;
  email: string;
  displayName: string;
  avatarUrl?: string;
  role: 'admin' | 'member' | 'viewer';
  status: 'active' | 'suspended';
  created: string;
  lastLogin: string;
}

// Group for access control
interface Group {
  id: string;
  name: string;
  slug: string;
  description?: string;
  parentId?: string;
  memberCount: number;
  created: string;
  updated: string;
}

// Policy for authorization
interface Policy {
  id: string;
  name: string;
  description?: string;
  scopeType: 'hub' | 'grove' | 'resource';
  scopeId?: string;
  resourceType: string;
  actions: string[];
  effect: 'allow' | 'deny';
  priority: number;
  created: string;
  updated: string;
}
```

#### Template Model

```typescript
interface Template {
  id: string;
  name: string;
  slug: string;
  displayName: string;
  description?: string;
  harness: 'claude' | 'gemini' | 'codex' | 'opencode' | 'generic';
  contentHash: string;
  scope: 'global' | 'grove' | 'user';
  scopeId?: string;
  storageUri?: string;
  config: TemplateConfig;
  files: TemplateFile[];
  visibility: 'private' | 'grove' | 'public';
  ownerId: string;
  status: 'pending' | 'active' | 'archived';
  created: string;
  updated: string;
  updatedBy: string;
}

interface TemplateFile {
  path: string;
  size: number;
  hash: string;
  mode: string;
}
```

#### Environment & Secrets Models

```typescript
interface EnvVar {
  id: string;
  key: string;
  value: string;
  scope: 'user' | 'grove' | 'runtime_broker';
  scopeId: string;
  description?: string;
  sensitive: boolean;
  created: string;
  updated: string;
}

interface Secret {
  id: string;
  key: string;
  scope: 'user' | 'grove' | 'runtime_broker';
  scopeId: string;
  description?: string;
  version: number;
  created: string;
  updated: string;
  // Note: value is never returned from API
}
```

### 2.3 Snapshot + Delta Pattern

This pattern provides efficient real-time updates with minimal data transfer:

```
┌─────────────────────────────────────────────────────────────────────────┐
│                    Snapshot + Delta Data Flow                            │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│  1. Initial Load (Snapshot)                                              │
│     ┌────────┐     ┌──────────┐     ┌─────────┐     ┌─────────┐         │
│     │Browser │────►│Koa Server│────►│ Hub API │────►│  SQL DB │         │
│     │        │◄────│   SSR    │◄────│  REST   │◄────│         │         │
│     └────────┘     └──────────┘     └─────────┘     └─────────┘         │
│         │          Full HTML with                                        │
│         │          current state                                         │
│         ▼                                                                │
│  2. Hydration (Client connects to SSE)                                   │
│     ┌────────┐     ┌──────────┐                                         │
│     │Browser │────►│Koa SSE   │  Client sends: GET /events              │
│     │ Lit    │◄────│ Endpoint │  Server holds connection open           │
│     └────────┘     └──────────┘                                         │
│         │                │                                               │
│         │                ▼                                               │
│  3. Live Wire (NATS subscription)                                        │
│                    ┌──────────┐     ┌─────────┐                         │
│                    │Koa NATS  │◄────│  NATS   │  Subject: resource.*    │
│                    │ Client   │     │  Server │                         │
│                    └──────────┘     └─────────┘                         │
│                         │                ▲                               │
│                         │                │                               │
│  4. Side-Effect Trigger                  │                               │
│     ┌─────────┐     ┌─────────┐         │                               │
│     │  SQL DB │────►│ Hub API │─────────┘  Publishes on DB change       │
│     │ Updated │     │ Service │             to NATS                      │
│     └─────────┘     └─────────┘                                         │
│                         │                                                │
│  5. Reactive Update     │                                                │
│     ┌────────┐     ┌────┴─────┐                                         │
│     │Browser │◄────│Koa SSE   │  Pushes delta payload                   │
│     │Updates │     │ Stream   │  via SSE                                │
│     │  DOM   │     └──────────┘                                         │
│     └────────┘                                                           │
│                                                                          │
└─────────────────────────────────────────────────────────────────────────┘
```

#### Data Flow Steps

1. **Initial Load (Snapshot):**
   - Browser requests a page (e.g., `/groves/my-project/agents`)
   - Koa server queries Hub API for current state
   - @lit-labs/ssr renders Lit components to HTML with current data
   - Full HTML response sent to browser

2. **Hydration:**
   - Lit components hydrate (become interactive)
   - Client opens SSE connection to `/events?sub=...` with view-scoped subscription subjects as query parameters
   - Server subscribes to the declared NATS subjects for this connection

3. **Live Wire (NATS):**
   - Koa server subscribes to NATS subjects declared in the SSE connection's query parameters
   - Subject scope matches the current view (see Section 12.2 for subscription tiers)
   - Example: grove view subscribes to `grove.{groveId}.>`, agent detail adds `agent.{agentId}.>`

4. **Side-Effect Trigger:**
   - Hub API publishes to NATS when database changes occur
   - Payload contains minimal delta information

5. **Reactive Update:**
   - Koa receives NATS message, pushes to relevant SSE connections
   - Lit components receive delta and update only affected properties
   - Lit's efficient DOM diffing minimizes re-renders

---

## 3. Server Architecture (Koa)

### 3.1 Koa Application Structure

```
web/
├── src/
│   ├── server/
│   │   ├── index.ts              # Koa app entry point
│   │   ├── app.ts                # Koa application setup
│   │   ├── middleware/
│   │   │   ├── auth.ts           # Session/JWT validation
│   │   │   ├── error-handler.ts  # Error boundary
│   │   │   ├── logger.ts         # Request logging
│   │   │   ├── static.ts         # Static asset serving
│   │   │   └── security.ts       # CORS, CSP, etc.
│   │   ├── routes/
│   │   │   ├── index.ts          # Route aggregation
│   │   │   ├── pages.ts          # SSR page routes
│   │   │   ├── api-proxy.ts      # Hub API proxy
│   │   │   ├── sse.ts            # SSE endpoint
│   │   │   ├── auth.ts           # OAuth routes
│   │   │   └── health.ts         # Health checks
│   │   ├── services/
│   │   │   ├── hub-client.ts     # Hub API client
│   │   │   ├── nats-client.ts    # NATS connection
│   │   │   ├── sse-manager.ts    # SSE connection management
│   │   │   └── session.ts        # Session store
│   │   ├── ssr/
│   │   │   ├── renderer.ts       # Lit SSR renderer
│   │   │   ├── templates.ts      # HTML shell templates
│   │   │   └── hydration.ts      # Hydration script generation
│   │   └── config.ts             # Server configuration
│   ├── components/               # Lit components (shared client/server)
│   │   ├── app-shell.ts          # Main application shell
│   │   ├── pages/
│   │   │   ├── dashboard.ts
│   │   │   ├── grove-list.ts
│   │   │   ├── grove-detail.ts
│   │   │   ├── agent-list.ts
│   │   │   ├── agent-detail.ts
│   │   │   ├── terminal.ts
│   │   │   ├── template-list.ts
│   │   │   ├── template-detail.ts
│   │   │   ├── template-upload.ts
│   │   │   ├── user-list.ts
│   │   │   ├── user-detail.ts
│   │   │   ├── group-list.ts
│   │   │   ├── group-detail.ts
│   │   │   ├── policy-list.ts
│   │   │   ├── policy-detail.ts
│   │   │   ├── settings-env.ts
│   │   │   ├── settings-secrets.ts
│   │   │   └── api-keys.ts
│   │   ├── shared/
│   │   │   ├── agent-card.ts
│   │   │   ├── status-badge.ts
│   │   │   ├── grove-selector.ts
│   │   │   ├── action-menu.ts
│   │   │   ├── template-card.ts
│   │   │   ├── template-selector.ts
│   │   │   ├── user-avatar.ts
│   │   │   ├── group-badge.ts
│   │   │   ├── permission-badge.ts
│   │   │   ├── scope-selector.ts
│   │   │   ├── env-var-editor.ts
│   │   │   ├── secret-editor.ts
│   │   │   ├── policy-editor.ts
│   │   │   ├── member-list.ts
│   │   │   └── file-upload.ts
│   │   └── terminal/
│   │       ├── pty-viewer.ts
│   │       └── xterm-wrapper.ts
│   ├── styles/
│   │   ├── theme.css             # Web Awesome theme overrides
│   │   └── utilities.css         # Utility classes
│   └── client/
│       ├── main.ts               # Client entry point
│       ├── sse-client.ts         # SSE connection handler
│       ├── router.ts             # Client-side routing
│       └── state.ts              # Client state management
├── public/
│   └── assets/                   # Static assets
├── vite.config.ts
├── tsconfig.json
└── package.json
```

### 3.2 Koa Middleware Stack

```typescript
// src/server/app.ts
import Koa from 'koa';
import Router from '@koa/router';
import session from 'koa-session';
import cors from '@koa/cors';
import serve from 'koa-static';
import { errorHandler } from './middleware/error-handler';
import { logger } from './middleware/logger';
import { auth } from './middleware/auth';
import { security } from './middleware/security';
import { pageRoutes } from './routes/pages';
import { apiProxy } from './routes/api-proxy';
import { sseRoutes } from './routes/sse';
import { authRoutes } from './routes/auth';
import { healthRoutes } from './routes/health';

export function createApp(config: AppConfig): Koa {
  const app = new Koa();
  const router = new Router();

  // Trust proxy headers (Cloud Run)
  app.proxy = true;

  // Core middleware
  app.use(errorHandler());
  app.use(logger());
  app.use(security(config));
  app.use(cors(config.cors));
  app.use(session(config.session, app));

  // Static assets (with caching)
  app.use(serve('public', {
    maxAge: config.production ? 86400000 : 0, // 24h in prod
    gzip: true,
    brotli: true
  }));

  // Health checks (unauthenticated)
  router.use('/healthz', healthRoutes.routes());
  router.use('/readyz', healthRoutes.routes());

  // Auth routes (login, logout, callback)
  router.use('/auth', authRoutes.routes());

  // API proxy (requires auth)
  router.use('/api', auth(), apiProxy.routes());

  // SSE endpoint (requires auth)
  router.use('/events', auth(), sseRoutes.routes());

  // SSR pages (requires auth for protected routes)
  router.use('/', pageRoutes.routes());

  app.use(router.routes());
  app.use(router.allowedMethods());

  return app;
}
```

### 3.3 SSR Renderer

```typescript
// src/server/ssr/renderer.ts
import { render } from '@lit-labs/ssr';
import { html } from 'lit';
import { unsafeHTML } from 'lit/directives/unsafe-html.js';
import { collectResult } from '@lit-labs/ssr/lib/render-result.js';
import { getHtmlTemplate } from './templates';

export interface RenderContext {
  url: string;
  user: User | null;
  initialData: Record<string, unknown>;
}

export async function renderPage(
  component: unknown,
  ctx: RenderContext
): Promise<string> {
  // Render the Lit component to HTML
  const componentHtml = await collectResult(render(component));

  // Wrap in HTML shell with hydration script
  const fullHtml = getHtmlTemplate({
    title: getPageTitle(ctx.url),
    content: componentHtml,
    initialData: ctx.initialData,
    user: ctx.user,
    scripts: ['/assets/main.js'],
    styles: ['/assets/main.css']
  });

  return fullHtml;
}

// src/server/ssr/templates.ts
export function getHtmlTemplate(opts: HtmlTemplateOptions): string {
  return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>${opts.title} - Scion</title>

  <!-- Web Awesome / Shoelace -->
  <link rel="stylesheet" href="https://cdn.webawesome.com/dist/themes/default.css">
  <script type="module" src="https://cdn.webawesome.com/dist/webawesome.js"></script>

  <!-- App styles -->
  ${opts.styles.map(s => `<link rel="stylesheet" href="${s}">`).join('\n  ')}

  <!-- Initial state for hydration -->
  <script id="__SCION_DATA__" type="application/json">
    ${JSON.stringify(opts.initialData)}
  </script>
</head>
<body>
  <div id="app">${opts.content}</div>

  <!-- Hydration scripts -->
  ${opts.scripts.map(s => `<script type="module" src="${s}"></script>`).join('\n  ')}
</body>
</html>`;
}
```

### 3.4 SSE Manager

Subscriptions are declared at connection creation time (via query parameters on the SSE endpoint). There is no in-band subscription mutation — the client closes the connection and opens a new one when navigating to a different view scope. This keeps each connection stateless and maps directly to the future gRPC `WatchRequest` pattern.

```typescript
// src/server/services/sse-manager.ts
import { PassThrough } from 'stream';
import { NatsClient } from './nats-client';

interface SSEConnection {
  id: string;
  stream: PassThrough;
  userId: string;
  subjects: string[];
  lastEventId: number;
}

export class SSEManager {
  private connections = new Map<string, SSEConnection>();
  private natsClient: NatsClient;

  constructor(natsClient: NatsClient) {
    this.natsClient = natsClient;
  }

  // Create a connection with all subscriptions declared upfront.
  // Subjects are immutable for the lifetime of the connection.
  async createConnection(
    userId: string,
    subjects: string[],
    resumeFrom: number = 0
  ): Promise<SSEConnection> {
    const id = crypto.randomUUID();
    const stream = new PassThrough();

    const conn: SSEConnection = {
      id,
      stream,
      userId,
      subjects,
      lastEventId: resumeFrom
    };

    this.connections.set(id, conn);

    // Subscribe to all NATS subjects for this connection
    for (const subject of subjects) {
      await this.natsClient.subscribe(subject, (data, natsSubject) => {
        this.sendEvent(conn, 'update', {
          subject: natsSubject,
          data,
          timestamp: Date.now()
        });
      });
    }

    // Send initial connection event
    this.sendEvent(conn, 'connected', {
      connectionId: id,
      subjects
    });

    return conn;
  }

  private sendEvent(conn: SSEConnection, type: string, data: unknown): void {
    conn.lastEventId++;
    const payload = JSON.stringify(data);
    conn.stream.write(`id: ${conn.lastEventId}\n`);
    conn.stream.write(`event: ${type}\n`);
    conn.stream.write(`data: ${payload}\n\n`);
  }

  removeConnection(connId: string): void {
    const conn = this.connections.get(connId);
    if (!conn) return;

    // Unsubscribe from all NATS subjects
    conn.subjects.forEach(subject => {
      this.natsClient.unsubscribe(subject);
    });

    conn.stream.end();
    this.connections.delete(connId);
  }
}
```

### 3.5 NATS Client

```typescript
// src/server/services/nats-client.ts
import { connect, NatsConnection, Subscription } from 'nats';

export interface NatsConfig {
  servers: string[];
  token?: string;
  reconnect: boolean;
  maxReconnectAttempts: number;
}

export class NatsClient {
  private connection: NatsConnection | null = null;
  private subscriptions = new Map<string, Subscription>();

  async connect(config: NatsConfig): Promise<void> {
    this.connection = await connect({
      servers: config.servers,
      token: config.token,
      reconnect: config.reconnect,
      maxReconnectAttempts: config.maxReconnectAttempts
    });

    console.log(`Connected to NATS: ${this.connection.getServer()}`);
  }

  async subscribe(
    subject: string,
    handler: (data: unknown) => void
  ): Promise<void> {
    if (!this.connection) {
      throw new Error('NATS not connected');
    }

    const sub = this.connection.subscribe(subject);
    this.subscriptions.set(subject, sub);

    (async () => {
      for await (const msg of sub) {
        try {
          const data = JSON.parse(new TextDecoder().decode(msg.data));
          handler(data);
        } catch (err) {
          console.error('Failed to parse NATS message:', err);
        }
      }
    })();
  }

  async publish(subject: string, data: unknown): Promise<void> {
    if (!this.connection) {
      throw new Error('NATS not connected');
    }

    const payload = new TextEncoder().encode(JSON.stringify(data));
    this.connection.publish(subject, payload);
  }

  unsubscribe(subject: string): void {
    const sub = this.subscriptions.get(subject);
    if (sub) {
      sub.unsubscribe();
      this.subscriptions.delete(subject);
    }
  }

  async close(): Promise<void> {
    if (this.connection) {
      await this.connection.drain();
      this.connection = null;
    }
  }
}
```

### 3.6 Hub API Proxy

```typescript
// src/server/routes/api-proxy.ts
import Router from '@koa/router';
import { Context } from 'koa';
import httpProxy from 'http-proxy-middleware';

const router = new Router();

// Proxy configuration
const hubApiUrl = process.env.HUB_API_URL || 'http://localhost:9810';

// Proxy all /api/* requests to Hub API
router.all('/(.*)', async (ctx: Context) => {
  const targetPath = `/api/v1/${ctx.params[0]}`;

  const response = await fetch(`${hubApiUrl}${targetPath}`, {
    method: ctx.method,
    headers: {
      'Content-Type': 'application/json',
      'Authorization': ctx.headers.authorization || '',
      'X-Request-ID': ctx.state.requestId,
      'X-Forwarded-For': ctx.ip
    },
    body: ctx.method !== 'GET' ? JSON.stringify(ctx.request.body) : undefined
  });

  ctx.status = response.status;
  ctx.set('Content-Type', response.headers.get('Content-Type') || 'application/json');

  // Forward rate limit headers
  const rateLimitHeaders = ['X-RateLimit-Limit', 'X-RateLimit-Remaining', 'X-RateLimit-Reset'];
  rateLimitHeaders.forEach(header => {
    const value = response.headers.get(header);
    if (value) ctx.set(header, value);
  });

  ctx.body = await response.json();
});

export const apiProxy = router;
```

---

## 4. Client Architecture (Lit Components)

### 4.1 Component Library: Web Awesome

Web Awesome is a component library built on Shoelace, providing:

- **Pre-styled components:** Buttons, cards, dialogs, tables, etc.
- **Built-in theming:** Shoelace-compatible CSS custom properties
- **Accessibility:** WCAG 2.1 AA compliant
- **Framework-agnostic:** Works with Lit, React, Vue, or vanilla JS

```html
<!-- Example: Using Web Awesome components -->
<wa-card>
  <div slot="header">
    <wa-icon name="box"></wa-icon>
    Agent Status
  </div>
  <wa-badge variant="success">Running</wa-badge>
  <div slot="footer">
    <wa-button variant="primary">Attach Terminal</wa-button>
    <wa-button variant="danger">Stop</wa-button>
  </div>
</wa-card>
```

### 4.2 Core Components

#### App Shell

```typescript
// src/components/app-shell.ts
import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { Router } from '@vaadin/router';

import './pages/dashboard';
import './pages/grove-list';
import './pages/grove-detail';
import './pages/agent-list';
import './pages/agent-detail';
import './pages/terminal';

@customElement('scion-app')
export class ScionApp extends LitElement {
  @property({ type: Object }) user: User | null = null;
  @state() private currentRoute = '';

  static styles = css`
    :host {
      display: flex;
      min-height: 100vh;
    }

    .sidebar {
      width: 260px;
      background: var(--wa-color-neutral-100);
      border-right: 1px solid var(--wa-color-neutral-200);
    }

    .main {
      flex: 1;
      display: flex;
      flex-direction: column;
    }

    .header {
      height: 60px;
      padding: 0 1.5rem;
      display: flex;
      align-items: center;
      justify-content: space-between;
      border-bottom: 1px solid var(--wa-color-neutral-200);
    }

    .content {
      flex: 1;
      padding: 1.5rem;
      overflow: auto;
    }
  `;

  firstUpdated() {
    const outlet = this.shadowRoot?.querySelector('#outlet');
    if (outlet) {
      const router = new Router(outlet);
      router.setRoutes([
        { path: '/', component: 'scion-dashboard' },
        { path: '/groves', component: 'scion-grove-list' },
        { path: '/groves/:groveId', component: 'scion-grove-detail' },
        { path: '/groves/:groveId/agents', component: 'scion-agent-list' },
        { path: '/agents/:agentId', component: 'scion-agent-detail' },
        { path: '/agents/:agentId/terminal', component: 'scion-terminal' },
      ]);
    }
  }

  render() {
    return html`
      <aside class="sidebar">
        <scion-nav .user=${this.user}></scion-nav>
      </aside>
      <main class="main">
        <header class="header">
          <scion-breadcrumb></scion-breadcrumb>
          <scion-user-menu .user=${this.user}></scion-user-menu>
        </header>
        <div class="content">
          <div id="outlet"></div>
        </div>
      </main>
    `;
  }
}
```

#### Agent Card Component

```typescript
// src/components/shared/agent-card.ts
import { LitElement, html, css } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import type { Agent } from '../types';

@customElement('scion-agent-card')
export class AgentCard extends LitElement {
  @property({ type: Object }) agent!: Agent;

  static styles = css`
    :host {
      display: block;
    }

    wa-card {
      --wa-card-border-radius: var(--wa-border-radius-large);
    }

    .header {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .title {
      font-weight: 600;
      font-size: 1rem;
    }

    .template {
      font-size: 0.875rem;
      color: var(--wa-color-neutral-600);
    }

    .status-row {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-top: 1rem;
    }

    .actions {
      display: flex;
      gap: 0.5rem;
    }
  `;

  private getStatusVariant(status: string): string {
    const variants: Record<string, string> = {
      running: 'success',
      stopped: 'neutral',
      provisioning: 'warning',
      error: 'danger'
    };
    return variants[status] || 'neutral';
  }

  private handleAction(action: string) {
    this.dispatchEvent(new CustomEvent('agent-action', {
      detail: { agentId: this.agent.id, action },
      bubbles: true,
      composed: true
    }));
  }

  render() {
    const { agent } = this;

    return html`
      <wa-card>
        <div slot="header" class="header">
          <wa-icon name="cpu"></wa-icon>
          <div>
            <div class="title">${agent.name}</div>
            <div class="template">${agent.template}</div>
          </div>
        </div>

        <div class="status-row">
          <wa-badge variant="${this.getStatusVariant(agent.status)}">
            ${agent.status}
          </wa-badge>
          ${agent.sessionStatus ? html`
            <wa-badge variant="primary" size="small">
              ${agent.sessionStatus}
            </wa-badge>
          ` : ''}
        </div>

        ${agent.taskSummary ? html`
          <p class="task">${agent.taskSummary}</p>
        ` : ''}

        <div slot="footer" class="actions">
          <wa-button
            variant="primary"
            size="small"
            @click=${() => this.handleAction('terminal')}
            ?disabled=${agent.status !== 'running'}
          >
            <wa-icon slot="prefix" name="terminal"></wa-icon>
            Terminal
          </wa-button>
          ${agent.status === 'running' ? html`
            <wa-button
              variant="danger"
              size="small"
              @click=${() => this.handleAction('stop')}
            >
              Stop
            </wa-button>
          ` : html`
            <wa-button
              variant="success"
              size="small"
              @click=${() => this.handleAction('start')}
            >
              Start
            </wa-button>
          `}
        </div>
      </wa-card>
    `;
  }
}
```

### 4.3 SSE Client

Subscriptions are declared as query parameters on the SSE endpoint URL. To change subscriptions (e.g., on navigation), the client closes the current connection and opens a new one with different parameters. There is no in-band subscription mutation.

```typescript
// src/client/sse-client.ts

export interface SSEUpdateEvent {
  subject: string;
  data: unknown;
  timestamp: number;
}

export class SSEClient extends EventTarget {
  private eventSource: EventSource | null = null;
  private reconnectAttempts = 0;
  private maxReconnectAttempts = 10;
  private reconnectDelay = 1000;
  private subjects: string[] = [];

  // Build the SSE URL with subscription subjects as query params.
  // Maps to the future gRPC WatchRequest pattern.
  private buildUrl(subjects: string[]): string {
    const params = subjects.map(s => `sub=${encodeURIComponent(s)}`).join('&');
    return `/events?${params}`;
  }

  // Open a connection scoped to the given subjects.
  // Closes any existing connection first.
  connect(subjects: string[]): void {
    this.disconnect();
    this.subjects = subjects;
    this.reconnectAttempts = 0;

    const url = this.buildUrl(subjects);
    this.eventSource = new EventSource(url, {
      withCredentials: true
    });

    this.eventSource.onopen = () => {
      this.reconnectAttempts = 0;
      this.dispatchEvent(new CustomEvent('connected'));
    };

    this.eventSource.onerror = () => {
      this.handleReconnect();
    };

    // Handle state update events
    this.eventSource.addEventListener('update', (event) => {
      const data = JSON.parse((event as MessageEvent).data);
      this.dispatchEvent(new CustomEvent('update', { detail: data }));
    });

    this.eventSource.addEventListener('connected', (event) => {
      const data = JSON.parse((event as MessageEvent).data);
      console.log('SSE connection:', data.connectionId, 'subjects:', data.subjects);
    });
  }

  private handleReconnect(): void {
    if (this.eventSource) {
      this.eventSource.close();
    }

    if (this.reconnectAttempts < this.maxReconnectAttempts) {
      this.reconnectAttempts++;
      const delay = this.reconnectDelay * Math.pow(2, this.reconnectAttempts - 1);

      // Reconnect with same subjects (EventSource also handles
      // Last-Event-ID automatically for resume)
      setTimeout(() => this.connect(this.subjects), delay);
    } else {
      this.dispatchEvent(new CustomEvent('disconnected'));
    }
  }

  disconnect(): void {
    if (this.eventSource) {
      this.eventSource.close();
      this.eventSource = null;
    }
  }
}
```

### 4.4 Reactive State Management

The StateManager uses **view-scoped subscriptions** — the subscription scope follows navigation, not individual entities. When the user navigates from a grove list to a grove detail view, the SSE connection is closed and reopened with different subject parameters. The StateManager maintains a full in-memory state map for whatever scope is active; pagination only affects which slice is rendered, not what is subscribed to.

```typescript
// src/client/state.ts
import { SSEClient, SSEUpdateEvent } from './sse-client';

// Subscription scope matches view context
export type ViewScope =
  | { type: 'dashboard' }
  | { type: 'grove'; groveId: string }
  | { type: 'agent-detail'; groveId: string; agentId: string };

export interface AppState {
  agents: Map<string, Agent>;
  groves: Map<string, Grove>;
  connected: boolean;
  scope: ViewScope | null;
}

export class StateManager extends EventTarget {
  private state: AppState = {
    agents: new Map(),
    groves: new Map(),
    connected: false,
    scope: null
  };

  private sseClient = new SSEClient();

  constructor() {
    super();

    this.sseClient.addEventListener('update', ((event: CustomEvent<SSEUpdateEvent>) => {
      this.handleUpdate(event.detail);
    }) as EventListener);

    this.sseClient.addEventListener('connected', () => {
      this.state.connected = true;
      this.notify('connected');
    });

    this.sseClient.addEventListener('disconnected', () => {
      this.state.connected = false;
      this.notify('disconnected');
    });
  }

  // Initialize with server-rendered data
  hydrate(initialData: { agents?: Agent[]; groves?: Grove[] }): void {
    if (initialData.agents) {
      initialData.agents.forEach(agent => {
        this.state.agents.set(agent.id, agent);
      });
    }

    if (initialData.groves) {
      initialData.groves.forEach(grove => {
        this.state.groves.set(grove.id, grove);
      });
    }
  }

  // Set the view scope. This closes any existing SSE connection
  // and opens a new one with subjects matching the view context.
  // Called by the router on navigation.
  setScope(scope: ViewScope): void {
    this.state.scope = scope;
    const subjects = this.subjectsForScope(scope);
    this.sseClient.connect(subjects);
  }

  // Map view scope to NATS subject patterns.
  // Matches the subscription tiers defined in Section 12.2.
  private subjectsForScope(scope: ViewScope): string[] {
    switch (scope.type) {
      case 'dashboard':
        return ['grove.*.summary'];

      case 'grove':
        // Grove-level wildcard receives all lightweight/medium events
        // for agents within this grove, plus grove metadata changes.
        return [`grove.${scope.groveId}.>`];

      case 'agent-detail':
        // Keep grove subscription for breadcrumb/sidebar freshness.
        // Add agent-specific subscription for heavy events (harness output).
        return [
          `grove.${scope.groveId}.>`,
          `agent.${scope.agentId}.>`
        ];
    }
  }

  // Handle delta updates from SSE
  private handleUpdate(update: SSEUpdateEvent): void {
    const { subject, data } = update;
    const parts = subject.split('.');

    if (parts[0] === 'agent') {
      this.handleAgentEvent(parts[1], parts[2], data);
    }

    if (parts[0] === 'grove') {
      const groveId = parts[1];
      if (parts[2] === 'agent') {
        // Agent event within grove: grove.{groveId}.agent.{eventType}
        this.handleAgentEvent(
          (data as { agentId: string }).agentId,
          parts[3],
          data
        );
      } else {
        // Grove metadata event
        const existing = this.state.groves.get(groveId) || {} as Grove;
        const updated = { ...existing, ...(data as Partial<Grove>) };
        this.state.groves.set(groveId, updated);
        this.notify('groves-updated');
      }
    }
  }

  private handleAgentEvent(agentId: string, eventType: string, data: unknown): void {
    if (eventType === 'deleted') {
      this.state.agents.delete(agentId);
    } else {
      // Merge delta into existing agent state
      const existing = this.state.agents.get(agentId) || {} as Agent;
      const updated = { ...existing, ...(data as Partial<Agent>) };
      this.state.agents.set(agentId, updated);
    }
    this.notify('agents-updated');
  }

  private notify(event: string): void {
    this.dispatchEvent(new CustomEvent(event, { detail: this.state }));
  }

  disconnect(): void {
    this.sseClient.disconnect();
  }

  // Getters — the full state map is maintained regardless of pagination.
  // Components render the slice they need.
  getAgents(): Agent[] {
    return Array.from(this.state.agents.values());
  }

  getAgent(id: string): Agent | undefined {
    return this.state.agents.get(id);
  }

  getGroves(): Grove[] {
    return Array.from(this.state.groves.values());
  }

  getGrove(id: string): Grove | undefined {
    return this.state.groves.get(id);
  }
}

// Singleton instance
export const stateManager = new StateManager();
```

---

## 5. Terminal Component (xterm.js)

### 5.1 Terminal Wrapper

```typescript
// src/components/terminal/pty-viewer.ts
import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { Terminal } from 'xterm';
import { FitAddon } from 'xterm-addon-fit';
import { WebLinksAddon } from 'xterm-addon-web-links';

@customElement('scion-terminal')
export class ScionTerminal extends LitElement {
  @property({ type: String }) agentId = '';
  @state() private connected = false;
  @state() private error: string | null = null;

  private terminal: Terminal | null = null;
  private fitAddon: FitAddon | null = null;
  private socket: WebSocket | null = null;
  private resizeObserver: ResizeObserver | null = null;

  static styles = css`
    :host {
      display: flex;
      flex-direction: column;
      height: 100%;
      background: var(--wa-color-neutral-900);
      border-radius: var(--wa-border-radius-medium);
      overflow: hidden;
    }

    .toolbar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0.5rem 1rem;
      background: var(--wa-color-neutral-800);
      border-bottom: 1px solid var(--wa-color-neutral-700);
    }

    .status {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.875rem;
      color: var(--wa-color-neutral-400);
    }

    .status-dot {
      width: 8px;
      height: 8px;
      border-radius: 50%;
      background: var(--wa-color-danger-500);
    }

    .status-dot.connected {
      background: var(--wa-color-success-500);
    }

    .terminal-container {
      flex: 1;
      padding: 0.5rem;
    }

    .error {
      color: var(--wa-color-danger-500);
      padding: 1rem;
      text-align: center;
    }
  `;

  async connectedCallback() {
    super.connectedCallback();
    await this.initTerminal();
    this.connectWebSocket();
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this.cleanup();
  }

  private async initTerminal(): Promise<void> {
    // Wait for first render
    await this.updateComplete;

    const container = this.shadowRoot?.querySelector('.terminal-container');
    if (!container) return;

    this.terminal = new Terminal({
      theme: {
        background: '#1a1a2e',
        foreground: '#eaeaea',
        cursor: '#f39c12',
        cursorAccent: '#1a1a2e',
        selection: 'rgba(255, 255, 255, 0.3)',
        black: '#1a1a2e',
        brightBlack: '#6c7086',
        red: '#f38ba8',
        brightRed: '#f38ba8',
        green: '#a6e3a1',
        brightGreen: '#a6e3a1',
        yellow: '#f9e2af',
        brightYellow: '#f9e2af',
        blue: '#89b4fa',
        brightBlue: '#89b4fa',
        magenta: '#cba6f7',
        brightMagenta: '#cba6f7',
        cyan: '#94e2d5',
        brightCyan: '#94e2d5',
        white: '#bac2de',
        brightWhite: '#ffffff'
      },
      fontFamily: 'JetBrains Mono, Menlo, Monaco, monospace',
      fontSize: 14,
      cursorBlink: true,
      cursorStyle: 'block',
      allowProposedApi: true
    });

    this.fitAddon = new FitAddon();
    this.terminal.loadAddon(this.fitAddon);
    this.terminal.loadAddon(new WebLinksAddon());

    this.terminal.open(container as HTMLElement);
    this.fitAddon.fit();

    // Handle terminal input
    this.terminal.onData((data) => {
      this.sendData(data);
    });

    // Handle resize
    this.resizeObserver = new ResizeObserver(() => {
      this.fitAddon?.fit();
      this.sendResize();
    });
    this.resizeObserver.observe(container);
  }

  private async connectWebSocket(): Promise<void> {
    try {
      // Get WebSocket ticket from API
      const ticketResponse = await fetch('/api/auth/ws-ticket', {
        method: 'POST',
        credentials: 'include'
      });
      const { ticket } = await ticketResponse.json();

      // Connect to PTY WebSocket
      const wsUrl = `${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.host}/api/agents/${this.agentId}/pty?ticket=${ticket}`;
      this.socket = new WebSocket(wsUrl);

      this.socket.onopen = () => {
        this.connected = true;
        this.error = null;
        this.sendResize();
      };

      this.socket.onmessage = (event) => {
        const msg = JSON.parse(event.data);
        if (msg.type === 'data') {
          // Decode base64 data
          const bytes = Uint8Array.from(atob(msg.data), c => c.charCodeAt(0));
          this.terminal?.write(bytes);
        }
      };

      this.socket.onerror = () => {
        this.error = 'Connection error';
        this.connected = false;
      };

      this.socket.onclose = (event) => {
        this.connected = false;
        if (event.code !== 1000) {
          this.error = `Connection closed: ${event.reason || 'Unknown error'}`;
        }
      };

    } catch (err) {
      this.error = `Failed to connect: ${err}`;
    }
  }

  private sendData(data: string): void {
    if (this.socket?.readyState === WebSocket.OPEN) {
      const encoded = btoa(data);
      this.socket.send(JSON.stringify({ type: 'data', data: encoded }));
    }
  }

  private sendResize(): void {
    if (this.socket?.readyState === WebSocket.OPEN && this.terminal) {
      this.socket.send(JSON.stringify({
        type: 'resize',
        cols: this.terminal.cols,
        rows: this.terminal.rows
      }));
    }
  }

  private cleanup(): void {
    this.socket?.close();
    this.terminal?.dispose();
    this.resizeObserver?.disconnect();
  }

  private handleReconnect(): void {
    this.cleanup();
    this.initTerminal();
    this.connectWebSocket();
  }

  render() {
    return html`
      <div class="toolbar">
        <div class="status">
          <div class="status-dot ${this.connected ? 'connected' : ''}"></div>
          ${this.connected ? 'Connected' : 'Disconnected'}
        </div>
        <div class="actions">
          <wa-button size="small" variant="text" @click=${this.handleReconnect}>
            <wa-icon name="refresh-cw"></wa-icon>
          </wa-button>
        </div>
      </div>
      ${this.error ? html`
        <div class="error">
          <wa-icon name="alert-circle"></wa-icon>
          ${this.error}
        </div>
      ` : ''}
      <div class="terminal-container"></div>
    `;
  }
}
```

---

## 6. Template Management UI

### 6.1 Template Browser

The template browser allows users to discover, view, and manage templates across scopes.

```typescript
// src/components/pages/template-list.ts
import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import type { Template } from '../types';

@customElement('scion-template-list')
export class TemplateList extends LitElement {
  @property({ type: String }) scope: 'global' | 'grove' | 'user' | 'all' = 'all';
  @property({ type: String }) groveId?: string;
  @state() private templates: Template[] = [];
  @state() private loading = true;

  static styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      justify-content: space-between;
      align-items: center;
      margin-bottom: 1.5rem;
    }

    .filters {
      display: flex;
      gap: 1rem;
      margin-bottom: 1rem;
    }

    .template-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
      gap: 1rem;
    }

    .scope-badge {
      font-size: 0.75rem;
      padding: 0.25rem 0.5rem;
      border-radius: var(--wa-border-radius-small);
    }

    .scope-global { background: var(--wa-color-primary-100); }
    .scope-grove { background: var(--wa-color-success-100); }
    .scope-user { background: var(--wa-color-warning-100); }
  `;

  render() {
    return html`
      <div class="header">
        <h1>Templates</h1>
        <wa-button variant="primary" @click=${this.handleCreate}>
          <wa-icon slot="prefix" name="plus"></wa-icon>
          New Template
        </wa-button>
      </div>

      <div class="filters">
        <wa-select label="Scope" @wa-change=${this.handleScopeChange}>
          <wa-option value="all">All Templates</wa-option>
          <wa-option value="global">Global</wa-option>
          <wa-option value="grove">Grove</wa-option>
          <wa-option value="user">Personal</wa-option>
        </wa-select>

        <wa-select label="Harness">
          <wa-option value="">All Harnesses</wa-option>
          <wa-option value="claude">Claude</wa-option>
          <wa-option value="gemini">Gemini</wa-option>
          <wa-option value="codex">Codex</wa-option>
          <wa-option value="opencode">OpenCode</wa-option>
        </wa-select>

        <wa-input
          placeholder="Search templates..."
          @wa-input=${this.handleSearch}
        >
          <wa-icon slot="prefix" name="search"></wa-icon>
        </wa-input>
      </div>

      ${this.loading ? html`
        <wa-spinner></wa-spinner>
      ` : html`
        <div class="template-grid">
          ${this.templates.map(template => html`
            <scion-template-card
              .template=${template}
              @template-action=${this.handleTemplateAction}
            ></scion-template-card>
          `)}
        </div>
      `}
    `;
  }
}
```

### 6.2 Template Card Component

```typescript
// src/components/shared/template-card.ts
@customElement('scion-template-card')
export class TemplateCard extends LitElement {
  @property({ type: Object }) template!: Template;

  private getScopeLabel(scope: string): string {
    return { global: 'Global', grove: 'Grove', user: 'Personal' }[scope] || scope;
  }

  private handleAction(action: string) {
    this.dispatchEvent(new CustomEvent('template-action', {
      detail: { templateId: this.template.id, action },
      bubbles: true,
      composed: true
    }));
  }

  render() {
    const { template } = this;

    return html`
      <wa-card>
        <div slot="header">
          <div class="header-content">
            <wa-icon name="file-code"></wa-icon>
            <div>
              <div class="title">${template.displayName || template.name}</div>
              <div class="harness">${template.harness}</div>
            </div>
            <span class="scope-badge scope-${template.scope}">
              ${this.getScopeLabel(template.scope)}
            </span>
          </div>
        </div>

        <p class="description">${template.description || 'No description'}</p>

        <div class="meta">
          <span>Files: ${template.files?.length || 0}</span>
          <span>Updated: ${new Date(template.updated).toLocaleDateString()}</span>
        </div>

        <div slot="footer" class="actions">
          <wa-button size="small" @click=${() => this.handleAction('view')}>
            View
          </wa-button>
          <wa-button size="small" @click=${() => this.handleAction('clone')}>
            Clone
          </wa-button>
          ${template.scope !== 'global' ? html`
            <wa-button size="small" variant="danger" @click=${() => this.handleAction('delete')}>
              Delete
            </wa-button>
          ` : ''}
        </div>
      </wa-card>
    `;
  }
}
```

### 6.3 Template Upload Flow

Template upload uses a multi-step wizard with signed URL uploads.

```typescript
// src/components/pages/template-upload.ts
@customElement('scion-template-upload')
export class TemplateUpload extends LitElement {
  @state() private step: 'metadata' | 'files' | 'confirm' = 'metadata';
  @state() private metadata: Partial<Template> = {};
  @state() private files: File[] = [];
  @state() private uploadUrls: { path: string; url: string }[] = [];
  @state() private uploading = false;
  @state() private uploadProgress: Map<string, number> = new Map();

  render() {
    return html`
      <wa-card>
        <h2 slot="header">Create Template</h2>

        <wa-stepper .active=${this.step}>
          <wa-step name="metadata" label="Details"></wa-step>
          <wa-step name="files" label="Files"></wa-step>
          <wa-step name="confirm" label="Confirm"></wa-step>
        </wa-stepper>

        ${this.step === 'metadata' ? this.renderMetadataStep() : ''}
        ${this.step === 'files' ? this.renderFilesStep() : ''}
        ${this.step === 'confirm' ? this.renderConfirmStep() : ''}
      </wa-card>
    `;
  }

  private renderMetadataStep() {
    return html`
      <form @submit=${this.handleMetadataSubmit}>
        <wa-input
          label="Template Name"
          required
          @wa-input=${(e: Event) => this.metadata.name = (e.target as HTMLInputElement).value}
        ></wa-input>

        <wa-textarea
          label="Description"
          @wa-input=${(e: Event) => this.metadata.description = (e.target as HTMLTextAreaElement).value}
        ></wa-textarea>

        <wa-select
          label="Harness"
          required
          @wa-change=${(e: Event) => this.metadata.harness = (e.target as HTMLSelectElement).value as any}
        >
          <wa-option value="claude">Claude</wa-option>
          <wa-option value="gemini">Gemini</wa-option>
          <wa-option value="codex">Codex</wa-option>
          <wa-option value="opencode">OpenCode</wa-option>
          <wa-option value="generic">Generic</wa-option>
        </wa-select>

        <wa-select
          label="Scope"
          @wa-change=${(e: Event) => this.metadata.scope = (e.target as HTMLSelectElement).value as any}
        >
          <wa-option value="user">Personal</wa-option>
          <wa-option value="grove">Grove</wa-option>
        </wa-select>

        <wa-button type="submit" variant="primary">Continue</wa-button>
      </form>
    `;
  }

  private renderFilesStep() {
    return html`
      <scion-file-upload
        multiple
        accept=".yaml,.yml,.md,.txt,.json,.sh"
        @files-selected=${this.handleFilesSelected}
      ></scion-file-upload>

      ${this.files.length > 0 ? html`
        <ul class="file-list">
          ${this.files.map(file => html`
            <li>
              ${file.name} (${this.formatSize(file.size)})
              ${this.uploadProgress.has(file.name) ? html`
                <wa-progress-bar
                  value=${this.uploadProgress.get(file.name)}
                ></wa-progress-bar>
              ` : ''}
            </li>
          `)}
        </ul>
      ` : ''}

      <div class="actions">
        <wa-button @click=${() => this.step = 'metadata'}>Back</wa-button>
        <wa-button
          variant="primary"
          ?disabled=${this.files.length === 0}
          @click=${() => this.step = 'confirm'}
        >Continue</wa-button>
      </div>
    `;
  }

  private async handleUpload() {
    this.uploading = true;

    // 1. Create template and get upload URLs
    const response = await fetch('/api/templates', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        ...this.metadata,
        files: this.files.map(f => ({ path: f.name, size: f.size }))
      }),
      credentials: 'include'
    });

    const { template, uploadUrls } = await response.json();

    // 2. Upload files to signed URLs
    for (const { path, url } of uploadUrls) {
      const file = this.files.find(f => f.name === path);
      if (file) {
        await this.uploadFile(file, url);
      }
    }

    // 3. Finalize template
    await fetch(`/api/templates/${template.id}/finalize`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ manifest: await this.buildManifest() }),
      credentials: 'include'
    });

    this.uploading = false;
    // Navigate to template detail
  }
}
```

---

## 7. User & Group Management UI

### 7.1 User List Page

```typescript
// src/components/pages/user-list.ts
@customElement('scion-user-list')
export class UserList extends LitElement {
  @state() private users: User[] = [];
  @state() private loading = true;

  render() {
    return html`
      <div class="header">
        <h1>Users</h1>
        <wa-input placeholder="Search users...">
          <wa-icon slot="prefix" name="search"></wa-icon>
        </wa-input>
      </div>

      <wa-table>
        <wa-table-header>
          <wa-table-row>
            <wa-table-cell header>User</wa-table-cell>
            <wa-table-cell header>Email</wa-table-cell>
            <wa-table-cell header>Role</wa-table-cell>
            <wa-table-cell header>Status</wa-table-cell>
            <wa-table-cell header>Last Login</wa-table-cell>
            <wa-table-cell header>Actions</wa-table-cell>
          </wa-table-row>
        </wa-table-header>
        <wa-table-body>
          ${this.users.map(user => html`
            <wa-table-row>
              <wa-table-cell>
                <div class="user-cell">
                  <scion-user-avatar .user=${user}></scion-user-avatar>
                  <span>${user.displayName}</span>
                </div>
              </wa-table-cell>
              <wa-table-cell>${user.email}</wa-table-cell>
              <wa-table-cell>
                <wa-badge variant=${this.getRoleVariant(user.role)}>
                  ${user.role}
                </wa-badge>
              </wa-table-cell>
              <wa-table-cell>
                <wa-badge variant=${user.status === 'active' ? 'success' : 'warning'}>
                  ${user.status}
                </wa-badge>
              </wa-table-cell>
              <wa-table-cell>${this.formatDate(user.lastLogin)}</wa-table-cell>
              <wa-table-cell>
                <wa-dropdown>
                  <wa-button slot="trigger" size="small">
                    <wa-icon name="more-vertical"></wa-icon>
                  </wa-button>
                  <wa-menu>
                    <wa-menu-item @click=${() => this.viewUser(user)}>View</wa-menu-item>
                    <wa-menu-item @click=${() => this.editUser(user)}>Edit Role</wa-menu-item>
                    <wa-divider></wa-divider>
                    <wa-menu-item variant="danger" @click=${() => this.suspendUser(user)}>
                      ${user.status === 'active' ? 'Suspend' : 'Activate'}
                    </wa-menu-item>
                  </wa-menu>
                </wa-dropdown>
              </wa-table-cell>
            </wa-table-row>
          `)}
        </wa-table-body>
      </wa-table>
    `;
  }
}
```

### 7.2 Group Management Page

```typescript
// src/components/pages/group-list.ts
@customElement('scion-group-list')
export class GroupList extends LitElement {
  @state() private groups: Group[] = [];

  render() {
    return html`
      <div class="header">
        <h1>Groups</h1>
        <wa-button variant="primary" @click=${this.handleCreate}>
          <wa-icon slot="prefix" name="plus"></wa-icon>
          New Group
        </wa-button>
      </div>

      <div class="group-grid">
        ${this.groups.map(group => html`
          <wa-card>
            <div slot="header">
              <wa-icon name="users"></wa-icon>
              <span>${group.name}</span>
            </div>

            <p>${group.description || 'No description'}</p>

            <div class="meta">
              <span>${group.memberCount} members</span>
              ${group.parentId ? html`
                <span>Parent: ${this.getGroupName(group.parentId)}</span>
              ` : ''}
            </div>

            <div slot="footer">
              <wa-button size="small" @click=${() => this.viewGroup(group)}>
                Manage Members
              </wa-button>
              <wa-button size="small" @click=${() => this.editGroup(group)}>
                Edit
              </wa-button>
            </div>
          </wa-card>
        `)}
      </div>
    `;
  }
}
```

### 7.3 Group Detail with Member Management

```typescript
// src/components/pages/group-detail.ts
@customElement('scion-group-detail')
export class GroupDetail extends LitElement {
  @property({ type: String }) groupId = '';
  @state() private group: Group | null = null;
  @state() private members: (User | Group)[] = [];

  render() {
    if (!this.group) return html`<wa-spinner></wa-spinner>`;

    return html`
      <div class="header">
        <div class="title-row">
          <wa-icon name="users"></wa-icon>
          <h1>${this.group.name}</h1>
          <scion-group-badge .group=${this.group}></scion-group-badge>
        </div>
        <p>${this.group.description}</p>
      </div>

      <wa-tabs>
        <wa-tab slot="nav" panel="members">Members</wa-tab>
        <wa-tab slot="nav" panel="policies">Policies</wa-tab>
        <wa-tab slot="nav" panel="settings">Settings</wa-tab>

        <wa-tab-panel name="members">
          <div class="members-header">
            <h3>Members (${this.members.length})</h3>
            <wa-button @click=${this.handleAddMember}>
              <wa-icon slot="prefix" name="user-plus"></wa-icon>
              Add Member
            </wa-button>
          </div>

          <scion-member-list
            .members=${this.members}
            @member-remove=${this.handleRemoveMember}
            @member-role-change=${this.handleRoleChange}
          ></scion-member-list>
        </wa-tab-panel>

        <wa-tab-panel name="policies">
          <scion-policy-list
            scopeType="group"
            scopeId=${this.groupId}
          ></scion-policy-list>
        </wa-tab-panel>

        <wa-tab-panel name="settings">
          <!-- Group settings form -->
        </wa-tab-panel>
      </wa-tabs>
    `;
  }
}
```

---

## 8. Permissions & Policy Management UI

### 8.1 Policy List Page

```typescript
// src/components/pages/policy-list.ts
@customElement('scion-policy-list')
export class PolicyList extends LitElement {
  @property({ type: String }) scopeType?: string;
  @property({ type: String }) scopeId?: string;
  @state() private policies: Policy[] = [];

  render() {
    return html`
      <div class="header">
        <h1>Policies</h1>
        <wa-button variant="primary" @click=${this.handleCreate}>
          <wa-icon slot="prefix" name="shield-plus"></wa-icon>
          New Policy
        </wa-button>
      </div>

      <div class="filters">
        <wa-select label="Scope" @wa-change=${this.handleScopeFilter}>
          <wa-option value="">All Scopes</wa-option>
          <wa-option value="hub">Hub</wa-option>
          <wa-option value="grove">Grove</wa-option>
          <wa-option value="resource">Resource</wa-option>
        </wa-select>

        <wa-select label="Effect">
          <wa-option value="">All</wa-option>
          <wa-option value="allow">Allow</wa-option>
          <wa-option value="deny">Deny</wa-option>
        </wa-select>
      </div>

      <wa-table>
        <wa-table-header>
          <wa-table-row>
            <wa-table-cell header>Policy</wa-table-cell>
            <wa-table-cell header>Scope</wa-table-cell>
            <wa-table-cell header>Resource</wa-table-cell>
            <wa-table-cell header>Actions</wa-table-cell>
            <wa-table-cell header>Effect</wa-table-cell>
            <wa-table-cell header>Principals</wa-table-cell>
            <wa-table-cell header></wa-table-cell>
          </wa-table-row>
        </wa-table-header>
        <wa-table-body>
          ${this.policies.map(policy => html`
            <wa-table-row>
              <wa-table-cell>
                <div>
                  <div class="policy-name">${policy.name}</div>
                  <div class="policy-desc">${policy.description}</div>
                </div>
              </wa-table-cell>
              <wa-table-cell>
                <wa-badge>${policy.scopeType}</wa-badge>
              </wa-table-cell>
              <wa-table-cell>${policy.resourceType}</wa-table-cell>
              <wa-table-cell>
                ${policy.actions.map(a => html`
                  <wa-badge size="small">${a}</wa-badge>
                `)}
              </wa-table-cell>
              <wa-table-cell>
                <wa-badge variant=${policy.effect === 'allow' ? 'success' : 'danger'}>
                  ${policy.effect}
                </wa-badge>
              </wa-table-cell>
              <wa-table-cell>
                <span>${this.getPrincipalCount(policy)} principals</span>
              </wa-table-cell>
              <wa-table-cell>
                <wa-button size="small" @click=${() => this.editPolicy(policy)}>
                  Edit
                </wa-button>
              </wa-table-cell>
            </wa-table-row>
          `)}
        </wa-table-body>
      </wa-table>
    `;
  }
}
```

### 8.2 Policy Editor Component

```typescript
// src/components/shared/policy-editor.ts
@customElement('scion-policy-editor')
export class PolicyEditor extends LitElement {
  @property({ type: Object }) policy: Partial<Policy> = {};
  @state() private selectedPrincipals: { type: string; id: string }[] = [];

  render() {
    return html`
      <form @submit=${this.handleSubmit}>
        <wa-input
          label="Policy Name"
          required
          .value=${this.policy.name || ''}
          @wa-input=${this.updateField('name')}
        ></wa-input>

        <wa-textarea
          label="Description"
          .value=${this.policy.description || ''}
          @wa-input=${this.updateField('description')}
        ></wa-textarea>

        <wa-select
          label="Scope Type"
          required
          .value=${this.policy.scopeType || 'hub'}
          @wa-change=${this.updateField('scopeType')}
        >
          <wa-option value="hub">Hub (Global)</wa-option>
          <wa-option value="grove">Grove</wa-option>
          <wa-option value="resource">Specific Resource</wa-option>
        </wa-select>

        ${this.policy.scopeType !== 'hub' ? html`
          <scion-scope-selector
            .scopeType=${this.policy.scopeType}
            .scopeId=${this.policy.scopeId}
            @scope-change=${this.handleScopeChange}
          ></scion-scope-selector>
        ` : ''}

        <wa-select
          label="Resource Type"
          .value=${this.policy.resourceType || '*'}
          @wa-change=${this.updateField('resourceType')}
        >
          <wa-option value="*">All Resources</wa-option>
          <wa-option value="agent">Agents</wa-option>
          <wa-option value="grove">Groves</wa-option>
          <wa-option value="template">Templates</wa-option>
          <wa-option value="user">Users</wa-option>
          <wa-option value="group">Groups</wa-option>
        </wa-select>

        <fieldset>
          <legend>Actions</legend>
          <div class="action-checkboxes">
            ${['create', 'read', 'update', 'delete', 'list', 'manage'].map(action => html`
              <wa-checkbox
                ?checked=${this.policy.actions?.includes(action)}
                @wa-change=${(e: Event) => this.toggleAction(action, (e.target as HTMLInputElement).checked)}
              >${action}</wa-checkbox>
            `)}
          </div>
        </fieldset>

        <wa-radio-group
          label="Effect"
          .value=${this.policy.effect || 'allow'}
          @wa-change=${this.updateField('effect')}
        >
          <wa-radio value="allow">Allow</wa-radio>
          <wa-radio value="deny">Deny</wa-radio>
        </wa-radio-group>

        <fieldset>
          <legend>Principals</legend>
          <scion-principal-selector
            .selected=${this.selectedPrincipals}
            @principals-change=${this.handlePrincipalsChange}
          ></scion-principal-selector>
        </fieldset>

        <div class="actions">
          <wa-button type="button" @click=${this.handleCancel}>Cancel</wa-button>
          <wa-button type="submit" variant="primary">Save Policy</wa-button>
        </div>
      </form>
    `;
  }
}
```

### 8.3 Access Evaluation Debug Tool

```typescript
// src/components/shared/access-evaluator.ts
@customElement('scion-access-evaluator')
export class AccessEvaluator extends LitElement {
  @state() private principal: { type: string; id: string } | null = null;
  @state() private resource: { type: string; id: string } | null = null;
  @state() private action = '';
  @state() private result: AccessEvalResult | null = null;
  @state() private evaluating = false;

  render() {
    return html`
      <wa-card>
        <h3 slot="header">Access Evaluation (Debug)</h3>

        <div class="eval-form">
          <scion-principal-selector
            single
            label="Principal"
            @principal-select=${this.handlePrincipalSelect}
          ></scion-principal-selector>

          <wa-select
            label="Resource Type"
            @wa-change=${(e: Event) => this.resource = { ...this.resource!, type: (e.target as HTMLSelectElement).value }}
          >
            <wa-option value="agent">Agent</wa-option>
            <wa-option value="grove">Grove</wa-option>
            <wa-option value="template">Template</wa-option>
          </wa-select>

          <wa-input
            label="Resource ID"
            @wa-input=${(e: Event) => this.resource = { ...this.resource!, id: (e.target as HTMLInputElement).value }}
          ></wa-input>

          <wa-select
            label="Action"
            @wa-change=${(e: Event) => this.action = (e.target as HTMLSelectElement).value}
          >
            <wa-option value="create">create</wa-option>
            <wa-option value="read">read</wa-option>
            <wa-option value="update">update</wa-option>
            <wa-option value="delete">delete</wa-option>
          </wa-select>

          <wa-button
            variant="primary"
            ?loading=${this.evaluating}
            @click=${this.evaluate}
          >Evaluate</wa-button>
        </div>

        ${this.result ? html`
          <div class="result ${this.result.allowed ? 'allowed' : 'denied'}">
            <wa-icon name=${this.result.allowed ? 'check-circle' : 'x-circle'}></wa-icon>
            <span>${this.result.allowed ? 'Access Allowed' : 'Access Denied'}</span>
            <span class="reason">${this.result.reason}</span>
          </div>

          ${this.result.matchedPolicy ? html`
            <div class="matched-policy">
              <h4>Matched Policy</h4>
              <dl>
                <dt>Name</dt><dd>${this.result.matchedPolicy.name}</dd>
                <dt>Scope</dt><dd>${this.result.matchedPolicy.scopeType}</dd>
                <dt>Effect</dt><dd>${this.result.matchedPolicy.effect}</dd>
              </dl>
            </div>
          ` : ''}

          ${this.result.effectiveGroups?.length ? html`
            <div class="effective-groups">
              <h4>Effective Groups</h4>
              <ul>
                ${this.result.effectiveGroups.map(g => html`<li>${g}</li>`)}
              </ul>
            </div>
          ` : ''}
        ` : ''}
      </wa-card>
    `;
  }

  private async evaluate() {
    this.evaluating = true;
    const response = await fetch('/api/policies/evaluate', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        principalType: this.principal?.type,
        principalId: this.principal?.id,
        resourceType: this.resource?.type,
        resourceId: this.resource?.id,
        action: this.action
      }),
      credentials: 'include'
    });
    this.result = await response.json();
    this.evaluating = false;
  }
}
```

---

## 9. Environment Variables & Secrets UI

### 9.1 Environment Settings Page

```typescript
// src/components/pages/settings-env.ts
@customElement('scion-settings-env')
export class SettingsEnv extends LitElement {
  @property({ type: String }) scope: 'user' | 'grove' | 'runtime_broker' = 'user';
  @property({ type: String }) scopeId?: string;
  @state() private envVars: EnvVar[] = [];
  @state() private secrets: Secret[] = [];

  render() {
    return html`
      <div class="header">
        <h1>Environment & Secrets</h1>
        <scion-scope-selector
          .scope=${this.scope}
          .scopeId=${this.scopeId}
          @scope-change=${this.handleScopeChange}
        ></scion-scope-selector>
      </div>

      <wa-tabs>
        <wa-tab slot="nav" panel="env">Environment Variables</wa-tab>
        <wa-tab slot="nav" panel="secrets">Secrets</wa-tab>

        <wa-tab-panel name="env">
          <div class="panel-header">
            <p>Environment variables are visible to agents at runtime.</p>
            <wa-button @click=${this.addEnvVar}>
              <wa-icon slot="prefix" name="plus"></wa-icon>
              Add Variable
            </wa-button>
          </div>

          <wa-table>
            <wa-table-header>
              <wa-table-row>
                <wa-table-cell header>Key</wa-table-cell>
                <wa-table-cell header>Value</wa-table-cell>
                <wa-table-cell header>Sensitive</wa-table-cell>
                <wa-table-cell header>Updated</wa-table-cell>
                <wa-table-cell header></wa-table-cell>
              </wa-table-row>
            </wa-table-header>
            <wa-table-body>
              ${this.envVars.map(env => html`
                <wa-table-row>
                  <wa-table-cell><code>${env.key}</code></wa-table-cell>
                  <wa-table-cell>
                    ${env.sensitive ? '••••••••' : env.value}
                  </wa-table-cell>
                  <wa-table-cell>
                    ${env.sensitive ? html`<wa-icon name="eye-off"></wa-icon>` : ''}
                  </wa-table-cell>
                  <wa-table-cell>${this.formatDate(env.updated)}</wa-table-cell>
                  <wa-table-cell>
                    <wa-button size="small" @click=${() => this.editEnvVar(env)}>Edit</wa-button>
                    <wa-button size="small" variant="danger" @click=${() => this.deleteEnvVar(env)}>Delete</wa-button>
                  </wa-table-cell>
                </wa-table-row>
              `)}
            </wa-table-body>
          </wa-table>
        </wa-tab-panel>

        <wa-tab-panel name="secrets">
          <div class="panel-header">
            <p>Secrets are write-only and cannot be retrieved after creation.</p>
            <wa-button @click=${this.addSecret}>
              <wa-icon slot="prefix" name="key"></wa-icon>
              Add Secret
            </wa-button>
          </div>

          <wa-table>
            <wa-table-header>
              <wa-table-row>
                <wa-table-cell header>Key</wa-table-cell>
                <wa-table-cell header>Description</wa-table-cell>
                <wa-table-cell header>Version</wa-table-cell>
                <wa-table-cell header>Updated</wa-table-cell>
                <wa-table-cell header></wa-table-cell>
              </wa-table-row>
            </wa-table-header>
            <wa-table-body>
              ${this.secrets.map(secret => html`
                <wa-table-row>
                  <wa-table-cell><code>${secret.key}</code></wa-table-cell>
                  <wa-table-cell>${secret.description || '-'}</wa-table-cell>
                  <wa-table-cell>v${secret.version}</wa-table-cell>
                  <wa-table-cell>${this.formatDate(secret.updated)}</wa-table-cell>
                  <wa-table-cell>
                    <wa-button size="small" @click=${() => this.updateSecret(secret)}>Update</wa-button>
                    <wa-button size="small" variant="danger" @click=${() => this.deleteSecret(secret)}>Delete</wa-button>
                  </wa-table-cell>
                </wa-table-row>
              `)}
            </wa-table-body>
          </wa-table>
        </wa-tab-panel>
      </wa-tabs>
    `;
  }
}
```

### 9.2 Env/Secret Editor Dialog

```typescript
// src/components/shared/env-var-editor.ts
@customElement('scion-env-var-editor')
export class EnvVarEditor extends LitElement {
  @property({ type: Object }) envVar?: EnvVar;
  @property({ type: Boolean }) isSecret = false;
  @property({ type: String }) scope: string = 'user';
  @property({ type: String }) scopeId?: string;

  render() {
    const isNew = !this.envVar?.id;

    return html`
      <wa-dialog label="${isNew ? 'Add' : 'Edit'} ${this.isSecret ? 'Secret' : 'Variable'}">
        <form @submit=${this.handleSubmit}>
          <wa-input
            label="Key"
            required
            pattern="[A-Z][A-Z0-9_]*"
            .value=${this.envVar?.key || ''}
            ?disabled=${!isNew}
            @wa-input=${this.updateKey}
          >
            <span slot="help-text">Use UPPER_SNAKE_CASE (e.g., API_KEY)</span>
          </wa-input>

          <wa-input
            label="Value"
            required
            type=${this.isSecret ? 'password' : 'text'}
            .value=${isNew || !this.isSecret ? (this.envVar as EnvVar)?.value || '' : ''}
            @wa-input=${this.updateValue}
          >
            ${this.isSecret && !isNew ? html`
              <span slot="help-text">Leave empty to keep current value</span>
            ` : ''}
          </wa-input>

          <wa-textarea
            label="Description"
            .value=${this.envVar?.description || ''}
            @wa-input=${this.updateDescription}
          ></wa-textarea>

          ${!this.isSecret ? html`
            <wa-checkbox
              ?checked=${(this.envVar as EnvVar)?.sensitive}
              @wa-change=${this.updateSensitive}
            >Mask value in UI</wa-checkbox>
          ` : ''}

          <div class="actions" slot="footer">
            <wa-button @click=${this.close}>Cancel</wa-button>
            <wa-button type="submit" variant="primary">Save</wa-button>
          </div>
        </form>
      </wa-dialog>
    `;
  }
}
```

---

## 10. API Key Management UI

### 10.1 API Keys Page

```typescript
// src/components/pages/api-keys.ts
@customElement('scion-api-keys')
export class ApiKeys extends LitElement {
  @state() private keys: ApiKey[] = [];
  @state() private newKey: { key: string; name: string } | null = null;

  render() {
    return html`
      <div class="header">
        <h1>API Keys</h1>
        <wa-button variant="primary" @click=${this.createKey}>
          <wa-icon slot="prefix" name="key"></wa-icon>
          Create API Key
        </wa-button>
      </div>

      <wa-alert variant="info" open>
        API keys provide programmatic access to the Scion Hub API.
        Keep them secure and never share them publicly.
      </wa-alert>

      ${this.newKey ? html`
        <wa-alert variant="success" open closable @wa-close=${() => this.newKey = null}>
          <strong>New API Key Created</strong>
          <p>Copy this key now. It won't be shown again.</p>
          <div class="key-display">
            <code>${this.newKey.key}</code>
            <wa-button size="small" @click=${() => this.copyKey(this.newKey!.key)}>
              <wa-icon name="copy"></wa-icon>
            </wa-button>
          </div>
        </wa-alert>
      ` : ''}

      <wa-table>
        <wa-table-header>
          <wa-table-row>
            <wa-table-cell header>Name</wa-table-cell>
            <wa-table-cell header>Key Prefix</wa-table-cell>
            <wa-table-cell header>Created</wa-table-cell>
            <wa-table-cell header>Last Used</wa-table-cell>
            <wa-table-cell header>Expires</wa-table-cell>
            <wa-table-cell header></wa-table-cell>
          </wa-table-row>
        </wa-table-header>
        <wa-table-body>
          ${this.keys.map(key => html`
            <wa-table-row>
              <wa-table-cell>${key.name}</wa-table-cell>
              <wa-table-cell><code>${key.prefix}...</code></wa-table-cell>
              <wa-table-cell>${this.formatDate(key.createdAt)}</wa-table-cell>
              <wa-table-cell>${key.lastUsed ? this.formatDate(key.lastUsed) : 'Never'}</wa-table-cell>
              <wa-table-cell>
                ${key.expiresAt ? this.formatDate(key.expiresAt) : 'Never'}
              </wa-table-cell>
              <wa-table-cell>
                <wa-button
                  size="small"
                  variant="danger"
                  @click=${() => this.revokeKey(key)}
                >Revoke</wa-button>
              </wa-table-cell>
            </wa-table-row>
          `)}
        </wa-table-body>
      </wa-table>
    `;
  }

  private async createKey() {
    const dialog = document.createElement('wa-dialog');
    dialog.label = 'Create API Key';
    dialog.innerHTML = `
      <form>
        <wa-input name="name" label="Key Name" required placeholder="e.g., CI/CD Pipeline"></wa-input>
        <wa-input name="expiresIn" label="Expires In (days)" type="number" placeholder="Leave empty for no expiry"></wa-input>
        <div slot="footer">
          <wa-button>Cancel</wa-button>
          <wa-button type="submit" variant="primary">Create</wa-button>
        </div>
      </form>
    `;
    // Handle form submission...
  }
}
```

---

## 12. Real-Time Event Integration

> **2026-02-19 — NATS approach abandoned.** The NATS-based event transport described in this section has been superseded by an in-process Go channel design. The Koa BFF is being consolidated into the Go binary, eliminating the need for NATS as a cross-process bridge. See `web-realtime.md` for the current design. The SSE transport model, subject hierarchy, and subscription semantics described below remain valid — only the backend delivery mechanism (NATS) is replaced by `ChannelEventPublisher`.

### 12.1 Transport Design: SSE for State, WebSocket for PTY

State updates use **Server-Sent Events (SSE)** rather than WebSocket. WebSocket is reserved for the terminal/PTY binary data stream only.

**Rationale:**

- State updates are inherently **server-to-client unidirectional**. The client sends commands via REST (`POST /api/agents/:id/stop`), not through the event stream. SSE matches this topology directly.
- The `EventSource` API provides **automatic reconnection** with `lastEventId` resume, which the client gets for free.
- SSE streams are regular HTTP responses that pass through CDNs, load balancers, and Cloud Run without special WebSocket upgrade configuration.
- SSE maps directly to a **gRPC server-streaming RPC**, which is the intended future transport. A WebSocket approach would map to a bidirectional stream — more complex operationally without benefit, since the client-to-server direction would be unused for state.

**Future gRPC mapping:**

```protobuf
// The SSE /events endpoint maps directly to this server-streaming RPC
rpc WatchResources(WatchRequest) returns (stream WatchEvent);

message WatchRequest {
  repeated string subjects = 1;    // NATS-style subject patterns
  string resume_token = 2;         // maps to SSE lastEventId
}

message WatchEvent {
  string subject = 1;
  string event_type = 2;           // "status", "created", "deleted", "event"
  bytes payload = 3;
  int64 sequence = 4;              // maps to SSE id
  int64 timestamp = 5;
}
```

### 12.2 View-Scoped Subscription Model

The subscription unit is the **view context**, not the individual entity. A paginated agent list showing page 3 of 20 does not subscribe to each visible agent individually — it subscribes at the grove level and maintains a full in-memory state map. Pagination is a rendering concern, not a data concern.

#### Subscription Tiers

| View | SSE Endpoint | NATS Subjects | Event Weight |
|------|-------------|---------------|--------------|
| Dashboard / Grove list | `GET /events?sub=grove.*.summary` | Aggregate stats per grove | Lightweight |
| Grove detail / Agent list | `GET /events?sub=grove.{groveId}.>` | All agent events within the grove | Lightweight to Medium |
| Agent detail | `GET /events?sub=grove.{groveId}.>&sub=agent.{agentId}.>` | Grove context + full agent event stream | All weights |
| Terminal | `GET /events?sub=agent.{agentId}.>` | Agent events (PTY data uses separate WebSocket) | All weights |

Subscription changes happen on **navigation events**, not on pagination or scroll. This keeps subscription churn at human-interaction frequency (seconds to minutes between transitions).

#### Subscription Lifecycle

```
User lands on /groves
  → open SSE: /events?sub=grove.*.summary

User clicks into /groves/abc
  → close previous SSE connection
  → open SSE: /events?sub=grove.abc.>

User clicks into /agents/xyz (within grove abc)
  → close previous SSE connection
  → open SSE: /events?sub=grove.abc.>&sub=agent.xyz.>
  (grove subscription kept for breadcrumb/sidebar freshness)

User navigates back to /groves/abc
  → close previous SSE connection
  → open SSE: /events?sub=grove.abc.>

User navigates to /groves/def
  → close previous SSE connection
  → open SSE: /events?sub=grove.def.>
```

Closing and reopening the SSE connection on navigation (rather than mutating subscriptions in-band) keeps the server stateless per-connection and maps directly to making a new gRPC `WatchRequest` call.

#### Event Weight Classes

Not all events carry the same payload cost. The NATS subject hierarchy controls which weight class a subscriber receives based on their subscription scope.

| Weight | Subject Pattern | Payload Size | Example |
|--------|----------------|-------------|---------|
| **Lightweight** | `grove.{groveId}.agent.status` | ~100 bytes | `{ agentId, status, sessionStatus }` |
| **Lightweight** | `grove.{groveId}.agent.deleted` | ~50 bytes | `{ agentId }` |
| **Medium** | `grove.{groveId}.agent.created` | ~500 bytes | `{ agentId, name, template, status }` |
| **Medium** | `agent.{agentId}.metrics` | ~200 bytes | `{ cpu, memory, tokens }` |
| **Heavy** | `agent.{agentId}.event` | 1-10 KB | Full `StatusEvent` with harness output |

The Hub publishes to **both** grove-scoped and agent-scoped subjects for status changes (dual-publish). Heavy events like `agent.{agentId}.event` are published **only** to the agent-specific subject. This means subscribing at the grove level via `grove.{groveId}.>` automatically filters out heavy payloads — the grove-level wildcard only receives events published to `grove.{groveId}.*` subjects.

#### Scalability

For a grove with 500 agents, `grove.{groveId}.>` carries all lightweight status changes. At one status change per agent per minute (a high estimate), that is ~8 events/second at ~100 bytes each — under 1 KB/s on the SSE stream. If thousands of agents with high-frequency events become a concern, a server-side aggregation tier can batch grove-level events into periodic consolidated snapshots (e.g., every 2 seconds). This is an optimization to add later, not something to design in now.

#### Design Constraints

- **Do not use viewport-aware subscriptions.** Subscribing to only the visible agents in a paginated list causes subscription churn on every page change and stale data when paging back. The grove-level subscription is cheaper.
- **Do not use a single global subscription.** Subscribing to `>` (everything) pushes events for groves the user has no interest in and requires server-side permission filtering on every message.
- **Do not mutate subscriptions in-band.** Close the SSE connection and open a new one with updated query parameters on navigation. This keeps the server stateless per-connection and maps directly to the future gRPC `WatchRequest` pattern.

### 12.3 NATS Subject Schema

The Hub API publishes events to NATS when database changes occur. The Web Frontend subscribes to relevant subjects based on view context.

#### Grove-Scoped Subjects (Lightweight/Medium events)

| Subject Pattern | Description | Payload | Weight |
|-----------------|-------------|---------|--------|
| `grove.{groveId}.agent.status` | Agent status change within grove | `{ agentId, status, sessionStatus, containerStatus }` | Lightweight |
| `grove.{groveId}.agent.created` | Agent created in grove | `{ agentId, name, template, status }` | Medium |
| `grove.{groveId}.agent.deleted` | Agent deleted from grove | `{ agentId }` | Lightweight |
| `grove.{groveId}.updated` | Grove metadata changed | `{ name?, labels?, ... }` | Lightweight |
| `grove.{groveId}.broker.connected` | Broker joined grove | `{ brokerId, brokerName }` | Lightweight |
| `grove.{groveId}.broker.disconnected` | Broker left grove | `{ brokerId }` | Lightweight |
| `grove.*.summary` | Periodic grove summary (dashboard) | `{ groveId, agentCounts, status }` | Lightweight |

#### Agent-Scoped Subjects (All weights, detail views only)

| Subject Pattern | Description | Payload | Weight |
|-----------------|-------------|---------|--------|
| `agent.{agentId}.status` | Agent status change | `{ status, sessionStatus, containerStatus }` | Lightweight |
| `agent.{agentId}.event` | Agent event (harness output) | Full `StatusEvent` | Heavy |
| `agent.{agentId}.metrics` | Resource usage, token counts | `{ cpu, memory, tokens }` | Medium |
| `agent.{agentId}.created` | Agent created | Full `Agent` object | Medium |
| `agent.{agentId}.deleted` | Agent deleted | `{ agentId }` | Lightweight |

#### Broker-Scoped Subjects

| Subject Pattern | Description | Payload | Weight |
|-----------------|-------------|---------|--------|
| `broker.{brokerId}.status` | Broker status change | `{ status, resources }` | Lightweight |

### 12.4 Hub-Side Publishing

The Hub API publishes events after successful database operations. Status changes are dual-published to both grove-scoped and agent-scoped subjects so that grove-level subscribers receive lightweight deltas without needing per-agent subscriptions. Heavy events (harness output) are published only to agent-scoped subjects.

```go
// pkg/hub/service/agent_service.go

func (s *AgentService) UpdateStatus(ctx context.Context, agentID string, status StatusUpdate) error {
    // Update database
    if err := s.store.UpdateAgentStatus(ctx, agentID, status); err != nil {
        return err
    }

    agent, _ := s.store.GetAgent(ctx, agentID)

    // Publish to agent-scoped subject (detail subscribers)
    s.nats.Publish(fmt.Sprintf("agent.%s.status", agentID), map[string]interface{}{
        "status":          status.Status,
        "sessionStatus":   status.SessionStatus,
        "containerStatus": status.ContainerStatus,
        "timestamp":       time.Now().UTC(),
    })

    // Dual-publish to grove-scoped subject (list/grove subscribers)
    // Includes agentId so grove-level subscribers can identify the source
    s.nats.Publish(fmt.Sprintf("grove.%s.agent.status", agent.GroveID), map[string]interface{}{
        "agentId":         agentID,
        "status":          status.Status,
        "sessionStatus":   status.SessionStatus,
        "containerStatus": status.ContainerStatus,
        "timestamp":       time.Now().UTC(),
    })

    return nil
}

// Heavy events are NOT dual-published to grove scope
func (s *AgentService) PublishHarnessEvent(ctx context.Context, agentID string, event StatusEvent) error {
    // Only publish to agent-scoped subject
    // Grove-level subscribers do not receive these
    s.nats.Publish(fmt.Sprintf("agent.%s.event", agentID), event)
    return nil
}
```

### 12.5 Web Frontend SSE Endpoint

The SSE endpoint accepts subscription subjects as query parameters. Each SSE connection is scoped to the subjects declared at connection time. To change subscriptions, the client closes the connection and opens a new one — there is no in-band subscription mutation.

```typescript
// src/server/routes/sse.ts
import Router from '@koa/router';
import { Context } from 'koa';
import { SSEManager } from '../services/sse-manager';

const router = new Router();

// SSE endpoint with query-param subscriptions (WatchRequest pattern)
// Usage: GET /events?sub=grove.abc.>&sub=agent.xyz.>
router.get('/', async (ctx: Context) => {
  const subjects = Array.isArray(ctx.query.sub)
    ? ctx.query.sub as string[]
    : ctx.query.sub ? [ctx.query.sub as string] : [];

  if (subjects.length === 0) {
    ctx.status = 400;
    ctx.body = { error: 'At least one sub parameter is required' };
    return;
  }

  // Validate all subjects against user permissions
  const allowedSubjects = subjects.filter(subject =>
    canSubscribe(ctx.state.user, subject)
  );

  if (allowedSubjects.length === 0) {
    ctx.status = 403;
    ctx.body = { error: 'No permitted subjects' };
    return;
  }

  // Resume support: client can pass Last-Event-ID header for reconnection
  const lastEventId = ctx.headers['last-event-id']
    ? parseInt(ctx.headers['last-event-id'] as string, 10)
    : 0;

  // Create SSE connection with all subscriptions declared upfront
  const sseManager: SSEManager = ctx.state.sseManager;
  const conn = await sseManager.createConnection(
    ctx.state.user.id,
    allowedSubjects,
    lastEventId
  );

  // Set SSE headers
  ctx.set({
    'Content-Type': 'text/event-stream',
    'Cache-Control': 'no-cache',
    'Connection': 'keep-alive',
    'X-Accel-Buffering': 'no'
  });

  ctx.status = 200;
  ctx.body = conn.stream;

  // Cleanup on close
  ctx.req.on('close', () => {
    sseManager.removeConnection(conn.id);
  });
});

function canSubscribe(user: User, subject: string): boolean {
  const parts = subject.split('.');

  // Dashboard summary is available to all authenticated users
  if (subject === 'grove.*.summary') {
    return true;
  }

  if (parts[0] === 'grove') {
    const groveId = parts[1];
    return userHasGroveAccess(user, groveId);
  }

  if (parts[0] === 'agent') {
    const agentId = parts[1];
    return userHasAgentAccess(user, agentId);
  }

  return false;
}

export const sseRoutes = router;
```

---

## 13. Authentication

### 13.1 OAuth Flow

```
┌─────────┐     ┌─────────────┐     ┌──────────────┐     ┌─────────┐
│ Browser │────►│Web Frontend │────►│OAuth Provider│────►│ Hub API │
│         │     │   :9820     │     │(Google/GitHub)│     │ :9810   │
└─────────┘     └─────────────┘     └──────────────┘     └─────────┘
     │                │                    │                  │
     │  1. /auth/login                     │                  │
     │───────────────►│                    │                  │
     │                │  2. Redirect       │                  │
     │◄───────────────│───────────────────►│                  │
     │  3. OAuth flow │                    │                  │
     │◄───────────────────────────────────►│                  │
     │                │  4. Callback       │                  │
     │───────────────►│◄───────────────────│                  │
     │                │  5. Exchange code  │                  │
     │                │───────────────────►│                  │
     │                │◄───────────────────│                  │
     │                │  6. Create/get user                   │
     │                │────────────────────────────────────►│
     │                │◄────────────────────────────────────│
     │                │  7. Set session cookie               │
     │◄───────────────│                    │                  │
     │  8. Redirect   │                    │                  │
     │◄───────────────│                    │                  │
```

### 13.2 Auth Routes

```typescript
// src/server/routes/auth.ts
import Router from '@koa/router';
import { Context } from 'koa';
import { OAuth2Client } from 'google-auth-library';

const router = new Router();

// OAuth configuration
const oauth = {
  google: new OAuth2Client({
    clientId: process.env.GOOGLE_CLIENT_ID,
    clientSecret: process.env.GOOGLE_CLIENT_SECRET,
    redirectUri: `${process.env.BASE_URL}/auth/callback/google`
  }),
  github: {
    clientId: process.env.GITHUB_CLIENT_ID,
    clientSecret: process.env.GITHUB_CLIENT_SECRET
  }
};

// Login initiation
router.get('/login/:provider', async (ctx: Context) => {
  const { provider } = ctx.params;
  const returnTo = ctx.query.returnTo || '/';

  // Store returnTo in session
  ctx.session.returnTo = returnTo;

  if (provider === 'google') {
    const authUrl = oauth.google.generateAuthUrl({
      access_type: 'offline',
      scope: ['email', 'profile']
    });
    ctx.redirect(authUrl);
  } else if (provider === 'github') {
    const authUrl = `https://github.com/login/oauth/authorize?client_id=${oauth.github.clientId}&scope=user:email`;
    ctx.redirect(authUrl);
  } else {
    ctx.status = 400;
    ctx.body = { error: 'Unknown provider' };
  }
});

// OAuth callback
router.get('/callback/:provider', async (ctx: Context) => {
  const { provider } = ctx.params;
  const { code } = ctx.query;

  try {
    let userInfo: { email: string; name: string; avatar?: string };

    if (provider === 'google') {
      const { tokens } = await oauth.google.getToken(code as string);
      oauth.google.setCredentials(tokens);
      const ticket = await oauth.google.verifyIdToken({
        idToken: tokens.id_token!,
        audience: process.env.GOOGLE_CLIENT_ID
      });
      const payload = ticket.getPayload()!;
      userInfo = {
        email: payload.email!,
        name: payload.name!,
        avatar: payload.picture
      };
    } else if (provider === 'github') {
      // Exchange code for token
      const tokenRes = await fetch('https://github.com/login/oauth/access_token', {
        method: 'POST',
        headers: {
          'Accept': 'application/json',
          'Content-Type': 'application/json'
        },
        body: JSON.stringify({
          client_id: oauth.github.clientId,
          client_secret: oauth.github.clientSecret,
          code
        })
      });
      const { access_token } = await tokenRes.json();

      // Get user info
      const userRes = await fetch('https://api.github.com/user', {
        headers: { Authorization: `Bearer ${access_token}` }
      });
      const ghUser = await userRes.json();
      userInfo = {
        email: ghUser.email,
        name: ghUser.name || ghUser.login,
        avatar: ghUser.avatar_url
      };
    } else {
      throw new Error('Unknown provider');
    }

    // Create or get user from Hub API
    const hubResponse = await fetch(`${process.env.HUB_API_URL}/api/v1/auth/login`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        provider,
        email: userInfo.email,
        name: userInfo.name,
        avatar: userInfo.avatar
      })
    });

    const { user, token } = await hubResponse.json();

    // Set session
    ctx.session.user = user;
    ctx.session.token = token;

    // Redirect to original destination
    const returnTo = ctx.session.returnTo || '/';
    delete ctx.session.returnTo;
    ctx.redirect(returnTo);

  } catch (err) {
    console.error('OAuth error:', err);
    ctx.redirect('/auth/error?message=Authentication+failed');
  }
});

// Logout
router.post('/logout', async (ctx: Context) => {
  ctx.session = null;
  ctx.body = { success: true };
});

// Current user
router.get('/me', async (ctx: Context) => {
  if (ctx.session.user) {
    ctx.body = { user: ctx.session.user };
  } else {
    ctx.status = 401;
    ctx.body = { error: 'Not authenticated' };
  }
});

export const authRoutes = router;
```

### 13.3 Session Configuration

**Important:** Cookie names cannot contain colons (`:`) per RFC 6265. Use underscores instead.

```typescript
// src/server/config.ts

export interface SessionConfig {
  key: string;
  maxAge: number;
  secure: boolean;
  httpOnly: boolean;
  sameSite: 'strict' | 'lax' | 'none';
  signed: boolean;
}

export function getSessionConfig(): SessionConfig {
  return {
    // Note: Cookie names cannot contain colons per RFC 6265
    key: 'scion_sess',
    maxAge: 24 * 60 * 60 * 1000, // 24 hours
    secure: process.env.NODE_ENV === 'production',
    httpOnly: true,
    sameSite: 'lax',
    signed: true
  };
}
```

### 13.4 Debug Mode

When `SCION_DEBUG=true`, the web frontend enables:

1. **Debug Logging:** Detailed console output for session, auth, and API proxy middleware
2. **Debug Panel:** A UI component (`scion-debug-panel`) that displays:
   - Current authentication state (session user, state user)
   - Session info (exists, isNew, keys)
   - Cookie presence and names
   - OAuth configuration status
3. **Debug Endpoint:** `GET /auth/debug` returns JSON with full auth state

All fetch() calls to `/api/*` endpoints include `credentials: 'include'` to ensure session cookies are always sent.

---

## 14. Cloud Run Deployment

### 14.1 Container Configuration

```dockerfile
# Dockerfile
FROM node:20-alpine AS builder

WORKDIR /app
COPY package*.json ./
RUN npm ci

COPY . .
RUN npm run build

# Production image
FROM node:20-alpine AS runner

WORKDIR /app

# Copy built assets
COPY --from=builder /app/dist ./dist
COPY --from=builder /app/public ./public
COPY --from=builder /app/node_modules ./node_modules
COPY --from=builder /app/package.json ./

# Non-root user
RUN addgroup -g 1001 -S nodejs
RUN adduser -S scion -u 1001
USER scion

ENV NODE_ENV=production
ENV PORT=8080

EXPOSE 8080

CMD ["node", "dist/server/index.js"]
```

### 14.2 Cloud Run Service Definition

```yaml
# cloudrun.yaml
apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: scion-web
  labels:
    app: scion
    component: web-frontend
spec:
  template:
    metadata:
      annotations:
        autoscaling.knative.dev/minScale: "1"
        autoscaling.knative.dev/maxScale: "10"
        run.googleapis.com/cpu-throttling: "false"
        run.googleapis.com/startup-cpu-boost: "true"
    spec:
      containerConcurrency: 80
      timeoutSeconds: 300
      containers:
        - image: gcr.io/PROJECT_ID/scion-web:latest
          ports:
            - containerPort: 8080
          env:
            - name: NODE_ENV
              value: production
            - name: HUB_API_URL
              value: http://scion-hub:9810
            - name: NATS_URL
              valueFrom:
                secretKeyRef:
                  name: scion-secrets
                  key: nats-url
            - name: SESSION_SECRET
              valueFrom:
                secretKeyRef:
                  name: scion-secrets
                  key: session-secret
            - name: GOOGLE_CLIENT_ID
              valueFrom:
                secretKeyRef:
                  name: scion-secrets
                  key: google-client-id
            - name: GOOGLE_CLIENT_SECRET
              valueFrom:
                secretKeyRef:
                  name: scion-secrets
                  key: google-client-secret
          resources:
            limits:
              cpu: "2"
              memory: 1Gi
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8080
            initialDelaySeconds: 5
            periodSeconds: 10
          readinessProbe:
            httpGet:
              path: /readyz
              port: 8080
            initialDelaySeconds: 5
            periodSeconds: 5
```

### 14.3 Cold Start Optimization

Cloud Run instances may be scaled to zero. Optimize cold starts:

1. **Minimal dependencies:** Use ES modules, tree-shake unused code
2. **Lazy loading:** Load NATS client and other services on first request
3. **Startup CPU boost:** Enable `run.googleapis.com/startup-cpu-boost`
4. **Precompiled templates:** Pre-render static content during build

```typescript
// src/server/index.ts
import { createApp } from './app';
import { loadConfig } from './config';

async function main() {
  const config = loadConfig();
  const app = createApp(config);

  // Lazy-initialize services
  let natsClient: NatsClient | null = null;

  app.use(async (ctx, next) => {
    // Initialize NATS on first request
    if (!natsClient) {
      natsClient = new NatsClient();
      await natsClient.connect(config.nats);
    }
    ctx.state.nats = natsClient;
    await next();
  });

  const port = process.env.PORT || 8080;
  app.listen(port, () => {
    console.log(`Server listening on port ${port}`);
  });
}

main().catch(console.error);
```

### 14.4 Health Endpoints

```typescript
// src/server/routes/health.ts
import Router from '@koa/router';
import { Context } from 'koa';

const router = new Router();

// Liveness probe
router.get('/healthz', async (ctx: Context) => {
  ctx.body = {
    status: 'healthy',
    timestamp: new Date().toISOString()
  };
});

// Readiness probe
router.get('/readyz', async (ctx: Context) => {
  const checks = {
    hubApi: await checkHubApi(),
    nats: await checkNats()
  };

  const allHealthy = Object.values(checks).every(c => c === 'healthy');

  ctx.status = allHealthy ? 200 : 503;
  ctx.body = {
    status: allHealthy ? 'ready' : 'not ready',
    checks,
    timestamp: new Date().toISOString()
  };
});

async function checkHubApi(): Promise<string> {
  try {
    const res = await fetch(`${process.env.HUB_API_URL}/healthz`, {
      timeout: 5000
    });
    return res.ok ? 'healthy' : 'unhealthy';
  } catch {
    return 'unhealthy';
  }
}

async function checkNats(): Promise<string> {
  // Check NATS connection status
  // Implementation depends on nats.js client
  return 'healthy';
}

export const healthRoutes = router;
```

---

## 15. Configuration

### 15.1 Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `PORT` | No | `8080` | Server port |
| `NODE_ENV` | No | `development` | Environment mode |
| `HUB_API_URL` | Yes | - | Hub API base URL |
| `NATS_URL` | Yes | - | NATS server URL |
| `SESSION_SECRET` | Yes | - | Session signing secret |
| `GOOGLE_CLIENT_ID` | No | - | Google OAuth client ID |
| `GOOGLE_CLIENT_SECRET` | No | - | Google OAuth client secret |
| `GITHUB_CLIENT_ID` | No | - | GitHub OAuth client ID |
| `GITHUB_CLIENT_SECRET` | No | - | GitHub OAuth client secret |
| `BASE_URL` | Yes | - | Public base URL for OAuth callbacks |
| `LOG_LEVEL` | No | `info` | Logging level |
| `LOG_FORMAT` | No | `json` | Log format (json/text) |

### 15.2 YAML Configuration

```yaml
# config/default.yaml
server:
  port: 8080
  trustProxy: true

hub:
  url: "http://localhost:9810"
  timeout: 30000

nats:
  servers:
    - "nats://localhost:4222"
  reconnect: true
  maxReconnectAttempts: 10

session:
  maxAge: 86400000  # 24 hours
  secure: true
  sameSite: lax

auth:
  providers:
    - google
    - github

sse:
  heartbeatInterval: 30000
  maxConnectionsPerUser: 5

assets:
  maxAge: 86400  # 24 hours
  gzip: true
  brotli: true
```

---

## 16. Security Considerations

### 16.1 Content Security Policy

```typescript
// src/server/middleware/security.ts
import { Context, Next } from 'koa';

export function security(config: AppConfig) {
  return async (ctx: Context, next: Next) => {
    // Content Security Policy
    ctx.set('Content-Security-Policy', [
      "default-src 'self'",
      "script-src 'self' 'unsafe-inline' https://cdn.webawesome.com",
      "style-src 'self' 'unsafe-inline' https://cdn.webawesome.com",
      "font-src 'self' https://cdn.webawesome.com",
      "img-src 'self' data: blob: https:",
      "connect-src 'self' wss: https:",
      "frame-ancestors 'none'"
    ].join('; '));

    // Other security headers
    ctx.set('X-Content-Type-Options', 'nosniff');
    ctx.set('X-Frame-Options', 'DENY');
    ctx.set('X-XSS-Protection', '1; mode=block');
    ctx.set('Referrer-Policy', 'strict-origin-when-cross-origin');

    if (config.production) {
      ctx.set('Strict-Transport-Security', 'max-age=31536000; includeSubDomains');
    }

    await next();
  };
}
```

### 16.2 CSRF Protection

```typescript
// src/server/middleware/csrf.ts
import { Context, Next } from 'koa';
import csrf from 'koa-csrf';

export function csrfProtection() {
  return new csrf({
    invalidTokenMessage: 'Invalid CSRF token',
    invalidTokenStatusCode: 403,
    excludedMethods: ['GET', 'HEAD', 'OPTIONS'],
    disableQuery: true
  });
}
```

### 16.3 Rate Limiting

```typescript
// src/server/middleware/rate-limit.ts
import ratelimit from 'koa-ratelimit';

export function rateLimiter() {
  const db = new Map();

  return ratelimit({
    driver: 'memory',
    db,
    duration: 60000, // 1 minute
    max: 100,
    id: (ctx) => ctx.ip,
    headers: {
      remaining: 'X-RateLimit-Remaining',
      reset: 'X-RateLimit-Reset',
      total: 'X-RateLimit-Total'
    },
    disableHeader: false
  });
}
```

---

## 17. Implementation

See **`frontend-milestones.md`** for the detailed implementation plan with 16 milestones, deliverables, test criteria, and progress tracking.

### Milestone Summary

| Milestone | Description |
|-----------|-------------|
| M1 | Koa Server Foundation |
| M2 | Lit SSR Integration |
| M3 | Web Awesome Component Library |
| M4 | Authentication Flow |
| M5 | Hub API Proxy |
| M6 | Grove & Agent Pages |
| M7 | SSE + NATS Server Infrastructure |
| M8 | Client Real-Time State Management |
| M9 | Terminal Component |
| M10 | Agent Creation Workflow |
| M11 | Template Management UI |
| M12 | User & Group Management UI |
| M13 | Permissions & Policy Management UI |
| M14 | Environment Variables & Secrets UI |
| M15 | API Key Management UI |
| M16 | Production Hardening |
| M17 | Cloud Run Deployment |

---

## 18. References

### Design Documents
- **Server Implementation:** `server-implementation-design.md`
- **Hub API:** `hub-api.md`
- **Hosted Architecture:** `hosted-architecture.md`
- **Authentication Design:** `authentication-design.md`
- **Permissions Design:** `permissions-design.md`
- **Hosted Templates:** `hosted-templates.md`
- **Frontend Milestones:** `frontend-milestones.md`

### External Documentation
- **Lit Documentation:** https://lit.dev/
- **Web Awesome:** https://webawesome.com/
- **Koa Documentation:** https://koajs.com/
- **@lit-labs/ssr:** https://github.com/lit/lit/tree/main/packages/labs/ssr
- **NATS.js:** https://github.com/nats-io/nats.js
- **xterm.js:** https://xtermjs.org/
