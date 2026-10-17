#!/usr/bin/env bash
# Regression tests for scripts/starter-hub/merge-settings.py.
#
# gce-start-hub.sh --full regenerates settings.yaml from a template carrying
# only the keys the script manages, then installs it on the instance. Installing
# it over an existing file drops everything else -- broker plugin blocks under
# server.plugins, server.message_broker, a top-level plugins section -- with no
# warning, so the hub comes back up without them.
#
# Run: bash hack/test-merge-settings.sh
set -euo pipefail

cd "$(dirname "$0")/.."

MERGE="scripts/starter-hub/merge-settings.py"

if ! command -v python3 >/dev/null 2>&1 || ! python3 -c 'import yaml' >/dev/null 2>&1; then
    echo "SKIP: the merge helper needs python3 with PyYAML"
    exit 0
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

checks=0
failures=0

fail() {
    echo "FAIL: $1"
    failures=$((failures + 1))
}

# assert_contains <file> <literal> <description>
assert_contains() {
    if grep -qF -- "$2" "$1"; then
        checks=$((checks + 1))
    else
        fail "$3 (expected to find: $2)"
    fi
}

# assert_missing <file> <literal> <description>
assert_missing() {
    if grep -qF -- "$2" "$1"; then
        fail "$3 (should not contain: $2)"
    else
        checks=$((checks + 1))
    fi
}

# --- Fixtures -------------------------------------------------------------

# What the deploy script generates: only the keys it owns.
cat > "$WORK/generated.yaml" <<'EOF'
schema_version: "1"
active_profile: local
server:
  mode: production
telemetry:
  enabled: true
  cloud:
    provider: "gcp"
EOF

# What an operator had configured by hand on the instance.
cat > "$WORK/existing.yaml" <<'EOF'
active_profile: remote
server:
  mode: production
  message_broker:
    enabled: true
    backend: nats
  plugins:
    broker:
      telegram:
        db_path: /home/scion/.scion/telegram.db
plugins:
  custom:
    enabled: true
default_harness_config: claude
EOF

# --- Existing file: hand-added config survives, generated values win -------

cp "$WORK/generated.yaml" "$WORK/merged.yaml"
python3 "$MERGE" "$WORK/existing.yaml" "$WORK/merged.yaml" >/dev/null

assert_contains "$WORK/merged.yaml" "backend: nats" \
    "server.message_broker is preserved"
assert_contains "$WORK/merged.yaml" "db_path: /home/scion/.scion/telegram.db" \
    "server.plugins broker config is preserved"
assert_contains "$WORK/merged.yaml" "custom:" \
    "top-level plugins is preserved"
assert_contains "$WORK/merged.yaml" "default_harness_config: claude" \
    "unrelated operator keys are preserved"
assert_contains "$WORK/merged.yaml" "provider: gcp" \
    "generated telemetry keys are added"
assert_contains "$WORK/merged.yaml" "active_profile: local" \
    "the generated active_profile wins over the stored one"

# --- No existing file: nothing to merge, generated file left alone --------

cp "$WORK/generated.yaml" "$WORK/fresh.yaml"
python3 "$MERGE" "$WORK/nonexistent.yaml" "$WORK/fresh.yaml" >/dev/null

if cmp -s "$WORK/generated.yaml" "$WORK/fresh.yaml"; then
    checks=$((checks + 1))
else
    fail "a missing existing file must leave the generated file untouched"
fi

# --- Empty existing file: also nothing to merge ---------------------------

: > "$WORK/empty.yaml"
cp "$WORK/generated.yaml" "$WORK/fresh-empty.yaml"
python3 "$MERGE" "$WORK/empty.yaml" "$WORK/fresh-empty.yaml" >/dev/null

if cmp -s "$WORK/generated.yaml" "$WORK/fresh-empty.yaml"; then
    checks=$((checks + 1))
else
    fail "an empty existing file must leave the generated file untouched"
fi

# --- Unparseable existing file: caller must be able to detect it ----------

printf 'server: [unclosed\n' > "$WORK/broken.yaml"
cp "$WORK/generated.yaml" "$WORK/broken-out.yaml"

if python3 "$MERGE" "$WORK/broken.yaml" "$WORK/broken-out.yaml" >/dev/null 2>&1; then
    fail "an unparseable existing file must exit non-zero"
else
    checks=$((checks + 1))
fi

# --- Wrong argument count -------------------------------------------------

if python3 "$MERGE" "$WORK/existing.yaml" >/dev/null 2>&1; then
    fail "a missing argument must exit non-zero"
else
    checks=$((checks + 1))
fi

echo "merge-settings: ${checks} check(s) passed, ${failures} failed"
[ "$failures" -eq 0 ]
