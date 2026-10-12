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

## Exporting helpers to a support package (`export`)

`export` moves helper declarations out of `pkg/hub` **verbatim** into a
non-test support package that never imports `pkg/hub` (by default
`pkg/hub/internal/hubtestutil`), so packages whose tests cannot import
`pkg/hub` (future `pkg/hub/<area>` packages) can use them. `pkg/hub` keeps
every old name through one generated, untagged alias file
(`zz_alias_hubtestutil_test.go`), so its tests need no edits.

```sh
go build -buildvcs=false -o /tmp/extract ./hack/hubshard/extract
mkdir /tmp/hub-before && git archive HEAD pkg/hub | tar -x -C /tmp/hub-before
/tmp/extract export -family hack/hubshard/extract/families/hubtestutil-wave1.txt   # -n: plan only
/tmp/extract verify-export -before /tmp/hub-before/pkg/hub -after pkg/hub \
    -family hack/hubshard/extract/families/hubtestutil-wave1.txt
```

`verify-export` finds the target as `-after`'s `internal/hubtestutil` by
default, so it works unchanged on a `git archive` of a commit
(`-after /tmp/gen/pkg/hub`); pass `-to`/`-import` for another target.

The family file lists the names to move, plus optional `rename old=New`
(export name; the default upper-cases the first letter) and
`field T.f=F` (export a field of a moved struct) lines.

What `export` does:

- Takes the requested declarations (only from `*_helpers_test.go` files)
  and the methods of requested types. It refuses a test entry point, part
  of a grouped declaration, and any moved declaration that uses a
  declaration that stays (request that one too, or leave both in place).
- Exports only the names that staying code uses, plus the moved types that
  staying code can reach through them: types in an exported func's or typed
  var's signature, in an exported type's exported fields, and in its exported
  methods' signatures (`GitSparseCheckoutStub.Calls` returns
  `[]GitSparseCheckoutCall`). Inside the moved code it renames exactly those
  identifiers; everything else, doc comments included, is byte-for-byte
  the original. Doc comments keep the old names.
- Writes one target file per origin (`x_helpers_test.go` -> `x.go`). Target
  files are unconstrained, so a declaration from a constrained origin must
  need nothing constrained (the `move` widening rule).
- Writes the alias file, one declaration per exported name:
  - type: `type x = pkg.X`; const: `const x = pkg.X`;
  - func: a wrapper with the original signature that forwards its
    arguments. If the original calls `t.Helper()` (as a top-level statement),
    the wrapper does too, so failures are still reported at the test's line.
    A `Helper()` call anywhere else in the body (a nested block, a closure,
    a non-parameter receiver) is refused, since a wrapper could not mirror it;
  - var: a value alias (`var x = pkg.X`) only for a slice or map literal,
    which shares its backing store; a pointer alias (`var x = &pkg.X`) for a
    var declared with a type and no value (a state holder such as
    `atomic.Bool`), so method calls reach the one instance. Any other var,
    and any var that the package assigns, increments or takes the address
    of, is refused: an alias copy would diverge.
- Rewrites a staying selector for a `field` export only where its operand
  syntactically has the type: a call of a moved func that returns it, or a
  local declared as `x := F(...)` / `var x = F(...)` from such a call and
  never written again (no reassignment, `++`, range assignment or `&x`).
  Shadowed locals resolve to their own declaration, and a call only counts
  if it names the package-level func (not a local shadowing it). Other
  selectors with the same name are listed as INFO; the compiler rejects any
  missed access. Only selectors are handled: composite-literal keys of the
  type in staying code (`T{f: ...}`) are not rewritten.
- Deletes helper files left without declarations, and drops unused
  imports.
- Lists review notes: source-scanning test files (moved code leaves their
  view; check that none of them matched it) and moved code that captures
  stacks or names.

What `verify-export` checks, independently of the rendering:

- every moved declaration, with its doc comment, has the same Go token
  stream (comments included) in the target, except identifiers that the
  rename map changes (old -> new, nothing else), and the target's non-test
  files declare nothing else;
- every other declaration of the source package is byte-identical, except
  field-export call sites, whose tokens differ only by the field renames;
  the test entry points (so `go test -list` under every tag set) and the
  free-floating comments are unchanged;
- the alias file, the edited source files and the target files are exactly
  what a fresh `export` on the before tree writes (no hand edits).

Then `make hubshard`, since deleted helper files leave stale
`assignment.txt` entries.

A hand-written comment fix on top of a generated export (for example doc
comments that still use the old names) goes in its own commit, and
`comment-only` checks it touches nothing but comments:

```sh
mkdir -p /tmp/gen /tmp/cmt
git archive <generated commit> pkg/hub | tar -x -C /tmp/gen
git archive <comment-only commit> pkg/hub | tar -x -C /tmp/cmt
/tmp/extract comment-only -before /tmp/gen/pkg/hub -after /tmp/cmt/pkg/hub
/tmp/extract comment-only -before /tmp/gen/pkg/hub/internal/hubtestutil -after /tmp/cmt/pkg/hub/internal/hubtestutil
```

Compare two archives of commits, not the working tree: later commits (or
uncommitted edits) would otherwise show up as differences, or hide them.
Comments the toolchain reads count as code: `//go:` directives (build
constraints, `//go:embed`, `//go:linkname`, ...), `// +build`, line
directives and cgo's `//export`, and every comment of a file that imports
`"C"` (the cgo preamble).
