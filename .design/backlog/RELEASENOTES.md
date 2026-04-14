# Release Notes — Scion

## Unreleased

### Fixed
- **K8s attach pod name resolution** — `scion attach` now uses the actual grove-prefixed pod name (e.g., `sciontest--hello`) instead of the bare agent name, fixing GKE Warden `autogke-no-pod-connect-limitation` errors
- **K8s attach su password prompt** — `scion attach` no longer prompts for a password on GKE Autopilot pods that run as non-root with `allowPrivilegeEscalation: false`

### Added
- All container images built and published to Artifact Registry (core-base, scion-base, scion-claude, scion-gemini, scion-opencode, scion-codex)

---

## v0.1 — Initial Release

Multi harness agent orchestrator

### Features
- Project scaffolding generated with appteam
- Multi-agent team structure configured
- Development pipeline and workflow established

### Team
- SWE-1: General Engineer 1
- SWE-2: General Engineer 2
- SWE-3: General Engineer 3
- SWE-4: General Engineer 4
- SWE-5: General Engineer 5
- SWE-Test: Automated testing
- SWE-QA: E2E testing & QA
- Platform Engineer: Infrastructure & deployment
- Reviewer: Code review & quality
