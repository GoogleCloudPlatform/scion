# Release Notes (2026-08-08)

A major security and infrastructure day: tiered agent authorization roles landed with sub-agent scope inheritance, OIDC shipped as both an identity provider and a federation authenticator, and the A2A bridge gained HA support with h2c port multiplexing for Cloud Run.

## 🚀 Features
* **[Auth]:** Tiered agent authorization roles — named roles (none/readonly/baseline/full) with a two-gate authority lattice. User-to-agent role grants via `--role` CLI flag, project `max_agent_role` setting, global hub default, and UI integration. Sub-agent no-escalation enforcement: parent agents cannot grant children higher roles than their own (fail-loud 403). Template `hubAccess.scopes` deprecated (#1089).
* **[Auth]:** OIDC-based federation authentication with multi-issuer support — external identities authenticate via OIDC tokens from trusted issuers (hub, GCP service account, Firebase/Google user). Per-issuer JWKS caching (RS256 pinned), OIDC discovery, `RequireFederationAccess` scope-gated middleware. Feature-gated behind `federation.enabled`. 80+ tests (#1088).
* **[OIDC]:** OIDC Identity Provider for agent identity tokens — `/.well-known/openid-configuration`, `/.well-known/jwks.json`, `POST /api/v1/agent/identity-token` endpoints. `sciontool identity-token --audience` CLI command. 24h key rotation with overlap. Enables agents to authenticate to external systems (Vault, GCP WIF, AWS IRSA, A2A bridges). Feature-gated via `oidc.enabled` (#1078).
* **[A2A Bridge]:** HA support with leaderless architecture — every replica is identical and interchangeable, no leader election or instance identity required (#1074). H2c port multiplexing for Cloud Run single-port deployment — auto-detects via `K_SERVICE`, routes HTTP (A2A protocol) and gRPC (Hub broker) on a shared port (#1084).
* **[Image Build]:** Thick base image variant using Google Cloud Workstations base — `thick-prep` intermediate layer, Cloud Build config, build script integration. All 10 images verified, amd64 only (#1087).
* **[CLI]:** Top-level `scion secret get/set/list` commands mirroring existing sciontool commands, using `hubclient` Secrets service (#1086).
* **[Admin]:** Restart Hub button on maintenance page — `POST /api/v1/admin/maintenance/restart` (fire-and-forget systemd restart, admin-gated) with confirmation dialog (#1085).

## 🐛 Fixes
* **[A2A Bridge]:** Update Dockerfile for Cloud Run — Go 1.26.1 builder, missing COPY statements for transitive deps, `-tags no_embed_web`, updated EXPOSE ports (#1083).
* **[Cloud Build]:** Anchor `.gcloudignore` `internal/` pattern to repo root — unanchored pattern excluded `extras/scion-a2a-bridge/internal/`, breaking Cloud Build for extras modules (#1082).
* **[CLI]:** Suppress local-only mode hints (`scion hub disable`, `--no-hub`) when running as hub-managed agent — disabling hub mode in managed infrastructure breaks orchestration connectivity (#1079).

## 📖 Docs
* **[Docs]:** Nightly documentation update for Aug 7 — agent runtime secret retrieval API, Discord `/scion secret` commands, admin settings clearing (#1080).

## 🔧 Chores
* **[Deps]:** Bump rclone to 1.75.0 across 4 modules (#1071, #1072, #1073, #1077). Bump dompurify to 3.4.13 in web (#1075).
