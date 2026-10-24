# scripts/single-node-vm/tests/test_install_layout.sh -- covers the
# versioned install root on single-node VMs (design D1/D7/D9,
# ptone/scion#6009):
#   - scion-layout.sh run directly against a temporary root: layout
#     creation, atomic current/previous handling, adopt (migrate-layout)
#     idempotence, and the settings.yaml install_root/update_policy edit;
#   - deploy.sh's generated binary-install command (verify, extract,
#     install into the layout), executed against local fixtures, including
#     the per-asset checksum rules (a bogus SHA256SUMS self-entry is
#     ignored; an empty-file-hash entry is refused);
#   - the D7 update_policy default (notify) and --update-policy;
#   - `deploy.sh migrate-layout`: its gcloud calls, that it downloads
#     nothing, and its generated VM command run twice against a fake root.
#
# This file has no shebang -- it is always `source`d, never executed --
# so shellcheck needs an explicit shell directive to know its dialect.
# shellcheck shell=bash

DEPLOY_SH="${TIER_DIR}/deploy.sh"
LAYOUT_SH="${TIER_DIR}/scion-layout.sh"
HUB="layouthub"
LAYOUT_EMPTY_SHA256="e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

# =====================================================================
# Helpers
# =====================================================================

# layout_config_json HUB [UPDATE_POLICY] -- a minimal, valid deploy.sh
# config. UPDATE_POLICY is omitted from the JSON when empty, so the D7
# default path is exercised.
layout_config_json() {
  local hub="$1" policy="${2:-}" policy_line=""
  if [[ -n "$policy" ]]; then
    policy_line="\"update_policy\": \"${policy}\","
  fi
  cat <<EOF
{
  "hub_name": "${hub}",
  "project_id": "demo-project",
  "region": "us-central1",
  "machine_size": "small",
  "disk_size_gb": 200,
  "chat_plugins": [],
  "container_images": {"source": "build", "registry": "", "force_rebuild": false},
  "admin_email": "admin@example.com",
  ${policy_line}
  "release_channel": "nightly"
}
EOF
}

# _layout_repo_root / _layout_go -- the repository root (resolving the
# tier dir through symlinks) and the Go toolchain, for the hub-parser
# helpers in tests/lib.
_layout_repo_root() {
  (cd "$(dirname "$(readlink -f "${TIER_DIR}/deploy.sh")")/../.." && pwd)
}
_layout_go() {
  command -v go || { [[ -x /usr/local/go/bin/go ]] && echo /usr/local/go/bin/go; }
}

# LAYOUT_YAML_TO_JSON -- scion-layout.sh's settings parser for these tests:
# the hub's own koanf YAML parser (tests/lib/settings-yaml-to-json.go),
# which, like the hub, rejects duplicate keys. On the VM the default is
# python3 + PyYAML.
LAYOUT_YAML_TO_JSON="cd '$(_layout_repo_root)' && '$(_layout_go)' run -buildvcs=false scripts/single-node-vm/tests/lib/settings-yaml-to-json.go"

# _settings_json FILE -- FILE parsed by the hub's parser, as JSON (fails
# with the parser's error if it does not parse).
_settings_json() {
  bash -c "$LAYOUT_YAML_TO_JSON" < "$1"
}

# _go_written_yaml JSON -- JSON re-encoded as YAML by gopkg.in/yaml.v3, the
# encoder the hub uses when it rewrites settings.yaml (4-space indent).
_go_written_yaml() {
  printf '%s' "$1" | (cd "$(_layout_repo_root)" && "$(_layout_go)" run -buildvcs=false \
    scripts/single-node-vm/tests/lib/settings-json-to-yaml.go)
}

# _json_eq A B -- "true" when JSON texts A and B are equal as data.
_json_eq() {
  "$PYTHON" -c 'import json, sys; print("true" if json.loads(sys.argv[1]) == json.loads(sys.argv[2]) else "false")' "$1" "$2"
}

# _layout_new_host -- a fresh fake host under one mktemp dir: LH_ROOT (the
# install root, not yet created), LH_BIN (the CLI path's parent dir
# exists), LH_DIR (everything, for cleanup).
_layout_new_host() {
  LH_DIR="$(mktemp -d)"
  LH_ROOT="${LH_DIR}/opt/scion"
  mkdir -p "${LH_DIR}/usr/local/bin"
  LH_BIN="${LH_DIR}/usr/local/bin/scion"
}

# _layout CMD... -- runs scion-layout.sh against the fake host,
# unprivileged (no chown). Sets LAYOUT_RC and LAYOUT_OUT.
_layout() {
  LAYOUT_OUT="$(SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
    SCION_LAYOUT_YAML_TO_JSON="$LAYOUT_YAML_TO_JSON" bash "$LAYOUT_SH" "$@" 2>&1)"
  LAYOUT_RC=$?
}

# _fake_scion PATH VERSION [COMMIT] -- writes an executable that answers
# `version --format json` the way the real binary does (indented JSON).
_fake_scion() {
  local path="$1" ver="$2" commit="${3:-0123456789abcdef0123456789abcdef01234567}"
  cat > "$path" <<EOF
#!/bin/sh
if [ "\$1" = version ] && [ "\$2" = --format ] && [ "\$3" = json ]; then
  printf '{\n  "buildTime": "2026-10-23T00:00:00Z",\n  "commit": "%s",\n  "short": "x",\n  "version": "%s"\n}\n' '${commit}' '${ver}'
  exit 0
fi
echo "fake scion ${ver}"
EOF
  chmod 0755 "$path"
}

# _layout_state -- a canonical listing of the fake host's install root and
# CLI path: type, mode, symlink target and content hash of every entry.
# Two runs that leave the host in the same state print the same text.
_layout_state() {
  (
    cd "$LH_DIR" || exit 1
    find . -path ./work -prune -o -path ./settings.yaml -prune -o -print | LC_ALL=C sort | while IFS= read -r p; do
      if [[ -L "$p" ]]; then
        echo "$p link $(readlink "$p")"
      elif [[ -d "$p" ]]; then
        echo "$p dir $(stat -c %a "$p")"
      else
        echo "$p file $(stat -c %a "$p") $(sha256sum < "$p" | cut -c1-16)"
      fi
    done
  )
}

# =====================================================================
# scion-layout.sh install
# =====================================================================

test_layout_install_creates_contract_layout() {
  _layout_new_host
  _fake_scion "${LH_DIR}/scion-new" "nightly-20261023"
  _layout install nightly-20261023 "${LH_DIR}/scion-new"

  assert_eq "0" "$LAYOUT_RC" "install on a fresh host must succeed (output: ${LAYOUT_OUT})"
  assert_eq "755" "$(stat -c %a "$LH_ROOT")" "the install root must be 0755"
  assert_eq "755" "$(stat -c %a "$LH_ROOT/releases")" "releases/ must be 0755"
  assert_eq "755" "$(stat -c %a "$LH_ROOT/snapshots")" "snapshots/ must exist, 0755"
  assert_eq "755" "$(stat -c %a "$LH_ROOT/releases/nightly-20261023/scion")" "the release binary must be 0755"
  assert_true "$(cmp -s "${LH_DIR}/scion-new" "$LH_ROOT/releases/nightly-20261023/scion" && echo true || echo false)" \
    "releases/<tag>/scion must be the installed binary"
  assert_eq "releases/nightly-20261023" "$(readlink "$LH_ROOT/current")" \
    "current must be a relative symlink to releases/<tag>"
  assert_false "$([[ -e "$LH_ROOT/previous" || -L "$LH_ROOT/previous" ]] && echo true || echo false)" \
    "a first install has no previous release"
  assert_eq "$LH_ROOT/current/scion" "$(readlink "$LH_BIN")" \
    "the CLI path must be a symlink to <root>/current/scion"
  assert_contains "$("$LH_BIN")" "fake scion nightly-20261023" "the CLI path must run the current release"
  assert_false "$([[ -e "$LH_ROOT/releases/nightly-20261023.partial" ]] && echo true || echo false)" \
    "no .partial staging dir may be left behind"
  rm -rf "$LH_DIR"
}

test_layout_install_second_tag_moves_current_to_previous() {
  _layout_new_host
  _fake_scion "${LH_DIR}/a" "nightly-20261022"
  _fake_scion "${LH_DIR}/b" "nightly-20261023"
  _layout install nightly-20261022 "${LH_DIR}/a"
  _layout install nightly-20261023 "${LH_DIR}/b"

  assert_eq "0" "$LAYOUT_RC" "the second install must succeed (output: ${LAYOUT_OUT})"
  assert_eq "releases/nightly-20261023" "$(readlink "$LH_ROOT/current")" "current must move to the new release"
  assert_eq "releases/nightly-20261022" "$(readlink "$LH_ROOT/previous")" \
    "previous must point at the release current pointed at before"
  assert_true "$([[ -L "$LH_ROOT/current" && -L "$LH_ROOT/previous" ]] && echo true || echo false)" \
    "current and previous must stay symlinks (renamed over, never replaced by a directory)"
  assert_false "$([[ -e "$LH_ROOT/current.new" || -L "$LH_ROOT/current.new" || -L "$LH_ROOT/previous.new" ]] && echo true || echo false)" \
    "the temporary link used for the atomic rename must not be left behind"
  assert_true "$([[ -f "$LH_ROOT/releases/nightly-20261022/scion" ]] && echo true || echo false)" \
    "the previous release must be retained for rollback"

  # A third install: previous follows current again.
  _fake_scion "${LH_DIR}/c" "nightly-20261024"
  _layout install nightly-20261024 "${LH_DIR}/c"
  assert_eq "releases/nightly-20261023" "$(readlink "$LH_ROOT/previous")" "previous must track the last current"
  rm -rf "$LH_DIR"
}

