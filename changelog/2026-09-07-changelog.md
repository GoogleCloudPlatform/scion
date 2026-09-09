# Release Notes (2026-09-07)

The authorization audit lands — 131 commits implementing route-level permission classification, authorization catalog enforcement, and structured denial responses across all 177 registered routes.

## 🚀 Features
* **Authorization audit (#1487):** Full authorization audit integration (131 commits) — route-level permission classification, authorization catalog enforcement with 177/177 routes and 194/194 classifications verified, role-binding CRUD, project membership service, access constraints, and structured denial responses. Includes D-002 migration coexistence fix and cold-start super-admin binding.

## 🐛 Fixes
* **BuildKit enabled in cloudbuild-omni (#1484):** Omni cloudbuild config used plain `docker build` (not `docker buildx build`) but upstream Dockerfiles now use `RUN --mount=type=secret` which requires BuildKit. Added `DOCKER_BUILDKIT=1` to all 8 build steps.
