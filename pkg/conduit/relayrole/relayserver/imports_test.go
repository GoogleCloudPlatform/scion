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

package relayserver

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The relay's code paths must not reach the hub's state: no relay path
// imports pkg/store, pkg/ent or pkg/hub, and the relay process reaches the
// database only through pkg/store/relaydb (the registry store, the
// allowlisted settings reader and the event LISTEN source), which only
// cmd/server_relay.go imports. The check parses sources with go/parser
// (every non-test .go file, whatever its build tags) and needs no build.

// forbiddenRelayImport matches the module-relative paths no relay path may
// import.
var forbiddenRelayImport = regexp.MustCompile(`^pkg/(store|ent|hub)(/|$)`)

// dbDriverImport matches the external database driver and client paths no
// relay path may import (relaydb excepted).
var dbDriverImport = regexp.MustCompile(`^(database/sql(/|$)|github\.com/jackc/pgx|github\.com/lib/pq(/|$)|entgo\.io/|modernc\.org/sqlite(/|$)|github\.com/mattn/go-sqlite3(/|$)|github\.com/glebarez/(go-)?sqlite(/|$)|github\.com/ncruces/go-sqlite3(/|$))`)

// relayDBPath is the relay's database opener.
const relayDBPath = "pkg/store/relaydb"

// relayCmdFile is the relay process's composition root, the only file
// allowed to import relayDBPath.
const relayCmdFile = "cmd/server_relay.go"

// relayRoots are the relay packages whose module-local imports are walked.
var relayRoots = []string{"pkg/conduit/relayrole", "pkg/conduit/relayrole/relayserver", "pkg/conduit/relay"}

// relayDBImports are the only module-local packages relaydb may import:
// the registry store adapter, the one ent entity it reads (hub settings),
// the settings document parser and the experiment registry.
var relayDBImports = map[string]bool{
	"pkg/conduit/registry":  true,
	"pkg/conduit/relayrole": true,
	"pkg/config/opsettings": true,
	"pkg/ent":               true,
	"pkg/ent/entc":          true,
	"pkg/ent/hubsetting":    true,
	"pkg/experiments":       true,
	"pkg/store/entadapter":  true,
}

// relayDBSelectors are the only selectors relaydb may use on the database
// packages: the conduit registry store, the hub settings query on the
// experiments section, and ent's not-found test.
var relayDBSelectors = map[string]bool{
	"entadapter.NewConduitRegistryStore": true,
	"hubsetting.SectionEQ":               true,
	"ent.IsNotFound":                     true,
	"ent.Client":                         true,
	"entc.OpenPostgres":                  true,
	"entc.OpenSQLite":                    true,
	"entc.PoolConfig":                    true,
}

// importScan is a module's import graph, read from source.
type importScan struct {
	root   string              // module root directory
	module string              // module path
	cache  map[string][]string // module-local imports
	ext    map[string][]string // imports outside the module
}

func newImportScan(t *testing.T, root string) *importScan {
	t.Helper()
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if mod, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return &importScan{root: root, module: strings.TrimSpace(mod), cache: map[string][]string{}, ext: map[string][]string{}}
		}
	}
	t.Fatal("go.mod has no module line")
	return nil
}

