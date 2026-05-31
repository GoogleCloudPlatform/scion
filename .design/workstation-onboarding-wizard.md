# Workstation Onboarding Wizard — Detailed Sub-Design (W1 + W4)

**Date:** 2026-05-30 (decisions folded in 2026-05-31)
**Status:** Proposal — Detailed Design (decisions confirmed)
**Author:** Scion Agent (workstation-onboarding)
**Parent doc:** [`workstation-onboarding.md`](./workstation-onboarding.md) (see §1a for
the confirmed decisions and §7 for the single primary work sequence)
**Scope:** W1 (onboarding wizard + supporting API) and W4 (harness-aware, Go-native
image **pull + local build** with SSE progress). Touches W2/W3 only where the wizard
depends on them.

> **Confirmed decisions (2026-05-31):** **D1** identity is cosmetic (settable display
> name/email, stable UUID, default OS user) — the identity step writes it via
> `PUT /system/identity`. **D3** images: prebuilt pull from the pre-seeded
> `ghcr.io/homebrew-scion` is the default; **add a local build option** once a runtime
> is confirmed. **D4** per-image pull progress (build streams log lines). **D8**
> `scion server start` **auto-opens** the browser to `/onboarding` when un-onboarded and
> **always prints the URL, before backgrounding**. SSE travels on the shared `/events`
> stream (`system.images.<jobId>`); status cached in `sessionStorage`.

---

## 1. Scope & Relationship to Parent Doc

The parent doc surveys the whole initiative and recommends two follow-up sub-designs.
This is the first: it specifies **how the browser-based first-run wizard works** and
**the API it stands on**. It deliberately defers:

- **Linked-groves-from-browser UX (W5)** → its own doc (`linked-groves-ui.md`). This
  doc only defines the *workspace step's* contract with that work.
- **Identity config internals (W2)** and **dev-token rename (W3)** → folded into
  implementation PRs. This doc consumes them: the wizard's "identity" step is the UI
  that *writes* the identity config W2 introduces.

What this doc fully owns:
1. The wizard **UX flow** and an explicit **state machine** (steps, transitions,
   resumability).
2. The **first-run detection signal** — what marks a machine as "needs onboarding".
3. The **bootstrap-auth window** — how the wizard is reachable *before* identity is set.
4. The **API surface**: `system/check`, `system/runtime`, `system/init`,
   `system/images/*` (including SSE progress), plus a small `system/status` endpoint
   that powers first-run detection and resumability.

---

## 2. Architecture Overview

```
Browser (Lit SPA)                         Hub (Go, net/http.ServeMux)
─────────────────                         ───────────────────────────
/onboarding route        ── GET ──▶  GET  /api/v1/system/status     ┐
  scion-page-onboarding                                              │ thin wrappers over
  (wizard state machine) ── GET ──▶  GET  /api/v1/system/check      │ existing logic:
                                     GET  /api/v1/system/runtime    │  • doctor.go
                         ── POST ─▶  PUT  /api/v1/system/runtime    │  • runtime_detect.go
                                     POST /api/v1/system/init       │  • config.InitMachine
                         ── POST ─▶  POST /api/v1/system/images/pull│  • runtime.PullImage (NEW glue)
                         ── SSE ──▶  GET  /api/v1/system/images/events ┘ (new SSE channel)
```

Everything runs against the **co-located workstation Hub** bound to `127.0.0.1`. No new
process, no new server — new handlers register on the existing `s.mux` in
`pkg/hub/server.go` and a new Lit page registers on the existing client router in
`web/src/client/main.ts`.

---

## 3. First-Run Detection

### 3.1 The signal

A machine "needs onboarding" when its bootstrap is **incomplete**. Rather than a single
boolean, compute a small struct from cheap filesystem/config probes and let the wizard
decide which steps remain. This makes the signal double as the resumability source
(§5.4).

The authoritative computation lives server-side in a new
`GET /api/v1/system/status` handler. It assembles:

