#!/usr/bin/env bash
# Flags new scion.io/ annotation keys introduced outside the existing constants
# and compatibility surfaces. The annotation prefix is migrating from scion.io/
# to scion.dev/ (see ptone/scion#1188). New code must use scion.dev/ keys; this
# check ensures no new scion.io/ references are added without explicit intent.
#
# Severity: FORMATTING-GRADE
#   - Missing rg: exit 3 (nothing analysed; see hack/lib/require-tool.sh)
#   - No candidates: exit 0
#   - Violations found: exit 1
#   - Clean: exit 0
#
# See hack/LINT-CONVENTIONS.md for conventions.
set -euo pipefail

cd "$(dirname "$0")/.."

# --- Provenance ---
sha="$(git rev-parse --short HEAD)"
if [[ -n "$(git status --porcelain 2>/dev/null)" ]]; then
  sha="${sha}-dirty"
fi

# --- Dependency check (exit 3 if missing, at every severity level) ---
# shellcheck source=SCRIPTDIR/lib/require-tool.sh
source hack/lib/require-tool.sh
require_tool rg check-annotation-prefix "ripgrep (rg)"

# --- Pre-filter: find candidate files ---
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

# The explicit "." matters: with no path argument, rg searches stdin whenever
# stdin is a pipe (as it is for a CI step), finds nothing, and the check
# passes vacuously. With "." rg prints paths as ./pkg/...; the sed strips that
# prefix so the anchored allowlist patterns below still match.
{ rg -n 'scion\.io/' . \
  --glob '*.go' \
  --glob '*.yaml' \
  --glob '*.yml' \
  --glob '*.tpl' \
  --glob '!*_test.go' \
  --glob '!pkg/ent/**' \
  --glob '!hack/**' \
  --glob '!docs/**' \
  --glob '!docs-site/**' \
  --glob '!docs-repo/**' \
  --glob '!reviews/**' \
  --glob '!web/**' \
  --glob '!scratch/**' || true; } | sed 's|^\./||' >"$tmp"

if [[ ! -s "$tmp" ]]; then
  echo "check-annotation-prefix: analysed ${sha}, no scion.io/ references found" >&2
  exit 0
fi

# --- Allowlist ---
# Every entry must have a comment explaining why it is allowed.
# Anchored on file path (relative to repo root), grouped by package.
allowed_paths=(
  # --- cmd/ ---
  # Embedded broker bootstrap: sets scion.io/system, scion.io/global, and
  # scion.io/broker-role labels on the built-in Global project broker.
  "^cmd/server_broker.go$"

  # Foreground server: sets scion.io/plugin label on plugin-backed brokers.
  "^cmd/server_foreground.go$"

  # Remote broker registration: sets scion.io/broker-role label.
  "^cmd/broker.go$"

  # --- pkg/store/ ---
  # Store interface: documents scion.io/template and scion.io/broker-role
  # label conventions used for filtering brokers and templates.
  "^pkg/store/store.go$"

  # Ent adapter: queries brokers by scion.io/broker-role label.
  "^pkg/store/entadapter/project_store.go$"

  # Model constants: defines LabelTemplate = "scion.io/template".
  "^pkg/store/models.go$"

  # --- pkg/hub/ ---
  # Broker auth: sets default scion.io/broker-type label on registration.
  "^pkg/hub/brokerauth.go$"

  # Runtime broker handlers: filters by scion.io/plugin label.
  "^pkg/hub/handlers_runtime_brokers.go$"

  # Agent creation helpers: references scion.io/default-harness-config in
  # design comment.
  "^pkg/hub/handlers_agent_create_helpers.go$"

  # Resolved project settings: documents annotation key format in comment.
  "^pkg/hub/project_settings_resolved.go$"

  # Project clone: skips scion.io/* system labels during project cloning.
  "^pkg/hub/project_clone.go$"

  # Harness config handlers: checks scion.io/plugin label on brokers.
  "^pkg/hub/harness_config_handlers.go$"

  # Project settings handlers: defines the authoritative list of scion.io/*
  # annotation keys (projectSettingDefaultTemplate, etc.). This is the
  # canonical registry of all project-level scion.io/ keys.
  "^pkg/hub/project_settings_handlers.go$"

  # --- pkg/hubclient/ ---
  # Client types: documents scion.io/ annotation key format in comment.
  "^pkg/hubclient/types.go$"

  # --- pkg/store/entadapter/ ---
  # Composite adapter: defines system annotation constants for project
  # members-group, agents-group, and adoption-review-required.
  "^pkg/store/entadapter/composite.go$"

  # --- pkg/hub/ (additional) ---
  # Core project handlers: defines system annotation constants for project
  # members-group and agents-group, including the legacy read-only
  # scion.io/system-project-members-group key (ptone/scion#2556).
  "^pkg/hub/handlers_projects_core.go$"

  # Passthrough gate: checks scion.io/broker-role label on embedded brokers.
  "^pkg/hub/passthrough_gate.go$"

  # Default GCP identity: documents that identity resolution does not trust
  # the scion.io/broker-role label, since broker labels are writable.
  "^pkg/hub/default_gcp_identity.go$"

  # --- pkg/brokerownership/ ---
  # Backfill attribution: defines LabelBrokerRole = "scion.io/broker-role" to
  # exclude operator-owned (embedded) brokers from CreatedBy attribution.
  "^pkg/brokerownership/backfill.go$"

  # --- deploy/helm/ ---
  # Helm chart templates and values: uses scion.io/hub-id pod annotation for
  # hub identity. Golden test files mirror the rendered output.
  "^deploy/helm/scion-hub/templates/deployment.yaml$"
  "^deploy/helm/scion-hub/values.yaml$"
  "^deploy/helm/scion-hub/golden/.*\.yaml$"
)

# Build a combined regex from the allowlist: join with | for grep -E.
# Each path pattern gets ':' appended to match the rg output format (file:line:match).
allowlist="$(printf '%s\n' "${allowed_paths[@]}" | sed 's/\$$/:/' | paste -sd '|' -)"

# --- Classify: filter out allowed files ---
violations="$(grep -Ev "$allowlist" "$tmp" || true)"

# --- Report ---
if [[ -n "$violations" ]]; then
  echo "check-annotation-prefix: analysed ${sha}, violations found" >&2
  echo "" >&2
  echo "New scion.io/ annotation keys found outside the allowlist:" >&2
  echo "$violations" >&2
  echo "" >&2
  echo "The annotation prefix is migrating from scion.io/ to scion.dev/ (see #1188)." >&2
  echo "Use scion.dev/ for new annotation keys, or add to the allowlist in" >&2
  echo "hack/check-annotation-prefix.sh with a comment if this is intentional." >&2
  exit 1
fi

count="$(wc -l < "$tmp" | tr -d ' ')"
echo "check-annotation-prefix: analysed ${sha}, ${count} scion.io/ references — all allowlisted" >&2
