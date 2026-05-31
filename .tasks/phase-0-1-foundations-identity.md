# Phase 0+1: Foundations, Identity & Labels

**Branch:** workstation-improvements  
**Design docs:** `.design/workstation-onboarding.md` §7, `.design/workstation-onboarding-wizard.md`, `.design/linked-groves-ui.md`  
**Commit all changes to the current branch as you go.**

---

## Phase 0 — Foundations

### 0.1 — `Workstation bool` on `hub.ServerConfig`

File: `pkg/hub/web.go` (or wherever `ServerConfig` is defined — search for the struct)

- Add `Workstation bool` field to `hub.ServerConfig`
- In `cmd/server_foreground.go` around line 774, set `cfg.Workstation = !productionMode` when building the server config
- On the server struct, store it: `s.workstation bool`
- Add two helpers:
  - `requireWorkstation(next http.Handler) http.Handler` — middleware that returns 404 if `!s.workstation`
  - `assertLoopback(r *http.Request) error` — checks `r.RemoteAddr` is loopback (127.x or ::1)
- These will gate all `/system/*` and filesystem endpoints

### 0.2 — `GetEmbeddedBrokerID()` accessor

- Add a method on the server (or hub config) that returns the pre-generated embedded broker ID from `settings.yaml`
- This is used in W5 when the two-step linked-grove create needs to find the co-located broker

---

## Phase 1 — Cosmetic Identity (W2) + Developer Token Relabel (W3)

### 1.1 — Identity fields on DevAuthConfig / V1AuthConfig (W2)

Files:
- `pkg/config/hub_config.go` — `DevAuthConfig` struct (around line 142-158): add `Username`, `DisplayName`, `Email string` fields
- `pkg/config/settings_v1.go` — `V1AuthConfig` (around line 383-388): same additions
- `pkg/hub/devauth.go` — `DevUser` construction (around line 26-49):
  - **Keep the stable UUID** (`be67fbc9-...`) unchanged
  - Read `Username`/`DisplayName`/`Email` from config
  - If unset, default to OS user via `os/user` (`user.Current()` → `u.Username`, `u.Name`)
  - Email default: `<osusername>@localhost`

Add a small `PUT /api/v1/system/identity` endpoint:
- Body: `{ "displayName": "...", "email": "..." }`
- Writes to `DevAuthConfig` in `settings.yaml` via the config save path
- Returns the updated identity
- Protected by `requireWorkstation`

### 1.2 — "Developer token" relabel (W3)

This is a text/copy pass only — **no code logic changes**:
- `cmd/server_daemon.go` `printWorkstationQuickstart()` (line 361-384): change "dev token" → "developer token" in the printed output
- CLI help strings referencing "dev token" in `cmd/` — update to "developer token"  
- Web copy in `web/src/` — grep for "dev token" / "dev-token" in UI strings and update display text (not variable names, not `scion_dev_` format, not env var names — those stay)
- Any user-facing docs in `docs-site/` or `docs-repo/`

---

## Commit Instructions

- Commit Phase 0 work as one commit: `feat: add Workstation flag and workstation-only middleware to ServerConfig`
- Commit Phase 1 work as two commits:
  - `feat: make dev identity configurable, default to OS user (W2)`
  - `chore: relabel dev token as "developer token" in UI and docs (W3)`
- Run `go build ./...` and `go vet ./...` before each commit to verify no compile errors
- Do not open PRs — commit directly to the `workstation-improvements` branch
