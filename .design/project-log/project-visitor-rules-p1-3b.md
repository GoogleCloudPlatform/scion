# Project visitor rules: visitor grants by email domain (phase 1, item 3, part B)

**Date:** 2026-10-13
**Stacked on:** project visitor grants by email (part A)
**Tracked in:** ptone/scion#4603

## What changed

- **Schema (ent, additive).** New tables `project_visitor_rules` (project,
  issuer, lower-cased domain, `not_before`/`expires_at`, `created_by`,
  `created_at`; unique `(project_id, issuer, domain)`, index
  `(issuer, domain)`) and `project_visitor_exclusions` (`rule_id`,
  `user_id`; unique pair). `role_bindings.source_rule_id` (nullable,
  immutable, indexed). Existing rows are unchanged. The generated code was
  produced by `go generate ./pkg/ent` (entc), not by hand.
- **Store.** `CreateProjectVisitorRule`, `GetProjectVisitorRule`,
  `ListProjectVisitorRules`, `ListActiveProjectVisitorRules(issuer,
  domain, at)` (exact domain match; the window is evaluated in Go with
  `ProjectVisitorRule.ActiveAt`, the same rule as bindings),
  `UpdateProjectVisitorRuleWindow` (the rule and every binding it created,
  one transaction), `DeleteProjectVisitorRule` (its bindings, its
  exclusions and the rule, one transaction; returns the deleted bindings),
  `CreateProjectVisitorExclusion` (idempotent; `ON CONFLICT DO NOTHING`
  through `CreateBulk` so a Postgres transaction stays usable) and
  `IsProjectVisitorExcluded`. `CreateVisitorRoleBinding` accepts
  `SourceRuleID`; the rule must exist and be of the binding's project and
  the account's home issuer. The general `CreateRoleBinding` refuses a
  source rule. `store.ValidVisitorRuleDomain` admits host names only: no
  wildcard, no `@`, no leading or trailing dot.
- **Routes** under `/api/v1/projects/{id}/visitors/rules`: `GET`, `POST
  {issuer, domain, notBefore?, expiresAt?}`, `PATCH /{ruleId}` (replace the
  window), `DELETE /{ruleId}`. Same admission as the grant routes
  (experiment, method, interactive session, `project.manage_visitors`) and
  the same non-amplification: adding a rule, or extending its window,
  needs every `project-visitor` permission on the project; shortening and
  removing do not. The issuer must accept visitor grants, and its
  `allowed_domains` (when set) must list the domain and its
  `allowed_emails` (when set) must hold a leading-wildcard pattern that
  covers every address in it (`visitorDomainAllowedByIssuer`). Audits:
  `project_visitor_rule_add`, `_update`, `_remove` (the removal lists the
  removed visitor IDs), all in the transaction of the change.
- **Request path.** `linkPendingVisitor` (first contact) now also creates
  an account when no pending one exists but an active rule matches the
  issuer token's verified email domain, links it, and applies the matching rules
  (`bindVisitorRules`), all in one transaction. For an account linked
  earlier, `applyVisitorRules` adds the bindings of rules added since; it
  writes nothing when nothing is missing, and a failure there is logged
  and does not refuse the request (the next request retries). A rule
  binding carries the rule's ID and window and `created_by` of the rule's
  author; each is audited as `visitor_linked` with `rule_id`,
  `project_id` and `binding_id`. A project where the account already has
  a visitor binding (any source) or an exclusion from the rule is skipped,
  so repeats are idempotent and grants by email are never replaced.
  `allow_visitor_grants` off stops rule materialization and first-contact
  creation, and linked visitors are refused as in part A.
- **Grants list and item routes.** Each visitor carries `source` (`email`
  or `rule`) and `sourceRuleId`. `DELETE /visitors/{id}` on a rule binding
  deletes it and records an exclusion in the same transaction; `PATCH`
  on a rule binding answers 409 (its window follows the rule).
- **Catalog.** Four operations `project.visitors.rules.{list,add,update,
  remove}` (session only, `GOV_PENDING`); mutation symbols and call-site
  classifications for the rule store methods, the exclusion, and the
  rule binding on the request path (authentication-only exemption).
  The catalog report was updated by hand to match `RenderMarkdown`.
- **CLI.** `scion project visitors add --domain`, `list` shows each
  visitor's source and the rules table (`--json` prints `{visitors,
  rules}`), `remove --domain` (or a rule ID) removes a rule. Still not in
  `agentAllowed`.

## Decisions

- Natural expiry of a rule does not delete rows: the bindings carry the
  rule's window, so they stop authorizing at the same instant. Removing
  the rule deletes them. A sweeper for expired rules is a follow-up.
- Rule matching uses the email of the current verified issuer token; the
  account's stored email (fixed at first contact) is not re-read for it.
- One project-visitor binding per account and project (the existing
  unique index): an email grant and a rule on the same project never
  coexist. An active grant by email stands and the rule adds nothing
  there; an inactive one (expired, or not started yet) is replaced by the
  rule binding (see "Review changes" below).
- After first contact the subject is the account's key (design §4.3): a
  later email change at the issuer does not remove a binding a rule
  created. The binding stays while the rule stands and the person is not
  revoked; removing the rule, or revoking the person, ends it. A rule of
  the new domain can still bind the account on another project.

## Review changes

- **Rule changes and a concurrent rule binding.** The request path reads
  the rules before its transaction. The store now re-reads the rule in
  the binding's transaction (`FOR SHARE` on Postgres), refuses one that is
  missing or not active, and gives the binding the rule's window as read
  there, whatever the caller passed. Removing a rule and changing its
  window take the rule row `FOR UPDATE` before touching bindings, so a
  concurrent rule binding either commits first and is then removed or
  changed with the others, or waits and then sees the change. A first
  contact that created an account only for rules that no longer bind it
  rolls back. Revoking a rule visitor whose rule no longer exists removes
  the binding without an exclusion instead of answering 404.
- **Project deletion.** `cascadeSecurityState` now deletes the project's
  visitor rules with their bindings and exclusions
  (`DeleteProjectVisitorRulesByProject`, step 1b, counted in the cascade
  summary as `project_visitor_rules_deleted`); the rule rows have no
  foreign key to the project.
- **Inactive grant by email and a rule.** When the account's only visitor
  binding on a rule's project is a grant by email whose window does not
  contain now, the rule binding replaces it in one transaction
  (`DeleteInactiveVisitorGrant` re-reads and re-checks the grant, `FOR
  UPDATE` on Postgres); the `visitor_linked` record carries
  `replaced_binding_id`. A grant that became active meanwhile is kept.
- **Grant by email to a rule visitor** answers 409 with a message saying
  the person has access through a visitor rule and how to keep access
  independently of it (revoke, then grant).
- **Allowed-email patterns** are lower-cased only, as the per-email check
  does, when deciding whether an issuer admits a rule's domain.

## Verification

Written without running any Go command (coordinator rule for this
task); the generated ent code came from one approved `go generate` run.
Compile, tests, ent-check, catalog and inventory checks run in the gate
after review.
