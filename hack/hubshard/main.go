// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command hubshard splits the pkg/hub test files into build-tag shards, so
// that one shard's test binary compiles with a fraction of the memory the
// full pkg/hub test binary needs.
//
// The tags are never committed. CI (and make test-hub-shard) runs
//
//	go run ./hack/hubshard -overlay DIR
//
// which writes the tagged copies of the test files and the generated
// imports files to DIR, plus DIR/overlay.json, and then compiles with
//
//	go test -overlay DIR/overlay.json -tags hubshard,hubshard_K ./pkg/hub
//
// The working tree is never modified, so the default build (no tags) is
// the plain tree: every test file compiles. In a shard build the test
// binary contains the pkg/hub sources, the common test files, and only
// shard K's test files. A sharded file carries the constraint
//
//	//go:build <existing constraint> && (!hubshard || hubshard_K)
//
// A test file is "common" (untagged by hubshard, so it compiles in every
// shard) when one of these makes it so:
//   - it is a seed: a helper-only *_helpers_test.go file, or a file with
//     TestMain or an init function that does more than discard values;
//   - closure: a common file uses a symbol it declares;
//   - size cut: its group is larger than total/-maxgroupdiv lines, it is
//     used by at least two group members, and among those candidates it has
//     the best ratio of users to lines added to common.
//
// Files that reference each other but are not common are kept together in
// one group, and a group always lands in a single shard. Being used by
// another file does not by itself make a file common. See README.md for
// the analysis and its limits.
//
// Blank imports (driver registrations and other init side effects) have no
// symbol edge, so the overlay also holds untagged
// zz_hubshard_drivers*_test.go files that blank-import every package a test
// file blank-imports (see imports.go).
//
// The assignment is computed from the tree on every run: it is
// deterministic (the same tree always gives the same shards) and there is
// no checked-in state. Groups are placed largest first onto the lightest
// shard (by lines). -parity checks that every test runs in exactly one
// shard (see parity.go).
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const shardTag = "hubshard"

// defaultShards is the -n default. The CI matrix (.github/workflows/ci.yml,
// job hub-shard-plan), the make targets and the nightly loops take the
// shard list from -shards, so this is the one place the count lives.
const defaultShards = 6

// defaultMaxGroupDiv is the -maxgroupdiv default.
const defaultMaxGroupDiv = 16

// maxGroupDiv bounds the size of a group of files that must share a shard:
// at most (total test lines)/maxGroupDiv. Every run sets it from
// -maxgroupdiv, so one run's flag does not leak into the next (tests call
// run repeatedly).
var maxGroupDiv = defaultMaxGroupDiv

type method struct {
	recv string
	name string
}

