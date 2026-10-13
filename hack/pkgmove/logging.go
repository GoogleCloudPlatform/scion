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

package main

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"
)

const categoryLogging = "moved function calls log/slog: log sourceLocation (file/function) and ERROR stack traces change; check log-based metrics and Error Reporting grouping"

// loggingPkgs are the packages whose calls record the caller's file and
// function (slog AddSource, log Lshortfile/Llongfile, Cloud Logging
// sourceLocation, Error Reporting stack grouping).
var loggingPkgs = map[string]bool{"log": true, "log/slog": true}

// loggingFindings warns once per moved function (or package-level var
// initialiser) that calls, or uses as a func value, a record-emitting
// function of log or log/slog, or such a method of log.Logger or slog.Logger
// (emitFuncs): the logged source location and the
// stack traces of ERROR entries now name the target package and file, which
// moves log-based metrics filters and Error Reporting groups.
func (a *analysis) loggingFindings() {
	for _, f := range a.files {
		if !f.Moved {
			continue
		}
		typed := f.Included && !f.XTest
		var logImports map[string]string // external tests: import name -> path
		if !typed {
			logImports = map[string]string{}
			for _, spec := range f.AST.Imports {
				if p := importPath(spec); loggingPkgs[p] {
					if n := importName(spec); n != "_" && n != "." {
						logImports[n] = p
					}
				}
			}
			if len(logImports) == 0 {
				continue
			}
		}
		report := func(pos token.Pos, label string, bodies ...ast.Node) {
			var uses []string
			values := false
			for _, body := range bodies {
				callFuns := map[*ast.SelectorExpr]bool{}
				ast.Inspect(body, func(n ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok {
						if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
							callFuns[sel] = true
						}
					}
					return true
				})
				ast.Inspect(body, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					var c string
					if typed {
						c = a.loggingEmitter(sel)
					} else if x, ok := sel.X.(*ast.Ident); ok && logImports[x.Name] != "" && emitFuncs[logImports[x.Name]][sel.Sel.Name] {
						c = x.Name + "." + sel.Sel.Name
					}
					if c == "" {
						return true
					}
					if !callFuns[sel] {
						// A func value (f := slog.Error; var logf = log.Printf;
						// l.Info passed along) logs from wherever it is called.
						c += " (func value)"
						values = true
					}
					uses = append(uses, c)
					return true
				})
			}
			if len(uses) == 0 {
				return
			}
			sort.Strings(uses)
			verb := "calls"
			if values {
				verb = "calls or uses as func values"
			}
			a.plan.add(levelWarn, categoryLogging, a.posOf(pos), "%s %s %s", label, verb, strings.Join(dedupStrings(uses), ", "))
		}
		for _, d := range f.AST.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				label := "func " + d.Name.Name
				if d.Recv != nil && len(d.Recv.List) == 1 {
					if id := embeddedIdent(d.Recv.List[0].Type); id != nil {
						label = "method " + id.Name + "." + d.Name.Name
					}
				}
				report(d.Name.Pos(), label, d.Body)
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, s := range d.Specs {
					vs := s.(*ast.ValueSpec)
					var values []ast.Node
					for _, v := range vs.Values {
						values = append(values, v)
					}
					report(vs.Pos(), "var "+identNames(vs.Names), values...)
				}
			}
		}
	}
}

// emitFuncs lists, per logging package, the functions and Logger methods
// that emit a record (and so record the caller's source location). Logger
// constructors, With/WithGroup, Handler, Enabled, Default and attribute
// constructors only build values and are not reported.
var emitFuncs = map[string]map[string]bool{
	"log/slog": setOf("Debug", "DebugContext", "Info", "InfoContext", "Warn", "WarnContext",
		"Error", "ErrorContext", "Log", "LogAttrs"),
	"log": setOf("Print", "Printf", "Println", "Fatal", "Fatalf", "Fatalln",
		"Panic", "Panicf", "Panicln", "Output"),
}

func setOf(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

// loggingEmitter names the record-emitting logging function or Logger method
// that sel denotes (called or as a value), or "".
func (a *analysis) loggingEmitter(sel *ast.SelectorExpr) string {
	var fn *types.Func
	if s, ok := a.info.Selections[sel]; ok {
		fn, _ = s.Obj().(*types.Func)
	} else {
		fn, _ = a.info.Uses[sel.Sel].(*types.Func)
	}
	if fn == nil || fn.Pkg() == nil || !emitFuncs[fn.Pkg().Path()][fn.Name()] {
		return ""
	}
	recv := fn.Signature().Recv()
	if recv == nil {
		return fn.Pkg().Name() + "." + fn.Name()
	}
	t := recv.Type()
	ptr := ""
	if p, ok := t.(*types.Pointer); ok {
		t, ptr = p.Elem(), "*"
	}
	n, ok := types.Unalias(t).(*types.Named)
	if !ok || n.Obj().Name() != "Logger" {
		return ""
	}
	return "(" + ptr + fn.Pkg().Name() + ".Logger)." + fn.Name()
}
