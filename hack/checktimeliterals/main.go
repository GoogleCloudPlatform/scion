// Package main implements the time-literals regression gate
// (hack/check-time-literals.sh, `make time-literals`).
//
// Invariant enforced over the server-side packages (see defaultDirs):
//
//  1. format-utc: no time is formatted to a string without first being
//     converted to UTC. A call R.Format(L) or R.AppendFormat(b, L), where L is
//     a time layout, must have a receiver R that is provably UTC: a call to
//     .UTC() (or .AsTime(), which protobuf timestamps define to return UTC),
//     an Add/AddDate/Truncate/Round of such a value, or a local variable
//     whose every assignment in the enclosing function is one of those.
//  2. ent-bind-formatted: no raw SQL statement that names an ent table binds
//     a formatted time string, and no ent sql predicate (sql.EQ, sql.LT, ...)
//     compares against one. Ent time columns store time.Time values; a
//     formatted string sorts differently from them.
//  3. webchat-bind-time: no raw SQL statement that names a webchat_* table
//     binds a time.Time value in the SQLite store. Those columns are TEXT
//     holding RFC3339Nano UTC strings; modernc would store Time.String()
//     text that their readers cannot parse. The Postgres twin
//     (*_postgres.go) is exempt: its webchat_* columns are TIMESTAMPTZ.
//
// The check is syntactic (go/ast, no type checking), so it is deliberately
// conservative about what counts as UTC and best-effort about what counts as
// a time value or a formatted string. Known limits, documented so that a green
// run is not over-read:
//   - A layout held in a function parameter or a struct field is not
//     recognised; layouts are time.<Const>, string literals containing Go
//     layout tokens, and package-level constants with such values.
//   - fmt verbs (%s/%v) and Time.String() are not checked.
//   - SQL text is resolved only from string literals, package constants and
//     local variables built from literals (including fmt.Sprintf formats and
//     += appends). Statements built any other way are skipped.
//   - A struct field counts as a time value when any struct in the scanned
//     packages declares a field of that name with type time.Time.
//
// Exceptions live in hack/time-literals-allowlist.txt, one per line:
//
//	<file> | <function> | <finding> | <one-line justification>
//
// anchored on the file, the enclosing function and the finding text (never a
// line number). An entry that matches nothing fails the run, so the list
// cannot go stale.
//
// Exit codes: 0 clean, 1 violations (or a malformed or stale allowlist),
// 3 could not analyse (bad flags, unreadable allowlist, parse error),
// 4 no candidate files.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var defaultDirs = []string{
	"pkg/hub",
	"pkg/store",
	"pkg/runtimebroker",
	"pkg/hubsync",
	"pkg/sciontool/hub",
	"pkg/runtime/cloudrun",
}

const (
	ruleFormatUTC        = "format-utc"
	ruleEntBindFormatted = "ent-bind-formatted"
	ruleWebchatBindTime  = "webchat-bind-time"
)

// Finding is one violation of the invariant.
type Finding struct {
	File string // slash-separated path relative to the root
	Line int
	Func string
	Rule string
	Text string // normalised source text, the allowlist anchor
	Msg  string
}

func (f Finding) key() string { return f.File + "|" + f.Func + "|" + f.Text }

// AllowEntry is one allowlist line.
type AllowEntry struct {
	Line   int
	File   string
	Func   string
	Text   string
	Reason string
	used   bool
}

func main() {
	root := flag.String("root", ".", "repository root")
	allowPath := flag.String("allowlist", "hack/time-literals-allowlist.txt", "allowlist file, relative to -root (empty for none)")
	flag.Parse()
	dirs := flag.Args()
	if len(dirs) == 0 {
		dirs = defaultDirs
	}
	os.Exit(run(*root, *allowPath, dirs))
}

