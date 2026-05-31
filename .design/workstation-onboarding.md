# Workstation Mode as a First-Class Onboarding Experience

**Date:** 2026-05-30 (decisions folded in 2026-05-31)
**Status:** Proposal — Survey & Planning (decisions confirmed)
**Author:** Scion Agent (workstation-onboarding)
**Sub-designs:**
[`workstation-onboarding-wizard.md`](./workstation-onboarding-wizard.md) (W1+W4),
[`linked-groves-ui.md`](./linked-groves-ui.md) (W5)

---

## 1. Executive Summary

Today, **workstation mode** is something a user typically reaches *after* they have
already bootstrapped a machine on the command line (`scion init --machine`, built or
pulled images, configured a runtime). `scion server start` then lights up a co-located
Hub + Runtime Broker + Web UI on `127.0.0.1` with a generated dev token. It is, in
effect, a "phase two" convenience rather than an entry point.

This document proposes treating workstation mode as a **valid — if not primary —
first entry point** into Scion: a user installs (e.g. via the Homebrew tap), runs
`scion server start`, and is met with a browser-based **onboarding experience** that
walks them through the setup that currently only exists as disconnected CLI steps:

- Choosing which **harnesses** they want (Claude Code, Gemini, Codex, OpenCode).
- Initializing the **global directory** (`~/.scion`).
- Verifying the **container runtime** is installed and reachable.
- **Pulling images** (default registry pre-seeded by the Homebrew install) — or
  **building them locally** once a runtime is confirmed.
- Setting a **username / identity** instead of the hardcoded `dev@localhost`.
- Adding **workspaces**, including **linked groves** — local directories that live
  *outside* the Hub's managed path space — directly from the browser via a
  server-side directory browser.

This is a survey of the current state plus a plan for the workstreams. Two of the
workstreams (the onboarding wizard and linked groves) have their own sub-design docs,
linked above. **This doc is the single source of truth for the primary, end-to-end
sequence of work (§7); the sub-docs carry the detailed designs.**

---

## 1a. Confirmed Decisions (2026-05-31)

Decisions from the design review with the product owner. These supersede any
contradicting text in earlier drafts of the sub-docs; the sub-docs have been updated to
match.

| # | Decision | Effect |
|---|---|---|
| D1 | **Identity is cosmetic** for local single-user. Keep the stable `DevUser` UUID for DB integrity; allow a settable display name + email, defaulting to the **OS username** instead of `dev@localhost`. | W2 stays small. No real user-record creation. |
| D2 | **Do not rename the dev token.** Keep "dev token", the `scion_dev_` format, `~/.scion/dev-token`, and `SCION_DEV_TOKEN` exactly as-is internally. **Only relabel docs/UI** to "developer token" for consistency. | W3 shrinks to a copy/label pass; no new env var, no `local auth mode`. |
| D3 | **Images: prefer prebuilt, add local build.** The Homebrew install pre-seeds `ghcr.io/homebrew-scion` as the registry, so the pull path is the default and "just works". **Add a local image-build option** in the wizard, available **after a runtime is confirmed present**. | W4 = pull (default) + build (fallback). |
| D4 | **Per-image pull progress** (queued → pulling → done/exists/error). Layer-level streaming is a later enhancement. Local build streams raw build log lines into a collapsible panel. | W4 progress fidelity. |
| D5 | **Linked-grove picker = server-side directory browser** (a custom web folder tree with a **"New folder"** button), **strictly disabled (404) when serving in production.** Not a native OS dialog (not reachable from a served web page). | W5 elevates the browser to v1. |
| D6 | **Hard-fail** when a user tries to link a directory that is inside the hub-managed path space (`~/.scion/projects/`, legacy `groves/`). | W5 validation rule. |
| D7 | **Two-step linked-grove create** (create project, then add the co-located broker as a provider with the local path), mirroring `scion hub link`. Recoverable on failure; revisit an atomic create-handler only if it proves flaky. | W5 submit flow. |
| D8 | **`scion server start` auto-opens the browser** to `/onboarding` when the machine is un-onboarded, **and always prints the URL prominently** — printed **before** the daemon backgrounds itself. | W1 launch behavior. |

