# Direct decision audit emission contract checkpoint

Date: 2026-10-09 UTC.

Immutable baseline: `01800719b0c9b1a72b65353a8a91637d532a7398`.
The reviewed implementation consists of exactly these five paths:

- `pkg/hub/auditevent/types.go`
- `pkg/hub/auditevent/catalog.go`
- `pkg/hub/auditevent/validate.go`
- `pkg/hub/auditevent/auditevent_test.go`
- `pkg/hub/decision_audit_admission_test.go`

The implementation diff has 1,947 lines / 82,421 bytes and SHA-256
`285205d668e0319329c2396452d9aa042594b0d5114166b9a591c80a3cfbdb3a`.
This log is the sole additional repository path.

Three independent reports approve the exact bounded five-path implementation.
All required findings in that review scope are closed. Their identities are:

| Report scope | Verdict | Lines / bytes | SHA-256 |
| --- | --- | --- | --- |
| Code correctness | APPROVE | 202 / 17,370 | `94e9360db1208408aaf6628fb92a14abfaf4dfe2e91375f6ed336ea7f216a00f` |
| Behavior conservation | APPROVE | 295 / 30,469 | `76d36cee0e6119c40c8163ef97d967b600a67346956f920160fac846d5c003d7` |
| Implementation assurance | APPROVE | 304 / 27,314 | `82db1d45072d2b3f8c0e6d4e0466594bf31d8dc46a5b6a0ea839a8404b522421` |

These approvals cover the schema and finite test fixtures, including returned
permission consistency, independent timing inclusion and joined worker cleanup.
They do not establish successful Hub execution or production readiness.

Both focused local invocations remain INCONCLUSIVE:

| Invocation | Result | Evidence lines / bytes | Evidence SHA-256 |
| --- | --- | --- | --- |
| 15-minute outer cap | Cap expired during Hub compilation; exit 124 | 1,004 / 89,241 | `32e36a1f6c58ae8ae593aaf4c54187aa90c764c6de899702aeef64086fa585a1` |
| 30-minute outer cap | Hub compiler memory allocation failed; exit 1 | 966 / 88,142 | `efeae123eb0b4e2a9395e37b36ef4c9786edfbef5a0d4a0dc277f2f19de85d73` |

Each invocation produced 112/112 expected experiments/auditevent RUN/PASS
outcomes (14 top-level tests and 98 subtests). All 284 expected Hub outcomes
(56 top-level tests and 228 subtests) were unexecuted and missing. There were
zero observed test FAIL/SKIP outcomes. The second invocation reported Hub
package build failure; neither invocation is GREEN. No further local or
community validation retry is authorized. Upstream CI is the remaining Hub
validation authority, and successful required Hub validation is necessary
before this checkpoint can become merge-ready.

The contract fixtures establish observable synchronous local selected-handler
and writer routing, including zero decoy activity. They do not establish hidden
pointer identity, external ingestion, acknowledgement or external durability.
Production NEW remains default-off and structurally unadmitted.

The positive domain is finite registered nonempty returned PermissionID values
and trusted request correlation. Empty/unknown permissions and absent/invalid
correlation have explicit rejection coverage. General missing permission and
correlation classes, all legacy-covered classes, real-Decide placement and
proof of zero routine database writes remain later obligations. This checkpoint
does not establish Store/Decide provenance or placement completion.

No production admission, activation, deployment or legacy deletion is
authorized. The broader phase remains incomplete pending upstream validation
and subsequent obligations.
