# hubshard

`hack/hubshard` splits the `pkg/hub` **test files** into build-tag shards, so
a shard's test binary compiles with a fraction of the memory the full
`pkg/hub` test binary needs (about 6 GiB instead of 12). Production code is
not touched.

**The tags are never committed.** Every run computes the shards from the
tree and writes the tagged copies of the test files to a temporary
directory, together with an `overlay.json` for `go build -overlay`:

```sh
go run ./hack/hubshard -overlay /tmp/ov
go test -overlay /tmp/ov/overlay.json -tags hubshard,hubshard_3 ./pkg/hub
```

- **The tree is the default build.** Without the overlay every test file
  compiles, exactly as before sharding. Nothing to regenerate, nothing to
  commit, no merge conflicts on build lines.
- **With the overlay and `-tags hubshard,hubshard_K`** the binary contains
  the `pkg/hub` sources, the **common** test files, and only shard K's test
  files. A sharded file's build line in the overlay is its own constraint
  (if any) AND the shard clause:

  ```go
  //go:build !no_sqlite && (!hubshard || hubshard_3)
  ```

CI never compiles the whole `pkg/hub` test package: the `pkg/hub shard K/N`
jobs run the shards in parallel, and the `pkg/hub SQLite Tests` check
passes only when all of them do (N = 6 today, from `-shards`).

## Commands

```sh
make test-hub-shard SHARD=2            # compile shard 2, check its test list, run it
make test-hub-shard SHARD=2 NOSQLITE=1 # the same with -tags no_sqlite (RACE=1 adds -race)
make build-hub-shard SHARD=2           # compile only (binary discarded)
make vet-hub-shard SHARD=2             # go vet the shard with both vet tag sets
make test-hub-postgres SHARD=2         # the shard's Postgres tests (needs the Postgres env vars)
make test-hub-postgres-default SHARD=2     # ... only the default-tag ones (CI: in the shard job)
make test-hub-postgres-integration SHARD=2 # ... only the -tags integration ones (CI: own job)
make hub-postgres-integration-shards   # the shards holding a -tags integration Postgres test
make hubshard-parity                   # parse only: every test runs in exactly one shard
make test-hub-shards                   # every shard in turn (NOSQLITE=1, RACE=1 as above)
make test-hub-sqlite                   # every shard in turn, plus the non-hub SQLite packages
go run ./hack/hubshard                 # print the shard sizes
go run ./hack/hubshard -v              # ... and the common files (with the reason) and groups
```

The make targets call [`shard.sh`](shard.sh), which writes the overlay to a
temporary directory (removed on exit), so they never leave modified files
behind. Other tags combine as usual: `shard.sh` passes `no_sqlite` or
`integration` next to the shard tags.

Tool flags: `-n N` (shard count, default 6), `-shards` (print 1..N; the
CI matrix and `make test-hub-shards` loop over it),
`-overlay DIR`, `-parity`, `-list K` (the tests shard K's binary compiles,
under `-tags`), `-skip K` (the `go test -skip` pattern for shard K, see
below), `-locate NAME...` (the shard that runs each named test), `-untag`
(strip clauses an older generator committed), `-v`, `-dumpgraph FILE`.

## Exactly once

Every shard compiles the common files, so their tests are in every shard's
binary. `shard.sh` passes `go test -skip '^(TestA|TestB|...)$'` (from
`hubshard -skip K`) in shards 2..N, so a common file's tests run only in
shard 1 and every test runs in exactly one shard.

`make hubshard-parity` (Build & Test, and `make check-custom`) checks,
under the default, `no_sqlite`, `integration` and vet-integration builds,
with `go/build` itself deciding which file each build compiles:

- the union of the shards runs exactly the tests the plain build runs, each
  exactly once (applying the real `-skip` pattern);
- every rewritten file compiles in its own shard and in no other (a common
  file in all), so a `//go:build` line `go/build` ignored would fail here
  instead of silently putting a file in every shard;
- no file uses a declaration from a file of another shard (the shard build
  would fail with `undefined: helperX`), and every blank import reaches
  every shard (see below);