type fileInfo struct {
	name         string // base name, e.g. agents_test.go
	lines        int
	decls        []string        // top-level non-method names (functions, types, vars, consts)
	methods      []method        // methods declared in this file
	idents       map[string]bool // unresolved identifiers (possible package-level references)
	selectors    map[string]bool // x.Name selector names (possible method references)
	hasInit      bool            // an init function that does more than discard values
	hasTestMain  bool
	blankImports []string // paths imported as _ (for their init side effects)
	pkgName      string
	tests        []string // top-level functions go test lists: TestXxx, BenchmarkXxx, FuzzXxx, ExampleXxx with output

	src        []byte
	buildLine  int             // index of the //go:build line in lines(src), -1 if none
	constraint constraint.Expr // the build constraint without the hubshard clause
	nameCond   constraint.Expr // the GOOS/GOARCH constraint of the file name (foo_linux_test.go), nil if none
	diskShard  int             // K of the hubshard clause in src, 0 if none, -1 if K is not a number
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "hubshard:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("hubshard", flag.ContinueOnError)
	dir := flags.String("dir", "pkg/hub", "package directory whose _test.go files are sharded")
	root := flags.String("root", ".", "repository root, scanned for hubshard tags (none may be committed)")
	n := flags.Int("n", defaultShards, "number of shards")
	tagList := flags.String("tags", "", "comma-separated build tags for -list, -skip and -locate, besides hubshard,hubshard_K (for example no_sqlite or integration)")
	overlay := flags.String("overlay", "", "write the tagged test files, the generated imports files and overlay.json (for go build -overlay) to this directory; the tree is not modified")
	parity := flags.Bool("parity", false, "check that every test runs in exactly one shard and that every shard build compiles (see parity.go); writes nothing")
	list := flags.Int("list", 0, "print the tests (one per line, sorted) that shard K's test binary compiles under -tags")
	skip := flags.Int("skip", 0, "print the go test -skip pattern for shard K under -tags: the anchored names of the common files' tests, which every shard compiles and only shard 1 runs (empty for shard 1)")
	locate := flags.Bool("locate", false, "for each test name argument print \"<name> <K>\": the shard that runs it under -tags (a common file's tests run in shard 1); fail if a name is in no shard")
	shards := flags.Bool("shards", false, "print the shard numbers 1..N (one per line), for scripts that loop over every shard")
	untag := flags.Bool("untag", false, "remove every hubshard clause and generated imports file from -dir in place (for files tagged by an older generator)")
	verbose := flags.Bool("v", false, "list common files and groups")
	dump := flags.String("dumpgraph", "", "write the dependency graph to this file and exit")
	flags.IntVar(&maxGroupDiv, "maxgroupdiv", defaultMaxGroupDiv, "largest allowed group = total test lines / maxgroupdiv")
	if err := flags.Parse(args); err != nil {
		return err
	}
	modes := 0
	for _, on := range []bool{*overlay != "", *parity, *list != 0, *skip != 0, *locate, *shards, *untag, *dump != ""} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		return fmt.Errorf("-overlay, -parity, -list, -skip, -locate, -shards, -untag and -dumpgraph are exclusive")
	}
	if *locate != (flags.NArg() > 0) {
		return fmt.Errorf("test name arguments go with -locate, and -locate needs at least one")
	}
	if *n < 1 {
		return fmt.Errorf("-n must be at least 1")
	}
	for name, k := range map[string]int{"-list": *list, "-skip": *skip} {
		if k < 0 || k > *n {
			return fmt.Errorf("%s %d: want 1 <= K <= %d", name, k, *n)
		}
	}
	tags := splitTags(*tagList)
	for _, t := range tags {
		if t == shardTag || strings.HasPrefix(t, shardTag+"_") {
			return fmt.Errorf("-tags %s: the hubshard tags are added per shard", t)
		}
	}

	if *shards {
		var ks []string
		for k := 1; k <= *n; k++ {
			ks = append(ks, strconv.Itoa(k))
		}
		return printLines(stdout, ks)
	}
	if *untag {
		return runUntag(*root, *dir, stdout)
	}

	stray, err := strayShardClauses(*root, *dir, true)
	if err != nil {
		return err
	}
	files, err := loadFiles(*dir)
	if err != nil {
		return err
	}
	gen := splitGenerated(files)
	for _, name := range sortedNames(gen) {
		stray = append(stray, filepath.Join(*dir, name))
	}
	if len(stray) > 0 {
		return fmt.Errorf("the tree must not carry hubshard tags or generated imports files: the tool generates them on every run (see hack/hubshard/README.md). Fix: go run ./hack/hubshard -untag for %s; elsewhere delete the (!hubshard || hubshard_K) clause from the //go:build line by hand. Found: %s", *dir, strings.Join(stray, ", "))
	}
	if len(files) == 0 {
		return fmt.Errorf("no _test.go files in %s other than generated ones", *dir)
	}
	an := analyze(files)
	assign := assignShards(an, files, *n)
	if *dump != "" {
		var b bytes.Buffer
		for _, name := range sortedNames(files) {
			fmt.Fprintf(&b, "N %s %d\n", name, files[name].lines)
			for to := range an.uses[name] {
				fmt.Fprintf(&b, "E %s %s\n", name, to)
			}
		}
		return os.WriteFile(*dump, b.Bytes(), 0o644)
	}
	sh, err := newSharding(*dir, files, an, assign, *n)
	if err != nil {
		return err
	}

	switch {
	case *list != 0:
		names, err := sh.shardTests(*list, tags)
		if err != nil {
			return err
		}
		return printLines(stdout, names)
	case *skip != 0:
		pattern, err := sh.skipPattern(*skip, tags)
		if err != nil {
			return err
		}
		return printLines(stdout, []string{pattern})
	case *locate:
		return sh.locate(stdout, tags, flags.Args())
	}

	if err := report(stdout, an, files, assign, *n, *verbose); err != nil {
		return err
	}
	switch {
	case *overlay != "":
		return sh.writeOverlay(*overlay, stdout)
	case *parity:
		return sh.parity(stdout)
	}
	return nil
}

// runUntag removes the hubshard clauses and generated imports files that
// an older generator wrote into dir, restoring each file's own build line.
// Tags outside dir are reported, not touched.
func runUntag(root, dir string, stdout io.Writer) error {
	stray, err := strayShardClauses(root, dir, false)
	if err != nil {
		return err
	}
	if len(stray) > 0 {
		return fmt.Errorf("hubshard build tags outside %s; fix by hand: delete the (!hubshard || hubshard_K) clause from the //go:build line of: %s", dir, strings.Join(stray, ", "))
	}
	files, err := loadFiles(dir)
	if err != nil {
		return err
	}
	gen := splitGenerated(files)
	changed := 0
	for _, name := range sortedNames(files) {
		f := files[name]
		if f.constraint != nil && mentionsShardTag(f.constraint) {
			return fmt.Errorf("%s: hubshard tag outside a (!hubshard || hubshard_K) clause; fix by hand", name)
		}
		out, err := rewrite(f, 0)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if bytes.Equal(out, f.src) {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), out, 0o644); err != nil {
			return err
		}
		changed++
	}
	for _, name := range sortedNames(gen) {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
		changed++
	}
	_, err = fmt.Fprintf(stdout, "untagged %d file(s)\n", changed)
	return err
}