Minor implementation defaults (also confirmed): prod fence via a `Workstation` flag on
`hub.ServerConfig` (all `/system/*` + fs endpoints 404 in prod, plus loopback assertion
+ normal auth); directory browser opens at `$HOME` by default; image progress travels on
the existing `/events` SSE stream under a `system.images.<jobId>` subject; first-run
detection is a server-computed status struct cached client-side in `sessionStorage`
(cleared on wizard completion); identity is written via a small `PUT /system/identity`;
exactly one co-located broker is assumed per workstation.

---

## 2. Current State (Survey)

### 2.1 How workstation mode works today

`scion server start` is the entry point. In the absence of `--production`, it applies
workstation defaults and prints a quickstart.

- **Defaults** — `applyWorkstationDefaults()` at `cmd/server_config.go:25-44` turns on
  Hub, Runtime Broker, Web, dev-auth, and auto-provide, and binds to `127.0.0.1`.
  Explicit flags always win (`cmd.Flags().Changed(...)`).
- **Command family** — `cmd/server.go:24-279` defines `server start | stop | restart |
  status | install`. The start path is `cmd/server_daemon.go:33-161`
  (`runServerStartOrDaemon`); the foreground runner is `cmd/server_foreground.go:56-424`
  (`runServerStart`). Default behavior is a backgrounded daemon; `--foreground` runs in
  the terminal.
- **Quickstart** — `printWorkstationQuickstart()` at `cmd/server_daemon.go:361-384`
  prints the Web UI URL and `export SCION_DEV_TOKEN=...`. This is the *entire* current
  "onboarding": a URL and a token, with no guided setup behind it. **(D8 extends this to
  print the `/onboarding` URL and auto-open the browser when un-onboarded.)**
- **Mode detection / persistence** — `pkg/config/hub_config.go` (`Mode` field,
  `LoadServerMode`); `mode: workstation` can be set persistently in
  `~/.scion/settings.yaml`.

**Key takeaway:** workstation mode is an *opinionated preset* that co-locates three
services. It assumes the machine is already initialized. There is no first-run gate
that detects an un-initialized machine and offers to set it up.

### 2.2 The web UI

- **Stack** — Lit + Vite + Shoelace + xterm.js + CodeMirror, server-rendered shell with
  client hydration. Web server: `pkg/hub/web.go` (`WebServer`, default port 8080,
  `/healthz`, `/assets/*`, SPA catch-all, `/events` SSE). API mounted at `/api/v1/*`
  via `MountHubAPI` (`pkg/hub/web.go:518-527`).
- **Routes** — `web/src/client/main.ts:127-158`. Pages include `/`, `/login`,
  `/projects`, `/projects/new`, `/agents`, `/agents/new`, `/brokers`, `/invite`,
  `/github-app/installed`, and an `/admin/*` family.
- **Existing onboarding-ish flows** — GitHub App setup
  (`web/src/components/pages/github-app-setup.ts`), invite redemption
  (`web/src/components/pages/invite.ts`), the admin server-config editor
  (`web/src/components/pages/admin-server-config.ts`), and an empty-state-friendly home
  dashboard (`web/src/components/pages/home.ts`).

**Key takeaway:** there is **no first-run onboarding wizard**. Building blocks exist but
are not stitched into a guided first-run flow. (Designed in
[`workstation-onboarding-wizard.md`](./workstation-onboarding-wizard.md).)

### 2.3 Machine init, runtime, and images

- **`scion init --machine`** — `cmd/init.go:21-48` → `cmd/project.go:59-116` →
  `config.InitMachine()` at `pkg/config/init.go:548-620`. Creates `~/.scion`, detects
  the runtime, writes `settings.yaml`, seeds harness-configs for all four built-ins,
  seeds the default template, pre-generates a stable broker ID, and (Homebrew install)
  pre-seeds `image_registry: ghcr.io/homebrew-scion`.
- **Runtime detection** — `pkg/config/runtime_detect.go:52-75` (`DetectLocalRuntime`,
  preference podman → container[macOS] → docker). Factory: `pkg/runtime/factory.go:31-137`.
- **Image pulling** — today a *shell script*, `image-build/scripts/pull-containers.sh`.
  Prebuilt public images are published at `ghcr.io/homebrew-scion/` (see the Homebrew
  distribution design). W4 adds a Go-native, harness-aware pull plus a local build path.