// fileImports returns the import paths of one source file.
func fileImports(path string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(f.Imports))
	for _, im := range f.Imports {
		p, err := strconv.Unquote(im.Path.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// local returns the module-relative path of an import, or false when it
// is outside the module.
func (s *importScan) local(imp string) (string, bool) {
	rel, ok := strings.CutPrefix(imp, s.module+"/")
	return rel, ok
}

// pkgImports returns the module-local imports of the non-test files of the
// package in directory rel.
func (s *importScan) pkgImports(t *testing.T, rel string) []string {
	t.Helper()
	if v, ok := s.cache[rel]; ok {
		return v
	}
	entries, err := os.ReadDir(filepath.Join(s.root, rel))
	if err != nil {
		t.Fatalf("package %s: %v", rel, err)
	}
	seen, external := map[string]bool{}, map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		imps, err := fileImports(filepath.Join(s.root, rel, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range imps {
			if l, ok := s.local(imp); ok {
				seen[l] = true
			} else {
				external[imp] = true
			}
		}
	}
	for imp := range external {
		s.ext[rel] = append(s.ext[rel], imp)
	}
	sort.Strings(s.ext[rel])
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	s.cache[rel] = out
	return out
}

// forbiddenReach walks the module-local imports from roots and returns, for
// every forbidden package reached and every database driver imported on
// the way, the import chain that reaches it.
func (s *importScan) forbiddenReach(t *testing.T, roots ...string) []string {
	t.Helper()
	parent := map[string]string{}
	queue := []string{}
	for _, r := range roots {
		if _, ok := parent[r]; !ok {
			parent[r] = ""
			queue = append(queue, r)
		}
	}
	var bad []string
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if forbiddenRelayImport.MatchString(p) {
			chain := []string{p}
			for q := parent[p]; q != ""; q = parent[q] {
				chain = append(chain, q)
			}
			bad = append(bad, strings.Join(chain, " <- "))
			continue
		}
		for _, q := range s.pkgImports(t, p) {
			if _, ok := parent[q]; !ok {
				parent[q] = p
				queue = append(queue, q)
			}
		}
		for _, imp := range s.ext[p] {
			if dbDriverImport.MatchString(imp) {
				chain := []string{imp, p}
				for q := parent[p]; q != ""; q = parent[q] {
					chain = append(chain, q)
				}
				bad = append(bad, strings.Join(chain, " <- "))
			}
		}
	}
	sort.Strings(bad)
	return bad
}

// cmdViolations checks the relay composition root: of the database and hub
// packages it may import only relaydb, and nothing else it imports reaches
// them.
func (s *importScan) cmdViolations(t *testing.T) []string {
	t.Helper()
	imps, err := fileImports(filepath.Join(s.root, relayCmdFile))
	if err != nil {
		t.Fatal(err)
	}
	var bad, roots []string
	for _, imp := range imps {
		l, ok := s.local(imp)
		switch {
		case !ok:
			if dbDriverImport.MatchString(imp) {
				bad = append(bad, relayCmdFile+" imports "+imp)
			}
		case l == relayDBPath:
		case forbiddenRelayImport.MatchString(l):
			bad = append(bad, relayCmdFile+" imports "+l)
		default:
			roots = append(roots, l)
		}
	}
	for _, chain := range s.forbiddenReach(t, roots...) {
		bad = append(bad, relayCmdFile+": "+chain)
	}
	return bad
}

// relayDBImporters returns every non-test file in the module, other than
// relaydb's own, that imports relaydb.
func (s *importScan) relayDBImporters(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(s.root, path)
		rel = filepath.ToSlash(rel)
		if filepath.ToSlash(filepath.Dir(rel)) == relayDBPath {
			return nil
		}
		imps, err := fileImports(path)
		if err != nil {
			return nil // not a parseable Go file; the build reports it
		}
		for _, imp := range imps {
			if l, ok := s.local(imp); ok && l == relayDBPath {
				out = append(out, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// relayDBViolations checks what relaydb reaches in the database: only the
// allowed imports and selectors, and only the experiments settings row.
func (s *importScan) relayDBViolations(t *testing.T) []string {
	t.Helper()
	entFields := s.entClientFields(t)
	dir := filepath.Join(s.root, relayDBPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		// Every HubSetting use is the exact experiments query chain.
		var settingSels []*ast.SelectorExpr
		inQuery := map[*ast.SelectorExpr]bool{}
		names := map[string]string{} // local name -> module-relative path
		for _, im := range f.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			l, ok := s.local(p)
			if !ok {
				continue
			}
			if !relayDBImports[l] {
				bad = append(bad, fmt.Sprintf("%s imports %s", e.Name(), l))
			}
			name := filepath.Base(l)
			if im.Name != nil {
				name = im.Name.Name
			}
			names[name] = l
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				// A LISTEN names ConduitChannel and nothing else.
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Exec" && len(n.Args) >= 2 && listensOn(n.Args[1]) != "" && listensOn(n.Args[1]) != "ConduitChannel" {
					bad = append(bad, fmt.Sprintf("%s LISTENs on %s (only ConduitChannel)", e.Name(), listensOn(n.Args[1])))
				}
				// The settings query names only the experiments section.
				if hs := experimentsQuery(n, names); hs != nil {
					inQuery[hs] = true
				}
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "SectionEQ" {
					if len(n.Args) != 1 || !isIdent(n.Args[0], "experimentsSection") {
						bad = append(bad, fmt.Sprintf("%s queries a settings section other than experimentsSection", e.Name()))
					}
				}
			case *ast.ValueSpec:
				for i, name := range n.Names {
					if name.Name == "ConduitChannel" && (i >= len(n.Values) || !isString(n.Values[i], "scion_ev_conduit")) {
						bad = append(bad, fmt.Sprintf("%s: ConduitChannel is not \"scion_ev_conduit\"", e.Name()))
					}
				}
				for i, name := range n.Names {
					if name.Name == "experimentsSection" && (i >= len(n.Values) || !isString(n.Values[i], "experiments")) {
						bad = append(bad, fmt.Sprintf("%s: experimentsSection is not \"experiments\"", e.Name()))
					}
				}
			case *ast.SelectorExpr:
				// HubSetting is the only ent.Client field used, whatever
				// the receiver.
				switch {
				case n.Sel.Name == "HubSetting":
					settingSels = append(settingSels, n)
				case entFields[n.Sel.Name]:
					bad = append(bad, fmt.Sprintf("%s uses the ent.Client field %s", e.Name(), n.Sel.Name))
				}
				id, ok := n.X.(*ast.Ident)
				if !ok {
					return true
				}
				if l, ok := names[id.Name]; ok && strings.HasPrefix(l, "pkg/") && (strings.HasPrefix(l, "pkg/ent") || strings.HasPrefix(l, "pkg/store")) {
					if !relayDBSelectors[id.Name+"."+n.Sel.Name] {
						bad = append(bad, fmt.Sprintf("%s uses %s.%s", e.Name(), id.Name, n.Sel.Name))
					}
				}
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					v, _ := strconv.Unquote(n.Value)
					// The relay LISTENs on the conduit channel only.
					if strings.Contains(v, "scion_ev_") && v != "scion_ev_conduit" {
						bad = append(bad, fmt.Sprintf("%s names the event channel %q (only scion_ev_conduit)", e.Name(), v))
					}
					low := strings.ToLower(v)
					for _, w := range []string{"token", "secret", "credential", "api_key", "apikey", "signing"} {
						if strings.Contains(low, w) {
							bad = append(bad, fmt.Sprintf("%s mentions %q", e.Name(), v))
						}
					}
				}
			}
			return true
		})
		for _, sel := range settingSels {
			if !inQuery[sel] {
				bad = append(bad, fmt.Sprintf("%s uses HubSetting outside X.HubSetting.Query().Where(hubsetting.SectionEQ(experimentsSection))", e.Name()))
			}
		}
	}
	return bad
}

// entClientFields returns the field names of ent.Client (pkg/ent/client.go):
// one entity client per field, plus Schema.
func (s *importScan) entClientFields(t *testing.T) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(s.root, "pkg/ent/client.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Client" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, fld := range st.Fields.List {
			for _, name := range fld.Names {
				out[name.Name] = true
			}
		}
		return false
	})
	if len(out) == 0 {
		t.Fatal("no ent.Client fields found")
	}
	return out
}

