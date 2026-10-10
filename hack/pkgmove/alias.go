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
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// aliasCandidate is one moved package-level object that gets an alias.
type aliasCandidate struct {
	obj        types.Object
	test       bool
	constraint string
	asVar      bool // function alias that must fall back to a var
}

// renderAliases decides the alias entries, exports what they need to spell,
// and renders the alias files.
func (a *analysis) renderAliases() error {
	var cands []*aliasCandidate
	for _, name := range a.pkg.Scope().Names() {
		obj := a.pkg.Scope().Lookup(name)
		h := a.home(obj)
		if h == nil || !h.Moved || h.IsTest || name == "_" {
			continue
		}
		users, used := a.forward[obj]
		if !used && !obj.Exported() {
			continue
		}
		if kindOf(obj) == "var" {
			continue // vars are rewritten, never aliased
		}
		c := &aliasCandidate{obj: obj, constraint: h.Constraint}
		if !obj.Exported() {
			c.test = true
			for _, u := range users {
				if !u.IsTest {
					c.test = false
				}
			}
		}
		cands = append(cands, c)
	}

	// Types needed to spell wrapper signatures and type parameter lists.
	need := map[types.Object]bool{}
	for _, c := range cands {
		var ok bool
		switch obj := c.obj.(type) {
		case *types.Func:
			if why := a.wrapperHazard(obj); why != "" {
				if obj.Signature().TypeParams().Len() > 0 {
					a.plan.errorf("%s: generic func %s %s, so it needs a var alias, but a generic func cannot be aliased as a var; keep it in %s or restructure first",
						a.posOf(obj.Pos()), obj.Name(), why, a.srcName)
					ok = true
					break
				}
				c.asVar = true
				a.plan.add(levelWarn, "function alias declared as a var (a wrapper would change behaviour)", a.posOf(obj.Pos()),
					"%s %s, so it is aliased as a var instead of a wrapper; the var is assignable and initialised at package init", obj.Name(), why)
				ok = true
				break
			}
			ok = a.spellable(obj.Signature(), need)
			if !ok && obj.Signature().TypeParams().Len() == 0 {
				c.asVar = true
				a.plan.add(levelWarn, "function alias declared as a var (signature not spellable from the source package)", a.posOf(obj.Pos()),
					"%s is aliased as a package-level var; it is now assignable and is initialised at package init", obj.Name())
				ok = true
			}
		case *types.TypeName:
			ok = a.spellableTypeParams(typeParamsOf(obj), need)
		default:
			ok = true
		}
		if !ok {
			a.plan.errorf("%s: cannot spell the alias for %s %s from %s (its signature or constraints use struct/interface literals with unexported members or local types)",
				a.posOf(c.obj.Pos()), kindOf(c.obj), c.obj.Name(), a.srcName)
		}
	}
	a.exportObjects(need)

	// Import names, shared by all alias files.
	reserved := map[string]bool{"_": true}
	for _, name := range a.pkg.Scope().Names() {
		reserved[name] = true
	}
	paths := map[string]string{} // path -> preferred name
	collect := func(p *types.Package) string {
		if p == a.pkg {
			paths[a.dstImport] = a.cfg.PkgName
		} else {
			paths[p.Path()] = p.Name()
		}
		return "x"
	}
	for _, c := range cands {
		a.renderDecl(c, collect)
	}
	paths[a.dstImport] = a.cfg.PkgName
	sortedPaths := make([]string, 0, len(paths))
	for p := range paths {
		sortedPaths = append(sortedPaths, p)
	}
	sort.Slice(sortedPaths, func(i, j int) bool {
		if (sortedPaths[i] == a.dstImport) != (sortedPaths[j] == a.dstImport) {
			return sortedPaths[i] == a.dstImport
		}
		return sortedPaths[i] < sortedPaths[j]
	})
	names := map[string]string{}
	used := map[string]bool{}
	for _, p := range sortedPaths {
		base := paths[p]
		name := base
		for i := 2; reserved[name] || used[name]; i++ {
			name = base + strconv.Itoa(i)
		}
		names[p] = name
		used[name] = true
	}

	// Render and group.
	type group struct {
		test       bool
		constraint string
	}
	byGroup := map[group][]*aliasCandidate{}
	constraints := map[string]bool{}
	for _, c := range cands {
		g := group{c.test, c.constraint}
		byGroup[g] = append(byGroup[g], c)
		if c.constraint != "" {
			constraints[c.constraint] = true
		}
	}
	consIndex := map[string]int{}
	var consList []string
	for c := range constraints {
		consList = append(consList, c)
	}
	sort.Strings(consList)
	for i, c := range consList {
		consIndex[c] = i + 1
	}
	var groups []group
	for g := range byGroup {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].test != groups[j].test {
			return !groups[i].test
		}
		return consIndex[groups[i].constraint] < consIndex[groups[j].constraint]
	})
	header := a.licenseHeader()
	for _, g := range groups {
		fileImports := map[string]bool{}
		q := func(p *types.Package) string {
			path := p.Path()
			if p == a.pkg {
				path = a.dstImport
			}
			fileImports[path] = true
			return names[path]
		}
		var entries []aliasEntry
		for _, c := range byGroup[g] {
			decl := a.renderDecl(c, q)
			kind := kindOf(c.obj)
			if c.asVar {
				kind = "var"
			}
			entries = append(entries, aliasEntry{Kind: kind, Old: c.obj.Name(), New: a.newName(c.obj), Test: c.test, Constraint: c.constraint, Decl: decl})
		}
		a.plan.Aliases = append(a.plan.Aliases, entries...)
		name := "zz_alias_" + a.cfg.Area
		if g.constraint != "" {
			name += fmt.Sprintf("_c%d", consIndex[g.constraint])
		}
		if g.test {
			name += "_test"
		}
		name += ".go"
		content, err := a.aliasFile(header, g.constraint, fileImports, names, paths, entries)
		if err != nil {
			return err
		}
		path := filepath.Join(a.cfg.SrcDir, name)
		if a.byPath[path] != nil {
			a.plan.errorf("%s already exists; regenerate the move from a tree without it", a.rel(path))
		}
		a.plan.AliasFiles = append(a.plan.AliasFiles, generatedFile{Path: a.rel(path), Content: content})
	}
	return nil
}