| Field | Source of truth | "Done" when |
|---|---|---|
| `initialized` | `config.GetSettingsPath()` returns a path (i.e. `~/.scion/settings.yaml` exists) — see `pkg/config/init.go:560` | settings file present |
| `runtimeDetected` | `config.DetectLocalRuntime()` (`pkg/config/runtime_detect.go:57`) returns no error | a runtime is reachable |
| `runtimeConfigured` | `VersionedSettings.ResolveRuntime("")` yields a non-empty type (`pkg/config/settings_v1.go:90`) | runtime persisted in settings |
| `harnessesSeeded` | non-empty `VersionedSettings.HarnessConfigs` (`settings_v1.go:224`) | at least one harness-config seeded |
| `imageRegistry` | `VersionedSettings.ResolveImageRegistry("")` (`settings_v1.go:152`) | (informational; may be empty — see §7.4) |
| `imagesPresent` | per chosen harness, `runtime.ImageExists(ctx, img)` (`pkg/runtime/interface.go:64`) | all chosen-harness images exist |
| `identitySet` | new identity fields on `V1AuthConfig` (W2) are non-default (not `dev@localhost`) | user has named themselves |
| `hasWorkspace` | `store.ListProjects` returns ≥1 project | at least one project exists |

`needsOnboarding = !initialized || !harnessesSeeded || !hasWorkspace` (the hard
minimum). Soft-incomplete fields (no images, default identity) don't *force* the wizard
but pre-select the resume step.

### 3.2 Where detection is consumed

Two consumers:

1. **CLI quickstart + auto-open (D8)** — `printWorkstationQuickstart()`
   (`cmd/server_daemon.go:362`) currently prints only a URL + token. Extend it: if
   `needsOnboarding`, **print the `/onboarding` URL prominently** (e.g.
   `Open http://127.0.0.1:8080/onboarding to finish setup`) and **auto-open the browser**
   to it. Two firm requirements from D8:
   - **Always print, and print *before* the daemon backgrounds itself** — the URL must
     reach the terminal regardless of whether auto-open succeeds, and before the process
     detaches. (The daemon launch path forks/backgrounds in
     `runServerStartOrDaemon`, `cmd/server_daemon.go:33-161`; emit the quickstart on the
     parent/foreground side before detaching.)
   - **Auto-open is best-effort and guarded** — use `open`/`xdg-open`/`start` behind a
     `--no-browser` opt-out, and **skip auto-open when stdout is not a TTY or the session
     looks headless/SSH** (`$SSH_CONNECTION`), so CI and remote starts don't try to
     launch a browser. Print-only is the always-on fallback.
   This requires the daemon to call the same status computation (factor it into
   `pkg/config` so both CLI and handler share it — `pkg/config/onboarding_status.go`,
   returning a struct the handler JSON-encodes).

2. **Browser redirect** — the client router (`web/src/client/main.ts`, `renderRoute()`
   ~line 329) gains a guard: on first navigation to `/` (dashboard), if the SPA has not
   yet confirmed setup, fetch `/api/v1/system/status`; when `needsOnboarding`, call
   `navigateTo('/onboarding')`. Gate this behind a workstation-mode flag exposed in the
   bootstrapped page data (`__SCION_DATA__`, consumed in `main.ts:238`) so production
   Hubs never trigger it. Cache the "setup complete" result in `sessionStorage` to avoid
   a status fetch on every navigation.

### 3.3 Why not just "settings.yaml missing"

The parent doc's open question (Q2) asks for "a crisp, cheap signal." A bare
`settings.yaml`-exists check is cheap but wrong: `scion server start` itself can create
settings via workstation defaults before the user has chosen harnesses or made a
workspace. The struct approach keeps each probe cheap (all are stat/in-memory except
`imagesPresent`, which is only computed when explicitly requested with
`?images=true`) while correctly distinguishing "server booted" from "user onboarded".

---

## 4. Bootstrap-Auth Window

This is the subtle part (parent Q3): onboarding must run *before* identity is set, yet
every `/api/v1/*` route sits behind auth.

### 4.1 The window already exists — via dev-auth auto-login

In workstation mode, `applyWorkstationDefaults()` (`cmd/server_config.go:25`) turns on
dev-auth, so the web server starts with `ws.config.DevAuthToken != ""`. The web
middleware chain (`buildHandler`, `pkg/hub/web.go:1618`) then runs
`devAuthMiddleware` (`web.go:1115`), which **auto-creates an admin session for any
browser request when no user is in the session** (`web.go:1141-1177`):

