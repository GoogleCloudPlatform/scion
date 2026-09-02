# Release Notes (2026-09-01)

Access boundaries ship end-to-end with a canonical evaluator, typed constraint model, preview engine, and full admin UI — the last major piece of the authorization foundation refactor.

## 🚀 Features
* **Access boundary UI/UX with backend security gates (#1445):** Full-stack access boundary implementation (40 commits). Backend: canonical evaluator, typed constraint model, preview engine, provenance/explain, transactional governance, atomic audit, HTTP API and error contract. Frontend: inventory page, guided authoring workflow, preview/commit/detail views, effective-access integration, responsive/a11y hardening. Shared TypeScript contract with AbortController/sequence protection/idempotency.
* **Group management admin UI (#1448):** Complete admin UI/UX for creating and managing custom membership groups (frontend-only, no backend changes).
* **Project settings Auth & Security tab (#1450):** General tab split into General (template, harness, model, telemetry) and Auth & Security (harness auth, agent role, max role, service account), matching the agent-create form's existing structure.

## 🐛 Fixes
* **TestFixtureCoverage fix (#1443):** Added fixture row for `access_constraints` table and bumped expected table count from 58 to 59 — test had been failing on main since the table was added.
