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