func run(root, allowPath string, dirs []string) int {
	findings, nfiles, err := Scan(root, dirs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "time-literals: could not analyse: %v\n", err)
		return 3
	}
	if nfiles == 0 {
		fmt.Fprintf(os.Stderr, "time-literals: no Go files found under %v (wrong working directory?)\n", dirs)
		return 4
	}
	var allow []*AllowEntry
	var allowErrs []string
	if allowPath != "" {
		data, err := os.ReadFile(filepath.Join(root, allowPath))
		if err != nil {
			fmt.Fprintf(os.Stderr, "time-literals: could not read allowlist: %v\n", err)
			return 3
		}
		allow, allowErrs = ParseAllowlist(string(data))
	}
	remaining, stale := ApplyAllowlist(findings, allow)

	rc := 0
	for _, e := range allowErrs {
		fmt.Fprintf(os.Stderr, "%s: %s\n", allowPath, e)
		rc = 1
	}
	for _, e := range stale {
		fmt.Fprintf(os.Stderr, "%s:%d: stale allowlist entry matches nothing: %s | %s | %s\n", allowPath, e.Line, e.File, e.Func, e.Text)
		rc = 1
	}
	for _, f := range remaining {
		fmt.Fprintf(os.Stderr, "%s:%d: [%s] %s: %s\n    %s\n    allowlist anchor: %s | %s | %s | <justification>\n",
			f.File, f.Line, f.Rule, f.Func, f.Text, f.Msg, f.File, f.Func, f.Text)
		rc = 1
	}
	if rc != 0 {
		fmt.Fprintf(os.Stderr, "time-literals: FAILED (%d violation(s), %d allowlisted, %d file(s) scanned). See hack/check-time-literals.sh.\n",
			len(remaining), len(findings)-len(remaining), nfiles)
		return 1
	}
	fmt.Printf("time-literals: no violations (%d file(s) scanned, %d allowlisted)\n", nfiles, len(findings))
	return 0
}

// ParseAllowlist parses allowlist text. Blank lines and lines starting with
// '#' are ignored.
func ParseAllowlist(text string) ([]*AllowEntry, []string) {
	var entries []*AllowEntry
	var errs []string
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			errs = append(errs, fmt.Sprintf("line %d: want 4 '|'-separated fields (file | function | finding | justification)", n))
			continue
		}
		e := &AllowEntry{Line: n}
		e.File = strings.TrimSpace(parts[0])
		e.Func = strings.TrimSpace(parts[1])
		e.Text = normalise(parts[2])
		e.Reason = strings.TrimSpace(parts[3])
		if e.File == "" || e.Func == "" || e.Text == "" || e.Reason == "" {
			errs = append(errs, fmt.Sprintf("line %d: every field, including the justification, must be non-empty", n))
			continue
		}
		entries = append(entries, e)
	}
	return entries, errs
}

// ApplyAllowlist removes allowlisted findings and returns the remainder plus
// the entries that matched nothing.
func ApplyAllowlist(findings []Finding, allow []*AllowEntry) ([]Finding, []*AllowEntry) {
	byKey := map[string]*AllowEntry{}
	for _, e := range allow {
		byKey[e.File+"|"+e.Func+"|"+e.Text] = e
	}
	var remaining []Finding
	for _, f := range findings {
		if e, ok := byKey[f.key()]; ok {
			e.used = true
			continue
		}
		remaining = append(remaining, f)
	}
	var stale []*AllowEntry
	for _, e := range allow {
		if !e.used {
			stale = append(stale, e)
		}
	}
	return remaining, stale
}