// experimentsQuery returns the HubSetting selector of call when call is
// exactly X.HubSetting.Query().Where(hubsetting.SectionEQ(experimentsSection)),
// else nil.
func experimentsQuery(call *ast.CallExpr, names map[string]string) *ast.SelectorExpr {
	where, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || where.Sel.Name != "Where" || len(call.Args) != 1 {
		return nil
	}
	queryCall, ok := where.X.(*ast.CallExpr)
	if !ok || len(queryCall.Args) != 0 {
		return nil
	}
	query, ok := queryCall.Fun.(*ast.SelectorExpr)
	if !ok || query.Sel.Name != "Query" {
		return nil
	}
	hs, ok := query.X.(*ast.SelectorExpr)
	if !ok || hs.Sel.Name != "HubSetting" {
		return nil
	}
	filter, ok := call.Args[0].(*ast.CallExpr)
	if !ok || len(filter.Args) != 1 || !isIdent(filter.Args[0], "experimentsSection") {
		return nil
	}
	eq, ok := filter.Fun.(*ast.SelectorExpr)
	if !ok || eq.Sel.Name != "SectionEQ" {
		return nil
	}
	pkg, ok := eq.X.(*ast.Ident)
	if !ok || names[pkg.Name] != "pkg/ent/hubsetting" {
		return nil
	}
	return hs
}

