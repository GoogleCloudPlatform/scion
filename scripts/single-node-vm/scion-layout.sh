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
#
# Environment (defaults are what deploy.sh uses; tests override them):
#   SCION_INSTALL_ROOT    install root            (default /opt/scion)
#   SCION_BIN_LINK        CLI symlink path        (default /usr/local/bin/scion)
#   SCION_INSTALL_OWNER   user:group owning the root (default scion:scion);
#                         empty skips chown (tests run unprivileged)

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

# ensure_root -- create <root>, releases/ and snapshots/ with the right
# owner and mode. Safe to repeat.
ensure_root() {
  local d
  for d in "$INSTALL_ROOT" "$INSTALL_ROOT/releases" "$INSTALL_ROOT/snapshots"; do
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

# stage_release TAG BINARY -- copy BINARY to releases/TAG/scion through
# releases/TAG.partial/, then rename into place. An existing identical
# release is kept as is; a differing one is replaced.
stage_release() {
  local tag="$1" bin="$2"
  local dest="$INSTALL_ROOT/releases/$tag" partial="$INSTALL_ROOT/releases/$tag.partial"
  if [[ -f "$dest/scion" ]] && cmp -s "$bin" "$dest/scion"; then
    log "releases/$tag already installed"
    return 0
  fi
  rm -rf "$partial"
  mkdir -p "$partial"
  cp "$bin" "$partial/scion"
  chmod 0755 "$partial" "$partial/scion"
  own "$partial" "$partial/scion"
  if [[ -e "$dest" ]]; then
    local old="$INSTALL_ROOT/releases/$tag.old"
    rm -rf "$old"
    mv -T "$dest" "$old"
    mv -T "$partial" "$dest"
    rm -rf "$old"
  else
    mv -T "$partial" "$dest"
  fi
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

# adopt_existing -- move the plain binary <bin-link> resolves to into the
# layout. Returns 1 (after a message) when its version cannot be read.
adopt_existing() {
  local src tag
  src="$(readlink -f "$BIN_LINK")"
  [[ -f "$src" ]] || die "$BIN_LINK does not resolve to a file; nothing to adopt (run a full deploy.sh with --version instead)"
  tag="$(binary_tag "$src")"
  if [[ -z "$tag" ]]; then
    echo "scion-layout: could not read a release version or commit from $src (scion version --format json)" >&2
    return 1
  fi
  log "adopting $src as releases/$tag"
  ensure_root
  stage_release "$tag" "$src"
  activate "$tag"
  link_bin
}

cmd_install() {
  [[ $# -eq 2 ]] || die "usage: install TAG BINARY"
  local tag="$1" bin="$2"
  valid_tag "$tag" || die "invalid release tag: '$tag'"
  [[ -f "$bin" ]] || die "binary not found: $bin"
  ensure_root
  if ! is_converted && [[ -e "$BIN_LINK" ]] && [[ ! -e "$INSTALL_ROOT/current" ]]; then
    # Old layout: keep the binary being replaced as a release, so previous
    # points at it. Best effort -- an unreadable version only loses that.
    adopt_existing || echo "scion-layout: WARNING: not keeping the old $BIN_LINK as a release" >&2
  fi
  stage_release "$tag" "$bin"
  activate "$tag"
  link_bin
}

cmd_adopt() {
  [[ $# -eq 0 ]] || die "usage: adopt"
  if is_converted; then
    ensure_root
    log "already converted: $BIN_LINK -> $INSTALL_ROOT/current/scion -> $(readlink "$INSTALL_ROOT/current")"
    return 0
  fi
  [[ -e "$BIN_LINK" ]] || die "$BIN_LINK not found; nothing to adopt (run a full deploy.sh with --version instead)"
  adopt_existing || die "cannot determine the installed version; run a full deploy.sh with --version instead"
}

# cmd_set_maintenance FILE POLICY -- line-based edit of the server.maintenance
# block, so comments and every other key stay byte-for-byte as they were.
# Works for deploy.sh's own rendering (2-space indent) and the admin
# server-config page's rewrite (any consistent indent). Creates the
# maintenance block under server: when it is missing.
cmd_set_maintenance() {
  [[ $# -eq 2 ]] || die "usage: set-maintenance SETTINGS_FILE POLICY"
  local file="$1" policy="$2"
  case "$policy" in
    auto|notify|disabled|default) ;;
    *) die "invalid update policy: '$policy' (expected auto, notify, disabled or default)" ;;
  esac
  [[ -f "$file" ]] || die "settings file not found: $file"
  grep -q '^server:' "$file" || die "$file has no top-level server: block; set server.maintenance.install_root by hand"
  local tmp="$file.scion-layout.tmp"
  awk -v root="$INSTALL_ROOT" -v policy="$policy" '
    function indent_of(s) { match(s, /^ */); return RLENGTH }
    function emit_missing(ind,   pad) {
      pad = sprintf("%" ind "s", "")
      handled = 1
      if (!seen_root) print pad "install_root: \"" root "\""
      if (!seen_policy) print pad "update_policy: \"" (policy == "default" ? "notify" : policy) "\""
    }
    {
      line = $0
      blank = (line ~ /^[ \t]*(#.*)?$/)
      ind = indent_of(line)
      if (in_maint && !blank && ind <= maint_ind) {
        emit_missing(child_ind ? child_ind : maint_ind + 2); in_maint = 0; done_maint = 1
      }
      if (in_server && !blank && ind == 0 && line !~ /^server:/) {
        if (!done_maint) { print "  maintenance:"; emit_missing(4); done_maint = 1 }
        in_server = 0
      }
      if (line ~ /^server:[ \t]*$/) { in_server = 1; print; next }
      if (in_server && !in_maint && !done_maint && line ~ /^ +maintenance:[ \t]*$/) {
        in_maint = 1; handled = 1; maint_ind = ind; child_ind = 0; print; next
      }
      if (in_maint && !blank) {
        if (!child_ind) child_ind = ind
        if (ind == child_ind && line ~ /^ *install_root:/) {
          print sprintf("%" ind "s", "") "install_root: \"" root "\""; seen_root = 1; next
        }
        if (ind == child_ind && line ~ /^ *update_policy:/) {
          seen_policy = 1
          if (policy == "default") {
            v = line; sub(/^ *update_policy:[ \t]*/, "", v); sub(/[ \t]*#.*$/, "", v); gsub(/"|\047/, "", v)
            if (v == "auto") { print sprintf("%" ind "s", "") "update_policy: \"notify\""; next }
            print; next
          }
          print sprintf("%" ind "s", "") "update_policy: \"" policy "\""; next
        }
      }
      print
    }
    END {
      if (in_maint) emit_missing(child_ind ? child_ind : maint_ind + 2)
      else if (in_server && !done_maint) { print "  maintenance:"; emit_missing(4) }
      if (!handled) exit 3
    }
  ' "$file" > "$tmp" || { rm -f "$tmp"; die "could not find a block-style server: section in $file; set server.maintenance.install_root by hand"; }
  if cmp -s "$tmp" "$file"; then
    rm -f "$tmp"
    log "$file already up to date"
    return 0
  fi
  chown --reference="$file" "$tmp" 2>/dev/null || true
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
