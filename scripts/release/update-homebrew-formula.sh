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

# Generate the Homebrew formula (Formula/scion.rb in the homebrew-scion tap)
# for a published release. The four per-OS/arch sha256 values come from the
# release's SHA256SUMS asset, so no tarball is downloaded or rebuilt.
#
# Usage:
#   update-homebrew-formula.sh [--sums-file FILE] [--allow-downgrade] VERSION_TAG OUT_FILE
#
#   VERSION_TAG        Release tag, e.g. v0.3.0-preview.5 (vX.Y.Z, -preview.N or -rc.N).
#   OUT_FILE           Formula path to write, e.g. tap/Formula/scion.rb.
#   --sums-file FILE   Read SHA256SUMS from FILE instead of downloading it.
#   --allow-downgrade  Write even if OUT_FILE already holds a newer version.
#
# The output depends only on the tag and the SHA256SUMS content. OUT_FILE is
# replaced atomically and left untouched on any error. An existing OUT_FILE
# with a newer version is refused, so rerunning an older release cannot move
# the tap backwards by accident.
#
# Tests: scripts/release/test-update-homebrew-formula.sh

set -euo pipefail

REPO="GoogleCloudPlatform/scion"
RELEASE_BASE="https://github.com/${REPO}/releases/download"

usage() {
  sed -n 's/^# \{0,1\}//; 20,26p' < "$0" >&2
  exit 2
}

die() {
  echo "update-homebrew-formula: $*" >&2
  exit 1
}

sums_file=""
allow_downgrade="false"
args=()
while [ $# -gt 0 ]; do
  case "$1" in
    --sums-file)
      [ $# -ge 2 ] || usage
      sums_file="$2"
      shift 2
      ;;
    --allow-downgrade)
      allow_downgrade="true"
      shift
      ;;
    -h | --help) usage ;;
    --) shift; args+=("$@"); break ;;
    -*) echo "update-homebrew-formula: unknown option: $1" >&2; usage ;;
    *) args+=("$1"); shift ;;
  esac
done
[ "${#args[@]}" -eq 2 ] || usage
tag="${args[0]}"
out_file="${args[1]}"

# Only release tags that the formula may follow. The pattern also keeps the
# tag safe to embed in a Ruby string and a URL.
tag_re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(preview|rc)\.(0|[1-9][0-9]*))?$'
[[ "$tag" =~ $tag_re ]] || die "not a release tag (want vX.Y.Z, vX.Y.Z-preview.N or vX.Y.Z-rc.N): '$tag'"
version="${tag#v}"

# staged is the temporary formula next to OUT_FILE; the trap removes it if a
# failure stops the script before the final rename.
staged=""
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"; if [ -n "$staged" ]; then rm -f -- "$staged"; fi' EXIT

if [ -z "$sums_file" ]; then
  sums_file="$tmp/SHA256SUMS"
  curl -fsSL --proto '=https' --proto-redir '=https' --max-time 60 --retry 3 \
    -o "$sums_file" "${RELEASE_BASE}/${tag}/SHA256SUMS" ||
    die "could not download SHA256SUMS for ${tag}"
fi
[ -f "$sums_file" ] || die "no such SHA256SUMS file: $sums_file"

# sha_for ASSET — print the hash on the single SHA256SUMS line for ASSET.
# Lines are coreutils format: 64 lowercase hex, two separators ("  " or
# " *" in binary mode), the file name. File paths reach awk and sed on
# stdin, never as arguments: awk reads an argument like "a=b" as a variable
# assignment, and both read a leading "-" as an option.
sha_for() {
  local asset="$1" matches count
  matches=$(awk -v name="$asset" '
    ($2 == name || $2 == "*" name) && NF == 2 { print $1 }
  ' < "$sums_file")
  count=$(printf '%s' "$matches" | grep -c . || true)
  [ "$count" -eq 1 ] || die "want exactly one SHA256SUMS line for ${asset}, found ${count}"
  [[ "$matches" =~ ^[0-9a-f]{64}$ ]] || die "malformed sha256 for ${asset}: '${matches}'"
  printf '%s' "$matches"
}

darwin_arm64=$(sha_for scion-darwin-arm64.tar.gz)
darwin_amd64=$(sha_for scion-darwin-amd64.tar.gz)
linux_arm64=$(sha_for scion-linux-arm64.tar.gz)
linux_amd64=$(sha_for scion-linux-amd64.tar.gz)

# version_key VERSION — print a key that sorts release versions correctly
# with sort -V: X.Y.Z, then 0 for a pre-release or 1 for a final release,
# then the pre-release label (preview < rc) and number.
version_key() {
  local v="$1" core pre label num
  core="${v%%-*}"
  if [ "$core" = "$v" ]; then
    printf '%s.1.0.0\n' "$core"
    return
  fi
  pre="${v#*-}"
  label="${pre%%.*}"
  num="${pre#*.}"
  case "$label" in
    preview) label=1 ;;
    rc) label=2 ;;
    *) return 1 ;;
  esac
  printf '%s.0.%s.%s\n' "$core" "$label" "$num"
}

