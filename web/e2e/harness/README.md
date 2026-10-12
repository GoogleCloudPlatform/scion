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
  ├─ auth.ts             ← Creates authenticated sessions via test-login,
  │                         and redeems test identity web-login codes
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

## Test identity browser sessions

A *test identity* is a short-lived synthetic member or viewer user that a
test hub issues (ptone/scion#4240). A browser context can hold a hub web
session as a test identity without test-login: the runner issues the
identity and mints a single-use **web-login code** with the CLI, and the
harness redeems the code in the browser context.

The hub must run with test identities enabled at the member level and
carry the test infrastructure marker (`scion server test-infra mark`),
which holds the hub-local key for these sessions. `startHub()` does both
with `testIdentities: true`:

```typescript
import { startHub, runScionCLI } from './harness/hub.js';

const state = await startHub({ testLogin: false, testIdentities: true, stateFile });
```

That throwaway hub gets a random hub id, a temp database marked before
start, and a private HOME and `SCION_*`-free environment under its temp
dir. `runScionCLI(state, args)` runs the CLI against it as the dev-auth
admin (the issuer). The `test-identity-session` suite
(`e2e/test-identity-session/`, run in the Web E2E workflow) is the
reference use.

### Flow

```typescript
import { assertTestIdentity, redeemTestIdentity } from './harness/auth.js';

// 1. Issue (out of the browser). The token file is not needed for the
//    browser path; delete it.
const issued = JSON.parse(
  runScionCLI(state, ['hub', 'test-identity', 'issue', '--out', tokenFile, '--json'])
);
// 2. Mint a code into a new 0600 file in a task-owned temp dir.
runScionCLI(state, ['hub', 'test-identity', 'web-code', issued.identity.id, '--out', codeFile]);

// 3. Redeem in a fresh context, before any navigation.
const context = await browser.newContext({ baseURL: state.baseURL });
await redeemTestIdentity(context, { codeFile }); // reads, redeems, removes codeFile
await assertTestIdentity(context.request, { email: issued.identity.email, role: 'member' });

// 4. Journeys; assert the identity again after each.
const page = await context.newPage();
await page.goto('/', { waitUntil: 'domcontentloaded' });
// ...
await assertTestIdentity(page, { email: issued.identity.email });

// 5. Teardown: close pages, delete the identity; /auth/me then answers 401.
```

### Helpers

| Function | What it does |
| --- | --- |
| `redeemTestIdentity(context, opts)` | `POST /auth/test-identity/redeem` with `{code}` from `opts.code` or `opts.codeFile`; expects 204 and leaves the session cookie in the context. With `codeFile`, the file is read and removed after a successful redeem. `opts.baseURL` defaults to the context's baseURL. `opts.proxyAuthorization` is sent as `Proxy-Authorization` (IAP-fronted hubs). Any other answer throws with the status and the hub error code only, never a body. |
| `assertTestIdentity(target, {email, role?})` | `target` is a page or a request context. Reads `GET /auth/me` and fails on an email mismatch, on a role mismatch when `role` is given, or when the session is not a test identity session; also checks that `GET /api/v1/auth/me` answers for the same email. Returns `{email, role, expiresAt}`. |
| `withTracingPaused(context, fn)` | Runs `fn` with trace recording paused (used by the redeem). |

### Rules

- The code is valid for **5 minutes** and can be redeemed **once**. A new
  code replaces an unredeemed earlier one.
- **Traces:** the redeem pauses trace recording (it ends the current trace
  chunk without saving it and starts a new one), so no trace holds the
  redeem body or its `Set-Cookie`. Actions in the same context before the
  redeem are dropped from the trace; redeem first.
- Keep the code out of URLs, logs, screenshots and argv: pass `codeFile`,
  not the code. The CLI never prints the code.
- Write no `storageState` of a test identity session to a shared path.
- Use `domcontentloaded` (SSE keeps connections open).
- After teardown, the first request with the ended session answers 401
  and clears it; with `--dev-auth`, a later request without a session is
  logged in as the dev admin again. Close the pages before deleting the
  identity, and check `/auth/me` once.
- test-login (`createSession()`) is unchanged for the main suite.

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
   `scopeType` defaults to `'system'`, whose `scopeId` defaults to `''`
   (the hub's representation of system scope; the `createRoleBinding`
   helper, not the hub, rejects a non-empty value). Any other scope, e.g.
   `'project'`, requires an explicit `scopeId`.
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
