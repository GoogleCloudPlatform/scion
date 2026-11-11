#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Regression tests for scripts/starter-hub/merge-settings.py.
#
# gce-start-hub.sh --full regenerates settings.yaml from a template carrying
# only the keys the script manages, then merges it over the existing file on
# the instance so operator-owned sections (server.plugins,
# server.message_broker, top-level plugins, ...) survive updates.
#
# Run: bash scripts/starter-hub/tests/test-merge-settings.sh
set -euo pipefail

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STARTER_DIR="$(cd "${TESTS_DIR}/.." && pwd)"
MERGE="${STARTER_DIR}/merge-settings.py"

if ! command -v python3 >/dev/null 2>&1 || ! python3 -c 'import yaml' >/dev/null 2>&1; then
    if [ "${ALLOW_SKIP_NO_PYYAML:-}" = "1" ] && [ "${CI:-}" != "true" ]; then
        echo "SKIP: merge-settings tests require python3 with PyYAML (ALLOW_SKIP_NO_PYYAML=1)"
        exit 0
    fi
    echo "FAIL: python3 with PyYAML is required (install python3-yaml, or set ALLOW_SKIP_NO_PYYAML=1 locally)" >&2
    exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

checks=0
failures=0

fail() {
    echo "FAIL: $1"
    failures=$((failures + 1))
}

pass() {
    checks=$((checks + 1))
}

assert_eq() {
    if [ "$1" = "$2" ]; then
        pass
    else
        fail "$3 (expected '$1', got '$2')"
    fi
}

# assert_contains <file> <literal> <description>
assert_contains() {
    if grep -qF -- "$2" "$1"; then
        pass
    else
        fail "$3 (expected to find: $2)"
    fi
}

# assert_missing <file> <literal> <description>
assert_missing() {
    if grep -qF -- "$2" "$1"; then
        fail "$3 (should not contain: $2)"
    else
        pass
    fi
}

file_mode() {
    python3 -c 'import os, sys; print(f"{os.stat(sys.argv[1]).st_mode & 0o777:04o}")' "$1"
}

