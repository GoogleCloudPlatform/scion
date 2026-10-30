# Port proxy journeys

Playwright journeys for an agent's exposed port opened through the hub's
path proxy (`/api/v1/agents/<id>/ports/<port>/proxy/...`) in a signed-in
browser (ptone/scion#5568). See the header of `port-proxy.pw.ts` for the
checks.

## Run

Local throwaway hub (default). Builds the hub binary, the web assets and
the fake agent helper unless `E2E_PREBUILT=1`:

```bash
cd web
npx playwright test -c e2e/port-proxy/playwright.config.ts
```

Prebuilt, as in CI:

```bash
make build
npm --prefix web run build
go build -buildvcs=false -o build/e2e-fakeagent ./hack/e2e/fakeagent
cd web
E2E_PREBUILT=1 npx playwright test -c e2e/port-proxy/playwright.config.ts
```

The page view journeys (3 and 4, ptone/scion#5569; see the header of
`port-view.pw.ts`) need `hub.port_view` on from hub start, so they run on a
second hub of the same rig under their own config:

```bash
cd web
npx playwright test -c e2e/port-proxy/playwright.view.config.ts
```

That config sets `E2E_PP_VIEW=1` for global setup, which then starts the
hub with `hub.port_view` on as well, serves the port from the conduit agent
only, makes the member project-owner of the fixture project, and signs in
two more test identities: a peer (project-admin) and an outsider (no
project role). The default config skips `port-view.pw.ts`.

`E2E_FAKEAGENT_BINARY` points at another helper binary.
`E2E_KEEP_HUB_STATE=1` keeps the hub's temp dir and the suite's state dir
(helper and fixture logs) at teardown; both hold hub credentials, so do not
copy them into evidence or artifacts.

No container runtime or credentials are needed.

## The rig

- **Hub**: a member-level test hub from the shared harness
  (`../harness/hub.ts`), started with `startupExperiments: ['hub.conduit']`:
  the harness turns the experiment on through the admin API and restarts
  the hub on the same database, so the conduit relay runs.
- **Fixture app** (`serve-fixture.mjs`, files in `fixture/`), on a loopback
  port:
  - `single.html`: inline style and script; writes `single-ok` to `#probe`.
  - `index.html`: stylesheet `./site.css`, module script `./app.js`, image
    `./img/dot.png` (built from bytes; no binary in the repo). `app.js`
    fetches `./api/ping` and echoes over the WebSocket `./ws`, then writes
    `ok` (or `fail:...`) to `#probe`.
  - `abs.html`: loads `/app.js` by root-relative URL.
- **Agents**: `hack/e2e/fakeagent` joins the hub as a Runtime Broker (with a
  join token it requests as the dev-auth admin), creates a project and one
  agent per transport (`port-proxy-conduit`, `port-proxy-legacy`), and runs
  the real agent-side clients in process with the environment the broker
  built for each agent. Each agent registers the fixture port; the helper
  writes its ready file once the port answers through the hub.
- **Restart**: after the agents are ready, global setup issues the member
  test identity and makes it project-admin of the fixture project, then
  restarts the hub without development sign-in
  (`restartHubWithoutDevAuth`), as in production. The agents reconnect on
  their own; the spec waits until each port answers again before the
  journeys.
- **Browser**: global setup redeems the member's web-login code once after
  the restart and keeps the session as a storage state (0600, in the
  state dir); the spec's browser context loads it, so a retried run signs
  in the same way. The
  journeys use that session cookie only. Every journey request is made by a
  page, and the spec fails any page request that carries an Authorization
  header (the 308 check reads the redirect hop of a page navigation).

No traces, videos or screenshots are kept: they would hold the session
cookie.
