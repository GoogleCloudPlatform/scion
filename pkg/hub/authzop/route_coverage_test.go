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

package authzop

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// routePlaceholder matches "{name}" path parameter placeholders so patterns
// with different parameter names (e.g. "{id}" vs "{artifactId}") compare equal.
var routePlaceholder = regexp.MustCompile(`\{[^}]+\}`)

// normalizeRoutePattern canonicalizes path parameter names to "{}".
func normalizeRoutePattern(pattern string) string {
	return routePlaceholder.ReplaceAllString(pattern, "{}")
}

// splitRoutePattern splits a route key like "GET /api/v1/foo" into
// ("GET", "/api/v1/foo"). Keys without a method prefix return ("", key).
func splitRoutePattern(key string) (method, path string) {
	if idx := strings.Index(key, " /"); idx >= 0 {
		return key[:idx], key[idx+1:]
	}
	return "", key
}

// routeCoverageSet holds normalized entry-point patterns from the catalog,
// exemptions, and (optionally) PendingBearerDispositions for route coverage
// reconciliation.
type routeCoverageSet struct {
	// Catalog entries (always method-qualified for HTTP-like entries, plus
	// path-only for non-method-prefixed route-metadata keys).
	catalogMethodPaths map[string]bool // "METHOD /normalized/path"
	catalogPaths       map[string]bool // "/normalized/path"

	// Exemption entries.
	exemptionMethodPaths map[string]bool // "METHOD /normalized/path"
	exemptionPaths       map[string]bool // "/normalized/path" (method-less exemptions)

	// PendingBearerDispositions entries (used for sub-route coverage).
	pendingMethodPaths map[string]bool // "METHOD /normalized/path"
	pendingPaths       map[string]bool // "/normalized/path"
}

func newRouteCoverageSet(catalog []OperationSpec, exemptions []EntryPointExemption, pending []PendingEntry) routeCoverageSet {
	cov := routeCoverageSet{
		catalogMethodPaths:   make(map[string]bool),
		catalogPaths:         make(map[string]bool),
		exemptionMethodPaths: make(map[string]bool),
		exemptionPaths:       make(map[string]bool),
		pendingMethodPaths:   make(map[string]bool),
		pendingPaths:         make(map[string]bool),
	}

	for _, spec := range catalog {
		for _, ep := range spec.EntryPoints {
			norm := normalizeRoutePattern(ep.Pattern)
			cov.catalogPaths[norm] = true
			if ep.Method != "" {
				cov.catalogMethodPaths[ep.Method+" "+norm] = true
			}
		}
	}

	for _, ex := range exemptions {
		m, p := splitRoutePattern(ex.Pattern)
		norm := normalizeRoutePattern(p)
		if m != "" {
			cov.exemptionMethodPaths[m+" "+norm] = true
		} else {
			cov.exemptionPaths[norm] = true
		}
	}

	for _, pe := range pending {
		norm := normalizeRoutePattern(pe.Pattern)
		cov.pendingPaths[norm] = true
		if pe.Method != "" {
			cov.pendingMethodPaths[pe.Method+" "+norm] = true
		}
	}

	return cov
}

func (c routeCoverageSet) hasExactMethodPath(method, normPath string, includePending bool) bool {
	key := method + " " + normPath
	if c.catalogMethodPaths[key] || c.exemptionMethodPaths[key] || c.exemptionPaths[normPath] {
		return true
	}
	if includePending && c.pendingMethodPaths[key] {
		return true
	}
	return false
}

func (c routeCoverageSet) hasExactPath(normPath string, includePending bool) bool {
	if c.catalogPaths[normPath] || c.exemptionPaths[normPath] {
		return true
	}
	for mp := range c.exemptionMethodPaths {
		if _, p := splitRoutePattern(mp); p == normPath {
			return true
		}
	}
	if includePending && c.pendingPaths[normPath] {
		return true
	}
	return false
}

func (c routeCoverageSet) hasPrefixExemption(normPath string) bool {
	for p := range c.exemptionPaths {
		if p == normPath || (strings.HasSuffix(p, "/") && len(p) > 1 && strings.HasPrefix(normPath, p)) {
			return true
		}
	}
	return false
}

