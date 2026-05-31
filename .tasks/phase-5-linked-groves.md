# Phase 5: Linked Groves via Directory Browser (W5)

**Branch:** workstation-improvements  
**Design docs:** `.design/linked-groves-ui.md`, `.design/workstation-onboarding.md` §7 Phase 5  
**Prereq:** Phase 3 complete (wizard shell exists with placeholder workspace step)  
**Commit all changes to the current branch as you go.**

---

## Overview

Add a workstation-only (404-in-production) server-side directory browser and wire it into `project-create.ts` as a third project mode ("linked local directory"). Also wire the wizard's workspace step (step 5).

---

## 5.1 — `pkg/hub/fs_safety.go`

New file. Core path-safety logic:

```go
// PathClass describes what kind of path was resolved.
type PathClass struct {
    Resolved    string // symlink-resolved absolute path
    Exists      bool
    IsDir       bool
    IsGit       bool   // contains a .git dir/file
    IsManaged   bool   // inside ~/.scion/projects/ or ~/.scion/groves/
    AlreadyLinked bool // already registered as a ProjectProvider LocalPath
}

// ClassifyPath resolves and classifies a candidate path.
// managedRoot is computed via hubNativeProjectPath (handlers.go:3736-3752).
// It queries existing providers to detect already-linked paths.
func ClassifyPath(ctx context.Context, store Store, path, managedRoot string) (PathClass, error)
```

Key rules:
- Symlink-expand with `filepath.EvalSymlinks`
- `IsManaged`: resolved path has `managedRoot` as a prefix → **hard-fail** (D6)
- `AlreadyLinked`: scan `GetProjectProviders` for any provider with matching `LocalPath`

## 5.2 — Fenced filesystem endpoints

In `pkg/hub/system_handlers.go`, add (all wrapped with `requireWorkstation` + `assertLoopback`):

### `GET /api/v1/system/fs/list?path=<dir>`
- If `path` is empty, default to `$HOME`
- Call `os.ReadDir(path)` after resolving the path
- Return: `{ "path": "/abs/path", "entries": [{ "name": "foo", "isDir": true, "isGit": bool }] }`
- Filter out hidden entries (starting with `.`) except `.git` (to detect git repos)
- Reject paths outside `$HOME` (safety: don't expose the whole filesystem)

### `POST /api/v1/system/fs/mkdir`
- Body: `{ "parent": "/abs/path", "name": "new-folder" }`
- Validate parent is within `$HOME`, name has no path separators
- `os.Mkdir(filepath.Join(parent, name), 0755)`
- Returns: `{ "path": "/abs/path/new-folder" }`

### `POST /api/v1/system/fs/validate-path`
- Body: `{ "path": "/abs/path" }`
- Calls `ClassifyPath`; returns `PathClass` as JSON
- If `IsManaged: true`, also set `"error": "This path is inside the Scion managed directory and cannot be linked"`
- Frontend uses this for pre-submit validation

## 5.3 — `project-create.ts` changes

File: `web/src/components/pages/project-create.ts`

Add a third project creation mode: **"Add local directory (linked)"**.

UI flow:
1. Mode selector gains a third tab/radio: "Local directory"
2. On selecting it, show a directory browser component (`scion-dir-browser`) that calls `GET /system/fs/list` as the user navigates
3. Directory browser features:
   - Current path breadcrumb
   - Entry list (folders only, clicking navigates in; files shown greyed out)
   - "New folder" button → `POST /system/fs/mkdir` → refresh listing
   - Selected path shown in a read-only input
4. On path selection, call `POST /system/fs/validate-path`; show inline error if managed-path overlap (D6)
5. Submit = two-step (D7):
   a. `POST /api/v1/projects` to create the project (hub-native initially)
   b. `POST /api/v1/projects/{id}/providers` with `{ "localPath": resolvedPath, "brokerId": embeddedBrokerID }`
   - On step (b) failure: show error with "Retry" — don't delete the project (recoverable per D7)
   - `embeddedBrokerID` from `GET /api/v1/system/status` (add a `embeddedBrokerID` field there) or from `GET /api/v1/brokers` filtered to the embedded one

Add `scion-dir-browser` as a new component in `web/src/components/` — a simple Lit element that manages the navigation state and renders the listing.

## 5.4 — Add `embeddedBrokerID` to system status

In `pkg/hub/system_handlers.go` `handleSystemStatus`:
- Add `EmbeddedBrokerID string` to `OnboardingStatus`
- Populate it from `GetEmbeddedBrokerID()` (added in Phase 0.2)

## 5.5 — Wire up wizard workspace step (step 5)

In `web/src/components/pages/onboarding.ts`, replace the placeholder workspace step:
- Three cards: "Hub-native project", "Link a git repo", "Add local directory"
- Clicking "Add local directory" opens the same directory-browser flow from `project-create.ts` (reuse the `scion-dir-browser` component)
- On completion, advance to step 6 (Done)
- "Skip for now" remains available

---

## Security Checklist (non-negotiable)

- [ ] All `fs/*` and `system/*` endpoints are wrapped with `requireWorkstation` (404 in prod)
- [ ] `assertLoopback` on all `fs/*` endpoints
- [ ] `fs/list` rejects paths outside `$HOME`
- [ ] `fs/mkdir` validates no path separators in `name`
- [ ] `ClassifyPath` hard-fails on managed-path overlap
- [ ] Normal auth required (no anonymous access)

---

## Commit Instructions

- `feat: add path-safety helpers for workstation filesystem operations (W5)`
- `feat: add fenced fs/list, fs/mkdir, fs/validate-path endpoints (W5)`
- `feat: add linked-grove creation mode with directory browser to project-create (W5)`
- `feat: wire wizard workspace step to linked-grove and hub-native create flows (W5)`
- Run `go build ./...` and `go vet ./...` before committing Go changes
- Do not open PRs — commit directly to `workstation-improvements`
