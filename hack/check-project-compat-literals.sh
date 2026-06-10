#!/usr/bin/env bash
# Flags legacy grove literals outside known compatibility, test, fixture, and
# documentation surfaces. This starts broad by design; tighten the allowlist as
# projectcompat adoption expands.
set -euo pipefail

cd "$(dirname "$0")/.."

if ! command -v rg >/dev/null 2>&1; then
  echo "Error: ripgrep (rg) is required to run this script but was not found in PATH." >&2
  exit 1
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

rg -n 'grove|Grove|scion\.grove|grove_id|groveId|/groves' \
  cmd pkg extras \
  --glob '*.go' \
  --glob '!pkg/ent/**' >"$tmp" || true

if [[ ! -s "$tmp" ]]; then
  exit 0
fi

allowlist='(^pkg/projectcompat/)|(_test\.go:)|(^cmd/)|(^pkg/agent/)|(^pkg/api/)|(^pkg/brokerclient/)|(^pkg/config/)|(^pkg/hub/)|(^pkg/hubclient/)|(^pkg/hubsync/)|(^pkg/plugin/refbroker/)|(^pkg/runtime/)|(^pkg/runtimebroker/)|(^pkg/sciontool/)|(^pkg/storage/)|(^pkg/store/)|(^pkg/util/logging/)|(^pkg/wsprotocol/)|(^extras/)'

violations="$(grep -Ev "$allowlist" "$tmp" || true)"
if [[ -n "$violations" ]]; then
  echo "Legacy grove literals found outside the project compatibility allowlist:" >&2
  echo "$violations" >&2
  echo >&2
  echo "Use project vocabulary for new code, or route legacy handling through pkg/projectcompat." >&2
  exit 1
fi