// wrapperHazard explains why calling f through a wrapper function would
// change behaviour, or returns "":
//   - f calls recover() directly: under `defer foo()` recover only works when
//     called directly by the deferred function;
//   - f (or a moved function it calls) inspects its call stack
//     (runtime.Caller/Callers, log.Output call depth, testing Helper): the
//     wrapper adds a frame, so the reported caller changes.
func (a *analysis) wrapperHazard(f *types.Func) string {
	if a.hazards == nil {
		a.computeHazards()
	}
	return a.hazards[f]
}

func (a *analysis) computeHazards() {
	a.hazards = map[*types.Func]string{}
	direct := map[*types.Func]string{}
	calls := map[*types.Func][]*types.Func{}
	var funcs []*types.Func
	for _, file := range a.files {
		if !file.Moved || !file.Included || file.XTest {
			continue
		}
		for _, d := range file.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			f, ok := a.info.Defs[fd.Name].(*types.Func)
			if !ok {
				continue
			}
			funcs = append(funcs, f)
			// recover counts only outside nested func literals.
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.FuncLit:
					return false
				case *ast.CallExpr:
					if id, ok := ast.Unparen(n.Fun).(*ast.Ident); ok {
						if b, ok := a.info.Uses[id].(*types.Builtin); ok && b.Name() == "recover" && direct[f] == "" {
							direct[f] = "calls recover() directly (a wrapper would make it return nil under defer)"
						}
					}
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var id *ast.Ident
				switch fn := ast.Unparen(call.Fun).(type) {
				case *ast.Ident:
					id = fn
				case *ast.SelectorExpr:
					id = fn.Sel
				}
				if id == nil {
					return true
				}
				callee, ok := origin(a.info.Uses[id]).(*types.Func)
				if !ok || callee.Pkg() == nil {
					return true
				}
				switch {
				case callee.Pkg().Path() == "runtime" && (callee.Name() == "Caller" || callee.Name() == "Callers"):
					if direct[f] == "" {
						direct[f] = "inspects its call stack (runtime." + callee.Name() + "; a wrapper adds a frame)"
					}
				case callee.Pkg().Path() == "log" && callee.Name() == "Output":
					if direct[f] == "" {
						direct[f] = "uses log Output call depth (a wrapper adds a frame)"
					}
				case callee.Pkg().Path() == "testing" && callee.Name() == "Helper":
					if direct[f] == "" {
						direct[f] = "calls testing Helper (a wrapper frame would be reported as the call site)"
					}
				case callee.Pkg() == a.pkg:
					calls[f] = append(calls[f], callee)
				}
				return true
			})
		}
	}
	// Stack inspection propagates to moved callers; recover does not.
	stack := map[*types.Func]string{}
	for _, f := range funcs {
		if why := direct[f]; why != "" && !strings.HasPrefix(why, "calls recover") {
			stack[f] = why
		}
	}
	for changed := true; changed; {
		changed = false
		for _, f := range funcs {
			if stack[f] != "" {
				continue
			}
			for _, g := range calls[f] {
				if why := stack[g]; why != "" {
					stack[f] = "calls " + g.Name() + ", which " + why
					changed = true
					break
				}
			}
		}
	}
	for _, f := range funcs {
		if why := firstNonEmpty(direct[f], stack[f]); why != "" {
			a.hazards[f] = why
		}
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func typeParamsOf(tn *types.TypeName) *types.TypeParamList {
	switch t := tn.Type().(type) {
	case *types.Named:
		if t.Obj() == tn {
			return t.TypeParams()
		}
	case *types.Alias:
		return t.TypeParams()
	}
	return nil
}

func (a *analysis) licenseHeader() string {
	for _, f := range a.files {
		if !f.Moved || len(f.AST.Comments) == 0 {
			continue
		}
		cg := f.AST.Comments[0]
		if cg.End() < f.AST.Package && cg != f.AST.Doc && strings.Contains(cg.Text(), "Copyright") {
			start := a.fset.Position(cg.Pos()).Offset
			end := a.fset.Position(cg.End()).Offset
			return string(f.Src[start:end]) + "\n\n"
		}
		return ""
	}
	return ""
}

func (a *analysis) aliasFile(header, constraint string, imports map[string]bool, names, realNames map[string]string, entries []aliasEntry) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(header)
	if constraint != "" {
		fmt.Fprintf(&b, "//go:build %s\n\n", constraint)
	}
	fmt.Fprintf(&b, "package %s\n\n", a.srcName)
	fmt.Fprintf(&b, "// Aliases for symbols moved to\n// %s\n", a.dstImport)
	fmt.Fprintf(&b, "// by hack/pkgmove, so existing references in package %s keep compiling.\n\n", a.srcName)
	var paths []string
	for p := range imports {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	// Standard-library imports first, then a blank line, then the rest.
	sort.SliceStable(paths, func(i, j int) bool { return isStdPath(paths[i]) && !isStdPath(paths[j]) })
	b.WriteString("import (\n")
	for i, p := range paths {
		if i > 0 && isStdPath(paths[i-1]) && !isStdPath(p) {
			b.WriteString("\n")
		}
		if names[p] == realNames[p] && realNames[p] == p[strings.LastIndex(p, "/")+1:] {
			fmt.Fprintf(&b, "\t%q\n", p)
		} else {
			fmt.Fprintf(&b, "\t%s %q\n", names[p], p)
		}
	}
	b.WriteString(")\n")
	for _, kind := range []string{"type", "const", "var"} {
		var lines []string
		for _, e := range entries {
			if e.Kind == kind {
				lines = append(lines, e.Decl)
			}
		}
		if len(lines) == 0 {
			continue
		}
		sort.Strings(lines)
		fmt.Fprintf(&b, "\n%s (\n", kind)
		for _, l := range lines {
			fmt.Fprintf(&b, "\t%s\n", l)
		}
		b.WriteString(")\n")
	}
	var funcs []string
	for _, e := range entries {
		if e.Kind == "func" {
			funcs = append(funcs, e.Decl)
		}
	}
	sort.Strings(funcs)
	for _, f := range funcs {
		fmt.Fprintf(&b, "\n%s\n", f)
	}
	out, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("formatting alias file: %v\n%s", err, b.String())
	}
	return out, nil
}

