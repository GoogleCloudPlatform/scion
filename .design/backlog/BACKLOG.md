# Project Backlog — Scion

**Maintained by:** TPM
**Last updated:** (initial generation)

---

## How to Read This Backlog

- **ID:** Unique feature identifier (`F-0001`, `F-0002`, etc.) — sequential across all milestones, never reused
- **Priority:** P0 (critical path), P1 (important), P2 (nice to have)
- **Status:** `backlog` | `in-progress` | `in-review` | `done` | `blocked`
- **Owner:** Assigned team member
- **Branch:** Git feature branch
- **Dependencies:** Other feature IDs that must complete first
- **Feedback:** Review notes, blockers, decisions — updated as work progresses

---

## Current Milestone

| ID | Feature | Spec | Priority | Status | Owner | Branch | Dependencies | Feedback |
|----|---------|------|----------|--------|-------|--------|--------------|----------|
| F-0001 | Fix K8s attach pod name resolution | — | P0 | done | Direct | main | — | Pod name used bare agent name (`hello`) instead of grove-prefixed pod name (`sciontest--hello`), causing GKE Warden rejection. Fixed in `pkg/runtime/k8s_runtime.go:1725` by setting `podName = agent.ContainerID` after lookup. |
| F-0002 | Fix K8s attach su password prompt | — | P0 | done | Direct | main | F-0001 | `su - scion` prompts for password when container already runs as `scion` user (GKE Autopilot sets `runAsUser: 1000`, `allowPrivilegeEscalation: false`). Fixed in `pkg/runtime/k8s_runtime.go:1747-1751` with runtime `whoami` check to skip `su` when already the target user. |

---

## Future Items

| ID | Feature | Spec | Priority | Status | Owner | Branch | Dependencies | Feedback |
|----|---------|------|----------|--------|-------|--------|--------------|----------|
| | | | | | | | | |

---

## Team Roster

| Role | Agent | Specialty |
|------|-------|-----------|
| PM | PM | Product requirements & PO communication |
| TPM | TPM | Backlog, coordination & progress tracking |
| SWE-1 | SWE-1 | General Engineer 1 |
| SWE-2 | SWE-2 | General Engineer 2 |
| SWE-3 | SWE-3 | General Engineer 3 |
| SWE-4 | SWE-4 | General Engineer 4 |
| SWE-5 | SWE-5 | General Engineer 5 |
| SWE-Test | SWE-Test | Automated testing & coverage |
| SWE-QA | SWE-QA | E2E testing & QA |
| Platform | Platform Engineer | Infrastructure & deployment |
| Reviewer | Reviewer | Code review & quality |
