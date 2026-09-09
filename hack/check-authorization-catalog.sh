#!/usr/bin/env bash
# Guard: authorization operation catalog must be structurally valid, permission
# coverage must be complete, and the generated catalog report must be up to date.
#
# This is the CI gate for AF1 (Authorization Audit Foundation). It runs ALL
# tests in pkg/hub/authzop/ — every test in the package is an AF1 gate test.
# Using full-package execution avoids brittle -run regex that can silently
# omit new gate tests.
#
# WHAT THIS CHECKS
#
# 1. Every OperationSpec in the Catalog passes deterministic validation.
# 2. No duplicate operation IDs or entry points exist.
# 3. Every entry point exemption is typed, scoped, owned, and rationalized.
# 4. No route is both cataloged and exempted.
# 5. Every operation's base permission exists in the permission registry.
# 6. Every registered permission is consumed or explicitly reserved/deferred.
# 7. Project membership operations declare peer_superior governance (CT1 appendix).
# 8. Test refs reference existing packages and functions.
# 9. The generated catalog report matches the checked-in version.
# 10. Proof tests demonstrate that unclassified/duplicate violations are caught.
# 11. Every route-metadata entry is covered by catalog or exemption.
# 12. Every mutation call site is classified (bidirectional).
# 13. Stale exemptions are detected.
# 14. Domain-resource semantic compatibility is assertive and fail-closed.
#
# EXIT CODES
#   0  all checks pass
#   1  one or more checks failed
#   2  could not run (build or environment failure)

set -euo pipefail

cd "$(dirname "$0")/.."

echo "=== Authorization catalog validation ==="
echo ""

# Run ALL tests in the authzop package. Every test in the package is an AF1
# gate test. Full-package execution is future-proof: new tests are automatically
# included without updating a -run regex.
if ! go test -tags no_sqlite -count=1 ./pkg/hub/authzop/ -v 2>&1; then
    echo ""
    echo "check-authorization-catalog: FAILED"
    exit 1
fi

echo ""
echo "check-authorization-catalog: all checks pass"
exit 0
