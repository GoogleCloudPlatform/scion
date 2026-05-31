# Phase 2: System API Endpoints

**Branch:** workstation-improvements  
**Design docs:** `.design/workstation-onboarding-wizard.md` §6, `.design/workstation-onboarding.md` §7  
**Prereq:** Phase 0+1 complete (Workstation flag and requireWorkstation middleware exist)  
**Commit all changes to the current branch as you go.**

---

## Overview

Add thin `GET/PUT /api/v1/system/*` API endpoints that wrap existing Go logic. All are gated by `requireWorkstation` (404 in production). All require normal auth.

---

## 2.1 — Refactor doctor into a returnable function + `GET /system/check`

- In `cmd/doctor.go` (line 30-261) or `pkg/runtime/doctor.go` (line 15-41), extract the core check logic into a function `GatherDiagnostics(ctx context.Context, cfg *config.Config) ([]DiagnosticResult, error)` that returns structured results instead of printing them.
- `DiagnosticResult` struct: `{ Name, Status ("pass"|"warn"|"fail"), Message string }`
- Add `GET /api/v1/system/check` handler:
  - Calls `GatherDiagnostics`
  - Returns JSON: `{ "results": [...], "ready": bool }` where `ready` = no "fail" results

## 2.2 — `GET` and `PUT /system/runtime`

- `GET /api/v1/system/runtime`:
  - Calls `config.DetectLocalRuntime()` (`pkg/config/runtime_detect.go:52-75`)
  - Returns: `{ "detected": "docker"|"podman"|"container", "configured": "...", "available": bool }`
- `PUT /api/v1/system/runtime`:
  - Body: `{ "runtime": "docker"|"podman"|"container" }`
  - Validates the choice, writes `active_profile` (or runtime setting) to `settings.yaml`
  - Returns the updated runtime config

## 2.3 — `ComputeOnboardingStatus` + `GET /system/status`

Compute a struct describing the onboarding state of the machine:

```go
type OnboardingStatus struct {
    Initialized    bool   // ~/.scion/settings.yaml exists
    IdentitySet    bool   // DevAuthConfig has username set (non-default)
    RuntimeOK      bool   // a runtime is detected and reachable
    HarnessesSeeded bool  // at least one harness-config exists
    ImagesPresent  bool   // at least one harness image is present (optional check)
    HasWorkspace   bool   // at least one project exists
    Complete       bool   // all required steps done
}
```

- `GET /api/v1/system/status` returns this struct as JSON
- Used by the frontend to detect first-run and resume the wizard

Also wire up `PUT /api/v1/system/identity` here if not done in Phase 1:
- Body: `{ "displayName": "...", "email": "..." }`
- Writes to `DevAuthConfig`, returns updated identity

## 2.4 — `POST /system/init`

- Body: `{ "harnesses": ["claude", "gemini", "codex", "opencode"] }` (subset allowed)
- Calls `config.InitMachine()` (`pkg/config/init.go:548-620`) if not already initialized
- Seeds only the selected harness-configs (filter the full seed set)
- Returns `{ "ok": true, "initialized": true }`
- Idempotent: safe to call on an already-initialized machine (no-op or partial re-seed)

---

## Route Registration

Register all endpoints in the existing `MountHubAPI` function (`pkg/hub/web.go:518-527`), wrapped with `requireWorkstation`:

```go
// System / onboarding endpoints (workstation only)
r.With(s.requireWorkstation).Get("/system/status", s.handleSystemStatus)
r.With(s.requireWorkstation).Get("/system/check", s.handleSystemCheck)
r.With(s.requireWorkstation).Get("/system/runtime", s.handleGetRuntime)
r.With(s.requireWorkstation).Put("/system/runtime", s.handlePutRuntime)
r.With(s.requireWorkstation).Post("/system/init", s.handleSystemInit)
r.With(s.requireWorkstation).Put("/system/identity", s.handlePutIdentity)
```

Put handler implementations in a new file: `pkg/hub/system_handlers.go`

---

## Commit Instructions

- One commit per logical unit is fine, or bundle as: `feat: add /system/* API endpoints for workstation onboarding (W1/W2)`
- Run `go build ./...` and `go vet ./...` before committing
- Do not open PRs — commit directly to `workstation-improvements`
