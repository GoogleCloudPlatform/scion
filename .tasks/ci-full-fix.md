# Run ci-full and Fix All Issues

**Branch:** workstation-improvements  
**Goal:** Run `make ci-full` and fix every failure, including pre-existing ones unrelated to our changes.

---

## Steps

1. Run `make ci-full` and capture all failures
2. Fix each failure category in turn:
   - `fmt-check` failures → run `make fmt` then re-check
   - `web` (Vite build) failures → fix TypeScript/JS build errors
   - `web-typecheck` failures → fix TypeScript type errors
   - `lint` failures → fix lint errors (go vet, staticcheck, etc.)
   - `golangci-lint` failures → fix golangci-lint findings
   - `test-fast` failures → fix failing tests (including pre-existing ones)
   - `build` failures → fix compile errors

3. After fixing each category, re-run that specific step to confirm it passes before moving on
4. Run full `make ci-full` at the end to confirm everything passes together

## Known pre-existing test failures (from earlier run)

These tests in `pkg/config` were failing before our changes — fix them too:
- `TestIsInsideProject`
- `TestRequireProjectPath_NoProjectError`
- `TestFindProjectRoot_HubContextNoScion_Disabled`
- `TestDiscoverProjects_GitProjectWithExternalConfigUsesWorkspaceMarkerProjectID`
- `TestIsHubContext`

Investigate each failure message and fix the root cause.

## Commit instructions

- Commit fixes in logical groups (e.g. one commit for fmt fixes, one for test fixes, etc.)
- Use clear commit messages describing what was broken and what was fixed
- Run `make ci-full` one final time to confirm all green before the last commit
- Do not open PRs — commit directly to `workstation-improvements`