assert_no_temp_files() {
    local leftover
    leftover="$(find "$1" -maxdepth 1 -name '.settings.yaml.*')"
    if [ -z "$leftover" ]; then
        pass
    else
        fail "$2 (leftover temp files: $leftover)"
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

cp "$WORK/generated.yaml" "$WORK/generated.orig.yaml"
cp "$WORK/existing.yaml" "$WORK/existing.orig.yaml"

# --- Existing file: hand-added config survives, generated values win -------

umask 0022
python3 "$MERGE" "$WORK/existing.yaml" "$WORK/generated.yaml" "$WORK/merged.yaml" >/dev/null

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
assert_eq "0600" "$(file_mode "$WORK/merged.yaml")" \
    "newly created output file has mode 0600"
assert_no_temp_files "$WORK" "no temp files left after normal merge"

if cmp -s "$WORK/generated.orig.yaml" "$WORK/generated.yaml" && cmp -s "$WORK/existing.orig.yaml" "$WORK/existing.yaml"; then
    pass
else
    fail "merge-settings.py must not modify its input files"
fi

# Overwriting a looser-mode target still leaves the file 0600.
printf 'old: true\n' > "$WORK/preexisting-0644.yaml"
chmod 0644 "$WORK/preexisting-0644.yaml"
python3 "$MERGE" "$WORK/existing.yaml" "$WORK/generated.yaml" "$WORK/preexisting-0644.yaml" >/dev/null
assert_eq "0600" "$(file_mode "$WORK/preexisting-0644.yaml")" \
    "overwritten output file has mode 0600"

# --- PRESERVE: existing values win even when generated sets those paths ----

cat > "$WORK/generated-with-preserve.yaml" <<'EOF'
schema_version: "1"
active_profile: local
server:
  mode: debug
  message_broker:
    enabled: false
    backend: memory
  plugins:
    broker:
      telegram:
        db_path: /generated/should-not-win.db
      generated_broker_plugin:
        enabled: true
plugins:
  custom:
    enabled: false
  generated_top_plugin:
    enabled: true
EOF

MERGE_STDOUT="$(python3 "$MERGE" "$WORK/existing.yaml" "$WORK/generated-with-preserve.yaml" "$WORK/preserve-conflict.yaml")"
assert_contains "$WORK/preserve-conflict.yaml" "backend: nats" \
    "PRESERVE keeps existing server.message_broker over generated"
assert_missing "$WORK/preserve-conflict.yaml" "backend: memory" \
    "PRESERVE drops generated server.message_broker when existing sets it"
assert_contains "$WORK/preserve-conflict.yaml" "db_path: /home/scion/.scion/telegram.db" \
    "PRESERVE keeps existing server.plugins over generated"
assert_missing "$WORK/preserve-conflict.yaml" "should-not-win.db" \
    "PRESERVE drops generated server.plugins value when existing sets it"
assert_missing "$WORK/preserve-conflict.yaml" "generated_broker_plugin" \
    "PRESERVE replaces server.plugins subtree wholesale from existing"
assert_missing "$WORK/preserve-conflict.yaml" "generated_top_plugin" \
    "PRESERVE replaces top-level plugins subtree wholesale from existing"
assert_contains "$WORK/preserve-conflict.yaml" "mode: debug" \
    "non-preserved server.mode still takes the generated value"
if [[ "$MERGE_STDOUT" == *"server.plugins"* && "$MERGE_STDOUT" == *"server.message_broker"* && "$MERGE_STDOUT" == *"plugins"* ]]; then
    pass
else
    fail "stdout reports preserved paths (got: $MERGE_STDOUT)"
fi

# --- PRESERVE: path absent from existing keeps generated value -------------

cat > "$WORK/existing-partial.yaml" <<'EOF'
active_profile: remote
server:
  mode: production
  plugins:
    broker:
      telegram:
        db_path: /home/scion/.scion/telegram.db
EOF

python3 "$MERGE" "$WORK/existing-partial.yaml" "$WORK/generated-with-preserve.yaml" "$WORK/preserve-absent.yaml" >/dev/null
assert_contains "$WORK/preserve-absent.yaml" "db_path: /home/scion/.scion/telegram.db" \
    "present preserved path (server.plugins) comes from existing"
assert_contains "$WORK/preserve-absent.yaml" "backend: memory" \
    "preserved path absent from existing (server.message_broker) keeps generated value"
assert_contains "$WORK/preserve-absent.yaml" "generated_top_plugin:" \
    "preserved path absent from existing (plugins) keeps generated value"

# --- No existing file: generated file written with mode 0600 ---------------

python3 "$MERGE" "$WORK/nonexistent.yaml" "$WORK/generated.yaml" "$WORK/fresh.yaml" >/dev/null

if cmp -s "$WORK/generated.yaml" "$WORK/fresh.yaml"; then
    pass
else
    fail "a missing existing file must write the generated content as-is"
fi
assert_eq "0600" "$(file_mode "$WORK/fresh.yaml")" \
    "fresh output file has mode 0600"

# --- Empty existing file: generated file written with mode 0600 ------------

: > "$WORK/empty.yaml"
python3 "$MERGE" "$WORK/empty.yaml" "$WORK/generated.yaml" "$WORK/fresh-empty.yaml" >/dev/null

if cmp -s "$WORK/generated.yaml" "$WORK/fresh-empty.yaml"; then
    pass
else
    fail "an empty existing file must write the generated content as-is"
fi
assert_eq "0600" "$(file_mode "$WORK/fresh-empty.yaml")" \
    "empty-existing output file has mode 0600"

# --- Unparseable existing file: non-zero exit, output untouched, no leak ---

printf 'secret_token: "TOP_SECRET_CREDENTIAL_42" : unexpected\nserver: [unclosed\n' > "$WORK/broken.yaml"
printf 'sentinel: keep-me\n' > "$WORK/preexisting-target.yaml"

if ERR_OUT="$(python3 "$MERGE" "$WORK/broken.yaml" "$WORK/generated.yaml" "$WORK/never-created.yaml" 2>&1)"; then
    fail "an unparseable existing file must exit non-zero"
else
    pass
fi
if [ ! -e "$WORK/never-created.yaml" ]; then
    pass
else
    fail "failed merge must not create output file"
fi
if [[ "$ERR_OUT" == *"line 1, column "* ]]; then
    pass
else
    fail "YAML parse error should report line and column (got: $ERR_OUT)"
fi
if [[ "$ERR_OUT" != *"TOP_SECRET_CREDENTIAL_42"* && "$ERR_OUT" != *"[unclosed"* ]]; then
    pass
else
    fail "YAML parse error must not echo source snippet (got: $ERR_OUT)"
fi

if python3 "$MERGE" "$WORK/broken.yaml" "$WORK/generated.yaml" "$WORK/preexisting-target.yaml" >/dev/null 2>&1; then
    fail "an unparseable existing file with pre-existing output must exit non-zero"
else
    pass
fi
assert_contains "$WORK/preexisting-target.yaml" "sentinel: keep-me" \
    "failed merge must not clobber pre-existing output file"
assert_no_temp_files "$WORK" "no temp files left after failed merge"

# --- Non-mapping existing file: non-zero exit, output not created ----------

printf -- '- item1\n- item2\n' > "$WORK/list.yaml"
if python3 "$MERGE" "$WORK/list.yaml" "$WORK/generated.yaml" "$WORK/list-out.yaml" >/dev/null 2>&1; then
    fail "a non-mapping existing file must exit non-zero"
else
    pass
fi
if [ ! -e "$WORK/list-out.yaml" ]; then
    pass
else
    fail "non-mapping existing file must not create output file"
fi

# --- Wrong argument count --------------------------------------------------

if python3 "$MERGE" "$WORK/existing.yaml" "$WORK/generated.yaml" >/dev/null 2>&1; then
    fail "two-argument invocation must exit non-zero"
else
    pass
fi

# --- Missing PyYAML fails in CI / by default -------------------------------

if [ -z "${_TESTING_NO_PYYAML:-}" ]; then
    mkdir -p "$WORK/nobin"
    cat > "$WORK/nobin/python3" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
    chmod +x "$WORK/nobin/python3"

    if env -u ALLOW_SKIP_NO_PYYAML _TESTING_NO_PYYAML=1 PATH="$WORK/nobin:$PATH" bash "$0" >/dev/null 2>&1; then
        fail "missing PyYAML must fail by default"
    else
        pass
    fi
    if env ALLOW_SKIP_NO_PYYAML=1 CI=true _TESTING_NO_PYYAML=1 PATH="$WORK/nobin:$PATH" bash "$0" >/dev/null 2>&1; then
        fail "missing PyYAML must fail when CI=true even if ALLOW_SKIP_NO_PYYAML=1"
    else
        pass
    fi
    if env ALLOW_SKIP_NO_PYYAML=1 CI=false _TESTING_NO_PYYAML=1 PATH="$WORK/nobin:$PATH" bash "$0" >/dev/null 2>&1; then
        pass
    else
        fail "ALLOW_SKIP_NO_PYYAML=1 outside CI should allow local skip"
    fi
fi

echo "merge-settings: ${checks} check(s) passed, ${failures} failed"
[ "$failures" -eq 0 ]
