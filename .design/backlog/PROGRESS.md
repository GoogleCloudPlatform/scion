# Development Progress Log — Scion

## Session 1

### Goals
- Initial project setup and configuration

### Completed
- Generated project scaffolding with appteam
  - CLAUDE.md with team workflow, conventions, and pipeline rules
  - Agent definitions for PM, TPM, SWE-1, SWE-2, SWE-3, SWE-4, SWE-5, SWE-Test, SWE-QA, Platform Engineer, Reviewer
  - BACKLOG.md, PROGRESS.md, RELEASENOTES.md

### Next Steps
- Define initial feature backlog in BACKLOG.md
- Begin implementation of first milestone

## Session 2 — 2026-04-13/14

### Goals
- Build all scion container images in Artifact Registry
- Run a test Claude agent on GKE cluster
- Investigate and fix attach issues for remote K8s agents

### Completed
- **Built all container images via Cloud Build** (build `174bd4ab`, ~83 min)
  - core-base, scion-base, scion-claude, scion-gemini, scion-opencode, scion-codex
  - All tagged `c1713dff` + `latest` in `us-central1-docker.pkg.dev/deploy-demo-test/public-docker/`
- **Successfully ran a test Claude agent on GKE** (`scion start test-agent --profile remote --grove global`)
  - Claude Code wrote `hello.py`, ran it, reported task completion — full end-to-end verified
- **F-0001: Fixed K8s attach pod name resolution** (`pkg/runtime/k8s_runtime.go`)
  - `Attach()` used bare agent name as pod name but K8s pods have grove-prefixed names (e.g., `sciontest--hello`)
  - Fixed by setting `podName = agent.ContainerID` after agent lookup
- **F-0002: Fixed K8s attach su password prompt** (`pkg/runtime/k8s_runtime.go`)
  - `su - scion` prompts for password on GKE Autopilot where container runs as non-root with `allowPrivilegeEscalation: false`
  - Fixed with runtime `whoami` check: skip `su` when already the target user
- **Confirmed maintainer push access** to `github.com/GoogleCloudPlatform/scion` via test branch
- **Researched scion attach architecture**: SPDY exec, K8s auth chain, tmux session lifecycle
- **Updated CLAUDE.md** with project overview, tech stack, infrastructure details

### Key Decisions
- Used `--grove global` to work around K8s label validation issue with long filesystem paths
- Cloud Build arm64 QEMU emulation is the primary build bottleneck (~45 min for git compilation alone)
- F-0001 and F-0002 were hotfixed directly without agent teams due to urgency — all future code changes will use agent teams per PO direction

### Next Steps
- Commit F-0001 and F-0002 fixes
- Use agent teams (TeamCreate) for all future code changes
- Investigate K8s label validation bug with long grove paths (separate backlog item)