```go
// No user — auto-login with dev identity
devUser := &webSessionUser{ UserID: DevUserID, Email: "dev@localhost",
                            Name: "Development User", Role: "admin" }
... session.Save ...
// also mints Hub JWTs so session-to-bearer can auth /api/v1 calls
```

Consequences for onboarding:

- A fresh browser hitting `/onboarding` is **already authenticated** as the admin
  DevUser, with no login screen (`sessionAuthMiddleware`, `web.go:1181`, sees the user
  in context and passes through).
- API calls from the wizard use the same session cookie; the
  session-to-bearer middleware (mounted at `MountHubAPI`, `web.go:518`) converts the
  session's Hub JWT into a Bearer token, so `UnifiedAuthMiddleware`
  (`pkg/hub/auth.go:68`) accepts them as the admin DevUser.

**So the bootstrap window is "the dev-auth auto-login session on loopback."** No new
unauthenticated endpoints are required, and we explicitly **do not** add the
`system/*` routes to `isUnauthenticatedEndpoint()` (`auth.go:292`) — keeping them
admin-gated is safer.

### 4.2 Fencing the window

The auto-login is only acceptable because workstation mode binds to `127.0.0.1`
(`applyWorkstationDefaults`) and is single-user. To prevent these powerful endpoints
(they write `~/.scion`, pull images, read the host) from ever being reachable on a
multi-user/production Hub, every `system/*` handler **must** guard on workstation mode:

```go
if !s.requireWorkstation(w, r) { return } // 404 in production
```

**Fence mechanism (reconciled with [`linked-groves-ui.md`](./linked-groves-ui.md) §4 —
this is the authoritative version):** `ServerConfig` has no operating-mode field today
(confirmed in `pkg/hub/server.go`). Add a **`Workstation bool`** to `ServerConfig`, set
from the already-computed `!productionMode` boolean where the config literal is
assembled (`cmd/server_foreground.go:774`). Store `s.workstation` and expose a
`requireWorkstation(w, r)` helper that returns **404** (not 403, keeping the surface
invisible) when false. Do **not** infer mode from `DevAuthToken != ""` or the bind host
— those are separately overridable and would couple unrelated concerns. The same flag
and helper guard the `system/*` endpoints here and the `fs/*` endpoints in W5.

### 4.3 Transition to configured identity

The identity step (W2) writes `username`/`displayName`/`email` into `V1AuthConfig` and
the wizard then refreshes the session so the DevUser's email/display name reflect the
chosen identity (the **UUID stays `DevUserID`** for DB integrity — parent §2.4). Two
implementation choices, decide at build time:

- **Simplest:** identity fields are read at `DevUser`/`webSessionUser` construction
  time from settings; after the identity POST, the wizard forces a session refresh
  (clear `sessKeyUser*`, let `devAuthMiddleware` re-populate from the now-updated
  config). Requires `devAuthMiddleware` to source identity from settings instead of the
  hardcoded literals at `web.go:1144-1147` / `devauth.go:42-45`.
- That hardcoding is exactly what W2 replaces; this doc assumes W2 lands first or
  alongside, and the wizard's identity step is its UI.

---

## 5. Wizard UX & State Machine

### 5.1 Route & shell

- **Route:** add to the `ROUTES` array in `web/src/client/main.ts` (the table around
  lines 127-158):
  ```ts
  { pattern: /^\/onboarding$/, tag: 'scion-page-onboarding',
    load: () => import('../components/pages/onboarding.js') }
  ```
- **Shell:** use the **standalone** shell (like `/login` and `/invite`) by adding the
  tag to `STANDALONE_ROUTES` (`main.ts:163`). The wizard is a full-screen takeover, not
  a page inside the app chrome with sidebar.
- **New component:** `web/src/components/pages/onboarding.ts`,
  `@customElement('scion-page-onboarding') class ScionPageOnboarding extends LitElement`.
  Model it on `invite.ts` (`web/src/components/pages/invite.ts`) for the
  multi-`@state()` step machine and on `admin-server-config.ts` for the form-field /
  `apiFetch` / `extractApiError` patterns. Reuse Shoelace `sl-tab`/`sl-step`-style
  components already in the bundle.

### 5.2 Steps (matches parent §4)

