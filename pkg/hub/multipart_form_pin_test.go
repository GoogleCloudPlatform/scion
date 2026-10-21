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

package hub

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// parseMultipartFormExceptions lists the non-test functions that may still
// call ParseMultipartForm directly, keyed by file then function.
//
// handleAttachmentUpload is NOT covered by this change: it does not remove its
// parsed form, so temporary files from its larger parts stay behind after the
// request. ptone/scion#4596 covers it. When that lands, convert the site to
// parseMultipartUpload and delete this entry; if it stalls, convert it here.
var parseMultipartFormExceptions = map[string]map[string]bool{
	"handlers_chat_v2.go": {"handleAttachmentUpload": true},
}

// multipartUploadHandlers are the handlers known to parse multipart forms
// through parseMultipartUpload. The pin requires each of them to call it, so
// the scan cannot pass by finding nothing.
var multipartUploadHandlers = []string{
	"handleAdminAllowListImport",
	"handleAdminUserInviteBulk",
	"handleHarnessConfigFileUpload",
	"handleProjectWorkspaceUpload",
	"handleTemplateFileUpload",
	"publishSkillVersionMultipart",
}

// implicitMultipartParsers are the *http.Request methods that run
// ParseMultipartForm themselves (at 32 MiB) when the form is not parsed yet,
// mapped to whether an earlier ParseForm on the same request prevents that.
// FormValue checks r.Form and PostFormValue checks r.PostForm, both of which
// ParseForm sets; FormFile checks only r.MultipartForm.
var implicitMultipartParsers = map[string]bool{
	"FormValue":     true,
	"PostFormValue": true,
	"FormFile":      false,
}

// TestMultipartFormSourcePin checks the pkg/hub non-test sources so that every
// multipart form a handler parses is removed by that handler:
//
//  1. ParseMultipartForm is referenced only inside parseMultipartUpload (plus
//     parseMultipartFormExceptions). The whole file is walked, package-level
//     function literals included.
//  2. Every reference to parseMultipartUpload is the callee of a statement
//     "cleanup, err := parseMultipartUpload(...)" directly in a top-level
//     function's own body (not in a function literal, whose deferred cleanup
//     would run when the literal returns), followed immediately by
//     "defer cleanup()".
//  3. FormValue, FormFile and PostFormValue are called only after an explicit
//     parse of the same request variable: a parseMultipartUpload statement,
//     or, for FormValue and PostFormValue only, a ParseForm statement on a
//     parameter of type *http.Request. The parse must be an earlier statement
//     of a block that encloses the call within the same function, so a parse
//     in a sibling branch does not count. Excepted functions are skipped.
//  4. Server.uploadFormMemoryForTest is referenced (read, set, incremented or
//     addressed) only in uploadFormMaxMemory; tests set it.
//
// The check is syntactic: it matches method names, not types, so a FormValue
// method on another type is checked too (conservative), and a ParseForm is
// accepted only on a *http.Request parameter of the enclosing function.
func TestMultipartFormSourcePin(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	p := &multipartPin{fset: token.NewFileSet(), callers: map[string]bool{}}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		f, err := parser.ParseFile(p.fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		p.scanFile(f)
	}
	for _, h := range multipartUploadHandlers {
		if !p.callers[h] {
			p.problems = append(p.problems, "expected "+h+" to call parseMultipartUpload")
		}
	}
	sort.Strings(p.problems)
	for _, msg := range p.problems {
		t.Error(msg)
	}
}

type multipartPin struct {
	fset     *token.FileSet
	callers  map[string]bool
	problems []string
}

func (p *multipartPin) report(pos token.Pos, format string, args ...any) {
	at := p.fset.Position(pos)
	p.problems = append(p.problems, fmt.Sprintf("%s:%d: ", at.Filename, at.Line)+fmt.Sprintf(format, args...))
}