// mostSpecificRouteOwner returns the normalized route-metadata path that owns
// candidatePath under Go's longest-prefix ServeMux routing rules. Exact path
// matches win over trailing-slash prefixes, and longer trailing-slash prefixes
// win over shorter ones.
func mostSpecificRouteOwner(candidatePath string, exactRoutes map[string]bool, sortedPrefixes []string) string {
	if exactRoutes[candidatePath] {
		return candidatePath
	}
	for _, prefix := range sortedPrefixes {
		if strings.HasPrefix(candidatePath, prefix) {
			return prefix
		}
	}
	return ""
}

// findUncoveredRoutes returns route-metadata keys not covered by any catalog
// entry-point or exemption pattern. Used by TestEntryPointsCoverRouteMetadata
// and proof tests.
//
// Unlike naive prefix matching:
//   - A method-prefixed route-metadata key (e.g. "PUT /api/v1/github-app" or
//     "PUT /api/v1/github-app/installations/") requires a matching HTTP method
//     (or a method-less exemption on that exact pattern), so a GET entry never
//     masks a PUT or DELETE route.
//   - A trailing-slash route-metadata key (e.g. "/api/v1/artifacts/" or
//     "/api/v1/users/") is only satisfied by a pattern whose most-specific
//     owning route-metadata key is that same prefix, so a more-specific route
//     (e.g. "/api/v1/artifacts/shared/" or "/api/v1/users/me/groups") never
//     masks the parent prefix.
func findUncoveredRoutes(routeKeys map[string]bool, cov routeCoverageSet) []string {
	exactRoutes := make(map[string]bool)
	prefixSet := make(map[string]bool)
	for route := range routeKeys {
		_, pathOnly := splitRoutePattern(route)
		norm := normalizeRoutePattern(pathOnly)
		if _, hasSuffix := normalizeTrailingSlash(norm); hasSuffix {
			prefixSet[norm] = true
		} else {
			exactRoutes[norm] = true
		}
	}
	sortedPrefixes := make([]string, 0, len(prefixSet))
	for p := range prefixSet {
		sortedPrefixes = append(sortedPrefixes, p)
	}
	// Longest prefix first so mostSpecificRouteOwner finds the tightest match.
	sort.Slice(sortedPrefixes, func(i, j int) bool {
		if len(sortedPrefixes[i]) != len(sortedPrefixes[j]) {
			return len(sortedPrefixes[i]) > len(sortedPrefixes[j])
		}
		return sortedPrefixes[i] < sortedPrefixes[j]
	})

	var uncovered []string
	for route := range routeKeys {
		method, pathOnly := splitRoutePattern(route)
		normPath := normalizeRoutePattern(pathOnly)
		base, hasSuffix := normalizeTrailingSlash(normPath)

		if hasSuffix {
			if routePrefixCovered(method, base, cov, exactRoutes, sortedPrefixes) {
				continue
			}
			uncovered = append(uncovered, route)
			continue
		}

		if method != "" {
			if !cov.hasExactMethodPath(method, normPath, false) {
				uncovered = append(uncovered, route)
			}
		} else {
			if !cov.hasExactPath(normPath, false) {
				uncovered = append(uncovered, route)
			}
		}
	}
	sort.Strings(uncovered)
	return uncovered
}