| # | Step id | Purpose | Primary API | Blocking? |
|---|---|---|---|---|
| 0 | `welcome` | intro + load `system/status` | `GET /system/status` | no |
| 1 | `identity` | set display name + email (W2, cosmetic — D1) | `PUT /system/identity` | no (defaults to OS user) |
| 2 | `system-check` | run doctor checks | `GET /system/check` | **block on hard fail** (no runtime) |
| 3 | `runtime` | confirm/switch runtime | `GET` + `PUT /system/runtime` | block until one valid runtime persisted |
| 4 | `harnesses` | pick harnesses; seed configs | `POST /system/init` | block until ≥1 chosen |
| 5 | `images` | pull/verify images w/ progress | `POST /system/images/pull` + SSE | non-block (skippable; can build later) |
| 6 | `workspace` | create first project / link grove | existing `POST /api/v1/projects` (+ W5) | block until ≥1 workspace |
| 7 | `done` | summary; `navigateTo('/')` | — | terminal |

Step 1 (identity) is intentionally early ("welcome / identity" per parent §4) but
non-blocking: skipping it keeps the OS-user default (W2). The hard gates are 2→3
(runtime must exist), 4 (a harness), and 6 (a workspace) — these define
`needsOnboarding` in §3.1.

### 5.3 State machine

```
            ┌─────────── on mount: GET /system/status ───────────┐
            ▼                                                     │
 ┌────────────────────────────────────────────────────────────┐ │
 │ resume(status): pick first incomplete step (see 5.4 table)  │─┘
 └────────────────────────────────────────────────────────────┘
            │
            ▼
 [welcome] ──next──▶ [identity] ──next/skip──▶ [system-check]
                                                    │
                          hard fail (no runtime) ───┤ (show remediation,
                                                    │  allow re-run only)
                                                    ▼ pass/warn
                                               [runtime]
                                                    │ PUT ok
                                                    ▼
                                               [harnesses]
                                                    │ POST /system/init ok
                                                    ▼
                                               [images] ──skip──┐
                                                    │ pull done │
                                                    ▼           ▼
                                               [workspace] ◀────┘
                                                    │ project created
                                                    ▼
                                                 [done] ──▶ navigateTo('/')
```

Each step is a `@state() currentStep` enum. `next()`/`back()` mutate it; guarded steps
refuse `next()` until their precondition holds (e.g. `runtimeConfigured`). All API
results are kept in component state (`status`, `checkReport`, `detectedRuntime`,
`selectedHarnesses`, `imageProgress[]`) so back/forward navigation never re-fetches
unnecessarily. Client state is **derived**, not authoritative — the server status is
the source of truth, re-fetched on `done` to confirm completion before redirect.

### 5.4 Resumability

On mount, `resume(status)` maps the §3.1 struct to a starting step:

| Condition (first match wins) | Resume at |
|---|---|
| `!runtimeDetected` (doctor would hard-fail) | `system-check` |
| `!runtimeConfigured` | `runtime` |
| `!harnessesSeeded` | `harnesses` |
| `!imagesPresent` (and harnesses chosen) | `images` |
| `!hasWorkspace` | `workspace` |
| otherwise | `done` (then auto-redirect) |

Every backing operation is **idempotent** (§6.5, §7.5), so resuming and re-running a
completed step is safe. A returning user with a half-set-up machine lands on the right
step instead of restarting (parent §4 closing requirement).

---

## 6. API Surface — System (W1)

All routes mount on the existing stdlib mux in `pkg/hub/server.go registerRoutes()`
(pattern around lines 1990-2116, e.g. `s.mux.HandleFunc("/api/v1/agents", s.handleAgents)`).
All use the `(s *Server)` receiver to reach `s.config`, `s.store`, `s.events`
(`pkg/hub/handlers.go`). All begin with the workstation-mode guard (§4.2) and require
the admin DevUser via the normal middleware chain. JSON via the existing `writeJSON` /
`writeError` helpers.

New files: `pkg/hub/handlers_system.go` (handlers) and
`pkg/config/onboarding_status.go` (shared status logic).

### 6.1 `GET /api/v1/system/status`

Returns the §3.1 struct. `?images=true` additionally probes `ImageExists` per chosen
harness (slower). Powers first-run detection and wizard resume.

