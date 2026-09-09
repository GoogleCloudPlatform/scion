# Release Notes (2026-09-08)

Post-authorization-audit stabilization — three P0 authorization defects fixed (progeny secret inheritance, broker template reads, agent SA assignment), WebServer login paths now create super-admin RoleBindings, and several UI capability-gate corrections.

## 🚀 Features
* **Internal Python package index for image builds (#1488):** Mirrors the npm registry plumbing — `PIP_INDEX_URL` build-arg and BuildKit secret mount for `pip.conf` enable image builds behind corporate proxies blocking PyPI.

## 🐛 Fixes
* **Progeny secret inheritance (#1489):** P0 — three stacked authorization defects prevented progeny agents from receiving inherited secrets: missing scopes on synthetic identity, wrong permission derivation via `CheckAccess`, and `walkDelegationChain` ignoring the explicit Permission field.
* **Broker template/harness-config reads (#1494):** P0 — broker and agent-scoped reads of templates and harness configs denied by the authorization kernel after the authorization audit merged. Added broker-identity bypass before the `authorize()` security gate and `AgentScopes` to the permission registry.
* **Agent `gcp_service_account.assign` scope (#1502):** P0 — agents denied `gcp_service_account.assign` during agent-creates-agent because the permission had no `AgentScopes` in the registry. Added `project:agent:create` scope.
* **WebServer super-admin RoleBinding (#1508):** Both WebServer login paths (proxy auth and OAuth callback) provisioned users without calling `ensureSuperAdminBinding`, causing `IsSystemAdmin` to return false despite `Role="admin"`. Extracted binding functions to package-level for both Server and WebServer.
* **Agent template resolution (#1504):** Three stacked defects — `template.read/list` missing `AgentScopes`, error mislabelled as authentication failure, and scope-fallback path abort.
* **Full-role agents can manage templates (#1506):** Added `project:template:write` scope for `template.create` and `template.update`, granted only to `agent-role-full`.
* **Embedded broker passthrough gate (#1507):** Co-located broker registered without `CreatedBy`, so the passthrough identity gate's ownership check always failed. Admin-role users now treated as owners for embedded brokers.
* **Agent Start/Stop UI capability (#1490):** Start/Stop buttons never rendered because the UI checked for capability names the backend cannot produce. Now gates on `attach`, matching server-side `authorizeAgentLifecycle` semantics (12 call sites).
* **Duplicate access-denied toasts (#1503):** Added `suppressAccessDeniedToast` to 7 `apiFetch` calls for admin-only page-load requests that already degrade gracefully on 403.
* **Chat terminal window (#1505):** Terminal link now opens in a named window per agent instead of in-app navigation that replaces the chat tab. Named window prevents tab accumulation with popup-blocker fallback.