func splitTags(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
}

func printLines(w io.Writer, lines []string) error {
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	_, err := w.Write(b.Bytes())
	return err
}

// ---------------------------------------------------------------------------
// Loading and per-file analysis

func loadFiles(dir string) (map[string]*fileInfo, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no _test.go files in %s", dir)
	}
	files := map[string]*fileInfo{}
	for _, p := range matches {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		f, err := parseFile(p, src)
		if err != nil {
			return nil, err
		}
		files[f.name] = f
	}
	return files, nil
}

// parseFile analyzes one test file (p is its path, for messages and the
// base name).
func parseFile(p string, src []byte) (*fileInfo, error) {
	af, err := parser.ParseFile(token.NewFileSet(), p, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	f := &fileInfo{name: filepath.Base(p), src: src, idents: map[string]bool{}, selectors: map[string]bool{}}
	f.lines = bytes.Count(src, []byte("\n"))
	if err := parseBuildLine(f); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	f.nameCond = fileNameConstraint(f.name)
	collect(f, af)
	return f, nil
}

// splitGenerated removes the generated imports files (see imports.go) from
// files and returns them.
func splitGenerated(files map[string]*fileInfo) map[string]*fileInfo {
	gen := map[string]*fileInfo{}
	for n, f := range files {
		if isImportsFile(n) {
			gen[n] = f
			delete(files, n)
		}
	}
	return gen
}

// collect records the file's top-level declarations and the identifiers it
// mentions. The identifier set over-approximates the file's references to
// package-level symbols (it also contains locals, fields, and so on), which
// only makes more files common, never fewer.
func collect(f *fileInfo, af *ast.File) {
	f.pkgName = af.Name.Name
	imports := map[string]bool{}
	for _, is := range af.Imports {
		if is.Name != nil && is.Name.Name == "_" {
			p, _ := strconv.Unquote(is.Path.Value)
			f.blankImports = append(f.blankImports, p)
			continue
		}
		if is.Name != nil {
			imports[is.Name.Name] = true
			continue
		}
		p, _ := strconv.Unquote(is.Path.Value)
		imports[guessImportName(p)] = true
	}

	for _, d := range af.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) > 0 {
				f.methods = append(f.methods, method{recv: recvTypeName(d.Recv.List[0].Type), name: d.Name.Name})
				continue
			}
			switch name := d.Name.Name; name {
			case "init":
				if !inertInit(d) {
					f.hasInit = true
				}
			case "TestMain":
				f.hasTestMain = true
			case "_":
			default:
				f.decls = append(f.decls, name)
				if isListedTest(d, af) {
					f.tests = append(f.tests, name)
				}
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					f.decls = append(f.decls, s.Name.Name)
				case *ast.ValueSpec:
					for _, id := range s.Names {
						if id.Name != "_" {
							f.decls = append(f.decls, id.Name)
						}
					}
				}
			}
		}
	}

	ast.Inspect(af, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.SelectorExpr:
			// pkg.Name refers to another package: skip both parts.
			if x, ok := n.X.(*ast.Ident); ok && x.Obj == nil && imports[x.Name] {
				return false
			}
			// x.Name: Name is a field or method. It is recorded (in
			// f.selectors) because a test file may declare a method on a
			// pkg/hub type that other test files call.
			ast.Inspect(n.X, func(m ast.Node) bool { return visitIdent(f, m) })
			f.selectors[n.Sel.Name] = true
			return false
		}
		return visitIdent(f, n)
	})
}

// inertInit reports whether an init function only discards values
// (`_ = x` with no calls), so it has no effect on other files' tests.
func inertInit(d *ast.FuncDecl) bool {
	for _, st := range d.Body.List {
		as, ok := st.(*ast.AssignStmt)
		if !ok || as.Tok != token.ASSIGN {
			return false
		}
		for _, l := range as.Lhs {
			if id, ok := l.(*ast.Ident); !ok || id.Name != "_" {
				return false
			}
		}
		calls := false
		for _, r := range as.Rhs {
			ast.Inspect(r, func(n ast.Node) bool {
				if _, ok := n.(*ast.CallExpr); ok {
					calls = true
				}
				return !calls
			})
		}
		if calls {
			return false
		}
	}
	return true
}