// listensOn returns what a LISTEN statement expression names: "" when e is
// not a LISTEN, "ConduitChannel" when it is "LISTEN " plus an expression
// using only that identifier, and the expression's text otherwise.
func listensOn(e ast.Expr) string {
	if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		v, _ := strconv.Unquote(lit.Value)
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(v)), "LISTEN") {
			return v
		}
		return ""
	}
	bin, ok := e.(*ast.BinaryExpr)
	if !ok || bin.Op != token.ADD {
		return ""
	}
	lit, ok := bin.X.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	v, _ := strconv.Unquote(lit.Value)
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(v)), "LISTEN") {
		return ""
	}
	idents := map[string]bool{}
	ast.Inspect(bin.Y, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			idents[id.Name] = true
		}
		return true
	})
	if idents["ConduitChannel"] {
		for name := range idents {
			if name != "ConduitChannel" && name != "pgx" && name != "Identifier" && name != "Sanitize" {
				return "an expression using " + name
			}
		}
		return "ConduitChannel"
	}
	return "a channel other than ConduitChannel"
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func isString(e ast.Expr, want string) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	return err == nil && v == want
}

// relayForbiddenCalls are registry operations only the hub performs: a
// relay never deletes a principal's epoch row.
var relayForbiddenCalls = map[string]bool{"ForgetPrincipalEpoch": true, "DeletePrincipalEpoch": true}

// forbiddenCalls returns every selector naming a relayForbiddenCalls
// operation in the non-test files of the packages in dirs.
func (s *importScan) forbiddenCalls(t *testing.T, dirs ...string) []string {
	t.Helper()
	var bad []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(filepath.Join(s.root, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(s.root, dir, e.Name()), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && relayForbiddenCalls[sel.Sel.Name] {
					bad = append(bad, fmt.Sprintf("%s/%s uses %s", dir, e.Name(), sel.Sel.Name))
				}
				return true
			})
		}
	}
	return bad
}

// moduleRoot finds the module root above the test's directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		up := filepath.Dir(dir)
		if up == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = up
	}
}

// TestRelayCodeUsesOnlyRegistryStore: the relay packages and the relay
// composition root reach no hub state, and the relay's database access is
// relaydb only, imported only by cmd/server_relay.go and limited to the
// registry store, the experiments settings row and the event LISTEN.
func TestRelayCodeUsesOnlyRegistryStore(t *testing.T) {
	s := newImportScan(t, moduleRoot(t))
	for _, chain := range s.forbiddenReach(t, relayRoots...) {
		t.Errorf("a relay package reaches hub state: %s", chain)
	}
	for _, v := range s.cmdViolations(t) {
		t.Errorf("relay composition root: %s", v)
	}
	if got := s.relayDBImporters(t); len(got) != 1 || got[0] != relayCmdFile {
		t.Errorf("%s is imported by %v, want only %s", relayDBPath, got, relayCmdFile)
	}
	for _, v := range s.relayDBViolations(t) {
		t.Errorf("relay database access: %s", v)
	}
	for _, v := range s.forbiddenCalls(t, relayRoots...) {
		t.Errorf("a relay package deletes epoch rows: %s", v)
	}
}

// writeModule writes a module of source files (module-relative path ->
// content) for the checker's own tests.
func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files["go.mod"] = "module example.com/m\n"
	for rel, src := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestRelayImportCheckCatchesViolations: the checker reports each kind of