```jsonc
{
  "needsOnboarding": true,
  "initialized": true,
  "runtimeDetected": "podman",
  "runtimeConfigured": "",
  "harnessesSeeded": [],
  "imageRegistry": "",
  "imagesPresent": null,            // null unless ?images=true
  "identitySet": false,
  "hasWorkspace": false
}
```

Implementation: call the shared `config.ComputeOnboardingStatus(ctx, store, runtime)`
so the CLI quickstart (§3.2) reuses it.

### 6.2 `GET /api/v1/system/check`

Wraps doctor. Reuse `pkg/runtime/doctor.go` types directly:

```go
type CheckResult struct {        // pkg/runtime/doctor.go:17
  Name, Status, Message, Remediation string  // status ∈ pass|warn|fail|skip
}
type DiagnosticReport struct { Runtime string; Checks []CheckResult }
```

The CLI's `runDoctor()` (`cmd/doctor.go:48`) prints directly and isn't reusable as-is.
**Refactor:** extract the check-gathering core from `cmd/doctor.go` into a returnable
`func GatherDiagnostics(ctx) DiagnosticReport` (in `pkg/runtime` or a new
`pkg/runtime/checks.go`) that runs `checkGit`/`checkTmux` (`cmd/doctor.go:154,168`)
plus runtime reachability and returns `[]CheckResult` instead of calling
`printCheck`. The CLI then becomes a thin printer over the same function — no behavior
drift between CLI doctor and wizard system-check. Response: `DiagnosticReport` as JSON.

The wizard renders pass/warn/fail with remediation and only **blocks** when a `fail`
indicates no runtime.

### 6.3 `GET /api/v1/system/runtime` and `PUT /api/v1/system/runtime`

- **GET** → `{ "detected": "podman", "configured": "", "candidates": ["podman","docker"] }`.
  `detected` from `config.DetectLocalRuntime()` (`runtime_detect.go:57`); `configured`
  from `VersionedSettings.ResolveRuntime("")` (`settings_v1.go:90`); `candidates` from
  probing the known set (podman/docker/container) for availability.
- **PUT** `{ "type": "docker" }` → validate it's a real, reachable runtime (instantiate
  via `pkg/runtime/factory.go GetRuntime` or a lightweight probe), then persist. Persist
  through `config.UpdateSetting(projectPath="", key="runtimes.local.type"/"runtime",
  value, global=true)` (`pkg/config/settings.go:535`) or load→mutate→
  `SaveVersionedSettings` (`settings_v1.go:1870`). Return the updated GET payload.

Setting a runtime that doesn't resolve returns `400` with remediation text.

### 6.4 `POST /api/v1/system/init`

Wraps `config.InitMachine(harnesses, opts)` (`pkg/config/init.go:548`):

```jsonc
// request
{ "harnesses": ["claude", "gemini"], "imageRegistry": "ghcr.io/acme", "force": false }
```

- Map harness ids → `[]api.Harness` (the same resolution `cmd/project.go:82` uses).
- Build `InitMachineOpts{ Force: req.force, ImageRegistry: req.imageRegistry }`
  (`init.go:538`).
- `InitMachine` creates `~/.scion`, detects runtime, seeds `settings.yaml`, seeds the
  chosen harness-configs, seeds the default template, ensures a broker ID — all
  idempotent (it skips seeding when settings already exist; `MkdirAll`/`ensureBrokerID`
  are no-ops when present, per `init.go:554-635`).
- Response: the refreshed `GET /system/status` body so the wizard advances without a
  second round-trip.

Note: `InitMachine` seeds harness-configs for the harnesses passed. The wizard's
harness *selection* is the input; passing only chosen harnesses keeps the seed minimal.

### 6.5 Idempotency & errors

Every handler is safe to call repeatedly (re-init, re-detect, re-put runtime). Errors
use `writeError(w, status, code, message, details)` with actionable `message` strings
the wizard surfaces via `extractApiError` (`web/src/client/api.ts:96`).

---

## 7. API Surface — Images (W4)

Today image pulling is only the shell script
`image-build/scripts/pull-containers.sh` (pulls `scion-claude|gemini|opencode|codex`,
prunes after). There is no Go path wired to the server. W4 adds one, reusing the
runtime interface that already supports pulls.