- **Doctor** — `cmd/doctor.go:30-261` already runs git/tmux/runtime checks with
  structured pass/warn/fail results (`pkg/runtime/doctor.go:15-41`) — the data source
  for the wizard's system-check step.

### 2.4 Auth, dev token, and username

- **Dev token** — generated/resolved in `pkg/apiclient/devauth.go:27-157`
  (`scion_dev_` prefix, `~/.scion/dev-token`, env `SCION_DEV_TOKEN`). Server side:
  `initDevAuth()` at `cmd/server_foreground.go:678-700`; middleware at
  `pkg/hub/devauth.go:53-148`. **(D2: all of this stays exactly as-is.)**
- **Unified auth** — `pkg/hub/auth.go:60-248` (`UnifiedAuthMiddleware`).
- **Hardcoded dev identity** — `pkg/hub/devauth.go:26-49`: `DevUser` is a fixed pseudo
  user (UUID `be67fbc9-…`, `dev@localhost`, `Development User`, role `admin`).
  **(D1: keep the UUID; make display name/email configurable, defaulting to the OS user.)**
- **Config surface** — `DevAuthConfig` (`pkg/config/hub_config.go:142-158`) and
  `V1AuthConfig` (`pkg/config/settings_v1.go:383-388`) carry token fields but no
  identity fields today.

### 2.5 Groves, the managed path space, and linked groves

(A **grove → project** rename is in flight — `.design/grove-to-project-rename.md`.
"grove" and "project" are the same concept; build against "project".)

- **Project types** — `pkg/store/models.go:186-246` computes `ProjectType`:
  `hub-native` vs `linked`.
- **Managed path space** — `hubNativeProjectPath()` at `pkg/hub/handlers.go:3736-3752`
  places hub-native/shared-workspace projects under `~/.scion/projects/<slug>/`
  (legacy `~/.scion/groves/<slug>/`). This is "the Hub's managed section of the
  filesystem".
- **Linked projects already exist in the model** — `ProjectProvider`
  (`pkg/store/models.go:337-379`) carries `LocalPath`/`BrokerID`/`LinkedBy`/`LinkedAt`;
  link API `POST /api/v1/projects/{projectId}/providers`
  (`pkg/hub/handlers.go:7980-8043`); WebDAV honors a co-located broker's `LocalPath`
  (`pkg/hub/project_webdav.go:136-190`). CLI precedent: `scion hub link`
  (`cmd/hub.go:215-235`, `runHubLink`).
- **The browser gap** — `web/src/components/pages/project-create.ts` only supports
  git-backed / hub-native projects. There is **no UI to register an arbitrary local
  directory as a linked grove.** (Closed by
  [`linked-groves-ui.md`](./linked-groves-ui.md).)

---

## 3. Goals and Non-Goals

### Goals
1. `scion server start` on a fresh machine leads to a **guided, browser-based
   onboarding** that can fully bootstrap Scion (and auto-opens — D8).
2. Onboarding can: pick harnesses, init `~/.scion`, verify runtime, **pull prebuilt
   images or build them locally** (D3), set identity, and add at least one workspace.
3. **Cosmetic configurable identity** for local mode (D1).
4. **Relabel** dev-token references to "developer token" in docs/UI only (D2).
5. **Add linked groves from the browser** via a server-side directory browser (D5),
   hard-fenced to workstation mode.

### Non-Goals
- Multi-user / production auth changes beyond the cosmetic identity (D1).
- Replacing the existing CLI init paths (onboarding *reuses* their logic).
- Remote-broker linked-grove UX (initial focus is the co-located workstation broker).
- The grove→project rename itself (tracked separately).
- Publishing/operating the prebuilt image pipeline (owned by the Homebrew distribution
  work; onboarding only *consumes* the pre-seeded registry).

---

## 4. Proposed Onboarding Experience

A first-run flow served by the workstation Web UI, gated on detecting an
un-/under-initialized machine. A wizard with skippable, resumable steps:

1. **Welcome / identity** — set display name + username (D1; defaults to OS user).
2. **System check** — run the existing `doctor` checks; render pass/warn/fail. Block
   only on hard failures (no runtime).
