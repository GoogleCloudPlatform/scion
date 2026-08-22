#!/usr/bin/env bash
# Flags _ = os.Setenv(...) calls where the environment variable name contains a
# security-sensitive keyword (TOKEN, SECRET, KEY, AUTH). Silencing the error
# return from os.Setenv on a credential variable is a process-wide exposure risk
# — the value leaks into every child process's environment and can be inherited
# by any exec'd subprocess, and a silenced error means a failure to set the
# variable is never noticed.
#
# Severity: FORMATTING-GRADE
#   Missing rg:      exit 0 (silent skip)
#   No candidates:   exit 0
#   Violations found: exit 1
#   Clean:           exit 0
#
# See hack/LINT-CONVENTIONS.md for the conventions this script follows.
set -euo pipefail

cd "$(dirname "$0")/.."

# Provenance — report which tree was analysed.
sha="$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")"
if [[ "$sha" != "unknown" ]] && [[ -n "$(git status --porcelain 2>/dev/null)" ]]; then
  sha="${sha}-dirty"
fi

# Dependency check — formatting-grade: exit 0 if rg is missing.
if ! command -v rg >/dev/null 2>&1; then
  echo "Warning: ripgrep (rg) not found — skipping setenv-guard check" >&2
  exit 0
fi

# Pre-filter: find non-test Go files with _ = os.Setenv(.
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

rg -n '_ = os\.Setenv\(' \
  cmd pkg extras \
  --glob '*.go' \
  --glob '!**/*_test.go' >"$tmp" 2>/dev/null || true

if [[ ! -s "$tmp" ]]; then
  echo "check-setenv-guard: analysed ${sha}, no candidates found" >&2
  exit 0
fi

# Filter to lines where the env var name contains a security-sensitive keyword.
# The pattern matches the first string argument to os.Setenv.
candidates="$(grep -iE '_ = os\.Setenv\("[^"]*(TOKEN|SECRET|KEY|AUTH)' "$tmp" || true)"

if [[ -z "$candidates" ]]; then
  echo "check-setenv-guard: analysed ${sha}, no security-sensitive setenv calls" >&2
  exit 0
fi

# Allowlist — anchor on file path. Each entry has a mandatory comment.
allowed_paths=(
  # Dev server credential injection — sets tokens for the local dev loop.
  "^cmd/server_foreground.go:"

  # Credential helper — refreshes GITHUB_TOKEN for gh CLI subprocess calls.
  "^cmd/sciontool/commands/credential_helper.go:"

  # GH CLI wrapper — injects GH_TOKEN before exec'ing gh.
  "^cmd/sciontool/commands/gh_wrapper.go:"

  # Hub client — refreshes GITHUB_TOKEN for API calls.
  "^pkg/sciontool/hub/client.go:"
)

allowlist="$(printf '%s\n' "${allowed_paths[@]}" | paste -sd '|' -)"

violations="$(echo "$candidates" | grep -Ev "$allowlist" || true)"
if [[ -n "$violations" ]]; then
  echo "check-setenv-guard: analysed ${sha}" >&2
  echo >&2
  echo "Silenced os.Setenv of security-sensitive environment variables:" >&2
  echo "$violations" >&2
  echo >&2
  echo "Do not discard the error from os.Setenv for TOKEN/SECRET/KEY/AUTH vars." >&2
  echo "Either handle the error or use a helper that logs on failure." >&2
  exit 1
fi

echo "check-setenv-guard: analysed ${sha}, no violations" >&2
