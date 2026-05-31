# Review Round 2 — Fix Remaining Minor Issues

**Branch:** workstation-improvements  
**Review doc:** `.scratch/review-round-2.md`  
**Commit all fixes to the current branch.**

Fix these 3 remaining minor issues that were either not applied or partially applied in round 1:

---

## m4 — Replace `context.Background()` with server-lifetime context (PARTIAL FIX NEEDED)

**File:** `pkg/hub/system_handlers.go` — pull goroutine (~L418) and build goroutine (~L496)

Both goroutines still use `context.Background()`, meaning they cannot be cancelled on server shutdown.

**Fix:** The server struct likely has a context or done channel. Look for a `ctx context.Context` field on the server struct, or a `shutdownCtx`. Use that instead of `context.Background()` in both goroutines. If no server-lifetime context exists, use `context.WithCancel` tied to the server's `Close()`/`Shutdown()` method — store the cancel func on the server struct.

---

## m5 — Remove `devUser := devUser` self-shadow

**File:** `pkg/hub/auth.go` line 207 (approximately)

Search for `devUser := devUser` in `pkg/hub/auth.go` and remove the redundant self-assignment. The variable is already in scope.

---

## N1 — M2 empty-ActiveProfile edge case in `handlePutRuntime`

**File:** `pkg/hub/system_handlers.go` — `handlePutRuntime` (~L189-191)

When `vs.ActiveProfile == ""`, the handler falls back to writing to `"default"` profile but doesn't set `vs.ActiveProfile`. Then `handleGetRuntime` reads `vs.Profiles[""]` (empty key, not found) and returns `configured == ""` — inconsistent with the PUT.

**Fix:** Apply the same `"default"` fallback in `handleGetRuntime` when `vs.ActiveProfile == ""`:
```go
activeProfile := vs.ActiveProfile
if activeProfile == "" {
    activeProfile = "default"
}
profile := vs.Profiles[activeProfile]
```
This makes GET and PUT use the same fallback key consistently.

---

## Commit Instructions

- `fix: use server-lifetime context in image pull/build goroutines (m4)`
- `fix: remove devUser self-shadow in auth.go (m5) and fix empty-ActiveProfile GET/PUT inconsistency (N1)`
- Run `go build ./...` and `go vet ./...` before committing
- Do not open PRs — commit directly to `workstation-improvements`
