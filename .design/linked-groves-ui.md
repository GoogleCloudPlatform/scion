# Linked Groves from the Browser

**Date:** 2026-05-30 (decisions folded in 2026-05-31)
**Status:** Sub-design — detailed plan (W5 of `workstation-onboarding.md`)
**Author:** Scion Agent (workstation-improvements)
**Parent:** [`.design/workstation-onboarding.md`](./workstation-onboarding.md) §2.5, §5 (W5), §1a (D5/D6/D7)

> **Confirmed decisions (2026-05-31):** **D5** — ship the **server-side directory
> browser** (a custom web folder tree + a **"New folder"** button), **strictly 404'd
> when serving in production**; *not* a native OS dialog. **D6** — **hard-fail** on
> managed-path overlap. **D7** — **two-step** create (project, then provider). These
> supersede the original "Option A first" recommendation below; §2 has been updated.

---

## 1. Scope

Surface **linked groves** — local directories that live *outside* the Hub's
managed path space (`~/.scion/projects/<slug>/`) — as a first-class create flow in
the workstation Web UI.

Everything below the UI already exists:

- **Data model** — `ProjectProvider` carries `LocalPath`, `BrokerID`, `LinkedBy`,
  `LinkedAt` (`pkg/store/models.go:337-379`); `ProjectType` returns `linked` when a
  provider supplies a `LocalPath` (`pkg/store/models.go:186-246`).
- **Link API** — `POST /api/v1/projects/{projectId}/providers` →
  `addProjectProvider` (`pkg/hub/handlers.go:7980-8043`), request shape
  `AddProviderRequest{ BrokerID, LocalPath }` (`pkg/hub/handlers.go:3211-3215`).
- **WebDAV/file resolution** honors a co-located broker's `LocalPath` directly
  (`pkg/hub/project_webdav.go:136-190`).
- **CLI precedent** — `scion hub link` ensures the project exists, then adds the
  local broker as a provider with `LocalPath: resolvedPath`
  (`cmd/hub.go:2376-2388`, full command `runHubLink` at `cmd/hub.go:2172`).

The gaps this doc closes:

1. A **third mode** (`'linked'`) in `web/src/components/pages/project-create.ts`
   (today only `'git' | 'hub'`, line 29), with a **directory-browser modal** and a
   **"New folder"** button (D5).
2. A small family of **filesystem API endpoints** the co-located broker exposes:
   `fs/list` (browse), `fs/mkdir` (create new dir), and `fs/validate-path` (confirm a
   candidate directory is real, readable, and not already managed) — before it is linked.
