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

# scripts/single-node-vm/scion-layout.sh -- VM-side helper that manages the
# versioned scion install root on a single-node VM hub. deploy.sh copies it
# to the VM over `gcloud compute ssh` and runs it as root; it is never run
# on the operator's machine. It contains no deploy.sh state, so the test
# suite runs it directly against a temporary root.
#
# Staging: a release is copied into a root-owned temp dir next to <root>
# (<root>/../.scion-stage.*), checked, then renamed into releases/. The
# hub's own downloads use releases/<tag>.partial/ instead.
#
# Layout (design D1, ptone/scion#6009):
#   <root>/                     owner 0755
#     releases/<tag>/scion      0755, a verified release binary
#     current  -> releases/<tag>   relative symlink, swapped atomically
#     previous -> releases/<tag>   the release `current` pointed at before
#     snapshots/                DB snapshots (written by the hub)
#   <bin-link> -> <root>/current/scion   root-owned symlink for CLI use
#
# The hub (running as the owner) swaps `current` itself to update, so the
# root, releases and both symlinks are owned by the owner, not root.
#
# Usage:
#   scion-layout.sh install TAG BINARY
#       Install BINARY (already verified by the caller) as releases/TAG and
#       make it current. If <bin-link> is still a plain binary from the old
#       layout, it is first adopted as a release so `previous` points at it.
#   scion-layout.sh adopt
#       Convert an old-layout host: move the binary <bin-link> resolves to
#       into releases/<its version>, without changing the version and
#       without downloading anything. A no-op on a converted host.
#   scion-layout.sh set-maintenance SETTINGS_FILE POLICY
#       Set server.maintenance.install_root to <root> in SETTINGS_FILE, and
#       server.maintenance.update_policy per POLICY: auto|notify|disabled
#       writes that value; "default" rewrites auto to notify (design D7),
#       keeps notify/disabled, and writes notify when the key is absent.
#       Run as root, it re-runs itself as the settings file's owner for the
#       edit, and verifies the result before replacing the file.
#
# Environment (defaults are what deploy.sh uses; tests override them):
#   SCION_INSTALL_ROOT    install root            (default /opt/scion)
#   SCION_BIN_LINK        CLI symlink path        (default /usr/local/bin/scion)
#   SCION_INSTALL_OWNER   user:group owning the root (default scion:scion);
#                         empty skips chown (tests run unprivileged)
#   SCION_LAYOUT_YAML_TO_JSON
#                         shell command that parses YAML on stdin to JSON,
#                         used to verify settings edits (default: python3
#                         with PyYAML, as shipped on the Ubuntu VM image)

set -euo pipefail

INSTALL_ROOT="${SCION_INSTALL_ROOT:-/opt/scion}"
BIN_LINK="${SCION_BIN_LINK:-/usr/local/bin/scion}"
OWNER="${SCION_INSTALL_OWNER-scion:scion}"

log() { echo "scion-layout: $*"; }
die() { echo "scion-layout: ERROR: $*" >&2; exit 1; }

# valid_tag TAG -- a release tag (nightly-20261023, v0.3.0-preview.4) or a
# git build id (git-<sha12>). Same character rule as deploy.sh's VERSION
# check, so a tag can never contain "/" or start with "." or "-".
valid_tag() {
  [[ "$1" =~ ^[0-9A-Za-z][0-9A-Za-z._-]*$ ]] && [[ "$1" != *.partial ]] && [[ "$1" != *.old ]]
}

own() {
  if [[ -n "$OWNER" ]]; then
    chown -h "$OWNER" "$@"
  fi
}

# refuse_symlink PATH -- root never writes, chmods or chowns through a
# symlink the owner could have planted inside its own tree.
refuse_symlink() {
  if [[ -L "$1" ]]; then
    die "$1 is a symlink; refusing to write through it"
  fi
}

# ensure_root -- create <root>, releases/ and snapshots/ with the right
# owner and mode. Safe to repeat.
ensure_root() {
  local d
  for d in "$INSTALL_ROOT" "$INSTALL_ROOT/releases" "$INSTALL_ROOT/snapshots"; do
    refuse_symlink "$d"
    mkdir -p "$d"
    chmod 0755 "$d"
    own "$d"
  done
}

# swap_link NAME TARGET -- point <root>/NAME at TARGET (relative) with an
# atomic rename, so a reader always sees either the old or the new link.
swap_link() {
  local name="$1" target="$2" tmp="$INSTALL_ROOT/$1.new"
  rm -f "$tmp"
  ln -s "$target" "$tmp"
  own "$tmp"
  mv -T "$tmp" "$INSTALL_ROOT/$name"
}

