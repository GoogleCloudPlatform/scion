#!/usr/bin/env bash
# TODO(P3): placeholder runner entrypoint; it exits non-zero on purpose.
#
# P3 implements (see cr-ci/README.md, "Phases", and design sections 1.2, 3.2, 3.3):
#   1. Create /scratch/{_work,_temp,gocache,gomodcache,golangci-lint-cache} on the
#      ephemeral-disk volume and export RUNNER_WORKSPACE, RUNNER_TEMP, TMPDIR,
#      GOCACHE, GOMODCACHE and GOLANGCI_LINT_CACHE to point there.
#   2. Exchange the one-time RUNNER_CLAIM_TOKEN with the mediator's claim
#      endpoint for the JIT runner config (never log it, never write it to disk).
#   3. exec ./run.sh --jitconfig "$JIT_CONFIG" as PID 1 (one job, then exit).
set -euo pipefail
echo "cr-ci runner entrypoint is a P1 placeholder; see cr-ci/runner/entrypoint.sh" >&2
exit 64