// visitIdent records identifiers the parser could not resolve inside the
// file. Those are the only ones that can refer to a package-level symbol in
// another file (locals and same-file declarations resolve). Struct literal
// keys are also unresolved and are kept: over-approximation is safe.
func visitIdent(f *fileInfo, n ast.Node) bool {
	if id, ok := n.(*ast.Ident); ok && id.Obj == nil {
		f.idents[id.Name] = true
	}
	return true
}

var majorVersion = regexp.MustCompile(`^v[0-9]+$`)

// guessImportName returns the usual local name of an import path. A wrong
// guess only means a pkg.Name selector is kept as a possible reference,
// which is conservative.
func guessImportName(p string) string {
	base := path.Base(p)
	if majorVersion.MatchString(base) && path.Dir(p) != "." {
		base = path.Base(path.Dir(p))
	}
	base = strings.TrimPrefix(base, "go-")
	if i := strings.Index(base, "."); i >= 0 {
		base = base[:i]
	}
	return strings.ReplaceAll(base, "-", "_")
}

func recvTypeName(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

// isListedTest reports whether go test lists the top-level function d of
// file af (go test -list, and the binary's -test.list): TestXxx,
// BenchmarkXxx and FuzzXxx, where Xxx does not start with a lower-case
// letter, and ExampleXxx with an output comment (go test compiles an
// example without one but does not run it). TestMain is not a test.
func isListedTest(d *ast.FuncDecl, af *ast.File) bool {
	name := d.Name.Name
	if d.Recv != nil || d.Type.TypeParams != nil || name == "TestMain" {
		return false
	}
	for _, p := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if !strings.HasPrefix(name, p) {
			continue
		}
		if rest := name[len(p):]; rest != "" {
			if r, _ := utf8.DecodeRuneInString(rest); unicode.IsLower(r) {
				return false
			}
		}
		if p == "Example" {
			return hasOutputComment(d, af)
		}
		return true
	}
	return false
}

var outputComment = regexp.MustCompile(`(?i)^[[:space:]]*(unordered )?output:`)

// hasOutputComment reports whether the last comment in the example's body
// is an output comment, as go test requires (go/doc.Examples).
func hasOutputComment(d *ast.FuncDecl, af *ast.File) bool {
	if d.Body == nil {
		return false
	}
	var last *ast.CommentGroup
	for _, cg := range af.Comments {
		if cg.Pos() > d.Body.Lbrace && cg.End() < d.Body.Rbrace {
			last = cg
		}
	}
	return last != nil && outputComment.MatchString(last.Text())
}

// ---------------------------------------------------------------------------
// Build constraints

// parseBuildLine finds the //go:build line in the file header and strips any
// hubshard clause from it, leaving the file's own constraint.
func parseBuildLine(f *fileInfo) error {
	line, e, err := headerBuildLine(f.src)
	if err != nil {
		return err
	}
	f.buildLine = line
	if e != nil {
		f.constraint = stripShard(e)
		f.diskShard = shardOf(e)
	}
	return nil
}

// shardOf returns K of the file's hubshard clause: 0 if the build line has
// no top-level (!hubshard || hubshard_K) conjunct, -1 if the clause is not
// one the generator writes (more than one clause, or a tag other than
// hubshard_K for a decimal K >= 1 without leading zeros), so that the build
// may put the file in no shard.
func shardOf(e constraint.Expr) int {
	var clauses []constraint.Expr
	var walk func(constraint.Expr)
	walk = func(e constraint.Expr) {
		if isShardExpr(e) {
			clauses = append(clauses, e)
			return
		}
		if a, ok := e.(*constraint.AndExpr); ok {
			walk(a.X)
			walk(a.Y)
		}
	}
	walk(e)
	switch len(clauses) {
	case 0:
		return 0
	case 1:
	default:
		return -1
	}
	tag := clauses[0].(*constraint.OrExpr).Y.(*constraint.TagExpr).Tag
	k, err := strconv.Atoi(strings.TrimPrefix(tag, shardTag+"_"))
	if err != nil || k < 1 || tag != fmt.Sprintf("%s_%d", shardTag, k) {
		return -1
	}
	return k
}

// headerBuildLine returns the index and the expression of the //go:build
// line in the file header (-1, nil when there is none). The header is
// everything before the first line holding something other than blanks
// and comments; /* */ blocks (e.g. a license) are skipped, and code after a
// closing */ on the same line ends the header.
func headerBuildLine(src []byte) (int, constraint.Expr, error) {
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	inBlock := false
	for i := 0; sc.Scan(); i++ {
		line := strings.TrimSpace(sc.Text())
		if inBlock || strings.HasPrefix(line, "/*") {
			var rest string
			rest, inBlock = skipBlockComments(line, inBlock)
			if inBlock || rest == "" || strings.HasPrefix(rest, "//") {
				continue // still comments only
			}
			break // code after a closing */ ends the header
		}
		switch {
		case constraint.IsPlusBuild(line):
			return -1, nil, fmt.Errorf("// +build lines are not supported")
		case constraint.IsGoBuild(line):
			e, err := constraint.Parse(line)
			if err != nil {
				return -1, nil, err
			}
			return i, e, nil
		case line == "" || strings.HasPrefix(line, "//"):
			continue
		}
		break
	}
	return -1, nil, sc.Err()
}

