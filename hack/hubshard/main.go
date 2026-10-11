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
// The default build (no tags) is unchanged: every test file compiles. With
// -tags hubshard,hubshard_K the test binary contains the pkg/hub sources,
// the common test files, and only shard K's test files. A sharded file
// carries the constraint
//
//	//go:build <existing constraint> && (!hubshard || hubshard_K)
//
// A test file is "common" (untagged by hubshard, so it compiles in every
// shard) when one of these makes it so:
//   - it is a seed: a helper-only *_helpers_test.go file, or a file with
//     TestMain or an init function that does more than discard values;
//   - closure: a common file uses a symbol it declares;
//   - size cut: its group is larger than total/-maxgroupdiv lines and it is
//     the group member with the best ratio of users to lines added to
//     common;
//   - prior assignment: it is common in the assignment file and another
//     test file still uses it.
//
// Files that reference each other but are not common are kept together in
// one group, and a group always lands in a single shard. Being used by
// another file does not by itself make a file common. See README.md for
// the analysis and its limits.
//
// The command is deterministic and idempotent. Shard membership is kept in
// a checked-in assignment file, so a file's shard does not move when
// unrelated files change. New groups go to the lightest shard (by lines).
// -rebalance recomputes every assignment from scratch.
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
)

const shardTag = "hubshard"

// maxGroupDiv bounds the size of a group of files that must share a shard:
// at most (total test lines)/maxGroupDiv.
var maxGroupDiv = 16

type method struct {
	recv string
	name string
}

