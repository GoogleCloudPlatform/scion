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

// Command checkrawsqlpredicates fails when an ent raw-SQL predicate builder
// (entsql.P / entsql.ExprP) emits a top-level OR without enclosing outer
// parentheses.
//
// Ent's Selector.Where joins predicates with AND without wrapping raw
// predicates in parentheses. An unparenthesized top-level OR inside a raw
// predicate (such as `NOT (labels @> $1::jsonb) OR labels IS NULL`,
// ptone/scion#8531) breaks precedence when combined with surrounding WHERE
// clauses (e.g. access-scoping predicates), turning `scope AND (a OR b)` into
// `(scope AND a) OR b`.
//
// # Rules
//
// top-level-or: an entsql.P(func(b *entsql.Builder) { ... }) or entsql.ExprP(...)
// call emits SQL containing the OR keyword at parenthesis depth 0 and outside
// any CASE ... END expression.
//
// # Allowlist
//
// Exceptions live in hack/raw-sql-predicates-allowlist.txt, anchored on
// file, enclosing function, and rule (never line numbers). Malformed or stale
// entries fail the check.
//
// # Scope
//
// Non-test, non-generated Go files under pkg, cmd, and internal (default).
//
// # Limitations
//
// The analyzer is syntactic (go/ast). It models string literals, + concatenation,
// fmt.Sprintf with a literal or resolvable format string, same-package top-level
// string constants and never-reassigned vars, and recursive same-directory
// top-level func(*entsql.Builder) helpers.
//
// Unmodeled shapes (silent negatives): local variables, closures inside a builder
// body, zero-arg entsql.P().Append(...), b.Join(entsql.Expr(...)),
// defer/go/return/labeled/select/type-switch statements in a builder closure,
// method helpers taking *entsql.Builder, and cross-package helpers.
//
// Postgres E'...\” escape strings and $$...$$ dollar-quoted strings are not
// lexed and may produce false positives (fail-closed; cover with the allowlist).
//
// Exit codes:
//
//	0  analysed, no violations
//	1  violations found, or malformed/stale allowlist entry
//	3  could not analyse (parse error, unreadable path, bad allowlist path)
//	4  no candidate files matched
//	5  usage error (unknown flag)
package main

import (
	"bufio"
	"errors"
	"flag"
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
)

const (
	entSQLImportPath = "entgo.io/ent/dialect/sql"
	fmtImportPath    = "fmt"
	ruleTopLevelOR   = "top-level-or"
)

// DefaultPaths are the repo roots scanned when no positional paths are given.
var DefaultPaths = []string{"pkg", "cmd", "internal"}

// DefaultAllowlistPath is the default allowlist file relative to the repo root.
const DefaultAllowlistPath = "hack/raw-sql-predicates-allowlist.txt"

// generatedHeaderRE matches the standard Go generated-file marker.
var generatedHeaderRE = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// Finding represents one raw-SQL predicate violation.
type Finding struct {
	File     string
	Line     int
	Column   int
	Func     string
	Rule     string
	Template string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d:%d: %s: %s: raw SQL predicate contains top-level OR without enclosing parentheses (template: %q; allowlist anchor: %s | %s | %s)",
		f.File, f.Line, f.Column, f.Func, f.Rule, f.Template, f.File, f.Func, f.Rule)
}

// AllowlistEntry is one parsed line from the allowlist file.
type AllowlistEntry struct {
	LineNum       int
	File          string
	Func          string
	Rule          string
	Justification string
	Matched       bool
}

