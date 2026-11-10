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

# scripts/single-node-vm/release-order.sh -- release tag ordering for
# deploy.sh's downgrade guard. Sourced by deploy.sh (and by the test
# suite); defines functions only.
#
# This runs on the operator's machine, which may be macOS: pure bash 3.2,
# no external commands. In particular no `sort -V` (BSD sort has no -V).
# version_cmp reimplements GNU sort -V's comparison (gnulib filevercmp) so
# the results match what deploy.sh computed with `sort -V` before.
#
# This file has no shebang -- it is always `source`d, never executed.
# shellcheck shell=bash

# _vo_order CHAR -- filevercmp's per-character weight, in _VO_ORD: digits
# 0 (never compared here), letters their code, '~' -1, anything else its
# code + 256 (so it sorts after every letter). An empty CHAR (end of
# string) is 0.
_vo_order() {
  local c="$1"
  if [[ -z "$c" ]]; then
    _VO_ORD=0
  elif [[ "$c" == [0-9] ]]; then
    _VO_ORD=0
  elif [[ "$c" == [A-Za-z] ]]; then
    _VO_ORD="$(printf '%d' "'$c")"
  elif [[ "$c" == "~" ]]; then
    _VO_ORD=-1
  else
    _VO_ORD=$(( $(printf '%d' "'$c") + 256 ))
  fi
}

# _vo_verrevcmp A B -- gnulib's verrevcmp: alternate non-digit runs
# (compared character by character with _vo_order) and digit runs
# (compared numerically, leading zeros ignored). Sets _VO_CMP to a
# negative, zero or positive number.
_vo_verrevcmp() {
  local a="$1" b="$2" i=0 j=0 la=${#1} lb=${#2} ca cb oa ob first_diff
  while (( i < la || j < lb )); do
    first_diff=0
    while :; do
      ca="${a:i:1}"
      cb="${b:j:1}"
      if ! { (( i < la )) && [[ "$ca" != [0-9] ]]; } && ! { (( j < lb )) && [[ "$cb" != [0-9] ]]; }; then
        break
      fi
      if (( i < la )); then _vo_order "$ca"; oa=$_VO_ORD; else oa=0; fi
      if (( j < lb )); then _vo_order "$cb"; ob=$_VO_ORD; else ob=0; fi
      if (( oa != ob )); then
        _VO_CMP=$(( oa - ob ))
        return 0
      fi
      i=$(( i + 1 ))
      j=$(( j + 1 ))
    done
    while (( i < la )) && [[ "${a:i:1}" == 0 ]]; do i=$(( i + 1 )); done
    while (( j < lb )) && [[ "${b:j:1}" == 0 ]]; do j=$(( j + 1 )); done
    while (( i < la && j < lb )) && [[ "${a:i:1}" == [0-9] && "${b:j:1}" == [0-9] ]]; do
      if (( first_diff == 0 )); then
        first_diff=$(( $(printf '%d' "'${a:i:1}") - $(printf '%d' "'${b:j:1}") ))
      fi
      i=$(( i + 1 ))
      j=$(( j + 1 ))
    done
    if (( i < la )) && [[ "${a:i:1}" == [0-9] ]]; then _VO_CMP=1; return 0; fi
    if (( j < lb )) && [[ "${b:j:1}" == [0-9] ]]; then _VO_CMP=-1; return 0; fi
    if (( first_diff != 0 )); then _VO_CMP=$first_diff; return 0; fi
  done
  _VO_CMP=0
}

# _vo_prefixlen S -- length of S without a trailing "file suffix", gnulib's
# (\.[A-Za-z~][A-Za-z0-9~]*)*$, never counting the first character as
# part of it. Sets _VO_PLEN.
_vo_prefixlen() {
  local s="$1" n=${#1} i=0 prefixlen=0
  while :; do
    if (( i >= n )); then
      _VO_PLEN=$prefixlen
      return 0
    fi
    i=$(( i + 1 ))
    prefixlen=$i
    while (( i + 1 < n )) && [[ "${s:i:1}" == "." && "${s:i+1:1}" == [A-Za-z~] ]]; do
      i=$(( i + 2 ))
      while (( i < n )) && [[ "${s:i:1}" == [A-Za-z0-9~] ]]; do i=$(( i + 1 )); done
    done
  done
}

# version_cmp A B -- GNU sort -V's order for A and B (gnulib filevercmp,
# plus sort's last-resort byte comparison when filevercmp ties). Sets
# VERSION_CMP to -1, 0 or 1. Neither argument may start with "." (the
# special cases for hidden file names are not implemented; release tags
# never start with one).
version_cmp() {
  local LC_ALL=C a="$1" b="$2" pa pb
  if [[ "$a" == "$b" ]]; then VERSION_CMP=0; return 0; fi
  if [[ -z "$a" ]]; then VERSION_CMP=-1; return 0; fi
  if [[ -z "$b" ]]; then VERSION_CMP=1; return 0; fi
  _vo_prefixlen "$a"; pa=$_VO_PLEN
  _vo_prefixlen "$b"; pb=$_VO_PLEN
  _vo_verrevcmp "${a:0:pa}" "${b:0:pb}"
  if (( _VO_CMP == 0 )) && ! (( pa == ${#a} && pb == ${#b} )); then
    _vo_verrevcmp "$a" "$b"
  fi
  if (( _VO_CMP == 0 )); then
    # Equal as versions (e.g. "01" and "1"): sort's last resort is a
    # byte comparison.
    if [[ "$a" < "$b" ]]; then _VO_CMP=-1; else _VO_CMP=1; fi
  fi
  if (( _VO_CMP < 0 )); then VERSION_CMP=-1; else VERSION_CMP=1; fi
}

# release_order A B -- prints older, newer or same for release tag A
# relative to B, or unknown when they cannot be ordered (different
# channels, or a git build). Nightlies order by date; semver tags order by
# version, with a pre-release (v1.0.0-preview.2) older than its release.
release_order() {
  local a="$1" b="$2" semver_re='^v?([0-9]+\.[0-9]+\.[0-9]+)(-([0-9A-Za-z.-]+))?$'
  local ka kb ap bp
  if [[ "$a" == "$b" ]]; then
    echo same
    return 0
  fi
  if [[ "$a" == nightly-* && "$b" == nightly-* ]]; then
    ka="${a#nightly-}"
    kb="${b#nightly-}"
  elif [[ "$a" =~ $semver_re ]]; then
    ka="${BASH_REMATCH[1]}"
    ap="${BASH_REMATCH[3]}"
    if [[ ! "$b" =~ $semver_re ]]; then
      echo unknown
      return 0
    fi
    kb="${BASH_REMATCH[1]}"
    bp="${BASH_REMATCH[3]}"
    if [[ "$ka" == "$kb" ]]; then
      # Same core version: the release outranks any pre-release of it.
      if [[ -z "$ap" ]]; then echo newer; return 0; fi
      if [[ -z "$bp" ]]; then echo older; return 0; fi
      ka="$ap"
      kb="$bp"
    fi
  else
    echo unknown
    return 0
  fi
  if [[ "$ka" == "$kb" ]]; then
    echo same
    return 0
  fi
  version_cmp "$ka" "$kb"
  if (( VERSION_CMP < 0 )); then
    echo older
  else
    echo newer
  fi
}