func routePrefixCovered(method, base string, cov routeCoverageSet, exactRoutes map[string]bool, sortedPrefixes []string) bool {
	if method != "" {
		if cov.exemptionPaths[base] || cov.exemptionMethodPaths[method+" "+base] {
			return true
		}
		for mp := range cov.catalogMethodPaths {
			cm, cp := splitRoutePattern(mp)
			if cm != method {
				continue
			}
			if strings.HasPrefix(cp, base) && mostSpecificRouteOwner(cp, exactRoutes, sortedPrefixes) == base {
				return true
			}
		}
		for mp := range cov.exemptionMethodPaths {
			em, ep := splitRoutePattern(mp)
			if em != method {
				continue
			}
			if strings.HasPrefix(ep, base) && mostSpecificRouteOwner(ep, exactRoutes, sortedPrefixes) == base {
				return true
			}
		}
		return false
	}

	if cov.exemptionPaths[base] || cov.catalogPaths[base] {
		return true
	}
	for cp := range cov.catalogPaths {
		if strings.HasPrefix(cp, base) && mostSpecificRouteOwner(cp, exactRoutes, sortedPrefixes) == base {
			return true
		}
	}
	for ep := range cov.exemptionPaths {
		if strings.HasPrefix(ep, base) && mostSpecificRouteOwner(ep, exactRoutes, sortedPrefixes) == base {
			return true
		}
	}
	for mp := range cov.exemptionMethodPaths {
		_, ep := splitRoutePattern(mp)
		if strings.HasPrefix(ep, base) && mostSpecificRouteOwner(ep, exactRoutes, sortedPrefixes) == base {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Sub-route coverage under trailing-slash prefixes (ptone/scion#4883)
// ---------------------------------------------------------------------------

// discoveredSubRoute represents a concrete operation or sub-route dispatched
// under a trailing-slash route-metadata prefix (e.g. /api/v1/artifacts/,
// /api/v1/agents/, /api/v1/projects/).
type discoveredSubRoute struct {
	Source           string // origin table/handler for diagnostics
	Method           string // HTTP method when known, or "" for path/segment-level checks
	Pattern          string // normalized path pattern (placeholders canonicalized to "{}")
	AllowPrefix      bool   // true when any covered pattern under Pattern+"/" satisfies this entry
	AllowOpaqueChild bool   // true when Pattern+"/{}" also satisfies this entry
}

func (sr discoveredSubRoute) label() string {
	if sr.Method != "" {
		return fmt.Sprintf("[%s] %s %s", sr.Source, sr.Method, sr.Pattern)
	}
	return fmt.Sprintf("[%s] %s", sr.Source, sr.Pattern)
}

// findUncoveredSubRoutes verifies that every discovered sub-route under a
// covered prefix has its own catalog entry, exemption, or pending entry.
func findUncoveredSubRoutes(subRoutes []discoveredSubRoute, cov routeCoverageSet) []string {
	var uncovered []string
	for _, sr := range subRoutes {
		if subRouteCovered(sr, cov) {
			continue
		}
		uncovered = append(uncovered, sr.label())
	}
	sort.Strings(uncovered)
	return uncovered
}

func subRouteCovered(sr discoveredSubRoute, cov routeCoverageSet) bool {
	if cov.hasPrefixExemption(sr.Pattern) {
		return true
	}
	if sr.Method != "" {
		if cov.hasExactMethodPath(sr.Method, sr.Pattern, true) {
			return true
		}
		if sr.AllowOpaqueChild && cov.hasExactMethodPath(sr.Method, sr.Pattern+"/{}", true) {
			return true
		}
		return false
	}
	if cov.hasExactPath(sr.Pattern, true) {
		return true
	}
	if sr.AllowOpaqueChild && cov.hasExactPath(sr.Pattern+"/{}", true) {
		return true
	}
	if sr.AllowPrefix {
		prefix := sr.Pattern + "/"
		for _, set := range []map[string]bool{cov.catalogPaths, cov.exemptionPaths, cov.pendingPaths} {
			for p := range set {
				if strings.HasPrefix(p, prefix) {
					return true
				}
			}
		}
	}
	return false
}

// discoverPrefixSubRoutes enumerates concrete sub-routes dispatched under
// trailing-slash prefixes from their authoritative source definitions:
//   - pkg/artifacts/service.go ((*Service).ServeHTTP) under /api/v1/artifacts/
//   - pkg/hub/agent_routes.go (agentSubRouteTable) under /api/v1/agents/ and /api/v1/projects/{}/agents/
//   - pkg/hub/project_subroutes.go (projectSubRouteTable) under /api/v1/projects/
func discoverPrefixSubRoutes(t *testing.T, repoRoot string) []discoveredSubRoute {
	t.Helper()
	var all []discoveredSubRoute
	all = append(all, discoverArtifactSubRoutes(t, repoRoot)...)
	all = append(all, discoverAgentSubRoutes(t, repoRoot)...)
	all = append(all, discoverProjectSubRoutes(t, repoRoot)...)
	return all
}

// ---------------------------------------------------------------------------
// Artifact sub-route discovery (pkg/artifacts/service.go)
// ---------------------------------------------------------------------------

var httpMethodConstToVerb = map[string]string{
	"MethodGet":     "GET",
	"MethodPost":    "POST",
	"MethodPut":     "PUT",
	"MethodPatch":   "PATCH",
	"MethodDelete":  "DELETE",
	"MethodOptions": "OPTIONS",
}

// discoverArtifactSubRoutes AST-parses pkg/artifacts/service.go's
// (*Service).ServeHTTP switch (and 1-level s.handle* helpers in pkg/artifacts/)
// to extract every (method, normalizedPath) dispatched under /api/v1/artifacts/.
func discoverArtifactSubRoutes(t *testing.T, repoRoot string) []discoveredSubRoute {
	t.Helper()
	artDir := filepath.Join(repoRoot, "pkg/artifacts")
	entries, err := os.ReadDir(artDir)
	if err != nil {
		t.Fatalf("cannot read %s: %v", artDir, err)
	}

	fset := token.NewFileSet()
	consts := make(map[string]string)
	handlers := make(map[string]*ast.FuncDecl)
	var serveHTTP *ast.FuncDecl

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(artDir, e.Name())
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("cannot parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if i < len(vs.Values) {
							if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
								if unq, err := strconv.Unquote(lit.Value); err == nil {
									consts[name.Name] = unq
								}
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil || d.Body == nil {
					continue
				}
				if d.Name.Name == "ServeHTTP" && e.Name() == "service.go" {
					serveHTTP = d
				}
				if strings.HasPrefix(d.Name.Name, "handle") {
					handlers[d.Name.Name] = d
				}
			}
		}
	}

	if serveHTTP == nil {
		t.Fatal("could not locate (*Service).ServeHTTP in pkg/artifacts/service.go")
	}

	// Locate the main segment dispatch switch in ServeHTTP (the top-level switch
	// whose cases inspect len(segs)).
	var mainSwitch *ast.SwitchStmt
	for _, stmt := range serveHTTP.Body.List {
		if sw, ok := stmt.(*ast.SwitchStmt); ok && switchInspectsSegs(sw) {
			mainSwitch = sw
			break
		}
	}
	if mainSwitch == nil {
		t.Fatal("could not locate segment dispatch switch in (*Service).ServeHTTP")
	}

	var out []discoveredSubRoute
	extractArtifactSwitchRoutes(t, mainSwitch, nil, consts, handlers, &out)
	if len(out) == 0 {
		t.Fatal("discovered 0 artifact sub-routes from pkg/artifacts/service.go — parser is broken")
	}
	return out
}

func switchInspectsSegs(sw *ast.SwitchStmt) bool {
	if sw == nil || sw.Body == nil {
		return false
	}
	for _, s := range sw.Body.List {
		cc, ok := s.(*ast.CaseClause)
		if !ok || len(cc.List) == 0 {
			continue
		}
		found := false
		ast.Inspect(cc.List[0], func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "segs" {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

func extractArtifactSwitchRoutes(
	t *testing.T,
	sw *ast.SwitchStmt,
	inheritedSegs map[int]string,
	consts map[string]string,
	handlers map[string]*ast.FuncDecl,
	out *[]discoveredSubRoute,
) {
	t.Helper()
	for _, stmt := range sw.Body.List {
		cc, ok := stmt.(*ast.CaseClause)
		if !ok || len(cc.List) == 0 {
			continue // skip default: writeNotFound(w)
		}

		segLen, segVals := parseSegsCondition(cc.List[0], inheritedSegs, consts)
		if segLen <= 0 {
			t.Fatalf("artifact ServeHTTP case has unrecognized segment condition")
		}

		// Check if this case delegates to a nested segment switch (e.g. versions/*).
		var nestedSwitch *ast.SwitchStmt
		for _, bodyStmt := range cc.Body {
			if nsw, ok := bodyStmt.(*ast.SwitchStmt); ok && switchInspectsSegs(nsw) {
				nestedSwitch = nsw
				break
			}
		}
		if nestedSwitch != nil {
			extractArtifactSwitchRoutes(t, nestedSwitch, segVals, consts, handlers, out)
			continue
		}

		// Build normalized path: /api/v1/artifacts/{}/seg1/seg2/...
		parts := make([]string, segLen)
		parts[0] = "{}" // canonical artifact ID
		for i := 1; i < segLen; i++ {
			if v, ok := segVals[i]; ok {
				parts[i] = v
			} else {
				parts[i] = "{}"
			}
		}
		pattern := "/api/v1/artifacts/" + strings.Join(parts, "/")

		methods := extractAcceptedHTTPMethods(cc.Body)
		if len(methods) == 0 {
			// Follow 1-level s.handle* call (e.g. s.handleReviewDraft).
			for _, hName := range findCalledServiceHandlers(cc.Body) {
				if fn, ok := handlers[hName]; ok && fn.Body != nil {
					methods = append(methods, extractAcceptedHTTPMethods(fn.Body.List)...)
				}
			}
		}
		methods = uniqueSortedStrings(methods)
		if len(methods) == 0 {
			t.Fatalf("artifact ServeHTTP route %s: could not determine accepted HTTP methods", pattern)
		}
		for _, m := range methods {
			*out = append(*out, discoveredSubRoute{
				Source:  "pkg/artifacts/service.go",
				Method:  m,
				Pattern: pattern,
			})
		}
	}
}

// parseSegsCondition parses boolean conjunctions like:
//
//	len(segs) == 4 && segs[1] == reviewRequestsSegment && segs[3] == "withdraw"
//
// and returns the minimum/exact segment count and known literal segment values.
func parseSegsCondition(expr ast.Expr, inherited map[int]string, consts map[string]string) (int, map[int]string) {
	segVals := make(map[int]string, len(inherited))
	for k, v := range inherited {
		segVals[k] = v
	}
	segLen := 0

	var walk func(e ast.Expr)
	walk = func(e ast.Expr) {
		bin, ok := e.(*ast.BinaryExpr)
		if !ok {
			return
		}
		if bin.Op == token.LAND {
			walk(bin.X)
			walk(bin.Y)
			return
		}
		if bin.Op == token.EQL || bin.Op == token.GEQ {
			if isLenSegs(bin.X) {
				if n, ok := intLitValue(bin.Y); ok {
					segLen = n
				}
				return
			}
			if idx, ok := segsIndex(bin.X); ok && bin.Op == token.EQL {
				if val, ok := stringExprValue(bin.Y, consts); ok {
					segVals[idx] = val
				}
			}
		}
	}
	walk(expr)
	return segLen, segVals
}

func isLenSegs(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	fn, ok := call.Fun.(*ast.Ident)
	if !ok || fn.Name != "len" {
		return false
	}
	arg, ok := call.Args[0].(*ast.Ident)
	return ok && arg.Name == "segs"
}

func segsIndex(expr ast.Expr) (int, bool) {
	ix, ok := expr.(*ast.IndexExpr)
	if !ok {
		return 0, false
	}
	id, ok := ix.X.(*ast.Ident)
	if !ok || id.Name != "segs" {
		return 0, false
	}
	return intLitValue(ix.Index)
}

func intLitValue(expr ast.Expr) (int, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	n, err := strconv.Atoi(lit.Value)
	return n, err == nil
}

func stringExprValue(expr ast.Expr, consts map[string]string) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			s, err := strconv.Unquote(e.Value)
			return s, err == nil
		}
	case *ast.Ident:
		v, ok := consts[e.Name]
		return v, ok
	}
	return "", false
}

// extractAcceptedHTTPMethods inspects a statement slice for isRead(r.Method)
// and http.Method* references in r.Method comparisons.
func extractAcceptedHTTPMethods(stmts []ast.Stmt) []string {
	var methods []string
	for _, stmt := range stmts {
		ast.Inspect(stmt, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "isRead" {
					methods = append(methods, "GET")
				}
			case *ast.BinaryExpr:
				if node.Op != token.EQL && node.Op != token.NEQ {
					return true
				}
				if isReqMethod(node.X) {
					if m, ok := httpMethodValue(node.Y); ok {
						methods = append(methods, m)
					}
				} else if isReqMethod(node.Y) {
					if m, ok := httpMethodValue(node.X); ok {
						methods = append(methods, m)
					}
				}
			}
			return true
		})
	}
	return uniqueSortedStrings(methods)
}

func isReqMethod(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Method" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "r"
}

func httpMethodValue(expr ast.Expr) (string, bool) {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "http" {
		return "", false
	}
	verb, ok := httpMethodConstToVerb[sel.Sel.Name]
	return verb, ok
}

func findCalledServiceHandlers(stmts []ast.Stmt) []string {
	var names []string
	for _, stmt := range stmts {
		ast.Inspect(stmt, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "s" && strings.HasPrefix(sel.Sel.Name, "handle") {
				names = append(names, sel.Sel.Name)
			}
			return true
		})
	}
	return names
}

