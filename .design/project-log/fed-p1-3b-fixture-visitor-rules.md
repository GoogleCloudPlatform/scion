# Fixture rows for project visitor rules and exclusions (ptone/scion#4603, 3B)

- The hub schema gained `project_visitor_rules` and `project_visitor_exclusions`; `internal/fixturegen` had no rows for them, so `TestFixtureCoverage` reported both as missing (85 tables, constant pinned at 83).
- Added to `internal/fixturegen/spec.go`: a federated user (synthetic `.invalid` email, `home_issuer`, `federated_email`), the seeded `project-visitor` role definition (system, project scope, `confers_membership` false), two rules on the same normalized issuer and lower-case domain (one with a window and creator on the main project, one with NULL window and creator on the minimal project), a project-visitor role binding carrying the first rule's `source_rule_id` and window, and an exclusion of the federated user from the second rule.
- `expectedTableCount` is now 85. `TestFixtureLoadable` reads the rows back through `CompositeStore` and `RoleStore` (UUID ids, `source_rule_id`, window, exclusion lookup).
- No production change; no consumer outside `internal/fixturegen` reads the generated fixture.
