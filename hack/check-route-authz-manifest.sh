#!/usr/bin/env bash
# Verifies that every route registered in registerRoutes() (server.go) has a
# corresponding entry in the route-authz manifest (route_authz_manifest.go).
#
# A handler with no authorization at all — one that never calls authorize(),
# authorizeMsg(), requireAdmin, or any gating function — passes every other
# lint check silently, because there is no guard to key on. The authz-guards
# check (hack/check-authz-guards.sh, lines 55-63) explicitly acknowledges
# this gap and points to this script's parent issue.
#
# This check closes that gap: every registered route must appear in the
# manifest declaring its authorization posture. A route missing from the
# manifest is a VIOLATION — it means nobody has reviewed and classified how
# that endpoint is authorized.
#
# SEVERITY: Security-grade. A silently skipped run ships an unreviewed route.
#
# EXIT CODES
#
#   0  Analysed, no violations — every route is in the manifest.
#   1  Analysed, violations found — unlisted routes on stderr.
#   2  RESERVED — GNU make flattens all non-zero recipe exits to 2.
#   3  COULD NOT ANALYSE: required tool missing — nothing was examined.
#   4  COULD NOT ANALYSE: no routes found — wrong cwd or empty checkout.
#
# See hack/LINT-CONVENTIONS.md and ptone/scion#598.
set -euo pipefail

# ── Self-test ───────────────────────────────────────────────────────────────
if [[ "${1:-}" == "--self-test" ]]; then
  fixture_dir="$(mktemp -d)"
  trap 'rm -rf "$fixture_dir"' EXIT

  # Minimal fixture: a fake server.go with route registrations and a fake
  # manifest file with some entries.
  cat >"$fixture_dir/server.go" <<'SERVER'
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/api/v1/agents", s.handleAgents)
	s.mux.HandleFunc("/api/v1/agents/", s.handleAgentByID)
	s.mux.Handle("/api/v1/system/check", s.requireWorkstation(http.HandlerFunc(s.handleSystemCheck)))
	s.mux.HandleFunc("GET /.well-known/openid-configuration", s.handleOIDCDiscovery)
}
SERVER

  # Manifest covers /healthz and /api/v1/agents but NOT /api/v1/agents/
  # Also includes a stale entry /api/v1/stale that is not registered.
  cat >"$fixture_dir/manifest.go" <<'MANIFEST'
var routeAuthzManifest = map[string]string{
	"/healthz":                              "public",
	"/api/v1/agents":                        "authenticated",
	"/api/v1/system/check":                  "workstation",
	"GET /.well-known/openid-configuration": "oidc-public",
	"/api/v1/stale":                         "authenticated",
}
MANIFEST

  # Run extraction logic on fixtures
  server_file="$fixture_dir/server.go"
  manifest_file="$fixture_dir/manifest.go"

  # Extract registered routes from fixture server.go
  registered=$(grep -oP 's\.mux\.(HandleFunc|Handle)\(\s*"([^"]+)"' "$server_file" \
    | sed 's/s\.mux\.\(HandleFunc\|Handle\)(\s*"//;s/"$//' | sort -u)

  # Extract manifest routes from fixture manifest.go
  manifested=$(grep -oP '^\t"([^"]+)"' "$manifest_file" \
    | sed 's/^\t"//;s/"$//' | sort -u)

  # Compute missing (in registered but not manifest) and stale (in manifest but not registered)
  missing=$(comm -23 <(echo "$registered") <(echo "$manifested"))
  stale=$(comm -13 <(echo "$registered") <(echo "$manifested"))

  errors=0

  # Expect exactly one missing route: /api/v1/agents/
  if [[ "$missing" != "/api/v1/agents/" ]]; then
    echo "check-route-authz-manifest self-test: FAIL — expected missing route '/api/v1/agents/', got:" >&2
    echo "$missing" >&2
    errors=1
  fi

  # Expect exactly one stale route: /api/v1/stale
  if [[ "$stale" != "/api/v1/stale" ]]; then
    echo "check-route-authz-manifest self-test: FAIL — expected stale route '/api/v1/stale', got:" >&2
    echo "$stale" >&2
    errors=1
  fi

  if [[ "$errors" -eq 0 ]]; then
    echo "check-route-authz-manifest self-test: PASS (1 missing detected, 1 stale detected, 4 clean)"
    exit 0
  fi
  exit 1