// Scan parses every non-test, non-generated Go file under dirs (relative to
// root) and returns the findings, sorted by file and line.
func Scan(root string, dirs []string) ([]Finding, int, error) {
	type pkgFiles struct {
		files []*ast.File
		paths []string
	}
	fset := token.NewFileSet()
	pkgs := map[string]*pkgFiles{} // keyed by directory
	var order []string
	nfiles := 0
	for _, d := range dirs {
		base := filepath.Join(root, d)
		if _, err := os.Stat(base); err != nil {
			return nil, 0, fmt.Errorf("scan directory %s: %w", d, err)
		}
		err := filepath.WalkDir(base, func(path string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if de.IsDir() {
				name := de.Name()
				if path != base && (name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if isGenerated(src) {
				return nil
			}
			f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			dir := filepath.Dir(path)
			p := pkgs[dir]
			if p == nil {
				p = &pkgFiles{}
				pkgs[dir] = p
				order = append(order, dir)
			}
			p.files = append(p.files, f)
			p.paths = append(p.paths, filepath.ToSlash(rel))
			nfiles++
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}

	// Pass 1: names of struct fields typed time.Time anywhere in scope.
	timeFields := map[string]bool{}
	for _, p := range pkgs {
		for _, f := range p.files {
			collectTimeFields(f, timeFields)
		}
	}

	// Pass 2: per package, constants, then per file, the checks.
	var findings []Finding
	sort.Strings(order)
	for _, dir := range order {
		p := pkgs[dir]
		consts := map[string]string{}
		for _, f := range p.files {
			collectStringConsts(f, consts)
		}
		for i, f := range p.files {
			c := &checker{
				fset:       fset,
				path:       p.paths[i],
				consts:     consts,
				timeFields: timeFields,
				postgres:   strings.HasSuffix(p.paths[i], "_postgres.go"),
			}
			c.file(f)
			findings = append(findings, c.findings...)
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, nfiles, nil
}

var generatedRE = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

func isGenerated(src []byte) bool {
	head := src
	if len(head) > 4096 {
		head = head[:4096]
	}
	return generatedRE.Match(head)
}

func isTimeTimeType(e ast.Expr) bool {
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "time" && sel.Sel.Name == "Time"
}

func collectTimeFields(f *ast.File, out map[string]bool) {
	ast.Inspect(f, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok {
			return true
		}
		for _, fld := range st.Fields.List {
			if isTimeTimeType(fld.Type) {
				for _, nm := range fld.Names {
					out[nm.Name] = true
				}
			}
		}
		return true
	})
}

func collectStringConsts(f *ast.File, out map[string]string) {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
			continue
		}
		for _, sp := range gd.Specs {
			vs := sp.(*ast.ValueSpec)
			for i, nm := range vs.Names {
				if i < len(vs.Values) {
					if s, ok := literalString(vs.Values[i], nil); ok {
						out[nm.Name] = s
					}
				}
			}
		}
	}
}

// literalString evaluates string literals and their concatenation. consts may
// be nil.
func literalString(e ast.Expr, consts map[string]string) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.Ident:
		if consts != nil {
			s, ok := consts[v.Name]
			return s, ok
		}
	case *ast.ParenExpr:
		return literalString(v.X, consts)
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			a, ok1 := literalString(v.X, consts)
			b, ok2 := literalString(v.Y, consts)
			if ok1 && ok2 {
				return a + b, true
			}
		}
	}
	return "", false
}

var layoutConsts = map[string]bool{
	"Layout": true, "ANSIC": true, "UnixDate": true, "RubyDate": true,
	"RFC822": true, "RFC822Z": true, "RFC850": true, "RFC1123": true,
	"RFC1123Z": true, "RFC3339": true, "RFC3339Nano": true, "Kitchen": true,
	"Stamp": true, "StampMilli": true, "StampMicro": true, "StampNano": true,
	"DateTime": true, "DateOnly": true, "TimeOnly": true,
}

// layoutTokenRE matches the Go reference-time tokens that make a string a
// time layout rather than ordinary text.
var layoutTokenRE = regexp.MustCompile(`2006|15:04|Z07|-07:?00`)

// Kind of value a local variable holds, as far as the checker can tell.
type kind uint8

const (
	kUnknown   kind = 1 << iota
	kUTC            // a time.Time known to be in UTC
	kTime           // a time.Time of unknown location
	kFormatted      // a string produced by formatting a time
	kSQL            // a string built from literals (possible SQL)
)

type checker struct {
	fset       *token.FileSet
	path       string
	consts     map[string]string
	timeFields map[string]bool
	postgres   bool
	findings   []Finding

	fn    string
	vars  map[string]kind
	sqls  map[string][]string   // local name -> literal fragments assigned to it
	slice map[string][]ast.Expr // local name -> elements appended to an args slice
	local map[string]string     // local string consts within the function
}

