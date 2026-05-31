# Phase 4: Harness-Aware Image Pull + Local Build (W4)

**Branch:** workstation-improvements  
**Design docs:** `.design/workstation-onboarding-wizard.md` §7, `.design/workstation-onboarding.md` §7 Phase 4  
**Prereq:** Phase 3 complete (wizard shell exists with placeholder image step)  
**Commit all changes to the current branch as you go.**

---

## Overview

Add Go-native image pull (per-image progress via SSE) and a local build option. Wire the wizard Images step (step 4) to these endpoints.

---

## 4.1 — `pkg/runtime/imagepull.go`

New file. Implement:

```go
// HarnessImages returns the image names needed for the given harness keys.
// Keys: "claude", "gemini", "codex", "opencode"
func HarnessImages(harnesses []string, registry string) []string

// PullResult is the per-image result streamed to the caller.
type PullResult struct {
    Image  string
    Status string // "queued" | "exists" | "pulling" | "done" | "error"
    Error  string
}

// PullImages pulls the images for the given harnesses, streaming PullResult
// events to the provided callback. Uses the runtime's ImageExists / PullImage.
func PullImages(ctx context.Context, rt runtime.Runtime, harnesses []string, registry string, onEvent func(PullResult)) error
```

- Use `rt.ImageExists(ctx, image)` first; if true, emit `status: "exists"` and skip
- Otherwise emit `status: "pulling"`, call `rt.PullImage(ctx, image)`, emit `status: "done"` or `status: "error"`
- Pull sequentially (one at a time) to avoid overwhelming the daemon

Look at `pkg/runtime/interface.go` for `ImageExists` and `PullImage` method signatures.

Determine the image names per harness by looking at how `pull-containers.sh` (`image-build/scripts/pull-containers.sh`) resolves them, or at the harness-config seeds in `pkg/config/init.go`.

## 4.2 — `POST /api/v1/system/images/pull`

In `pkg/hub/system_handlers.go`:
- Body: `{ "harnesses": ["claude", ...] }` (subset of what was seeded in Phase 2)
- Reads `image_registry` from settings (the pre-seeded `ghcr.io/homebrew-scion` or user-configured value)
- Starts a background pull job, assigns a `jobId` (UUID)
- Streams `PullResult` events on the existing `/events` SSE stream under subject `system.images.<jobId>`
- Returns immediately: `{ "jobId": "..." }`

SSE event format (look at how other events are published in `pkg/hub/` for the pattern):
```json
{ "subject": "system.images.<jobId>", "image": "...", "status": "pulling"|"done"|"exists"|"error", "error": "..." }
```

## 4.3 — `POST /api/v1/system/images/build` (local build option)

- Body: `{ "harnesses": ["claude", ...] }`  
- Only available after runtime is confirmed (check `GET /system/runtime` `available: true`)
- Shells out to `image-build/scripts/build-images.sh` (or the appropriate build script) with the correct flags
- Streams stdout/stderr lines as SSE events under `system.images.<jobId>` with `{ "type": "log", "line": "..." }`
- Returns immediately: `{ "jobId": "..." }`

If no build script is found or accessible from the Hub's working directory, return a clear error.

## 4.4 — Wire up the wizard Images step (step 4)

In `web/src/components/pages/onboarding.ts`:
- Replace the placeholder images step with real UI
- Call `GET /system/status` to know which harnesses were seeded; display them as a list
- "Pull images" button → `POST /system/images/pull`; subscribe to `/events` filtered by `system.images.<jobId>`
- Render per-image status pills: queued → pulling (spinner) → done ✓ / exists ✓ / error ✗
- "Build locally" toggle/button (shown only if runtime is available): calls `POST /system/images/build`; shows a collapsible log panel with streaming output
- "Skip for now" button always available — step is not blocking

---

## Commit Instructions

- `feat: add Go-native harness image pull with per-image SSE progress (W4)`
- `feat: add local image build option via POST /system/images/build (W4)`  
- `feat: wire wizard images step to pull/build endpoints (W4)`
- Run `go build ./...` and `go vet ./...` before committing Go changes
- Do not open PRs — commit directly to `workstation-improvements`
