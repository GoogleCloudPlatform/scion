# pkgmove

`pkgmove` moves a set of files out of a Go package into a new package. The
move does not change behaviour, and every package that used to compile still
does. Existing callers in the source package need no edits, because the tool
leaves an alias file behind.

It is the generator for the `pkg/hub` split. Every move PR is **generated
against the current main tip**. When main moves, the PR is regenerated rather
than rebased (see [Workflow](#workflow-regenerate-dont-rebase)).

## Usage

```sh
go build -buildvcs=false -o /tmp/pkgmove ./hack/pkgmove

# 1. Plan only: prints files, renames, alias entries and the safety report.
/tmp/pkgmove -dry-run -from pkg/hub -to pkg/hub/maintenance \
    maintenance_executors.go maintenance_executors_test.go

# 2. Real run (in a clean checkout of the main tip).
/tmp/pkgmove -from pkg/hub -to pkg/hub/maintenance \
    maintenance_executors.go maintenance_executors_test.go
```

The files to move are named by base name, relative to `-from`. List the
`_test.go` files you want to move explicitly. The tool reports any companion
file (`foo.go` / `foo_test.go`) that you leave behind. Non-Go files, such as
`go:embed` assets, can be listed too and are moved verbatim.

| Flag | Default | Meaning |
|---|---|---|
| `-from` | (required) | source package directory |
| `-to` | (required) | target package directory; must not contain Go files yet |
| `-name` | base of `-to` | target package name |
| `-area` | package name | alias file stem: `zz_alias_<area>.go` |
| `-tags` | none | build tags for the analysis (see [Build tags](#build-tags)) |
| `-dry-run` | off | print the plan and safety report; touch nothing |
| `-typecheck` | on | after the move, type-check the target, then the source with its in-package tests (in process, no compile) |
| `-vet` | **off** | also run `go vet` on both packages. vet is banned on some brokers, so it is opt-in |
| `-allow-field-export` | off | allow exporting struct fields (reported as HIGH) |
| `-no-git` | off | use `os.Rename` instead of `git mv` / `git add` |
| `-report` | `<from>/zz_alias_<area>_safety.txt` | where the safety report is written |

Exit codes: `0` success, `1` the move cannot be generated (errors are listed in
the report, and the tree is untouched), `2` usage, `3` tool or post-check failure.

`pkg/hub` note: the analysis type-checks the whole source package with its
tests, so for `pkg/hub` even `-dry-run` is a **16G run**. Follow the compile
rules: send a notice, wait for the GO, and use the mandated form.

## What it does

1. **Moves** the files with `git mv` and rewrites their package clauses
   (`package hub` becomes `package <name>`, and `package hub_test` becomes
   `package <name>_test`).
2. **Finds every reference across the new boundary.** It type-checks the
   source package with its in-package tests, using `go/types` over export data
   from `go list -export`. It needs no dependencies beyond the standard
   library.
   - **Forward references** (staying code uses moved code):
     - Unexported package-level symbols are exported with a deterministic
       rename (`fooBar` becomes `FooBar`).
     - Unexported methods and fields used by staying code are exported too.
     - Methods that share a name through interface satisfaction inside the
       source package are renamed as one group. This includes anonymous
       interfaces in type assertions, so `v.(interface{ run() })` keeps
       matching.
     - Collisions fail loudly: two names exporting to the same name, a new
       name shadowed by a local, a field or method name already present on
       the type, or a change in which member a selector resolves to.
   - **Backward references** (moved code uses staying code) are rejected. The
     alias file makes the source import the target, so the target cannot
     import the source. Move the dependency too, or invert it first (P1-3
     does this for `errors.go`).
3. **Generates the alias file** `zz_alias_<area>.go` in the source package:
   - types: `type foo = target.Foo`, including generic aliases
     `type P[K comparable] = target.P[K]`;
   - functions: wrapper functions with the original signature, including
     generic ones;
   - constants: `const foo = target.Foo`.

   Exported moved symbols are always aliased. Unexported ones get an alias
   only when staying code uses them. Aliases used only by staying tests go to
   `zz_alias_<area>_test.go`. Symbols declared in files with a build
   constraint get a separate `zz_alias_<area>_cN.go` that carries the same
   constraint.

   **Package-level vars are never aliased.** `var foo = target.Foo` would be a
   copy, which changes behaviour for assignments, hook overrides in tests and
   error identity. Instead, references to them in staying files are rewritten
   to `target.Foo`, with an import added.
4. **Rewrites imports.** It adds the target import where vars are rewritten.
   In moved external tests, it re-qualifies `hub.X` as `target.X` and drops
   the source import if nothing else uses it. All edits are byte-offset
   edits, so comments and layout are preserved, and every touched file is
   then gofmt'd.
5. **Runs sanity checks:**
   - `go list -test` on both packages;
   - an in-process type-check of the target (with and without its tests) and
     of the source with its tests;
   - `go vet`, only when `-vet` is passed.
6. **Writes the safety report** (see below). It is printed, and also written
   next to the alias file. It is left unstaged. Paste it into the PR, then
   delete the file.
7. **Is deterministic.** Every list is sorted, and the report contains no
   timestamps or absolute paths, so the same inputs on the same commit give a
   byte-identical tree and output. The tests check this.

## Safety report

A pure move can still change behaviour, mainly through **initialisation
order**. The moved package is initialised before the package that imports it.
So its `init()` functions and package-level var initialisers now run before
**every** initialiser of the source package, and no longer in the source's
dependency/declaration order. The report has these sections, sorted by
severity:

| Level | Finding |
|---|---|
| ERROR | anything that makes the move impossible or unsafe (listed below); nothing is changed |
| HIGH | `init()` in a moved file |
| HIGH | a moved package-level var initialiser that calls code of the source package, or an immediately invoked func literal |
| HIGH | `//go:linkname` and `//go:embed` directives |
| HIGH | exported struct fields (only with `-allow-field-export`) |
| WARN | a moved var initialiser that reads vars or funcs of other files |
| WARN | methods exported to new names (they may newly satisfy interfaces) |
| WARN | `%T`, `reflect.TypeOf`, `gob.Register`, `runtime.Caller` or `FuncForPC` in moved files: type and function names now print as `target.X` |
| WARN | the package doc comment moving |
| WARN | `//go:generate` directives |
| WARN | function aliases that had to fall back to a var |
| INFO | staying var initialisers that read moved symbols |
| INFO | initialisers that call other packages (harmless: imported packages initialise first in both layouts) |
| INFO | moved external tests, and moved files with build constraints |
| INFO | files excluded by the build tags (scanned by name only) |
| INFO | test companions left behind |

**Errors** (the tool refuses the move):

- A method would end up in a different package from its receiver type, in
  either direction. Go forbids methods on non-local types.
- A backward reference: moved code uses a symbol that stays.
- **Separating a test from its helpers:**
  - a moved test uses a helper declared in a staying `_test.go` file;
  - a staying test (in-package or external) uses a helper declared in a moved
    `_test.go` file. Test-only symbols cannot be aliased.
- A rename collision or shadowing.
- An export that cannot be done safely:
  - exporting a struct field changes `encoding/json`, yaml, gob and reflection
    visibility, so it is refused unless `-allow-field-export` is passed;
  - a method cannot be exported to a name with dynamic meaning (`String`,
    `Error`, `MarshalJSON`, `Read`, `ServeHTTP`, ...).
- A moved exported var is referenced from another package of the module,
  where it cannot be aliased.
- A moved file is excluded by the build tags, or uses cgo.
- A `go:embed` pattern matches files that are not in the move set.
- The target directory already has Go files, or an alias file already exists.

## Build tags

The analysis uses one build configuration: the default, plus `-tags`.

- Moved files must be included in that configuration.
- Staying files that the configuration excludes, such as `//go:build
  integration` files in `pkg/hub`, cannot be type-checked. They are scanned by
  name: any identifier that matches a moved package-level name gets an alias.
  This is conservative.
- A moved var referenced from an excluded file is an error.
- A selector on an excluded file that matches a renamed member is a WARN.
  Check those files under their own tags.

## Not supported (rejected, or out of scope)

- Moving into a package that already has Go files. Each move creates a new
  package. Moving a second batch into an existing package would also need
  references through the first batch's aliases to be rewritten.
- Backward-only moves, where the target imports the source and nothing aliases
  back (for example, moving e2e tests out of `pkg/hub`).
- Analysing several build configurations in one run.
- cgo files.
- Type-checking moved external tests. They are rewritten syntactically, and
  `go list -test` checks their imports.

## Workflow: regenerate, don't rebase

1. When the merge window opens, start from a clean checkout of the current main
   tip on a fresh branch.
2. Run `-dry-run` and review the plan and safety report. Resolve any ERRORs on
   main first (for example, invert a back-reference), then regenerate.
3. Run the real move. The tool stages the renames, the edits and the alias
   files. Commit them:
   ```sh
   git commit -m "Move <area> to pkg/hub/<name> (generated by hack/pkgmove)"
   ```
4. Put the plan, the safety report, the exact command line, and the
   compile/test evidence in the PR description.
5. **If main moves before the PR merges, do not rebase by hand.** Reset the
   branch to the new main tip and re-run the same command line. The result is
   deterministic, so the reviewer can re-run it and diff the output against
   the PR.
6. Branches that edited the moved files re-apply their diff to the new path.
   Git's rename detection usually does this automatically. New code in
   `pkg/hub` that calls a just-moved unexported helper fails to compile at
   once, and the fix is one alias line or a `target.` qualifier.

## Tests

```sh
go test ./hack/pkgmove/...           # golden, determinism, dry-run and unit tests
go test ./hack/pkgmove/ -update      # rewrite the goldens after an intended change
```

Each fixture under `testdata/<case>/in` is a small module. A test copies it to
a temp dir, moves files from `hub/` to `hub/sub/`, and compares the result with
`want/` and `stdout.golden`. Failing cases must leave the tree untouched.

| Fixture | What it covers |
|---|---|
| `basic` | export renames, aliases, generics, var rewrite, test-only alias, init detection |
| `tags` | build constraints |
| `xtest` | external tests, embed and linkname |
| `embed` | embedded-field rename |
| `iface` | interface-group rename |
| `fields` | field export with `-allow-field-export` |
| `methods` | methods on a type that stays, rejected |
| `backref` | back-reference, rejected |
| `testsep` | test/helper separation, rejected |
| `collide` | rename collision and shadowing, rejected |
| `dynamic` | method exported to `String`, rejected |
| `cgo` | cgo file, rejected |
