# PR #264 Feedback — Address All Review Comments

**Branch:** workstation-improvements  
**PR:** https://github.com/GoogleCloudPlatform/scion/pull/264  
**Commit all fixes to the current branch.**

Address all feedback from the Gemini code review on PR #264. Fix each issue exactly as suggested.

---

## HIGH Priority

### H1 — Infinite spinner on early image pull failure
**File:** `web/src/components/pages/onboarding.ts` (~line 1025)

The SSE event listener only handles events with an `image` key. Top-level errors from `PullImages` (e.g. when the registry is unreachable before any image starts) publish `{ status: "error", error: "..." }` without an `image` key — these are silently ignored, leaving the spinner running forever.

**Fix:** Add an else-if branch to handle top-level error events in the pull mode:

```typescript
if (mode === 'pull') {
  if (d['image']) {
    const image = d['image'] as string;
    const status = d['status'] as string;
    const error = d['error'] as string | undefined;
    const harness = this.imageNameToHarness(image);
    if (harness) {
      const next = new Map(this.imageStatuses);
      const entry: { status: string; error?: string } = { status };
      if (error) entry.error = error;
      next.set(harness, entry);
      this.imageStatuses = next;
    }
    if (status === 'done' || status === 'exists' || status === 'error') {
      doneCount++;
      if (doneCount >= totalImages) {
        this.imagePulling = false;
        this.cleanupImageEvents();
      }
    }
  } else if (d['status'] === 'error') {
    this.error = (d['error'] as string) || 'An error occurred during image pull.';
    this.imagePulling = false;
    this.cleanupImageEvents();
  }
}
```

### H2 — Windows path separator not normalized in dir-browser
**File:** `web/src/components/shared/dir-browser.ts` (~line 197)

Windows backends return `\\` separators; the frontend splits on `/`, breaking breadcrumbs and navigation on Windows.

**Fix:** Normalize path separators when receiving from API:
```typescript
this.currentPath = data.path.replace(/\\/g, '/');
this.entries = data.entries ?? [];
```

### H3 — Windows drive letter breadcrumb produces invalid path
**File:** `web/src/components/shared/dir-browser.ts` (~line 222)

`navigateToBreadcrumb` prepends `/` to all paths, producing `/C:` (invalid) on Windows.

**Fix:** Replace `navigateToBreadcrumb` with:
```typescript
private navigateToBreadcrumb(index: number): void {
  const segments = this.currentPath.split('/').filter(Boolean);
  const subSegments = segments.slice(0, index + 1);
  let path = '';
  if (subSegments[0] && /^[a-zA-Z]:$/.test(subSegments[0])) {
    path = subSegments.join('/');
    if (subSegments.length === 1) {
      path += '/';
    }
  } else {
    path = '/' + subSegments.join('/');
  }
  void this.navigate(path);
}
```

---

## MEDIUM Priority

### M1 — Windows drive root shows invalid `..` entry
**File:** `web/src/components/shared/dir-browser.ts` (~line 291)

At the root of a Windows drive (e.g. `C:/`), segments.length is 1, so a `..` entry is shown that navigates to invalid `C:`.

**Fix:** Update the condition guarding the `..` entry to also exclude Windows drive roots:
```
!(segments.length === 0 || (segments.length === 1 && /^[a-zA-Z]:$/.test(segments[0])))
```
(i.e. don't render the `..` entry when at home root OR at a Windows drive root)

### M2 — `bufio.Scanner` error not checked after scan loop
**File:** `pkg/hub/system_handlers.go` (~line 519) in `handleSystemImagesBuild`

If a log line exceeds 64KB, `scanner.Scan()` returns false with `scanner.Err() == bufio.ErrTooLong`. This is currently silently swallowed.

**Fix:** After the scan loop, check and publish the error:
```go
scanner := bufio.NewScanner(stdout)
for scanner.Scan() {
    s.events.PublishRaw(subject, imageBuildLogEvent{Type: "log", Line: scanner.Text()})
}
if err := scanner.Err(); err != nil {
    s.events.PublishRaw(subject, imageBuildLogEvent{Type: "log", Line: "error reading build log: " + err.Error()})
}
```

### M3 — Image pull loop doesn't check context cancellation
**File:** `pkg/runtime/imagepull.go` (~line 66)

When the server shuts down and `ctx` is cancelled, the pull loop still iterates all remaining images and fires error events for each.

**Fix:** Check `ctx.Err()` at the top of each iteration:
```go
for _, img := range images {
    if err := ctx.Err(); err != nil {
        return err
    }
    exists, err := rt.ImageExists(ctx, img)
    // ...
}
```

### M4 — No concurrency control for local image builds
**File:** `pkg/hub/system_handlers.go` (~line 445) in `handleSystemImagesBuild`

Multiple concurrent POST requests spawn multiple `build-images.sh` processes simultaneously, overwhelming the workstation.

**Fix:** Add an atomic boolean to `Server` struct to track active build:
```go
// on Server struct:
imageBuildActive atomic.Bool

// in handleSystemImagesBuild:
if !s.imageBuildActive.CompareAndSwap(false, true) {
    http.Error(w, "a build is already in progress", http.StatusConflict)
    return
}
defer s.imageBuildActive.Store(false)
```

---

## Commit Instructions

- `fix: handle top-level pull error events to prevent infinite spinner (H1)`
- `fix: normalize Windows path separators and drive letter breadcrumbs in dir-browser (H2, H3, M1)`
- `fix: check scanner.Err after build log scan loop (M2)`
- `fix: check context cancellation in image pull loop (M3)`
- `fix: add concurrency guard for concurrent image build requests (M4)`
- Run `go build ./...` and `go vet ./...` before committing Go changes
- Do not open PRs — commit directly to `workstation-improvements`