- the per-shard vets (`-tags no_sqlite` and `-tags 'integration volume_test
  tzcontract'`) cover every test file the module-wide `make lint` and
  `make vet-integration` cover;
- no `.go` file in the repository carries a hubshard tag and no generated
  imports file is committed. A clause left by the old generator (for
  example on a branch that predates this) is stripped with `go run
  ./hack/hubshard -untag`; a clause outside `pkg/hub` (a file moved by
  `hack/pkgmove`) is deleted by hand.

Each shard job also compares its compiled binary's `-test.list` with
`hubshard -list K` and fails on any difference, so the static check and
the real build cannot drift apart.

## Line numbers

The overlay keeps each file's line count where it can, so line numbers in a
shard's test output match the source. A file with a build line gets the
clause on that line. A file without one gets the clause on an empty `//`
line of its license header, when a blank line separates the header from
the package clause: `go/build` and `cmd/go` accept a `//go:build` line
anywhere among the leading `//` comments (see the comment on
`spareCommentLine` in `main.go`). Otherwise (no header, a `/* */` header,
or a header that is also the package doc comment) a line and a blank line
are added on top, which shifts that file's line numbers by 2 in shard
builds only.

## How files are classified

1. **Dependency graph (file level).** Every test file is parsed with
   `go/parser`. File X *uses* file Y when X mentions an identifier the parser
   could not resolve inside X (so not a local and not one of X's own
   declarations), and Y declares that name at top level. Methods tie a file to
   the test file that declares the receiver type, in both directions. A method
   a test file declares on a `pkg/hub` type is linked to every file that
   selects that name (`x.Name`). The analysis is syntactic and conservative:
   a false edge (for example a struct-literal key that happens to match a
   helper name) can only make a file common or group files together. It can
   never drop a needed file from a shard.
2. **Common seeds.** Helper-only `*_helpers_test.go` files (see
   `hack/hubshard/extract`), files with `TestMain`, and files with an `init`
   function that does more than `_ = x` discards.
3. **Closure.** Anything a common file uses is common.
4. **Groups.** The remaining files are split into connected components of
   the use graph. A group always lands in one shard, so every file a sharded
   file needs is either common or in the same shard.
5. **Cut.** While the largest group is bigger than total/`-maxgroupdiv`
   lines (default 16), one of its files moves to common, together with its
   closure. The candidates are the group's files that at least two group
   members use; the one chosen has the best ratio of "group members using
   it" to "lines added to common". A group without a candidate stays whole,
   and the cut stops there.
6. **Assignment.** Groups go, largest first (ties by name), onto the
   lightest shard by lines. Nothing is stored: the same tree always gives
   the same shards, and a change to the test files may move other files
   between shards. That is invisible (no committed tags) and covered by
   the parity check.

## Blank imports (driver registration)

A blank import is a dependency without a symbol: a file that blank-imports
`github.com/mattn/go-sqlite3` registers the `sqlite3` `database/sql`
driver, and another file that only calls `sql.Open("sqlite3", ...)` relies
on it without referencing anything the first file declares. The use graph
has no edge between them, so a shard without an importing file would
compile and then fail at run time (`sql: unknown driver "sqlite3"`).

So the overlay also holds generated, untagged
`pkg/hub/zz_hubshard_drivers*_test.go` files that blank-import every
package a `pkg/hub` test file blank-imports, so every shard links them:

- `zz_hubshard_drivers_test.go` holds the packages that at least one
  importing file imports without a build constraint;
- `zz_hubshard_drivers_K_test.go` (K = 1, 2, ...) holds the rest, one file
  per build condition, carrying `//go:build <condition>`, where the
  condition is the OR of the importing files' own constraints (including a
  GOOS/GOARCH file-name suffix as go/build reads it: `foo_linux_test.go`
  counts as `linux`). For example, `pgx/v5/stdlib` imported only by
  `!no_sqlite` files stays out of a `-tags no_sqlite` build.

They exist only in the overlay.

## Known limitations

