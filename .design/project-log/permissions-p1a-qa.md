# Permissions Phase 1A QA Log

Date: 2026-08-22
Branch: `scion/permissions-p1a`
Developer commit: `137b7c13d8d85da1ce2e5fa1893ab621f74eb70c`
QA report: `/scion-volumes/scratchpad/projects/auth-refactor/reports/pf-1a-test1.md`
Verdict: `APPROVE`

## Scope

- Verified checkout branch/head and confirmed commit parent is Phase 0 baseline `feb3e188f147c52d760d3530710a1f72eb7062b7`.
- Inspected the Phase 1A permission registry, capability/UAT derivation, CLI scope help generation, web token-list surface, and registry drift tests.
- Re-ran focused registry/UAT/capability tests, broader `pkg/hub` tests, web typecheck, token-list prettier check, and web lint caveat reproduction.

## Results

- Focused registry/UAT/capability test run passed.
- `go test ./pkg/hub -timeout=600s -count=1` passed.
- `web` typecheck passed.
- `web` token-list prettier check passed.
- `web` lint still fails on repo-wide typed-lint/test-tsconfig/strict-rule debt; observed token-list lint findings are outside the edited scope-list entries.

## Notes

- Approved with a Medium follow-up: the web token scope list remains static and is only partially guarded by the Go drift test. A later phase should make the UI list registry-served or strengthen the drift test to assert exact exposed-scope equality.
