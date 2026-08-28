# PR-D2: UAT Enforcement Intersection Tests

**Date:** 2026-08-28
**Branch:** `scion/pf-p2d-enforcement`
**Agent:** dev-pf-p2d-enforcement

## Summary

Wrote comprehensive tests verifying that UAT enforcement correctly intersects with RoleBinding-derived permissions after the D4 conversion. This is a test-only PR — no production code was changed.

## Test File

`pkg/hub/uat_enforcement_test.go` — 864 lines, 13 test functions, 99 test cases.

## Test Groups Covered

| Group | Description | Tests |
|-------|-------------|-------|
| 1 | Scope match + role binding → allowed | 5 resource types (skill, template, group, harness_config) |
| 2 | Scope match + no role binding → denied (narrows, never widens) | 5 cases proving the critical security property |
| 3 | No scope match → denied regardless of bindings | 3 cases (wrong resource, wrong action, cross-resource) |
| 4 | Project constraint enforcement | 5 cases (match, mismatch, hub-level, project-resource match/mismatch) |
| 5 | agent:manage alias and special cases | 4 expansion tests, 3 no-policy-scope tests, 3 expansion-content tests |
| 6 | Cross-resource scope isolation | 4 cases (skill/template isolation, action-specific isolation) |
| 7 | Existing agent/project scopes regression | 4 cases (agent:create/read/delete, project:read) |
| Unit | enforceUATConstraints direct tests for all D1 resource types | 52 cases (26 scope-present + 26 scope-absent) |
| Unit | Hub-level resource denial | 2 cases (broker, user) |
| Unit | ValidUATScopes completeness | 2 cases (includes all D1 scopes, excludes escalation scopes) |

## Key Findings

1. **Enforcement intersection works correctly.** The `enforceUATConstraints` + `checkAccessForUser` pipeline correctly implements `effective = principal_grants ∩ credential_caveats`. A UAT scope alone never grants access without a matching role binding.

2. **Broker and user resources are hub-level.** Both `brokerResource()` and `userResource()` produce parentless resources, so UATs correctly deny them via the hub-level resource check in `enforceUATConstraints`.

3. **agent:manage expansion is token-creation-time.** The `expandScopes()` function in `useraccesstoken.go` expands `agent:manage` into concrete `agent:*` scopes when the token is created. Tests simulate this by using the expanded scope list from `permissions.UATManageScopes()`.

4. **Permission field required for role-binding path.** The `checkAccessForUser` role-binding evaluation (step 3) only fires when `permissionID != ""`. Integration tests use `Decide()` with the `Permission` field set, mirroring how real handlers invoke authz via `RouteMetadata`.

5. **No policy/role/role_binding UAT scopes exist.** Verified that `ValidUATScopes()` contains no `policy:*`, `role:*`, or `role_binding:*` entries.

## No Issues Found

The enforcement intersection is correct. No bugs or gaps were discovered during testing.