### 7.1 Building blocks to reuse

- **Runtime interface** (`pkg/runtime/interface.go:56`): already has
  `ImageExists(ctx, image) (bool, error)` (line 64) and `PullImage(ctx, image) error`
  (line 65), implemented for Docker (`docker.go:274-281`), Podman (`podman.go:354,359`),
  K8s (`k8s_runtime.go:1924,1937`). The workstation runtime comes from
  `pkg/runtime/factory.go GetRuntime`.
- **Registry resolution** (`pkg/config/settings_v1.go`):
  `ResolveImageRegistry(profile)` (line 152) and `RewriteImageRegistry(fullImage,
  newRegistry)` (line 190) to compute the actual image refs per harness.
- **Image set per harness:** the four `scion-<harness>` names from the shell script,
  resolved against the configured registry + tag.

### 7.2 New Go glue

New file `pkg/runtime/imagepull.go` (runtime-agnostic, depends only on the `Runtime`
interface + settings):

```go
// ImagesForHarnesses maps chosen harness ids to fully-qualified image refs,
// applying ResolveImageRegistry + RewriteImageRegistry.
func ImagesForHarnesses(harnesses []string, settings *config.VersionedSettings) []string

// PullImages pulls each image via rt, emitting progress callbacks. Skips images
// that already exist (ImageExists) unless force. Errors per-image are reported,
// not fatal to the batch.
func PullImages(ctx context.Context, rt Runtime, images []string,
                force bool, progress func(ImagePullEvent)) error

type ImagePullEvent struct {
  Image  string `json:"image"`
  Status string `json:"status"` // queued|exists|pulling|done|error
  Detail string `json:"detail,omitempty"`
  Error  string `json:"error,omitempty"`
}
```

Note `PullImage(ctx, image) error` is currently coarse (Docker uses
`runInteractiveCommand`, `docker.go:280`) — it doesn't stream layer-level progress. For
v1, progress is **per-image** (`queued → pulling → done|error`), which is honest and
enough for the wizard. A later enhancement can add a streaming variant
(`PullImageStream`) that parses `docker pull --progress` / `podman pull` output; call
that out as a follow-up rather than blocking W4 on it.

### 7.3 `POST /api/v1/system/images/pull`

```jsonc
// request
{ "harnesses": ["claude","gemini"], "force": false }
// response (starts the job, returns a job id)
{ "jobId": "imgpull-7f3a", "images": ["ghcr.io/acme/scion-claude:...", "..."] }
```

Starts `PullImages` in a goroutine, publishing each `ImagePullEvent` to the event bus
(see §7.4). Returns immediately with a `jobId`. Workstation-mode guarded; admin only.
Concurrency: refuse a second pull while one is active (return `409` with the active
`jobId`) to keep state simple.

### 7.4 SSE progress: `GET /api/v1/system/images/events`

Model on the existing SSE handler `handleSSE` (`pkg/hub/web.go:957-1024`):

- Set headers exactly as `web.go:988`: `Content-Type: text/event-stream`,
  `Cache-Control: no-cache`, `Connection: keep-alive`, `X-Accel-Buffering: no`; obtain
  `http.Flusher` (`web.go:963`) and clear the write deadline with
  `http.NewResponseController` (`web.go:980`) so the long-lived stream survives the
  60s `WriteTimeout` (`web.go:1661`).
- Emit each event in the same wire format (`web.go:1014`):
  `event: image-progress\ndata: {<ImagePullEvent json>}\n\n` then `flusher.Flush()`.
- **Reuse the existing event bus** rather than a bespoke channel: publish
  `ImagePullEvent`s through `s.events` (the `EventPublisher` already wired for SSE,
  subjects like `system.images.<jobId>`) and let the wizard subscribe. Two options:
  1. **Reuse the existing `/events` SSE endpoint** with a new subject
     `system.images.>` — preferred, since the client already has `sse-client.ts` and
     `state.ts` subject plumbing (`web/src/client/state.ts:175`). The wizard adds the
     subject to its scope; no new endpoint at all. The dedicated
     `/system/images/events` route is the fallback if subject-scoping to a non-project
     stream proves awkward.
  2. Dedicated endpoint as written above.

  **Recommendation:** option 1 (subject on the shared `/events` stream) — least new
  code, reuses `SSEClient` reconnection/backoff (`sse-client.ts:148`). Document the
  subject contract: `system.images.<jobId>` carries `ImagePullEvent` payloads;
  terminal event has `status:"done"` or `status:"error"` for the whole batch.