// renderDecl renders one alias declaration. For type, const and var aliases
// the result is a spec without the keyword (it goes into a block).
func (a *analysis) renderDecl(c *aliasCandidate, q func(*types.Package) string) string {
	obj := c.obj
	target := q(a.pkg) + "." + a.newName(obj)
	switch obj := obj.(type) {
	case *types.Const:
		return fmt.Sprintf("%s = %s", obj.Name(), target)
	case *types.TypeName:
		tps := typeParamsOf(obj)
		if tps == nil || tps.Len() == 0 {
			return fmt.Sprintf("%s = %s", obj.Name(), target)
		}
		decl, args := a.typeParamDecl(tps, q)
		return fmt.Sprintf("%s%s = %s[%s]", obj.Name(), decl, target, args)
	case *types.Func:
		if c.asVar {
			return fmt.Sprintf("%s = %s", obj.Name(), target)
		}
		sig := obj.Signature()
		reservedParams := map[string]bool{}
		for i := 0; i < sig.TypeParams().Len(); i++ {
			reservedParams[sig.TypeParams().At(i).Obj().Name()] = true
		}
		prefix := "p"
		for i := 0; i < sig.Params().Len(); i++ {
			if reservedParams[prefix+strconv.Itoa(i)] {
				prefix = "arg_"
			}
		}
		var params, args []string
		for i := 0; i < sig.Params().Len(); i++ {
			p := sig.Params().At(i)
			name := prefix + strconv.Itoa(i)
			if sig.Variadic() && i == sig.Params().Len()-1 {
				elem := p.Type().(*types.Slice).Elem()
				params = append(params, name+" ..."+a.typeStr(elem, q))
				args = append(args, name+"...")
			} else {
				params = append(params, name+" "+a.typeStr(p.Type(), q))
				args = append(args, name)
			}
		}
		var results []string
		for i := 0; i < sig.Results().Len(); i++ {
			results = append(results, a.typeStr(sig.Results().At(i).Type(), q))
		}
		tpDecl, tpArgs := "", ""
		if sig.TypeParams().Len() > 0 {
			tpDecl, tpArgs = a.typeParamDecl(sig.TypeParams(), q)
			tpArgs = "[" + tpArgs + "]"
		}
		res := ""
		switch len(results) {
		case 0:
		case 1:
			res = " " + results[0]
		default:
			res = " (" + strings.Join(results, ", ") + ")"
		}
		call := fmt.Sprintf("%s%s(%s)", target, tpArgs, strings.Join(args, ", "))
		body := call
		if len(results) > 0 {
			body = "return " + call
		}
		return fmt.Sprintf("func %s%s(%s)%s {\n\t%s\n}", obj.Name(), tpDecl, strings.Join(params, ", "), res, body)
	}
	return ""
}

