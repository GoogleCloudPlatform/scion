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
- **Browser**: a member test identity, made project-admin of the fixture
  project, redeems a web-login code in its own browser context. The
  journeys use that session cookie only; the spec fails any request that
  carries an Authorization header.

No traces, videos or screenshots are kept: they would hold the session
cookie.