// ParseAllowlist parses an allowlist file. Each non-comment, non-blank line
// must have the form:
//
//	<file> | <function> | <rule> | <one-line justification>
func ParseAllowlist(path string) ([]*AllowlistEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var entries []*AllowlistEntry
	sc := bufio.NewScanner(f)
	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 4 {
			return nil, fmt.Errorf("%s:%d: malformed allowlist entry (expected 4 '|'-separated fields: file | func | rule | justification)", path, lineNum)
		}
		file := filepath.ToSlash(strings.TrimSpace(parts[0]))
		fn := strings.TrimSpace(parts[1])
		rule := strings.TrimSpace(parts[2])
		just := strings.TrimSpace(parts[3])
		if file == "" || fn == "" || rule == "" || just == "" {
			return nil, fmt.Errorf("%s:%d: malformed allowlist entry (empty field)", path, lineNum)
		}
		if rule != ruleTopLevelOR {
			return nil, fmt.Errorf("%s:%d: unknown allowlist rule %q", path, lineNum, rule)
		}
		entries = append(entries, &AllowlistEntry{
			LineNum:       lineNum,
			File:          file,
			Func:          fn,
			Rule:          rule,
			Justification: just,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// CollectFiles returns sorted non-test .go files under paths.
func CollectFiles(paths []string) ([]string, error) {
	seen := map[string]bool{}
	var files []string
	add := func(p string) {
		p = filepath.ToSlash(filepath.Clean(p))
		if !seen[p] {
			seen[p] = true
			files = append(files, p)
		}
	}
	for _, root := range paths {
		info, err := os.Stat(root)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if strings.HasSuffix(root, ".go") && !strings.HasSuffix(root, "_test.go") {
				add(root)
			}
			continue
		}
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				name := d.Name()
				if name == "testdata" || name == ".git" || name == "vendor" || name == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			add(p)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	return files, nil
}

// isGenerated reports whether a parsed Go file has the standard Go generated header.
func isGenerated(f *ast.File) bool {
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if c.Pos() > f.Package {
				return false
			}
			if generatedHeaderRE.MatchString(strings.TrimSpace(c.Text)) {
				return true
			}
		}
	}
	return false
}

// importAliases returns the local package identifier(s) importing importPath.
func importAliases(f *ast.File, importPath, defaultName string) map[string]bool {
	aliases := map[string]bool{}
	for _, imp := range f.Imports {
		val, err := strconv.Unquote(imp.Path.Value)
		if err != nil || val != importPath {
			continue
		}
		if imp.Name != nil {
			name := imp.Name.Name
			if name != "_" && name != "." {
				aliases[name] = true
			}
		} else {
			aliases[defaultName] = true
		}
	}
	return aliases
}

// entSQLAliases returns the local package identifier(s) importing entgo.io/ent/dialect/sql.
func entSQLAliases(f *ast.File) map[string]bool {
	return importAliases(f, entSQLImportPath, "sql")
}

// fmtAliases returns the local package identifier(s) importing fmt.
func fmtAliases(f *ast.File) map[string]bool {
	return importAliases(f, fmtImportPath, "fmt")
}

// builderHelper describes a same-package function that accepts a *entsql.Builder.
type builderHelper struct {
	decl       *ast.FuncDecl
	paramIdx   int
	paramName  string
	fmtAliases map[string]bool
}

// stringDef describes a same-package top-level const or never-reassigned var.
type stringDef struct {
	expr       ast.Expr
	fmtAliases map[string]bool
}

// collectBuilderHelpers indexes top-level functions taking a *<alias>.Builder parameter
// across a directory's parsed files so calls like writeActiveHoldExpr(b) inside an
// entsql.P closure can be traversed.
func collectBuilderHelpers(files []*parsedFile) map[string]builderHelper {
	helpers := map[string]builderHelper{}
	for _, pf := range files {
		if len(pf.aliases) == 0 {
			continue
		}
		for _, decl := range pf.ast.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Body == nil || fd.Type.Params == nil {
				continue
			}
			idx := 0
			for _, field := range fd.Type.Params.List {
				isBuilder := isBuilderPtrType(field.Type, pf.aliases)
				if len(field.Names) == 0 {
					idx++
					continue
				}
				for _, name := range field.Names {
					if isBuilder && name.Name != "_" {
						helpers[fd.Name.Name] = builderHelper{
							decl:       fd,
							paramIdx:   idx,
							paramName:  name.Name,
							fmtAliases: pf.fmtAliases,
						}
					}
					idx++
				}
			}
		}
	}
	return helpers
}

