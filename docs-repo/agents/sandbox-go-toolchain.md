# Go builds, tests and lint in an agent container

- **Toolchain:** Go 1.26.1 everywhere (`go.mod`, `image-build/core-base/Dockerfile`, CI). `fmt-check` failures are real, often misaligned struct fields: run `make fmt` and confirm the diff is pure alignment.
- **extras modules** (`extras/*`) have their own `go` lines. Each Dockerfile's `golang` image must be at least that version; bump both together (`make check-extras-go-images`).
- **VCS status error** (`exit status 128`) in a worktree: add `-buildvcs=false`.
- **Running code at main's tip:** `main` is checked out elsewhere, so use a detached worktree: `git worktree add --detach /tmp/check <main-sha>`, then `git worktree remove --force /tmp/check`. For single files, `git show main:path`.
- **golangci-lint OOM (exit 137):** `GOGC=40 golangci-lint run --new-from-rev=main --concurrency=1 ./pkg/foo/... ./cmd/...`. `--new-from-rev=main` shows only issues you introduced.
- **pkg/hub tests run in shards.** The whole test package needs about 12 GiB to compile; never build it. `make test-hub-shard SHARD=K` (about 6 GiB; `NOSQLITE=1` for no_sqlite), locate a test with `hack/hubshard/hubshard.sh -locate TestName`. See `hack/hubshard/README.md`.
- **`-race` on pkg/hub** can OOM under a `ulimit -v` cap. Run targeted tests without `-race`, run `-race` uncapped, or use the nightly Race Detection workflow (`.github/workflows/race-detection.yml`): reporting-only, no_sqlite subset; SQLite-backed tests get race coverage only from an uncapped local run.
- **Leaked `SCION_*` env vars** (`SCION_PROJECT`, `SCION_CREATOR`, …) can affect config, hub and sync tests. Rule them out with `env -u SCION_PROJECT … go test …`.
- **Latest main:** you cannot fetch from a worktree. Once the human has pulled into the primary checkout, `git rebase main` sees it. When fixing a test, check whether it already fails on main (detached worktree) so you fix the current version.