// ---------------------------------------------------------------------------
// Agent sub-route discovery (pkg/hub/agent_routes.go: agentSubRouteTable)
// ---------------------------------------------------------------------------

// discoverAgentSubRoutes AST-parses agentSubRouteTable in
// pkg/hub/agent_routes.go and emits one discoveredSubRoute per declared
// (method, path) — or one path-level entry when a row declares no ops map.
func discoverAgentSubRoutes(t *testing.T, repoRoot string) []discoveredSubRoute {
	t.Helper()
	path := filepath.Join(repoRoot, "pkg/hub/agent_routes.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("cannot parse %s: %v", path, err)
	}

	var tableLit *ast.CompositeLit
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if name.Name == "agentSubRouteTable" && i < len(vs.Values) {
					if cl, ok := vs.Values[i].(*ast.CompositeLit); ok {
						tableLit = cl
					}
				}
			}
		}
	}
	if tableLit == nil {
		t.Fatal("could not locate agentSubRouteTable in pkg/hub/agent_routes.go")
	}

	var out []discoveredSubRoute
	for _, elt := range tableLit.Elts {
		row, ok := elt.(*ast.CompositeLit)
		if !ok {
			continue
		}
		var (
			isProject bool
			noAgent   bool
			opaque    bool
			segs      []string
			methods   []string
		)
		for _, field := range row.Elts {
			kv, ok := field.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case "form":
				if id, ok := kv.Value.(*ast.Ident); ok && id.Name == "agentFormProject" {
					isProject = true
				}
			case "noAgent":
				if id, ok := kv.Value.(*ast.Ident); ok && id.Name == "true" {
					noAgent = true
				}
			case "opaque":
				if id, ok := kv.Value.(*ast.Ident); ok && id.Name == "true" {
					opaque = true
				}
			case "segs":
				if cl, ok := kv.Value.(*ast.CompositeLit); ok {
					for _, se := range cl.Elts {
						switch v := se.(type) {
						case *ast.BasicLit:
							if v.Kind == token.STRING {
								if unq, err := strconv.Unquote(v.Value); err == nil {
									segs = append(segs, unq)
								}
							}
						case *ast.Ident:
							if v.Name == "agentRouteParam" {
								segs = append(segs, "{}")
							}
						}
					}
				}
			case "ops":
				methods = extractAgentRowMethods(kv.Value)
			}
		}

		prefix := "/api/v1/agents"
		if isProject {
			prefix = "/api/v1/projects/{}/agents"
		}
		var b strings.Builder
		b.WriteString(prefix)
		if !noAgent {
			b.WriteString("/{}")
		}
		for _, s := range segs {
			b.WriteByte('/')
			b.WriteString(s)
		}
		pattern := b.String()

		if len(methods) > 0 {
			for _, m := range uniqueSortedStrings(methods) {
				out = append(out, discoveredSubRoute{
					Source:           "pkg/hub/agent_routes.go",
					Method:           m,
					Pattern:          pattern,
					AllowOpaqueChild: opaque,
				})
			}
		} else {
			out = append(out, discoveredSubRoute{
				Source:           "pkg/hub/agent_routes.go",
				Pattern:          pattern,
				AllowOpaqueChild: opaque,
			})
		}
	}

	if len(out) == 0 {
		t.Fatal("discovered 0 agent sub-routes from agentSubRouteTable — parser is broken")
	}
	return out
}