test_layout_install_same_tag_twice_is_idempotent() {
  _layout_new_host
  _fake_scion "${LH_DIR}/a" "nightly-20261022"
  _fake_scion "${LH_DIR}/b" "nightly-20261023"
  _layout install nightly-20261022 "${LH_DIR}/a"
  _layout install nightly-20261023 "${LH_DIR}/b"
  local before after
  before="$(_layout_state)"
  _layout install nightly-20261023 "${LH_DIR}/b"
  after="$(_layout_state)"

  assert_eq "0" "$LAYOUT_RC" "re-installing the current tag must succeed"
  assert_eq "$before" "$after" "re-installing the current tag must leave the layout unchanged (previous kept)"
  rm -rf "$LH_DIR"
}

test_layout_install_over_old_layout_keeps_old_binary_as_previous() {
  _layout_new_host
  _fake_scion "$LH_BIN" "nightly-20261001"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  _layout install nightly-20261023 "${LH_DIR}/new"

  assert_eq "0" "$LAYOUT_RC" "install over an old-layout host must succeed (output: ${LAYOUT_OUT})"
  assert_eq "releases/nightly-20261023" "$(readlink "$LH_ROOT/current")" "current must be the new release"
  assert_eq "releases/nightly-20261001" "$(readlink "$LH_ROOT/previous")" \
    "the plain binary being replaced must be kept as the previous release"
  assert_eq "$LH_ROOT/current/scion" "$(readlink "$LH_BIN")" "the old plain binary must be replaced by the symlink"
  rm -rf "$LH_DIR"
}

test_layout_install_rejects_unsafe_tags() {
  local tag
  for tag in "../evil" "a/b" "-x" ".hidden" "nightly.partial" ""; do
    _layout_new_host
    _fake_scion "${LH_DIR}/new" "x"
    _layout install "$tag" "${LH_DIR}/new"
    assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "install must refuse tag '${tag}'"
    assert_false "$([[ -e "$LH_ROOT" ]] && echo true || echo false)" \
      "a refused tag '${tag}' must not create anything"
    rm -rf "$LH_DIR"
  done
}

test_layout_install_refuses_non_symlink_current() {
  _layout_new_host
  mkdir -p "$LH_ROOT/current"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  _layout install nightly-20261023 "${LH_DIR}/new"
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" \
    "install must refuse to replace a current that is not a symlink"
  assert_contains "$LAYOUT_OUT" "not a symlink" "the refusal must say why"
  rm -rf "$LH_DIR"
}

# =====================================================================
# scion-layout.sh adopt (what migrate-layout runs on the VM)
# =====================================================================

test_layout_adopt_converts_old_layout_without_version_change() {
  _layout_new_host
  _fake_scion "$LH_BIN" "v0.3.0-preview.4"
  cp "$LH_BIN" "${LH_DIR}/orig"
  _layout adopt

  assert_eq "0" "$LAYOUT_RC" "adopt on an old-layout host must succeed (output: ${LAYOUT_OUT})"
  assert_true "$(cmp -s "${LH_DIR}/orig" "$LH_ROOT/releases/v0.3.0-preview.4/scion" && echo true || echo false)" \
    "the installed binary must be moved into releases/<its own version> unchanged"
  assert_eq "releases/v0.3.0-preview.4" "$(readlink "$LH_ROOT/current")" "current must point at the adopted release"
  assert_eq "$LH_ROOT/current/scion" "$(readlink "$LH_BIN")" "the CLI path must become the symlink"
  assert_contains "$("$LH_BIN")" "fake scion v0.3.0-preview.4" "the same version must still run"
  assert_true "$([[ -d "$LH_ROOT/snapshots" ]] && echo true || echo false)" "adopt must create snapshots/"
  rm -rf "$LH_DIR"
}

test_layout_adopt_twice_is_a_noop() {
  _layout_new_host
  _fake_scion "$LH_BIN" "nightly-20261023"
  _layout adopt
  local first
  first="$(_layout_state)"
  _layout adopt
  assert_eq "0" "$LAYOUT_RC" "a second adopt must succeed"
  assert_contains "$LAYOUT_OUT" "already converted" "a second adopt must report the host as converted"
  assert_eq "$first" "$(_layout_state)" "a second adopt must leave the host exactly as the first left it"
  rm -rf "$LH_DIR"
}

test_layout_adopt_follows_existing_symlink() {
  _layout_new_host
  mkdir -p "${LH_DIR}/usr/local/lib"
  _fake_scion "${LH_DIR}/usr/local/lib/scion-real" "nightly-20261020"
  ln -s "${LH_DIR}/usr/local/lib/scion-real" "$LH_BIN"
  _layout adopt
  assert_eq "0" "$LAYOUT_RC" "adopt must handle a CLI path that is already a symlink elsewhere (output: ${LAYOUT_OUT})"
  assert_eq "releases/nightly-20261020" "$(readlink "$LH_ROOT/current")" "the symlink's target must be adopted"
  assert_eq "$LH_ROOT/current/scion" "$(readlink "$LH_BIN")" "the CLI path must be re-pointed at current"
  rm -rf "$LH_DIR"
}

test_layout_adopt_build_without_version_uses_git_commit() {
  _layout_new_host
  _fake_scion "$LH_BIN" "" "abcdef0123456789abcdef0123456789abcdef01"
  _layout adopt
  assert_eq "0" "$LAYOUT_RC" "adopt must accept a build with no release version (output: ${LAYOUT_OUT})"
  assert_eq "releases/git-abcdef012345" "$(readlink "$LH_ROOT/current")" \
    "a build with no release version must be adopted as git-<sha12>"
  rm -rf "$LH_DIR"
}

test_layout_adopt_unknown_version_fails_and_changes_nothing() {
  _layout_new_host
  printf '#!/bin/sh\necho not-json\n' > "$LH_BIN"
  chmod 0755 "$LH_BIN"
  _layout adopt
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" \
    "adopt must fail when the installed binary's version cannot be read"
  assert_contains "$LAYOUT_OUT" "--version" "the failure must point at a full deploy.sh run"
  assert_false "$([[ -L "$LH_BIN" ]] && echo true || echo false)" "the CLI binary must be left in place"
  assert_false "$([[ -e "$LH_ROOT/current" ]] && echo true || echo false)" "no current link may be created"
  rm -rf "$LH_DIR"
}

# =====================================================================
# scion-layout.sh set-maintenance (D7 + install_root)
# =====================================================================

# _deploy_rendered_settings POLICY -- settings.yaml shaped exactly like
# deploy.sh's own rendering before this change (no install_root).
_deploy_rendered_settings() {
  cat <<EOF
schema_version: "1"
# a comment that must survive
default_harness_config: antigravity
server:
  hub:
    hub_name: "demo"
  maintenance:
    deployment_tier: "binary"
    release_channel: "nightly"
    update_policy: "$1"
  storage:
    local_path: /home/scion/.scion/workspace-storage
  auth:
    mode: proxy
  # Listen port: set by --web-port in scion-hub.service, not here.
EOF
}

test_layout_settings_default_rewrites_auto_to_notify() {
  _layout_new_host
  _deploy_rendered_settings auto > "${LH_DIR}/settings.yaml"
  chmod 0640 "${LH_DIR}/settings.yaml"
  _layout set-maintenance "${LH_DIR}/settings.yaml" default
  local got expected
  got="$(cat "${LH_DIR}/settings.yaml")"
  expected="$(_deploy_rendered_settings notify | sed "s|^    update_policy: \"notify\"\$|&\n    install_root: \"${LH_ROOT}\"|")"
  assert_eq "0" "$LAYOUT_RC" "set-maintenance must succeed (output: ${LAYOUT_OUT})"
  assert_eq "$expected" "$got" \
    "default must rewrite auto to notify, add install_root, and leave every other line untouched"
  assert_eq "640" "$(stat -c %a "${LH_DIR}/settings.yaml")" "the file mode must be preserved"
  assert_eq "notify" "$(_settings_json "${LH_DIR}/settings.yaml" | "$PYTHON" -c 'import json,sys; print(json.load(sys.stdin)["server"]["maintenance"]["update_policy"])')" \
    "the result must parse with the hub's own YAML parser"

  _layout set-maintenance "${LH_DIR}/settings.yaml" default
  assert_eq "$got" "$(cat "${LH_DIR}/settings.yaml")" "a second run must not change the file"
  assert_contains "$LAYOUT_OUT" "already up to date" "a second run must report no change"
  rm -rf "$LH_DIR"
}

test_layout_settings_explicit_auto_is_kept() {
  _layout_new_host
  _deploy_rendered_settings auto > "${LH_DIR}/settings.yaml"
  _layout set-maintenance "${LH_DIR}/settings.yaml" auto
  assert_contains "$(cat "${LH_DIR}/settings.yaml")" '    update_policy: "auto"' "an explicit auto must be kept"
  assert_not_contains "$(cat "${LH_DIR}/settings.yaml")" 'notify' "an explicit auto must not become notify"
  rm -rf "$LH_DIR"
}

test_layout_settings_default_keeps_notify_and_disabled() {
  local p
  for p in notify disabled; do
    _layout_new_host
    _deploy_rendered_settings "$p" > "${LH_DIR}/settings.yaml"
    _layout set-maintenance "${LH_DIR}/settings.yaml" default
    assert_contains "$(cat "${LH_DIR}/settings.yaml")" "    update_policy: \"${p}\"" \
      "default must keep an existing ${p}"
    rm -rf "$LH_DIR"
  done
}

