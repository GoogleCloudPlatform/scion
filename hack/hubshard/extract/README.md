# hubshard/extract

Moves shared test-helper declarations of `pkg/hub` **verbatim** into dedicated,
helper-only `*_helpers_test.go` files (same package), and verifies such a move.

Why: almost every `pkg/hub` test file declares or uses a helper that other test
files use (`tid`, `testServer`, `doRequest`, ...), so the test files form one
connected component and build-tag sharding cannot split them. With the shared
helpers in their own untagged files, a shard needs only those files plus its own
tests.

It works on syntax only (`go/parser`). It never type-checks or compiles the
package, so running it on `pkg/hub` is cheap.

## Usage

```sh
go build -buildvcs=false -o /tmp/extract ./hack/hubshard/extract

# Move one family (re-runnable: a second run reports "nothing to move").
/tmp/extract move -dir pkg/hub -family hack/hubshard/extract/families/ids.txt
#   -n  dry run: print the plan, write nothing

# Verify against a snapshot of the tree before the move.
mkdir /tmp/hub-before && git archive main pkg/hub | tar -x -C /tmp/hub-before
/tmp/extract verify -before /tmp/hub-before/pkg/hub -after pkg/hub
```

A family file lists the destination and the requested names:

```text
# comment
dest id_helpers_test.go
tid
tidSlugSafe
```

An optional `build <expr>` line right after `dest` gives the `//go:build`
constraint of a new helper file (for helpers that use `!no_sqlite`-only code,
such as `newTestStore`). If the helper file already exists, it must carry that
constraint.

```text
dest server_helpers_test.go
build !no_sqlite
testServer
```

To regenerate a family move against a newer `main`, check out the generator,
run `move` again, and run `verify`; do not hand-rebase moved code.

## Bulk extraction

`bulk` moves **every** declaration that another test file uses into helper
files. No family file is needed: to regenerate on a newer `main`, run it again
and `verify`.

```sh
/tmp/extract bulk -dir pkg/hub -keep hack/hubshard/extract/bulk-keep.txt   # what pkg/hub uses
/tmp/extract bulk -dir pkg/hub -by origin              # one helper file per origin
/tmp/extract bulk -dir pkg/hub -origins '[a-g]*'       # origins matching a glob
#   -n  dry run: print the first round's plan, write nothing
```

Where the declarations go (each helper file has exactly one build constraint,
its origins' own):

- `-by area` (the default): the area is the origin's file name up to its
  first `_` (`handlers_agent_test.go` -> `handlers`). Declarations go to
  `<area>_helpers_test.go` from unconstrained origins,
  `<area>_sqlite_helpers_test.go` from `!no_sqlite` origins, and
  `<area>_<tag>_helpers_test.go` for any other constraint (`<tag>` is the
  constraint's letters and digits, `!` spelled `not`).
- `-by origin`: `x_test.go` -> `x_helpers_test.go`.

How it works:

- A declaration counts as cross-file used when a declaration in a different
  `_test.go` file refers to it (the same syntactic references `move` uses).
- Each round makes **one `move` per dest file**: the requested names are the
  cross-file declarations of all of that dest's origins, so the usual closure,
  method and constraint rules apply. Each family is planned on a fresh load,
  and asks only for names still declared in its origins (an earlier family of
  the round may have taken one as its closure). So every declaration moves
  once.
- In bulk, the closure is local: it takes a dependency only from a non-helper
  test file with the dest's own constraint. It never empties another helper
  file into this one, and a dependency with a different constraint stays
  where it is (same package, still visible) instead of failing the family.
- A grouped `var (...)`/`const (...)`/`type (...)` block moves whole.
- It repeats until no origin has a cross-file declaration left. A moved helper
  that uses a declaration its origin keeps makes that declaration cross-file
  used in the next round. Only rounds that move something are counted.
- It never takes declarations from `*_helpers_test.go` files. Some declarations
  are reported (`note ...`) and left in place: test entry points
  (`Test*`/`Benchmark*`/...), and names declared more than once (per-platform
  files).
- `-keep FILE` lists declarations (one name per line, `#` comments) that bulk
  never moves: they are not requested and no closure takes them. pkg/hub's
  list is `bulk-keep.txt`; each entry names the guard it satisfies.
- **Reviewer check:** guards that pin declarations to FILE NAMES (per-file
  ratchets like `hack/hub-store-reassign-baseline.txt`, call-site allow-lists
  in AST tests) need a `-keep` entry. Run every `hack/check-*.sh` and those
  AST/guard tests on the regenerated tree before pushing.
- If `move` refuses a dest's family, **every origin of that dest** is reported
  (`skip <dest> (from <origins>)`) and left alone for the rest of the run.

## What `move` does

- Moves each requested top-level declaration with its doc comment and any
  comment trailing its last line. A type takes all its methods with it.
- Also moves any test-file declaration that is used **only** by declarations
  being moved (transitively). Declarations used anywhere else stay put; they
  are in the same package, so the moved code still sees them.
- Writes the helper file: license header, build constraint (if any), package
  clause, the imports the moved code needs (std group, then others), then the
  existing helper-file declarations followed by the moved ones. Then gofmt.
- Removes the moved declarations from their source files, drops imports those
  files no longer use, and gofmts them. A source file left with no
  declarations is deleted. Free-floating comments (for example a
  `// Helpers` banner above the moved code) are left where they are.

## What `move` refuses

- A destination that is not a base name ending in `_helpers_test.go`.
- A name declared zero or several times, a method by name, or a declaration in
  a non-test file.
- Part of a grouped `var (...)` / `const (...)` / `type (...)` declaration.
- A source file that is not gofmt-clean (the final gofmt would touch unrelated
  lines), or whose import package names cannot be inferred (give the import an
  explicit name).
- An existing helper file with free-floating comments (a regeneration would drop
  them).
- **Build constraints.** Moving from a file with constraint S into a helper file
  with constraint D is allowed if S == D (the declaration is then built under
  exactly the same tag sets as before, so its users are unaffected, whatever
  their own constraints), or if D is empty and the moved code needs nothing that is not
  always built: every package-level name it uses is declared in an
  unconstrained file (or moves too), and every import it needs is already
  imported by an unconstrained file. So moving a helper out of a
  `!no_sqlite` file into an untagged helper file never adds a dependency to the
  `no_sqlite` build.

## What `verify` checks

Over **every** top-level declaration of the package (not only the moved ones):

- each declaration exists exactly once before and after, byte-identical
  (including doc comment); moved ones are listed;
- a moved declaration's build constraint either stays the same or widens to
  unconstrained;
- the set of `Test`/`Benchmark`/`Fuzz`/`Example` functions and their file
  constraints are unchanged, so `go test -list` is unchanged under every tag set;
- the multiset of free-floating comments is unchanged.

It exits non-zero on any difference.