3. **Security fencing** so those endpoints (which read/create on the host filesystem on
   the user's behalf) are reachable **only** in workstation mode (404 in prod), on a
   loopback bind, behind auth.
4. A way for the UI to **discover the co-located broker ID** to link against.

Non-goals (inherited from parent §3): remote-broker linked-grove UX (focus is the
co-located workstation broker only), and the grove→project rename.

---

## 2. UX decision: path-entry vs. directory-browser

Two candidate interactions, both noted as open in parent §5 (W5) / §2.5.

### Option A — Free-text path entry + validate (retained as the underlying field; see Decision)

A single text input ("Local directory path", e.g. `/home/alice/code/myrepo`) with a
debounced **Validate** call to the new endpoint (mirroring the existing debounced
`checkExistingProjects` pattern at `project-create.ts:307-349`). The result renders
inline as a pass/warn/fail line (reuse `status-badge.js`, already imported at
`project-create.ts:27`).

- **Pros:** small surface area; one new endpoint; no recursive filesystem exposure;
  matches `scion hub link`'s "you are already standing in the directory" mental
  model; trivially fenceable.
- **Cons:** user must know/paste the absolute path; no discoverability.

### Option B — Server-side directory browser

A modal tree/list backed by a `GET .../fs/list?path=` endpoint that enumerates
directory entries the broker can read, letting the user click down the tree.

- **Pros:** friendlier; discoverable; no typing of long paths.
- **Cons:** a second, **more dangerous** endpoint — it lists arbitrary host
  directory contents over HTTP. Larger fencing burden (parent §6 Q1), more UI, path
  traversal/symlink surface, and a "home root" decision (where does browsing start?).

### Decision (updated 2026-05-31 — D5)

**Ship Option B: the server-side directory browser**, with a **"New folder"** button
for creating a destination directory inline. The browser is a **custom web component**
(a folder tree the hub serves over the fenced `fs/list` endpoint) — **not a native OS
dialog**, which a served web page cannot invoke to obtain a server-usable absolute path.
The browser is **strictly disabled (404) when serving in production** (§4).

The path-entry input from Option A is **retained as the underlying source of truth**:
the directory browser and "New folder" action simply populate `localPath`, and a
debounced `validate-path` call still runs against the resolved selection before submit.
So Option A's validation endpoint and field remain; Option B adds two siblings
(`fs/list`, `fs/mkdir`) under the same workstation fence and path-safety helpers.

This doc specifies all three endpoints (`fs/validate-path`, `fs/list`, `fs/mkdir`) and
the browser UI. Why this is acceptable despite the larger surface: every endpoint is
404 in production, loopback-asserted, auth-gated, and shares one path-safety helper
(symlink-expand + managed-root checks), keeping the attack surface bounded (§4).

---

## 3. Path-validation API endpoint

### 3.1 Surface

```
POST /api/v1/system/fs/validate-path
Request:  { "path": "/abs/or/~-relative/path" }
Response: {
  "valid":      true,
  "resolved":   "/home/alice/code/myrepo",   // absolute, symlink-expanded
  "exists":     true,
  "isDir":      true,
  "readable":   true,
  "isGitRepo":  true,                          // contains .git
  "alreadyManaged":  false,                     // under ~/.scion/projects (or legacy groves)
  "alreadyLinked":   false,                     // a provider already points here
  "warnings":   ["Path is a git repository; agents will operate on the working tree."],
  "error":      ""                              // human-readable when valid=false
}
```

Aligns with the parent's proposed `/api/v1/system/*` namespace (W1, parent §5:214-218)
so onboarding's system endpoints cluster together. `fs/` sub-namespace reserves room
for the Option B `fs/list` sibling.

### 3.2 Checks performed (in order)

1. **Resolve & normalize** — expand `~`, make absolute, `filepath.Clean`, then
   `filepath.EvalSymlinks` to defeat symlink games. Reject empty/`.`-only input.
2. **Exists + is directory** — `os.Stat`; populate `exists`, `isDir`.
3. **Readable** — attempt to open the directory for listing; populate `readable`.
4. **Not already managed** — reject (or warn) if `resolved` is within the managed
   path space. Compute the managed root the same way the hub does:
   `hubNativeProjectPath()` at `pkg/hub/handlers.go:3736-3752` places projects under
   `~/.scion/projects/<slug>/` (legacy `~/.scion/groves/<slug>/`). Linking a managed
   directory back in as "linked" is nonsensical and is a hard `valid=false`.
5. **Not already linked** — scan existing providers for a `LocalPath` whose resolved
   form equals `resolved`; surface as `alreadyLinked` (warn, not hard fail — the user
   may be re-linking).
6. **Git detection** — presence of a `.git` entry → `isGitRepo` (informational; the
   parent notes "optionally is/isn't a git repo", §5:250).

`valid` is `true` only when `exists && isDir && readable && !alreadyManaged`.

### 3.3 Where the code lives

- **Handler** — new method `(s *Server) validateLocalPath(w, r)` in a new file
  `pkg/hub/system_handlers.go` (groups the W1 `/system/*` handlers — check, runtime,
  init, images — alongside this one). Request/response structs (`ValidatePathRequest`,
  `ValidatePathResponse`) beside it.
- **Path-safety helper** — factor the resolve+managed-root logic into
  `pkg/hub/fs_safety.go` (`resolveAndClassifyPath(path string) (...)`) so Option B's
  `fs/list` can reuse it. The managed-root computation should call/share the existing
  `hubNativeProjectPath` logic rather than re-deriving `~/.scion/projects`.
- **Provider scan** — reuse `s.store.GetProjectProviders` (already used at
  `pkg/hub/handlers.go:5373`, `:7969`) across the user's projects, or add a
  store helper `GetProviderByLocalPath` if a full scan proves too coarse. Start with
  the scan; it's a workstation, the project count is small.

### 3.4 Routing

The hub dispatches by path-prefix string matching (see the nested-path style at
`pkg/hub/handlers.go:4410-4416` for `/providers`). Add a `system/fs/` branch in the
top-level API router that, after the workstation guard (§4), routes
`validate-path` (POST), `list` (GET), and `mkdir` (POST) to their handlers. Mount under
the same `MountHubAPI` tree (`pkg/hub/web.go:518-527`) as everything else.

### 3.5 `GET /api/v1/system/fs/list` (directory browser — D5)

Backs the folder tree. Lists the **immediate** entries of one directory (no recursion).

```
GET /api/v1/system/fs/list?path=/home/alice
Response: {
  "path":    "/home/alice",          // resolved, symlink-expanded
  "parent":  "/home",                // null at filesystem root
  "entries": [
    { "name": "code", "isDir": true,  "isGitRepo": true,  "readable": true },
    { "name": "Documents", "isDir": true, "isGitRepo": false, "readable": true }
  ]
}
```

- **Default root:** when `path` is empty, start at `$HOME` (confirmed default). The UI
  shows a breadcrumb and can navigate up via `parent` (never above what the helper
  permits).
- **Dirs only by default** — the picker only needs directories; files may be omitted or
  flagged `isDir:false` and rendered disabled. `isGitRepo` annotates folders for the
  user.
- Reuses `resolveAndClassifyPath` / the shared path-safety helper (§3.3) for each path;
  unreadable entries are returned with `readable:false` rather than omitted, so the user
  sees why they can't descend.

### 3.6 `POST /api/v1/system/fs/mkdir` ("New folder" — D5)

Creates a single new directory under a parent the user is browsing.

```
POST /api/v1/system/fs/mkdir
Request:  { "parent": "/home/alice/code", "name": "my-new-project" }
Response: { "created": true, "path": "/home/alice/code/my-new-project" }
```

- Validate `name` (no path separators, no `.`/`..`, length-bounded) and that `parent`
  resolves, exists, is a dir, and is **not** inside the managed path space (§3.2 step 4
  — D6). `os.Mkdir` (not `MkdirAll`) so a typo can't create a deep tree; `409` if it
  already exists. On success the UI selects the new directory and runs `validate-path`.

---

## 4. Security fencing to workstation mode only

This is parent §6 Q1 — the must-solve. The endpoint reads the host filesystem on the
user's behalf and must **never** be reachable on a multi-user / remote Hub.

### 4.1 The signal: an explicit `Workstation` flag on `ServerConfig`

There is currently **no mode field on `hub.ServerConfig`** (it has `AdminMode`,
`UserAccessMode`, `DevAuthToken`, etc., but not server operating mode — verified in
`pkg/hub/server.go` ServerConfig struct). The operating mode lives one layer up in
`config.GlobalConfig.Mode` (`pkg/config/hub_config.go:190-192`, read standalone via
`LoadServerMode`, `:610`) and is consulted in `cmd/server_foreground.go:524`
(`cfg.Mode == "production"`).

**Add a field** `Workstation bool` to `hub.ServerConfig` and set it where the config
is assembled at `cmd/server_foreground.go:774` (the `hub.ServerConfig{...}` literal),
from `!productionMode` (the same boolean already computed in `loadAndReconcileConfig`,
`cmd/server_foreground.go:522-542`). Do **not** infer mode from `DevAuthToken != ""`
or the bind host — those are separately overridable and would couple unrelated
concerns.

Store it on the server (e.g. `s.workstation bool`) and expose a guard helper:

```go
// pkg/hub/fs_safety.go
func (s *Server) requireWorkstation(w http.ResponseWriter, r *http.Request) bool {
    if !s.workstation {
        NotFound(w) // 404, not 403 — do not advertise the route's existence off-workstation
        return false
    }
    return true
}
```

### 4.2 Defense in depth

1. **Mode gate (primary)** — `requireWorkstation` at the top of `validateLocalPath`
   (and any future `fs/list`). Return **404** so the route is invisible in production.
2. **Loopback assertion (secondary)** — additionally require the request to be local
   (remote addr is loopback / matches the configured `127.0.0.1` bind). Workstation
   already binds `127.0.0.1` by default (`applyWorkstationDefaults`,
   `cmd/server_config.go:42-44`; reasserted `cmd/server_foreground.go:534-536`), but
   asserting in-handler protects against a misconfigured non-loopback workstation
   bind. Reuse `getClientIP(r)` (already used at `pkg/hub/handlers.go:8038`).
3. **Auth still required** — the endpoint sits behind the normal
   `UnifiedAuthMiddleware` (`pkg/hub/auth.go:60-248`); the developer (dev) token is
   required like every other `/api/v1` call. The guard is *in addition to* auth.
4. **Embedded-broker-only linking** — the create flow (§5) links against the
   **co-located/embedded broker only**. The hub already tracks it via
   `embeddedBrokerID` + `isEmbeddedBroker()` (`pkg/hub/server.go:1062-1074`, set at
   `cmd/server_foreground.go:1116`). A remote broker's filesystem is never read by the
   hub, so validation is meaningless there and is refused.
5. **Directory listing is in scope (D5) — and is the riskiest endpoint.** `fs/list`
   enumerates host directory contents and `fs/mkdir` creates directories, so the fence
   matters more, not less. Mitigations beyond the mode gate: list is **non-recursive**
   (one level per call); `mkdir` uses `os.Mkdir` (one level, no `MkdirAll`) with strict
   `name` validation (no separators/`.`/`..`); both run every path through the shared
   `resolveAndClassifyPath` helper (symlink-expand + managed-root rejection); and both
   inherit the 404-in-prod + loopback + auth gates above. The bound on blast radius is
   the fence + the path-safety helper, which `fs/list`/`fs/mkdir`/`validate-path` all
   share — there is exactly one place to get path safety right.

### 4.3 Why a model field rather than reading `LoadServerMode()` per request

`LoadServerMode` re-reads `settings.yaml` from disk; the authoritative resolved mode
(after flag reconciliation, including `--production` overrides) lives in the
already-computed `productionMode` boolean at server start. Threading it into
`ServerConfig` once is cheaper and matches how `AdminMode` is already plumbed.

---

## 5. Changes to `project-create.ts`

File: `web/src/components/pages/project-create.ts`.

### 5.1 Mode type and selector

- Extend the mode union (line 29):
  `type ProjectMode = 'git' | 'hub' | 'linked';`
- Add an `<sl-option value="linked">` to the Workspace Type select (lines 466-480)
  labeled e.g. **"Local Directory (linked)"**, with hint copy describing that the
  directory stays where it is and is operated on in place.
- `onModeChange` (line 303) needs no change — it already casts to `ProjectMode`.

### 5.2 New state + validation handler

Add `@state()` fields mirroring the existing git block:

```ts
@state() private localPath = '';
@state() private pathValidation: ValidatePathResponse | null = null;
@state() private validatingPath = false;
private pathCheckTimer: ReturnType<typeof setTimeout> | null = null;
```

Add `onLocalPathInput` that debounces a `POST /api/v1/system/fs/validate-path` call
— structurally identical to `onGitRemoteInput` + `checkExistingProjects`
(lines 307-349). Render the result inline using `status-badge.js` plus the existing
`.info-banner` / `.error-banner` styles (already defined, lines 196-230). Surface
`isGitRepo`/`alreadyLinked` as warnings, `valid=false` as a blocking error.

### 5.3 Conditional form fields

In `render()`, add a `${this.mode === 'linked' ? html\`...\` : nothing}` block
(parallel to the `this.mode === 'git'` block at lines 482-568) containing:

- the **Local directory path** input bound to `localPath` / `onLocalPathInput`
  (retained as the source of truth — D5), plus a **"Browse…"** button that opens the
  directory-browser modal;
- the **directory-browser modal** (new component, e.g.
  `web/src/components/dialogs/directory-browser.ts`, `scion-directory-browser`): a
  breadcrumb + folder list backed by `GET /system/fs/list`, a **"New folder"** button
  backed by `POST /system/fs/mkdir`, and a **Select** action that writes the chosen
  resolved path back into `localPath` and closes the modal (which triggers
  `onLocalPathInput`'s `validate-path` call);
- the inline validation result;
- (no git-remote, branch, workspace-mode, or github-token fields — hide them).

Name/slug/visibility fields below (lines 570-622) stay shared. The git-only Default
Branch block (lines 592-607) already keys off `this.mode === 'git'`, so it stays
hidden for `linked` with no change.

### 5.4 Submit path (two-step, mirroring `scion hub link`)

`handleSubmit` (line 356) currently builds one `POST /api/v1/projects` body. For
`linked`, follow the CLI's link semantics (`cmd/hub.go:2376-2388`): **create the
project, then add the embedded broker as a provider carrying `LocalPath`.**

1. **Guard:** require `name` (existing check, line 357) and a `pathValidation?.valid`
   result; otherwise set `this.error` and return.
2. **Discover the broker:** the UI needs the embedded broker's ID (§6). Fetch it
   once (see §6) and keep it in state.
3. **Create the project** via the existing `POST /api/v1/projects` call (lines
   405-410). Body is the minimal `{ name, slug?, visibility }` — **no `gitRemote`,
   no `workspaceMode`** (this makes it a managed/hub-shell project that the provider
   then redirects to the local path). Capture `projectId` exactly as today
   (lines 416-421), including the 200-vs-201 existing-project handling (lines 423-427).
4. **Add the provider:**
   `POST /api/v1/projects/{projectId}/providers` with
   `{ brokerId: <embeddedBrokerId>, localPath: pathValidation.resolved }` — the same
   request the CLI sends (`AddProviderRequest`, `cmd/hub.go:2379-2382`;
   server handler `addProjectProvider`, `pkg/hub/handlers.go:7980-8043`). Use the
   **resolved** path from validation so the stored `LocalPath` is canonical.
5. On success, `navigateToProject(projectId)` (line 430). On provider failure after
   project creation, surface the error and leave the user on the form (the project
   exists but is unlinked — acceptable; re-submitting will hit the 200 existing-project
   path and retry the provider add).

> Atomic alternative (optional, larger): extend `POST /api/v1/projects` to accept an
> optional `{ linkedPath, brokerId }` and have the handler create the provider in the
> same transaction. Cleaner UX (no orphaned project on failure) but a backend change
> to the create handler; defer unless the two-step proves flaky.

---

## 6. Discovering the co-located broker in the UI

The provider add needs the embedded broker's ID. The hub knows it
(`embeddedBrokerID`, `pkg/hub/server.go:503`, accessor surface at `:1059-1074` —
note only `isEmbeddedBroker` exists today; **add a `GetEmbeddedBrokerID() string`
accessor**). Expose it to the browser via one of:

- **Preferred:** include an `embedded: true` flag on the relevant broker in the
  existing brokers list the UI already consumes, and/or surface the ID on the public
  settings/bootstrap payload the workstation UI loads at startup. The UI filters for
  the embedded broker and uses its ID.
- The validation response could also echo the broker that performed the check
  (`"brokerId": "<embedded>"`), letting the UI carry it straight from validate →
  provider-add without a separate lookup. This is the lowest-friction option and is
  recommended: the path is only meaningful relative to the broker that validated it.

Gate this disclosure behind the same workstation flag (§4) — production UIs never
need an embedded-broker shortcut.

---

## 7. File-by-file change list

**Backend**

| File | Change |
| --- | --- |
| `pkg/hub/server.go` | Add `Workstation bool` to `ServerConfig`; store `s.workstation`; add `GetEmbeddedBrokerID()` accessor. |
| `cmd/server_foreground.go:774` | Set `Workstation: !productionMode` in the `hub.ServerConfig{}` literal. |
| `pkg/hub/system_handlers.go` *(new)* | `validateLocalPath`, `listDir` (`fs/list`), `mkdir` (`fs/mkdir`) handlers + request/response structs. |
| `pkg/hub/fs_safety.go` *(new)* | `requireWorkstation` guard, `resolveAndClassifyPath` (resolve, symlink-expand, managed-root + git + already-linked classification), reusing `hubNativeProjectPath` logic (`handlers.go:3736-3752`). Shared by all three `fs/*` handlers. |
| `pkg/hub/handlers.go` router | Add a guarded `system/fs/` branch routing `validate-path` (POST), `list` (GET), `mkdir` (POST) in the `MountHubAPI` dispatch (style per `:4410-4416`). |
| `pkg/store` *(optional)* | `GetProviderByLocalPath` helper if the provider scan is too coarse. |

**Frontend**

| File | Change |
| --- | --- |
| `web/src/components/pages/project-create.ts` | `'linked'` mode: type (line 29), selector option (466-480), state + debounced `onLocalPathInput`, "Browse…" button, conditional render block, two-step `handleSubmit` (356). |
| `web/src/components/dialogs/directory-browser.ts` *(new)* | `scion-directory-browser`: breadcrumb + folder list over `fs/list`, "New folder" over `fs/mkdir`, Select writes resolved path to `localPath`. |
| brokers/settings client payload | Surface embedded-broker ID/flag for the UI (§6), or echo `brokerId` from validate response. |

**Docs**

| File | Change |
| --- | --- |
| `.design/workstation-onboarding.md` | Mark W5 as detailed here; cross-link. |

---

## 8. Testing

- **Backend unit** — `resolveAndClassifyPath`: nonexistent path, file-not-dir,
  unreadable dir, managed-path rejection (`~/.scion/projects/...`), symlink that
  escapes into a managed path, git vs non-git, already-linked. Follow existing hub
  handler test style (`pkg/hub/handlers_project_test.go`, which already sets
  `SetEmbeddedBrokerID`, e.g. `:866`).
- **Fencing** — `validate-path`, `fs/list`, and `fs/mkdir` each return 404 when
  `Workstation=false`; return results when `true`; reject non-loopback client when bind
  is non-loopback. `fs/list` is non-recursive; `fs/mkdir` rejects names with
  separators/`.`/`..` and refuses managed-path parents.
- **Integration** — two-step create: project + provider, assert resulting
  `ProjectType == "linked"` (`models.go:186-246`) and WebDAV resolves to the local
  path (`project_webdav.go:136-190`).
- **Frontend** — mode switch hides git fields; debounced validation renders
  pass/warn/fail; submit issues the two calls in order with the resolved path.

---

## 9. Resolved decisions (2026-05-31)

1. **Managed-path overlap → hard-fail (D6).** No legitimate reason to link a subdir of
   the managed space; `validate-path` and `mkdir` both reject it (§3.2 step 4, §3.6).
2. **Provider-add failure → two-step, accept the recovery model (D7).** §5.4's
   create-then-provider flow stays; a failed provider-add leaves a recoverable unlinked
   project (re-submit retries). The atomic create-handler change is deferred unless this
   proves flaky.
3. **Directory-browser home root → `$HOME`.** `fs/list` defaults to `$HOME` when no
   `path` is given (§3.5); the user can navigate elsewhere, every read fenced.
4. **Single embedded broker assumed.** Exactly one co-located broker per workstation
   (matches today's combo mode). If that ever changes, §6 needs a picker.