func extractAgentRowMethods(expr ast.Expr) []string {
	switch e := expr.(type) {
	case *ast.CallExpr:
		if id, ok := e.Fun.(*ast.Ident); ok {
			switch id.Name {
			case "postOp":
				return []string{"POST"}
			case "getOp":
				return []string{"GET"}
			}
		}
	case *ast.CompositeLit:
		var methods []string
		for _, elt := range e.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if m, ok := httpMethodValue(kv.Key); ok {
				methods = append(methods, m)
			}
		}
		return methods
	}
	return nil
}

// ---------------------------------------------------------------------------
// Project sub-route discovery (pkg/hub/project_subroutes.go)
// ---------------------------------------------------------------------------

// discoverProjectSubRoutes AST-parses projectSubRouteTable in
// pkg/hub/project_subroutes.go and emits the project resource path plus one
// entry per dispatched first-level segment under /api/v1/projects/{}/.
func discoverProjectSubRoutes(t *testing.T, repoRoot string) []discoveredSubRoute {
	t.Helper()
	path := filepath.Join(repoRoot, "pkg/hub/project_subroutes.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("cannot parse %s: %v", path, err)
	}

	var tableLit *ast.CompositeLit
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if name.Name == "projectSubRouteTable" && i < len(vs.Values) {
					if cl, ok := vs.Values[i].(*ast.CompositeLit); ok {
						tableLit = cl
					}
				}
			}
		}
	}
	if tableLit == nil {
		t.Fatal("could not locate projectSubRouteTable in pkg/hub/project_subroutes.go")
	}

	out := []discoveredSubRoute{
		{
			Source:  "pkg/hub/project_subroutes.go",
			Pattern: "/api/v1/projects/{}",
		},
	}
	for _, elt := range tableLit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		lit, ok := kv.Key.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		seg, err := strconv.Unquote(lit.Value)
		if err != nil || seg == "" {
			continue
		}
		out = append(out, discoveredSubRoute{
			Source:      "pkg/hub/project_subroutes.go",
			Pattern:     "/api/v1/projects/{}/" + seg,
			AllowPrefix: true,
		})
	}
	if len(out) <= 1 {
		t.Fatal("discovered 0 project sub-routes from projectSubRouteTable — parser is broken")
	}
	return out
}

func uniqueSortedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Proof tests for prefix-masking prevention (ptone/scion#4883)
// ---------------------------------------------------------------------------

// filterCatalogWithout returns a shallow copy of catalog with any EntryPoint
// matching pred removed.
func filterCatalogWithout(catalog []OperationSpec, pred func(EntryPoint) bool) []OperationSpec {
	out := make([]OperationSpec, 0, len(catalog))
	for _, spec := range catalog {
		copied := spec
		var eps []EntryPoint
		for _, ep := range spec.EntryPoints {
			if pred(ep) {
				continue
			}
			eps = append(eps, ep)
		}
		copied.EntryPoints = eps
		out = append(out, copied)
	}
	return out
}

// TestProofPrefixDoesNotMaskUncoveredArtifactOperation reproduces the
// ptone/scion#4574 / ptone/scion#4883 failure mode: keeping GET
// /api/v1/artifacts/{id} in the catalog must NOT hide a missing sibling
// operation (such as PATCH /api/v1/artifacts/{id} or POST
// /api/v1/artifacts/{id}/versions) under the /api/v1/artifacts/ prefix.
func TestProofPrefixDoesNotMaskUncoveredArtifactOperation(t *testing.T) {
	subRoutes := discoverArtifactSubRoutes(t, findRepoRoot(t))

	cases := []struct {
		name   string
		method string
		pat    string
	}{
		{"missing PATCH on /api/v1/artifacts/{}", "PATCH", "/api/v1/artifacts/{}"},
		{"missing POST on /api/v1/artifacts/{}/versions", "POST", "/api/v1/artifacts/{}/versions"},
		{"missing DELETE on /api/v1/artifacts/{}/grants/{}", "DELETE", "/api/v1/artifacts/{}/grants/{}"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reduced := filterCatalogWithout(Catalog, func(ep EntryPoint) bool {
				return ep.Method == tc.method && normalizeRoutePattern(ep.Pattern) == tc.pat
			})
			cov := newRouteCoverageSet(reduced, EntryPointExemptions, PendingBearerDispositions)
			uncovered := findUncoveredSubRoutes(subRoutes, cov)

			want := fmt.Sprintf("[pkg/artifacts/service.go] %s %s", tc.method, tc.pat)
			found := false
			for _, u := range uncovered {
				if u == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected %q to be reported uncovered, got %v", want, uncovered)
			}
		})
	}
}