test_layout_settings_explicit_policy_overrides() {
  _layout_new_host
  _deploy_rendered_settings auto > "${LH_DIR}/settings.yaml"
  _layout set-maintenance "${LH_DIR}/settings.yaml" disabled
  assert_contains "$(cat "${LH_DIR}/settings.yaml")" '    update_policy: "disabled"' "an explicit policy must be written"
  rm -rf "$LH_DIR"
}

# The admin server-config page rewrites settings.yaml with Go's YAML
# encoder (4-space indent, unquoted values) and may already carry an
# install_root: the edit must follow the file's own indentation and update
# rather than duplicate keys.
test_layout_settings_handles_four_space_indent_and_existing_root() {
  _layout_new_host
  cat > "${LH_DIR}/settings.yaml" <<'EOF'
schema_version: "1"
server:
    hub:
        hub_name: demo
    maintenance:
        deployment_tier: binary
        install_root: /somewhere/else
        update_policy: auto
    secrets:
        backend: local
EOF
  _layout set-maintenance "${LH_DIR}/settings.yaml" default
  local got
  got="$(cat "${LH_DIR}/settings.yaml")"
  assert_contains "$got" "        install_root: \"${LH_ROOT}\"" "install_root must be updated in place at the file's indent"
  assert_contains "$got" '        update_policy: "notify"' "auto must be rewritten at the file's indent"
  assert_eq "1" "$(grep -c 'install_root:' <<< "$got")" "install_root must not be duplicated"
  assert_contains "$got" $'    secrets:\n        backend: local' "following blocks must be untouched"
  rm -rf "$LH_DIR"
}

test_layout_settings_adds_missing_keys_and_block() {
  _layout_new_host
  cat > "${LH_DIR}/settings.yaml" <<'EOF'
schema_version: "1"
server:
  hub:
    hub_name: "demo"
image_registry: "x"
EOF
  _layout set-maintenance "${LH_DIR}/settings.yaml" default
  local got
  got="$(cat "${LH_DIR}/settings.yaml")"
  assert_eq "0" "$LAYOUT_RC" "a file with no maintenance block must still be updated (output: ${LAYOUT_OUT})"
  assert_contains "$got" $'    hub_name: "demo"\n  maintenance:\n    install_root: "'"${LH_ROOT}"$'"\n    update_policy: "notify"\nimage_registry: "x"' \
    "a missing maintenance block must be created inside server: with install_root and the notify default"
  rm -rf "$LH_DIR"

  _layout_new_host
  cat > "${LH_DIR}/settings.yaml" <<'EOF'
server:
  maintenance:
    deployment_tier: "binary"
EOF
  _layout set-maintenance "${LH_DIR}/settings.yaml" default
  assert_contains "$(cat "${LH_DIR}/settings.yaml")" $'    deployment_tier: "binary"\n    install_root: "'"${LH_ROOT}"$'"\n    update_policy: "notify"' \
    "missing keys at the end of the file must be appended inside the maintenance block"
  rm -rf "$LH_DIR"
}

test_layout_settings_without_server_block_fails_unchanged() {
  _layout_new_host
  printf 'schema_version: "1"\nserver: {hub: {hub_name: x}}\n' > "${LH_DIR}/settings.yaml"
  local before
  before="$(cat "${LH_DIR}/settings.yaml")"
  _layout set-maintenance "${LH_DIR}/settings.yaml" default
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" \
    "set-maintenance must fail on a server: block it cannot edit line by line"
  assert_eq "$before" "$(cat "${LH_DIR}/settings.yaml")" "the file must be left unchanged"
  assert_false "$(ls "${LH_DIR}"/settings.yaml.* >/dev/null 2>&1 && echo true || echo false)" "no temp file may be left behind"
  rm -rf "$LH_DIR"
}

# =====================================================================
# deploy.sh create path
# =====================================================================

# Same background-run pattern as test_release_checksum.sh's
# run_deploy_create_through_phase3 (each test file keeps its own copy --
# see README.md "How it works"): stops at the settings.yaml dev-mode write,
# which comes after the binary install command. EXTRA_ARGS go after
# --config.
# LAYOUT_DEPLOY_VERSION picks the --version value (default v1.0.0-test);
# "none" omits --version entirely.
_layout_start_deploy_bg() {
  local config_file="$1" log_file="$2" version="${LAYOUT_DEPLOY_VERSION:-v1.0.0-test}"
  shift 2
  local version_args=(--version "$version")
  [[ "$version" == "none" ]] && version_args=()
  set -m
  bash "$DEPLOY_SH" --config "$config_file" "${version_args[@]}" "$@" \
    < /dev/null > "$log_file" 2>&1 &
  _LAYOUT_DEPLOY_PID=$!
  set +m
}

_layout_stop_deploy_bg() {
  local pid="$1" waited_ms=0
  kill -TERM -- "-${pid}" 2>/dev/null || true
  wait "$pid" 2>/dev/null
  while kill -0 -- "-${pid}" 2>/dev/null && [[ "$waited_ms" -lt 5000 ]]; do
    sleep 0.05
    waited_ms=$((waited_ms + 50))
  done
}

# layout_deploy_through_settings CONFIG_JSON [EXTRA_ARGS...] -- sets
# DEPLOY_LOG, LAYOUT_CURL_LOG (deploy.sh's own local curl calls) and
# LAYOUT_REACHED_SETTINGS (true/false).
layout_deploy_through_settings() {
  local config_json="$1" config_file log_file pid waited_ms=0
  shift
  config_file="$(mktemp)"
  log_file="$(mktemp)"
  local curl_log
  curl_log="$(mktemp)"
  export CURL_STUB_LOG="$curl_log"
  printf '%s' "$config_json" > "$config_file"
  local sentinel="${GCLOUD_STUB_STATE_DIR}/settings-yaml-dev-mode-written"
  export GCLOUD_STUB_SSH_SUCCEEDS=true
  _layout_start_deploy_bg "$config_file" "$log_file" "$@"
  pid="$_LAYOUT_DEPLOY_PID"
  while [[ ! -f "$sentinel" && "$waited_ms" -lt 30000 ]]; do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
    waited_ms=$((waited_ms + 100))
  done
  LAYOUT_REACHED_SETTINGS=false
  [[ -f "$sentinel" ]] && LAYOUT_REACHED_SETTINGS=true
  _layout_stop_deploy_bg "$pid"
  DEPLOY_LOG="$(cat "$log_file")"
  LAYOUT_CURL_LOG="$(cat "$curl_log")"
  rm -f "$config_file" "$log_file" "$curl_log"
  unset GCLOUD_STUB_SSH_SUCCEEDS CURL_STUB_LOG
}

# _layout_binary_install_cmd LOG -- the binary install command's body,
# from "Downloading scion binary..." through "Installed scion binary",
# verbatim (the embedded scion-layout.sh heredoc must stay byte-for-byte).
_layout_binary_install_cmd() {
  printf '%s\n' "$1" | awk '
    /Downloading scion binary\.\.\./ { on = 1 }
    on { print }
    /Installed scion binary/ { if (on) exit }
  '
}

# _layout_settings_policy LOG -- the update_policy value of the first
# settings.yaml write in LOG.
_layout_settings_policy() {
  printf '%s\n' "$1" | awk "/<< 'SETTINGSEOF'/{f=1; next} /^SETTINGSEOF\$/{exit} f" \
    | sed -n 's/^    update_policy: "\(.*\)"$/\1/p'
}

test_layout_deploy_installs_binary_into_layout() {
  fresh_gcloud_state
  layout_deploy_through_settings "$(layout_config_json "$HUB" auto)"
  assert_eq "true" "$LAYOUT_REACHED_SETTINGS" "deploy must reach the settings.yaml write (log: ${DEPLOY_LOG: -400})"
  local cmd
  cmd="$(_layout_binary_install_cmd "$(gcloud_log)")"
  assert_contains "$cmd" "SCIONLAYOUTEOF" "the binary install must carry scion-layout.sh"
  # shellcheck disable=SC2016 # a literal from the generated command
  assert_contains "$cmd" 'sudo bash "$SCION_LAYOUT_SH" install v1.0.0-test /tmp/scion' \
    "the verified binary must be installed as releases/<VERSION> through scion-layout.sh"
  assert_not_contains "$cmd" "sudo mv /tmp/scion /usr/local/bin/scion" \
    "the binary must no longer be moved over /usr/local/bin/scion"
  assert_contains "$(gcloud_log)" '    install_root: "/opt/scion"' \
    "the settings.yaml write must carry server.maintenance.install_root"
}

test_layout_unit_template_runs_current_symlink() {
  local unit
  unit="$(cat "${TIER_DIR}/config-templates/scion-hub.service")"
  assert_contains "$unit" $'\nExecStart=/opt/scion/current/scion server start ' \
    "the unit must run the binary through /opt/scion/current"
  assert_not_contains "$unit" "/usr/local/bin/scion" "the unit must not run /usr/local/bin/scion"
  assert_not_contains "$unit" "OnFailure" "no boot-failure guard in this phase"
  assert_not_contains "$unit" "StartLimit" "no start-limit settings in this phase"
}

# _layout_exec_install_cmd CMD WORKDIR -- runs deploy.sh's generated binary
# install command against local fixtures: /tmp is rewritten to WORKDIR,
# `sudo` is dropped, and the layout goes to the fake host (LH_*). curl
# resolves to tests/lib/curl, so CURL_STUB_FIXTURE_DIR serves downloads.
_layout_exec_install_cmd() {
  local cmd="$1" workdir="$2" rewritten
  rewritten="${cmd//\/tmp/${workdir}}"
  rewritten="${rewritten//sudo bash/bash}"
  SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
    bash -c "set -euo pipefail
$rewritten" 2>&1
}