// skipBlockComments removes the leading /* */ comments from line (which
// starts inside a block when inBlock) and returns what follows them,
// trimmed, and whether a block is still open at the end of the line.
func skipBlockComments(line string, inBlock bool) (string, bool) {
	for {
		if inBlock {
			end := strings.Index(line, "*/")
			if end < 0 {
				return "", true
			}
			line = line[end+2:]
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "/*") {
			return line, false
		}
		line, inBlock = line[2:], true
	}
}

// strayShardClauses lists the .go files under root whose build line
// mentions a hubshard tag; dir's own files only when includeDir (its
// subdirectories are always scanned). No file may carry one: the tags exist
// only in the overlay, and a committed clause (for example on a test file
// moved out of dir by hack/pkgmove, or left by an older generator) would
// drop the file from a -tags hubshard,hubshard_J build of its package.
// Hidden directories, node_modules and vendor are skipped.
func strayShardClauses(root, dir string, includeDir bool) ([]string, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	var stray []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if p != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		if parent, err := filepath.Abs(filepath.Dir(p)); err == nil && parent == absDir && !includeDir {
			return nil // -untag handles dir's own files
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		// A file whose build line does not parse is not this tool's concern.
		if _, e, perr := headerBuildLine(src); perr == nil && e != nil && mentionsShardTag(e) {
			stray = append(stray, p)
		}
		return nil
	})
	return stray, err
}

// mentionsShardTag reports whether e uses the hubshard tag or a
// hubshard_K tag.
func mentionsShardTag(e constraint.Expr) bool {
	switch e := e.(type) {
	case *constraint.TagExpr:
		return e.Tag == shardTag || strings.HasPrefix(e.Tag, shardTag+"_")
	case *constraint.NotExpr:
		return mentionsShardTag(e.X)
	case *constraint.AndExpr:
		return mentionsShardTag(e.X) || mentionsShardTag(e.Y)
	case *constraint.OrExpr:
		return mentionsShardTag(e.X) || mentionsShardTag(e.Y)
	}
	return false
}

// stripShard removes a top-level (!hubshard || hubshard_K) conjunct.
func stripShard(e constraint.Expr) constraint.Expr {
	if isShardExpr(e) {
		return nil
	}
	if a, ok := e.(*constraint.AndExpr); ok {
		x, y := stripShard(a.X), stripShard(a.Y)
		switch {
		case x == nil:
			return y
		case y == nil:
			return x
		default:
			return &constraint.AndExpr{X: x, Y: y}
		}
	}
	return e
}

func isShardExpr(e constraint.Expr) bool {
	o, ok := e.(*constraint.OrExpr)
	if !ok {
		return false
	}
	n, ok := o.X.(*constraint.NotExpr)
	if !ok {
		return false
	}
	t, ok := n.X.(*constraint.TagExpr)
	if !ok || t.Tag != shardTag {
		return false
	}
	k, ok := o.Y.(*constraint.TagExpr)
	return ok && strings.HasPrefix(k.Tag, shardTag+"_")
}

func shardExpr(k int) constraint.Expr {
	return &constraint.OrExpr{
		X: &constraint.NotExpr{X: &constraint.TagExpr{Tag: shardTag}},
		Y: &constraint.TagExpr{Tag: fmt.Sprintf("%s_%d", shardTag, k)},
	}
}

// rewrite returns the file contents with the hubshard clause for shard k
// (0 = common, no clause). A file whose build line has no hubshard clause
// and that stays common is returned unchanged byte for byte.
func rewrite(f *fileInfo, k int) ([]byte, error) {
	lines := strings.SplitAfter(string(f.src), "\n")
	var want constraint.Expr
	switch {
	case k == 0:
		want = f.constraint
	case f.constraint == nil:
		want = shardExpr(k)
	default:
		want = &constraint.AndExpr{X: f.constraint, Y: shardExpr(k)}
	}

	if f.buildLine < 0 {
		if want == nil {
			return f.src, nil
		}
		newLine := "//go:build " + want.String()
		if i := spareCommentLine(lines); i >= 0 {
			// Keep the line count, so that line numbers in a shard's test
			// output match the source.
			lines[i] = newLine + "\n"
			return []byte(strings.Join(lines, "")), nil
		}
		return []byte(newLine + "\n\n" + string(f.src)), nil
	}
	old := strings.TrimRight(lines[f.buildLine], "\r\n")
	if !strings.Contains(old, shardTag) && k == 0 {
		return f.src, nil
	}
	if want == nil {
		// The build line held only a hubshard clause. If it sits between
		// two // lines, it replaced an empty // line (spareCommentLine):
		// put that back. Otherwise the generator added it together with
		// one blank line after it: remove both.
		if i := f.buildLine; i > 0 && i+1 < len(lines) && isLineComment(lines[i-1]) && isLineComment(lines[i+1]) {
			lines[i] = "//\n"
			return []byte(strings.Join(lines, "")), nil
		}
		rest := lines[f.buildLine+1:]
		if len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
			rest = rest[1:]
		}
		return []byte(strings.Join(lines[:f.buildLine], "") + strings.Join(rest, "")), nil
	}
	newLine := "//go:build " + want.String()
	if newLine == old {
		return f.src, nil
	}
	lines[f.buildLine] = newLine + "\n"
	return []byte(strings.Join(lines, "")), nil
}

