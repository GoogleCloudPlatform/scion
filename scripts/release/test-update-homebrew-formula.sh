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

# Tests for scripts/release/update-homebrew-formula.sh. Needs only bash and
# coreutils; no network (the download path runs against a stub curl).
#
# testdata/homebrew/SHA256SUMS is the published SHA256SUMS of
# v0.3.0-preview.5, and scion-v0.3.0-preview.5.rb is the formula expected
# from it. After an intended change to the formula text, regenerate it:
#   scripts/release/update-homebrew-formula.sh \
#     --sums-file scripts/release/testdata/homebrew/SHA256SUMS \
#     v0.3.0-preview.5 scripts/release/testdata/homebrew/scion-v0.3.0-preview.5.rb

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$here/update-homebrew-formula.sh"
DATA="$here/testdata/homebrew"
SUMS="$DATA/SHA256SUMS"
GOLDEN="$DATA/scion-v0.3.0-preview.5.rb"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fails=0
fail() {
  echo "FAIL: $name: $*" >&2
  fails=$((fails + 1))
}

# run ARGS... — run the script; sets $status and leaves stderr in $tmp/stderr.
# stdin is /dev/null, so a regression that reads stdin fails instead of hanging.
run() {
  status=0
  "$SCRIPT" "$@" </dev/null >"$tmp/stdout" 2>"$tmp/stderr" || status=$?
}

want_status() {
  [ "$status" -eq "$1" ] || fail "exit status: got $status, want $1 (stderr: $(cat "$tmp/stderr"))"
}

want_stderr() {
  grep -qF -- "$1" "$tmp/stderr" || fail "stderr lacks '$1': $(cat "$tmp/stderr")"
}

# sums_without ASSET — fixture SHA256SUMS minus the line for ASSET.
sums_without() {
  grep -v "  $1\$" "$SUMS" > "$tmp/sums"
}

name="golden: preview.5 formula matches the fixture"
out="$tmp/golden/scion.rb"
mkdir -p "$tmp/golden"
run --sums-file "$SUMS" v0.3.0-preview.5 "$out"
want_status 0
diff -u "$GOLDEN" "$out" >&2 || fail "output differs from $GOLDEN"

name="deterministic: a second run writes identical bytes"
cp "$out" "$tmp/first.rb"
run --sums-file "$SUMS" v0.3.0-preview.5 "$out"
want_status 0
cmp -s "$tmp/first.rb" "$out" || fail "second run changed the output"

