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
| `-strict` | off | treat every HIGH finding as an error (see the [Safety report](#safety-report) table) |
| `-testmain-support` | none | import path of a test-support package exporting `RunTestMain(m *testing.M) int`; when moved tests leave a package that has a `TestMain`, generates a delegating `TestMain` in the target (see [TestMain](#testmain)) |
| `-no-git` | off | use `os.Rename` instead of `git mv` / `git add` |
| `-report` | `<from>/zz_alias_<area>_safety.txt` | where the safety report is written |

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
| HIGH | TestMain separation: moved tests leave a package that has a `TestMain` (see [TestMain](#testmain)) |
| HIGH | source-scanning test does not cover the target: a test file of the source package (staying or moved) imports `go/parser` or `go/packages` and enumerates files (`os.ReadDir`, `filepath.Glob`, `WalkDir`, `os.Getwd`, `parser.ParseDir`, `packages.Load`, ...). Such guard tests silently stop scanning the moved files and still pass. A file with a [`pkgmove:scan-covers`](#declaring-scan-coverage-pkgmovescan-covers) marker that covers the target gets the INFO below instead |
| WARN | a staying var initialiser that makes dynamic calls (through func values or interfaces), directly or through helpers, when the moved files have package-level state |
| WARN | each moved func or method whose value is taken, plus exported funcs (var aliases): `runtime.FuncForPC` names and panic traces show the new package path |
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
| INFO | source-scanning test declares coverage of the target: a source-scanning test file that carries a `pkgmove:scan-covers` marker covering the target. The line names the marker's file:line and echoes its reason line, so the reviewer can verify each one |

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
- A `//go:linkname` anywhere in the module (test and build-excluded files
  included) targets `<source import path>.<moved name>`.
- `TestMain` itself is in the move set.
- A `go:embed` pattern matches files that are not in the move set.
- The target directory already has Go files, or an alias file already exists.

### Declaring scan coverage (`pkgmove:scan-covers`)

The source-scanning check is syntactic: it cannot tell whether a guard test
already scans the target directory. A test file declares that it does with a
marker, a whole comment line followed by a comment line that gives the reason:

```go
	// pkgmove:scan-covers pkg/hub/apierr
	// recursive walk already includes pkg/hub/apierr (walks the whole module).
	err := filepath.WalkDir(root, func(...) error { ... })
```

- **Format:** the marker line is exactly `// pkgmove:scan-covers <dir>`, alone
  on its line (indentation is fine), with one directory. A marker after code
  on the same line, with extra words, or with a malformed directory covers
  nothing.
- **Reason line (required):** the next comment line says why the claim holds,
  for example "recursive walk already includes ..." or "unaffected: guards X,
  which the moved files do not contain". The tool does not interpret it but
  echoes it in the report; the reviewer rejects a marker without one.
- **Directory:** relative to the module root, slash-separated (`./` prefix
  and trailing `/` are allowed). `pkg/hub/...` covers `pkg/hub` and every
  directory below it, `./...` (or `...`) the whole module. Use a `/...`
  marker only for a test that really walks recursively. Absolute paths, `..`
  elements and other glob characters (`*`, `?`, `[`) are malformed.
- **Placement:** anywhere in the file; put it next to the directory
  enumeration it describes. A file may carry several markers (one per line),
  for example one per move it has been checked for.

For each source-scanning test file, the HIGH is cleared only if one of its
markers names the move's target directory, or a parent of it with `/...`.
The report then lists the file as INFO "source-scanning test declares
coverage of <target> (marker at file:line; reason: ...)". Otherwise the HIGH
stays (and fails `-strict`); when the file has markers for other
directories, the HIGH lists them.

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
- a file to move is untracked;
- a file the move edits has staged changes.

If any later step fails (`git mv`, a write, `git add`), everything done so far
is rolled back: moves, rewritten files, alias files, staging, and the target
directory if the run created it. The error says whether the rollback was
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

- Moving into a package that already has Go files: **planned, decision at
  P3-1**. Each move creates a new package. Moving a second batch into an
  existing package would also need references through the first batch's
  aliases to be rewritten. This is not needed if the P3-1 design gives the
  authz sub-moves sibling subpackages.
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
- **`pkgmove:scan-covers` markers:** for each scan-covers marker listed as INFO, verify the test really enumerates the target dir (or that its subject cannot appear in the moved files), and that the reason line says which. The marker is a claim, not a proof: the tool only checks that the named directory covers the target.
- **Working-directory paths in moved tests:** if moved tests build paths from the working directory (`..`, `../`, `os.Getwd`, a `find...Dir` helper walking up to the module root), check that each path still resolves from the target directory, which is one level deeper. A test that skips when a file is missing passes silently.
- **Assets read at run time:** if moved code reads files relative to the
  working directory or the package directory (beyond `testdata/` and `./`
  literals), move those files too.
- **Other modules:** if modules outside this one (for example `extras/*`)
  import the source package, check them for moved vars, linkname and func
  values. Only this module is scanned.
- **TestMain equivalence:** if `-testmain-support` was used, verify that the
  helper does everything the source `TestMain` does.

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
a temp dir, moves files from `hub/` to `hub/sub/`, and compares the result with
`want/` and `stdout.golden`. Failing cases must leave the tree untouched.

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
