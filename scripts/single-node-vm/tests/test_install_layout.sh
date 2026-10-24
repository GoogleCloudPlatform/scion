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
  # LAYOUT_SENTINEL picks a later stop point (a gcloud stub sentinel name).
  local sentinel="${GCLOUD_STUB_STATE_DIR}/${LAYOUT_SENTINEL:-settings-yaml-dev-mode-written}"
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
  assert_contains "$cmd" 'sudo bash "$SCION_LAYOUT_SH" install v1.0.0-test "$SCION_EXTRACT_DIR/scion"' \
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
  assert_eq "" "$(find "$work" -maxdepth 1 -name 'scion-extract.*')" "the private extraction dir must be cleaned up"
  # shellcheck disable=SC2016 # a literal from the generated command
  assert_contains "$cmd" 'SCION_EXTRACT_DIR="$(mktemp -d /tmp/scion-extract.XXXXXX)"' \
    "the tarball must be extracted into a fresh mktemp directory, not a fixed /tmp name"
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
    /^compute / { if (on && buf ~ /SCION_LAYOUT_SH" adopt/) { printf "%s", buf; done = 1; exit } on = 0 }
    /^compute ssh .*--command=$/ { buf = ""; on = 1; next }
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
  assert_contains "$(gcloud_log)" 'sudo bash "$SCION_LAYOUT_SH" install nightly-20261001 "$SCION_EXTRACT_DIR/scion"' \
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
    assert_contains "$(gcloud_log)" 'sudo bash "$SCION_LAYOUT_SH" install v1.0.0-test "$SCION_EXTRACT_DIR/scion"' \
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
  assert_contains "$(gcloud_log)" 'sudo bash "$SCION_LAYOUT_SH" install v1.0.0-test "$SCION_EXTRACT_DIR/scion"' \
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
  assert_eq "" "$(find "${TMPDIR:-/tmp}" -maxdepth 1 -name 'scion-layout-input.*' 2>/dev/null)" "$2: no input copy dir may be left behind"
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
  assert_eq "" "$(find "${TMPDIR:-/tmp}" -maxdepth 1 -name 'scion-layout-input.*')" "no input copy dir may be left behind"
  assert_eq "nightly-20261022" "$(ls "$LH_ROOT/releases")" "no .partial/.old leftovers in releases/"
  rm -rf "$LH_DIR"
}

test_layout_refuses_symlinked_dirs() {
  local victim sub
  for sub in releases snapshots; do
    _layout_new_host
    victim="${LH_DIR}/victim"
    mkdir -p "$victim" "$LH_ROOT"
    chmod 0700 "$victim"
    [[ "$sub" == releases ]] && mkdir -m 0755 "$LH_ROOT/snapshots"
    [[ "$sub" == snapshots ]] && mkdir -m 0755 "$LH_ROOT/releases"
    ln -s "$victim" "$LH_ROOT/$sub"
    assert_true "$([[ -L "$LH_ROOT/$sub" && -d "$LH_ROOT/$([[ $sub == releases ]] && echo snapshots || echo releases)" ]] && echo true || echo false)" \
      "setup: the ${sub} symlink must be planted in an existing tree"
    _fake_scion "${LH_DIR}/new" "nightly-20261023"
    _layout install nightly-20261023 "${LH_DIR}/new"
    assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "a symlinked ${sub}/ must be refused"
    assert_contains "$LAYOUT_OUT" "symbolic link" "the refusal for ${sub}/ must say what was found"
    assert_eq "" "$(ls -A "$victim")" "nothing may be written through the ${sub} symlink"
    assert_eq "700" "$(stat -c %a "$victim")" "the ${sub} symlink target must not be chmod-ed"
    rm -rf "$LH_DIR"
  done

  # A planted releases/<tag> symlink is replaced by the owner-side rename
  # (the link itself is moved aside and removed), never written through.
  _layout_new_host
  victim="${LH_DIR}/victim"
  mkdir -p "$victim" "$(dirname "$LH_ROOT")"
  mkdir -m 0755 "$LH_ROOT" "$LH_ROOT/releases"
  ln -s "$victim" "$LH_ROOT/releases/nightly-20261023"
  assert_true "$([[ -L "$LH_ROOT/releases/nightly-20261023" ]] && echo true || echo false)" \
    "setup: the releases/<tag> symlink must be planted before the install"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  _layout install nightly-20261023 "${LH_DIR}/new"
  assert_eq "0" "$LAYOUT_RC" "a planted release symlink must be replaced (output: ${LAYOUT_OUT})"
  assert_eq "" "$(ls -A "$victim")" "nothing may be written through a planted release symlink"
  assert_true "$([[ -d "$LH_ROOT/releases/nightly-20261023" && ! -L "$LH_ROOT/releases/nightly-20261023" ]] && echo true || echo false)" \
    "releases/<tag> must now be a real directory"
  rm -rf "$LH_DIR"
}

# =====================================================================
# G1 (security audit): root never changes anything inside <root> after
# its checks; an existing tree is verified and refused, never chmod-ed.
# =====================================================================

test_layout_existing_tree_wrong_mode_is_refused_not_fixed() {
  _layout_new_host
  _fake_scion "${LH_DIR}/a" "nightly-20261022"
  _layout install nightly-20261022 "${LH_DIR}/a"
  chmod 0700 "$LH_ROOT/releases"
  _fake_scion "${LH_DIR}/b" "nightly-20261023"
  _layout install nightly-20261023 "${LH_DIR}/b"
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "a releases/ with the wrong mode must be refused"
  assert_contains "$LAYOUT_OUT" "mode 0755" "the refusal must name the expected mode"
  assert_eq "700" "$(stat -c %a "$LH_ROOT/releases")" "the mode must be left as found, not chmod-ed"
  chmod 0755 "$LH_ROOT/releases"
  assert_eq "releases/nightly-20261022" "$(readlink "$LH_ROOT/current")" "current must not have moved"
  rm -rf "$LH_DIR"
}

# _layout_stat_stub DIR [WRONG_OWNER_PATH] [PLANT_PATH VICTIM] -- a PATH
# dir with a `stat` that reports like the real one, except:
#   - for WRONG_OWNER_PATH it reports the owner as "mallory";
#   - right after reporting on PLANT_PATH (the check passing), it swaps
#     PLANT_PATH for a symlink to VICTIM, simulating the owner winning the
#     race between root's check and any later use.
_layout_stat_stub() {
  mkdir -p "$1"
  cat > "$1/stat" <<EOF
#!/usr/bin/env bash
out="\$(/usr/bin/stat "\$@")" || exit \$?
path="\${!#}"
if [[ -n "${2:-}" && "\$path" == "${2:-}" ]]; then out="\${out/|\$(id -un)|/|mallory|}"; fi
printf '%s\n' "\$out"
if [[ -n "${3:-}" && "\$path" == "${3:-}" && ! -L "${3:-}" ]]; then
  mv "${3:-}" "${3:-}.moved" && ln -s "${4:-}" "${3:-}"
fi
EOF
  chmod 0755 "$1/stat"
}

test_layout_existing_tree_wrong_owner_is_refused() {
  _layout_new_host
  _fake_scion "${LH_DIR}/a" "nightly-20261022"
  _layout install nightly-20261022 "${LH_DIR}/a"
  _layout_stat_stub "${LH_DIR}/stub" "$LH_ROOT/snapshots"
  _fake_scion "${LH_DIR}/b" "nightly-20261023"
  LAYOUT_OUT="$(PATH="${LH_DIR}/stub:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
    bash "$LAYOUT_SH" install nightly-20261023 "${LH_DIR}/b" 2>&1)"
  LAYOUT_RC=$?
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "a subdir owned by someone else must be refused"
  assert_contains "$LAYOUT_OUT" "mallory" "the refusal must say who owns it"
  assert_eq "releases/nightly-20261022" "$(readlink "$LH_ROOT/current")" "current must not have moved"
  rm -rf "$LH_DIR"
}