// collectStringDefs indexes same-package top-level consts and never-reassigned
// package-level vars across a directory's parsed files.
func collectStringDefs(files []*parsedFile) map[string]stringDef {
	defs := map[string]stringDef{}
	vars := map[string]stringDef{}

	for _, pf := range files {
		for _, decl := range pf.ast.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Values) != len(vs.Names) {
					continue
				}
				for i, name := range vs.Names {
					if name.Name == "_" {
						continue
					}
					def := stringDef{
						expr:       vs.Values[i],
						fmtAliases: pf.fmtAliases,
					}
					if gd.Tok == token.CONST {
						defs[name.Name] = def
					} else {
						vars[name.Name] = def
					}
				}
			}
		}
	}

	if len(vars) > 0 {
		reassigned := map[string]bool{}
		for _, pf := range files {
			ast.Inspect(pf.ast, func(n ast.Node) bool {
				switch s := n.(type) {
				case *ast.AssignStmt:
					if s.Tok != token.DEFINE {
						for _, lhs := range s.Lhs {
							if id, ok := lhs.(*ast.Ident); ok {
								reassigned[id.Name] = true
							}
						}
					}
				case *ast.UnaryExpr:
					if s.Op == token.AND {
						if id, ok := s.X.(*ast.Ident); ok {
							reassigned[id.Name] = true
						}
					}
				}
				return true
			})
		}
		for name, def := range vars {
			if !reassigned[name] {
				defs[name] = def
			}
		}
	}

	return defs
}

func isBuilderPtrType(expr ast.Expr, aliases map[string]bool) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Builder" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && aliases[id.Name]
}

type parsedFile struct {
	path       string
	ast        *ast.File
	aliases    map[string]bool
	fmtAliases map[string]bool
}

// AnalyzeFiles parses and inspects the given Go files, returning all findings.
func AnalyzeFiles(paths []string) ([]Finding, int, error) {
	fset := token.NewFileSet()
	byDir := map[string][]*parsedFile{}
	var dirs []string
	analysed := 0

	for _, p := range paths {
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			return nil, analysed, fmt.Errorf("parse %s: %w", p, err)
		}
		if isGenerated(f) {
			continue
		}
		analysed++
		dir := filepath.Dir(p)
		if _, exists := byDir[dir]; !exists {
			dirs = append(dirs, dir)
		}
		byDir[dir] = append(byDir[dir], &parsedFile{
			path:       filepath.ToSlash(p),
			ast:        f,
			aliases:    entSQLAliases(f),
			fmtAliases: fmtAliases(f),
		})
	}
	sort.Strings(dirs)

	var findings []Finding
	for _, dir := range dirs {
		pfiles := byDir[dir]
		helpers := collectBuilderHelpers(pfiles)
		strDefs := collectStringDefs(pfiles)
		for _, pf := range pfiles {
			if len(pf.aliases) == 0 {
				continue
			}
			findings = append(findings, analyzeFile(fset, pf, helpers, strDefs)...)
		}
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].Column < findings[j].Column
	})
	return findings, analysed, nil
}

func analyzeFile(fset *token.FileSet, pf *parsedFile, helpers map[string]builderHelper, strDefs map[string]stringDef) []Finding {
	var findings []Finding
	for _, decl := range pf.ast.Decls {
		funcName := "(package)"
		if fd, ok := decl.(*ast.FuncDecl); ok {
			funcName = formatFuncName(fd)
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok || !pf.aliases[pkgIdent.Name] {
				return true
			}

			switch sel.Sel.Name {
			case "P":
				if len(call.Args) != 1 {
					return true
				}
				fn, ok := call.Args[0].(*ast.FuncLit)
				if !ok || fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
					return true
				}
				bName := firstParamName(fn.Type.Params)
				if bName == "" {
					return true
				}
				tmpl := extractBuilderTemplate(fn.Body, bName, pf.fmtAliases, helpers, strDefs, map[string]bool{})
				if HasTopLevelOR(tmpl) {
					pos := fset.Position(call.Pos())
					findings = append(findings, Finding{
						File:     pf.path,
						Line:     pos.Line,
						Column:   pos.Column,
						Func:     funcName,
						Rule:     ruleTopLevelOR,
						Template: compactSpaces(tmpl),
					})
				}
			case "ExprP":
				if len(call.Args) < 1 {
					return true
				}
				tmpl := evalStringTemplate(call.Args[0], pf.fmtAliases, strDefs, map[string]bool{})
				if HasTopLevelOR(tmpl) {
					pos := fset.Position(call.Pos())
					findings = append(findings, Finding{
						File:     pf.path,
						Line:     pos.Line,
						Column:   pos.Column,
						Func:     funcName,
						Rule:     ruleTopLevelOR,
						Template: compactSpaces(tmpl),
					})
				}
			}
			return true
		})
	}
	return findings
}

func formatFuncName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recvType := recvTypeName(fd.Recv.List[0].Type)
	if recvType == "" {
		return fd.Name.Name
	}
	return recvType + "." + fd.Name.Name
}

func recvTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.IndexExpr:
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