fi

# ── Main script ─────────────────────────────────────────────────────────────
cd "$(dirname "$0")/.."

provenance() {
  local sha
  if ! sha="$(git rev-parse --short HEAD 2>/dev/null)"; then
    printf '(git unavailable or not a worktree)'
    return
  fi
  if [[ -n "$(git status --porcelain 2>/dev/null)" ]]; then
    printf '%s-dirty' "$sha"
  else
    printf '%s' "$sha"
  fi
}

# Dependency check — grep is always available, no exotic tools needed.
# (This script uses only grep, sed, sort, comm — all POSIX.)

server_file="pkg/hub/server.go"
manifest_file="pkg/hub/route_authz_manifest.go"

if [[ ! -f "$server_file" ]]; then
  msg="check-route-authz-manifest: $server_file not found — NOTHING WAS ANALYSED (wrong cwd or empty checkout)"
  echo "$msg"
  echo "$msg" >&2
  exit 4
fi

if [[ ! -f "$manifest_file" ]]; then
  msg="check-route-authz-manifest: $manifest_file not found — NOTHING WAS ANALYSED (manifest file missing)"
  echo "$msg"
  echo "$msg" >&2
  exit 4
fi

# ── Extract registered routes from server.go ────────────────────────────────
# Matches both s.mux.HandleFunc("path", ...) and s.mux.Handle("path", ...)
# including method-scoped patterns like "GET /path".
registered=$(grep -oP 's\.mux\.(HandleFunc|Handle)\(\s*"([^"]+)"' "$server_file" \
  | sed 's/s\.mux\.\(HandleFunc\|Handle\)(\s*"//;s/"$//' | sort -u)

if [[ -z "$registered" ]]; then
  msg="check-route-authz-manifest: analysed $(provenance) — no routes found in $server_file, NOTHING WAS ANALYSED"
  echo "$msg"
  echo "$msg" >&2
  exit 4
fi

route_count=$(echo "$registered" | wc -l)

# ── Extract manifest entries from route_authz_manifest.go ───────────────────
# Matches lines like:   "/healthz": "public", // comment
manifested=$(grep -oP '^\t"([^"]+)"' "$manifest_file" \
  | sed 's/^\t"//;s/"$//' | sort -u)

if [[ -z "$manifested" ]]; then
  msg="check-route-authz-manifest: analysed $(provenance) — no manifest entries found in $manifest_file, NOTHING WAS ANALYSED"
  echo "$msg"
  echo "$msg" >&2
  exit 4
fi

manifest_count=$(echo "$manifested" | wc -l)

# ── Compare ─────────────────────────────────────────────────────────────────
missing=$(comm -23 <(echo "$registered") <(echo "$manifested"))
stale=$(comm -13 <(echo "$registered") <(echo "$manifested"))

exit_code=0

if [[ -n "$stale" ]]; then
  stale_count=$(echo "$stale" | wc -l)
  echo "check-route-authz-manifest: WARNING — $stale_count stale manifest entries (route no longer registered):" >&2
  echo "$stale" | sed 's/^/  /' >&2
  echo >&2
fi

if [[ -n "$missing" ]]; then
  missing_count=$(echo "$missing" | wc -l)
  echo "check-route-authz-manifest: analysed $(provenance) — $missing_count routes without declared authorization posture" >&2
  echo >&2
  echo "Routes registered in registerRoutes() but missing from route_authz_manifest.go:" >&2
  echo "$missing" | sed 's/^/  /' >&2
  echo >&2
  echo "Add each route to pkg/hub/route_authz_manifest.go with its authorization" >&2
  echo "posture (public, auth-flow, authenticated, admin, workstation, broker-hmac," >&2
  echo "webhook, agent-token, oidc-public). See ptone/scion#598." >&2
  exit_code=1
fi

if [[ "$exit_code" -eq 0 ]]; then
  echo "check-route-authz-manifest: analysed $(provenance), $route_count routes registered, $manifest_count manifest entries, no violations"
fi

exit "$exit_code"