- **Client:** the images step adds `system.images.>` to its SSE scope, renders a row per
  image with a status pill, and enables **Next** when all images reach `done`/`exists`
  or the user clicks **Skip** (images is non-blocking, §5.2).

### 7.5 No-registry / build-path handling (parent Q5)

When `ResolveImageRegistry("")` is empty, `images/pull` cannot pull. The handler returns
a structured `409`/`422` with code `image_registry_unset` and guidance text (mirroring
the existing `RequireImageRegistry`-style guidance referenced in
`.design/image-onboarding.md`). The wizard renders this as a non-dead-end: it shows the
build instructions (`image-build/`) and a **Skip for now** that advances to the
workspace step. `imagesPresent` simply stays incomplete in status; the user can pull
later. This satisfies "must gracefully handle 'you need to build images first'".

Re-running a pull is idempotent: `ImageExists` short-circuits already-present images to
`status:"exists"`.

### 7.6 Local image build (D3) — `POST /api/v1/system/images/build`

Default onboarding pulls prebuilt images from the pre-seeded `ghcr.io/homebrew-scion`
registry (D3), so most users never build. But for users on their own registry, an
air-gapped setup, or who simply want local images, the wizard offers a **"Build images
locally"** action — **enabled only after a runtime is confirmed present** (the runtime
step, §5.2, must be complete; the button is disabled otherwise).

- **Mechanism:** shell out to the existing build script
  `image-build/scripts/build-images.sh` with the resolved registry/tag and a target of
  `common` (scion-base + harnesses + hub; the script's default — see
  `.design/image-onboarding.md`). The chosen builder follows the active runtime
  (`local-docker` / `local-podman`).
- **Progress (D4):** builds are long-running, so unlike per-image pull pills, the build
  step **streams raw build log lines** into a **collapsible log panel**, over the same
  `/events` SSE stream under a `system.images.<jobId>` subject (event payloads carry a
  `line` field; terminal event carries `status:"done"|"error"`). One build job at a time
  (`409` if already running), mirroring the pull job (§7.3).
- **Endpoint:** `POST /system/images/build { "target": "common", "force": false }` →
  `{ "jobId": "imgbuild-…" }`. Workstation-mode guarded; admin only. New glue in
  `pkg/runtime/imagebuild.go` (or alongside `imagepull.go`) wrapping the script
  invocation and line-streaming.
- After a successful build, `ImageExists` reports the images present, so the wizard's
  `images` step turns green and `imagesPresent` flips complete in status.

> Building from source images also requires the build context (Dockerfiles under
> `image-build/`) to be present on disk. For a Homebrew-installed binary that ships
> without the build context, the build option is **hidden** unless the context is found
> (probe for `image-build/`); those users rely on the prebuilt pull path. The wizard
> surfaces which path is available rather than offering a build that can't run.

---

## 8. Security Considerations

1. **Workstation-mode hard fence (§4.2).** Every `system/*` handler returns `404` unless
   `s.requireWorkstation(...)` (the `Workstation bool` flag set from `!production`). These
   endpoints write `~/.scion`, mutate runtime config, and pull/build images — they must
   be invisible on any multi-user/production Hub. This is the same fence the parent doc
   demands for filesystem access (W5 shares the flag and helper).
2. **Loopback only.** Auto-login (§4.1) is acceptable solely because
   `applyWorkstationDefaults` binds `127.0.0.1`. Do not relax the bind in workstation
   mode.
3. **Admin role required.** Handlers require the admin DevUser (the default workstation
   identity is `role: admin`), enforced by the normal `UnifiedAuthMiddleware` chain — no
   bypass via `isUnauthenticatedEndpoint`.
4. **Input validation.** `runtime.type` must be in the known set;
   `harnesses` must be known harness ids; `imageRegistry` validated as a registry ref
   before persistence. Reject anything else with `400`.