func (p *multipartPin) scanFile(f *ast.File) {
	// Rule 2: well-formed helper calls, found in the statement lists of each
	// top-level function's own body (function literals are not entered).
	wellFormed := map[*ast.Ident]bool{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if _, ok := n.(*ast.FuncLit); ok {
				return false
			}
			list := stmtList(n)
			for i, st := range list {
				callee, cleanupName := helperAssign(st)
				if callee == nil {
					continue
				}
				if cleanupName == "" {
					p.report(callee.Pos(), "parseMultipartUpload result must be assigned as \"cleanup, err := ...\" with a named cleanup")
					continue
				}
				if i+1 >= len(list) || !isDeferCallOf(list[i+1], cleanupName) {
					p.report(callee.Pos(), "parseMultipartUpload must be followed immediately by \"defer %s()\"", cleanupName)
					continue
				}
				wellFormed[callee] = true
				p.callers[fn.Name.Name] = true
			}
			return true
		})
	}

	// Rules 1 to 4 over the whole file, with the path of enclosing nodes.
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		fnName := enclosingFuncDeclName(stack)
		where := fnName
		if where == "" {
			where = "package-level code"
		}
		switch x := n.(type) {
		case *ast.Ident:
			if x.Name == "parseMultipartUpload" && !isFuncDeclName(stack, x) && !wellFormed[x] {
				p.report(x.Pos(), "parseMultipartUpload must be called in the handler's own body as \"cleanup, err := parseMultipartUpload(...)\" followed by \"defer cleanup()\"")
			}
		case *ast.SelectorExpr:
			switch name := x.Sel.Name; {
			case name == "ParseMultipartForm":
				if !(fnName == "parseMultipartUpload" && enclosingFuncDeclIsPlain(stack)) && !parseMultipartFormExceptions[fileName(p, x)][fnName] {
					p.report(x.Pos(), "ParseMultipartForm called directly in %s; use parseMultipartUpload", where)
				}
			case name == "uploadFormMemoryForTest":
				if fnName != "uploadFormMaxMemory" {
					p.report(x.Pos(), "uploadFormMemoryForTest is referenced outside uploadFormMaxMemory")
				}
			default:
				if afterParseForm, ok := implicitMultipartParsers[name]; ok && !parseMultipartFormExceptions[fileName(p, x)][fnName] {
					if !parsedBefore(stack, x, afterParseForm) {
						p.report(x.Pos(), "%s in %s without an earlier parse of the same request in an enclosing block; it parses the multipart form implicitly", name, where)
					}
				}
			}
		case *ast.KeyValueExpr:
			if id, ok := x.Key.(*ast.Ident); ok && id.Name == "uploadFormMemoryForTest" {
				p.report(id.Pos(), "uploadFormMemoryForTest is referenced outside uploadFormMaxMemory")
			}
		}
		return true
	})
}

// fileName returns the base file name of n's position.
func fileName(p *multipartPin, n ast.Node) string {
	return filepath.Base(p.fset.Position(n.Pos()).Filename)
}

// stmtList returns the statement list n holds directly, if any.
func stmtList(n ast.Node) []ast.Stmt {
	switch b := n.(type) {
	case *ast.BlockStmt:
		return b.List
	case *ast.CaseClause:
		return b.Body
	case *ast.CommClause:
		return b.Body
	}
	return nil
}

// enclosingFuncDeclName returns the name of the top-level function the path
// is in, or "" at package level.
func enclosingFuncDeclName(stack []ast.Node) string {
	for _, n := range stack {
		if fn, ok := n.(*ast.FuncDecl); ok {
			return fn.Name.Name
		}
	}
	return ""
}

// enclosingFuncDeclIsPlain reports whether the top-level function on the path
// is a plain function (no receiver).
func enclosingFuncDeclIsPlain(stack []ast.Node) bool {
	for _, n := range stack {
		if fn, ok := n.(*ast.FuncDecl); ok {
			return fn.Recv == nil
		}
	}
	return false
}

// isFuncDeclName reports whether id is the name of the FuncDecl that holds it.
func isFuncDeclName(stack []ast.Node, id *ast.Ident) bool {
	for _, n := range stack {
		if fn, ok := n.(*ast.FuncDecl); ok {
			return fn.Name == id
		}
	}
	return false
}