3. **Runtime** — confirm or switch the detected runtime; persist to `settings.yaml`.
4. **Harness selection** — choose harnesses; seed the selected harness-configs.
5. **Images** — pull from the pre-seeded registry (D3), with per-image progress (D4);
   **or** build locally now that a runtime is confirmed. Skippable.
6. **First workspace** — create a hub-native project, link a git repo, *or* add a
   linked grove by browsing to / creating a local directory (D5).
7. **Done** — land on the dashboard, ready to start an agent.

State is resumable from the server-computed status struct (see the wizard doc §3/§5.4).

---

## 5. Workstreams

Detailed designs live in the sub-docs; this section is the index. The **build order
that interleaves these is §7.**

### W1 — Onboarding wizard (web UI + supporting API)
Detailed in [`workstation-onboarding-wizard.md`](./workstation-onboarding-wizard.md).
New `/onboarding` route + Lit page; first-run detection + redirect/auto-open (D8);
`/api/v1/system/*` endpoints (`status`, `check`, `runtime`, `init`, `images/*`) that
wrap existing logic (`doctor`, `DetectLocalRuntime`, `config.InitMachine`).

### W2 — Cosmetic identity (D1)
Add `username`/`displayName`/`email` to `DevAuthConfig`
(`pkg/config/hub_config.go:142-158`) and `V1AuthConfig`
(`pkg/config/settings_v1.go:383-388`); thread into `DevUser`
(`pkg/hub/devauth.go:26-49`) **keeping the stable UUID**; default to the OS user
(`os/user`) when unset. Written via `PUT /system/identity` from the wizard.

### W3 — "Developer token" relabel (D2)
Docs/UI copy only: standardize on "developer token" in CLI help, quickstart output
(`cmd/server_daemon.go:361-384`), web copy, and docs. **No code/format/env-var change.**
Coordinate with `cli-modes.md`.

### W4 — Harness-aware images: pull + local build (D3, D4)
Detailed in the wizard doc §7. Go-native pull via the runtime interface
(`pkg/runtime/interface.go` `ImageExists`/`PullImage`), defaulting to the pre-seeded
`ghcr.io/homebrew-scion` registry, with **per-image** progress on the `/events` SSE
stream (D4). Add a **local build** option (shells out to the build scripts) enabled only
after a runtime is confirmed, streaming build logs into a collapsible panel.

### W5 — Linked groves via a directory browser (D5, D6, D7)
Detailed in [`linked-groves-ui.md`](./linked-groves-ui.md). A workstation-only
(404-in-prod) **directory browser** with a **"New folder"** button, backed by fenced
`fs/list` + `fs/mkdir` + `fs/validate-path` endpoints; **hard-fail** on managed-path
overlap (D6); **two-step** create (project + provider) (D7).

---

## 6. Resolved Decisions & Remaining Risks

The original open questions are now resolved by §1a:

| Original question | Resolution |
|---|---|
| Q1 Filesystem access fencing | `Workstation` flag on `ServerConfig`; all `system/*`/fs endpoints 404 in prod + loopback + auth (D5; wizard §4.2, linked §4). |
| Q2 First-run detection signal | Server-computed status struct, `sessionStorage`-cached (wizard §3). |
| Q3 Bootstrap-auth window | Dev-auth auto-login session on loopback (wizard §4). |
| Q4 Rename naming | Build against "project" per the in-flight rename. |
| Q5 Image / no-registry UX | Pre-seeded `ghcr.io/homebrew-scion` is the default; local build added; step skippable (D3). |
| Q6 Resumability/idempotency | Idempotent endpoints + resume from status struct (wizard §5.4). |
| Q7 Username scope | Cosmetic only, stable UUID (D1). |

**Remaining risks to watch during implementation:**
1. **Directory-browser blast radius.** `fs/list` + `fs/mkdir` read/create on the host
   filesystem. The 404-in-prod fence, loopback assertion, path-safety helpers
   (symlink-expand, managed-root checks), and auth are all mandatory — not optional.
2. **Build-path UX.** Local builds are long and can fail for environment reasons; the
   wizard must stream logs and fail gracefully without dead-ending onboarding.
3. **Grove→project rename churn.** New UI/API must follow whatever naming is canonical
   at implementation time.
4. **Two-step linked create orphans** (D7): a failed provider-add leaves a project with
   no local path; ensure re-submit recovers and surface the state clearly.

