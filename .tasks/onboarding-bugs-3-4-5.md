# Fix: Onboarding Bugs 3, 4, 5

**Branch:** workstation-improvements  
**Commit all changes to the current branch.**

---

## Bug 3 — Wizard skips to step 4 on fresh install

**File:** `web/src/components/pages/onboarding.ts`

**Problem:** `initialize()` auto-advances `currentStep` based on backend status flags (`runtimeOK`, `harnessesSeeded`). After `scion init --machine`, both are already true, so the wizard jumps to step 4 (Images) on first launch, skipping Identity, System Check, Runtime, and Harness steps.

**Fix:** The resume logic must only fire if the user has previously progressed through the wizard. Add a `previouslyStarted` check — use `sessionStorage.getItem('onboardingStarted')` (set it when the user clicks "Next" for the first time). Only run the resume auto-advance if `previouslyStarted` is true:

```typescript
async initialize() {
  const status = await this.fetchStatus();
  const previouslyStarted = sessionStorage.getItem('onboardingStarted') === 'true';
  
  if (previouslyStarted) {
    // Resume logic: advance past already-complete steps
    if (status.identitySet && this.currentStep === 0) this.currentStep = 1;
    if (status.runtimeOK && this.currentStep <= 2) this.currentStep = Math.max(this.currentStep, 3);
    if (status.harnessesSeeded && this.currentStep <= 3) this.currentStep = Math.max(this.currentStep, 4);
  }
  // Always start at step 0 on fresh install regardless of backend status
}
```

Set `sessionStorage.setItem('onboardingStarted', 'true')` when the user advances from step 0 for the first time (i.e. in the "Next" handler for step 0, or on any step advance).

Clear `sessionStorage.removeItem('onboardingStarted')` when the wizard completes (step "Done").

---

## Bug 4 — Image names missing registry prefix on step 5

**Files:** `web/src/components/pages/onboarding.ts` and `pkg/hub/system_handlers.go`

### Frontend fix

**Problem:** The event handler extracts only the harness name via `imageNameToHarness()` and reconstructs a partial name for display. The full registry-qualified image name is in the SSE event as `d['image']` but is discarded.

**Fix:** Store and display the full image name from the event data. In the image status map, use the full image name as the key (not the harness name), OR store both:

```typescript
// In the SSE event handler (around line 1004-1016):
if (d['image']) {
  const fullImageName = d['image'] as string;  // e.g. "ghcr.io/homebrew-scion/scion-claude:latest"
  const status = d['status'] as string;
  // Store by full image name
  const next = new Map(this.imageStatuses);
  next.set(fullImageName, { status, error: d['error'] as string | undefined });
  this.imageStatuses = next;
}
```

In the render template (around line 872), display the full image name:
```typescript
// Show: "ghcr.io/homebrew-scion/scion-claude:latest" instead of "scion-claude:latest"
${[...this.imageStatuses.entries()].map(([image, info]) => html`
  <div class="image-status">
    <code>${image}</code>
    <span class="status-${info.status}">${info.status}</span>
    ${info.error ? html`<span class="error">${info.error}</span>` : ''}
  </div>
`)}
```

### Backend: add registry to /system/status

**Problem:** The frontend has no way to verify what registry is configured.

**Fix:** Add `ImageRegistry string` to `OnboardingStatus` in `pkg/hub/system_handlers.go` and populate it by reading `image_registry` from settings (via `config.LoadSettings("")` → `settings.ImageRegistry` or however it's stored). The frontend can display this in the images step header: "Pulling from: ghcr.io/homebrew-scion".

---

## Bug 5 — "Build locally" fails for Homebrew installs

**File:** `pkg/hub/system_handlers.go` (handleSystemImagesBuild, ~line 492-504)

**Problem:** Build script is not present in Homebrew installations. The handler falls back to a CWD/binary-relative path that doesn't exist.

**Fix:** Add an availability check before the build option is shown or invoked:

In `handleSystemImagesBuild`:
```go
// Check build script exists before attempting
buildScript := resolveBuildScript() // existing resolution logic
if buildScript == "" {
    http.Error(w, `{"error":"local builds require a source checkout; use image pull instead","buildUnavailable":true}`, http.StatusUnprocessableEntity)
    return
}
```

In `GET /system/status`, add `BuildAvailable bool` that returns `true` only if the build script can be resolved. Frontend uses this to:
- Hide the "Build locally" button when `!status.buildAvailable`
- Show an explanatory note: "Pre-built images are available from ghcr.io/homebrew-scion. Local builds require a source checkout."

This ensures that for Homebrew installs the user only sees the pull path, while developer checkouts still get the build option.

---

## Commit Instructions

- `fix: only resume wizard progress if user has previously started onboarding (bug 3)`
- `fix: display full registry-qualified image names in wizard step 5 (bug 4)`
- `fix: add registry and buildAvailable to system/status; hide build option for brew installs (bug 5)`
- Run `go build ./...` and `go vet ./...` before committing Go changes
- Do not open PRs — commit directly to `workstation-improvements`