func (a *analysis) typeParamDecl(tps *types.TypeParamList, q func(*types.Package) string) (decl, args string) {
	var ds, as []string
	for i := 0; i < tps.Len(); i++ {
		tp := tps.At(i)
		ds = append(ds, tp.Obj().Name()+" "+a.typeStr(tp.Constraint(), q))
		as = append(as, tp.Obj().Name())
	}
	return "[" + strings.Join(ds, ", ") + "]", strings.Join(as, ", ")
}

// spellable reports whether t can be written from the source package once its
// moved named types are reachable as target.Name. Unexported moved named types
// it mentions are added to need (they must be exported).
func (a *analysis) spellable(t types.Type, need map[types.Object]bool) bool {
	ok := true
	var walk func(t types.Type)
	seen := map[types.Type]bool{}
	walk = func(t types.Type) {
		if !ok || t == nil || seen[t] {
			return
		}
		seen[t] = true
		switch t := t.(type) {
		case *types.Basic, *types.TypeParam:
		case *types.Pointer:
			walk(t.Elem())
		case *types.Slice:
			walk(t.Elem())
		case *types.Array:
			walk(t.Elem())
		case *types.Map:
			walk(t.Key())
			walk(t.Elem())
		case *types.Chan:
			walk(t.Elem())
		case *types.Tuple:
			for i := 0; i < t.Len(); i++ {
				walk(t.At(i).Type())
			}
		case *types.Signature:
			if !a.spellableTypeParams(t.TypeParams(), need) {
				ok = false
				return
			}
			walk(t.Params())
			walk(t.Results())
		case *types.Struct:
			for i := 0; i < t.NumFields(); i++ {
				f := t.Field(i)
				if !f.Exported() && !f.Embedded() {
					ok = false
					return
				}
				walk(f.Type())
			}
		case *types.Interface:
			for i := 0; i < t.NumExplicitMethods(); i++ {
				m := t.ExplicitMethod(i)
				if !m.Exported() {
					ok = false
					return
				}
				walk(m.Type())
			}
			for i := 0; i < t.NumEmbeddeds(); i++ {
				walk(t.EmbeddedType(i))
			}
		case *types.Union:
			for i := 0; i < t.Len(); i++ {
				walk(t.Term(i).Type())
			}
		case *types.Named:
			ok = ok && a.namedSpellable(t.Obj(), need)
			for i := 0; i < t.TypeArgs().Len(); i++ {
				walk(t.TypeArgs().At(i))
			}
		case *types.Alias:
			ok = ok && a.namedSpellable(t.Obj(), need)
			for i := 0; i < t.TypeArgs().Len(); i++ {
				walk(t.TypeArgs().At(i))
			}
		default:
			ok = false
		}
	}
	walk(t)
	return ok
}