// TestProofPrefixDoesNotMaskUncoveredAgentSubRouteMethod proves that covering
// GET and DELETE on /api/v1/agents/{id} does not hide a missing PATCH entry.
func TestProofPrefixDoesNotMaskUncoveredAgentSubRouteMethod(t *testing.T) {
	subRoutes := discoverAgentSubRoutes(t, findRepoRoot(t))
	reduced := filterCatalogWithout(Catalog, func(ep EntryPoint) bool {
		return ep.Method == "PATCH" && normalizeRoutePattern(ep.Pattern) == "/api/v1/agents/{}"
	})
	cov := newRouteCoverageSet(reduced, EntryPointExemptions, PendingBearerDispositions)
	uncovered := findUncoveredSubRoutes(subRoutes, cov)

	want := "[pkg/hub/agent_routes.go] PATCH /api/v1/agents/{}"
	found := false
	for _, u := range uncovered {
		if u == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected %q to be reported uncovered, got %v", want, uncovered)
	}
}

// TestProofPrefixDoesNotMaskUncoveredProjectSubRoute proves that covering
// other segments under /api/v1/projects/{id}/ does not hide an uncovered
// segment listed in projectSubRouteTable.
func TestProofPrefixDoesNotMaskUncoveredProjectSubRoute(t *testing.T) {
	subRoutes := discoverProjectSubRoutes(t, findRepoRoot(t))
	reduced := filterCatalogWithout(Catalog, func(ep EntryPoint) bool {
		n := normalizeRoutePattern(ep.Pattern)
		return n == "/api/v1/projects/{}/members" || strings.HasPrefix(n, "/api/v1/projects/{}/members/")
	})
	cov := newRouteCoverageSet(reduced, EntryPointExemptions, PendingBearerDispositions)
	uncovered := findUncoveredSubRoutes(subRoutes, cov)

	want := "[pkg/hub/project_subroutes.go] /api/v1/projects/{}/members"
	found := false
	for _, u := range uncovered {
		if u == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected %q to be reported uncovered, got %v", want, uncovered)
	}
}

