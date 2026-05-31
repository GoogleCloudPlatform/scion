# Review Round 1 — Fix MAJOR Issues

**Branch:** workstation-improvements  
**Review doc:** `.scratch/review-round-1.md`  
**Commit all fixes to the current branch.**

Fix the 3 MAJOR issues and the 5 MINOR issues from the code review.

---

## M1 — Home-directory fence: sibling-prefix bypass (MUST FIX)

**File:** `pkg/hub/system_handlers.go` — `handleFSList` (~L470) and `handleFSMkdir` (~L560)

**Problem:** `strings.HasPrefix(resolved, home)` allows `/home/alice-backup` when `home=/home/alice`.

**Fix:** Replace the home-boundary check in both handlers with:
```go
sep := string(filepath.Separator)
if resolved != home && !strings.HasPrefix(resolved, home+sep) {
    http.Error(w, "path must be within the home directory", http.StatusForbidden)
    return
}
```

Also add a sibling-prefix test case to `TestFSList_OutsideHome` in the test file.

---

## M2 — `PUT /system/runtime` writes runtime into `active_profile` (MUST FIX)

**File:** `pkg/hub/system_handlers.go` — `handlePutRuntime` (~L165)

**Problem:** `config.UpdateSetting("", "active_profile", req.Runtime, true)` sets a profile *name* to a runtime value like `"docker"` — wrong field, inconsistent with GET.

**Fix:** Instead of overwriting `active_profile`, update the runtime field of the *current active profile*. Look at how `V1Settings.Profiles` is structured in `pkg/config/settings_v1.go`. The correct approach is:
1. Load the current settings
2. Get the active profile name (`vs.ActiveProfile`)
3. Set `vs.Profiles[activeProfile].Runtime = req.Runtime` (or the equivalent field name)
4. Save settings

If the profile's runtime field has a different path/key in UpdateSetting, use the correct dotted path like `"runtimes.<name>.type"` or whatever the schema uses. Look at the existing settings YAML structure to find the right key.

Also ensure `handleGetRuntime` reads from the same location so GET and PUT are consistent.

---

## M3 — `fs/validate-path` has no home fence (MUST FIX)

**File:** `pkg/hub/system_handlers.go` — `handleFSValidatePath`

**Context:** The design explicitly requires linked groves to work with arbitrary paths (outside `$HOME` even), since users may have projects on external drives. However, the asymmetry with `fs/list` and `fs/mkdir` is surprising and should be explicit.

**Fix:** Add a comment in `handleFSValidatePath` explicitly documenting that this endpoint intentionally has no home-boundary fence, since linked groves can be anywhere on disk. Also add `assertLoopback` to this handler if it's not already there (verify). The managed-path overlap check in `ClassifyPath` is sufficient for its safety guarantee.

---

## Minor fixes (also implement these)

### m1 — Build script path from CWD
**File:** `pkg/hub/system_handlers.go` — `handleSystemImagesBuild`

Replace `os.Getwd()` with the binary's executable path:
```go
exe, err := os.Executable()
if err != nil { ... }
buildScript := filepath.Join(filepath.Dir(exe), "..", "image-build", "scripts", "build-images.sh")
```
Or if that's not appropriate for the install layout, check `SCION_ROOT` env var first, then fall back to a documented path. At minimum, emit a clear error message if the script is not found (not a silent 404).

### m2 — ClassifyPath project cap
**File:** `pkg/hub/fs_safety.go`

Change the hard `Limit: 500` to a larger value (e.g. 10000) or add a comment acknowledging the cap. This is low-priority but document the limitation.

### m3 — Use `apiFetch` in project-create.ts
**File:** `web/src/components/pages/project-create.ts` (~L198)

Replace the bare `fetch()` call for the providers POST with `apiFetch()` (or whatever the project's authenticated fetch wrapper is — look at how other pages make API calls).

### m4 — Fire-and-forget goroutines
**File:** `pkg/hub/system_handlers.go` (images/pull ~L300, images/build ~L420)

Replace `context.Background()` with a server-lifetime context (store one on the server struct, or use `r.Context()` passed through). For the pull goroutine, emit a terminal SSE event on overall failure (e.g. `{ "status": "error", "error": "top-level failure message" }`).

### m5 — Cosmetic
**File:** `pkg/hub/devauth.go` (~L207)

Remove `devUser := devUser` self-shadow.

---

## Commit Instructions

- `fix: correct home-directory boundary check in fs/list and fs/mkdir (M1)`
- `fix: write runtime to active profile runtime field not active_profile key (M2)`
- `fix: document intentional unfenced validate-path + assertLoopback (M3)`
- `fix: address minor review findings (m1-m5)`
- Run `go build ./...` and `go vet ./...` before committing
- Do not open PRs — commit directly to `workstation-improvements`