_layout_make_tarball() {
  local dir="$1" content="$2" work
  work="$(mktemp -d)"
  printf '%s' "$content" > "${work}/scion"
  tar -czf "${dir}/scion-linux-amd64.tar.gz" -C "$work" scion
  rm -rf "$work"
}

test_layout_deploy_install_cmd_end_to_end_with_bogus_self_entry() {
  fresh_gcloud_state
  layout_deploy_through_settings "$(layout_config_json "$HUB" auto)"
  local cmd fixdir work out rc
  cmd="$(_layout_binary_install_cmd "$(gcloud_log)")"
  fixdir="$(mktemp -d)"
  work="$(mktemp -d)"
  _layout_new_host
  _layout_make_tarball "$fixdir" "real-scion-binary-payload"
  # A bogus line for SHA256SUMS itself (a self-entry can never be right)
  # next to the asset's correct line: only the asset's own line counts.
  {
    echo "0000000000000000000000000000000000000000000000000000000000000000  SHA256SUMS"
    (cd "$fixdir" && sha256sum scion-linux-amd64.tar.gz)
  } > "${fixdir}/SHA256SUMS.tmp"
  mv "${fixdir}/SHA256SUMS.tmp" "${fixdir}/SHA256SUMS"

  out="$(CURL_STUB_FIXTURE_DIR="$fixdir" _layout_exec_install_cmd "$cmd" "$work")"
  rc=$?
  assert_eq "0" "$rc" "a correct asset line must verify despite a bogus self-entry (output: ${out})"
  assert_eq "real-scion-binary-payload" "$(cat "$LH_ROOT/releases/v1.0.0-test/scion" 2>/dev/null || echo '<missing>')" \
    "the verified payload must land in releases/<VERSION>/scion"
  assert_eq "releases/v1.0.0-test" "$(readlink "$LH_ROOT/current")" "current must point at the installed release"
  assert_eq "$LH_ROOT/current/scion" "$(readlink "$LH_BIN")" "the CLI path must be the symlink"
  assert_false "$([[ -e "${work}/scion" ]] && echo true || echo false)" "the extracted temp binary must be cleaned up"
  rm -rf "$fixdir" "$work" "$LH_DIR"
}

test_layout_deploy_install_cmd_refuses_empty_file_hash() {
  fresh_gcloud_state
  layout_deploy_through_settings "$(layout_config_json "$HUB" auto)"
  local cmd fixdir work out rc
  cmd="$(_layout_binary_install_cmd "$(gcloud_log)")"
  fixdir="$(mktemp -d)"
  work="$(mktemp -d)"
  _layout_new_host
  # An empty asset whose entry is the empty-file hash: `sha256sum -c`
  # alone would report OK, so this must be refused before that.
  : > "${fixdir}/scion-linux-amd64.tar.gz"
  echo "${LAYOUT_EMPTY_SHA256}  scion-linux-amd64.tar.gz" > "${fixdir}/SHA256SUMS"

  out="$(CURL_STUB_FIXTURE_DIR="$fixdir" _layout_exec_install_cmd "$cmd" "$work")"
  rc=$?
  assert_true "$([[ "$rc" -ne 0 ]] && echo true || echo false)" "an empty-file-hash entry must fail the install"
  assert_contains "$out" "empty-file hash" "the failure must name the empty-file hash"
  assert_not_contains "$out" ": OK" "the entry must be refused before sha256sum -c ever runs"
  assert_false "$([[ -e "$LH_ROOT" ]] && echo true || echo false)" "nothing may be installed"

  # Even ALLOW_UNVERIFIED_RELEASE=true must not let it through.
  fresh_gcloud_state
  export ALLOW_UNVERIFIED_RELEASE=true
  layout_deploy_through_settings "$(layout_config_json "$HUB" auto)"
  unset ALLOW_UNVERIFIED_RELEASE
  cmd="$(_layout_binary_install_cmd "$(gcloud_log)")"
  out="$(CURL_STUB_FIXTURE_DIR="$fixdir" _layout_exec_install_cmd "$cmd" "$work")"
  rc=$?
  assert_true "$([[ "$rc" -ne 0 ]] && echo true || echo false)" \
    "ALLOW_UNVERIFIED_RELEASE=true must not bypass the empty-file-hash refusal"
  rm -rf "$fixdir" "$work" "$LH_DIR"
}

test_layout_deploy_plugin_install_refuses_empty_file_hash() {
  fresh_gcloud_state
  layout_deploy_through_settings "$(layout_config_json "$HUB" auto | sed 's/"chat_plugins": \[\]/"chat_plugins": ["telegram"]/')"
  assert_contains "$(gcloud_log)" "^${LAYOUT_EMPTY_SHA256} [ *]scion-plugin-telegram-linux-amd64\\.tar\\.gz" \
    "the plugin install must check for the empty-file hash on the plugin's own line"
}

# =====================================================================
# D7: update_policy default and --update-policy
# =====================================================================

test_layout_d7_default_policy_is_notify() {
  fresh_gcloud_state
  layout_deploy_through_settings "$(layout_config_json "$HUB")"
  assert_eq "true" "$LAYOUT_REACHED_SETTINGS" "deploy must reach the settings.yaml write"
  assert_eq "notify" "$(_layout_settings_policy "$(gcloud_log)")" \
    "with no update_policy in config or flags, the default must be notify"
  assert_contains "$(cat "$DEPLOY_SH")" 'config_prompt UPDATE_CHOICE "Select [2]: " "2"' \
    "the interactive menu must default to notify (choice 2)"
}

test_layout_d7_explicit_auto_is_honoured() {
  fresh_gcloud_state
  layout_deploy_through_settings "$(layout_config_json "$HUB" auto)"
  assert_eq "auto" "$(_layout_settings_policy "$(gcloud_log)")" "config update_policy auto must be honoured"

  fresh_gcloud_state
  layout_deploy_through_settings "$(layout_config_json "$HUB")" --update-policy auto
  assert_eq "auto" "$(_layout_settings_policy "$(gcloud_log)")" "--update-policy auto must be honoured"
}

test_layout_d7_flag_overrides_config() {
  fresh_gcloud_state
  layout_deploy_through_settings "$(layout_config_json "$HUB" auto)" --update-policy disabled
  assert_eq "disabled" "$(_layout_settings_policy "$(gcloud_log)")" \
    "--update-policy must override the config file's update_policy"
}

test_layout_d7_invalid_flag_fails_before_any_call() {
  fresh_gcloud_state
  local config_file
  config_file="$(mktemp)"
  layout_config_json "$HUB" > "$config_file"
  run_expect_fail bash "$DEPLOY_SH" --config "$config_file" --version v1.0.0-test --update-policy sometimes
  assert_true "$([[ "$RUN_EXIT_CODE" -ne 0 ]] && echo true || echo false)" "an invalid --update-policy must fail"
  assert_contains "$RUN_OUTPUT" "Invalid --update-policy" "the failure must name the flag"
  assert_eq "0" "$(gcloud_call_count)" "an invalid flag must fail before any gcloud call"

  run_expect_fail bash "$DEPLOY_SH" --config "$config_file" --update-policy
  assert_true "$([[ "$RUN_EXIT_CODE" -ne 0 ]] && echo true || echo false)" "--update-policy with no value must fail"
  rm -f "$config_file"
}

# =====================================================================
# deploy.sh migrate-layout
# =====================================================================

# layout_run_migrate CONFIG_JSON [ARGS...] -- runs `deploy.sh
# migrate-layout` to completion against a seeded VM. Sets RUN_EXIT_CODE,
# RUN_OUTPUT and MIGRATE_CURL_LOG (local curl calls).
layout_run_migrate() {
  local config_json="$1" config_file curl_log
  shift
  config_file="$(mktemp)"
  curl_log="$(mktemp)"
  printf '%s' "$config_json" > "$config_file"
  seed_instance "scion-hub-${HUB}" "us-central1-b"
  GCLOUD_STUB_SSH_SUCCEEDS=true CURL_STUB_LOG="$curl_log" \
    run_expect_fail bash "$DEPLOY_SH" migrate-layout --config "$config_file" "$@"
  MIGRATE_CURL_LOG="$(cat "$curl_log")"
  rm -f "$config_file" "$curl_log"
}

