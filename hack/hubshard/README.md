# hubshard

`hack/hubshard` splits the `pkg/hub` **test files** into build-tag shards, so a
shard's test binary compiles with a fraction of the memory the full `pkg/hub`
test binary needs. Production code is not touched.

- **Default build (no tags): unchanged.** Every test file compiles and runs,
  because each shard constraint is `!hubshard || hubshard_K`.
- **`-tags hubshard,hubshard_K`**: the binary contains the `pkg/hub` sources,
  the **common** test files, and only shard K's test files.

A sharded file's build line is its own constraint (if any) AND the shard
clause:

```go
//go:build !no_sqlite && (!hubshard || hubshard_3)
```

## Commands

```sh
make hubshard                    # (re)apply the tags: go run ./hack/hubshard
make hubshard-check              # fail if any file or the assignment is stale
make test-hub-shard SHARD=2      # go test -tags hubshard,hubshard_2 ./pkg/hub
make build-hub-shard SHARD=2     # compile only (go test -c), binary discarded
```

Other tags combine as usual, e.g. `go test -tags no_sqlite,hubshard,hubshard_2`.
The union of the shards runs every test exactly once, except the tests in
common files, which run in every shard.

Flags: `-n N` sets the shard count (it rebalances everything when it differs
from the assignment file), `-rebalance` recomputes all assignments, `-check`
writes nothing and fails if anything is out of date, `-v` lists the common
files (with the reason) and the multi-file groups, and `-dumpgraph FILE`
writes the file dependency graph.

## When to re-run

Re-run `make hubshard` after adding, deleting or renaming `pkg/hub` test
files, or after changing which helpers a test file uses. A new **untagged**
file is always safe: it compiles in every shard. It just costs memory in each
shard until the generator assigns it.

A file has to be common when a sharded file in a different shard starts
using a helper it declares. Until the generator has re-run, that shard fails
to compile with `undefined: helperX`. That is a loud failure, never a silently
skipped test.

**Moving a test file out of `pkg/hub`** (for example with `hack/pkgmove`):
its `(!hubshard || hubshard_K)` clause moves with it. Remove the clause by
hand. `make hubshard` and `make hubshard-check` scan the whole repository
(skipping hidden directories, `node_modules` and `vendor`) and fail while any
`.go` file outside `pkg/hub` itself, including `pkg/hub` subpackages, has a
hubshard tag in its build line: a `-tags hubshard,hubshard_J` build would
silently drop that file from its package.

`make hubshard-check` is part of `make ci` (it only parses files).

## Test flags

A `flag.Bool` registered in a sharded file (for example `-update` in
`agent_create_inherited_golden_test.go`, or `-update-gcp-identity-fixture`)
exists only in that file's shard binary. Under `-tags hubshard,hubshard_K`,
pass it only to that shard; the other shards' binaries reject it as an
unknown flag. The default build has every flag.

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
6. **Assignment.** `assignment.txt` records every file's shard (`0` =
   common). Existing entries are kept: a group goes to the shard holding most
   of its lines. A file that was common stays common while another test file
   still uses it. New groups go to the lightest shard by lines, largest group
   first, with ties broken by name. So a file's shard does not move when
   unrelated files change.

Every step is deterministic, and the tool is idempotent: a second run changes
nothing.

## Current numbers (N=6)

1366 test files, 612,737 lines as the generator counts them (before
tagging). The shared helpers live in 176 helper-only `*_helpers_test.go` files (`hack/hubshard/extract`), so the common set is
**184 files, 48,273 lines (7.9%)**: the 176 helper files, the 2 `TestMain`
files, and 6 files that declare something a helper file still needs (kept in
place, for example because a guard pins the declaration to its file). The
common files hold 91 test functions, which run in every shard. Each shard
gets about 94.1k lines.

Measured at N=6 (16G default form: `ulimit -v 16000000`, `GOMAXPROCS=2`,
`GOMEMLIMIT=6GiB`, `GOGC=40`, `-p 1`, `-gcflags=<pkg/hub>=-c=1`, cold cache;
peak RSS of the largest process, from `hack/buildstats`):

| Build | Test lines (`wc -l`) | Compile wall | Peak RSS | Tests listed |
|---|---|---|---|---|
| default (no tags) | 613,227 | 901 s | 12.44 GiB | 13,467 |
| shard 1 | 142,442 | 235 s | 5.89 GiB | 2,277 |
| shard 2 | 142,405 | 161 s | 5.45 GiB | 2,183 |
| shard 3 | 142,463 | 167 s | 5.51 GiB | 2,346 |
| shard 4 | 142,436 | 167 s | 5.45 GiB | 2,321 |
| shard 5 | 142,432 | 172 s | 5.55 GiB | 2,375 |
| shard 6 | 142,414 | 165 s | 5.80 GiB | 2,420 |

(The shards ran in this order in one fresh cache, after a `./cmd` build, so
shard 1's wall also includes compiling the test-only dependencies.)
Between the default build and a shard, the peak grows by about 1.45 GiB per
100k test lines. N=4 would put about 189k test lines in each shard, so its
projected peak is about 6.3 GiB, over the 6 GiB per-compile target. That is
why N=6.

Regenerate from scratch with:

```sh
make hubshard ARGS='-n 6 -rebalance'
```