func isLineComment(l string) bool {
	return strings.HasPrefix(strings.TrimSpace(l), "//")
}

// spareCommentLine returns the index of an empty "//" line in the file
// header that a //go:build line can replace, or -1.
//
// Placement rules, from the parsers themselves (Go 1.26):
//   - go/build parseFileHeader (src/go/build/build.go) and the copy cmd/go
//     uses for go test and go vet (cmd/go/internal/imports/build.go
//     parseFileHeader) take a //go:build line from anywhere among the
//     leading // comments, before the first line that is not blank or a //
//     comment. The "end" position they track (the last blank line before
//     that line) only trims the header for the old // +build lines; it
//     does not limit //go:build. A blank line right after the //go:build
//     line is not required ("should be followed by a blank line" in go help
//     buildconstraint is advice, to keep it out of package docs).
//   - vet's buildtag analyzer reports a //go:build line as misplaced only
//     after the package clause or inside a /* */ comment.
//
// So the empty // line of a license header can carry the clause, and the
// file keeps its line count (line numbers in a shard's test output match
// the source). To stay out of the package doc comment, the candidate must
// come before the last blank line of the header. A header with a /* */
// comment gets no candidate (the caller prepends a line instead).
// -parity (tagPlacement) checks every rewritten file with go/build, and
// the shard jobs vet it, so a placement the parsers ignored fails loudly.
func spareCommentLine(lines []string) int {
	cand, lastBlank := -1, -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case t == "":
			lastBlank = i
		case t == "//":
			if cand < 0 {
				cand = i
			}
		case strings.HasPrefix(t, "/*"):
			return -1
		case strings.HasPrefix(t, "//"):
		default:
			if cand >= 0 && cand < lastBlank {
				return cand
			}
			return -1
		}
	}
	return -1
}