func firstParamName(params *ast.FieldList) string {
	if params == nil || len(params.List) == 0 || len(params.List[0].Names) == 0 {
		return ""
	}
	name := params.List[0].Names[0].Name
	if name == "_" {
		return ""
	}
	return name
}

// extractBuilderTemplate walks a *entsql.Builder closure body in execution order
// and reconstructs the emitted SQL template string.
func extractBuilderTemplate(body *ast.BlockStmt, bName string, fileFmtAliases map[string]bool, helpers map[string]builderHelper, strDefs map[string]stringDef, visited map[string]bool) string {
	var out strings.Builder
	var walk func(node ast.Node)
	walk = func(node ast.Node) {
		if node == nil {
			return
		}
		switch n := node.(type) {
		case *ast.BlockStmt:
			for _, st := range n.List {
				walk(st)
			}
		case *ast.ExprStmt:
			walk(n.X)
		case *ast.CallExpr:
			if chain, ok := unwrapBuilderChain(n, bName); ok {
				for _, step := range chain {
					emitBuilderMethod(&out, step, fileFmtAliases, strDefs)
				}
				return
			}
			// Check for a same-package helper call like writeActiveHoldExpr(b).
			if id, ok := n.Fun.(*ast.Ident); ok {
				if h, found := helpers[id.Name]; found && !visited[id.Name] && h.paramIdx < len(n.Args) {
					if argID, ok := n.Args[h.paramIdx].(*ast.Ident); ok && argID.Name == bName {
						visited[id.Name] = true
						out.WriteString(extractBuilderTemplate(h.decl.Body, h.paramName, h.fmtAliases, helpers, strDefs, visited))
						delete(visited, id.Name)
						return
					}
				}
			}
		case *ast.IfStmt:
			if n.Init != nil {
				walk(n.Init)
			}
			walk(n.Body)
			if n.Else != nil {
				out.WriteByte(' ')
				walk(n.Else)
			}
		case *ast.SwitchStmt:
			if n.Init != nil {
				walk(n.Init)
			}
			walk(n.Body)
		case *ast.CaseClause:
			for _, st := range n.Body {
				walk(st)
			}
		case *ast.ForStmt:
			if n.Init != nil {
				walk(n.Init)
			}
			walk(n.Body)
		case *ast.RangeStmt:
			walk(n.Body)
		case *ast.AssignStmt:
			for _, rhs := range n.Rhs {
				walk(rhs)
			}
		}
	}
	walk(body)
	return out.String()
}

type builderStep struct {
	method string
	args   []ast.Expr
}

// unwrapBuilderChain unrolls a fluent call chain rooted at bName (e.g.
// b.WriteString("a").Arg(x).WriteString("b")) in left-to-right execution order.
func unwrapBuilderChain(call *ast.CallExpr, bName string) ([]builderStep, bool) {
	var rev []builderStep
	cur := call
	for {
		sel, ok := cur.Fun.(*ast.SelectorExpr)
		if !ok {
			return nil, false
		}
		rev = append(rev, builderStep{method: sel.Sel.Name, args: cur.Args})
		switch x := sel.X.(type) {
		case *ast.Ident:
			if x.Name != bName {
				return nil, false
			}
			// Reverse into execution order.
			for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
				rev[i], rev[j] = rev[j], rev[i]
			}
			return rev, true
		case *ast.CallExpr:
			cur = x
		default:
			return nil, false
		}
	}
}

func emitBuilderMethod(out *strings.Builder, step builderStep, fileFmtAliases map[string]bool, strDefs map[string]stringDef) {
	switch step.method {
	case "WriteString":
		if len(step.args) == 1 {
			out.WriteString(evalStringTemplate(step.args[0], fileFmtAliases, strDefs, map[string]bool{}))
		}
	case "WriteByte":
		if len(step.args) == 1 {
			if lit, ok := step.args[0].(*ast.BasicLit); ok && lit.Kind == token.CHAR {
				if val, err := strconv.Unquote(lit.Value); err == nil {
					out.WriteString(val)
					return
				}
			}
			out.WriteString(" __expr__ ")
		}
	case "WriteOp":
		if len(step.args) == 1 {
			out.WriteString(evalOp(step.args[0]))
		}
	case "Arg", "Args", "Ident", "IdentComma":
		out.WriteString(" __arg__ ")
	case "Comma":
		out.WriteString(", ")
	case "Pad":
		out.WriteByte(' ')
	case "Nested":
		// b.Nested(func(b *entsql.Builder) { ... }) wraps its body in parentheses.
		out.WriteString("(__nested__)")
	}
}