5. **No arbitrary path input here.** This doc's endpoints take no host paths; the
   filesystem-touching workspace step is W5's responsibility under its own fence.

---

## 9. Files Touched / Created

**Backend (Go):**

| Action | Path | Notes |
|---|---|---|
| new | `pkg/hub/handlers_system.go` | all `system/*` handlers |
| new | `pkg/config/onboarding_status.go` | shared status logic (CLI + handler) |
| new | `pkg/runtime/imagepull.go` | `ImagesForHarnesses`, `PullImages`, `ImagePullEvent` |
| new | `pkg/runtime/checks.go` (or extend `doctor.go`) | returnable `GatherDiagnostics` |
| edit | `pkg/hub/server.go` | register routes (~`registerRoutes` 1990-2116); add `Workstation bool` to `ServerConfig` + `requireWorkstation()` helper (shared with W5) |
| edit | `cmd/doctor.go` | refactor `runDoctor` to print over `GatherDiagnostics` |
| edit | `cmd/server_daemon.go` | `printWorkstationQuickstart` prints `/onboarding` URL when `needsOnboarding` |
| edit | `pkg/hub/devauth.go` / `web.go` devAuthMiddleware | source identity from settings (W2 dependency) |

**Frontend (Lit/TS):**

| Action | Path | Notes |
|---|---|---|
| new | `web/src/components/pages/onboarding.ts` | `scion-page-onboarding` wizard + state machine |
| edit | `web/src/client/main.ts` | add route to `ROUTES`; add to `STANDALONE_ROUTES`; first-run redirect guard in `renderRoute` |
| reuse | `web/src/client/api.ts` | `apiFetch`, `extractApiError` |
| reuse | `web/src/client/sse-client.ts`, `state.ts` | subscribe `system.images.>` for image progress |

---

## 10. Resolved Decisions (2026-05-31)

1. **Identity persistence shape (W2) → small dedicated `PUT /system/identity`.**
   Identity (cosmetic: display name + email, default OS user — D1) lives on
   `V1AuthConfig`/`DevAuthConfig`; the wizard writes it via `PUT /system/identity` rather
   than overloading admin server-config.
2. **Pull progress → per-image (D4).** v1 ships per-image status pills; parsed
   layer-level progress (`PullImageStream`) is a later enhancement. Local builds stream
   raw log lines (§7.6).
3. **SSE channel → shared `/events` stream** with a `system.images.<jobId>` subject (no
   dedicated endpoint), reusing `SSEClient` reconnection/backoff.
4. **Status caching → `sessionStorage`.** Cache the "setup complete" result in
   `sessionStorage` and clear it on wizard completion; re-fetch `/system/status` on a
   fresh session. (Cross-tab invalidation is unnecessary for a single-user workstation.)
5. **Launch behavior → auto-open + always print, before backgrounding (D8).** The daemon
   prints the `/onboarding` URL prominently before detaching and best-effort auto-opens
   the browser (with `--no-browser` and TTY/SSH guards). See §3.2.

---

## 11. Build Order (W1/W4 slice)

> The **authoritative cross-workstream sequence is the parent doc
> [`workstation-onboarding.md`](./workstation-onboarding.md) §7.** The list below is the
> W1/W4 slice of that sequence, for local reference.

1. `Workstation bool` on `ServerConfig` + `requireWorkstation()` fence (parent §7 Phase 0
   — shared with W5; unblocks all handlers safely).
2. `GatherDiagnostics` refactor + `GET /system/check`; `GET/PUT /system/runtime` (thin,
   high-leverage wrappers).
3. `ComputeOnboardingStatus` + `GET /system/status`; `PUT /system/identity` (W2); wire
   CLI quickstart with auto-open before backgrounding (§3.2, D8).
4. `POST /system/init`.
5. Wizard shell (`onboarding.ts`) wiring steps 0–4,6–7 behind the first-run gate;
   `sessionStorage` status cache.
6. W4 image **pull** (`imagepull.go`, `POST /system/images/pull`, per-image SSE) + images
   step.
7. W4 image **local build** (`POST /system/images/build`, log-line SSE, gated on runtime
   present and build-context present — §7.6).
8. (W5, [`linked-groves-ui.md`](./linked-groves-ui.md)) workspace step's linked-grove
   directory-browser mode.