# The race: as root (stub id -u = 0, owner configured), the owner swaps
# releases/ or snapshots/ for a symlink right after root's check. The
# runuser stub records the owner step and does NOT run it, so anything
# that reaches the victim was done by the root side. Nothing may: no
# write, no chmod (victim stays 0700), no chown.
test_layout_root_side_never_uses_tree_after_check() {
  local sub victim stub log
  for sub in releases snapshots; do
    _layout_new_host
    _fake_scion "${LH_DIR}/a" "nightly-20261022"
    _layout install nightly-20261022 "${LH_DIR}/a"
    victim="${LH_DIR}/victim"
    mkdir -p "$victim"
    chmod 0700 "$victim"
    stub="${LH_DIR}/stub"
    log="${LH_DIR}/calls.log"
    _layout_stat_stub "$stub" "" "$LH_ROOT/$sub" "$victim"
    cat > "${stub}/id" <<'STUB'
#!/usr/bin/env bash
if [[ "${1:-}" == "-u" && $# -eq 1 ]]; then echo 0; exit 0; fi
exec /usr/bin/id "$@"
STUB
    cat > "${stub}/runuser" <<STUB
#!/usr/bin/env bash
echo "runuser \$*" >> "$log"
exit 0
STUB
    cat > "${stub}/chown" <<STUB
#!/usr/bin/env bash
echo "chown \$*" >> "$log"
exec /usr/bin/chown "\$@"
STUB
    chmod 0755 "${stub}/id" "${stub}/runuser" "${stub}/chown"
    _fake_scion "${LH_DIR}/b" "nightly-20261023"
    LAYOUT_OUT="$(PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" \
      SCION_INSTALL_OWNER="$(id -un):$(id -gn)" bash "$LAYOUT_SH" install nightly-20261023 "${LH_DIR}/b" 2>&1)"
    LAYOUT_RC=$?
    assert_eq "0" "$LAYOUT_RC" "the root side must complete (output: ${LAYOUT_OUT})"
    assert_true "$([[ -L "$LH_ROOT/$sub" ]] && echo true || echo false)" "sanity: the ${sub} plant must have happened after the check"
    assert_contains "$(cat "$log" 2>/dev/null)" "__as-owner nightly-20261023 ${TMPDIR:-/tmp}/scion-layout-input." \
      "the release must be handed to the owner step"
    assert_eq "" "$(ls -A "$victim")" "root must not write through ${sub} planted after its check"
    assert_eq "700" "$(stat -c %a "$victim")" "root must not chmod through ${sub} planted after its check"
    assert_not_contains "$(cat "$log" 2>/dev/null)" "chown" "root must not chown anything in an existing tree"
    rm -rf "$LH_DIR"
  done
}

# Fresh host: the tree is built while <root> is still root's and handed to
# the owner last, <root> itself at the very end.
test_layout_fresh_tree_handed_over_last() {
  _layout_new_host
  local stub="${LH_DIR}/stub" log="${LH_DIR}/calls.log"
  mkdir -p "$stub"
  cat > "${stub}/chown" <<STUB
#!/usr/bin/env bash
echo "chown \$*" >> "$log"
exec /usr/bin/chown "\$@"
STUB
  chmod 0755 "${stub}/chown"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  LAYOUT_OUT="$(PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" \
    SCION_INSTALL_OWNER="$(id -un):$(id -gn)" bash "$LAYOUT_SH" install nightly-20261023 "${LH_DIR}/new" 2>&1)"
  LAYOUT_RC=$?
  assert_eq "0" "$LAYOUT_RC" "a fresh install with an owner set must succeed (output: ${LAYOUT_OUT})"
  local calls
  calls="$(cat "$log" 2>/dev/null)"
  assert_eq "3" "$(grep -c '^chown' <<< "$calls")" "exactly <root>, releases and snapshots are chowned"
  assert_eq "chown -h $(id -un):$(id -gn) ${LH_ROOT}" "$(tail -1 <<< "$calls")" "<root> must be handed over last"
  assert_eq "755" "$(stat -c %a "$LH_ROOT/releases")" "releases/ must be 0755"
  assert_eq "755" "$(stat -c %a "$LH_ROOT/snapshots")" "snapshots/ must be 0755"
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

# As root with an owner configured, install and adopt hand the tree work to
# the owner through runuser (stub: records, then runs the command as the
# current user) and still produce the full layout -- the re-run of the
# script from stdin carries its arguments and settings.
test_layout_owner_step_runs_through_runuser() {
  _layout_new_host
  local stub="${LH_DIR}/stub" log="${LH_DIR}/calls.log"
  mkdir -p "$stub"
  cat > "${stub}/id" <<'STUB'
#!/usr/bin/env bash
if [[ "${1:-}" == "-u" && $# -eq 1 && -z "${LAYOUT_STUB_DROPPED:-}" ]]; then echo 0; exit 0; fi
exec /usr/bin/id "$@"
STUB
  cat > "${stub}/runuser" <<STUB
#!/usr/bin/env bash
[[ "\$1" == "-u" && "\$3" == "--" ]] || { echo "runuser stub: unexpected args: \$*" >&2; exit 2; }
echo "runuser \$*" >> "$log"
shift 3
LAYOUT_STUB_DROPPED=1 exec "\$@"
STUB
  chmod 0755 "${stub}/id" "${stub}/runuser"
  _fake_scion "$LH_BIN" "nightly-20261001"
  LAYOUT_OUT="$(PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" \
    SCION_INSTALL_OWNER="$(id -un):$(id -gn)" bash "$LAYOUT_SH" adopt 2>&1)"
  LAYOUT_RC=$?
  assert_eq "0" "$LAYOUT_RC" "adopt as root must succeed through the owner step (output: ${LAYOUT_OUT})"
  assert_contains "$(cat "$log")" "-- env SCION_INSTALL_ROOT=${LH_ROOT} SCION_INSTALL_OWNER= bash -s -- __as-owner nightly-20261001 " \
    "the tree work must run as the owner via runuser"
  assert_eq "releases/nightly-20261001" "$(readlink "$LH_ROOT/current")" "the owner step must have activated the release"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  LAYOUT_OUT="$(PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" \
    SCION_INSTALL_OWNER="$(id -un):$(id -gn)" bash "$LAYOUT_SH" install nightly-20261023 "${LH_DIR}/new" 2>&1)"
  assert_eq "0" "$?" "install as root must succeed through the owner step (output: ${LAYOUT_OUT})"
  assert_eq "releases/nightly-20261023" "$(readlink "$LH_ROOT/current")" "current must move to the new release"
  assert_eq "releases/nightly-20261001" "$(readlink "$LH_ROOT/previous")" "previous must be set by the owner step"
  assert_eq "$LH_ROOT/current/scion" "$(readlink "$LH_BIN")" "root must link the CLI path"
  rm -rf "$LH_DIR"
}

# =====================================================================
# G1 review round 1
# =====================================================================

# _layout_root_stubs DIR LOG -- stub id (uid 0 until dropped) and a
# runuser that logs its args plus the mode/owner of every existing file
# argument and of its directory, then runs the command as the current user.
_layout_root_stubs() {
  mkdir -p "$1"
  cat > "$1/id" <<'STUB'
#!/usr/bin/env bash
if [[ "${1:-}" == "-u" && $# -eq 1 && -z "${LAYOUT_STUB_DROPPED:-}" ]]; then echo 0; exit 0; fi
exec /usr/bin/id "$@"
STUB
  cat > "$1/runuser" <<STUB
#!/usr/bin/env bash
[[ "\$1" == "-u" && "\$3" == "--" ]] || { echo "runuser stub: unexpected args: \$*" >&2; exit 2; }
echo "runuser \$*" >> "$2"
for a in "\$@"; do
  if [[ "\$a" == /* && -f "\$a" ]]; then
    echo "input \$a file=\$(stat -c %a "\$a") dir=\$(stat -c %a "\$(dirname "\$a")")" >> "$2"
  fi
done
shift 3
LAYOUT_STUB_DROPPED=1 exec "\$@"
STUB
  chmod 0755 "$1/id" "$1/runuser"
}

# Under a restrictive umask the input binary is 0700 and given by a
# relative path: root must hand the owner step an absolute 0755 copy in a
# 0711 directory outside <root>, and clean it up afterwards.
test_layout_owner_step_gets_readable_input_copy() {
  _layout_new_host
  local stub="${LH_DIR}/stub" log="${LH_DIR}/calls.log" calls
  _layout_root_stubs "$stub" "$log"
  (umask 077 && _fake_scion "${LH_DIR}/new" "nightly-20261023" && chmod 0700 "${LH_DIR}/new")
  assert_eq "700" "$(stat -c %a "${LH_DIR}/new")" "setup: the input binary must be 0700"
  LAYOUT_OUT="$(cd "$LH_DIR" && umask 077 && PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" \
    SCION_INSTALL_OWNER="$(id -un):$(id -gn)" bash "$LAYOUT_SH" install nightly-20261023 new 2>&1)"
  LAYOUT_RC=$?
  calls="$(cat "$log" 2>/dev/null)"
  assert_eq "0" "$LAYOUT_RC" "install with a 0700 relative input must succeed (output: ${LAYOUT_OUT})"
  assert_contains "$calls" "__as-owner nightly-20261023 ${TMPDIR:-/tmp}/scion-layout-input." \
    "the owner step must get an absolute path to a copy outside <root>"
  assert_contains "$calls" "/nightly-20261023 file=755 dir=711" "the copy must be 0755 in a 0711 directory"
  assert_not_contains "$calls" "__as-owner nightly-20261023 new" "the original relative path must not be handed over"
  assert_true "$(cmp -s "${LH_DIR}/new" "$LH_ROOT/releases/nightly-20261023/scion" && echo true || echo false)" \
    "the installed release must be the input binary"
  assert_eq "" "$(find "${TMPDIR:-/tmp}" -maxdepth 1 -name 'scion-layout-input.*')" "the input copy dir must be removed"
  rm -rf "$LH_DIR"
}

test_layout_unreadable_input_has_clear_error() {
  local input case_name
  for case_name in mode-000 missing; do
    _layout_new_host
    mkdir -p "$(dirname "$LH_ROOT")"
    mkdir -m 0755 "$LH_ROOT" "$LH_ROOT/releases" "$LH_ROOT/snapshots"
    input="${LH_DIR}/input"
    if [[ "$case_name" == mode-000 ]]; then
      # The case the message names: a file the owner step cannot read.
      # (This runs unprivileged; as root, a 000 file would still be readable.)
      _fake_scion "$input" "nightly-20261023"
      chmod 000 "$input"
      assert_false "$([[ -r "$input" ]] && echo true || echo false)" "setup: the 000 input must be unreadable to the test user"
    fi
    LAYOUT_OUT="$(SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
      bash "$LAYOUT_SH" __as-owner nightly-20261023 "$input" 2>&1)"
    LAYOUT_RC=$?
    assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "an unreadable (${case_name}) input must fail"
    assert_contains "$LAYOUT_OUT" "could not copy" "a ${case_name} read failure must be reported as such"
    assert_not_contains "$LAYOUT_OUT" "incomplete" "a ${case_name} read failure must not be reported as an incomplete copy"
    assert_eq "" "$(ls -A "$LH_ROOT/releases")" "${case_name}: nothing may be left in releases/"
    chmod 0644 "$input" 2>/dev/null || true
    rm -rf "$LH_DIR"
  done
}

# A fresh handover that failed partway (<root> still root's, snapshots/
# missing) is finished on the next run instead of refused -- but only when
# <root>'s parent is root's too. (stat stub reports root as the owner.)
test_layout_resumes_partial_fresh_handover() {
  local parent_owner
  for parent_owner in root mallory; do
    _layout_new_host
    mkdir -p "$(dirname "$LH_ROOT")"
    mkdir -m 0755 "$LH_ROOT" "$LH_ROOT/releases"
    local stub="${LH_DIR}/stub"
    mkdir -p "$stub"
    cat > "${stub}/stat" <<EOF
#!/usr/bin/env bash
out="\$(/usr/bin/stat "\$@")" || exit \$?
path="\${!#}"
me="\$(/usr/bin/id -un)"
if [[ "\$path" == "$LH_ROOT" ]]; then out="\${out//\$me/root}"; fi
if [[ "\$path" == "$(dirname "$LH_ROOT")" ]]; then out="\${out//\$me/$parent_owner}"; fi
printf '%s\n' "\$out"
EOF
    chmod 0755 "${stub}/stat"
    _fake_scion "${LH_DIR}/new" "nightly-20261023"
    LAYOUT_OUT="$(PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
      bash "$LAYOUT_SH" install nightly-20261023 "${LH_DIR}/new" 2>&1)"
    LAYOUT_RC=$?
    if [[ "$parent_owner" == root ]]; then
      assert_eq "0" "$LAYOUT_RC" "a root-owned <root> in a root-owned parent must be resumed (output: ${LAYOUT_OUT})"
      assert_contains "$LAYOUT_OUT" "resumed the handover of" "the resume must be reported"
      assert_true "$([[ -d "$LH_ROOT/snapshots" ]] && echo true || echo false)" "the missing snapshots/ must be created"
      assert_eq "releases/nightly-20261023" "$(readlink "$LH_ROOT/current")" "the install must complete"
    else
      assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" \
        "a root-owned <root> whose parent is not root's must be refused, not resumed"
      assert_contains "$LAYOUT_OUT" "is owned by mallory, not root" "the refusal must name the unsafe parent, not <root>"
    fi
    rm -rf "$LH_DIR"
  done
}

# If the owner step dies between moving the old release aside and moving
# the new one in, the leftover ends in .partial, which the hub's release
# listing ignores. (mv stub fails the second rename.)
test_layout_replaced_release_aside_name_is_partial() {
  _layout_new_host
  _fake_scion "${LH_DIR}/a" "nightly-20261022"
  _layout install nightly-20261022 "${LH_DIR}/a"
  local stub="${LH_DIR}/stub"
  mkdir -p "$stub"
  cat > "${stub}/mv" <<EOF
#!/usr/bin/env bash
if [[ "\$*" == *"releases/nightly-20261022.partial $LH_ROOT/releases/nightly-20261022" ]]; then
  echo "mv stub: simulated failure" >&2; exit 1
fi
exec /usr/bin/mv "\$@"
EOF
  chmod 0755 "${stub}/mv"
  _fake_scion "${LH_DIR}/b" "nightly-20261022-rebuilt"
  LAYOUT_OUT="$(PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
    bash "$LAYOUT_SH" install nightly-20261022 "${LH_DIR}/b" 2>&1)"
  LAYOUT_RC=$?
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "sanity: the simulated rename failure must stop the run"
  assert_true "$([[ -d "$LH_ROOT/releases/nightly-20261022.old.partial" ]] && echo true || echo false)" \
    "the old release moved aside must be named <tag>.old.partial"
  assert_eq "" "$(find "$LH_ROOT/releases" -mindepth 1 -maxdepth 1 ! -name '*.partial')" \
    "every leftover in releases/ must end in .partial (ignored by the hub)"
  rm -rf "$LH_DIR"
}

test_layout_as_owner_step_refuses_direct_root_use() {
  _layout_new_host
  local stub="${LH_DIR}/stub" log="${LH_DIR}/calls.log"
  _layout_root_stubs "$stub" "$log"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  LAYOUT_OUT="$(PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" \
    SCION_INSTALL_OWNER="$(id -un):$(id -gn)" bash "$LAYOUT_SH" __as-owner nightly-20261023 "${LH_DIR}/new" 2>&1)"
  LAYOUT_RC=$?
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "__as-owner run directly as root must be refused"
  assert_contains "$LAYOUT_OUT" "internal step" "the refusal must say why"
  assert_false "$([[ -e "$LH_ROOT" ]] && echo true || echo false)" "nothing may be created"
  rm -rf "$LH_DIR"
}

# =====================================================================
# Fix-delta audit F2/F3
# =====================================================================

# F2: adopt where <bin-link> leads through <root> (<root>/current is a
# plain directory, so the host is not "converted"). The owner swaps
# <root>/current/scion for a symlink to another file right after its
# version is read (the fake binary does the swap while answering
# `version`, which binary_tag runs as the owner). Root must never read
# through that path: its copy command (install, stubbed to log) is never
# pointed into <root>, and the owner step gets the path to read itself.
test_layout_adopt_never_reads_owner_path_as_root() {
  _layout_new_host
  local stub="${LH_DIR}/stub" log="${LH_DIR}/calls.log" secret="${LH_DIR}/secret"
  _layout_root_stubs "$stub" "$log"
  printf '#!/usr/bin/env bash\necho "install $*" >> "%s"\nexec /usr/bin/install "$@"\n' "$log" > "${stub}/install"
  chmod 0755 "${stub}/install"
  echo "not-a-scion-binary" > "$secret"
  mkdir -p "$(dirname "$LH_ROOT")"
  mkdir -m 0755 "$LH_ROOT" "$LH_ROOT/releases" "$LH_ROOT/snapshots" "$LH_ROOT/current"
  # shellcheck disable=SC2016 # script text written to a stub file
  printf '#!/bin/sh\nif [ "$1" = version ]; then\n  rm -f "%s" && ln -s "%s" "%s"\n  printf %s\nfi\n' \
    "$LH_ROOT/current/scion" "$secret" "$LH_ROOT/current/scion" \
    "'{\n  \"commit\": \"0123456789abcdef0123456789abcdef01234567\",\n  \"version\": \"nightly-20261020\"\n}\n'" \
    > "$LH_ROOT/current/scion"
  chmod 0755 "$LH_ROOT/current/scion"
  ln -s "$LH_ROOT/current/scion" "$LH_BIN"
  LAYOUT_OUT="$(PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" \
    SCION_INSTALL_OWNER="$(id -un):$(id -gn)" bash "$LAYOUT_SH" adopt 2>&1)"
  LAYOUT_RC=$?
  local calls
  calls="$(cat "$log" 2>/dev/null)"
  assert_true "$([[ -L "$LH_ROOT/current/scion" ]] && echo true || echo false)" \
    "sanity: the owner's swap must have happened after the version was read (output: ${LAYOUT_OUT})"
  assert_not_contains "$calls" "install -m 0755 -- ${LH_ROOT}/" "root must not copy (read) a file reached through <root>"
  assert_contains "$calls" "__as-owner nightly-20261020 ${LH_ROOT}/current/scion" \
    "the owner step must be given the path to read itself"
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" \
    "the adopt must still stop: a plain-directory current is refused by the owner step"
  assert_contains "$LAYOUT_OUT" "current exists but is not a symlink" "the refusal must come from the owner step"
  rm -rf "$LH_DIR"
}

# F2, the normal old-layout case: <bin-link> is a plain binary in root's
# own directory, so root copies it (it may be 0700) for the owner step.
test_layout_adopt_plain_bin_link_is_copied_by_root() {
  _layout_new_host
  local stub="${LH_DIR}/stub" log="${LH_DIR}/calls.log"
  _layout_root_stubs "$stub" "$log"
  _fake_scion "$LH_BIN" "nightly-20261001"
  LAYOUT_OUT="$(PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" \
    SCION_INSTALL_OWNER="$(id -un):$(id -gn)" bash "$LAYOUT_SH" adopt 2>&1)"
  assert_eq "0" "$?" "adopt of a plain binary must succeed (output: ${LAYOUT_OUT})"
  assert_contains "$(cat "$log")" "__as-owner nightly-20261001 ${TMPDIR:-/tmp}/scion-layout-input." \
    "a plain <bin-link> is copied by root and handed over as a 0755 copy"
  assert_eq "releases/nightly-20261001" "$(readlink "$LH_ROOT/current")" "the adopted release must be current"
  rm -rf "$LH_DIR"
}

# F3: the fresh-create path checks <root>'s parent like the resume path:
# it must not be writable by group or others, nor owned by another user.
test_layout_fresh_create_requires_safe_parent() {
  _layout_new_host
  mkdir -p "$(dirname "$LH_ROOT")"
  chmod 0775 "$(dirname "$LH_ROOT")"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  _layout install nightly-20261023 "${LH_DIR}/new"
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "a group-writable parent must be refused"
  assert_contains "$LAYOUT_OUT" "not writable by group or others" "the refusal must say why"
  assert_false "$([[ -e "$LH_ROOT" ]] && echo true || echo false)" "nothing may be created under an unsafe parent"
  rm -rf "$LH_DIR"

  _layout_new_host
  mkdir -p "$(dirname "$LH_ROOT")"
  local stub="${LH_DIR}/stub" parent
  parent="$(dirname "$LH_ROOT")"
  mkdir -p "$stub"
  # shellcheck disable=SC2016 # script text written to a stub file
  printf '#!/usr/bin/env bash\nout="$(/usr/bin/stat "$@")" || exit $?\nif [[ "${!#}" == "%s" ]]; then out="${out//$(/usr/bin/id -un)/mallory}"; fi\nprintf "%%s\\n" "$out"\n' \
    "$parent" > "${stub}/stat"
  chmod 0755 "${stub}/stat"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  LAYOUT_OUT="$(PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
    bash "$LAYOUT_SH" install nightly-20261023 "${LH_DIR}/new" 2>&1)"
  LAYOUT_RC=$?
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "a parent owned by another user must be refused (output: ${LAYOUT_OUT})"
  assert_contains "$LAYOUT_OUT" "is owned by mallory, not root" "the refusal must say why"
  assert_false "$([[ -e "$LH_ROOT" ]] && echo true || echo false)" "nothing may be created under a parent owned by another user"
  rm -rf "$LH_DIR"
}

# =====================================================================
# F2/F3 review round 1: whole ancestor chains, plain bin link dir
# =====================================================================

# A <bin-link> whose directory another user controls is refused up front:
# root writes <bin-link> there (link_bin) and would read a plain binary
# from it. Nothing changes. (stat stub: the bin dir is mallory's.)
test_layout_unsafe_bin_dir_is_refused_before_any_change() {
  local cmd
  for cmd in adopt install; do
    _layout_new_host
    local stub="${LH_DIR}/stub" log="${LH_DIR}/calls.log" bindir
    bindir="$(dirname "$LH_BIN")"
    _layout_root_stubs "$stub" "$log"
    # shellcheck disable=SC2016 # script text written to a stub file
    printf '#!/usr/bin/env bash\nout="$(/usr/bin/stat "$@")" || exit $?\nif [[ "${!#}" == "%s" ]]; then out="${out//$(/usr/bin/id -un)/mallory}"; fi\nprintf "%%s\\n" "$out"\n' \
      "$bindir" > "${stub}/stat"
    chmod 0755 "${stub}/stat"
    _fake_scion "$LH_BIN" "nightly-20261001"
    _fake_scion "${LH_DIR}/new" "nightly-20261023"
    local args=(adopt)
    [[ "$cmd" == install ]] && args=(install nightly-20261023 "${LH_DIR}/new")
    LAYOUT_OUT="$(PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" \
      SCION_INSTALL_OWNER="$(id -un):$(id -gn)" bash "$LAYOUT_SH" "${args[@]}" 2>&1)"
    LAYOUT_RC=$?
    assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "${cmd} with an unsafe bin dir must be refused"
    assert_contains "$LAYOUT_OUT" "${bindir} is owned by mallory, not root" "the ${cmd} refusal must name the bin dir"
    assert_false "$([[ -e "$LH_ROOT" ]] && echo true || echo false)" "${cmd}: nothing may change before the refusal"
    assert_false "$([[ -L "$LH_BIN" ]] && echo true || echo false)" "${cmd}: the CLI binary must be left alone"
    rm -rf "$LH_DIR"
  done
}

# R2 + N2 + N3: every ancestor of <root> counts, a symlinked ancestor is
# named as such, and nothing is created before the check.
test_layout_fresh_create_checks_whole_ancestor_chain() {
  # Unsafe grandparent (group-writable), safe existing parent.
  _layout_new_host
  mkdir -p "$(dirname "$LH_ROOT")"
  chmod 0755 "$(dirname "$LH_ROOT")"
  chmod 0775 "$LH_DIR"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  _layout install nightly-20261023 "${LH_DIR}/new"
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "a group-writable grandparent must be refused"
  assert_contains "$LAYOUT_OUT" "${LH_DIR} is writable by group or others" "the refusal must name the unsafe ancestor"
  assert_false "$([[ -e "$LH_ROOT" ]] && echo true || echo false)" "nothing may be created under an unsafe ancestor"
  chmod 0700 "$LH_DIR"
  rm -rf "$LH_DIR"

  # N3: the parent does not exist yet and the grandparent is unsafe: the
  # check runs on the nearest existing ancestor first, so the parent is
  # never created.
  _layout_new_host
  chmod 0775 "$LH_DIR"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  _layout install nightly-20261023 "${LH_DIR}/new"
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "a missing parent under an unsafe ancestor must be refused"
  assert_false "$([[ -e "$(dirname "$LH_ROOT")" ]] && echo true || echo false)" "the missing parent must not be created before the check"
  chmod 0700 "$LH_DIR"
  rm -rf "$LH_DIR"

  # N2: a symlinked parent is refused and named as a symlink.
  _layout_new_host
  mkdir -m 0755 "${LH_DIR}/real-opt"
  ln -s "${LH_DIR}/real-opt" "$(dirname "$LH_ROOT")"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  _layout install nightly-20261023 "${LH_DIR}/new"
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "a symlinked parent must be refused"
  assert_contains "$LAYOUT_OUT" "$(dirname "$LH_ROOT") is a symlink" "the refusal must say it is a symlink"
  assert_eq "" "$(ls -A "${LH_DIR}/real-opt")" "nothing may be created through the symlinked parent"
  rm -rf "$LH_DIR"

  # A sticky world-writable directory (like /tmp) is fine ABOVE a strictly
  # safe one: the existing parent is ours and 0755, the grandparent 1777.
  _layout_new_host
  mkdir -p "$(dirname "$LH_ROOT")"
  chmod 0755 "$(dirname "$LH_ROOT")"
  chmod 1777 "$LH_DIR"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  _layout install nightly-20261023 "${LH_DIR}/new"
  assert_eq "0" "$LAYOUT_RC" "a sticky ancestor above a safe parent must be accepted (output: ${LAYOUT_OUT})"
  chmod 0700 "$LH_DIR"
  rm -rf "$LH_DIR"

  # But nothing is created directly under a sticky directory: there, others
  # can create and swap their own entries (R2b).
  _layout_new_host
  chmod 1777 "$LH_DIR"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  _layout install nightly-20261023 "${LH_DIR}/new"
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "a missing parent directly under a sticky dir must be refused"
  assert_contains "$LAYOUT_OUT" "${LH_DIR} is writable by group or others" "the refusal must name the sticky dir"
  assert_false "$([[ -e "$(dirname "$LH_ROOT")" ]] && echo true || echo false)" "nothing may be created under the sticky dir"
  chmod 0700 "$LH_DIR"
  rm -rf "$LH_DIR"
}

# FYI-2: chat plugins are extracted into a fresh mktemp directory and
# copied into the scion user's plugins directory by that user; root no
# longer moves or chowns anything there.
test_layout_deploy_plugin_installed_by_scion_user() {
  fresh_gcloud_state
  layout_deploy_through_settings "$(layout_config_json "$HUB" auto | sed 's/"chat_plugins": \[\]/"chat_plugins": ["telegram"]/')"
  local log
  log="$(gcloud_log)"
  # shellcheck disable=SC2016 # literals from the generated command
  assert_contains "$log" 'PLUGIN_EXTRACT_DIR="$(mktemp -d /tmp/scion-extract.XXXXXX)"' "plugins must be extracted into a fresh directory"
  # shellcheck disable=SC2016
  assert_contains "$log" 'sudo -u scion install -m 0755 "$PLUGIN_EXTRACT_DIR/scion-plugin-telegram" /home/scion/.scion/plugins/broker/scion-plugin-telegram' \
    "the plugin must be copied in by the scion user"
  assert_not_contains "$log" "sudo chown scion:scion /home/scion/.scion/plugins" "root must not chown inside the plugins directory"
  assert_not_contains "$log" "sudo mv /tmp/scion-plugin" "root must not move a fixed /tmp name into the plugins directory"
}

# =====================================================================
# F2/F3 review round 2
# =====================================================================

# R1: relative or non-canonical paths are refused at once (before, a
# relative path made the ancestor walk loop forever).
test_layout_refuses_non_canonical_paths() {
  local bad var out rc
  for var in SCION_INSTALL_ROOT SCION_BIN_LINK; do
    for bad in "scion" "opt2/scion" "/opt/../scion" "/opt//scion" "/opt/./scion" "/opt/scion/"; do
      out="$(env "$var=$bad" SCION_INSTALL_OWNER="" timeout 10 bash "$LAYOUT_SH" adopt 2>&1)"
      rc=$?
      assert_true "$([[ "$rc" -ne 0 && "$rc" -ne 124 ]] && echo true || echo false)" \
        "${var}='${bad}' must be refused promptly (rc ${rc}; 124 = timed out)"
      assert_contains "$out" "${var} must be an absolute path" "${var}='${bad}': the refusal must say why"
    done
  done
}

# R2(a): a plain <bin-link> in a safe directory, but owned by another user
# (who could swap it in a sticky or shared setup), is read by the owner
# step, not copied by root. (stat stub: the bin link file is mallory's.)
test_layout_adopt_bin_link_owned_by_other_user_is_read_by_owner() {
  _layout_new_host
  local stub="${LH_DIR}/stub" log="${LH_DIR}/calls.log"
  _layout_root_stubs "$stub" "$log"
  # shellcheck disable=SC2016 # script text written to a stub file
  printf '#!/usr/bin/env bash\nout="$(/usr/bin/stat "$@")" || exit $?\nif [[ "${!#}" == "%s" ]]; then out="${out//$(/usr/bin/id -un)/mallory}"; fi\nprintf "%%s\\n" "$out"\n' \
    "$LH_BIN" > "${stub}/stat"
  chmod 0755 "${stub}/stat"
  _fake_scion "$LH_BIN" "nightly-20261001"
  LAYOUT_OUT="$(PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" \
    SCION_INSTALL_OWNER="$(id -un):$(id -gn)" bash "$LAYOUT_SH" adopt 2>&1)"
  assert_eq "0" "$?" "the adopt must succeed through the owner step (output: ${LAYOUT_OUT})"
  assert_contains "$(cat "$log")" "__as-owner nightly-20261001 ${LH_BIN}" \
    "a bin link owned by another user must be handed to the owner as the raw path"
  assert_not_contains "$(cat "$log")" "scion-layout-input." "root must not copy it"
  rm -rf "$LH_DIR"
}

# R2(b): missing components are created one at a time with plain mkdir, so
# a symlink planted between two components is never followed. (mkdir
# stub: right after the first missing component is created, a symlink to
# another directory is planted where the next one goes.)
test_layout_creates_missing_parents_without_following_symlinks() {
  _layout_new_host
  local stub="${LH_DIR}/stub" elsewhere="${LH_DIR}/elsewhere" first
  mkdir -p "$stub" "$elsewhere"
  LH_ROOT="${LH_DIR}/a/b/scion"
  first="${LH_DIR}/a"
  # shellcheck disable=SC2016 # script text written to a stub file
  printf '#!/usr/bin/env bash\n/usr/bin/mkdir "$@" || exit $?\nif [[ "${!#}" == "%s" ]]; then ln -s "%s" "%s/b"; fi\n' \
    "$first" "$elsewhere" "$first" > "${stub}/mkdir"
  chmod 0755 "${stub}/mkdir"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  LAYOUT_OUT="$(PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
    bash "$LAYOUT_SH" install nightly-20261023 "${LH_DIR}/new" 2>&1)"
  LAYOUT_RC=$?
  assert_true "$([[ -L "${first}/b" ]] && echo true || echo false)" "sanity: the symlink must have been planted between components"
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "creation through a planted symlink must be refused"
  assert_eq "" "$(ls -A "$elsewhere")" "nothing may be created through the planted symlink"
  rm -rf "$LH_DIR"

  # Without interference, several missing components are created, 0755.
  _layout_new_host
  LH_ROOT="${LH_DIR}/a/b/scion"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  _layout install nightly-20261023 "${LH_DIR}/new"
  assert_eq "0" "$LAYOUT_RC" "missing parents must be created (output: ${LAYOUT_OUT})"
  assert_eq "755 755" "$(stat -c %a "${LH_DIR}/a" "${LH_DIR}/a/b" | tr '\n' ' ' | sed 's/ $//')" "created parents must be 0755"
  rm -rf "$LH_DIR"
}

# FYI-1: a leftover <bin-link>.new that is a symlink to a directory must
# not make root create the link inside that directory (ln -sfnT).
test_layout_link_bin_ignores_planted_dot_new_dir_link() {
  _layout_new_host
  local elsewhere="${LH_DIR}/elsewhere"
  mkdir -p "$elsewhere"
  ln -s "$elsewhere" "${LH_BIN}.new"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  _layout install nightly-20261023 "${LH_DIR}/new"
  assert_eq "0" "$LAYOUT_RC" "the install must succeed (output: ${LAYOUT_OUT})"
  assert_eq "$LH_ROOT/current/scion" "$(readlink "$LH_BIN")" "the CLI link must be right"
  assert_eq "" "$(ls -A "$elsewhere")" "no link may be created inside the planted directory"
  rm -rf "$LH_DIR"
}

# The directory itself must be strictly safe: sticky is only accepted for
# ancestors above it. A sticky (1777) bin dir, or a sticky existing parent
# of <root>, is refused: others can create and swap their own entries in
# it (R2).
test_layout_sticky_dir_itself_is_not_safe() {
  _layout_new_host
  chmod 1777 "$(dirname "$LH_BIN")"
  _fake_scion "$LH_BIN" "nightly-20261001"
  _layout adopt
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "a sticky bin dir must be refused"
  assert_contains "$LAYOUT_OUT" "$(dirname "$LH_BIN") is writable by group or others" "the refusal must name the sticky bin dir"
  assert_false "$([[ -e "$LH_ROOT" ]] && echo true || echo false)" "nothing may change"
  chmod 0755 "$(dirname "$LH_BIN")"
  rm -rf "$LH_DIR"

  _layout_new_host
  mkdir -p "$(dirname "$LH_ROOT")"
  chmod 1777 "$(dirname "$LH_ROOT")"
  _fake_scion "${LH_DIR}/new" "nightly-20261023"
  _layout install nightly-20261023 "${LH_DIR}/new"
  assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "a sticky existing parent of <root> must be refused"
  assert_false "$([[ -e "$LH_ROOT" ]] && echo true || echo false)" "<root> must not be created in a sticky parent"
  chmod 0755 "$(dirname "$LH_ROOT")"
  rm -rf "$LH_DIR"
}

# The resume path finishes a handover only if <root> itself is strictly
# safe too: a root-owned <root> that is group-writable (so others may have
# planted entries) is refused and left untouched, not chmod-ed first; a
# symlinked <root> is refused as well. (stat stub: <root> and its parent
# report root as the owner.)
test_layout_resume_requires_strictly_safe_root() {
  local kind
  for kind in writable symlink; do
    _layout_new_host
    mkdir -p "$(dirname "$LH_ROOT")"
    local real="${LH_DIR}/real-root"
    if [[ "$kind" == writable ]]; then
      mkdir -m 0755 "$LH_ROOT" "$LH_ROOT/releases"
      chmod 0775 "$LH_ROOT"
    else
      mkdir -m 0755 "$real"
      ln -s "$real" "$LH_ROOT"
    fi
    local stub="${LH_DIR}/stub"
    mkdir -p "$stub"
    # shellcheck disable=SC2016 # script text written to a stub file
    printf '#!/usr/bin/env bash\nout="$(/usr/bin/stat "$@")" || exit $?\nme="$(/usr/bin/id -un)"\nif [[ "${!#}" == "%s" || "${!#}" == "%s" ]]; then out="${out//$me/root}"; fi\nprintf "%%s\\n" "$out"\n' \
      "$LH_ROOT" "$(dirname "$LH_ROOT")" > "${stub}/stat"
    chmod 0755 "${stub}/stat"
    _fake_scion "${LH_DIR}/new" "nightly-20261023"
    LAYOUT_OUT="$(PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
      bash "$LAYOUT_SH" install nightly-20261023 "${LH_DIR}/new" 2>&1)"
    LAYOUT_RC=$?
    assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "a ${kind} root-owned <root> must not be resumed"
    assert_not_contains "$LAYOUT_OUT" "resumed the handover" "${kind}: no resume may be reported"
    if [[ "$kind" == writable ]]; then
      assert_contains "$LAYOUT_OUT" "is writable by group or others" "the refusal must say why"
      assert_eq "775" "$(stat -c %a "$LH_ROOT")" "<root> must be left as found, not chmod-ed"
      assert_false "$([[ -e "$LH_ROOT/snapshots" ]] && echo true || echo false)" "nothing may be created inside it"
    else
      assert_eq "" "$(ls -A "$real")" "nothing may be created through the symlinked <root>"
    fi
    rm -rf "$LH_DIR"
  done
}

# =====================================================================
# Fix3: hub.env install, settings file owner
# =====================================================================

# _layout_ssh_cmd_with LOG MARKER -- the --command body of the (first)
# compute ssh call whose command contains MARKER.
_layout_ssh_cmd_with() {
  printf '%s\n' "$1" | awk -v marker="$2" '
    /^compute / { if (on && index(buf, marker)) { printf "%s", buf; done = 1; exit } on = 0 }
    /^compute ssh .*--command=$/ { buf = ""; on = 1; next }
    on { buf = buf $0 "\n" }
    END { if (!done && on && index(buf, marker)) printf "%s", buf }
  '
}

# hub.env is staged in a fresh private directory on the VM (never the
# fixed /tmp/hub.env) and written into ~scion/.scion by the scion user;
# root never moves, chowns or chmods it there.
test_layout_hub_env_staged_privately_and_written_by_scion() {
  fresh_gcloud_state
  layout_deploy_through_settings "$(layout_config_json "$HUB" notify)"
  assert_eq "true" "$LAYOUT_REACHED_SETTINGS" "deploy must reach the settings write (log: ${DEPLOY_LOG: -400})"
  local log cmd
  log="$(gcloud_log)"
  cmd="$(_layout_ssh_cmd_with "$log" "hub.env.new")"
  assert_contains "$log" "--command=mktemp -d /tmp/scion-hubenv.XXXXXX" "a private staging dir must be created on the VM"
  assert_contains "$log" "scion-hub-${HUB}:/tmp/scion-hubenv.STUB01/hub.env" "hub.env must be copied into the private dir"
  assert_not_contains "$log" "/tmp/hub.env" "the fixed /tmp/hub.env name must not be used anywhere"
  assert_contains "$cmd" "sudo -u scion sh -c 'umask 077 && cat > /home/scion/.scion/hub.env.new" \
    "the scion user must write hub.env itself"
  assert_contains "$cmd" "< /tmp/scion-hubenv.STUB01/hub.env" "the content must come from the private dir"
  assert_contains "$cmd" "trap 'rm -rf /tmp/scion-hubenv.STUB01' EXIT" "the private dir must be removed on exit"
  assert_not_contains "$log" "sudo mv /tmp" "root must not move a /tmp file into place"
  assert_not_contains "$log" "sudo chown scion:scion /home/scion/.scion/hub.env" "root must not chown hub.env"
}

test_layout_hub_env_refuses_unexpected_staging_dir() {
  local bad
  for bad in "" "/tmp/../etc" "/tmp/hub.env"; do
    fresh_gcloud_state
    export GCLOUD_STUB_SSH_HUBENV_DIR="$bad"
    layout_deploy_through_settings "$(layout_config_json "$HUB" notify)"
    unset GCLOUD_STUB_SSH_HUBENV_DIR
    assert_eq "false" "$LAYOUT_REACHED_SETTINGS" "staging dir '${bad}': deploy must stop"
    assert_contains "$DEPLOY_LOG" "Could not create a private staging directory" "staging dir '${bad}': the reason must be given"
    assert_not_contains "$(gcloud_log)" "hub.env.new" "staging dir '${bad}': nothing may be written"
  done
}

# Run the generated hub.env command for real (a sudo shim drops "-u
# scion"; /tmp and ~scion/.scion point into a work dir) with the old fixed
# name /tmp/hub.env pre-planted as a symlink to a victim file: it must be
# ignored, the real content must land with mode 0600, and the private dir
# must be removed.
test_layout_hub_env_command_ignores_planted_tmp_name() {
  fresh_gcloud_state
  layout_deploy_through_settings "$(layout_config_json "$HUB" notify)"
  local cmd work
  cmd="$(_layout_ssh_cmd_with "$(gcloud_log)" "hub.env.new")"
  work="$(mktemp -d)"
  mkdir -p "${work}/tmp/scion-hubenv.STUB01" "${work}/home/.scion" "${work}/shim"
  printf 'SCION_SERVER_SESSION_SECRET=real\n' > "${work}/tmp/scion-hubenv.STUB01/hub.env"
  echo "victim" > "${work}/victim"
  ln -s "${work}/victim" "${work}/tmp/hub.env"
  # shellcheck disable=SC2016 # script text written to a stub file
  printf '#!/usr/bin/env bash\n[[ "$1" == "-u" ]] && shift 2\nexec "$@"\n' > "${work}/shim/sudo"
  chmod 0755 "${work}/shim/sudo"
  # /tmp first: WORK itself lives under /tmp, so rewriting /tmp after the
  # home path would mangle it.
  cmd="${cmd//\/tmp\//${work}/tmp/}"
  cmd="${cmd//\/home\/scion\/.scion/${work}/home/.scion}"
  local out rc
  out="$(PATH="${work}/shim:${PATH}" bash -c "$cmd" 2>&1)"
  rc=$?
  assert_eq "0" "$rc" "the generated hub.env command must succeed (output: ${out})"
  assert_eq "SCION_SERVER_SESSION_SECRET=real" "$(cat "${work}/home/.scion/hub.env" 2>/dev/null)" "hub.env must hold the staged content"
  assert_eq "600" "$(stat -c %a "${work}/home/.scion/hub.env" 2>/dev/null)" "hub.env must be 0600"
  assert_eq "victim" "$(cat "${work}/victim")" "the planted /tmp/hub.env symlink target must be untouched"
  assert_false "$([[ -e "${work}/tmp/scion-hubenv.STUB01" ]] && echo true || echo false)" "the private staging dir must be removed"
  rm -rf "$work"
}

# set-maintenance edits only a settings file owned by the install owner:
# a root-owned one (deploy.sh never creates such a file) or one owned by
# another user is refused and left unchanged. (stat stub reports the owner.)
test_layout_settings_refuses_file_not_owned_by_install_owner() {
  local who
  for who in root mallory; do
    _layout_new_host
    _deploy_rendered_settings auto > "${LH_DIR}/settings.yaml"
    cp "${LH_DIR}/settings.yaml" "${LH_DIR}/orig.yaml"
    local stub="${LH_DIR}/stub"
    mkdir -p "$stub"
    # shellcheck disable=SC2016 # script text written to a stub file
    printf '#!/usr/bin/env bash\nout="$(/usr/bin/stat "$@")" || exit $?\nif [[ "${!#}" == "%s" ]]; then out="${out//$(/usr/bin/id -un)/%s}"; fi\nprintf "%%s\\n" "$out"\n' \
      "${LH_DIR}/settings.yaml" "$who" > "${stub}/stat"
    chmod 0755 "${stub}/stat"
    LAYOUT_OUT="$(PATH="${stub}:${PATH}" SCION_INSTALL_ROOT="$LH_ROOT" SCION_BIN_LINK="$LH_BIN" SCION_INSTALL_OWNER="" \
      SCION_LAYOUT_YAML_TO_JSON="$LAYOUT_YAML_TO_JSON" bash "$LAYOUT_SH" set-maintenance "${LH_DIR}/settings.yaml" default 2>&1)"
    LAYOUT_RC=$?
    assert_true "$([[ "$LAYOUT_RC" -ne 0 ]] && echo true || echo false)" "a settings file owned by ${who} must be refused"
    assert_contains "$LAYOUT_OUT" "is owned by ${who}, not by" "the ${who} refusal must say why"
    assert_true "$(cmp -s "${LH_DIR}/orig.yaml" "${LH_DIR}/settings.yaml" && echo true || echo false)" "${who}: the file must be unchanged"
    rm -rf "$LH_DIR"
  done
}

# =====================================================================
# Fix3 round 1: image build status files
# =====================================================================

# The build status lives in a private dir in the SSH user's home, not under
# fixed /tmp names, and the progress line is echoed with control
# characters (terminal escapes) stripped. Drives the real poll loop
# through the stub: one "running" poll, then exit 0.
test_layout_image_build_status_private_and_log_sanitized() {
  fresh_gcloud_state
  export GCLOUD_STUB_SSH_BUILD_EXIT_SEQ="running 0"
  export GCLOUD_STUB_SSH_BUILD_LOG_LINE='step 2\033[2J\033]0;pwned\007 done'
  export SCION_TEST_BUILD_POLL_SECS=0
  LAYOUT_SENTINEL=images-verified layout_deploy_through_settings "$(layout_config_json "$HUB" notify)"
  unset GCLOUD_STUB_SSH_BUILD_EXIT_SEQ GCLOUD_STUB_SSH_BUILD_LOG_LINE SCION_TEST_BUILD_POLL_SECS
  local log
  log="$(gcloud_log)"
  assert_eq "true" "$LAYOUT_REACHED_SETTINGS" "the deploy must reach the image check after the build (log: ${DEPLOY_LOG: -400})"
  assert_not_contains "$log" "/tmp/scion-image-build" "no fixed /tmp status file may be used"
  # shellcheck disable=SC2016 # literals from the generated commands
  assert_contains "$log" 'B="$HOME/.scion-image-build"' "the build must keep its status in the private home dir"
  # shellcheck disable=SC2016
  assert_contains "$log" 'cat "$HOME/.scion-image-build/exit"' "the poll must read the private exit file"
  assert_contains "$DEPLOY_LOG" "[1] step 2[2J]0;pwned done" "the progress line must be echoed with control characters removed"
  # deploy.sh's own info/err lines are colored on purpose; check the
  # echoed progress line itself.
  local progress
  progress="$(grep -a '\[1\] step 2' <<< "$DEPLOY_LOG")"
  assert_false "$(LC_ALL=C grep -q $'\033\|\a' <<< "$progress" && echo true || echo false)" "no escape or bell byte from the log may reach the terminal"
  assert_contains "$DEPLOY_LOG" "Image build completed successfully" "exit 0 must be taken as success"
}

# Run the generated build-start command for real (sudo is a no-op shim,
# HOME and the source checkout point into a work dir): the status dir is
# created 0700 and gets exit/log/pid; a symlinked or non-directory status
# dir is refused and nothing is written through it.
test_layout_image_build_status_dir_checked() {
  fresh_gcloud_state
  export SCION_TEST_BUILD_POLL_SECS=0
  LAYOUT_SENTINEL=image-build-started layout_deploy_through_settings "$(layout_config_json "$HUB" notify)"
  unset SCION_TEST_BUILD_POLL_SECS
  local cmd kind work out rc waited
  cmd="$(_layout_ssh_cmd_with "$(gcloud_log)" "Image build started in background")"
  assert_contains "$cmd" "cd /opt/scion-source" "sanity: the build-start command must have been captured"
  for kind in fresh symlink file; do
    work="$(mktemp -d)"
    mkdir -p "${work}/home" "${work}/src" "${work}/shim" "${work}/victim"
    # No-op sudo; only `sudo tee` (the marker write) needs its stdin drained.
    # shellcheck disable=SC2016 # script text written to a stub file
    printf '#!/usr/bin/env bash\n[[ "${1:-}" == tee ]] && cat > /dev/null\nexit 0\n' > "${work}/shim/sudo"
    chmod 0755 "${work}/shim/sudo"
    case "$kind" in
      symlink) ln -s "${work}/victim" "${work}/home/.scion-image-build" ;;
      file) echo "not a dir" > "${work}/home/.scion-image-build" ;;
    esac
    out="$(cd "$work" && HOME="${work}/home" PATH="${work}/shim:${PATH}" \
      bash -c "${cmd//cd \/opt\/scion-source/cd \"${work}/src\"}" 2>&1 < /dev/null)"
    rc=$?
    if [[ "$kind" == fresh ]]; then
      assert_eq "0" "$rc" "the build start must succeed with a fresh status dir (output: ${out})"
      assert_eq "700" "$(stat -c %a "${work}/home/.scion-image-build" 2>/dev/null)" "the status dir must be 0700"
      waited=0
      while [[ ! -s "${work}/home/.scion-image-build/exit" && "$waited" -lt 100 ]]; do sleep 0.1; waited=$((waited + 1)); done
      assert_eq "0" "$(cat "${work}/home/.scion-image-build/exit" 2>/dev/null)" "the build's exit status must be written to the private dir"
      assert_true "$([[ -f "${work}/home/.scion-image-build/log" && -s "${work}/home/.scion-image-build/pid" ]] && echo true || echo false)" \
        "log and pid must be written to the private dir"
    else
      assert_true "$([[ "$rc" -ne 0 ]] && echo true || echo false)" "a ${kind} status dir must be refused"
      assert_contains "$out" "is not a directory owned by" "the ${kind} refusal must say why"
      assert_eq "" "$(ls -A "${work}/victim")" "${kind}: nothing may be written through it"
    fi
    rm -rf "$work"
  done
}