# _layout_migrate_cmd LOG -- the migrate-layout VM command (the
# --command body of the compute ssh call that carries `adopt`).
_layout_migrate_cmd() {
  printf '%s\n' "$1" | awk '
    /^compute ssh .*--command=$/ { buf = ""; on = 1; next }
    /^compute / { if (on && buf ~ /SCION_LAYOUT_SH" adopt/) { printf "%s", buf; done = 1; exit } on = 0 }
    on { buf = buf $0 "\n" }
    END { if (!done && on && buf ~ /SCION_LAYOUT_SH" adopt/) printf "%s", buf }
  '
}

test_layout_migrate_converts_and_restarts() {
  fresh_gcloud_state
  layout_run_migrate "$(layout_config_json "$HUB")"
  assert_eq "0" "$RUN_EXIT_CODE" "migrate-layout must succeed (output: ${RUN_OUTPUT: -600})"
  local log cmd
  log="$(gcloud_log)"
  cmd="$(_layout_migrate_cmd "$log")"
  # shellcheck disable=SC2016 # a literal from the generated command
  assert_contains "$cmd" 'sudo bash "$SCION_LAYOUT_SH" adopt' "migrate-layout must adopt the installed binary"
  # shellcheck disable=SC2016 # a literal from the generated command
  assert_contains "$cmd" 'sudo bash "$SCION_LAYOUT_SH" set-maintenance /home/scion/.scion/settings.yaml default' \
    "with no explicit policy, migrate-layout must apply the D7 default rule"
  assert_contains "$cmd" "ExecStart=/opt/scion/current/scion server start" "the unit must be re-rendered"
  assert_contains "$cmd" "sudo systemctl daemon-reload" "systemd must be reloaded"
  assert_contains "$cmd" "sudo systemctl restart scion-hub.service" "the hub must be restarted"
  local adopt_line unit_line
  adopt_line="$(line_number 'SCION_LAYOUT_SH" adopt' "$cmd")"
  unit_line="$(line_number 'sudo tee /etc/systemd/system/scion-hub.service' "$cmd")"
  assert_true "$([[ -n "$adopt_line" && -n "$unit_line" && "$adopt_line" -lt "$unit_line" ]] && echo true || echo false)" \
    "the layout must be in place before the unit points at it"
  assert_contains "$cmd" "set -euo pipefail" "a failed adopt must stop the command before the unit changes"
  assert_contains "$RUN_OUTPUT" "Post-migration health check passed" "migrate-layout must wait for the hub to come back"
}

test_layout_migrate_downloads_nothing_and_creates_nothing() {
  fresh_gcloud_state
  layout_run_migrate "$(layout_config_json "$HUB")"
  local log
  log="$(gcloud_log)"
  assert_eq "" "$MIGRATE_CURL_LOG" "migrate-layout must make no local curl call (no release lookup or checksum probe)"
  assert_not_contains "$log" "curl -fsSL" "the VM command must download nothing"
  assert_not_contains "$log" "releases/download" "no release asset may be referenced"
  assert_not_contains "$log" "services enable" "migrate-layout must not touch GCP APIs"
  assert_not_contains "$log" "compute instances create" "migrate-layout must not create a VM"
  assert_not_contains "$log" "SETTINGSEOF" "migrate-layout must edit settings.yaml, not re-render it"
}

test_layout_migrate_twice_issues_identical_commands() {
  fresh_gcloud_state
  layout_run_migrate "$(layout_config_json "$HUB")"
  local first
  first="$(gcloud_log)"
  : > "$GCLOUD_STUB_LOG"
  layout_run_migrate "$(layout_config_json "$HUB")"
  assert_eq "0" "$RUN_EXIT_CODE" "a second migrate-layout must succeed"
  assert_eq "$first" "$(gcloud_log)" "a re-run must issue exactly the same calls (the VM side decides what is a no-op)"
}

test_layout_migrate_explicit_auto_is_kept() {
  fresh_gcloud_state
  layout_run_migrate "$(layout_config_json "$HUB")" --update-policy auto
  assert_contains "$(_layout_migrate_cmd "$(gcloud_log)")" "set-maintenance /home/scion/.scion/settings.yaml auto" \
    "--update-policy auto must be passed through"

  fresh_gcloud_state
  layout_run_migrate "$(layout_config_json "$HUB" auto)"
  assert_contains "$(_layout_migrate_cmd "$(gcloud_log)")" "set-maintenance /home/scion/.scion/settings.yaml auto" \
    "config update_policy auto must be passed through"
}

test_layout_migrate_rejects_version_and_missing_vm() {
  fresh_gcloud_state
  local config_file
  config_file="$(mktemp)"
  layout_config_json "$HUB" > "$config_file"
  run_expect_fail bash "$DEPLOY_SH" migrate-layout --config "$config_file" --version v1.0.0-test
  assert_true "$([[ "$RUN_EXIT_CODE" -ne 0 ]] && echo true || echo false)" "migrate-layout --version must fail"
  assert_contains "$RUN_OUTPUT" "keeps the installed version" "the failure must explain why"

  run_expect_fail env GCLOUD_STUB_SSH_SUCCEEDS=true bash "$DEPLOY_SH" migrate-layout --config "$config_file"
  assert_true "$([[ "$RUN_EXIT_CODE" -ne 0 ]] && echo true || echo false)" "migrate-layout without the VM must fail"
  assert_contains "$RUN_OUTPUT" "not found" "the failure must say the VM was not found"
  assert_not_contains "$(gcloud_log)" "compute ssh" "nothing may be run when the VM does not exist"
  rm -f "$config_file"
}

# _layout_fake_sudo_dir DIR -- a PATH dir with a `sudo` shim for running
# the generated migrate command locally: `sudo tee /etc/...` writes under
# DIR/etc, `sudo systemctl ...` is logged to DIR/systemctl.log, anything
# else runs as the current user.
_layout_fake_sudo_dir() {
  local dir="$1"
  mkdir -p "${dir}/shim" "${dir}/etc"
  cat > "${dir}/shim/sudo" <<EOF
#!/usr/bin/env bash
case "\$1" in
  tee) shift; exec tee "${dir}\$1" ;;
  systemctl) echo "\$*" >> "${dir}/systemctl.log"; exit 0 ;;
  *) exec "\$@" ;;
esac
EOF
  chmod 0755 "${dir}/shim/sudo"
  mkdir -p "${dir}/etc/systemd/system"
}

test_layout_migrate_vm_command_runs_twice_to_same_state() {
  fresh_gcloud_state
  layout_run_migrate "$(layout_config_json "$HUB")"
  local cmd
  cmd="$(_layout_migrate_cmd "$(gcloud_log)")"
  _layout_new_host
  _fake_scion "$LH_BIN" "nightly-20261001"
  _deploy_rendered_settings auto > "${LH_DIR}/settings.yaml"
  mkdir -p "${LH_DIR}/work"
  _layout_fake_sudo_dir "${LH_DIR}/work"
  cmd="${cmd//\/home\/scion\/.scion\/settings.yaml/${LH_DIR}/settings.yaml}"

  local out rc first
  out="$(PATH="${LH_DIR}/work/shim:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
    SCION_LAYOUT_YAML_TO_JSON="$LAYOUT_YAML_TO_JSON" bash -c "$cmd" 2>&1)"
  rc=$?
  assert_eq "0" "$rc" "the generated migrate command must succeed on an old-layout host (output: ${out})"
  assert_eq "releases/nightly-20261001" "$(readlink "$LH_ROOT/current")" "the installed version must be kept"
  assert_eq "$LH_ROOT/current/scion" "$(readlink "$LH_BIN")" "the CLI path must become the symlink"
  assert_contains "$(cat "${LH_DIR}/work/etc/systemd/system/scion-hub.service")" \
    "ExecStart=/opt/scion/current/scion" "the unit must be written with the new ExecStart"
  assert_contains "$(cat "${LH_DIR}/settings.yaml")" '    update_policy: "notify"' "auto must become notify"
  assert_contains "$(cat "${LH_DIR}/settings.yaml")" "    install_root: \"${LH_ROOT}\"" "install_root must be written"
  first="$(_layout_state; cat "${LH_DIR}/settings.yaml")"

  out="$(PATH="${LH_DIR}/work/shim:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
    SCION_LAYOUT_YAML_TO_JSON="$LAYOUT_YAML_TO_JSON" bash -c "$cmd" 2>&1)"
  rc=$?
  assert_eq "0" "$rc" "a second run must succeed (output: ${out})"
  assert_eq "$first" "$(_layout_state; cat "${LH_DIR}/settings.yaml")" \
    "a second run must leave the layout and settings exactly as the first left them"
  assert_eq "2" "$(grep -c '^systemctl restart scion-hub.service' "${LH_DIR}/work/systemctl.log")" "each run must restart the hub"
  rm -rf "$LH_DIR"
}

# =====================================================================
# Re-runs on a VM that already has the layout (downgrade guard)
# =====================================================================

# Without --version, a VM with /opt/scion/current keeps the release it
# runs: no GitHub auto-detect, no checksum probe, no binary download or
# switch -- the hub may have updated itself past what auto-detect picks.
test_layout_rerun_without_version_keeps_current_release() {
  fresh_gcloud_state
  seed_instance "scion-hub-${HUB}" "us-central1-b"
  export GCLOUD_STUB_SSH_CURRENT_LINK="releases/nightly-20261020"
  LAYOUT_DEPLOY_VERSION=none layout_deploy_through_settings "$(layout_config_json "$HUB" notify)"
  unset GCLOUD_STUB_SSH_CURRENT_LINK
  local log
  log="$(gcloud_log)"
  assert_eq "true" "$LAYOUT_REACHED_SETTINGS" "the re-run must still reach the settings.yaml write (log: ${DEPLOY_LOG: -400})"
  assert_contains "$DEPLOY_LOG" "Keeping the installed release nightly-20261020" \
    "the re-run must say it keeps the current release"
  assert_contains "$DEPLOY_LOG" "Pass --version to install a different release" "it must say how to change release"
  assert_not_contains "$LAYOUT_CURL_LOG" "api.github.com" "no GitHub release auto-detect may run"
  assert_not_contains "$LAYOUT_CURL_LOG" "SHA256SUMS" "with no chat plugins, no checksum probe is needed"
  assert_not_contains "$log" "Downloading scion binary" "the binary must not be downloaded"
  # shellcheck disable=SC2016 # a literal from the generated command
  assert_not_contains "$log" 'SCION_LAYOUT_SH" install' "current must not be switched"
  assert_contains "$DEPLOY_LOG" "Keeping the installed scion binary" "Phase 3 must say the binary is kept"
}