- **Side effects without a blank import.** A package that a test file
  imports by name and that registers something in its `init` is linked into
  a shard only if a file of that shard (or a common file) imports it. If
  another shard's file relies on that registration by name only (a string
  driver name, a global registry lookup), the tool cannot see the
  dependency. Add a blank import of the package to the file that relies on
  it; the generated imports files then carry it to every shard.
- **`init` functions in test files.** A file with an `init` that does more
  than `_ = x` discards is common, so its `init` runs in every shard, as in
  the default build. Package-level initializers with side effects (`var _ =
  register()`) are neither detected nor warned about: syntactically they
  look like the common `var _ Iface = (*T)(nil)` assertions. Put such a
  call in an `init` function.
- **External test package.** Blank imports in `package hub_test` files are
  not copied into the imports files (the imported package might import
  `pkg/hub`, which would be an import cycle in package `hub`); `-parity`
  warns about them in sharded files.
- **Build tags are independent in the import coverage check.** It decides
  "constraint A implies B" over a truth table of the tags in A and B and
  does not know that, say, `linux` implies `unix`. That can only report a
  covered import as uncovered.

## Test flags

A `flag.Bool` registered in a sharded file (for example `-update` in
`agent_create_inherited_golden_test.go`, or `-update-gcp-identity-fixture`)
exists only in that file's shard binary. Under `-tags hubshard,hubshard_K`,
pass it only to that shard; the other shards' binaries reject it as an
unknown flag. The default build has every flag.

## Current numbers (N=6, as of commit 90546a309)

1395 test files, 625,431 lines as the generator counts them. The common set
is 186 files, 49,581 lines (7.9%), with 119 test functions. Each shard gets
about 96.0k lines of its own (194-207 files). The default build has 13,715
tests, and each runs exactly once:

| Shard | Own files | Own lines | Tests compiled | Tests run |
|---|---|---|---|---|
| 1 | 207 | 95,992 | 2,358 | 2,358 (includes the 119 common) |
| 2 | 206 | 95,958 | 2,444 | 2,325 |
| 3 | 201 | 95,958 | 2,203 | 2,084 |
| 4 | 195 | 95,993 | 2,679 | 2,560 |
| 5 | 206 | 95,958 | 2,247 | 2,128 |
| 6 | 194 | 95,991 | 2,379 | 2,260 |

Shards 2-6 compile the common files' 119 tests and skip them. The shard 1
overlay compile measured 5.86 GiB peak RSS (same 16G form as below, warm
dependency cache, 415 s at 2 CPUs).
`go run ./hack/hubshard` and `make hubshard-parity` print the current
figures.

Measured at N=6 with an earlier assignment (16G form: `ulimit -v 16000000`,
`GOMAXPROCS=2`, `GOMEMLIMIT=6GiB`, `GOGC=40`, `-p 1`,
`-gcflags=<pkg/hub>=-c=1`, cold cache; peak RSS of the largest process,
from `hack/buildstats`), when each shard held about 94.1k lines plus 48.3k
common:

| Build | Test lines (`wc -l`) | Compile wall | Peak RSS | Tests listed |
|---|---|---|---|---|
| default (no tags) | 613,227 | 901 s | 12.44 GiB | 13,467 |
| shard 1 | 142,442 | 235 s | 5.89 GiB | 2,277 |
| shard 2 | 142,405 | 161 s | 5.45 GiB | 2,183 |
| shard 3 | 142,463 | 167 s | 5.51 GiB | 2,346 |
| shard 4 | 142,436 | 167 s | 5.45 GiB | 2,321 |
| shard 5 | 142,432 | 172 s | 5.55 GiB | 2,375 |
| shard 6 | 142,414 | 165 s | 5.80 GiB | 2,420 |

The peak grows by about 1.45 GiB per 100k test lines, so the present
shards (about 145.6k lines each) project to about 6 GiB. When the shards
drift well past that, raise `defaultShards` in `main.go`: the CI matrix, the
make targets and the nightly loops all take the shard list from
`go run ./hack/hubshard -shards`.