name="content: four platform blocks, bin only, no post_install or plugins"
[ "$(grep -c '^      sha256 "[0-9a-f]\{64\}"$' "$out")" -eq 4 ] || fail "want 4 sha256 lines"
[ "$(grep -c '^      url "https://github.com/GoogleCloudPlatform/scion/releases/download/v0.3.0-preview.5/scion-' "$out")" -eq 4 ] ||
  fail "want 4 upstream urls"
grep -qF 'bin.install "scion"' "$out" || fail "missing bin.install"
grep -qF 'version "0.3.0-preview.5"' "$out" || fail "missing version"
grep -qF 'license "Apache-2.0"' "$out" || fail "missing license"
grep -qF 'shell_output("#{bin}/scion version")' "$out" || fail "missing test block"
grep -qF 'scion server start' "$out" || fail "caveats lack the onboarding command"
grep -qF 'ghcr.io/homebrew-scion is an old v0.2 image' "$out" || fail "caveats lack the stale :latest warning"
for bad in post_install pre_install 'scion init' telegram plugin libexec; do
  if grep -qF -- "$bad" "$out"; then fail "unexpected '$bad'"; fi
done

name="content: each platform takes its own hash"
for pair in darwin-arm64:24296f8e darwin-amd64:9ee065b5 linux-arm64:dd6ae0a2 linux-amd64:d319a3c0; do
  plat="${pair%%:*}" prefix="${pair#*:}"
  grep -A1 "scion-${plat}.tar.gz\"" "$out" | grep -q "sha256 \"${prefix}" ||
    fail "${plat} not followed by its sha256 ${prefix}..."
done

name="rc tag: accepted"
run --sums-file "$SUMS" v0.3.0-rc.1 "$tmp/rc.rb"
want_status 0
grep -qF 'version "0.3.0-rc.1"' "$tmp/rc.rb" || fail "rc version not written"

name="binary-mode sums lines (' *name') are accepted"
sed 's/  / */' "$SUMS" > "$tmp/sums"
run --sums-file "$tmp/sums" v0.3.0-preview.5 "$tmp/bin.rb"
want_status 0
cmp -s "$GOLDEN" "$tmp/bin.rb" || fail "binary-mode output differs from golden"

for tag in nightly-20261105 0.3.0-preview.5 v0.3.0 v0.3.0-beta.1 v0.3 v01.3.0 \
  'v0.3.0-preview.5"; system("id")' 'v0.3.0-preview.5 ' ''; do
  name="bad tag '$tag': rejected"
  # v0.3.0 is a valid stable tag; it must succeed, not fail.
  if [ "$tag" = v0.3.0 ]; then
    run --sums-file "$SUMS" "$tag" "$tmp/stable.rb"
    want_status 0
    grep -qF 'version "0.3.0"' "$tmp/stable.rb" || fail "stable version not written"
    continue
  fi
  run --sums-file "$SUMS" "$tag" "$tmp/bad.rb"
  want_status 1
  want_stderr "not a release tag"
  [ ! -e "$tmp/bad.rb" ] || fail "output written for a bad tag"
done

name="missing asset line: fails and leaves the existing file alone"
sums_without scion-linux-arm64.tar.gz
printf 'existing\n' > "$tmp/keep.rb"
run --sums-file "$tmp/sums" v0.3.0-preview.5 "$tmp/keep.rb"
want_status 1
want_stderr "found 0"
[ "$(cat "$tmp/keep.rb")" = existing ] || fail "existing file was modified"

name="duplicate asset line: fails"
{ cat "$SUMS"; grep '  scion-darwin-arm64.tar.gz$' "$SUMS"; } > "$tmp/sums"
run --sums-file "$tmp/sums" v0.3.0-preview.5 "$tmp/dup.rb"
want_status 1
want_stderr "found 2"
[ ! -e "$tmp/dup.rb" ] || fail "output written for duplicate lines"

name="malformed hash: fails"
sed 's/^24296f8e/24296F8E/' "$SUMS" > "$tmp/sums"
run --sums-file "$tmp/sums" v0.3.0-preview.5 "$tmp/hash.rb"
want_status 1
want_stderr "malformed sha256 for scion-darwin-arm64.tar.gz"

name="CRLF sums file: fails (no line matches the exact file name)"
sed 's/$/\r/' "$SUMS" > "$tmp/sums"
run --sums-file "$tmp/sums" v0.3.0-preview.5 "$tmp/crlf.rb"
want_status 1
want_stderr "found 0"

name="near-miss names are not matched"
sums_without scion-darwin-arm64.tar.gz
printf '%064d  scion-darwin-arm64.tar.gz.sig\n%064d  xscion-darwin-arm64.tar.gz\n' 0 0 >> "$tmp/sums"
run --sums-file "$tmp/sums" v0.3.0-preview.5 "$tmp/near.rb"
want_status 1
want_stderr "found 0"

name="missing sums file: fails"
run --sums-file "$tmp/nope" v0.3.0-preview.5 "$tmp/nope.rb"
want_status 1
want_stderr "no such SHA256SUMS file"

name="missing output directory: fails"
run --sums-file "$SUMS" v0.3.0-preview.5 "$tmp/no/such/dir/scion.rb"
want_status 1
want_stderr "output directory does not exist"

name="downgrade: refused against a newer existing formula"
mkdir -p "$tmp/tap"
run --sums-file "$SUMS" v0.3.0-preview.10 "$tmp/tap/scion.rb"
want_status 0
cp "$tmp/tap/scion.rb" "$tmp/p10.rb"
run --sums-file "$SUMS" v0.3.0-preview.9 "$tmp/tap/scion.rb"
want_status 1
want_stderr "has newer version 0.3.0-preview.10"
cmp -s "$tmp/p10.rb" "$tmp/tap/scion.rb" || fail "refused downgrade still changed the file"

name="downgrade: allowed with --allow-downgrade"
run --sums-file "$SUMS" --allow-downgrade v0.3.0-preview.9 "$tmp/tap/scion.rb"
want_status 0
grep -qF 'version "0.3.0-preview.9"' "$tmp/tap/scion.rb" || fail "forced downgrade not written"

name="ordering: preview < rc < stable < next patch preview"
run --sums-file "$SUMS" v0.3.0-rc.1 "$tmp/tap/scion.rb"
want_status 0
run --sums-file "$SUMS" v0.3.0-preview.11 "$tmp/tap/scion.rb"
want_status 1
run --sums-file "$SUMS" v0.3.0 "$tmp/tap/scion.rb"
want_status 0
run --sums-file "$SUMS" v0.3.0-rc.2 "$tmp/tap/scion.rb"
want_status 1
run --sums-file "$SUMS" v0.3.1-preview.1 "$tmp/tap/scion.rb"
want_status 0
run --sums-file "$SUMS" v0.10.0-preview.1 "$tmp/tap/scion.rb"
want_status 0
run --sums-file "$SUMS" v0.9.0 "$tmp/tap/scion.rb"
want_status 1

name="upgrade from the current tap formula (0.2.21)"
printf 'class Scion < Formula\n  version "0.2.21"\nend\n' > "$tmp/tap/scion.rb"
run --sums-file "$SUMS" v0.3.0-preview.5 "$tmp/tap/scion.rb"
want_status 0
cmp -s "$GOLDEN" "$tmp/tap/scion.rb" || fail "upgrade from 0.2.21 differs from golden"

name="same version: rewritten (idempotent rerun)"
run --sums-file "$SUMS" v0.3.0-preview.5 "$tmp/tap/scion.rb"
want_status 0

# run_in DIR ARGS... — like run, but from DIR, so relative paths reach the
# script exactly as given.
run_in() {
  local dir="$1"
  shift
  status=0
  (cd "$dir" && "$SCRIPT" "$@") </dev/null >"$tmp/stdout" 2>"$tmp/stderr" || status=$?
}

mkdir -p "$tmp/odd"

name="odd paths: a sums file named like an awk assignment (x=y) is read as a file"
cp "$SUMS" "$tmp/odd/sums=SHA256SUMS"
run_in "$tmp/odd" --sums-file sums=SHA256SUMS v0.3.0-preview.5 eq.rb
want_status 0
cmp -s "$GOLDEN" "$tmp/odd/eq.rb" || fail "output differs from golden"

name="odd paths: a sums file whose name starts with '-' is read as a file"
cp "$SUMS" "$tmp/odd/-SHA256SUMS"
run_in "$tmp/odd" --sums-file -SHA256SUMS v0.3.0-preview.5 dash-sums.rb
want_status 0
cmp -s "$GOLDEN" "$tmp/odd/dash-sums.rb" || fail "output differs from golden"

name="odd paths: an output file whose name starts with '-' still gets the downgrade check"
run_in "$tmp/odd" --sums-file "$SUMS" -- v0.3.0-preview.10 -scion.rb
want_status 0
run_in "$tmp/odd" --sums-file "$SUMS" -- v0.3.0-preview.9 -scion.rb
want_status 1
want_stderr "has newer version 0.3.0-preview.10"
grep -qF 'version "0.3.0-preview.10"' "$tmp/odd/-scion.rb" || fail "refused downgrade still changed the file"

name="odd paths: an output file named like an awk assignment (x=y) is written"
run_in "$tmp/odd" --sums-file "$SUMS" v0.3.0-preview.5 out=scion.rb
want_status 0
cmp -s "$GOLDEN" "$tmp/odd/out=scion.rb" || fail "output differs from golden"

name="odd paths: an output directory whose name starts with '-' is written"
mkdir -p "$tmp/odd/-dir"
run_in "$tmp/odd" --sums-file "$SUMS" -- v0.3.0-preview.5 -dir/scion.rb
want_status 0
cmp -s "$GOLDEN" "$tmp/odd/-dir/scion.rb" || fail "output differs from golden"
leftover=$(find "$tmp/odd/-dir" -name '.scion.rb.*' | head -1)
[ -z "$leftover" ] || fail "temp file left behind: $leftover"

name="interrupted staging: a failed copy leaves no temp file and the old formula"
mkdir -p "$tmp/stage" "$tmp/failbin"
printf 'existing\n' > "$tmp/stage/scion.rb"
cat > "$tmp/failbin/cp" <<'EOF'
#!/usr/bin/env bash
# Stub cp that fails, as an interrupted or full-disk copy would.
exit 1
EOF
chmod +x "$tmp/failbin/cp"
PATH="$tmp/failbin:$PATH" run --sums-file "$SUMS" v0.3.0-preview.5 "$tmp/stage/scion.rb"
[ "$status" -ne 0 ] || fail "exit status: got 0 with a failing cp"
leftover=$(find "$tmp/stage" -name '.scion.rb.*' | head -1)
[ -z "$leftover" ] || fail "temp file left behind: $leftover"
[ "$(cat "$tmp/stage/scion.rb")" = existing ] || fail "existing file was modified"

name="usage: wrong argument count exits 2"
run v0.3.0-preview.5
want_status 2
run --bogus v0.3.0-preview.5 "$tmp/x.rb"
want_status 2

name="download: fetches SHA256SUMS for the tag from the upstream release"
mkdir -p "$tmp/bin"
cat > "$tmp/bin/curl" <<'EOF'
#!/usr/bin/env bash
# Stub curl: records its arguments and writes the fixture to the -o target.
printf '%s\n' "$@" > "$CURL_ARGS"
out=""
while [ $# -gt 0 ]; do
  if [ "$1" = -o ]; then out="$2"; shift 2; else shift; fi
done
cp "$CURL_FIXTURE" "$out"
EOF
chmod +x "$tmp/bin/curl"
export CURL_ARGS="$tmp/curl-args" CURL_FIXTURE="$SUMS"
PATH="$tmp/bin:$PATH" run v0.3.0-preview.5 "$tmp/dl.rb"
want_status 0
grep -qx 'https://github.com/GoogleCloudPlatform/scion/releases/download/v0.3.0-preview.5/SHA256SUMS' "$CURL_ARGS" ||
  fail "unexpected curl url: $(cat "$CURL_ARGS")"
grep -qx -- '-fsSL' "$CURL_ARGS" || fail "curl not run with -fsSL"
for flag in --proto --proto-redir; do
  grep -A1 -x -- "$flag" "$CURL_ARGS" | grep -qx '=https' || fail "curl not run with $flag =https"
done
grep -qx -- --max-time "$CURL_ARGS" || fail "curl not run with --max-time"
cmp -s "$GOLDEN" "$tmp/dl.rb" || fail "downloaded-path output differs from golden"

name="download failure: fails without writing"
cat > "$tmp/bin/curl" <<'EOF'
#!/usr/bin/env bash
exit 22
EOF
PATH="$tmp/bin:$PATH" run v0.3.0-preview.5 "$tmp/dlfail.rb"
want_status 1
want_stderr "could not download SHA256SUMS for v0.3.0-preview.5"
[ ! -e "$tmp/dlfail.rb" ] || fail "output written after a failed download"

if [ "$fails" -gt 0 ]; then
  echo "update-homebrew-formula tests: $fails failure(s)" >&2
  exit 1
fi
echo "update-homebrew-formula tests: all passed"