test_layout_rerun_without_version_still_verifies_plugins() {
  fresh_gcloud_state
  seed_instance "scion-hub-${HUB}" "us-central1-b"
  export GCLOUD_STUB_SSH_CURRENT_LINK="releases/nightly-20261020"
  LAYOUT_DEPLOY_VERSION=none layout_deploy_through_settings \
    "$(layout_config_json "$HUB" notify | sed 's/"chat_plugins": \[\]/"chat_plugins": ["telegram"]/')"
  unset GCLOUD_STUB_SSH_CURRENT_LINK
  local log
  log="$(gcloud_log)"
  assert_contains "$LAYOUT_CURL_LOG" "releases/download/nightly-20261020/SHA256SUMS" \
    "chat plugins still come from the kept release, so its checksums must be probed"
  assert_contains "$log" "scion-plugin-telegram-linux-amd64.tar.gz" "the plugin must still be installed from the kept release"
  assert_not_contains "$log" "Downloading scion binary" "the binary must still not be downloaded"
}

test_layout_rerun_without_version_refuses_git_build() {
  fresh_gcloud_state
  seed_instance "scion-hub-${HUB}" "us-central1-b"
  local config_file
  config_file="$(mktemp)"
  layout_config_json "$HUB" notify > "$config_file"
  GCLOUD_STUB_SSH_SUCCEEDS=true GCLOUD_STUB_SSH_CURRENT_LINK="releases/git-abcdef012345" \
    run_expect_fail bash "$DEPLOY_SH" --config "$config_file"
  rm -f "$config_file"
  assert_true "$([[ "$RUN_EXIT_CODE" -ne 0 ]] && echo true || echo false)" \
    "a re-run without --version on a git build must stop"
  assert_contains "$RUN_OUTPUT" "runs a git build" "the failure must say why"
  assert_contains "$RUN_OUTPUT" "--version" "the failure must say how to proceed"
  assert_not_contains "$(gcloud_log)" "services enable" "it must stop before any GCP change"
}

# _layout_rerun_with_version VERSION CURRENT_LINK -- an explicit --version
# re-run against a VM whose current points at CURRENT_LINK.
_layout_rerun_with_version() {
  fresh_gcloud_state
  seed_instance "scion-hub-${HUB}" "us-central1-b"
  export GCLOUD_STUB_SSH_CURRENT_LINK="$2"
  LAYOUT_DEPLOY_VERSION="$1" layout_deploy_through_settings "$(layout_config_json "$HUB" notify)"
  unset GCLOUD_STUB_SSH_CURRENT_LINK
}

test_layout_rerun_explicit_older_version_warns_and_proceeds() {
  _layout_rerun_with_version nightly-20261001 releases/nightly-20261024
  assert_contains "$DEPLOY_LOG" "DOWNGRADE: --version nightly-20261001 is OLDER than the release scion-hub-${HUB} runs now (nightly-20261024)" \
    "an explicit older nightly must be flagged as a downgrade"
  assert_contains "$DEPLOY_LOG" "Proceeding because --version was given explicitly" "the warning must say it proceeds, and why"
  # shellcheck disable=SC2016 # a literal from the generated command
  assert_contains "$(gcloud_log)" 'sudo bash "$SCION_LAYOUT_SH" install nightly-20261001 /tmp/scion' \
    "the explicit version must still be installed"

  _layout_rerun_with_version v1.0.0-test releases/v1.0.0
  assert_contains "$DEPLOY_LOG" "DOWNGRADE: --version v1.0.0-test is OLDER" \
    "a pre-release of the running release's version must be flagged as a downgrade"
}

test_layout_rerun_explicit_newer_or_same_version_no_warning() {
  local current
  for current in releases/v0.9.0 releases/v1.0.0-rc.1 releases/v1.0.0-test; do
    _layout_rerun_with_version v1.0.0-test "$current"
    assert_not_contains "$DEPLOY_LOG" "DOWNGRADE" "no downgrade warning for v1.0.0-test over ${current}"
    assert_not_contains "$DEPLOY_LOG" "cannot be ordered" "no channel warning for v1.0.0-test over ${current}"
    # shellcheck disable=SC2016 # a literal from the generated command
    assert_contains "$(gcloud_log)" 'sudo bash "$SCION_LAYOUT_SH" install v1.0.0-test /tmp/scion' \
      "the explicit version must be installed over ${current}"
  done
  _layout_rerun_with_version nightly-20261024 releases/nightly-20261001
  assert_not_contains "$DEPLOY_LOG" "DOWNGRADE" "no downgrade warning for a newer nightly"
}

test_layout_rerun_explicit_cross_channel_version_warns() {
  _layout_rerun_with_version v1.0.0-test releases/nightly-20261024
  assert_contains "$DEPLOY_LOG" "cannot be ordered (different channels)" \
    "a version on another channel must get a may-be-a-downgrade warning"
  # shellcheck disable=SC2016 # a literal from the generated command
  assert_contains "$(gcloud_log)" 'sudo bash "$SCION_LAYOUT_SH" install v1.0.0-test /tmp/scion' \
    "and still be installed"
}

# =====================================================================
# Review round 1: failure injection, Go-written settings, hardening
# =====================================================================

# _layout_cp_stub_dir DIR RC -- a PATH dir whose `cp` writes only the first
# 10 bytes of the source and exits RC: RC=1 is a failed copy (ENOSPC),
# RC=0 a silent short copy.
_layout_cp_stub_dir() {
  mkdir -p "$1"
  cat > "$1/cp" <<EOF
#!/usr/bin/env bash
head -c 10 "\$1" > "\$2"
echo "cp: error writing '\$2': No space left on device" >&2
exit $2
EOF
  chmod 0755 "$1/cp"
}

# _layout_assert_untouched ORIG_COPY WHAT -- the host must still be on the
# old layout: the CLI path is the original plain binary, no current link,
# no release, and no staging dir left next to the root.
_layout_assert_untouched() {
  assert_false "$([[ -L "$LH_BIN" ]] && echo true || echo false)" "$2: the CLI path must still be the plain binary"
  assert_true "$(cmp -s "$1" "$LH_BIN" && echo true || echo false)" "$2: the original binary must be intact"
  assert_false "$([[ -e "$LH_ROOT/current" || -L "$LH_ROOT/current" ]] && echo true || echo false)" "$2: no current link may exist"
  assert_eq "" "$(ls -A "$LH_ROOT/releases" 2>/dev/null)" "$2: no release may be left in releases/"
  assert_eq "" "$(find "$(dirname "$LH_ROOT")" -maxdepth 1 -name '.scion-stage.*' 2>/dev/null)" "$2: no staging dir may be left behind"
}

test_layout_adopt_failed_copy_keeps_original_binary() {
  local rc_mode
  for rc_mode in 1 0; do
    _layout_new_host
    _fake_scion "$LH_BIN" "v0.1.0"
    cp "$LH_BIN" "${LH_DIR}/orig"
    _layout_cp_stub_dir "${LH_DIR}/stub" "$rc_mode"
    LAYOUT_OUT="$(PATH="${LH_DIR}/stub:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" \
      SCION_INSTALL_OWNER="" bash "$LAYOUT_SH" adopt 2>&1)"
    LAYOUT_RC=$?
    assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" \
      "adopt must fail when the copy fails or is short (cp exit ${rc_mode}; output: ${LAYOUT_OUT})"
    _layout_assert_untouched "${LH_DIR}/orig" "adopt with a broken copy (cp exit ${rc_mode})"
    rm -rf "$LH_DIR"
  done
}

test_layout_install_failed_copy_over_old_layout_keeps_original() {
  local rc_mode
  for rc_mode in 1 0; do
    _layout_new_host
    _fake_scion "$LH_BIN" "v0.1.0"
    cp "$LH_BIN" "${LH_DIR}/orig"
    _fake_scion "${LH_DIR}/new" "v0.2.0"
    _layout_cp_stub_dir "${LH_DIR}/stub" "$rc_mode"
    LAYOUT_OUT="$(PATH="${LH_DIR}/stub:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" \
      SCION_INSTALL_OWNER="" bash "$LAYOUT_SH" install v0.2.0 "${LH_DIR}/new" 2>&1)"
    LAYOUT_RC=$?
    assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" \
      "install over an old layout must fail when the copy fails or is short (cp exit ${rc_mode}; output: ${LAYOUT_OUT})"
    _layout_assert_untouched "${LH_DIR}/orig" "install with a broken copy (cp exit ${rc_mode})"
    rm -rf "$LH_DIR"
  done
}

test_layout_install_leaves_no_staging_dir() {
  _layout_new_host
  _fake_scion "${LH_DIR}/a" "nightly-20261022"
  _fake_scion "${LH_DIR}/b" "nightly-20261022-rebuilt"
  _layout install nightly-20261022 "${LH_DIR}/a"
  # Same tag, different bytes: the old release dir is replaced.
  cp "${LH_DIR}/b" "${LH_DIR}/a2"
  _layout install nightly-20261022 "${LH_DIR}/a2"
  assert_eq "0" "$LAYOUT_RC" "re-installing a tag with different bytes must succeed (output: ${LAYOUT_OUT})"
  assert_true "$(cmp -s "${LH_DIR}/a2" "$LH_ROOT/releases/nightly-20261022/scion" && echo true || echo false)" \
    "the release must hold the new bytes"
  assert_eq "" "$(find "$(dirname "$LH_ROOT")" -maxdepth 1 -name '.scion-stage.*')" "no staging dir may be left behind"
  assert_eq "nightly-20261022" "$(ls "$LH_ROOT/releases")" "no .partial/.old leftovers in releases/"
  rm -rf "$LH_DIR"
}

