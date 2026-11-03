#!/usr/bin/env bash
# Guard: no agent-row mutation outside pkg/store/... (WP1 design 2.8).
#
# Only the store's writers run the lifecycle observer, so a direct ent
# agent mutation anywhere else would commit a transition with no
# agent.lifecycle event. The per-writer classification inside pkg/store is
# TestAgentWritersAreClassified (pkg/store/entadapter); this script runs
# the module-wide half (hack/checkagentmutators), which type-checks every
# package that imports pkg/ent. It is wired into `make ci-full` only,
# because the type-check includes pkg/hub.
#
# Exit codes: 0 pass, 1 findings, 2 could not analyse. Like
# check-security-marker-gates it builds the program instead of `go run`,
# which would collapse exit 2 into 1.

set -euo pipefail

cd "$(dirname "$0")/.." || exit 2

tmpbin=$(mktemp)
trap 'rm -f "$tmpbin"' EXIT

if ! go build -o "$tmpbin" ./hack/checkagentmutators 2>&1; then
  echo "ABORT: could not compile check-agent-mutators" >&2
  echo "  Nothing was analysed. This is a build failure, not a guard failure." >&2
  exit 2
fi

exec "$tmpbin" "$@"