type fileInfo struct {
	name        string // base name, e.g. agents_test.go
	lines       int
	decls       []string        // top-level non-method names (functions, types, vars, consts)
	methods     []method        // methods declared in this file
	idents      map[string]bool // unresolved identifiers (possible package-level references)
	selectors   map[string]bool // x.Name selector names (possible method references)
	hasInit     bool
	hasTestMain bool
	tests       int // number of top-level TestXxx/BenchmarkXxx/ExampleXxx/FuzzXxx funcs

	src        []byte
	buildLine  int // index of the //go:build line in lines(src), -1 if none
	constraint constraint.Expr
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
	root := flags.String("root", ".", "repository root, scanned for hubshard tags outside -dir")
	assignPath := flags.String("assign", "hack/hubshard/assignment.txt", "checked-in shard assignment file")
	n := flags.Int("n", 0, "number of shards (default: the value recorded in the assignment file)")
	rebalance := flags.Bool("rebalance", false, "ignore existing assignments and rebalance every group")
	check := flags.Bool("check", false, "do not write; exit non-zero if any file or the assignment file is out of date")
	verbose := flags.Bool("v", false, "list common files and groups")
	dump := flags.String("dumpgraph", "", "write the dependency graph to this file and exit")
	flags.IntVar(&maxGroupDiv, "maxgroupdiv", maxGroupDiv, "largest allowed group = total test lines / maxgroupdiv")
	if err := flags.Parse(args); err != nil {
		return err
	}

	stray, err := strayShardClauses(*root, *dir)
	if err != nil {
		return err
	}
	if len(stray) > 0 {
		return fmt.Errorf("hubshard build tags outside %s (remove the (!hubshard || hubshard_K) clause from each): %s", *dir, strings.Join(stray, ", "))
	}
	files, err := loadFiles(*dir)
	if err != nil {
		return err
	}
	prevN, prev, err := readAssignment(*assignPath)
	if err != nil {
		return err
	}
	shards := *n
	if shards == 0 {
		shards = prevN
	}
	if shards < 1 {
		return fmt.Errorf("number of shards unknown: pass -n (no %s yet)", *assignPath)
	}
	if shards != prevN {
		*rebalance = true
	}
	if *rebalance {
		prev = nil
	}
	an := analyze(files, prev)
	assign := assignShards(an, files, prev, shards)
	if *dump != "" {
		var b bytes.Buffer
		for _, n := range sortedNames(files) {
			fmt.Fprintf(&b, "N %s %d\n", n, files[n].lines)
			for to := range an.uses[n] {
				fmt.Fprintf(&b, "E %s %s\n", n, to)
			}
		}
		return os.WriteFile(*dump, b.Bytes(), 0o644)
	}

	if err := report(stdout, an, files, assign, shards, *verbose); err != nil {
		return err
	}

	var stale []string
	for _, name := range sortedNames(files) {
		f := files[name]
		k := assign[name] // 0 = common
		out, err := rewrite(f, k)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if bytes.Equal(out, f.src) {
			continue
		}
		stale = append(stale, name)
		if !*check {
			if err := os.WriteFile(filepath.Join(*dir, name), out, 0o644); err != nil {
				return err
			}
		}
	}
	assignOut := formatAssignment(shards, assign, an.common)
	oldAssign, _ := os.ReadFile(*assignPath)
	if !bytes.Equal(oldAssign, assignOut) {
		stale = append(stale, *assignPath)
		if !*check {
			if err := os.WriteFile(*assignPath, assignOut, 0o644); err != nil {
				return err
			}
		}
	}
	if *check {
		if len(stale) > 0 {
			return fmt.Errorf("%d file(s) out of date, re-run go run ./hack/hubshard: %s", len(stale), strings.Join(stale, ", "))
		}
		_, err = fmt.Fprintln(stdout, "up to date")
		return err
	}
	_, err = fmt.Fprintf(stdout, "rewrote %d file(s)\n", len(stale))
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
	fset := token.NewFileSet()
	for _, p := range matches {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		af, err := parser.ParseFile(fset, p, src, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		f := &fileInfo{name: filepath.Base(p), src: src, idents: map[string]bool{}, selectors: map[string]bool{}}
		f.lines = bytes.Count(src, []byte("\n"))
		if err := parseBuildLine(f); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		collect(f, af)
		files[f.name] = f
	}
	return files, nil
}

// collect records the file's top-level declarations and the identifiers it
// mentions. The identifier set over-approximates the file's references to
// package-level symbols (it also contains locals, fields, and so on), which
// only makes more files common, never fewer.
func collect(f *fileInfo, af *ast.File) {
	imports := map[string]bool{}
	for _, is := range af.Imports {
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
				if isTestFunc(name) {
					f.tests++
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

func isTestFunc(name string) bool {
	for _, p := range []string{"Test", "Benchmark", "Example", "Fuzz"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
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
	}
	return nil
}

// headerBuildLine returns the index and the expression of the //go:build
// line in the file header (-1, nil when there is none). The header is
// everything before the first line that is not blank or a comment; /* */
// blocks (e.g. a license) are skipped.
func headerBuildLine(src []byte) (int, constraint.Expr, error) {
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	inBlock := false
	for i := 0; sc.Scan(); i++ {
		line := strings.TrimSpace(sc.Text())
		if inBlock {
			inBlock = !strings.Contains(line, "*/")
			continue
		}
		switch {
		case strings.HasPrefix(line, "/*"):
			inBlock = !strings.Contains(line, "*/")
			continue
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

// strayShardClauses lists the .go files under root, other than dir's own
// files (its subdirectories are scanned), whose build line mentions a
// hubshard tag. A test file moved out of dir (for
// example by hack/pkgmove) keeps its (!hubshard || hubshard_K) clause, and
// a -tags hubshard,hubshard_J build would then drop it from its package.
// Hidden directories, node_modules and vendor are skipped.
func strayShardClauses(root, dir string) ([]string, error) {
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
		if parent, err := filepath.Abs(filepath.Dir(p)); err == nil && parent == absDir {
			return nil // dir's own files are this tool's to tag
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
		return []byte("//go:build " + want.String() + "\n\n" + string(f.src)), nil
	}
	old := strings.TrimRight(lines[f.buildLine], "\r\n")
	if !strings.Contains(old, shardTag) && k == 0 {
		return f.src, nil
	}
	if want == nil {
		// The build line held only a hubshard clause, which this tool
		// added together with one blank line after it: remove both.
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

// ---------------------------------------------------------------------------
// Dependency graph, common set and groups

type analysis struct {
	uses       map[string]map[string]bool // file -> files it needs
	common     map[string]bool
	why        map[string]string // common file -> reason
	groups     [][]string        // sorted groups of shardable files, sorted by first member
	directUsed int               // files referenced by at least one other file (the simple rule)
}

// analyze builds the file dependency graph and picks the common set. prev is
// the previous assignment (shard 0 = common): a file that was common stays
// common while another test file still uses it, so the common set does not
// shift when unrelated files are added.
func analyze(files map[string]*fileInfo, prev map[string]int) *analysis {
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
		case usedBy[n] > 0 && prev != nil && prev[n] == 0 && hasKey(prev, n):
			markCommon(n, fmt.Sprintf("common in assignment file, used by %d files", usedBy[n]))
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

func assignShards(an *analysis, files map[string]*fileInfo, prev map[string]int, shards int) map[string]int {
	assign := map[string]int{}
	load := make([]int, shards+1)
	type pending struct {
		members []string
		lines   int
	}
	var todo []pending
	for _, g := range an.groups {
		lines := 0
		votes := map[int]int{}
		for _, n := range g {
			lines += files[n].lines
			if k, ok := prev[n]; ok && k >= 1 && k <= shards {
				votes[k] += files[n].lines
			}
		}
		if len(votes) == 0 {
			todo = append(todo, pending{g, lines})
			continue
		}
		best := 0
		for k := 1; k <= shards; k++ {
			if votes[k] > votes[best] || (best == 0 && votes[k] > 0) {
				best = k
			}
		}
		for _, n := range g {
			assign[n] = best
		}
		load[best] += lines
	}
	// Largest new groups first onto the lightest shard (ties: lowest K).
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

func readAssignment(p string) (int, map[string]int, error) {
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	n := 0
	m := map[string]int{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# shards:") {
			n, err = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "# shards:")))
			if err != nil {
				return 0, nil, fmt.Errorf("%s:%d: %w", p, i+1, err)
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return 0, nil, fmt.Errorf("%s:%d: want <shard> <file>", p, i+1)
		}
		k, err := strconv.Atoi(fields[0])
		if err != nil {
			return 0, nil, fmt.Errorf("%s:%d: %w", p, i+1, err)
		}
		m[fields[1]] = k
	}
	return n, m, nil
}

func hasKey(m map[string]int, k string) bool {
	_, ok := m[k]
	return ok
}

func formatAssignment(shards int, assign map[string]int, common map[string]bool) []byte {
	var b bytes.Buffer
	b.WriteString("# Generated by go run ./hack/hubshard; re-run it after editing.\n")
	b.WriteString("# <shard> <file>. Shard 0 = common: the file compiles in every shard. A file\n")
	b.WriteString("# not listed (new) is placed by the generator.\n")
	fmt.Fprintf(&b, "# shards: %d\n", shards)
	names := make([]string, 0, len(assign))
	for n, k := range assign {
		if k > 0 {
			names = append(names, n)
		}
	}
	for n := range common {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "%d %s\n", assign[n], n)
	}
	return b.Bytes()
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
	for _, n := range names {
		f := files[n]
		totalLines += f.lines
		if an.common[n] {
			commonFiles++
			commonLines += f.lines
			commonTests += f.tests
			continue
		}
		shardLines[assign[n]] += f.lines
		shardFiles[assign[n]]++
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
		fmt.Fprintf(&b, "shard %d: %d files, %d lines\n", k, shardFiles[k], shardLines[k])
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