---

## 7. Primary Sequence of Work (single source of truth)

One ordered, end-to-end plan spanning all workstreams. Each item notes its sub-doc.
Items within a phase can parallelize; phases are ordered by dependency.

**Phase 0 — Foundations (unblock everything safely)**
1. Add `Workstation bool` to `hub.ServerConfig` (set from `!production` at
   `cmd/server_foreground.go:774`), store `s.workstation`, add `requireWorkstation`
   (404) + loopback assertion helpers. *(W1/W5 — wizard §4.2, linked §4)*
2. Add `GetEmbeddedBrokerID()` accessor on the server. *(W5 — linked §6)*

**Phase 1 — Identity & labels (small, self-contained)**
3. W2: identity fields on `DevAuthConfig`/`V1AuthConfig`; thread into `DevUser`
   (keep UUID); default to OS user. *(D1)*
4. W3: "developer token" relabel across CLI help, quickstart, web copy, docs. *(D2)*

**Phase 2 — System API (thin wrappers over existing logic)**
5. Refactor `cmd/doctor.go` into a returnable `GatherDiagnostics`; add
   `GET /system/check`. *(W1 — wizard §6.2)*
6. `GET`/`PUT /system/runtime` (detect + persist). *(W1 — wizard §6.3)*
7. `ComputeOnboardingStatus` + `GET /system/status`; `PUT /system/identity`. *(W1+W2)*
8. `POST /system/init` (wraps `config.InitMachine` with chosen harnesses). *(W1 — §6.4)*

**Phase 3 — Wizard shell**
9. `web/src/components/pages/onboarding.ts` (`scion-page-onboarding`), route +
   standalone shell, state machine, steps 0–4 & 6–7 behind the first-run gate;
   `sessionStorage` "setup complete" cache. *(W1 — wizard §5)*
10. Daemon launch: print `/onboarding` URL **before backgrounding** and **auto-open the
    browser** when un-onboarded (with `--no-browser` opt-out; skip when not a TTY/over
    SSH). *(D8 — wizard §3.2)*

**Phase 4 — Images (pull + build)**
11. `pkg/runtime/imagepull.go`: `ImagesForHarnesses`, `PullImages` (per-image events),
    reusing `ImageExists`/`PullImage`. *(W4 — wizard §7.2)*
12. `POST /system/images/pull` + progress on `/events` (`system.images.<jobId>`); wizard
    images step renders per-image pills. *(W4 — D4, wizard §7.3–7.4)*
13. Local build option (post-runtime): `POST /system/images/build` shelling to the build
    scripts, streaming log lines into a collapsible panel. *(W4 — D3)*

**Phase 5 — Linked groves (directory browser)**
14. `pkg/hub/fs_safety.go`: `resolveAndClassifyPath` (resolve, symlink-expand,
    managed-root + git + already-linked classification), reusing `hubNativeProjectPath`.
    *(W5 — linked §3.3)*
15. Fenced endpoints: `POST /system/fs/validate-path`, `GET /system/fs/list`,
    `POST /system/fs/mkdir` (all 404 in prod). *(W5 — linked §3, §4)*
16. `project-create.ts`: `'linked'` mode with a directory-browser modal + "New folder"
    button populating the path; **hard-fail** on managed overlap (D6); **two-step**
    submit (create project, then add embedded broker as provider with resolved path)
    (D7). *(W5 — linked §5)*
17. Wire the workspace step (wizard step 6) to the linked-grove flow.

**Phase 6 — Polish & docs**
18. Tests per sub-doc (fencing 404s, path classification, two-step create →
    `ProjectType == "linked"`, wizard step gating).
19. Update user docs / README (remove "no prebuilt images" note; document onboarding,
    "developer token" label, linked groves).

---

## 8. Sub-Design Docs

- [`workstation-onboarding-wizard.md`](./workstation-onboarding-wizard.md) — W1 + W4:
  wizard UX/state machine, first-run detection, bootstrap-auth window, the `system/*`
  API, and image pull/build.
- [`linked-groves-ui.md`](./linked-groves-ui.md) — W5: directory-browser UX, the fenced
  `fs/*` endpoints, security model, and the two-step linked-create flow.

W2 and W3 are small enough to land as implementation PRs without separate design docs.
