# Audit update M1: shared credential metadata

## Behavior

- Added `pkg/credentialmeta` as the dependency-neutral owner of credential metadata bounds, canonical credential and boundary kinds, reserved label names, label grammar, secret-pattern detection, control/format rejection, and value-free typed validation errors.
- Preserved existing UAT issuance behavior: name remains optional, whitespace-only purpose remains accepted, the existing metadata bounds and label allowlist remain unchanged, and legacy stored decorations still use render-time sanitization rather than new issuance validation.
- Made audit credential references immutable and constructor-backed. `auditevent.NewCredentialRef` returns a validated reference, builders accept only that type, and rendering reads it through defensive accessors.
- Applied the full audit privacy contract to every serialized credential string: kind, ID, name, boundary kind, boundary project ID, label keys, and label values. Credential and boundary kinds are closed canonical enums; secret-shaped and control/format-bearing values are rejected without appearing in errors.

## Files

- `pkg/credentialmeta/metadata.go` and `metadata_test.go`: canonical contract, immutable reference, and contract tests.
- `pkg/hub/credential_decoration.go`: issuance validator and shared constants now delegate to `credentialmeta`; legacy rendering remains unchanged.
- `pkg/hub/auditevent/types.go`, `builder.go`, `render.go`, and `validate.go`: constructor-backed credential reference integration and removal of the divergent validator copy.
- `pkg/hub/auditevent/auditevent_test.go`: constructor, serialization, bounds, per-field privacy canaries, and value-free error coverage.

## Verification

- `go test -p 2 ./pkg/credentialmeta ./pkg/hub/auditevent`
- `go test -p 2 ./pkg/hub -run 'TestValidateCredentialMetadata|TestCredentialDecoration|TestAuditActor|TestAccessBoundary|TestCredentialValidation' -count=1`
- `git diff --check`

## Retained-author handoff

- Construct audit credentials with `auditevent.NewCredentialRef(auditevent.CredentialRefInput{...})`; direct field literals are intentionally unavailable.
- Use the canonical values exported by `auditevent` (`CredentialUAT`, `CredentialAgentJWT`, `CredentialBoundaryProject`, and peers). The serialized UAT kind is the server-canonical `uat` value.
- This change owns review-round-2 finding 1 only. Non-credential value-free errors, render snapshotting, capture-sink immutability, and the unrelated nil-interface issue remain with the retained author.

## Review round 1 fixes

- `hub.CredentialKind` and `permissions.BoundaryKind` now alias the dependency-neutral `credentialmeta` types, and all existing server constants alias the canonical constants. Bidirectional compile assertions and exhaustive parity tables prevent the server and audit vocabularies from drifting; unknown credential kinds remain rejected.
- Hub issuance wraps the shared field/rule in `ErrInvalidUATMetadata`, preserving the public `invalid <field>: <rule>` response while the audit constructor continues returning `credentialmeta.ValidationError` with its audit-specific stable text. The handler regression pins `invalid name: must be at most 128 bytes`.
- The credential-reference tests now directly cover valid absent/project/hub boundary pairs and reject project-without-ID, hub-with-project-ID, and absent-kind-with-project-ID pairs.

Review-fix verification:

- `go test -p 2 ./pkg/credentialmeta ./pkg/hub/permissions ./pkg/hub/auditevent`
- `go test -p 2 ./pkg/hub -run 'TestCredentialKindsAliasCanonicalContract|TestCredentialDecoration_MintPreservesValidationErrorMessage|TestValidateCredentialMetadata' -count=1`
