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