// violation, including through a build-tagged file and a transitive import.
func TestRelayImportCheckCatchesViolations(t *testing.T) {
	clean := map[string]string{
		"pkg/conduit/relayrole/a.go":             "package relayrole\nimport _ \"example.com/m/pkg/conduit/relay\"\n",
		"pkg/conduit/relayrole/relayserver/b.go": "package relayserver\nimport _ \"example.com/m/pkg/conduit/relayrole\"\n",
		"pkg/conduit/relay/c.go":                 "package relay\n",
		"cmd/server_relay.go":                    "package cmd\nimport (\n_ \"example.com/m/pkg/store/relaydb\"\n_ \"example.com/m/pkg/conduit/relayrole/relayserver\"\n)\n",
		"pkg/store/relaydb/d.go":                 "package relaydb\nimport _ \"example.com/m/pkg/store/entadapter\"\nfunc f() { _ = entadapter.NewConduitRegistryStore }\n",
		"pkg/ent/client.go":                      "package ent\ntype Client struct {\n\tconfig\n\tSchema *int\n\tHubSetting *HubSettingClient\n\tUser *UserClient\n\tUserAccessToken *UserAccessTokenClient\n}\n",
	}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range clean {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	t.Run("clean", func(t *testing.T) {
		s := newImportScan(t, writeModule(t, with(nil)))
		if got := s.forbiddenReach(t, relayRoots...); len(got) != 0 {
			t.Fatalf("violations in a clean module: %v", got)
		}
		if got := s.cmdViolations(t); len(got) != 0 {
			t.Fatalf("cmd violations in a clean module: %v", got)
		}
		if got := s.relayDBViolations(t); len(got) != 0 {
			t.Fatalf("relaydb violations in a clean module: %v", got)
		}
		if got := s.forbiddenCalls(t, relayRoots...); len(got) != 0 {
			t.Fatalf("forbidden calls in a clean module: %v", got)
		}
	})
	for name, tc := range map[string]struct {
		files map[string]string
		check func(*importScan) []string
	}{
		"relayrole imports pkg/hub (build-tagged file)": {
			files: map[string]string{"pkg/conduit/relayrole/z.go": "//go:build ignore\n\npackage relayrole\nimport _ \"example.com/m/pkg/hub\"\n"},
			check: func(s *importScan) []string { return s.forbiddenReach(t, relayRoots...) },
		},
		"relay reaches pkg/store transitively": {
			files: map[string]string{"pkg/conduit/relay/c.go": "package relay\nimport _ \"example.com/m/pkg/store\"\n"},
			check: func(s *importScan) []string { return s.forbiddenReach(t, relayRoots...) },
		},
		"relayserver imports pgx directly": {
			files: map[string]string{"pkg/conduit/relayrole/relayserver/p.go": "package relayserver\nimport _ \"github.com/jackc/pgx/v5\"\n"},
			check: func(s *importScan) []string { return s.forbiddenReach(t, relayRoots...) },
		},
		"relay reaches database/sql transitively": {
			files: map[string]string{
				"pkg/conduit/relay/c.go": "package relay\nimport _ \"example.com/m/pkg/util/dbutil\"\n",
				"pkg/util/dbutil/u.go":   "package dbutil\nimport _ \"database/sql\"\n",
			},
			check: func(s *importScan) []string { return s.forbiddenReach(t, relayRoots...) },
		},
		"cmd imports a sqlite driver": {
			files: map[string]string{"cmd/server_relay.go": "package cmd\nimport _ \"modernc.org/sqlite\"\n"},
			check: func(s *importScan) []string { return s.cmdViolations(t) },
		},
		"relayserver imports pkg/ent": {
			files: map[string]string{"pkg/conduit/relayrole/relayserver/e.go": "package relayserver\nimport _ \"example.com/m/pkg/ent\"\n"},
			check: func(s *importScan) []string { return s.forbiddenReach(t, relayRoots...) },
		},
		"relay forgets a principal epoch": {
			files: map[string]string{"pkg/conduit/relay/g.go": "package relay\nfunc g(r *x) { _ = r.registry.ForgetPrincipalEpoch(nil, \"agent\", \"a1\") }\n"},
			check: func(s *importScan) []string { return s.forbiddenCalls(t, relayRoots...) },
		},
		"cmd imports pkg/hub": {
			files: map[string]string{"cmd/server_relay.go": "package cmd\nimport _ \"example.com/m/pkg/hub\"\n"},
			check: func(s *importScan) []string { return s.cmdViolations(t) },
		},
		"cmd imports another store package": {
			files: map[string]string{"cmd/server_relay.go": "package cmd\nimport _ \"example.com/m/pkg/store/entadapter\"\n"},
			check: func(s *importScan) []string { return s.cmdViolations(t) },
		},
		"relaydb reads another entity": {
			files: map[string]string{"pkg/store/relaydb/f.go": "package relaydb\nfunc g(db *x) { _ = db.client.UserAccessToken }\n"},
			check: func(s *importScan) []string { return s.relayDBViolations(t) },
		},
		"relaydb uses the general settings store": {
			files: map[string]string{"pkg/store/relaydb/f.go": "package relaydb\nimport \"example.com/m/pkg/store/entadapter\"\nfunc g() { _ = entadapter.NewHubSettingStore }\n"},
			check: func(s *importScan) []string { return s.relayDBViolations(t) },
		},
		"relaydb imports a token entity": {
			files: map[string]string{"pkg/store/relaydb/f.go": "package relaydb\nimport _ \"example.com/m/pkg/ent/useraccesstoken\"\n"},
			check: func(s *importScan) []string { return s.relayDBViolations(t) },
		},
		"relaydb reads another settings section": {
			files: map[string]string{"pkg/store/relaydb/f.go": "package relaydb\nimport \"example.com/m/pkg/ent/hubsetting\"\nfunc g() { _ = hubsetting.SectionEQ(\"server\") }\n"},
			check: func(s *importScan) []string { return s.relayDBViolations(t) },
		},
		"relaydb uses another entity through a local variable": {
			files: map[string]string{"pkg/store/relaydb/f.go": "package relaydb\nimport \"example.com/m/pkg/ent\"\nfunc g() { var c *ent.Client; _ = c.UserAccessToken }\n"},
			check: func(s *importScan) []string { return s.relayDBViolations(t) },
		},
		"relaydb uses another entity through a parameter": {
			files: map[string]string{"pkg/store/relaydb/f.go": "package relaydb\nimport \"example.com/m/pkg/ent\"\nfunc g(db *ent.Client) { _ = db.User.Query() }\n"},
			check: func(s *importScan) []string { return s.relayDBViolations(t) },
		},
		"relaydb queries settings without the section filter": {
			files: map[string]string{"pkg/store/relaydb/f.go": "package relaydb\nimport \"example.com/m/pkg/ent/hubsetting\"\nfunc g(c *x) { _ = c.HubSetting.Query().Where(hubsetting.UpdatedByEQ(experimentsSection)) }\n"},
			check: func(s *importScan) []string { return s.relayDBViolations(t) },
		},
		"relaydb reads every settings section": {
			files: map[string]string{"pkg/store/relaydb/f.go": "package relaydb\nfunc g(r *x) { _, _ = r.client.HubSetting.Query().All(nil) }\n"},
			check: func(s *importScan) []string { return s.relayDBViolations(t) },
		},
		"relaydb writes a setting": {
			files: map[string]string{"pkg/store/relaydb/f.go": "package relaydb\nfunc g(r *x) { _ = r.client.HubSetting.Create() }\n"},
			check: func(s *importScan) []string { return s.relayDBViolations(t) },
		},
		"relaydb redefines the experiments section": {
			files: map[string]string{"pkg/store/relaydb/f.go": "package relaydb\nconst experimentsSection = \"server\"\n"},
			check: func(s *importScan) []string { return s.relayDBViolations(t) },
		},
		"relaydb listens on the global channel": {
			files: map[string]string{"pkg/store/relaydb/f.go": "package relaydb\nconst ch = \"scion_ev_global\"\n"},
			check: func(s *importScan) []string { return s.relayDBViolations(t) },
		},
		"relaydb listens on a project channel": {
			files: map[string]string{"pkg/store/relaydb/f.go": "package relaydb\nconst ch = \"scion_ev_g_p1\"\n"},
			check: func(s *importScan) []string { return s.relayDBViolations(t) },
		},
		"relaydb LISTENs on another identifier": {
			files: map[string]string{"pkg/store/relaydb/f.go": "package relaydb\nfunc g(c *x) { c.Exec(nil, \"LISTEN \"+other) }\n"},
			check: func(s *importScan) []string { return s.relayDBViolations(t) },
		},
		"relaydb redefines the conduit channel": {
			files: map[string]string{"pkg/store/relaydb/f.go": "package relaydb\nconst ConduitChannel = \"elsewhere\"\n"},
			check: func(s *importScan) []string { return s.relayDBViolations(t) },
		},
		"relaydb names a secret": {
			files: map[string]string{"pkg/store/relaydb/f.go": "package relaydb\nconst q = \"SELECT * FROM signing_keys\"\n"},
			check: func(s *importScan) []string { return s.relayDBViolations(t) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newImportScan(t, writeModule(t, with(tc.files)))
			if got := tc.check(s); len(got) == 0 {
				t.Fatal("not reported")
			}
		})
	}

	t.Run("another file imports relaydb", func(t *testing.T) {
		s := newImportScan(t, writeModule(t, with(map[string]string{"pkg/hub/h.go": "package hub\nimport _ \"example.com/m/pkg/store/relaydb\"\n"})))
		if got := s.relayDBImporters(t); len(got) != 2 {
			t.Fatalf("importers %v, want cmd/server_relay.go and pkg/hub/h.go", got)
		}
	})
}