// breakage lists what makes a -tags hubshard,hubshard_K build fail to
// compile or silently drop tests, judged from the tags in the generated
// files (files, re-parsed from the overlay contents) against the computed
// assignment (assign, with shards shards; 0 = common):
//   - a file uses a file of another shard (an untagged file is in every
//     shard, so it may only use untagged files);
//   - a hubshard tag that is not a valid (!hubshard || hubshard_K) clause
//     with 1 <= K <= shards (the file would be in no shard);
//   - a file whose tag disagrees with the assignment.
//
// importBreakage (imports.go) adds the blank imports of tagged files that
// no untagged file covers. The use graph is the generator's (conservative,
// syntactic) one. The generator never produces any of these; -parity runs
// the check to prove it.
func breakage(an *analysis, files map[string]*fileInfo, assign map[string]int, shards int) []string {
	var problems []string
	names := sortedNames(files)
	for _, n := range names {
		f := files[n]
		switch k := assign[n]; {
		case f.constraint != nil && mentionsShardTag(f.constraint):
			problems = append(problems, fmt.Sprintf("%s: hubshard tag outside a (!hubshard || hubshard_K) clause", n))
		case f.diskShard < 0, f.diskShard > shards:
			problems = append(problems, fmt.Sprintf("%s: hubshard tag is not (!hubshard || hubshard_K) with 1 <= K <= %d, so no shard compiles it", n, shards))
		case k != f.diskShard:
			problems = append(problems, fmt.Sprintf("%s: tagged for shard %d, assigned to %d (0 = common)", n, f.diskShard, k))
		}
	}
	for _, n := range names {
		from := files[n].diskShard
		for _, to := range sortedKeys(an.uses[n]) {
			k := files[to].diskShard
			if k == 0 || k == from {
				continue
			}
			where := fmt.Sprintf("shard %d", from)
			if from == 0 {
				where = fmt.Sprintf("every shard except %d (%s is untagged, so every shard compiles it)", k, n)
			}
			problems = append(problems, fmt.Sprintf("%s uses %s (shard %d): breaks %s", n, to, k, where))
		}
	}
	return problems
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// Dependency graph, common set and groups

type analysis struct {
	uses       map[string]map[string]bool // file -> files it needs
	common     map[string]bool
	why        map[string]string // common file -> reason
	groups     [][]string        // sorted groups of shardable files, sorted by first member
	directUsed int               // files referenced by at least one other file (the simple rule)
}

// analyze builds the file dependency graph and picks the common set.
func analyze(files map[string]*fileInfo) *analysis {
	names := sortedNames(files)
	declaredIn := map[string][]string{} // symbol -> files
	for _, n := range names {
		for _, d := range files[n].decls {
			declaredIn[d] = append(declaredIn[d], n)
		}
	}

	uses := map[string]map[string]bool{}
	addUse := func(from, to string) {
		if from == to {
			return
		}
		if uses[from] == nil {
			uses[from] = map[string]bool{}
		}
		uses[from][to] = true
	}

	// Methods: a method on a type declared in another test file ties the
	// two files together. A method on a type that no test file declares
	// (a pkg/hub type) is treated as a symbol referenced by name.
	looseMethods := map[string][]string{}
	for _, n := range names {
		own := map[string]bool{}
		for _, d := range files[n].decls {
			own[d] = true
		}
		for _, m := range files[n].methods {
			if own[m.recv] {
				continue
			}
			if decl := declaredIn[m.recv]; len(decl) > 0 {
				for _, other := range decl {
					addUse(n, other)
					addUse(other, n)
				}
				continue
			}
			looseMethods[m.name] = append(looseMethods[m.name], n)
		}
	}

	for _, n := range names {
		for id := range files[n].idents {
			for _, other := range declaredIn[id] {
				addUse(n, other)
			}
		}
		for sel := range files[n].selectors {
			for _, other := range looseMethods[sel] {
				addUse(n, other)
			}
		}
	}

	an := &analysis{uses: uses, common: map[string]bool{}, why: map[string]string{}}
	usedBy := map[string]int{}
	for _, n := range names {
		for to := range uses[n] {
			usedBy[to]++
		}
	}
	for _, n := range names {
		if usedBy[n] > 0 {
			an.directUsed++
		}
	}

	markCommon := func(n, why string) {
		if !an.common[n] {
			an.common[n] = true
			an.why[n] = why
		}
	}
	for _, n := range names {
		switch {
		case isHelperFile(n):
			markCommon(n, "helper file")
		case files[n].hasTestMain:
			markCommon(n, "TestMain")
		case files[n].hasInit:
			markCommon(n, "init")
		}
	}

	total := 0
	for _, n := range names {
		total += files[n].lines
	}
	// A group may not exceed total/maxGroupDiv lines. While the largest
	// group is too big, move one of its files (plus everything that file
	// needs) to the common set. The file chosen has the best ratio of
	// "group members that use it" to "lines added to common".
	maxGroup := total / maxGroupDiv
	for {
		closeCommon(an, names)
		groups := components(an, names)
		big, bigLines := []string(nil), 0
		for _, g := range groups {
			l := 0
			for _, n := range g {
				l += files[n].lines
			}
			if l > bigLines {
				big, bigLines = g, l
			}
		}
		if bigLines <= maxGroup || len(big) == 1 {
			an.groups = groups
			return an
		}
		inBig := map[string]bool{}
		for _, n := range big {
			inBig[n] = true
		}
		best, bestScore := "", 0.0
		for _, n := range big {
			u := 0
			for _, m := range big {
				if uses[m][n] {
					u++
				}
			}
			if u < 2 {
				continue
			}
			score := float64(u) / float64(closureLines(an, files, n))
			if score > bestScore {
				best, bestScore = n, score
			}
		}
		if best == "" {
			an.groups = groups
			return an
		}
		markCommon(best, fmt.Sprintf("used by %d files", usedBy[best]))
	}
}

// isHelperFile reports whether name is a helper-only test file
// (*_helpers_test.go, see hack/hubshard/extract). Helper files hold the
// declarations other test files share, so they are always common: they
// stay untagged and compile in every shard.
func isHelperFile(name string) bool {
	return strings.HasSuffix(name, "_helpers_test.go")
}

// closureLines returns the lines of n and of every non-common file n needs,
// directly or indirectly: the cost of making n common.
func closureLines(an *analysis, files map[string]*fileInfo, n string) int {
	seen := map[string]bool{n: true}
	stack := []string{n}
	total := 0
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		total += files[x].lines
		for y := range an.uses[x] {
			if !seen[y] && !an.common[y] {
				seen[y] = true
				stack = append(stack, y)
			}
		}
	}
	return total
}