func (a *analysis) spellableTypeParams(tps *types.TypeParamList, need map[types.Object]bool) bool {
	if tps == nil {
		return true
	}
	for i := 0; i < tps.Len(); i++ {
		if !a.spellable(tps.At(i).Constraint(), need) {
			return false
		}
	}
	return true
}

func (a *analysis) namedSpellable(tn *types.TypeName, need map[types.Object]bool) bool {
	if tn.Pkg() == nil {
		return true // universe: error, comparable, any
	}
	if tn.Parent() != tn.Pkg().Scope() {
		return false // local type
	}
	if tn.Pkg() != a.pkg {
		return tn.Exported()
	}
	if h := a.home(tn); h == nil || !h.Moved {
		return false
	}
	if !tn.Exported() {
		need[tn] = true
	}
	return true
}

// typeStr writes t as Go source, qualifying package-level names with q and
// using post-move names for moved objects.
func (a *analysis) typeStr(t types.Type, q func(*types.Package) string) string {
	var b strings.Builder
	a.writeType(&b, t, q)
	return b.String()
}

func (a *analysis) writeType(b *strings.Builder, t types.Type, q func(*types.Package) string) {
	switch t := t.(type) {
	case *types.Basic:
		if t.Kind() == types.UnsafePointer {
			b.WriteString(q(types.Unsafe) + ".Pointer")
			return
		}
		b.WriteString(t.Name())
	case *types.TypeParam:
		b.WriteString(t.Obj().Name())
	case *types.Pointer:
		b.WriteString("*")
		a.writeType(b, t.Elem(), q)
	case *types.Slice:
		b.WriteString("[]")
		a.writeType(b, t.Elem(), q)
	case *types.Array:
		fmt.Fprintf(b, "[%d]", t.Len())
		a.writeType(b, t.Elem(), q)
	case *types.Map:
		b.WriteString("map[")
		a.writeType(b, t.Key(), q)
		b.WriteString("]")
		a.writeType(b, t.Elem(), q)
	case *types.Chan:
		switch t.Dir() {
		case types.SendRecv:
			b.WriteString("chan ")
			if c, ok := t.Elem().(*types.Chan); ok && c.Dir() == types.RecvOnly {
				b.WriteString("(")
				a.writeType(b, t.Elem(), q)
				b.WriteString(")")
				return
			}
		case types.SendOnly:
			b.WriteString("chan<- ")
		case types.RecvOnly:
			b.WriteString("<-chan ")
		}
		a.writeType(b, t.Elem(), q)
	case *types.Signature:
		b.WriteString("func")
		a.writeSig(b, t, q)
	case *types.Struct:
		b.WriteString("struct{")
		for i := 0; i < t.NumFields(); i++ {
			if i > 0 {
				b.WriteString("; ")
			}
			f := t.Field(i)
			if !f.Embedded() {
				b.WriteString(f.Name() + " ")
			}
			a.writeType(b, f.Type(), q)
			if tag := t.Tag(i); tag != "" {
				b.WriteString(" " + strconv.Quote(tag))
			}
		}
		b.WriteString("}")
	case *types.Interface:
		if t.IsImplicit() && t.NumEmbeddeds() == 1 && t.NumExplicitMethods() == 0 {
			a.writeType(b, t.EmbeddedType(0), q)
			return
		}
		b.WriteString("interface{")
		first := true
		sep := func() {
			if !first {
				b.WriteString("; ")
			}
			first = false
		}
		for i := 0; i < t.NumEmbeddeds(); i++ {
			sep()
			a.writeType(b, t.EmbeddedType(i), q)
		}
		for i := 0; i < t.NumExplicitMethods(); i++ {
			sep()
			m := t.ExplicitMethod(i)
			b.WriteString(m.Name())
			a.writeSig(b, m.Type().(*types.Signature), q)
		}
		b.WriteString("}")
	case *types.Union:
		for i := 0; i < t.Len(); i++ {
			if i > 0 {
				b.WriteString(" | ")
			}
			if t.Term(i).Tilde() {
				b.WriteString("~")
			}
			a.writeType(b, t.Term(i).Type(), q)
		}
	case *types.Named:
		a.writeName(b, t.Obj(), t.TypeArgs(), q)
	case *types.Alias:
		a.writeName(b, t.Obj(), t.TypeArgs(), q)
	case *types.Tuple:
		b.WriteString("(")
		for i := 0; i < t.Len(); i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			a.writeType(b, t.At(i).Type(), q)
		}
		b.WriteString(")")
	default:
		b.WriteString(t.String())
	}
}