func (c *checker) file(f *ast.File) {
	for _, d := range f.Decls {
		switch v := d.(type) {
		case *ast.FuncDecl:
			if v.Body == nil {
				continue
			}
			c.begin(funcName(v))
			c.params(v.Type)
			c.body(v.Body)
		case *ast.GenDecl:
			if v.Tok == token.VAR {
				c.begin("(package)")
				c.body(v)
			}
		}
	}
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	switch x := t.(type) {
	case *ast.IndexExpr:
		t = x.X
	case *ast.IndexListExpr:
		t = x.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

func (c *checker) begin(fn string) {
	c.fn = fn
	c.vars = map[string]kind{}
	c.sqls = map[string][]string{}
	c.local = map[string]string{}
	c.slice = map[string][]ast.Expr{}
}

func (c *checker) params(ft *ast.FuncType) {
	lists := []*ast.FieldList{ft.Params, ft.Results}
	for _, l := range lists {
		if l == nil {
			continue
		}
		for _, fld := range l.List {
			k := kUnknown
			if isTimeTimeType(fld.Type) {
				k = kTime
			}
			for _, nm := range fld.Names {
				c.vars[nm.Name] |= k
			}
		}
	}
}

// body runs two passes over a function: first it classifies every local
// assignment (function literals included, since they share the names), then
// it checks every call.
func (c *checker) body(n ast.Node) {
	ast.Inspect(n, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncLit:
			c.params(v.Type)
		case *ast.AssignStmt:
			if len(v.Lhs) == len(v.Rhs) {
				for i := range v.Lhs {
					if id, ok := v.Lhs[i].(*ast.Ident); ok {
						c.assign(id.Name, v.Rhs[i], v.Tok)
					}
				}
			} else {
				for _, l := range v.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						c.vars[id.Name] |= c.multiKind(v.Rhs)
					}
				}
			}
		case *ast.ValueSpec:
			for i, nm := range v.Names {
				switch {
				case i < len(v.Values):
					c.assign(nm.Name, v.Values[i], token.DEFINE)
				case v.Type != nil && isTimeTimeType(v.Type):
					c.vars[nm.Name] |= kTime
				default:
					c.vars[nm.Name] |= kUnknown
				}
			}
		case *ast.RangeStmt:
			for _, e := range []ast.Expr{v.Key, v.Value} {
				if id, ok := e.(*ast.Ident); ok {
					c.vars[id.Name] |= kUnknown
				}
			}
		}
		return true
	})
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			c.call(call)
		}
		return true
	})
}

// multiKind classifies the targets of a multi-value assignment such as
// t, err := time.Parse(...).
func (c *checker) multiKind(rhs []ast.Expr) kind {
	if len(rhs) == 1 {
		if call, ok := rhs[0].(*ast.CallExpr); ok {
			if pkgFunc(call, "time", "Parse", "ParseInLocation") {
				return kTime
			}
		}
	}
	return kUnknown
}

func (c *checker) assign(name string, rhs ast.Expr, tok token.Token) {
	if elems, ok := sliceElems(rhs, name); ok {
		c.slice[name] = append(c.slice[name], elems...)
		c.vars[name] |= kUnknown
		return
	}
	if s, ok := literalString(rhs, c.mergedConsts()); ok {
		if tok == token.ADD_ASSIGN {
			c.sqls[name] = append(c.sqls[name], s)
		} else {
			c.sqls[name] = append(c.sqls[name], s)
			if tok == token.DEFINE {
				c.local[name] = s
			}
		}
		c.vars[name] |= kSQL
		return
	}
	if call, ok := rhs.(*ast.CallExpr); ok && pkgFunc(call, "fmt", "Sprintf") && len(call.Args) > 0 {
		if s, ok := literalString(call.Args[0], c.mergedConsts()); ok {
			c.sqls[name] = append(c.sqls[name], s)
			c.vars[name] |= kSQL
			return
		}
	}
	if tok == token.ADD_ASSIGN {
		return
	}
	switch {
	case c.isUTC(rhs):
		c.vars[name] |= kUTC
	case c.isFormatted(rhs):
		c.vars[name] |= kFormatted
	case c.isTimeValue(rhs):
		c.vars[name] |= kTime
	default:
		c.vars[name] |= kUnknown
	}
}

// sliceElems returns the elements of append(name, ...) or of a []any /
// []interface{} composite literal, the two ways an args slice for a variadic
// Exec/Query call is built.
func sliceElems(rhs ast.Expr, name string) ([]ast.Expr, bool) {
	switch v := unparen(rhs).(type) {
	case *ast.CallExpr:
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "append" && len(v.Args) > 0 && v.Ellipsis == token.NoPos {
			if first, ok := v.Args[0].(*ast.Ident); ok && first.Name == name {
				return v.Args[1:], true
			}
		}
	case *ast.CompositeLit:
		if at, ok := v.Type.(*ast.ArrayType); ok && at.Len == nil {
			switch et := at.Elt.(type) {
			case *ast.Ident:
				if et.Name == "any" {
					return v.Elts, true
				}
			case *ast.InterfaceType:
				return v.Elts, true
			}
		}
	}
	return nil, false
}