if [ -f "$out_file" ] && [ "$allow_downgrade" != "true" ]; then
  current=$(sed -n 's/^  version "\(.*\)"$/\1/p' < "$out_file" | head -1)
  if [ -n "$current" ] && [ "$current" != "$version" ]; then
    current_key=$(version_key "$current") ||
      die "cannot compare with the version in ${out_file} ('${current}'); pass --allow-downgrade to overwrite"
    new_key=$(version_key "$version")
    highest=$(printf '%s\n%s\n' "$current_key" "$new_key" | sort -V | tail -1)
    [ "$highest" = "$new_key" ] ||
      die "${out_file} has newer version ${current}; refusing to write ${version} (pass --allow-downgrade to force)"
  fi
fi

url="${RELEASE_BASE}/${tag}"
cat > "$tmp/scion.rb" <<EOF
# Generated from the SHA256SUMS of release ${tag} by
# scripts/release/update-homebrew-formula.sh in https://github.com/${REPO}.
# Do not edit by hand: the next release overwrites this file.
class Scion < Formula
  desc "Orchestrate concurrent LLM code agents in containers"
  homepage "https://github.com/${REPO}"
  version "${version}"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "${url}/scion-darwin-arm64.tar.gz"
      sha256 "${darwin_arm64}"
    end
    on_intel do
      url "${url}/scion-darwin-amd64.tar.gz"
      sha256 "${darwin_amd64}"
    end
  end

  on_linux do
    on_arm do
      url "${url}/scion-linux-arm64.tar.gz"
      sha256 "${linux_arm64}"
    end
    on_intel do
      url "${url}/scion-linux-amd64.tar.gz"
      sha256 "${linux_amd64}"
    end
  end

  def install
    bin.install "scion"
  end

  def caveats
    text = <<~EOS
      To set up Scion, start the workstation server:
        scion server start
      On first start it opens the onboarding wizard, which checks your
      container runtime and sets the image registry.

      If a server was already running, restart it to use the new binary:
        scion server restart

      Container images for this release are published with the release tag,
      for example:
        ghcr.io/homebrew-scion/scion-claude:${tag}
      This version of Scion pulls the images tagged with its own version.
    EOS
    if OS.linux?
      text += <<~EOS

        On Linux, Homebrew installs its own gcc, once for all formulae, when
        the system glibc is older than 2.39 or libstdc++ is older than
        6.0.33, and its own glibc as well when glibc is older than 2.39.
        Scion is a static binary and does not use them.
        On such hosts you can install the release tarball instead:
          https://github.com/${REPO}/releases/tag/${tag}
      EOS
    end
    text
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/scion version")
  end
end
EOF

out_dir=$(dirname -- "$out_file")
[ -d "$out_dir" ] || die "output directory does not exist: $out_dir"
# A relative directory gets a ./ prefix so mktemp never reads a leading "-"
# as an option (portable; not every mktemp accepts --).
case "$out_dir" in
  /*) ;;
  *) out_dir="./${out_dir}" ;;
esac
staged=$(mktemp "${out_dir}/.scion.rb.XXXXXX")
cp -- "$tmp/scion.rb" "$staged"
chmod 0644 "$staged"
mv -f -- "$staged" "$out_file"
staged=""
echo "Wrote ${out_file} for ${tag}"