// closeCommon makes every file needed by a common file common too.
func closeCommon(an *analysis, names []string) {
	for changed := true; changed; {
		changed = false
		for _, n := range names {
			if !an.common[n] {
				continue
			}
			for to := range an.uses[n] {
				if !an.common[to] {
					an.common[to] = true
					an.why[to] = "needed by common " + n
					changed = true
				}
			}
		}
	}
}

// components returns the connected components of the use graph restricted
// to non-common files.
func components(an *analysis, names []string) [][]string {
	parent := map[string]string{}
	find := func(x string) string {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	for _, n := range names {
		if !an.common[n] {
			parent[n] = n
		}
	}
	for _, n := range names {
		if an.common[n] {
			continue
		}
		for to := range an.uses[n] {
			if an.common[to] {
				continue
			}
			a, b := find(n), find(to)
			if a != b {
				if a < b {
					parent[b] = a
				} else {
					parent[a] = b
				}
			}
		}
	}
	byRoot := map[string][]string{}
	for _, n := range names {
		if !an.common[n] {
			r := find(n)
			byRoot[r] = append(byRoot[r], n)
		}
	}
	var groups [][]string
	for _, g := range byRoot {
		sort.Strings(g)
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i][0] < groups[j][0] })
	return groups
}

// ---------------------------------------------------------------------------
// Shard assignment

// assignShards places the groups, largest first, onto the lightest shard by
// lines (ties: lowest K; groups of equal size: by first member name). The
// result depends only on the files, so every run on the same tree gives
// the same shards.
func assignShards(an *analysis, files map[string]*fileInfo, shards int) map[string]int {
	assign := map[string]int{}
	load := make([]int, shards+1)
	type pending struct {
		members []string
		lines   int
	}
	var todo []pending
	for _, g := range an.groups {
		lines := 0
		for _, n := range g {
			lines += files[n].lines
		}
		todo = append(todo, pending{g, lines})
	}
	sort.SliceStable(todo, func(i, j int) bool {
		if todo[i].lines != todo[j].lines {
			return todo[i].lines > todo[j].lines
		}
		return todo[i].members[0] < todo[j].members[0]
	})
	for _, p := range todo {
		k := 1
		for j := 2; j <= shards; j++ {
			if load[j] < load[k] {
				k = j
			}
		}
		for _, n := range p.members {
			assign[n] = k
		}
		load[k] += p.lines
	}
	return assign
}

// ---------------------------------------------------------------------------
// Reporting

// report writes the summary (and with verbose the common files and groups)
// to w.
func report(w io.Writer, an *analysis, files map[string]*fileInfo, assign map[string]int, shards int, verbose bool) error {
	var b bytes.Buffer
	names := sortedNames(files)
	var totalLines, commonLines, commonFiles, commonTests int
	shardLines := make([]int, shards+1)
	shardFiles := make([]int, shards+1)
	shardTests := make([]int, shards+1)
	for _, n := range names {
		f := files[n]
		totalLines += f.lines
		if an.common[n] {
			commonFiles++
			commonLines += f.lines
			commonTests += len(f.tests)
			continue
		}
		shardLines[assign[n]] += f.lines
		shardFiles[assign[n]]++
		shardTests[assign[n]] += len(f.tests)
	}
	largest := 0
	for _, g := range an.groups {
		l := 0
		for _, n := range g {
			l += files[n].lines
		}
		if l > largest {
			largest = l
		}
	}
	fmt.Fprintf(&b, "test files: %d (%d lines)\n", len(names), totalLines)
	fmt.Fprintf(&b, "referenced by another test file (simple rule): %d files\n", an.directUsed)
	fmt.Fprintf(&b, "common: %d files, %d lines (%.1f%%), %d test funcs\n",
		commonFiles, commonLines, 100*float64(commonLines)/float64(totalLines), commonTests)
	fmt.Fprintf(&b, "groups: %d (largest %d lines)\n", len(an.groups), largest)
	for k := 1; k <= shards; k++ {
		fmt.Fprintf(&b, "shard %d: %d files, %d lines, %d test funcs (plus the common files)\n", k, shardFiles[k], shardLines[k], shardTests[k])
	}
	if verbose {
		reportVerbose(&b, an, files, assign, names)
	}
	_, err := w.Write(b.Bytes())
	return err
}

func reportVerbose(b *bytes.Buffer, an *analysis, files map[string]*fileInfo, assign map[string]int, names []string) {
	fmt.Fprintln(b, "\ncommon files:")
	for _, n := range names {
		if an.common[n] {
			fmt.Fprintf(b, "  %-60s %6d  %s\n", n, files[n].lines, an.why[n])
		}
	}
	fmt.Fprintln(b, "\nmulti-file groups:")
	for _, g := range an.groups {
		if len(g) > 1 {
			fmt.Fprintf(b, "  shard %d: %s\n", assign[g[0]], strings.Join(g, " "))
		}
	}
}

func sortedNames(files map[string]*fileInfo) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