func (c *checker) mergedConsts() map[string]string {
	if len(c.local) == 0 {
		return c.consts
	}
	m := make(map[string]string, len(c.consts)+len(c.local))
	for k, v := range c.consts {
		m[k] = v
	}
	for k, v := range c.local {
		m[k] = v
	}
	return m
}

func pkgFunc(call *ast.CallExpr, pkg string, names ...string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != pkg {
		return false
	}
	for _, n := range names {
		if sel.Sel.Name == n {
			return true
		}
	}
	return false
}

func methodName(call *ast.CallExpr) (string, ast.Expr) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", nil
	}
	return sel.Sel.Name, sel.X
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// isUTC reports whether e is provably a UTC time.Time.
func (c *checker) isUTC(e ast.Expr) bool {
	e = unparen(e)
	switch v := e.(type) {
	case *ast.CallExpr:
		name, recv := methodName(v)
		switch name {
		case "UTC", "AsTime":
			return len(v.Args) == 0 && !isPkgIdent(recv, "time")
		case "Add", "AddDate", "Truncate", "Round":
			return c.isUTC(recv)
		}
	case *ast.Ident:
		k := c.vars[v.Name]
		return k == kUTC
	}
	return false
}

func isPkgIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// isLayout reports whether e is a time layout.
func (c *checker) isLayout(e ast.Expr) bool {
	e = unparen(e)
	if sel, ok := e.(*ast.SelectorExpr); ok {
		return isPkgIdent(sel.X, "time") && layoutConsts[sel.Sel.Name]
	}
	if s, ok := literalString(e, c.mergedConsts()); ok {
		return layoutTokenRE.MatchString(s)
	}
	return false
}

// formatCall returns the receiver if call formats a time with a layout.
func (c *checker) formatCall(call *ast.CallExpr) (ast.Expr, bool) {
	name, recv := methodName(call)
	switch name {
	case "Format":
		if len(call.Args) == 1 && c.isLayout(call.Args[0]) {
			return recv, true
		}
	case "AppendFormat":
		if len(call.Args) == 2 && c.isLayout(call.Args[1]) {
			return recv, true
		}
	}
	return nil, false
}

func (c *checker) isFormatted(e ast.Expr) bool {
	e = unparen(e)
	switch v := e.(type) {
	case *ast.CallExpr:
		_, ok := c.formatCall(v)
		return ok
	case *ast.Ident:
		return c.vars[v.Name]&kFormatted != 0
	}
	return false
}

func (c *checker) isTimeValue(e ast.Expr) bool {
	e = unparen(e)
	switch v := e.(type) {
	case *ast.StarExpr:
		return c.isTimeValue(v.X)
	case *ast.CallExpr:
		if pkgFunc(v, "time", "Now", "Unix", "UnixMilli", "UnixMicro", "Date") {
			return true
		}
		name, recv := methodName(v)
		switch name {
		case "UTC", "Local", "In", "AsTime":
			return !isPkgIdent(recv, "time")
		case "Add", "AddDate", "Truncate", "Round":
			return c.isTimeValue(recv)
		}
	case *ast.Ident:
		return c.vars[v.Name]&(kUTC|kTime) != 0
	case *ast.SelectorExpr:
		return c.timeFields[v.Sel.Name]
	case *ast.CompositeLit:
		return isTimeTimeType(v.Type)
	}
	return false
}

var sqlMethods = map[string]int{ // method name -> index of the SQL argument
	"Exec": 0, "Query": 0, "QueryRow": 0,
	"ExecContext": 1, "QueryContext": 1, "QueryRowContext": 1,
}

var tableRE = regexp.MustCompile("(?i)\\b(?:INTO|UPDATE|FROM|JOIN)\\s+[\"`]?([a-z_][a-z0-9_]*)")