func evalOp(expr ast.Expr) string {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return " __op__ "
	}
	switch sel.Sel.Name {
	case "OpOr":
		return " OR "
	case "OpAnd":
		return " AND "
	case "OpEQ":
		return " = "
	case "OpNEQ":
		return " <> "
	case "OpGT":
		return " > "
	case "OpGTE":
		return " >= "
	case "OpLT":
		return " < "
	case "OpLTE":
		return " <= "
	case "OpIn":
		return " IN "
	case "OpNotIn":
		return " NOT IN "
	case "OpIsNull":
		return " IS NULL "
	case "OpNotNull":
		return " IS NOT NULL "
	default:
		return " __op__ "
	}
}

// evalStringTemplate evaluates a Go string expression into a SQL template,
// keeping string literals verbatim, expanding fmt.Sprintf format strings and
// same-package top-level string consts / never-reassigned vars, and replacing
// dynamic expressions with a neutral placeholder token so adjacent SQL tokens
// do not merge.
func evalStringTemplate(expr ast.Expr, fileFmtAliases map[string]bool, strDefs map[string]stringDef, visited map[string]bool) string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			if s, err := strconv.Unquote(e.Value); err == nil {
				return s
			}
		}
		return " __expr__ "
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return evalStringTemplate(e.X, fileFmtAliases, strDefs, visited) + evalStringTemplate(e.Y, fileFmtAliases, strDefs, visited)
		}
		return " __expr__ "
	case *ast.ParenExpr:
		return evalStringTemplate(e.X, fileFmtAliases, strDefs, visited)
	case *ast.Ident:
		if def, ok := strDefs[e.Name]; ok && !visited[e.Name] {
			visited[e.Name] = true
			res := evalStringTemplate(def.expr, def.fmtAliases, strDefs, visited)
			delete(visited, e.Name)
			return res
		}
		return " __expr__ "
	case *ast.CallExpr:
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sprintf" && len(e.Args) >= 1 {
			if pkgID, ok := sel.X.(*ast.Ident); ok && fileFmtAliases[pkgID.Name] {
				format := evalStringTemplate(e.Args[0], fileFmtAliases, strDefs, visited)
				return replaceFormatVerbs(format)
			}
		}
		return " __expr__ "
	default:
		return " __expr__ "
	}
}

// replaceFormatVerbs replaces fmt.Sprintf format verbs (%s, %[1]q, %.2f, etc.)
// with a neutral placeholder token and unescapes %% to %.
func replaceFormatVerbs(format string) string {
	var out strings.Builder
	n := len(format)
	for i := 0; i < n; {
		if format[i] != '%' {
			out.WriteByte(format[i])
			i++
			continue
		}
		if i+1 < n && format[i+1] == '%' {
			out.WriteByte('%')
			i += 2
			continue
		}
		j := i + 1
		// Skip flags, width, precision, and argument indexes ([n]).
		for j < n {
			c := format[j]
			if c == '+' || c == '-' || c == '#' || c == ' ' || c == '0' || c == '.' || c == '*' || (c >= '0' && c <= '9') {
				j++
				continue
			}
			if c == '[' {
				k := j + 1
				for k < n && format[k] >= '0' && format[k] <= '9' {
					k++
				}
				if k > j+1 && k < n && format[k] == ']' {
					j = k + 1
					continue
				}
			}
			break
		}
		if j < n && ((format[j] >= 'a' && format[j] <= 'z') || (format[j] >= 'A' && format[j] <= 'Z')) {
			out.WriteString(" __expr__ ")
			i = j + 1
			continue
		}
		out.WriteByte(format[i])
		i++
	}
	return out.String()
}