// TestProofMoreSpecificRouteDoesNotMaskParentPrefix proves that a more-specific
// route-metadata key (e.g. /api/v1/artifacts/shared/ or /api/v1/users/me/groups)
// does not satisfy a shorter parent prefix in findUncoveredRoutes.
func TestProofMoreSpecificRouteDoesNotMaskParentPrefix(t *testing.T) {
	routeKeys := routeMetadataKeys(t)

	t.Run("artifacts/shared exemption does not cover artifacts/", func(t *testing.T) {
		reduced := filterCatalogWithout(Catalog, func(ep EntryPoint) bool {
			return strings.HasPrefix(ep.Pattern, "/api/v1/artifacts/")
		})
		cov := newRouteCoverageSet(reduced, EntryPointExemptions, PendingBearerDispositions)
		uncovered := findUncoveredRoutes(routeKeys, cov)

		found := false
		for _, u := range uncovered {
			if u == "/api/v1/artifacts/" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected /api/v1/artifacts/ to be reported uncovered when only /api/v1/artifacts/shared/ and /view/ are exempted, got %v", uncovered)
		}
	})

	t.Run("users/me/* routes do not cover users/", func(t *testing.T) {
		reduced := filterCatalogWithout(Catalog, func(ep EntryPoint) bool {
			return strings.HasPrefix(ep.Pattern, "/api/v1/users/") && !strings.HasPrefix(ep.Pattern, "/api/v1/users/me/")
		})
		cov := newRouteCoverageSet(reduced, EntryPointExemptions, PendingBearerDispositions)
		uncovered := findUncoveredRoutes(routeKeys, cov)

		found := false
		for _, u := range uncovered {
			if u == "/api/v1/users/" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected /api/v1/users/ to be reported uncovered when only /api/v1/users/me/* are cataloged, got %v", uncovered)
		}
	})
}

// TestProofMethodPrefixedRouteNotMaskedByOtherMethod proves that a catalog
// entry for one HTTP method (e.g. GET) does not satisfy a route-metadata key
// for a different HTTP method (e.g. PUT) on either an exact or prefix route.
func TestProofMethodPrefixedRouteNotMaskedByOtherMethod(t *testing.T) {
	routeKeys := routeMetadataKeys(t)

	t.Run("exact method-prefixed route", func(t *testing.T) {
		reduced := filterCatalogWithout(Catalog, func(ep EntryPoint) bool {
			return ep.Method == "PUT" && ep.Pattern == "/api/v1/github-app"
		})
		cov := newRouteCoverageSet(reduced, EntryPointExemptions, PendingBearerDispositions)
		uncovered := findUncoveredRoutes(routeKeys, cov)

		found := false
		for _, u := range uncovered {
			if u == "PUT /api/v1/github-app" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected PUT /api/v1/github-app to be reported uncovered, got %v", uncovered)
		}
	})

	t.Run("trailing-slash method-prefixed route", func(t *testing.T) {
		reduced := filterCatalogWithout(Catalog, func(ep EntryPoint) bool {
			return ep.Method == "PUT" && strings.HasPrefix(ep.Pattern, "/api/v1/github-app/installations/")
		})
		cov := newRouteCoverageSet(reduced, EntryPointExemptions, PendingBearerDispositions)
		uncovered := findUncoveredRoutes(routeKeys, cov)

		found := false
		for _, u := range uncovered {
			if u == "PUT /api/v1/github-app/installations/" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected PUT /api/v1/github-app/installations/ to be reported uncovered, got %v", uncovered)
		}
	})
}