func (c *checker) call(call *ast.CallExpr) {
	if recv, ok := c.formatCall(call); ok && !c.isUTC(recv) {
		c.report(call, ruleFormatUTC, "time formatted without .UTC(); convert first, e.g. t.UTC().Format(...)")
	}

	if pkgFunc(call, "sql", "EQ", "NEQ", "LT", "LTE", "GT", "GTE", "In", "NotIn") {
		for _, a := range call.Args[min(1, len(call.Args)):] {
			if c.isFormatted(a) {
				c.report(call, ruleEntBindFormatted, "ent predicate compares a time column against a formatted string; pass the time.Time (t.UTC())")
				break
			}
		}
		return
	}
	name, _ := methodName(call)
	idx, ok := sqlMethods[name]
	if !ok || len(call.Args) <= idx {
		return
	}
	sqlText, ok := c.sqlText(call.Args[idx])
	if !ok {
		return
	}
	var ent, webchat []string
	seen := map[string]bool{}
	for _, m := range tableRE.FindAllStringSubmatch(sqlText, -1) {
		t := strings.ToLower(m[1])
		if seen[t] || sqlNonTable[t] {
			continue
		}
		seen[t] = true
		if strings.HasPrefix(t, "webchat_") {
			webchat = append(webchat, t)
		} else {
			ent = append(ent, t)
		}
	}
	binds := call.Args[idx+1:]
	if call.Ellipsis != token.NoPos && len(binds) > 0 {
		if id, ok := binds[len(binds)-1].(*ast.Ident); ok {
			binds = append(append([]ast.Expr{}, binds[:len(binds)-1]...), c.slice[id.Name]...)
		}
	}
	for _, a := range binds {
		if len(ent) > 0 && c.isFormatted(a) {
			c.reportArg(call, a, ruleEntBindFormatted,
				fmt.Sprintf("formatted time string bound in a statement on ent table(s) %s; bind the time.Time (t.UTC())", strings.Join(ent, ",")))
		}
		if len(webchat) > 0 && !c.postgres && c.isTimeValue(a) {
			c.reportArg(call, a, ruleWebchatBindTime,
				fmt.Sprintf("time.Time bound in a statement on SQLite TEXT table(s) %s; bind t.UTC().Format(time.RFC3339Nano)", strings.Join(webchat, ",")))
		}
	}
}

// sqlNonTable lists words the table regexp can pick up that are not tables.
var sqlNonTable = map[string]bool{
	"select": true, "the": true, "a": true, "set": true, "values": true, "excluded": true,
	"lateral": true, "unnest": true, "jsonb_array_elements": true, "json_each": true,
	"generate_series": true, "only": true,
}

func (c *checker) sqlText(e ast.Expr) (string, bool) {
	e = unparen(e)
	if s, ok := literalString(e, c.mergedConsts()); ok {
		return s, true
	}
	if id, ok := e.(*ast.Ident); ok {
		if frags, ok := c.sqls[id.Name]; ok {
			return strings.Join(frags, "\n"), true
		}
	}
	if call, ok := e.(*ast.CallExpr); ok && pkgFunc(call, "fmt", "Sprintf") && len(call.Args) > 0 {
		return c.sqlText(call.Args[0])
	}
	if b, ok := e.(*ast.BinaryExpr); ok && b.Op == token.ADD {
		l, ok1 := c.sqlText(b.X)
		r, ok2 := c.sqlText(b.Y)
		if ok1 || ok2 {
			return l + " " + r, true
		}
	}
	return "", false
}

func (c *checker) report(n ast.Node, rule, msg string) {
	c.findings = append(c.findings, Finding{
		File: c.path,
		Line: c.fset.Position(n.Pos()).Line,
		Func: c.fn,
		Rule: rule,
		Text: c.render(n),
		Msg:  msg,
	})
}

func (c *checker) reportArg(call *ast.CallExpr, arg ast.Expr, rule, msg string) {
	name, _ := methodName(call)
	text := name + " arg " + c.render(arg)
	line := c.fset.Position(arg.Pos()).Line
	for _, f := range c.findings {
		if f.Line == line && f.Rule == rule && f.Text == text {
			return // the same variable bound twice in one statement
		}
	}
	c.findings = append(c.findings, Finding{
		File: c.path,
		Line: c.fset.Position(arg.Pos()).Line,
		Func: c.fn,
		Rule: rule,
		Text: name + " arg " + c.render(arg),
		Msg:  msg,
	})
}

func (c *checker) render(n ast.Node) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, c.fset, n); err != nil {
		return "?"
	}
	return normalise(buf.String())
}

var spaceRE = regexp.MustCompile(`\s+`)

func normalise(s string) string {
	s = spaceRE.ReplaceAllString(strings.TrimSpace(s), " ")
	s = strings.ReplaceAll(s, ". ", ".")
	s = strings.ReplaceAll(s, "( ", "(")
	s = strings.ReplaceAll(s, " )", ")")
	s = strings.ReplaceAll(s, ", )", ")")
	s = strings.ReplaceAll(s, ",)", ")")
	return s
}