func (a *analysis) writeName(b *strings.Builder, tn *types.TypeName, args *types.TypeList, q func(*types.Package) string) {
	if tn.Pkg() != nil {
		b.WriteString(q(tn.Pkg()) + ".")
	}
	if tn.Pkg() == a.pkg {
		b.WriteString(a.newName(tn))
	} else {
		b.WriteString(tn.Name())
	}
	if args.Len() > 0 {
		b.WriteString("[")
		for i := 0; i < args.Len(); i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			a.writeType(b, args.At(i), q)
		}
		b.WriteString("]")
	}
}

func (a *analysis) writeSig(b *strings.Builder, sig *types.Signature, q func(*types.Package) string) {
	b.WriteString("(")
	for i := 0; i < sig.Params().Len(); i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		p := sig.Params().At(i)
		if sig.Variadic() && i == sig.Params().Len()-1 {
			b.WriteString("...")
			a.writeType(b, p.Type().(*types.Slice).Elem(), q)
		} else {
			a.writeType(b, p.Type(), q)
		}
	}
	b.WriteString(")")
	switch sig.Results().Len() {
	case 0:
	case 1:
		b.WriteString(" ")
		a.writeType(b, sig.Results().At(0).Type(), q)
	default:
		b.WriteString(" ")
		a.writeType(b, sig.Results(), q)
	}
}