test_layout_refuses_symlinked_dirs() {
  local victim
  _layout_new_host
  victim="${LH_DIR}/victim"
  mkdir -p "$victim" "$LH_ROOT"
  ln -s "$victim" "$LH_ROOT/releases"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  _layout install nightly-20261023 "${LH_DIR}/new"
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "a symlinked releases/ must be refused"
  assert_contains "$LAYOUT_OUT" "is a symlink" "the refusal must say why"
  assert_eq "" "$(ls -A "$victim")" "nothing may be written through the symlink"
  rm -rf "$LH_DIR"

  _layout_new_host
  victim="${LH_DIR}/victim"
  mkdir -p "$victim" "$LH_ROOT/releases"
  ln -s "$victim" "$LH_ROOT/releases/nightly-20261023"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  _layout install nightly-20261023 "${LH_DIR}/new"
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "a symlinked release dir must be refused"
  assert_eq "" "$(ls -A "$victim")" "nothing may be written through a planted release symlink"
  rm -rf "$LH_DIR"
}

# Go-written settings (the admin server-config save): 4-space indent, no
# quotes, keys sorted. The result must parse with the hub's parser and
# differ from the original only in the two keys.
test_layout_settings_go_written_without_maintenance_block() {
  _layout_new_host
  _go_written_yaml '{"schema_version":"1","server":{"hub":{"hub_name":"demo"},"storage":{"local_path":"/a"},"auth":{"mode":"proxy"}},"image_registry":"x"}' \
    > "${LH_DIR}/settings.yaml"
  _layout set-maintenance "${LH_DIR}/settings.yaml" default
  assert_eq "0" "$LAYOUT_RC" "a Go-written file with no maintenance block must be updated (output: ${LAYOUT_OUT})"
  local got
  got="$(_settings_json "${LH_DIR}/settings.yaml")"
  assert_eq "true" "$(_json_eq "$got" "{\"schema_version\":\"1\",\"image_registry\":\"x\",\"server\":{\"hub\":{\"hub_name\":\"demo\"},\"storage\":{\"local_path\":\"/a\"},\"auth\":{\"mode\":\"proxy\"},\"maintenance\":{\"install_root\":\"${LH_ROOT}\",\"update_policy\":\"notify\"}}}")" \
    "the result must parse with the hub's parser and add exactly server.maintenance (got: ${got})"
  assert_contains "$(cat "${LH_DIR}/settings.yaml")" $'\n    maintenance:\n        install_root:' \
    "the new block must follow the file's 4-space indent"
  rm -rf "$LH_DIR"
}

test_layout_settings_go_written_with_maintenance_block() {
  _layout_new_host
  _go_written_yaml '{"server":{"hub":{"hub_name":"demo"},"maintenance":{"deployment_tier":"binary","release_channel":"nightly","update_policy":"auto","github_repo":"o/r"},"secrets":{"backend":"local"}}}' \
    > "${LH_DIR}/settings.yaml"
  _layout set-maintenance "${LH_DIR}/settings.yaml" default
  assert_eq "0" "$LAYOUT_RC" "a Go-written maintenance block must be updated (output: ${LAYOUT_OUT})"
  assert_eq "true" "$(_json_eq "$(_settings_json "${LH_DIR}/settings.yaml")" "{\"server\":{\"hub\":{\"hub_name\":\"demo\"},\"maintenance\":{\"deployment_tier\":\"binary\",\"release_channel\":\"nightly\",\"update_policy\":\"notify\",\"github_repo\":\"o/r\",\"install_root\":\"${LH_ROOT}\"},\"secrets\":{\"backend\":\"local\"}}}")" \
    "only install_root and update_policy may change"
  rm -rf "$LH_DIR"
}

test_layout_settings_refuses_flow_and_scalar_maintenance() {
  local body
  for body in '  maintenance: {update_policy: auto}' '  maintenance: null' '  maintenance: {}'; do
    _layout_new_host
    printf 'server:\n  hub:\n    hub_name: x\n%s\n' "$body" > "${LH_DIR}/settings.yaml"
    cp "${LH_DIR}/settings.yaml" "${LH_DIR}/orig.yaml"
    _layout set-maintenance "${LH_DIR}/settings.yaml" default
    assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "'${body}' must be refused"
    assert_contains "$LAYOUT_OUT" "by hand" "the refusal for '${body}' must say what to set by hand"
    assert_true "$(cmp -s "${LH_DIR}/orig.yaml" "${LH_DIR}/settings.yaml" && echo true || echo false)" \
      "'${body}': the file must be unchanged"
    rm -rf "$LH_DIR"
  done
}

test_layout_settings_ignores_nested_maintenance_key() {
  _layout_new_host
  printf 'server:\n  broker:\n    maintenance:\n      window: nightly\n  hub:\n    hub_name: x\n' > "${LH_DIR}/settings.yaml"
  _layout set-maintenance "${LH_DIR}/settings.yaml" default
  assert_eq "0" "$LAYOUT_RC" "a nested maintenance key must not block the edit (output: ${LAYOUT_OUT})"
  assert_eq "true" "$(_json_eq "$(_settings_json "${LH_DIR}/settings.yaml")" "{\"server\":{\"broker\":{\"maintenance\":{\"window\":\"nightly\"}},\"hub\":{\"hub_name\":\"x\"},\"maintenance\":{\"install_root\":\"${LH_ROOT}\",\"update_policy\":\"notify\"}}}")" \
    "server.broker.maintenance must be left alone and server.maintenance added"
  rm -rf "$LH_DIR"
}

# A shape the line edit misreads (a quoted key) must be caught by the
# structural check, not written: the edit would add a second maintenance
# mapping, which the hub's parser rejects as a duplicate key.
test_layout_settings_verifier_blocks_bad_edit() {
  _layout_new_host
  printf 'server:\n  "maintenance":\n    update_policy: auto\n' > "${LH_DIR}/settings.yaml"
  cp "${LH_DIR}/settings.yaml" "${LH_DIR}/orig.yaml"
  _layout set-maintenance "${LH_DIR}/settings.yaml" default
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "an edit that does not parse must be refused"
  assert_contains "$LAYOUT_OUT" "would not parse" "the refusal must say the edit would not parse"
  assert_true "$(cmp -s "${LH_DIR}/orig.yaml" "${LH_DIR}/settings.yaml" && echo true || echo false)" "the file must be unchanged"
  assert_false "$(ls "${LH_DIR}"/settings.yaml.* >/dev/null 2>&1 && echo true || echo false)" "no temp file may be left behind"

  # No parser available at all: refuse rather than write unchecked.
  printf 'server:\n  maintenance:\n    update_policy: auto\n' > "${LH_DIR}/settings.yaml"
  cp "${LH_DIR}/settings.yaml" "${LH_DIR}/orig.yaml"
  LAYOUT_OUT="$(SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
    SCION_LAYOUT_YAML_TO_JSON="exit 1" bash "$LAYOUT_SH" set-maintenance "${LH_DIR}/settings.yaml" default 2>&1)"
  LAYOUT_RC=$?
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "with no working YAML parser the edit must be refused"
  assert_true "$(cmp -s "${LH_DIR}/orig.yaml" "${LH_DIR}/settings.yaml" && echo true || echo false)" "the file must be unchanged"
  rm -rf "$LH_DIR"
}

# Phase 3 fallback: the Phase 1 probe missed the layout (first readlink
# answers empty), so deploy.sh auto-detected a release; Phase 3 sees the
# layout and must keep current -- and use the kept tag for everything
# after, not just the binary.
test_layout_rerun_phase3_fallback_uses_kept_release_everywhere() {
  fresh_gcloud_state
  seed_instance "scion-hub-${HUB}" "us-central1-b"
  export GCLOUD_STUB_SSH_CURRENT_LINK="releases/nightly-20261020" GCLOUD_STUB_SSH_CURRENT_LINK_SKIP=1
  export CURL_STUB_LATEST_TAG="v9.9.9"
  LAYOUT_DEPLOY_VERSION=none layout_deploy_through_settings \
    "$(layout_config_json "$HUB" notify | sed 's/"chat_plugins": \[\]/"chat_plugins": ["telegram"]/')"
  unset GCLOUD_STUB_SSH_CURRENT_LINK GCLOUD_STUB_SSH_CURRENT_LINK_SKIP CURL_STUB_LATEST_TAG
  local log
  log="$(gcloud_log)"
  assert_eq "true" "$LAYOUT_REACHED_SETTINGS" "the run must reach the settings write (log: ${DEPLOY_LOG: -400})"
  assert_contains "$DEPLOY_LOG" "keeping it instead of switching to the auto-detected v9.9.9" \
    "Phase 3 must keep current and say so"
  assert_not_contains "$log" "Downloading scion binary" "the binary must not be downloaded"
  assert_contains "$log" "releases/download/nightly-20261020/scion-plugin-telegram-linux-amd64.tar.gz" \
    "chat plugins must come from the kept release"
  assert_not_contains "$log" "releases/download/v9.9.9/scion-plugin" "chat plugins must not come from the auto-detected release"
}

test_layout_rerun_kept_release_aged_out_explains() {
  fresh_gcloud_state
  seed_instance "scion-hub-${HUB}" "us-central1-b"
  local config_file
  config_file="$(mktemp)"
  layout_config_json "$HUB" notify | sed 's/"chat_plugins": \[\]/"chat_plugins": ["telegram"]/' > "$config_file"
  GCLOUD_STUB_SSH_SUCCEEDS=true GCLOUD_STUB_SSH_CURRENT_LINK="releases/nightly-20260901" CURL_STUB_SHA256SUMS_MISSING=true \
    run_expect_fail bash "$DEPLOY_SH" --config "$config_file"
  rm -f "$config_file"
  assert_true "$([[ "$RUN_EXIT_CODE" -ne 0 ]] && echo true || echo false)" "a kept release that is no longer published must stop the run"
  assert_contains "$RUN_OUTPUT" "The installed release nightly-20260901 is no longer published on GitHub" \
    "the error must say the kept release has aged out"
  assert_contains "$RUN_OUTPUT" "Pass --version" "the error must say how to proceed"
  assert_not_contains "$RUN_OUTPUT" "ALLOW_UNVERIFIED_RELEASE" "the unverified override is not the fix here"
  assert_not_contains "$(gcloud_log)" "services enable" "it must stop before any GCP change"
}

