# Patched golangci-lint (interim; dominikh/go-tools#1839)

`build.sh` builds golangci-lint from source with one patch to
`honnef.co/go/tools` (staticcheck / `unused`). This is the single copy of that
build. Its users:

- `.github/workflows/ci.yml`, the `golangci-lint` job (GitHub-hosted and the
  cr-ci runner alike): restores the binary from the Actions cache, or builds
  it, then runs it with today's flags.
- `make golangci-lint` (and `make ci-full`): builds `./build/tools/golangci-lint`
  first, unless `GOLANGCI_LINT` points at another binary.
- `cr-ci/runner/Dockerfile`: once cr-ci fixup PR 2 lands, the cr-ci runner
  image builds and ships the same binary from this directory.

## Why

`unused` calls `objectpath.For` once per object, and every call builds a fresh
`objectpath.Encoder`, which re-sorts the whole package scope. The cost is
quadratic in package size. On pkg/hub's test variant (about 2,300 files, 1.07 M
lines in one package) `unused` alone took 330-400 s, single-threaded: the long
tail of the lint job. Reusing one `Encoder` per graph took it to 5.6 s, with
identical findings, and the full lint reported the same 77 issues as the
stock binary. Upstream: dominikh/go-tools#1839 (same diagnosis; also #1394).

## What is pinned (`VERSIONS`)

| Pin | Value | Checked by build.sh |
|---|---|---|
| `GOLANGCI_LINT_VERSION` / `GOLANGCI_LINT_SUM` | v2.15.0, Go module `h1:` checksum | `go mod download` (GOSUMDB) and an explicit comparison with the pin |
| `GO_TOOLS_VERSION` / `GO_TOOLS_SUM` | v0.8.2, Go module `h1:` checksum | the same, plus: it must be the version golangci-lint's go.mod requires |
| `PATCH_SHA256` | sha256 of `go-tools-1839.patch` | `sha256sum` |

No prebuilt binary is downloaded: both modules come from the Go module proxy
as source zips, checksum-pinned; every other dependency is verified against
golangci-lint's own go.sum. The binary reports its version as
`2.15.0-scion-gotools1839`, built from `patch-<patch sha prefix>`.

## The patch

`go-tools-1839.patch`, against `unused/unused.go` at v0.8.2:

- adds a `pathEncoder objectpath.Encoder` field to `graph`;
- replaces `objectpath.For(obj)` with `g.pathEncoder.For(obj)`.

A `graph` is built per package and used on one goroutine, and the zero
`Encoder` is ready to use. `objectpath.For(obj)` is `new(Encoder).For(obj)`, so
the paths are the same by construction.

## Bumping golangci-lint before the fix is released

1. Set `GOLANGCI_LINT_VERSION`, then `GO_TOOLS_VERSION` to the
   `honnef.co/go/tools` version its go.mod requires.
2. Get both checksums: `go mod download -json <module>@<version>` prints `"Sum"`.
3. If `unused/unused.go` changed, regenerate the patch against the new go-tools
   source and update `PATCH_SHA256` (`sha256sum go-tools-1839.patch`).
4. Run `build.sh` once; it refuses to build if any pin is inconsistent.

## Removal plan

Drop all of this once a golangci-lint release includes a go-tools release
with the fix (watch dominikh/go-tools#1839, then golangci-lint's
`honnef.co/go/tools` bump in its release notes):

1. In `.github/workflows/ci.yml`'s lint job, delete the restore / build / save
   steps for the patched binary, and give `golangci-lint-action` back
   `version: <the fixed release>` instead of `install-mode: none`. Remove
   the `cr-ci/runner/golangci-lint/` alternative from the `changes` job's
   `go` regex.
2. In the Makefile, restore a plain `GOLANGCI_LINT` (installed binary) and
   delete the `golangci-lint-build` target.
3. In `cr-ci/runner/Dockerfile` (if cr-ci fixup PR 2 has landed its
   build stage), go back to the release binary download (pinned version and
   sha256 from the release checksums).
4. Delete this directory.
