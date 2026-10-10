# pkgmove

`pkgmove` moves a set of files out of a Go package into a new package. The
move does not change behaviour, and every package that used to compile still
does. Existing callers in the source package need no edits, because the tool
leaves an alias file behind.

It also has two modes for the later stages of a split:
- **Test-only moves into an existing package** (see
  [below](#test-only-moves-into-an-existing-package)): `_test.go` files follow
  code that an earlier move took out, in waves.
- **`-rewrite-aliases`** (see [below](#-rewrite-aliases-shrinking-alias-files)):
  references to alias entries become direct references to their targets, so
  alias files can shrink.

Every move also [sees through existing aliases](#alias-references): a moved
file that uses an alias left by an earlier move refers to the alias target
directly.

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

# 3. A later wave of tests, into the package that step 2 created.
/tmp/pkgmove -from pkg/hub -to pkg/hub/maintenance maintenance_wave2_test.go

# 4. Rewrite references to the aliases of one package, and drop the unused
#    unexported entries (no -to: the aliases of every package).
/tmp/pkgmove -rewrite-aliases -from pkg/hub -to pkg/hub/apierr
```

The files to move are named by base name, relative to `-from`. List the
`_test.go` files you want to move explicitly. The tool reports any companion
file (`foo.go` / `foo_test.go`) that you leave behind. Non-Go files, such as
`go:embed` assets, can be listed too and are moved verbatim.

| Flag | Default | Meaning |
|---|---|---|
| `-from` | (required) | source package directory |
| `-to` | (required, except with `-rewrite-aliases`) | target package directory. It must not contain Go files yet, unless every moved Go file is a `_test.go` file ([test-only move](#test-only-moves-into-an-existing-package)). With `-rewrite-aliases` it selects the aliases of that package |
| `-name` | the existing package's name, else base of `-to` | target package name; for an existing package it must match |
| `-area` | package name | alias file stem: `zz_alias_<area>.go` |
| `-tags` | none | build tags for the analysis (see [Build tags](#build-tags)) |
| `-dry-run` | off | print the plan and safety report; touch nothing |
| `-typecheck` | on | after the move, type-check the target, then the source with its in-package tests (in process, no compile) |
| `-vet` | **off** | also run `go vet` on both packages. vet is banned on some brokers, so it is opt-in |
| `-allow-field-export` | off | allow exporting struct fields (reported as HIGH) |
| `-strict` | off | treat every HIGH finding as an error (see the [Safety report](#safety-report) table) |
| `-testmain-support` | none | import path of a test-support package exporting `RunTestMain(m *testing.M) int`; when moved tests leave a package that has a `TestMain`, generates a delegating `TestMain` in the target (see [TestMain](#testmain)) |
| `-no-git` | off | use `os.Rename` instead of `git mv` / `git add` |
| `-report` | `<from>/zz_alias_<area>_safety.txt` (`<from>/zz_alias_rewrite_safety.txt` with `-rewrite-aliases`) | where the safety report is written |
| `-rewrite-aliases` | off | move nothing: rewrite alias references in the source package and its external tests (see [`-rewrite-aliases`](#-rewrite-aliases-shrinking-alias-files)); takes no files |

Exit codes: `0` success, `1` the move cannot be generated (errors are listed in
the report, and the tree is untouched), `2` usage, `3` tool or post-check failure.

### Cost: every run on `pkg/hub` is a 16G run

- **What the tool compiles:** `go list -export -deps` builds export data only
  for the *imports* of the source package. The source package itself is never
  compiled, and test-variant (`-test`) export data is never requested.
- **What it type-checks in memory:** the source package's own files and its
  in-package `_test.go` files, from source, against that non-test export
  data. With `-typecheck`, the target and the source are type-checked again
  after the rewrite.
- **Why it still needs a GO:** for `pkg/hub`, the dependency build plus the
  in-memory type-check of the largest package and its tests count as a
  pkg/hub type-check under the compile rules. So every run, including
  `-dry-run`, needs a coordinator GO. Use the mandated form
  (`ulimit -v 16000000`, `GOMAXPROCS=2`, `GOGC=40`).
- **Measured cost:** see the P1-1 PR description.

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
     - Generic types are grouped conservatively by name. `types.Implements`
       is unspecified for uninstantiated generic types, and any instantiation
       may satisfy an interface dynamically. So each unexported method of a
       generic type is grouped with every unexported interface method of the
       same name in the package.
     - Collisions fail loudly: two names exporting to the same name, a new
       name shadowed by a local, a field or method name already present on
       the type, or a change in which member a selector resolves to.
   - **Backward references** (moved code uses staying code) are rejected. The
     alias file makes the source import the target, so the target cannot
     import the source. Move the dependency too, or invert it first (P1-3
     does this for `errors.go`).
   - **References to aliases** of the source package (left by earlier moves)
     are not backward references: they are rewritten to the alias target
     (see [Alias references](#alias-references)).
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

   **Exported non-generic functions are aliased as vars**
   (`var Foo = target.Foo`). Importers may use them as values, and a var
   keeps the func's identity (reflect `Pointer`) and its caller frames.
   Dependency-ordered initialisation sets the var before any initialiser of
   the source package that refers to it. The value can be taken from outside
   only by a non-generic func, so for exported **generic** funcs (always
   wrappers) a value use in another package or an external test is an
   ERROR.

   **Some unexported functions also get a var alias instead of a wrapper**,
   because calling them through a wrapper would change behaviour. Each one is
   reported as a WARN:
   - functions that call `recover()` directly: under `defer foo()`, recover
     only works when called by the deferred function itself;
   - functions that inspect their call stack, directly or through moved
     functions they call: `runtime.Caller`, `runtime.Callers`, `log.Output`
     call depth, or `testing` `Helper`. A wrapper adds a frame, so the
     reported caller changes.

   slog `AddSource` records the call site inside the moved function, so a
   wrapper does not affect it. A generic function in either group cannot be
   aliased as a var, so it is an error.

   **Funcs used as values are not routed through the wrapper.** When staying
   code uses a moved func as a value rather than calling it (`hook =
   defaultHook`, `reflect.ValueOf(f).Pointer()`), the reference is rewritten
   to `target.Foo`, like a var. A wrapper has a different identity. Value uses
   in tag-excluded files and in external tests cannot be rewritten, so they
   are reported as WARN.

   **Package-level vars are never aliased.** `var foo = target.Foo` would be a
   copy, which changes behaviour for assignments, hook overrides in tests and
   error identity. Instead, references to them in staying files are rewritten
   to `target.Foo`, with an import added. The same applies to funcs used as
   values. Those staying files therefore appear in the diff. The plan lists them under "Remaining source files edited"; copy
   that list into the PR description as expected changes.
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

## Alias references

After a move, staying code reaches the moved symbols through the alias file.
A later move of such code must not treat those aliases as staying code: the
tool sees through them and rewrites each reference to the alias target (for
example `writeError` becomes `apierr.WriteError`, with the import added). The
plan lists every rewrite under "Alias references resolved to their targets".

**What counts as an alias** (any other declaration is ordinary staying code):

| Declaration | Where | Why it is equivalent |
|---|---|---|
| `type x = pkg.Y`, `type x[P any] = pkg.Y[P]` (type parameters forwarded in order) | any file | an alias is the same type |
| `const x = pkg.Y` (one name, no type, one value) | any file | the same constant |
| `var x = pkg.F` for a non-generic func `F` | pkgmove-generated alias files only, and only if nothing assigns `x`, increments it, ranges into it or takes its address: in the package (by type), in its tag-excluded files and external tests (by name), and, for exported names, in every importer in the module | the var holds `F` itself, so calls and values are identical |
| `func x(p0 T) R { return pkg.F(p0) }` (parameters forwarded in order, generic ones explicitly instantiated with their own type parameters, signature identical to `F`'s) | pkgmove-generated alias files only | the generator never wraps a function that calls `recover()` directly or inspects its call stack (those get var aliases), so dropping the wrapper frame changes nothing |

A generated alias file is a `zz_alias_*.go` file carrying the generator's
comment ("by hack/pkgmove, so existing references in package ..."). Hand-written
wrappers and vars are not resolved, because the target might call `recover()`
or inspect its call stack, or the var might be a hook. When a moved file uses
one, the back-reference error says why it was not resolved.

**Checks** on each resolved reference:
- an embedded field whose type is an alias with a different name than its
  target (`struct{ errBox }` for `apierr.ErrBox`) would be renamed: ERROR;
- a wrapper used as a func value (not called) now has the target's identity:
  WARN;
- in a [test-only move](#test-only-moves-into-an-existing-package), an
  in-package test may not import the alias target if that package imports the
  target package (an import cycle): ERROR. A reference to an alias of the
  target package itself becomes a bare name.

Moved external tests are rewritten the same way (`hub.WriteJSON` becomes
`apierr.WriteJSON`), except wrapper values and renamed embedded fields, which
keep the source import.

## Test-only moves into an existing package

When the target directory already holds a package, only `_test.go` files
(plus assets) may move into it. This lets tests follow their source in waves
(P3-1: authz moves its source once, then its tests in four waves). Source
moves always create a new package; a move set with a non-test Go file is
refused with exit code 3, before anything is analysed.

- **Package clause:** each moved test keeps its kind. In-package tests
  (`package hub`) join the target package (`package authz`); external tests
  (`package hub_test`) join `authz_test`. `-name` defaults to the existing
  package's name and must match it.
- **Nothing is aliased or renamed.** The moved tests may use: other moved
  test files, aliases of the source (resolved, see above), and other
  packages. Anything else in the source is a back-reference (ERROR), as for any
  move. If the move would export a source member (a moved test type shares an
  unexported method name with a staying interface), it is refused.
- **References into the target:** `authz.X` in a moved in-package test
  becomes `X` and the import is dropped; an alias of `authz.X` becomes `X`.
  A local declaration that would shadow the bare name is an ERROR.
- **Collisions fail loudly.** A package-level name of a moved in-package test
  that the target already declares (in any file, whatever its build tags) is
  an ERROR, as is a moved external test's name that an existing external test
  declares, and an import name that collides with a package-level name on
  the other side. The one exception is an **equivalent duplicate** (below).
- **Helper reuse.** A func or const declared by an in-package test file of
  both packages is *equivalent* when both have the same build constraint and
  the same tokens once every identifier is replaced by what it denotes (type
  information on both sides; aliases of the source are seen through; helpers
  they use must be equivalent too; comments, layout and trailing commas are
  ignored; `iota` is never equivalent). Equivalence is decided in the one
  analysis build configuration, so a name that either package declares more
  than once (build-tag variants, such as `limit_unix_test.go` and
  `limit_other_test.go`) or in a file the analysis tags exclude is **never**
  equivalent: the other variants cannot be compared, and the error says so.
  Then:
  - a moved test may use a staying helper whose target equivalent exists (the
    plan lists it under "Staying helpers reused from the target");
  - a moved declaration with a target equivalent is dropped from the moved
    file, with any imports only it used ("Moved declarations dropped").

  Types and vars are never reused: a shared var would share state. To let
  several waves use a helper, copy it into the target first (a prep PR); the
  last wave then moves the original, which is dropped as a duplicate.
- **TestMain:** see [TestMain](#testmain).
- **Import cycles:** a moved in-package test that imports (directly, or
  through an alias target) a package that imports the target is an ERROR
  (an in-package test cannot import its own package's importers). Move it as
  an external test instead.
- The post-move type-check covers the target with all its in-package tests,
  old and new.

## `-rewrite-aliases`: shrinking alias files

`pkgmove -rewrite-aliases -from <dir> [-to <dir>]` moves nothing. In the source
package (every type-checked file, including in-package tests) and its external
tests, each reference to an [alias](#alias-references) becomes a direct
reference to its target, with imports added (and the source import dropped
from external tests that no longer use it). With `-to`, only aliases of that
package are rewritten.

Then unexported entries of generated alias files that nothing references any
more are deleted; a file left without entries is deleted (`git rm`).

**Kept** (each listed in the report):
- exported entries: importers may use them (remove them in a separate step,
  after rewriting the importers);
- hand-written aliases (in ordinary files): never removed;
- references that cannot be rewritten without a change: wrappers used as func
  values, and embedded fields whose name would change; their entries stay;
- names that a file excluded by build tags uses (matched by name, WARN): those
  files are not rewritten;
- entries used by other kept entries of an alias file (a wrapper signature
  that names an alias type), and entries that a `//go:linkname` anywhere in
  the module targets.

References inside generated alias files are never rewritten. The post-run
checks are `go list -test` and an in-process type-check of the package with
its in-package tests.

## Safety report

A pure move can still change behaviour, mainly through **initialisation
order**. Pass `-strict` to make the tool refuse any move that has a HIGH
finding. The moved package is initialised before the package that imports it.
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
| HIGH | `gob.Register` of a moved type anywhere in the source package (element types of pointers, slices, arrays, maps and chans included): the gob name embeds the package path, so encoded data and peers that use the old name break |
| HIGH | exported struct fields (only with `-allow-field-export`). **This includes embedded fields:** exporting a moved type `inner` as `Inner` renames every field that embeds it (`Outer.inner` becomes `Outer.Inner`), which changes `%+v`, encoding/json, gob, cmp, templates and reflection |
| HIGH | a staying var initialiser that calls a moved func or a method of a moved type. This includes calls inside immediately-invoked func literals and calls through staying helpers that reach moved code (a static call graph over the package). Moved package state is now initialised before every source initialiser; for example, a registry filled by a staying initialiser looks empty to a moved initialiser |
| HIGH | TestMain separation: moved tests leave a package that has a `TestMain` (see [TestMain](#testmain)), including into an existing target whose own `TestMain` differs |
| HIGH | source-scanning test does not cover the target: a test file of the source package (staying or moved) imports `go/parser` or `go/packages` and enumerates files (`os.ReadDir`, `filepath.Glob`, `WalkDir`, `os.Getwd`, `parser.ParseDir`, `packages.Load`, ...). Such guard tests silently stop scanning the moved files and still pass |
| WARN | a staying var initialiser that makes dynamic calls (through func values or interfaces), directly or through helpers, when the moved files have package-level state |
| WARN | each moved func or method whose value is taken, plus exported funcs (var aliases): `runtime.FuncForPC` names and panic traces show the new package path |
| WARN | a wrapper alias used as a func value in a moved file: the moved file now uses the target func, whose identity differs from staying uses of the wrapper |
| WARN | test-only move: the target's `TestMain` delegates to `-testmain-support` (check it does everything the source's does), or the source has no `TestMain` but the target does |
| WARN | a moved test with a string literal starting with `testdata/`, `./`, `../` or equal to `..`, a call to `os.Getwd` or a `find...Dir` helper, or a `testdata` directory left behind while tests move: the package directory changes, so list the files in the file set (to move them as assets) or adjust the paths |
| WARN | an interface method spec (named or anonymous) in a tag-excluded file that matches an unexported method of a moved type |
| WARN | a moved var initialiser that calls another package's functions (for example `os.Getenv` or `slog.Default`), unless the call is provably pure (see below). It now runs before all of the source package's initialisers, so state they set is no longer visible to it, and its own effects happen earlier |
| WARN | a moved var initialiser that reads another package's vars (`os.Stderr`, `http.DefaultClient`) |
| WARN | a staying var initialiser that reads moved vars |
| WARN | staying var initialisers with side-effecting calls, listed when a moved file has `init()` or a side-effecting initialiser |
| WARN | a method declared in a tag-excluded file whose name matches an unexported method of a moved type or interface: satisfaction under those tags cannot be checked |
| WARN | files excluded by build tags that reference moved names. They are aliased conservatively, by name, without type-checking |
| WARN | `gob.RegisterName` of a moved type: the wire name is fixed, but identity checks and `%T` change |
| WARN | a moved var initialiser that reads vars or funcs of other files |
| WARN | every moved named type: `%T`, reflect `Type.String`/`PkgPath`, gob names and messages that print type names change from `src.X` to `target.X` for **all** users, including staying files and importers |
| WARN | methods exported to new names: they may newly satisfy interfaces. Types in **other packages** that embed the moved type are not checked for shadowing or newly promoted members; the WARN says so |
| WARN | a renamed method's old or new name in a template string (`{{.Name}}`), in a `MethodByName`/`FieldByName` call, or in a non-Go file of the source directory or any of its subdirectories (for example `templates/*.tmpl`). Exported methods become visible to text/template, html/template, reflect and RPC-style dispatch |
| WARN | `debug.Stack` or `runtime.Stack` in moved files: captured stacks and panic traces show the new package path |
| WARN | `%T`, `reflect.TypeOf`, `gob.Register`, `runtime.Caller` or `FuncForPC` in moved files: type and function names now print as `target.X` |
| WARN | the package doc comment moving |
| WARN | `//go:generate` directives |
| WARN | function aliases declared as vars (recover, stack inspection, or a signature that cannot be spelled) |
| INFO | moved var initialisers whose calls are all provably pure (see below) |
| INFO | staying var initialisers that use only moved consts, types or func values (no moved code runs and no moved var is read at init) |
| INFO | moved external tests, and moved files with build constraints |
| INFO | files excluded by the build tags that do not reference moved names |
| INFO | test companions left behind |
| INFO | test-only move: the target's `TestMain` is equivalent to the source's |

`-rewrite-aliases` reports: WARN for files excluded by build tags that name
aliases (not rewritten), INFO for references kept (wrapper func values,
embedded fields), for entries kept (exported, hand-written, still referenced,
targeted by `//go:linkname`), and for forwarding-shaped declarations that are
not resolved (with the reason).

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
    `Error`, `MarshalJSON`, `Read`, `ServeHTTP`, `IsZero`, `Equal`, `Flush`,
    `Hijack`, `AppendText`, `MarshalJSONTo`, ...).
- A moved exported var is referenced from another package of the module,
  where it cannot be aliased. The scan includes files that build constraints
  exclude (`IgnoredGoFiles`, such as `//go:build integration`), matched by
  name.
- A moved file is excluded by the build tags, or uses cgo.
- A moved func has no body (assembly or `go:linkname` pull), or the source
  directory has `.s` or `.syso` files.
- A moved file embeds an alias whose target has another name (the field
  would be renamed).
- A pkgmove-generated alias file is in the move set, or a destination file
  already exists.
- Test-only moves into an existing package: a name collision that is not an
  equivalent duplicate, a needed rename of a source member, an import cycle,
  or a bare target name shadowed by a local.
- A `//go:linkname` anywhere in the module (test and build-excluded files
  included) targets `<source import path>.<moved name>`.
- `TestMain` itself is in the move set.
- A `go:embed` pattern matches files that are not in the move set.
- The target directory already has Go files and the move set has a non-test
  Go file (exit code 3), or an alias file already exists.

### What counts as a pure initialiser call

The tool is **default-deny**: anything it cannot prove safe statically is
reported at WARN or higher, never INFO.

A call in a var initialiser is pure only when both of these hold.

1. **The callee is allow-listed** as a package-level function:
   - `errors`, `regexp`, `strings`, `strconv`, `unicode`, `unicode/utf8`,
     `math`, `bytes`;
   - `fmt.Errorf`, `Sprintf`, `Sprint` and `Sprintln`;
   - `reflect.TypeOf` and `TypeFor`;
   - `time.Date`, `Unix`, `UnixMilli` and `UnixMicro`;
   - `path.Join`, `filepath.Join`, `slices.Clone` and `maps.Clone`.

   Methods never count.
2. **No argument can run package code:**
   - no func-typed argument (this excludes `strings.Map`, `FieldsFunc` and
     the like);
   - no non-empty interface argument;
   - no argument whose method set includes methods from non-standard
     packages;
   - for `fmt`, only unnamed basic-typed arguments, because fmt calls
     `String`, `Error` and `Format`.

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

## Failure handling

All file contents are computed before the tree is touched. Git mode also
refuses to start in two cases:
- a file to move (or to delete, with `-rewrite-aliases`) is untracked;
- a file the move edits (or deletes) has staged changes.

If any later step fails (`git mv`, a write, a delete, `git add`), everything
done so far is rolled back: moves, rewritten and deleted files, alias files,
staging, and the target directory if the run created it. The error says whether the rollback was
complete. Post-move sanity-check failures (`go list`, type-check, vet) leave the
generated tree in place for inspection; use `git checkout`/`git reset` to
discard it.

## TestMain

A package's `TestMain` wraps every test in its test binary. pkg/hub's
`TestMain` isolates HOME, clears ambient GCP env, installs the hermetic git
runners, and runs the memory guard and the leak guard. Tests that move run
in the target's test binary, which has no `TestMain` unless the move adds
one.

- **TestMain moves:** ERROR.
- **Moved tests leave a package with a TestMain, no flag:** HIGH (ERROR under
  `-strict`). The tool also writes a reference stub
  `zz_alias_<area>_safety_testmain.go.txt` next to the report; it is not
  compiled and not staged.
- **With `-testmain-support <import path>`:** the tool generates
  `<target>/zz_testmain_test.go` and stages it:

  ```go
  func TestMain(m *testing.M) { os.Exit(<support>.RunTestMain(m)) }
  ```

  The report then carries a WARN to check the harness is equivalent.

**Into an existing package** that already has a `TestMain`, nothing is
generated (a second `TestMain` would not compile). Instead:
- the target's `TestMain` is equivalent to the source's (same canonical
  tokens, as for [helper reuse](#test-only-moves-into-an-existing-package),
  **and** every test helper it calls, such as a `setup()`, equivalent too):
  INFO;
- it is the delegating form for `-testmain-support`: WARN, as above;
- otherwise: HIGH (ERROR under `-strict`). This includes **build-tag
  variants**: INFO or WARN need exactly one `TestMain` on each side, in files
  the analysis tags include, with the same build constraint. pkg/hub has two
  (`main_test.go` for `!integration`, `main_integration_test.go` for
  `integration`), so a test-only move out of it reports HIGH naming both; check
  each configuration;
- the source has no `TestMain` but the target does: WARN.

If the target has none, the rules above apply (stub, or generated with
`-testmain-support`; an existing `zz_testmain_test.go` is an ERROR). A
generated `TestMain` wraps the whole test binary, so the target's **existing**
tests, which ran without a `TestMain`, now run under it too; the WARN says so,
and the reviewer checks that those tests still behave the same under the
harness (HOME isolation, env clearing, leak and memory guards).

**Expected helper:** a small test-support package (for pkg/hub, created by the
first real move, for example under `pkg/hub/internal/`) exporting:

```go
// RunTestMain sets up the package test harness (for pkg/hub: IsolateHome,
// clearing the ambient GCP env, the hermetic git runners, the memory guard and
// the leak guard), runs m, tears down, and returns the exit code.
func RunTestMain(m *testing.M) int
```

The source package's own `TestMain` should call it too, so the logic exists
once.

## Not supported (rejected, or out of scope)

- Moving non-test code into a package that already has Go files (P3-1
  decision): source moves always create a new package. Test-only moves are
  supported.
- Rewriting importers of the source package in `-rewrite-aliases` (only the
  source package and its external tests are rewritten; exported entries are
  kept for the importers).
- Backward-only moves, where the target imports the source and nothing aliases
  back (for example, moving e2e tests out of `pkg/hub`). **This is a non-goal.**
- Analysing several build configurations in one run.
- cgo files.
- Type-checking moved external tests. They are rewritten syntactically, and
  `go list -test` checks their imports.
- Checking types in other packages that embed a moved type, for shadowing or
  newly promoted members after a member export. This is noted in the WARN.
- Typed `gob.Register` detection in importers. The unconditional
  type-name WARN covers them.

## Known limitations: reviewer checks

The tool is a generator, not a proof. Every generated move PR still gets its
own independent review, the identical test-list check (`go test -list`
before and after) and the area's tests. The tool cannot see the cases below.
Each is phrased as a check for the reviewer of a generated PR.

- **Cross-package embedding:** if a moved type has members that the move
  exports, verify that no type in another package embeds it and relies on
  member resolution (shadowing or newly promoted methods).
- **Build configurations:** if the source package has tag-excluded files (the
  report lists them), build and test the moved area under those tags too.
  They are scanned by name only.
- **Init order beyond static calls:** if a staying initialiser or `init()`
  reaches moved code through another package (a callback registered
  elsewhere, a plugin registry), verify the order still holds. The call graph
  follows static calls inside the source package only.
- **Reflection by computed name:** if code looks up methods or fields by
  names built at run time (not string literals), verify that exported or
  renamed members don't change the result.
- **Importers' `%T`, reflect and gob:** if importers print, compare or
  register moved types by name (`%T`, `reflect.Type.String`, `gob.Register`),
  check the new package path is acceptable. The report lists every moved type
  but scans only the source package for gob registration.
- **Typed analysis of external tests:** if moved external tests (`package
  x_test`) use the source package beyond plain selectors, run them; they are
  rewritten syntactically.
- **Source-scanning guard tests:** if the report lists "source-scanning test does not cover the target", extend each listed test to scan the target directory as well. For example, use the package directory from `go list`, or walk both directories. Then check that it still fails on a planted violation in a moved file. pkg/hub has dozens of these, such as the `*_resource_literal_guard_test.go`, `*_enumeration_test.go` and `*_callsite*_test.go` files.
- **Working-directory paths in moved tests:** if moved tests build paths from the working directory (`..`, `../`, `os.Getwd`, a `find...Dir` helper walking up to the module root), check that each path still resolves from the target directory, which is one level deeper. A test that skips when a file is missing passes silently.
- **Assets read at run time:** if moved code reads files relative to the
  working directory or the package directory (beyond `testdata/` and `./`
  literals), move those files too.
- **Other modules:** if modules outside this one (for example `extras/*`)
  import the source package, check them for moved vars, linkname and func
  values. Only this module is scanned.
- **TestMain equivalence:** if `-testmain-support` was used, verify that the
  helper does everything the source `TestMain` does.
- **Hand-written forwarding funcs and vars:** only type and const aliases are
  resolved wherever they are; wrappers and func vars only in generated alias
  files. If a back-reference error says a declaration "is not in a
  pkgmove-generated alias file", either move it into one (after checking the
  target does not call `recover()` or inspect its stack, and that the var is
  not a hook) or rewrite the reference by hand.
- **Wrapper func values:** if the report lists "func value through a wrapper
  alias resolved to its target", check that nothing compares that value with
  staying uses of the wrapper (`reflect` `Pointer`, `FuncForPC` names).
- **Var aliases and init order:** a resolved var alias is read directly as
  its target func. If code ran during the source package's initialisation
  before the var was set (only possible through an initialisation-order
  cycle the compiler cannot see, such as an interface call), it saw `nil`
  before and now sees the function. pkgmove-generated var aliases are
  initialised first by dependency order, so this needs a hand-made cycle.
- **Helper reuse is for in-package tests:** equivalence is decided with type
  information on both sides, so external tests (which pkgmove does not
  type-check) never reuse helpers; their collisions are errors. Moved external
  tests that use helpers declared in staying external tests are not detected
  (as for any move); run them.
- **Test-only moves and `init()`:** an `init()` in a moved test file is
  reported as HIGH as for any move. In a test-only move it runs in the
  target's test binary; check what it initialises.
- **Excluded files in `-rewrite-aliases`:** files excluded by build tags are
  not rewritten; rewrite the names the report lists by hand under those tags.

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
   once. For funcs, types and consts, the fix is one alias line or a
   `target.` qualifier. **Vars and funcs used as values always need the
   `target.` qualifier, never an alias line:** a var alias would be a copy,
   and a wrapper has a different identity.

## Tests

```sh
go test ./hack/pkgmove/...           # golden, determinism (6 runs per fixture), behaviour, dry-run, rollback and unit tests
go test ./hack/pkgmove/ -update      # rewrite the goldens after an intended change
```

Each fixture under `testdata/<case>/in` is a small module. A test copies it to
a temp dir, moves files from `hub/` to `hub/sub/` (which already holds a
package in the `intoexisting*` fixtures; `rewritealiases` runs
`-rewrite-aliases` instead), and compares the result with `want/` and
`stdout.golden`. Failing cases must leave the tree untouched. The alias files
in the `alias*`, `rewritealiases` and `intoexisting*` inputs were generated by
pkgmove itself from an earlier move.

`TestBehaviour` runs the tests of every successful fixture before and after the move:
- **Where the move changes behaviour:** the tests must fail afterwards, and
  the report must contain the finding that explains why.
- **Where the tool preserves behaviour:** the tests must still pass.

| Fixture | What it covers |
|---|---|
| `basic` | export renames, aliases, generics, var rewrite, test-only alias, init detection, recover and stack-inspection var aliases, template and `MethodByName` warnings |
| `tags` | build constraints |
| `xtest` | external tests, embed and linkname |
| `embed` | embedded-field rename refused without `-allow-field-export` |
| `embedallow` | the same move allowed: HIGH, and the fixture's `%+v` test fails after the move |
| `iface` | interface-group rename |
| `fields` | field export with `-allow-field-export` |
| `methods` | methods on a type that stays, rejected |
| `backref` | back-reference, rejected |
| `testsep` | test/helper separation, rejected |
| `collide` | rename collision and shadowing, rejected |
| `dynamic` | method exported to `String`, rejected |
| `cgo` | cgo file, rejected |
| `generic` | generic type that satisfies an interface dynamically through an unexported method |
| `initorder` | WARN for a moved initialiser that calls another package, INFO for pure constructors, staying side-effecting initialisers |
| `samename` | several types renaming the same method name (deterministic report) |
| `typenames` | type-name WARN, HIGH for `gob.Register` (including `[]T`), WARN for `gob.RegisterName` |
| `registry` | a staying initialiser that fills a moved registry is HIGH; the fixture's test fails after the move |
| `purity` | which initialiser calls count as pure |
| `funcvalue` | func values rewritten to the target, so func identity is kept; the fixture's test passes after the move |
| `excludedmethod` | a method in an integration-tagged file that matches a moved interface: WARN; the tagged test fails after the move |
| `excludediface` | an anonymous interface spec in an integration-tagged file that a moved type satisfied: WARN; the tagged test fails after the move |
| `iife`, `transitive` | a staying initialiser reaching moved code through an immediately-invoked func literal, or through a staying helper: HIGH; the test fails after the move |
| `importervalue` | an importer uses an exported moved func as a value: it is aliased as a var, so identity is kept and the importer's test passes |
| `funcname` | the FuncForPC name change of a func value: WARN; the test pinning the name fails after the move |
| `testmain` | moved tests leave a TestMain: HIGH plus a reference stub; the HOME-isolation test fails after the move |
| `testmainsupport` | the same move with `-testmain-support`: the generated TestMain keeps the test passing |
| `testdatadir` | a moved test reads `testdata/` (which stays) and `../go.mod`, and calls `os.Getwd`: WARN (the tests silently skip after the move) |
| `sourcescan` | a staying go/parser guard test enumerating the package with `os.ReadDir`: HIGH. Even its `STRICT_GUARD=1` file-count check still passes after the move, because the alias file takes the moved file's place: the coverage loss is silent |
| `asm` | a body-less func with an assembly file: refused |
| `linkname` | a linkname in another package targeting a moved var: refused |
| `aliasresolve` | a move after an earlier one (alias file generated by pkgmove): type, generic type, const, wrapper, generic wrapper, var-alias (recover and stack-inspecting funcs) references resolved; an existing import reused; a parameter shadowing the import name; a wrapper func value WARN. The moved tests (including the call-site check) pass |
| `aliasreject` | refused: a hand-written wrapper and a hand-written func var (with the reason in the error), a var alias assigned by a staying test and by another package, and an embedded alias with a different target name |
| `rewritealiases` | `-rewrite-aliases -to apierr` (git): staying and external-test references rewritten; unused unexported entries removed; exported, embedded, func-value and tag-excluded uses kept; a hand-written alias of another package untouched. The package's tests pass |
| `intoexisting` | a test wave into the package of an earlier source move (git): aliases to the target become bare names, `sub.X` becomes `X`, an alias of another package is qualified, a staying helper is reused, a duplicate helper is dropped with its import, the external test is re-qualified, and the target's TestMain is equivalent. The moved tests pass |
| `intoexistingtestmain` | the same wave into a target whose TestMain differs: HIGH, and the moved test (which needs the source harness) fails after the move |
| `intoexistingtestmaindeps` | the target's TestMain has the same text as the source's, but the `setup()` helper it calls differs: HIGH, and the moved test fails after the move |
| `intoexistingtestmaintags` | the source has `!integration` and `integration` TestMains, the target only the first: HIGH naming the variants; under `-tags integration` the moved test fails after the move |
| `intoexistinghelpertags` | refused: a staying helper with build-tag variants (`limit_unix_test.go`, `limit_other_test.go`) whose analysed variant matches the target's but whose other variant does not |
| `intoexistingreject` | refused: a test name collision, a staying helper whose target copy differs, import cycles (directly and through an alias target), and an alias whose bare target name a local shadows |
