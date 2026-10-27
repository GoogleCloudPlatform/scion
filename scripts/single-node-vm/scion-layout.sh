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
# Privileges: run as root, it changes nothing inside <root> itself once the
# owner holds it; staging a release and swapping current/previous run as
# the owner (see "Privilege split" below). Defense in depth only: design
# D1 already treats the owner as root-equivalent.
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

# canonical_abs PATH -- an absolute path in plain form: starts with /, no
# empty, "." or ".." components, no trailing /. Everything below walks
# paths with dirname and compares them as strings, which only terminates
# and means what it says for such paths.
canonical_abs() {
  local p="$1"
  [[ "$p" == /?* && "$p" != */ && "$p" != *//* ]] || return 1
  [[ "/$p/" != */./* && "/$p/" != */../* ]]
}

log() { echo "scion-layout: $*"; }
die() { echo "scion-layout: ERROR: $*" >&2; exit 1; }

canonical_abs "$INSTALL_ROOT" || die "SCION_INSTALL_ROOT must be an absolute path without '.', '..', '//' or a trailing '/' (got '$INSTALL_ROOT')"
canonical_abs "$BIN_LINK" || die "SCION_BIN_LINK must be an absolute path without '.', '..', '//' or a trailing '/' (got '$BIN_LINK')"

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

# refuse_symlink PATH -- refuse a PATH that is a symlink. A check, not a
# guarantee (it can race); paths in the owner's tree are therefore only
# changed by the owner (see "Privilege split" below).
refuse_symlink() {
  if [[ -L "$1" ]]; then
    die "$1 is a symlink; refusing to use it"
  fi
}

# owner_user -- the user that must own <root>: the user of
# SCION_INSTALL_OWNER, or (tests, empty owner) the current user.
owner_user() {
  if [[ -n "$OWNER" ]]; then
    printf '%s' "${OWNER%%:*}"
  else
    id -un
  fi
}

# Privilege split. <root> and everything under it belong to the owner, who
# can rename, replace or symlink any entry there at any time. So, as
# defense in depth (design D1 already treats the owner as root-equivalent
# through the docker group; this is not a security boundary), the root
# side of this script never writes, chmods, chowns, renames or removes
# anything inside <root> once the owner holds it:
#   - root builds a fresh tree while <root> is still root-owned, then hands
#     it over (ensure_root);
#   - on an existing tree root only inspects, without following links
#     (verify_owned_dir), and refuses on any mismatch instead of fixing it;
#   - all changes inside <root> (staging a release, current/previous,
#     missing subdirs) run as the owner (run_as_owner -> __as-owner);
#   - root itself only writes outside the tree: <bin-link>, and readable
#     copies of input binaries it may read in a root-owned temp dir (stage_inputs;
#     a binary reached through a symlink is read by the owner instead),
#     so the owner step can always read its input.

# verify_owned_dir PATH -- PATH must be a real directory (not a symlink),
# owned by the owner, mode 0755. Inspected with stat, which does not follow
# a final symlink. Refuses rather than repairs.
verify_owned_dir() {
  local info want
  want="directory|$(owner_user)|755"
  info="$(stat -c '%F|%U|%a' -- "$1")" || die "cannot inspect $1"
  if [[ "$info" != "$want" ]]; then
    die "$1 is not a directory owned by $(owner_user) with mode 0755 (found: ${info//|/, }); refusing to use it. Fix it by hand (it must not be a symlink) and re-run."
  fi
}

# ensure_root -- make sure <root>, releases/ and snapshots/ exist, owned by
# the owner, mode 0755.
#   - Fresh host: <root> is created root-owned (its parent is root's), the
#     subdirs are made and set up inside it while only root can write
#     there, and ownership is handed over last, <root> itself at the end.
#   - Existing tree: verify only (no chmod/chown through anything the
#     owner could have swapped). A missing subdir is created later, as the
#     owner, by __as-owner.
ensure_root() {
  local d parent
  parent="$(dirname "$INSTALL_ROOT")"
  if [[ ! -e "$INSTALL_ROOT" && ! -L "$INSTALL_ROOT" ]]; then
    # Check before creating anything: the nearest existing ancestor and
    # everything above it. Missing parents are then made 0755, and the
    # full chain is checked again.
    dir_chain_is_safe "$(nearest_existing "$parent")" || unsafe_parent_die
    make_dirs_down_to "$parent"
    dir_chain_is_safe "$parent" || unsafe_parent_die
    mkdir -m 0755 "$INSTALL_ROOT"
    build_and_hand_over "created"
    return 0
  fi
  # Resume a handover that failed partway (e.g. the owner user did not
  # exist yet): <root> is still a directory owned by root. Root finishes
  # the job only if nobody else can have changed <root>'s entries: its
  # parent chain is safe (dir_chain_is_safe) AND <root> itself is strictly
  # safe -- a real directory (not a symlink), owned by root, with no group
  # or other write (dir_is_safe). Otherwise it refuses rather than chmod or
  # chown anything inside.
  if [[ "$(owner_user)" != "root" \
        && "$(stat -c '%F|%U' -- "$INSTALL_ROOT")" == "directory|root" ]]; then
    if [[ "$(stat -c '%U' -- "$parent")" != "root" ]] || ! dir_chain_is_safe "$parent"; then
      [[ "$(stat -c '%U' -- "$parent")" == "root" ]] || UNSAFE_REASON="$parent is owned by $(stat -c '%U' -- "$parent"), not root"
      unsafe_parent_die
    fi
    dir_is_safe "$INSTALL_ROOT" || die "$UNSAFE_REASON; refusing to resume the handover of $INSTALL_ROOT. Fix it by hand (a root-owned directory with mode 0755) and re-run."
    build_and_hand_over "resumed the handover of"
    return 0
  fi
  verify_owned_dir "$INSTALL_ROOT"
  for d in releases snapshots; do
    if [[ -e "$INSTALL_ROOT/$d" || -L "$INSTALL_ROOT/$d" ]]; then
      verify_owned_dir "$INSTALL_ROOT/$d"
    fi
  done
}

# dir_is_safe DIR [sticky-ok] -- DIR is a real directory (not a symlink)
# owned by root (or by the user running this, for unprivileged tests), and
# not writable by group or others. With sticky-ok, a sticky group/other-
# writable DIR (like /tmp) is accepted too: there, others cannot rename or
# remove an entry they do not own -- but they can create, rename and
# remove their own entries, so sticky-ok is only for ancestors ABOVE a
# strictly safe directory (see dir_chain_is_safe). Inspected with stat,
# which does not follow a final symlink. On failure sets UNSAFE_REASON.
dir_is_safe() {
  local info type owner mode sticky_ok="${2:-}"
  info="$(stat -c '%F|%U|%a' -- "$1" 2>/dev/null)" || { UNSAFE_REASON="$1 cannot be inspected"; return 1; }
  IFS='|' read -r type owner mode <<< "$info"
  if [[ "$type" == "symbolic link" ]]; then
    UNSAFE_REASON="$1 is a symlink"
    return 1
  fi
  if [[ "$type" != "directory" ]]; then
    UNSAFE_REASON="$1 is not a directory ($type)"
    return 1
  fi
  if [[ "$owner" != "root" && "$owner" != "$(id -un)" ]]; then
    UNSAFE_REASON="$1 is owned by $owner, not root"
    return 1
  fi
  # Group (020) or other (002) write: refused, except sticky with sticky-ok.
  if (( (8#$mode & 8#022) != 0 )); then
    if [[ -z "$sticky_ok" ]] || (( (8#$mode & 8#1000) == 0 )); then
      UNSAFE_REASON="$1 is writable by group or others (mode $mode)"
      return 1
    fi
  fi
}

# dir_chain_is_safe DIR -- DIR must be strictly safe (no group/other write,
# sticky or not), and every ancestor up to / safe, where a sticky ancestor
# is accepted: the entry below it is owned by root (or the running user),
# so nobody else can rename or replace it. Any other owner-writable
# component would let the owner swap the directories below it after the
# check. DIR must be canonical (canonical_abs), so the walk ends at /.
dir_chain_is_safe() {
  local d="$1"
  dir_is_safe "$d" || return 1
  while [[ "$d" != "/" ]]; do
    d="$(dirname "$d")"
    dir_is_safe "$d" sticky-ok || return 1
  done
}

# nearest_existing DIR -- DIR, or its closest ancestor that exists.
nearest_existing() {
  local d="$1"
  while [[ ! -e "$d" && ! -L "$d" ]]; do
    d="$(dirname "$d")"
  done
  printf '%s' "$d"
}

# make_dirs_down_to DIR -- create DIR's missing components one at a time,
# top down, never with mkdir -p (which would follow a symlink someone
# planted between components). Each component is created with a plain
# mkdir (which fails if anything, including a symlink, already has that
# name) inside a parent just verified as strictly safe, and is then itself
# verified as a strictly safe real directory.
make_dirs_down_to() {
  local target="$1" base rest comp
  base="$(nearest_existing "$target")"
  [[ "$base" == "$target" ]] && return 0
  rest="${target#"$base"}"
  rest="${rest#/}"
  while [[ -n "$rest" ]]; do
    comp="${rest%%/*}"
    if [[ "$rest" == */* ]]; then rest="${rest#*/}"; else rest=""; fi
    dir_is_safe "$base" || unsafe_parent_die
    base="${base%/}/$comp"
    mkdir -m 0755 -- "$base" || die "could not create $base"
    dir_is_safe "$base" || unsafe_parent_die
  done
}

# unsafe_parent_die -- refuse with the reason the parent chain is unsafe.
unsafe_parent_die() {
  die "$UNSAFE_REASON; the directories above $INSTALL_ROOT must be real directories owned by root and not writable by group or others. Refusing to set up $INSTALL_ROOT there."
}

# build_and_hand_over VERB -- with <root> a real directory only root can
# write to: set it to 0755, create or fix releases/ and snapshots/ (real
# directories only), hand them to the owner, and hand over <root> last.
build_and_hand_over() {
  local d
  chmod 0755 "$INSTALL_ROOT"
  for d in releases snapshots; do
    if [[ ! -e "$INSTALL_ROOT/$d" && ! -L "$INSTALL_ROOT/$d" ]]; then
      mkdir -m 0755 "$INSTALL_ROOT/$d"
    fi
    [[ "$(stat -c '%F' -- "$INSTALL_ROOT/$d")" == "directory" ]] || die "$INSTALL_ROOT/$d is not a directory; refusing to use it"
    chmod 0755 "$INSTALL_ROOT/$d"
    own "$INSTALL_ROOT/$d"
  done
  own "$INSTALL_ROOT"
  log "$1 $INSTALL_ROOT"
}

# INPUT_DIRS -- root-owned temp dirs holding readable copies of the input
# binaries (stage_inputs), removed on exit.
INPUT_DIRS=()
cleanup_input_dirs() {
  if [[ ${#INPUT_DIRS[@]} -gt 0 ]]; then
    rm -rf "${INPUT_DIRS[@]}"
  fi
}
trap cleanup_input_dirs EXIT

# stage_inputs TAG SRC WHO [TAG SRC WHO...] -- build STAGED_PAIRS, the
# TAG/BINARY pairs handed to the owner step, in order. WHO says who reads
# SRC:
#   root  -- root copies SRC (which root can read, but the owner may not: a
#            0750 file under umask 027, a relative path, a 0700 hand-installed
#            binary) to a 0755 file in a fresh root-owned 0711 directory
#            outside <root>, and the owner step gets that absolute path;
#   owner -- SRC is passed through unchanged and only the owner step reads
#            it, with the owner's own permissions. Used for any source root
#            must not read through (see adopt_reader).
# Root writes nothing inside <root> here.
stage_inputs() {
  local dir=""
  STAGED_PAIRS=()
  while [[ $# -gt 0 ]]; do
    if [[ "$3" == "owner" ]]; then
      STAGED_PAIRS+=("$1" "$2")
    else
      if [[ -z "$dir" ]]; then
        dir="$(mktemp -d "${TMPDIR:-/tmp}/scion-layout-input.XXXXXX")"
        INPUT_DIRS+=("$dir")
        chmod 0711 "$dir"
      fi
      install -m 0755 -- "$2" "$dir/$1"
      cmp -s "$2" "$dir/$1" || die "copy of $2 to $dir is incomplete (disk full?); nothing was changed"
      STAGED_PAIRS+=("$1" "$dir/$1")
    fi
    shift 3
  done
}

# adopt_reader -- who may read the binary being adopted: "root" only when
# <bin-link> is a plain file owned by root, in a directory chain only root
# can change (dir_chain_is_safe; /usr/local/bin on the VM), so nobody else
# can swap it between this check and root's copy. If <bin-link> is a symlink it can lead anywhere, including
# through <root> (e.g. <root>/current/scion with current a directory),
# where the owner can swap the file after its version was read; that copy
# is then made by the owner step ("owner"), so root never reads through an
# owner-controlled path.
adopt_reader() {
  local file_owner
  file_owner="$(stat -c '%U' -- "$BIN_LINK" 2>/dev/null || true)"
  if [[ ! -L "$BIN_LINK" ]] && [[ "$file_owner" == "root" || "$file_owner" == "$(id -un)" ]] \
     && dir_chain_is_safe "$(dirname "$BIN_LINK")"; then
    echo root
  else
    echo owner
  fi
}

# run_as_owner ARGS... -- run this script's internal __as-owner step with
# ARGS as the owner. As root (with an owner configured) it re-runs the
# script from stdin through runuser (the script's own temp file is not
# readable by the owner); otherwise (tests) it runs in-process.
run_as_owner() {
  if [[ "$(id -u)" == "0" && -n "$OWNER" ]]; then
    runuser -u "${OWNER%%:*}" -- env SCION_INSTALL_ROOT="$INSTALL_ROOT" SCION_INSTALL_OWNER="" \
      bash -s -- __as-owner "$@" < "${BASH_SOURCE[0]}"
  else
    (cmd_as_owner "$@")
  fi
}

# ---------------------------------------------------------------------------
# Owner side (__as-owner): everything that changes <root>'s contents.
# ---------------------------------------------------------------------------

# swap_link NAME TARGET -- point <root>/NAME at TARGET (relative) with an
# atomic rename, so a reader always sees either the old or the new link.
swap_link() {
  local name="$1" target="$2" tmp="$INSTALL_ROOT/$1.new"
  rm -f "$tmp"
  ln -s "$target" "$tmp"
  mv -T "$tmp" "$INSTALL_ROOT/$name"
}

# stage_release TAG BINARY -- install BINARY as releases/TAG/scion: copy
# into releases/TAG.partial/, check the copy byte for byte, then rename
# into place, so releases/TAG is absent, the old release, or the complete
# new one. An identical existing release is kept; a differing one is
# replaced.
stage_release() {
  local tag="$1" bin="$2"
  local dest="$INSTALL_ROOT/releases/$tag" partial="$INSTALL_ROOT/releases/$tag.partial"
  if [[ -f "$dest/scion" && ! -L "$dest" ]] && cmp -s "$bin" "$dest/scion"; then
    log "releases/$tag already installed"
    return 0
  fi
  rm -rf "$partial"
  mkdir -m 0755 "$partial"
  if ! cp "$bin" "$partial/scion"; then
    rm -rf "$partial"
    die "could not copy $bin into releases/ (input unreadable by $(id -un), or disk full); nothing was changed"
  fi
  if ! cmp -s "$bin" "$partial/scion"; then
    rm -rf "$partial"
    die "copy of $bin into releases/ is incomplete (disk full?); nothing was changed"
  fi
  chmod 0755 "$partial" "$partial/scion"
  if [[ -e "$dest" || -L "$dest" ]]; then
    # The aside name ends in .partial, so the hub's release listing
    # ignores it if this step dies between the two renames.
    rm -rf "$INSTALL_ROOT/releases/$tag.old.partial"
    mv -T "$dest" "$INSTALL_ROOT/releases/$tag.old.partial"
    mv -T "$partial" "$dest"
    rm -rf "$INSTALL_ROOT/releases/$tag.old.partial"
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

# cmd_as_owner TAG BINARY [TAG BINARY...] -- runs as the owner: create any
# missing subdir, then stage and activate each TAG/BINARY pair in order
# (an adopted old binary first, then the new release).
cmd_as_owner() {
  [[ "$(id -u)" != "0" || -z "$OWNER" ]] || die "__as-owner is an internal step and must not run as root with an owner configured; use install or adopt"
  [[ $# -ge 2 && $(( $# % 2 )) -eq 0 ]] || die "usage: __as-owner TAG BINARY [TAG BINARY...]"
  local d
  for d in releases snapshots; do
    if [[ ! -e "$INSTALL_ROOT/$d" && ! -L "$INSTALL_ROOT/$d" ]]; then
      mkdir -m 0755 "$INSTALL_ROOT/$d"
      chmod 0755 "$INSTALL_ROOT/$d"
    fi
  done
  while [[ $# -gt 0 ]]; do
    valid_tag "$1" || die "invalid release tag: '$1'"
    stage_release "$1" "$2"
    activate "$1"
    shift 2
  done
}

# ---------------------------------------------------------------------------
# Root side.
# ---------------------------------------------------------------------------

# link_bin -- make <bin-link> a symlink to <root>/current/scion (atomic
# rename over whatever was there). <bin-link>'s directory is root's, and
# the link is root-owned. Besides the input copies in $TMPDIR
# (stage_inputs), this is root's only write after the handover.
link_bin() {
  local want="$INSTALL_ROOT/current/scion"
  if [[ -L "$BIN_LINK" && "$(readlink "$BIN_LINK")" == "$want" ]]; then
    return 0
  fi
  # Root writes here, so the directory chain must be root's (and exist).
  dir_chain_is_safe "$(dirname "$BIN_LINK")" || die "$UNSAFE_REASON; refusing to write $BIN_LINK"
  # -T/-n: never treat an existing $BIN_LINK.new (even a symlink to a
  # directory) as a directory to create the link in.
  ln -sfnT "$want" "$BIN_LINK.new"
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

# check_bin_dir -- root writes <bin-link>, so its directory chain must be
# root's (dir_chain_is_safe). Checked before anything changes, so an unsafe
# bin dir is refused up front rather than after the tree was converted.
check_bin_dir() {
  dir_chain_is_safe "$(dirname "$BIN_LINK")" || die "$UNSAFE_REASON; the directories above $BIN_LINK must be real directories owned by root and not writable by group or others. Refusing to write $BIN_LINK there."
}

# Every step below is a plain statement (never on the left of || or in an
# if condition), so set -e stays in force: a failed step stops the script
# before <bin-link> changes.
cmd_install() {
  [[ $# -eq 2 ]] || die "usage: install TAG BINARY"
  local tag="$1" bin="$2" src old_tag
  local pairs=()
  valid_tag "$tag" || die "invalid release tag: '$tag'"
  [[ -f "$bin" ]] || die "binary not found: $bin"
  check_bin_dir
  ensure_root
  if ! is_converted && [[ -e "$BIN_LINK" ]] && [[ ! -e "$INSTALL_ROOT/current" && ! -L "$INSTALL_ROOT/current" ]]; then
    # Old layout: keep the binary being replaced as a release, so previous
    # points at it. Best effort -- an unreadable version only loses that.
    src="$(adopt_source)"
    old_tag="$(binary_tag "$src")"
    if [[ -n "$old_tag" ]]; then
      log "adopting $src as releases/$old_tag"
      pairs+=("$old_tag" "$src" "$(adopt_reader)")
    else
      echo "scion-layout: WARNING: could not read the version of $src; not keeping it as a release" >&2
    fi
  fi
  pairs+=("$tag" "$bin" root)
  stage_inputs "${pairs[@]}"
  run_as_owner "${STAGED_PAIRS[@]}"
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
  check_bin_dir
  src="$(adopt_source)"
  tag="$(binary_tag "$src")"
  if [[ -z "$tag" ]]; then
    die "could not read a release version or commit from $src (scion version --format json); run a full deploy.sh with --version instead"
  fi
  log "adopting $src as releases/$tag"
  ensure_root
  stage_inputs "$tag" "$src" "$(adopt_reader)"
  run_as_owner "${STAGED_PAIRS[@]}"
  link_bin
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
  # The settings file must belong to the install owner (the hub user):
  # deploy.sh only ever creates it as that user. Anything else -- a
  # root-owned file, or one owned by another user -- is refused, so the
  # edit below never runs as root in a directory someone else controls.
  # As root, the edit runs as that owner: the script re-runs itself from
  # stdin (its temp file is not readable by the owner) with the same
  # settings.
  local file_owner
  file_owner="$(stat -c %U -- "$file")"
  if [[ "$file_owner" != "$(owner_user)" ]]; then
    die "$file is owned by $file_owner, not by $(owner_user) (the install owner); refusing to edit it. Make it owned by $(owner_user), or set the keys by hand."
  fi
  if [[ "$(id -u)" == "0" && "$file_owner" != "root" ]]; then
    exec runuser -u "$file_owner" -- env \
      SCION_INSTALL_ROOT="$INSTALL_ROOT" \
      SCION_INSTALL_OWNER="$OWNER" \
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
    __as-owner) cmd_as_owner "$@" ;;
    adopt) cmd_adopt "$@" ;;
    set-maintenance) cmd_set_maintenance "$@" ;;
    *) die "usage: scion-layout.sh {install TAG BINARY | adopt | set-maintenance SETTINGS_FILE POLICY}" ;;
  esac
}

main "$@"