# STAGE_DIRS -- root-owned temp dirs made by new_stage_dir, removed on exit.
STAGE_DIRS=()
cleanup_stage_dirs() {
  if [[ ${#STAGE_DIRS[@]} -gt 0 ]]; then
    rm -rf "${STAGE_DIRS[@]}"
  fi
}
trap cleanup_stage_dirs EXIT

# new_stage_dir -- a fresh root-owned (0700) directory next to <root>, so
# it is on the same filesystem (rename works) but outside the tree the
# owner can modify. Sets STAGE.
new_stage_dir() {
  STAGE="$(mktemp -d "$(dirname "$INSTALL_ROOT")/.scion-stage.XXXXXX")"
  STAGE_DIRS+=("$STAGE")
}

# stage_release TAG BINARY -- install BINARY as releases/TAG/scion. The
# copy is made and checked in a root-owned staging dir, then the finished
# directory is renamed into releases/ in one step, so releases/TAG is
# either absent, the old release, or the complete new one. An identical
# existing release is kept as is; a differing one is replaced.
stage_release() {
  local tag="$1" bin="$2"
  local dest="$INSTALL_ROOT/releases/$tag"
  refuse_symlink "$dest"
  if [[ -f "$dest/scion" ]] && cmp -s "$bin" "$dest/scion"; then
    log "releases/$tag already installed"
    return 0
  fi
  new_stage_dir
  local dir="$STAGE/release"
  mkdir "$dir"
  cp "$bin" "$dir/scion"
  cmp -s "$bin" "$dir/scion" || die "copy of $bin to the staging dir is incomplete (disk full?); nothing was changed"
  chmod 0755 "$dir" "$dir/scion"
  own "$dir" "$dir/scion"
  if [[ -e "$dest" ]]; then
    mv -T "$dest" "$STAGE/old"
  fi
  mv -T "$dir" "$dest"
  log "installed releases/$tag"
}

# activate TAG -- make releases/TAG current; the release current pointed at
# before (if any, and if different) becomes previous.
activate() {
  local tag="$1" target="releases/$1" old=""
  if [[ -L "$INSTALL_ROOT/current" ]]; then
    old="$(readlink "$INSTALL_ROOT/current")"
  elif [[ -e "$INSTALL_ROOT/current" ]]; then
    die "$INSTALL_ROOT/current exists but is not a symlink; refusing to replace it"
  fi
  if [[ "$old" == "$target" ]]; then
    log "current already -> $target"
    return 0
  fi
  if [[ -n "$old" && -d "$INSTALL_ROOT/$old" ]]; then
    swap_link previous "$old"
  fi
  swap_link current "$target"
  log "current -> $target${old:+ (previous -> $old)}"
}

# link_bin -- make <bin-link> a symlink to <root>/current/scion (atomic
# rename over whatever was there). Created by root, so root-owned.
link_bin() {
  local want="$INSTALL_ROOT/current/scion"
  if [[ -L "$BIN_LINK" && "$(readlink "$BIN_LINK")" == "$want" ]]; then
    return 0
  fi
  mkdir -p "$(dirname "$BIN_LINK")"
  rm -f "$BIN_LINK.new"
  ln -s "$want" "$BIN_LINK.new"
  mv -T "$BIN_LINK.new" "$BIN_LINK"
  log "$BIN_LINK -> $want"
}

# is_converted -- <bin-link> already resolves through <root>/current.
is_converted() {
  [[ -L "$BIN_LINK" && "$(readlink "$BIN_LINK")" == "$INSTALL_ROOT/current/scion" && -L "$INSTALL_ROOT/current" ]]
}

# json_string_field KEY JSON -- the first "KEY": "value" string in JSON
# (indented or compact). Enough for `scion version --format json`, whose
# values are plain version/commit strings.
json_string_field() {
  printf '%s\n' "$2" | grep -o "\"$1\":[[:space:]]*\"[^\"]*\"" | head -1 | sed 's/.*:[[:space:]]*"\(.*\)"$/\1/' || true
}

# binary_tag BINARY -- the release tag BINARY reports through
# `scion version --format json`, or git-<sha12> for a build with no
# release version. Runs the binary as the owner when running as root, so it
# never creates state under root's home. Prints nothing if neither is known.
binary_tag() {
  local bin="$1" out user="${OWNER%%:*}" ver commit
  if [[ "$(id -u)" == "0" && -n "$user" ]]; then
    out="$(runuser -u "$user" -- "$bin" version --format json </dev/null 2>/dev/null)" || true
  else
    out="$("$bin" version --format json </dev/null 2>/dev/null)" || true
  fi
  ver="$(json_string_field version "$out")"
  if [[ -n "$ver" && "$ver" != "dev" ]] && valid_tag "$ver"; then
    printf '%s' "$ver"
    return 0
  fi
  commit="$(json_string_field commit "$out")"
  if [[ "$commit" =~ ^[0-9a-f]{12,}$ ]]; then
    printf 'git-%s' "${commit:0:12}"
  fi
}

# adopt_source -- the plain binary <bin-link> resolves to (fails if none).
adopt_source() {
  local src
  src="$(readlink -f "$BIN_LINK")"
  [[ -f "$src" ]] || die "$BIN_LINK does not resolve to a file; nothing to adopt (run a full deploy.sh with --version instead)"
  printf '%s' "$src"
}

# adopt_as SRC TAG -- move SRC into the layout as releases/TAG and make it
# current. Always called as a plain statement (never on the left of || or
# in an if condition), so set -e stays in force for every step: a failed
# copy stops the script before current or <bin-link> change.
adopt_as() {
  local src="$1" tag="$2"
  log "adopting $src as releases/$tag"
  ensure_root
  stage_release "$tag" "$src"
  activate "$tag"
  link_bin
}

cmd_install() {
  [[ $# -eq 2 ]] || die "usage: install TAG BINARY"
  local tag="$1" bin="$2" src old_tag
  valid_tag "$tag" || die "invalid release tag: '$tag'"
  [[ -f "$bin" ]] || die "binary not found: $bin"
  ensure_root
  if ! is_converted && [[ -e "$BIN_LINK" ]] && [[ ! -e "$INSTALL_ROOT/current" ]]; then
    # Old layout: keep the binary being replaced as a release, so previous
    # points at it. Best effort -- an unreadable version only loses that.
    src="$(adopt_source)"
    old_tag="$(binary_tag "$src")"
    if [[ -n "$old_tag" ]]; then
      adopt_as "$src" "$old_tag"
    else
      echo "scion-layout: WARNING: could not read the version of $src; not keeping it as a release" >&2
    fi
  fi
  stage_release "$tag" "$bin"
  activate "$tag"
  link_bin
}

cmd_adopt() {
  [[ $# -eq 0 ]] || die "usage: adopt"
  local src tag
  if is_converted; then
    ensure_root
    log "already converted: $BIN_LINK -> $INSTALL_ROOT/current/scion -> $(readlink "$INSTALL_ROOT/current")"
    return 0
  fi
  [[ -e "$BIN_LINK" ]] || die "$BIN_LINK not found; nothing to adopt (run a full deploy.sh with --version instead)"
  src="$(adopt_source)"
  tag="$(binary_tag "$src")"
  if [[ -z "$tag" ]]; then
    die "could not read a release version or commit from $src (scion version --format json); run a full deploy.sh with --version instead"
  fi
  adopt_as "$src" "$tag"
}

# YAML_TO_JSON_PY -- the default settings parser for verify_settings_edit:
# PyYAML (a cloud-init dependency, so present on the Ubuntu VM image) with
# duplicate mapping keys rejected, as the hub's own YAML parser does.
# shellcheck disable=SC2016 # Python source, not shell
YAML_TO_JSON_PY='
import json, sys, yaml
class Loader(yaml.SafeLoader):
    pass
def mapping(loader, node, deep=False):
    seen = set()
    for key_node, _ in node.value:
        key = loader.construct_object(key_node, deep=True)
        if key in seen:
            raise yaml.constructor.ConstructorError(None, None, "duplicate key %r" % (key,), key_node.start_mark)
        seen.add(key)
    return loader.construct_mapping(node, deep=True)
Loader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, mapping)
json.dump(yaml.load(sys.stdin, Loader=Loader), sys.stdout, default=str)
'

# EXPECT_PY -- checks that NEW (JSON) is exactly ORIG (JSON) with only
# server.maintenance.install_root and update_policy changed as requested.
# shellcheck disable=SC2016 # Python source, not shell
EXPECT_PY='
import json, sys
orig, new, root, policy = json.load(open(sys.argv[1])), json.load(open(sys.argv[2])), sys.argv[3], sys.argv[4]
if not isinstance(orig, dict) or not isinstance(orig.get("server"), dict):
    sys.exit("settings has no server mapping")
server = dict(orig["server"])
maint = server.get("maintenance") or {}
if not isinstance(maint, dict):
    sys.exit("server.maintenance is not a mapping")
maint = dict(maint)
maint["install_root"] = root
current = maint.get("update_policy")
if policy == "default":
    maint["update_policy"] = "notify" if current in (None, "auto") else current
else:
    maint["update_policy"] = policy
server["maintenance"] = maint
expected = dict(orig)
expected["server"] = server
if new != expected:
    sys.exit("edited settings differ from the intended change")
'

# settings_to_json FILE OUT -- parse a settings YAML file to JSON in OUT.
# SCION_LAYOUT_YAML_TO_JSON (a shell command reading YAML on stdin) replaces
# the PyYAML default; the tests point it at the hub's own koanf parser.
settings_to_json() {
  if [[ -n "${SCION_LAYOUT_YAML_TO_JSON:-}" ]]; then
    bash -c "$SCION_LAYOUT_YAML_TO_JSON" < "$1" > "$2"
  else
    python3 -I -c "$YAML_TO_JSON_PY" < "$1" > "$2"
  fi
}

# verify_settings_edit ORIG NEW POLICY -- fails unless NEW parses (no
# duplicate keys) and equals ORIG plus exactly the intended change.
verify_settings_edit() {
  local orig="$1" new="$2" policy="$3" oj nj rc=0
  oj="$(mktemp)"
  nj="$(mktemp)"
  settings_to_json "$orig" "$oj" 2>/dev/null || rc=1
  if [[ "$rc" -eq 0 ]]; then
    settings_to_json "$new" "$nj" 2>/dev/null || rc=2
  fi
  if [[ "$rc" -eq 0 ]]; then
    python3 -I -c "$EXPECT_PY" "$oj" "$nj" "$INSTALL_ROOT" "$policy" || rc=3
  fi
  rm -f "$oj" "$nj"
  case "$rc" in
    0) return 0 ;;
    1) echo "scion-layout: could not parse $orig as YAML, or no YAML parser is available (python3 with PyYAML). Install it with 'sudo apt-get install python3-yaml', or edit the file by hand and re-run." >&2 ;;
    2) echo "scion-layout: the edited settings would not parse as YAML" >&2 ;;
    3) echo "scion-layout: the edited settings would change more than server.maintenance.install_root/update_policy" >&2 ;;
  esac
  return 1
}

# cmd_set_maintenance FILE POLICY -- line-based edit of the server.maintenance
# block, so comments and every other key stay byte-for-byte as they were,
# then a structural check of the result (verify_settings_edit) before the
# file is replaced. Follows the file's own indentation (deploy.sh writes 2
# spaces, Go's YAML encoder 4). Creates the maintenance block under
# server: when it is missing. Refuses (file unchanged) anything it cannot
# edit line by line: a flow-style server: or maintenance:, a scalar
# maintenance value, or a duplicate maintenance key.
cmd_set_maintenance() {
  [[ $# -eq 2 ]] || die "usage: set-maintenance SETTINGS_FILE POLICY"
  local file="$1" policy="$2"
  case "$policy" in
    auto|notify|disabled|default) ;;
    *) die "invalid update policy: '$policy' (expected auto, notify, disabled or default)" ;;
  esac
  refuse_symlink "$file"
  [[ -f "$file" ]] || die "settings file not found: $file"
  # Edit as the file's owner, not as root: the file (and its directory)
  # belong to the hub user, so root gains nothing by editing it and must
  # not write through anything that user could plant next to it. The
  # script re-runs itself from stdin (its temp file is not readable by the
  # owner) with the same settings.
  local file_owner
  file_owner="$(stat -c %U "$file")"
  if [[ "$(id -u)" == "0" && "$file_owner" != "root" ]]; then
    exec runuser -u "$file_owner" -- env \
      SCION_INSTALL_ROOT="$INSTALL_ROOT" \
      SCION_LAYOUT_YAML_TO_JSON="${SCION_LAYOUT_YAML_TO_JSON:-}" \
      bash -s -- set-maintenance "$file" "$policy" < "${BASH_SOURCE[0]}"
  fi
  local manual="set server.maintenance.install_root: \"$INSTALL_ROOT\" (and update_policy) in $file by hand, then re-run"
  local tmp rc=0
  # A fresh, randomly named file (O_EXCL), never a fixed name that could
  # already exist as a symlink.
  tmp="$(mktemp "$file.XXXXXX")"
  awk -v root="$INSTALL_ROOT" -v policy="$policy" '
    function indent_of(s) { match(s, /^ */); return RLENGTH }
    function pad(n) { return sprintf("%" n "s", "") }
    function step() { return sci ? sci : 2 }
    # value_of(LINE, KEY) -- the scalar after "KEY:", without a trailing
    # comment or surrounding quotes. A line whose value already matches is
    # kept byte-for-byte, so a correct hand edit counts as up to date.
    # comment_of(LINE) -- a trailing " # ..." comment, kept when the line
    # is rewritten.
    function comment_of(l) { return match(l, /[ \t]+#.*$/) ? substr(l, RSTART) : "" }
    function value_of(l, key,   v) {
      v = l; sub("^ *" key ":[ \t]*", "", v); sub(/[ \t]+#.*$/, "", v); sub(/[ \t]+$/, "", v); gsub(/"|\047/, "", v)
      return v
    }
    function emit_missing(ind) {
      handled = 1
      if (!seen_root) print pad(ind) "install_root: \"" root "\""
      if (!seen_policy) print pad(ind) "update_policy: \"" (policy == "default" ? "notify" : policy) "\""
    }
    function insert_block() {
      print pad(step()) "maintenance:"
      emit_missing(2 * step())
      done_maint = 1
    }
    bad { next }
    {
      line = $0
      blank = (line ~ /^[ \t]*(#.*)?$/)
      ind = indent_of(line)
      if (in_maint && !blank && ind <= maint_ind) {
        emit_missing(child_ind ? child_ind : maint_ind + step()); in_maint = 0; done_maint = 1
      }
      if (in_server && !blank && ind == 0) {
        if (!done_maint) insert_block()
        in_server = 0
      }
      if (line ~ /^server:[ \t]*(#.*)?$/) {
        if (seen_server) { bad = 1; next }
        in_server = 1; seen_server = 1; print; next
      }
      if (in_server && !blank) {
        if (!sci) sci = ind
        if (ind == sci && line ~ /^ *maintenance:/) {
          rest = line; sub(/^ *maintenance:[ \t]*/, "", rest); sub(/^#.*$/, "", rest)
          if (rest != "" || done_maint) { bad = 1; next }
          in_maint = 1; maint_ind = ind; child_ind = 0; print; next
        }
      }
      if (in_maint && !blank) {
        if (!child_ind) child_ind = ind
        if (ind == child_ind && line ~ /^ *install_root:/) {
          seen_root = 1
          if (value_of(line, "install_root") == root) { print; next }
          print pad(ind) "install_root: \"" root "\"" comment_of(line); next
        }
        if (ind == child_ind && line ~ /^ *update_policy:/) {
          seen_policy = 1
          v = value_of(line, "update_policy")
          if (policy == "default") {
            if (v == "auto") { print pad(ind) "update_policy: \"notify\"" comment_of(line); next }
            print; next
          }
          if (v == policy) { print; next }
          print pad(ind) "update_policy: \"" policy "\"" comment_of(line); next
        }
      }
      print
    }
    END {
      if (bad) exit 4
      if (in_maint) emit_missing(child_ind ? child_ind : maint_ind + step())
      else if (in_server && !done_maint) insert_block()
      if (!handled) exit 3
    }
  ' "$file" > "$tmp" || rc=$?
  if [[ "$rc" -ne 0 ]]; then
    rm -f "$tmp"
    die "cannot edit $file line by line (no block-style server: section, or a flow-style, scalar or duplicate server/maintenance key); nothing changed. Please $manual"
  fi
  if cmp -s "$tmp" "$file"; then
    rm -f "$tmp"
    log "$file already up to date"
    return 0
  fi
  if ! verify_settings_edit "$file" "$tmp" "$policy"; then
    rm -f "$tmp"
    die "refusing to write $file; nothing changed. Please $manual"
  fi
  # Same owner already (the edit runs as the file's owner); keep the mode.
  chmod --reference="$file" "$tmp"
  mv -f "$tmp" "$file"
  log "updated server.maintenance in $file"
}

main() {
  local cmd="${1:-}"
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    install) cmd_install "$@" ;;
    adopt) cmd_adopt "$@" ;;
    set-maintenance) cmd_set_maintenance "$@" ;;
    *) die "usage: scion-layout.sh {install TAG BINARY | adopt | set-maintenance SETTINGS_FILE POLICY}" ;;
  esac
}

main "$@"
