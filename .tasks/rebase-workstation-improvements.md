# Rebase workstation-improvements onto latest upstream main

## Goal
Rebase `ptone/scion:workstation-improvements` onto the latest `GoogleCloudPlatform/scion:main`, resolve all conflicts carefully, verify the build, and force-push.

## Setup
```bash
git remote -v  # verify remotes; 'origin' = GoogleCloudPlatform/scion, 'ptone' = ptone/scion
git fetch origin
git fetch ptone
git checkout workstation-improvements  # or: git checkout ptone/workstation-improvements -b workstation-improvements
git rebase origin/main
```

## What's on the workstation-improvements branch (preserve all of this)

The branch adds a complete **workstation onboarding experience** on top of upstream main. Key additions:

### New files (ours, no upstream equivalent — no conflict expected)
- `pkg/daemon/ports.go` — detectOccupiedPorts() for phantom daemon detection
- `pkg/hub/system_handlers.go` — all `/system/*` API endpoints (status, check, runtime, init, identity, images, fs/*)
- `pkg/hub/system_identity.go` — PUT /system/identity handler
- `pkg/hub/fs_safety.go` — ClassifyPath() for linked grove path validation
- `pkg/runtime/imagepull.go` — Go-native image pull
- `web/src/components/pages/onboarding.ts` — the full wizard (7 steps)
- `web/src/components/shared/dir-browser.ts` — directory browser component
- `.tasks/` directory — task briefs (safe to keep or drop)
- `.design/workstation-onboarding*.md`, `.design/linked-groves-ui.md` — design docs

### Modified files (likely conflict zones)

**`cmd/root.go`** — We added `usesWorktrees(cmd)` helper and wrapped the git version check so it only fires for `start`/`run` commands. Look for the `if util.IsGitRepo() && usesWorktrees(cmd)` block and the `usesWorktrees` function near the bottom. If upstream touched this file, resolve by keeping both upstream changes AND our `usesWorktrees` guard.

**`cmd/server_daemon.go`** — We added:
- `needsOnboarding` captured BEFORE `daemon.StartComponent()` (race condition fix)
- `printWorkstationQuickstart` signature change: takes `needsOnboarding bool` + `globalDir string` instead of just `globalDir string`
- Port conflict detection: calls `detectOccupiedPorts(cfg)` before starting
If upstream touched server_daemon.go, carefully merge keeping both sets of changes.

**`cmd/server_foreground.go`** — We changed:
- `productionMode` → `hostedMode` in the `Workstation: !hostedMode` line
- Added `if hostedMode { log.Println("WARNING...") }` (dev-auth warning only in hosted mode)
- Removed the unconditional "WARNING: Development authentication enabled" log line
If upstream refactored this file, resolve: keep upstream structure + our Workstation flag assignment + our warning suppression.

**`cmd/server.go`** — We added `--force` flag to the `stop` command (calls `detectOccupiedPorts` and kills by port).

**`pkg/hub/server.go`** — We added `Workstation bool` to `ServerConfig`, `s.workstation bool` field, `requireWorkstation` and `assertLoopback` helpers, `GetEmbeddedBrokerID()`, server-lifetime context (`s.ctx`, `s.ctxCancel`). Also `seedDevUser` call now passes `cfg.DevUserConfig`.

**`pkg/hub/web.go`** — We changed dev auto-login (around line 1142) to read display name/email from `ws.store.GetUser(DevUserID)` instead of hardcoding "Development User"/"dev@localhost".

**`pkg/hub/seed.go`** — We changed `seedDevUser` signature to accept `DevUserConfig` and use `NewDevUser(cfg)` for initial values instead of hardcoding.

**`pkg/hub/auth.go`** — We removed a `devUser := devUser` self-shadow line.

**`pkg/config/settings_v1.go`**, **`pkg/config/hub_config.go`** — We added `Username`, `DisplayName`, `Email` fields to `DevAuthConfig`/`V1AuthConfig`.

**`pkg/hub/devauth.go`** — We updated `DevUser` construction to use config values and OS user fallback.

**`web/src/components/app-shell.ts`** — We added `'/onboarding': 'Setup'` to PAGE_TITLES.

**`web/src/components/pages/project-create.ts`** — We added the "linked local directory" third mode with `scion-dir-browser`.

## Conflict resolution principles

1. **Always keep our new files** — if upstream added similar functionality, compare carefully, but our `/system/*` handlers are unique.
2. **For modified files**: keep ALL upstream changes (bug fixes, new features) AND ALL our changes. Don't drop either side.
3. **When upstream reformatted/refactored a file we also modified**: apply our logical changes to the new upstream structure.
4. **After resolving each file**: run `go build ./pkg/...` for Go files, check TypeScript compiles for web files.
5. **After all conflicts resolved**: `go build ./... && go vet ./...` must pass before pushing.

## Push
```bash
git push ptone workstation-improvements --force
```

The `ptone` remote should already be configured (https://github.com/ptone/scion.git). If not, add it:
```bash
git remote add ptone https://github.com/ptone/scion.git
```

## Done
Report the commits rebased, any conflicts resolved, and confirm build passes.