// parsedBefore reports whether the call x.Sel(...) at the end of the path is
// preceded, in a block that encloses it within the same function, by a
// statement that parses the same request variable: a parseMultipartUpload
// assignment, or a ParseForm statement when afterParseForm allows it.
func parsedBefore(stack []ast.Node, sel *ast.SelectorExpr, afterParseForm bool) bool {
	recv, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	var fnType *ast.FuncType
	for i := len(stack) - 1; i >= 0; i-- {
		switch fn := stack[i].(type) {
		case *ast.FuncLit:
			fnType = fn.Type
		case *ast.FuncDecl:
			fnType = fn.Type
		}
		if fnType != nil {
			break
		}
	}
	for i := len(stack) - 2; i >= 0; i-- {
		switch stack[i].(type) {
		case *ast.FuncLit, *ast.FuncDecl:
			return false
		}
		list := stmtList(stack[i])
		if list == nil {
			continue
		}
		idx := -1
		for j, st := range list {
			if ast.Node(st) == stack[i+1] {
				idx = j
				break
			}
		}
		if idx < 0 {
			continue
		}
		for _, st := range list[:idx] {
			if callee, _ := helperAssign(st); callee != nil && firstArgIs(st, recv.Name) {
				return true
			}
			if afterParseForm && isParseFormStmt(st, recv.Name) && isRequestParam(fnType, recv.Name) {
				return true
			}
		}
	}
	return false
}

// firstArgIs reports whether the helper assignment st passes the variable
// name as the request.
func firstArgIs(st ast.Stmt, name string) bool {
	as := st.(*ast.AssignStmt)
	call := as.Rhs[0].(*ast.CallExpr)
	if len(call.Args) == 0 {
		return false
	}
	id, ok := call.Args[0].(*ast.Ident)
	return ok && id.Name == name
}

// isParseFormStmt reports whether st is "name.ParseForm()" as an expression
// statement, the right-hand side of an assignment, or an if statement's init.
func isParseFormStmt(st ast.Stmt, name string) bool {
	if is, ok := st.(*ast.IfStmt); ok && is.Init != nil {
		st = is.Init
	}
	var expr ast.Expr
	switch s := st.(type) {
	case *ast.ExprStmt:
		expr = s.X
	case *ast.AssignStmt:
		if len(s.Rhs) == 1 {
			expr = s.Rhs[0]
		}
	}
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "ParseForm" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == name
}

// isRequestParam reports whether name is a parameter of type *http.Request.
func isRequestParam(ft *ast.FuncType, name string) bool {
	if ft == nil || ft.Params == nil {
		return false
	}
	for _, field := range ft.Params.List {
		star, ok := field.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Request" {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "http" {
			continue
		}
		for _, n := range field.Names {
			if n.Name == name {
				return true
			}
		}
	}
	return false
}

// helperAssign reports whether st is an assignment whose single right-hand
// side is a call to parseMultipartUpload, and returns the callee identifier
// and the name bound to the cleanup result ("" if it is discarded or not an
// identifier).
func helperAssign(st ast.Stmt) (*ast.Ident, string) {
	as, ok := st.(*ast.AssignStmt)
	if !ok || len(as.Rhs) != 1 {
		return nil, ""
	}
	call, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok {
		return nil, ""
	}
	callee, ok := call.Fun.(*ast.Ident)
	if !ok || callee.Name != "parseMultipartUpload" {
		return nil, ""
	}
	if len(as.Lhs) != 2 || as.Tok != token.DEFINE {
		return callee, ""
	}
	id, ok := as.Lhs[0].(*ast.Ident)
	if !ok || id.Name == "_" {
		return callee, ""
	}
	return callee, id.Name
}

// isDeferCallOf reports whether st is "defer name()".
func isDeferCallOf(st ast.Stmt, name string) bool {
	d, ok := st.(*ast.DeferStmt)
	if !ok || len(d.Call.Args) != 0 {
		return false
	}
	id, ok := d.Call.Fun.(*ast.Ident)
	return ok && id.Name == name
}