# =====================================================================
# Review round 2: settings edit as the file owner, default PyYAML parser
# =====================================================================

# The old fixed temp name (settings.yaml.scion-layout.tmp) planted as a
# symlink must not be written or chowned through: the temp file is a fresh
# mktemp name, and settings.yaml stays a regular file.
test_layout_settings_ignores_planted_temp_symlink() {
  _layout_new_host
  _deploy_rendered_settings auto > "${LH_DIR}/settings.yaml"
  echo "sentinel" > "${LH_DIR}/victim"
  chmod 0600 "${LH_DIR}/victim"
  ln -s "${LH_DIR}/victim" "${LH_DIR}/settings.yaml.scion-layout.tmp"
  _layout set-maintenance "${LH_DIR}/settings.yaml" default
  assert_eq "0" "$LAYOUT_RC" "the edit must succeed (output: ${LAYOUT_OUT})"
  assert_eq "sentinel" "$(cat "${LH_DIR}/victim")" "the symlink target must not be written"
  assert_eq "600" "$(stat -c %a "${LH_DIR}/victim")" "the symlink target's mode must not change"
  assert_false "$([[ -L "${LH_DIR}/settings.yaml" ]] && echo true || echo false)" "settings.yaml must stay a regular file"
  assert_contains "$(cat "${LH_DIR}/settings.yaml")" '    update_policy: "notify"' "the edit must have been applied"
  rm -rf "$LH_DIR"
}

test_layout_settings_refuses_symlinked_settings_file() {
  _layout_new_host
  _deploy_rendered_settings auto > "${LH_DIR}/real.yaml"
  ln -s "${LH_DIR}/real.yaml" "${LH_DIR}/settings.yaml"
  _layout set-maintenance "${LH_DIR}/settings.yaml" default
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "a symlinked settings file must be refused"
  assert_contains "$(cat "${LH_DIR}/real.yaml")" '    update_policy: "auto"' "the link target must be unchanged"
  rm -rf "$LH_DIR"
}

# Run "as root" (stub id -u = 0), set-maintenance must re-run itself as the
# settings file's owner through runuser (stub: records the user, then runs
# the command), passing the install root and parser along, and the edit
# must then be applied by that re-run.
test_layout_settings_as_root_reexecs_as_file_owner() {
  _layout_new_host
  _deploy_rendered_settings auto > "${LH_DIR}/settings.yaml"
  chmod 0640 "${LH_DIR}/settings.yaml"
  local stub="${LH_DIR}/stub" me
  me="$(id -un)"
  mkdir -p "$stub"
  cat > "${stub}/id" <<'STUB'
#!/usr/bin/env bash
if [[ "${1:-}" == "-u" && $# -eq 1 && -z "${LAYOUT_STUB_DROPPED:-}" ]]; then echo 0; exit 0; fi
exec /usr/bin/id "$@"
STUB
  cat > "${stub}/runuser" <<STUB
#!/usr/bin/env bash
[[ "\$1" == "-u" && "\$3" == "--" ]] || { echo "runuser stub: unexpected args: \$*" >&2; exit 2; }
echo "\$2" > "${LH_DIR}/runuser-user"
shift 3
LAYOUT_STUB_DROPPED=1 exec "\$@"
STUB
  chmod 0755 "${stub}/id" "${stub}/runuser"
  LAYOUT_OUT="$(PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
    SCION_LAYOUT_YAML_TO_JSON="$LAYOUT_YAML_TO_JSON" bash "$LAYOUT_SH" set-maintenance "${LH_DIR}/settings.yaml" default 2>&1)"
  LAYOUT_RC=$?
  assert_eq "0" "$LAYOUT_RC" "the re-run as the owner must succeed (output: ${LAYOUT_OUT})"
  assert_eq "$me" "$(cat "${LH_DIR}/runuser-user" 2>/dev/null || echo '<runuser not called>')" \
    "as root, the edit must run as the settings file's owner"
  assert_contains "$(cat "${LH_DIR}/settings.yaml")" "    install_root: \"${LH_ROOT}\"" \
    "the re-run must receive the install root and apply the edit"
  assert_eq "640" "$(stat -c %a "${LH_DIR}/settings.yaml")" "the file mode must be preserved"
  rm -rf "$LH_DIR"
}

# The parser the VM actually uses: python3 + PyYAML with duplicate keys
# rejected. Runs when PyYAML is importable (CI installs python3-yaml);
# otherwise prints a visible SKIP line instead of passing silently.
test_layout_settings_default_pyyaml_parser() {
  if ! python3 -I -c 'import yaml' 2>/dev/null; then
    echo "SKIP [${CURRENT_TEST}]: python3 cannot import yaml (install python3-yaml to run this test)"
    return 0
  fi
  _layout_new_host
  _go_written_yaml '{"server":{"hub":{"hub_name":"demo"},"storage":{"local_path":"/a"}}}' > "${LH_DIR}/settings.yaml"
  LAYOUT_OUT="$(env -u SCION_LAYOUT_YAML_TO_JSON SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
    bash "$LAYOUT_SH" set-maintenance "${LH_DIR}/settings.yaml" default 2>&1)"
  LAYOUT_RC=$?
  assert_eq "0" "$LAYOUT_RC" "the PyYAML path must accept a valid 4-space insert (output: ${LAYOUT_OUT})"
  assert_eq "true" "$(_json_eq "$(_settings_json "${LH_DIR}/settings.yaml")" "{\"server\":{\"hub\":{\"hub_name\":\"demo\"},\"storage\":{\"local_path\":\"/a\"},\"maintenance\":{\"install_root\":\"${LH_ROOT}\",\"update_policy\":\"notify\"}}}")" \
    "the PyYAML-verified result must parse with the hub's parser as intended"

  printf 'server:\n  "maintenance":\n    update_policy: auto\n' > "${LH_DIR}/settings.yaml"
  cp "${LH_DIR}/settings.yaml" "${LH_DIR}/orig.yaml"
  LAYOUT_OUT="$(env -u SCION_LAYOUT_YAML_TO_JSON SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
    bash "$LAYOUT_SH" set-maintenance "${LH_DIR}/settings.yaml" default 2>&1)"
  LAYOUT_RC=$?
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "the PyYAML path must reject a duplicate key"
  assert_contains "$LAYOUT_OUT" "would not parse" "the refusal must come from the duplicate-key check"
  assert_true "$(cmp -s "${LH_DIR}/orig.yaml" "${LH_DIR}/settings.yaml" && echo true || echo false)" "the file must be unchanged"
  rm -rf "$LH_DIR"
}

# The documented recovery after a refused edit: the operator sets the two
# keys by hand (block style, quoting optional) and re-runs. That re-run
# must count as up to date without a YAML parser and leave the file alone.
test_layout_settings_hand_edit_is_up_to_date_without_parser() {
  _layout_new_host
  printf 'server:\n  maintenance:\n    install_root: %s   # set by hand\n    update_policy: notify\n' "$LH_ROOT" \
    > "${LH_DIR}/settings.yaml"
  cp "${LH_DIR}/settings.yaml" "${LH_DIR}/orig.yaml"
  LAYOUT_OUT="$(SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
    SCION_LAYOUT_YAML_TO_JSON="exit 1" bash "$LAYOUT_SH" set-maintenance "${LH_DIR}/settings.yaml" default 2>&1)"
  LAYOUT_RC=$?
  assert_eq "0" "$LAYOUT_RC" "a correct hand edit must count as up to date without a parser (output: ${LAYOUT_OUT})"
  assert_contains "$LAYOUT_OUT" "already up to date" "the re-run must report no change"
  assert_true "$(cmp -s "${LH_DIR}/orig.yaml" "${LH_DIR}/settings.yaml" && echo true || echo false)" "the file must be untouched"

  printf 'server:\n  maintenance:\n    install_root: "%s"\n    update_policy: auto\n' "$LH_ROOT" > "${LH_DIR}/settings.yaml"
  LAYOUT_OUT="$(SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
    SCION_LAYOUT_YAML_TO_JSON="exit 1" bash "$LAYOUT_SH" set-maintenance "${LH_DIR}/settings.yaml" auto 2>&1)"
  assert_eq "0" "$?" "an explicit policy that already matches must also be up to date (output: ${LAYOUT_OUT})"
  rm -rf "$LH_DIR"
}

# Comments are kept, including a trailing comment on a line that is
# rewritten.
test_layout_settings_keeps_trailing_comments_on_rewritten_lines() {
  _layout_new_host
  printf 'server:\n  maintenance:\n    install_root: /old   # where releases live\n    update_policy: auto  # chosen by deploy.sh\n' \
    > "${LH_DIR}/settings.yaml"
  _layout set-maintenance "${LH_DIR}/settings.yaml" default
  assert_eq "0" "$LAYOUT_RC" "the edit must succeed (output: ${LAYOUT_OUT})"
  assert_eq "$(printf 'server:\n  maintenance:\n    install_root: "%s"   # where releases live\n    update_policy: "notify"  # chosen by deploy.sh' "$LH_ROOT")" \
    "$(cat "${LH_DIR}/settings.yaml")" "rewritten lines must keep their trailing comments"

  _layout set-maintenance "${LH_DIR}/settings.yaml" disabled
  assert_contains "$(cat "${LH_DIR}/settings.yaml")" '    update_policy: "disabled"  # chosen by deploy.sh' \
    "an explicit policy rewrite must keep the trailing comment too"
  rm -rf "$LH_DIR"
}
