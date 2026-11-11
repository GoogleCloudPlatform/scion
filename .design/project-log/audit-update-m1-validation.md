# Audit update milestone 1 validation

## Validation defect and repair

Combined milestone validation at pinned remote head
`e71af37d5757de05ddccab21a9c324c947c45161` exposed an existing ownership
guard failure:

```text
TestCredentialDecorationNotReadByAuthzCode: accessConstraintAuditCredential
references CredentialDecorationFromContext outside the designated carriage points
```

`accessConstraintAuditCredential` is an audit-only mapper. It copies descriptive
credential metadata into `auditevent.CredentialRefInput` and passes that input to
`auditevent.NewCredentialRef`; it does not receive, return, or branch on an
authorization decision. The repair registers only that exact function in the
guard's function-level audit/rendering allowlist. It does not broaden the scanner,
permit a file or pattern, or change production authorization or audit mapping.

The pre-existing guard supplied the failing regression. After the one-entry
repair, this focused gate passed:

- `go test -count=1 -p 2 ./pkg/hub -run '^TestCredentialDecorationNotReadByAuthzCode$'`

Broader combined-slice verification and the final durable SHA are recorded in
the restricted validation report.

## Combined-slice disposition

The complete milestone-1 path was inspected across the typed envelope,
credential metadata, purpose-specific store, create transaction, structured
sink, retained-history endpoint, shared authentication normalization, cursor,
client contract, and the existing detail/timeline view. No additional defect
was found. Production code remains unchanged from the approved handoffs:

- store/schema/concurrency after `fc6954bc`;
- audit shared interface and credential contract after `c610586d`;
- create writer boundary after `6fbf81f8`;
- shared-auth implementation and focused tests after `b4b2d7bb`.

The accepted external PostgreSQL 16.15 evidence at test-code SHA `d7a51219`
therefore remains applicable: it proved transaction overlap and lock waiting in
the production `FOR UPDATE` path and the deterministic final 1,000-row window.

## Verification

- Focused normal Go tests across `pkg/hub`, `pkg/hub/auditevent`,
  `pkg/credentialmeta`, `pkg/hub/permissions`, `pkg/hub/authzop`, and
  `pkg/store/entadapter` — PASS.
- Focused race tests for audit envelope, credential metadata, permissions, and
  store packages — PASS. The combined command's `pkg/hub` portion remained
  active until the 10-minute wrapper exited 124 without diagnostics, so it is
  recorded as infrastructure-inconclusive and was not restarted.
- Focused web API/timeline/detail/contract tests — PASS, 4 files / 70 tests.
- Web TypeScript checking and production build — PASS.
- Focused production ESLint — PASS with zero errors and 29 existing
  explicit-return-type warnings; focused Prettier — PASS.
- Go formatting, `git diff --check`, scoped vet, and scoped build — PASS.
- Scoped single-concurrency `golangci-lint` reached its required 10-minute hard
  bound with no diagnostics; result is infrastructure-inconclusive, not a pass.

`make ci` and `make ci-full` were not run because the campaign brief prohibits
them. `npm ci` reported the lockfile-existing three advisories (one low, two
high); this validation changed neither manifest nor lockfile.

## Milestone-wide review round 1 disposition

Round 1's sole Required finding is closed by test-only commit
`04b6be7316177acbaa346f432fd5267ee0d8993b`. The existing
`TestConstraintAuditHistory_InvalidTokensFailBeforeHistoryQuery` table now
exercises each previously uncited fail-closed decoder branch while preserving
all earlier invalid-token cases:

- `unknown field` sends an otherwise valid cursor object with an additional
  JSON member;
- `trailing JSON value` sends a valid cursor object followed by a second JSON
  object;
- `encoded token over 2048 characters` sends a 2,049-character encoded token.

Every table case requires HTTP 400 and immediately asserts that the spy store's
`ListConstraintHistory` call count remains zero. No production code changed.
Because the production decoder already implemented all three rejection
branches, this is direct branch-execution coverage rather than a production
red/green repair.

Exact verification for the round-1 delta:

- `timeout 10m go test -count=1 -p 2 ./pkg/hub -run '^TestConstraintAuditHistory_InvalidTokensFailBeforeHistoryQuery$'`
  — PASS (`ok`, 1.032s test execution after the cold compile).
- `timeout 10m go test -v -count=1 -p 2 ./pkg/hub -run '^TestConstraintAuditHistory_'`
  — PASS (all focused endpoint/history tests; `ok`, 4.049s).
- `gofmt -l pkg/hub/handlers_access_constraint_audit_history_test.go` — PASS,
  no output.
- `git diff --check` — PASS.

The prior long Hub race, scoped lint, broad vet/build, and other milestone gates
were intentionally not rerun for this test-only delta. Their statuses and the
accepted unchanged evidence above remain unchanged.
