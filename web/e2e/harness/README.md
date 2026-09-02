# E2E Test Harness

End-to-end tests run against the **real Go hub binary** with an ephemeral
SQLite database. No mocked APIs are used anywhere in the E2E path.

## Quick start

```bash
cd web
npm run test:e2e
```

This single command builds assets, starts the hub, seeds data, and runs all
Playwright specs. Artifacts land in `test-results/` and `playwright-report/`
(both gitignored).

## Architecture

```
global-setup.ts          ← Playwright calls this before any spec
  ├─ hub.ts              ← Builds web + Go binary, starts hub on ephemeral port
  ├─ auth.ts             ← Creates authenticated sessions via test-login
  └─ seed.ts             ← Seeds groups, users, role bindings via the real API
smoke.spec.ts            ← Proves the loop: login → navigate → assert → axe
global-teardown.ts       ← Kills the hub, cleans up temp files
```

## How the hub is started

1. `npm run build` produces the web assets in `web/dist/client/`.
2. `make build` compiles the Go binary to `build/scion`.
3. The binary is started with:
   - `--foreground` (no daemonization)
   - `--host 127.0.0.1` (loopback only — required for dev-auth)
   - `--web-port <ephemeral>` (random free port)
   - `--dev-auth` (auto-login as admin on first page load)
   - `--enable-test-login` (programmatic session creation endpoint)
   - `--session-secret e2e-test-secret` (deterministic signing key)
   - `--db sqlite://<tmpdir>/e2e.db` (ephemeral database)
   - `--enable-hub --enable-web` (combined mode — API on the web port)
4. Setup waits for `GET /healthz` to return `{"status": "healthy"}`.

## Authentication strategy

### Admin user (dev-auth)

In workstation mode with `--dev-auth`, the first page load auto-creates a
session cookie for the development admin user (UUID
`be67fbc9-c869-5d43-b15d-c28ca3e8d355`, role `admin`).

### Programmatic multi-user auth (test-login)

For tests that need non-admin identities or multiple concurrent users:

1. The hub is started with `--enable-test-login` and a known
   `--session-secret`.
2. The test harness **derives the user-signing key** from the session secret
   using the same algorithm as the Go hub:
   ```
   sha256("scion-hub-signing-key:user_signing_key:" + sessionSecret)
   ```
3. It mints a short-lived HS256 JWT with audience `scion-test-login` (the
   "challenge token") using that derived key.
4. It calls `POST /api/v1/auth/test-login` with:
   - `Authorization: Bearer <challenge-token>`
   - Body: `{ "email": "...", "role": "admin|member|viewer", "displayName": "..." }`
5. The endpoint creates or updates the user, sets the session cookie, and
   returns an access token.
6. The session cookie is saved as a Playwright `storageState` JSON file.

### Using auth in specs

```typescript
import { getE2EEnv } from './harness/env.js';

test.describe('My feature', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState });

  test('admin can do X', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    // ...
  });
});
```

For a custom user, create a session in a `test.beforeAll`:

```typescript
import { createSession } from './harness/auth.js';
import { getE2EEnv } from './harness/env.js';

let viewerStorageState: string;

test.beforeAll(async () => {
  const env = getE2EEnv();
  const session = await createSession(env.baseURL, {
    email: 'viewer@e2e.test',
    role: 'viewer',
    displayName: 'E2E Viewer',
  });
  viewerStorageState = session.storageStatePath;
});

test.describe('Viewer restrictions', () => {
  test.use({ storageState: () => viewerStorageState });
  // ...
});
```

## Seeding test data

Seed helpers in `seed.ts` call the real hub API using the admin access token.
Available helpers:

| Function                        | What it creates                                    |
| ------------------------------- | -------------------------------------------------- |
| `createGroup()`                 | An explicit group with name, slug, labels           |
| `addGroupMember()`              | A user or group member with a governance role       |
| `createRoleBinding()`           | A role binding for a principal at a scope           |
| `findRoleDefinition()`          | Looks up a role definition by name                  |
| `setMaxMembersPerGroupQuota()`  | Sets the `max_members_per_group` limit to a value   |
| `createAccessBoundary()`        | Creates an access constraint via the preview flow   |
| `seedSmokeData()`               | Seeds the minimum data for the smoke test           |

### Adding a new identity with specific permissions

1. Create the user session via `createSession()` (this creates the user in
   the hub's database).
2. Find the role definition: `findRoleDefinition(baseURL, token, 'hub-admin')`.
3. Create a role binding: `createRoleBinding(baseURL, token, { ... })`.
4. The user's next API call will reflect the new permissions.

## Navigation waits

**Always use `domcontentloaded`**, never `networkidle`. The hub uses
Server-Sent Events (SSE) for real-time updates, which keeps a connection
open indefinitely. `networkidle` would never resolve.

```typescript
// CORRECT
await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });

// WRONG — will time out
await page.goto('/admin/groups', { waitUntil: 'networkidle' });
```

## Flake policy

- No `waitForTimeout()` — use Playwright auto-waiting and `expect()` with
  built-in retries.
- Use semantic locators (`getByRole`, `getByText`, `getByLabel`) over CSS
  selectors.
- All assertions use `expect()` which auto-retries by default.

## Artifacts

| Path                | Contents                          | Gitignored |
| ------------------- | --------------------------------- | ---------- |
| `web/test-results/` | Screenshots, videos, traces       | Yes        |
| `web/playwright-report/` | HTML report                  | Yes        |

To view the report after a run:

```bash
npx playwright show-report
```