// HasTopLevelOR reports whether sql contains an OR keyword token at parenthesis
// depth 0 and outside any CASE ... END expression, ignoring SQL string literals,
// quoted identifiers, and SQL comments.
func HasTopLevelOR(sql string) bool {
	parenDepth := 0
	caseDepth := 0
	n := len(sql)
	for i := 0; i < n; {
		c := sql[i]
		// Single-quoted SQL string literal ('...', with '' escape).
		if c == '\'' {
			i++
			for i < n {
				if sql[i] == '\'' {
					i++
					if i < n && sql[i] == '\'' {
						i++
						continue
					}
					break
				}
				i++
			}
			continue
		}
		// Double-quoted SQL identifier ("...", with "" escape).
		if c == '"' {
			i++
			for i < n {
				if sql[i] == '"' {
					i++
					if i < n && sql[i] == '"' {
						i++
						continue
					}
					break
				}
				i++
			}
			continue
		}
		// Line comment (-- ...).
		if c == '-' && i+1 < n && sql[i+1] == '-' {
			i += 2
			for i < n && sql[i] != '\n' {
				i++
			}
			continue
		}
		// Block comment (/* ... */).
		if c == '/' && i+1 < n && sql[i+1] == '*' {
			i += 2
			for i+1 < n && (sql[i] != '*' || sql[i+1] != '/') {
				i++
			}
			if i+1 < n {
				i += 2
			} else {
				i = n
			}
			continue
		}
		if c == '(' {
			parenDepth++
			i++
			continue
		}
		if c == ')' {
			if parenDepth > 0 {
				parenDepth--
			}
			i++
			continue
		}
		if isIdentStart(c) {
			start := i
			i++
			for i < n && isIdentPart(sql[i]) {
				i++
			}
			word := strings.ToUpper(sql[start:i])
			switch word {
			case "CASE":
				caseDepth++
			case "END":
				if caseDepth > 0 {
					caseDepth--
				}
			case "OR":
				if parenDepth == 0 && caseDepth == 0 {
					return true
				}
			}
			continue
		}
		i++
	}
	return false
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func compactSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// FilterFindings applies allowlist entries to findings, returning remaining
// violations and any stale allowlist entries.
func FilterFindings(findings []Finding, allowlist []*AllowlistEntry) (violations []Finding, stale []*AllowlistEntry) {
	for _, f := range findings {
		matched := false
		for _, a := range allowlist {
			if a.File == f.File && a.Func == f.Func && a.Rule == f.Rule {
				a.Matched = true
				matched = true
				break
			}
		}
		if !matched {
			violations = append(violations, f)
		}
	}
	for _, a := range allowlist {
		if !a.Matched {
			stale = append(stale, a)
		}
	}
	return violations, stale
}

func run(args []string) int {
	fs := flag.NewFlagSet("checkrawsqlpredicates", flag.ContinueOnError)
	allowlistPath := fs.String("allowlist", DefaultAllowlistPath, "path to allowlist file (empty to disable)")
	if err := fs.Parse(args); err != nil {
		return 5
	}
	paths := fs.Args()
	customPaths := len(paths) > 0
	if !customPaths {
		paths = DefaultPaths
	}

	var allowlist []*AllowlistEntry
	if *allowlistPath != "" {
		entries, err := ParseAllowlist(*allowlistPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "checkrawsqlpredicates: allowlist file not found: %v\n", err)
				return 3
			}
			fmt.Fprintf(os.Stderr, "checkrawsqlpredicates: %v\n", err)
			return 1
		}
		allowlist = entries
	}

	files, err := CollectFiles(paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "checkrawsqlpredicates: %v\n", err)
		return 3
	}
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "checkrawsqlpredicates: no candidate .go files found under %v — NOTHING WAS ANALYSED\n", paths)
		return 4
	}

	findings, analysed, err := AnalyzeFiles(files)
	if err != nil {
		fmt.Fprintf(os.Stderr, "checkrawsqlpredicates: %v\n", err)
		return 3
	}
	if analysed == 0 {
		fmt.Fprintf(os.Stderr, "checkrawsqlpredicates: all candidate files were generated — NOTHING WAS ANALYSED\n")
		return 4
	}

	violations, stale := FilterFindings(findings, allowlist)

	failed := false
	for _, v := range violations {
		fmt.Fprintln(os.Stderr, v.String())
		failed = true
	}
	if !customPaths {
		for _, s := range stale {
			fmt.Fprintf(os.Stderr, "%s:%d: stale allowlist entry (matched nothing): %s | %s | %s\n",
				*allowlistPath, s.LineNum, s.File, s.Func, s.Rule)
			failed = true
		}
	}
	if failed {
		return 1
	}
	fmt.Fprintf(os.Stderr, "checkrawsqlpredicates: OK (%d files analysed, %d allowlisted)\n", analysed, len(findings)-len(violations))
	return 0
}

func main() {
	os.Exit(run(os.Args[1:]))
}
