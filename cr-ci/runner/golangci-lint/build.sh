#!/usr/bin/env bash
# Build golangci-lint from source with honnef.co/go/tools (staticcheck,
# unused) patched for dominikh/go-tools#1839. See README.md for why, and for
# when to delete this.
#
# Usage: build.sh <output-binary-path>
#
# The single copy of this build: .github/workflows/ci.yml (the GitHub-hosted
# lint job), the Makefile's golangci-lint target and cr-ci/runner/Dockerfile
# all run this script.
#
# Inputs, all in this directory:
#   VERSIONS              pinned versions, Go module checksums, patch sha256
#   go-tools-1839.patch   the patch, applied to the go-tools module source
#
# What it checks before building:
#   - the patch file's sha256 is PATCH_SHA256;
#   - the golangci-lint and go-tools module zips that go downloads (through
#     GOPROXY, verified against go.sum / sum.golang.org as usual) have exactly
#     the pinned h1: checksums, so a GOSUMDB=off or GONOSUMCHECK environment
#     cannot slip in other source;
#   - golangci-lint's go.mod requires exactly GO_TOOLS_VERSION, so the
#     replace swaps in the patched copy of the same version it was released
#     with, and nothing else changes.
# Every other dependency comes from golangci-lint's own go.sum.
#
# Writes <output>.stamp next to the binary: a sha256 over this script,
# VERSIONS, the patch, `go version` and GOOS/GOARCH. If the binary and a
# matching stamp already exist, the build is skipped, so callers can restore
# the binary from a cache and run this script unconditionally.
#
# Needs: go (the version in the repository's go.mod; GOTOOLCHAIN=local, no
# toolchain download), git (to apply the patch), sha256sum, and network access
# to the Go module proxy unless the module cache already holds the modules.
# The caller's GOCACHE and GOMODCACHE are used; point them elsewhere to keep
# this build out of another cache.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
  echo "usage: $0 <output-binary-path>" >&2
  exit 2
fi
out=$1
case "$out" in
  /*) ;;
  *) out="$PWD/$out" ;;
esac

# shellcheck source=VERSIONS
. "$here/VERSIONS"
patch="$here/go-tools-1839.patch"

# A clean, predictable Go environment: no workspace, no inherited -mod or
# -gcflags, and no silent toolchain switch.
export GOWORK=off GOFLAGS='' GOTOOLCHAIN=local

die() { echo "build.sh: $*" >&2; exit 1; }

got=$(sha256sum "$patch" | cut -d' ' -f1)
[ "$got" = "$PATCH_SHA256" ] || die "patch sha256 mismatch: got $got, want $PATCH_SHA256 ($patch)"

stamp_key=$(
  {
    sha256sum "$here/build.sh" "$here/VERSIONS" "$patch" | cut -d' ' -f1
    go version
    go env GOOS GOARCH
  } | sha256sum | cut -d' ' -f1
)
if [ -x "$out" ] && [ -f "$out.stamp" ] && [ "$(cat "$out.stamp")" = "$stamp_key" ]; then
  echo "build.sh: $out is up to date (stamp $stamp_key)"
  exit 0
fi

work=$(mktemp -d)
cleanup() { chmod -R u+w "$work" 2>/dev/null || true; rm -rf "$work"; }
trap cleanup EXIT

# download <module> <version> <want-sum>: prints the module's directory in the
# module cache after checking its checksum against the pin.
download() {
  local json dir sum
  json=$(cd "$work" && go mod download -json "$1@$2") || die "go mod download $1@$2 failed: $json"
  dir=$(sed -n 's/^[[:space:]]*"Dir": "\(.*\)",\{0,1\}$/\1/p' <<<"$json")
  sum=$(sed -n 's/^[[:space:]]*"Sum": "\(.*\)",\{0,1\}$/\1/p' <<<"$json")
  { [ -n "$dir" ] && [ -d "$dir" ]; } || die "no module directory for $1@$2: $json"
  [ "$sum" = "$3" ] || die "checksum mismatch for $1@$2: got '$sum', want '$3'"
  echo "$dir"
}

gl_dir=$(download github.com/golangci/golangci-lint/v2 "$GOLANGCI_LINT_VERSION" "$GOLANGCI_LINT_SUM")
gt_dir=$(download honnef.co/go/tools "$GO_TOOLS_VERSION" "$GO_TOOLS_SUM")

# Module cache files are read-only; build from writable copies.
cp -R "$gl_dir" "$work/golangci-lint"
cp -R "$gt_dir" "$work/go-tools"
chmod -R u+w "$work/golangci-lint" "$work/go-tools"

cd "$work/golangci-lint"
modfile=$(go mod edit -print)
required=$(awk '
  $1 == "require" && $2 == "honnef.co/go/tools" { print $3 }
  $1 == "honnef.co/go/tools" { print $2 }' <<<"$modfile")
[ "$required" = "$GO_TOOLS_VERSION" ] ||
  die "golangci-lint $GOLANGCI_LINT_VERSION requires honnef.co/go/tools '$required', VERSIONS pins $GO_TOOLS_VERSION"
if grep -q '^replace.*honnef\.co/go/tools\|^[[:space:]]*honnef\.co/go/tools .*=>' <<<"$modfile"; then
  die "golangci-lint's go.mod already replaces honnef.co/go/tools"
fi

# git apply inside its own throwaway repository, so that it applies relative
# to the go-tools copy even if $TMPDIR lies inside another checkout.
git -C "$work/go-tools" init -q
git -C "$work/go-tools" apply --verbose "$patch"
grep -q 'g\.pathEncoder\.For(obj)' "$work/go-tools/unused/unused.go" || die "patch did not apply as expected"

go mod edit -replace "honnef.co/go/tools=$work/go-tools"

# main.date must be non-empty: otherwise golangci-lint ignores main.version
# and reports the build info's "(devel)" version instead.
version="${GOLANGCI_LINT_VERSION#v}-scion-gotools1839"
mkdir -p "$(dirname "$out")"
tmp_out="$out.tmp.$$"
go build -trimpath -mod=readonly \
  -ldflags "-s -w -X main.version=$version -X main.commit=patch-${PATCH_SHA256:0:12} -X main.date=unknown" \
  -o "$tmp_out" ./cmd/golangci-lint
"$tmp_out" version | grep -qF "$version" || { rm -f "$tmp_out"; die "built binary does not report version $version"; }
mv -f "$tmp_out" "$out"
echo "$stamp_key" > "$out.stamp"
echo "build.sh: built $out ($("$out" version))"
