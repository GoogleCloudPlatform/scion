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

//go:build !no_sqlite

package hub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
)

// bearerPlaceholder matches one "{name}" pattern parameter.
var bearerPlaceholder = regexp.MustCompile(`\{[^}/]*\}`)

// normalizeBearerPattern replaces every "{name}" parameter with "{}", so
// patterns that name their parameters differently compare equal.
func normalizeBearerPattern(p string) string {
	return bearerPlaceholder.ReplaceAllString(p, "{}")
}

// splitRouteKey splits a route metadata key into its method prefix (empty
// when the key has none) and its path.
func splitRouteKey(key string) (method, path string) {
	if i := strings.Index(key, " /"); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

// bearerEntryKey identifies an entry point by method, normalized pattern
// and variant. Kind is left out so an SSE or WebSocket entry and an HTTP
// entry on the same method and pattern count as the same entry point.
type bearerEntryKey struct {
	Method  string
	Pattern string
	Variant string
}

// nonUserExemptionKinds are the entry-point exemption kinds that record a
// route as having no user bearer surface. An exemption of any other kind
// (authentication_only, hub_admin, ...) is a user surface, so its route
// needs a catalog entry point or a pending entry as well.
var nonUserExemptionKinds = map[authzop.ExemptionKind]bool{
	authzop.ExemptionPublicEndpoint: true,
	authzop.ExemptionInternalOnly:   true,
}

type bearerCoverage struct {
	catalog      map[bearerEntryKey]authzop.OperationID
	catalogPaths map[string]bool
	exemptPaths  map[string]bool
	pending      map[bearerEntryKey]bool
	pendingPaths map[string]bool
}

func newBearerCoverage() bearerCoverage {
	c := bearerCoverage{
		catalog:      map[bearerEntryKey]authzop.OperationID{},
		catalogPaths: map[string]bool{},
		exemptPaths:  map[string]bool{},
		pending:      map[bearerEntryKey]bool{},
		pendingPaths: map[string]bool{},
	}
	for _, spec := range authzop.Catalog {
		for _, ep := range spec.EntryPoints {
			n := normalizeBearerPattern(ep.Pattern)
			c.catalog[bearerEntryKey{ep.Method, n, ep.Variant}] = spec.ID
			c.catalogPaths[n] = true
		}
	}
	for _, ex := range authzop.EntryPointExemptions {
		if !nonUserExemptionKinds[ex.Kind] {
			continue
		}
		_, path := splitRouteKey(ex.Pattern)
		c.exemptPaths[normalizeBearerPattern(path)] = true
	}
	for _, pe := range authzop.PendingBearerDispositions {
		n := normalizeBearerPattern(pe.Pattern)
		c.pending[bearerEntryKey{pe.Method, n, pe.Variant}] = true
		c.pendingPaths[n] = true
	}
	return c
}

// hasPath reports whether a catalog entry point, an exemption or a pending
// entry has exactly the normalized path.
func (c bearerCoverage) hasPath(n string) bool {
	return c.catalogPaths[n] || c.exemptPaths[n] || c.pendingPaths[n]
}

// hasPrefix reports whether a catalog entry point, an exemption or a
// pending entry has a normalized path equal to prefix or below it.
func (c bearerCoverage) hasPrefix(prefix string) bool {
	for _, set := range []map[string]bool{c.catalogPaths, c.exemptPaths, c.pendingPaths} {
		for p := range set {
			if p == prefix || strings.HasPrefix(p, prefix+"/") || (strings.HasSuffix(prefix, "/") && strings.HasPrefix(p, prefix)) {
				return true
			}
		}
	}
	return false
}

// TestBearerDisposition_EveryRoutePatternCovered requires every live route
// to have a bearer record: a catalog entry point, an entry-point exemption
// of a non-user kind (nonUserExemptionKinds) or a pending entry.
//
//   - Every routeMetadataTable pattern. An exact pattern needs a record on
//     the same path (and the same method when the key names one). A prefix
//     pattern needs a record below the prefix.
//   - Every agentSubRouteTable row, by its catalog pattern (the agent
//     prefix "/api/v1/agents/" is covered row by row, not by prefix).
//   - Every projectSubRouteTable segment (the project prefix
//     "/api/v1/projects/" is covered segment by segment, plus the project
//     resource itself).
//
// Limitations:
//   - Coverage is checked per path, not per method. A route metadata key
//     without a method, every agent and project sub-route and every
//     exemption match a record on the same path whatever its method, so a
//     new method on a covered path needs a method-level check to be caught.
//   - The other prefix dispatchers (users/, groups/, templates/, skills/,
//     harness-configs/, runtime-brokers/, gcp-service-accounts/, admin/*)
//     are covered by prefix. A new action segment on one of them is caught
//     only when its pattern is added to route metadata or the catalog.
//   - Project sub-routes are enumerated by first segment only. A new
//     second-level branch inside a listed project segment (for example
//     settings/<name> or metrics/<name>) is caught only when its pattern is
//     added to route metadata or the catalog.
func TestBearerDisposition_EveryRoutePatternCovered(t *testing.T) {
	c := newBearerCoverage()
	var uncovered []string

	keys := make([]string, 0, len(routeMetadataTable))
	for k := range routeMetadataTable {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		method, path := splitRouteKey(key)
		switch {
		case path == "/api/v1/agents/" || path == "/api/v1/projects/":
			continue // covered row by row below
		case strings.HasSuffix(path, "/"):
			if !c.hasPrefix(path) {
				uncovered = append(uncovered, "route prefix "+key)
			}
		case method != "":
			n := normalizeBearerPattern(path)
			if _, ok := c.catalog[bearerEntryKey{method, n, ""}]; !ok && !c.pending[bearerEntryKey{method, n, ""}] && !c.exemptPaths[n] {
				uncovered = append(uncovered, "route "+key)
			}
		default:
			if !c.hasPath(normalizeBearerPattern(path)) {
				uncovered = append(uncovered, "route "+key)
			}
		}
	}

	for _, row := range agentSubRouteTable {
		n := normalizeBearerPattern(row.catalogPattern(false))
		if !c.hasPath(n) && !c.hasPath(n+"/{}") {
			uncovered = append(uncovered, fmt.Sprintf("agent sub-route %s (%s)", row.id, n))
		}
	}

	if !c.hasPath("/api/v1/projects/{}") {
		uncovered = append(uncovered, "project resource /api/v1/projects/{}")
	}
	for seg := range projectSubRouteTable {
		prefix := "/api/v1/projects/{}/" + seg
		if !c.hasPrefix(prefix) {
			uncovered = append(uncovered, "project sub-route segment "+seg)
		}
	}

	sort.Strings(uncovered)
	if len(uncovered) > 0 {
		t.Errorf("live routes with no catalog entry point, exemption or pending entry (%d):\n  %s",
			len(uncovered), strings.Join(uncovered, "\n  "))
	}
}

// TestBearerDisposition_AgentSubRoutesNameAnOperation requires every
// agentSubRouteTable row to name a catalog operation (ops or allMethodsOp),
// to have its catalog pattern declared as a catalog entry point (an opaque
// row also by its "/{key}" form), or to have its catalog pattern listed in
// PendingBearerDispositions. Every operation a row names must be a catalog
// operation.
func TestBearerDisposition_AgentSubRoutesNameAnOperation(t *testing.T) {
	c := newBearerCoverage()
	ids := authzop.CatalogOperationIDs()
	for _, row := range agentSubRouteTable {
		var ops []authzop.OperationID
		for _, op := range row.ops {
			ops = append(ops, op)
		}
		if row.allMethodsOp != "" {
			ops = append(ops, row.allMethodsOp)
		}
		n := normalizeBearerPattern(row.catalogPattern(false))
		catalogued := c.catalogPaths[n] || (row.opaque && c.catalogPaths[n+"/{}"])
		if len(ops) == 0 && !catalogued && !c.pendingPaths[n] && !c.pendingPaths[n+"/{}"] {
			t.Errorf("agent sub-route %s (%s) names no operation, is not a catalog entry point and is not in PendingBearerDispositions", row.id, n)
		}
		for _, op := range ops {
			if !ids[op] {
				t.Errorf("agent sub-route %s names %q, which is not a catalog operation", row.id, op)
			}
		}
	}
}

var bearerHTTPMethods = map[string]bool{
	"GET": true, "HEAD": true, "OPTIONS": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
	"MKCOL": true, "COPY": true, "MOVE": true, "PROPFIND": true, "PROPPATCH": true, "LOCK": true, "UNLOCK": true,
}

var bearerHTTPLikeKinds = map[authzop.EntryPointKind]bool{
	authzop.EntryPointHTTPRoute: true,
	authzop.EntryPointSSE:       true,
	authzop.EntryPointWebSocket: true,
}

// livePendingPath reports whether a sample path built from an HTTP-like
// pending pattern reaches a live dispatcher.
func livePendingPath(method, pattern string) bool {
	i := 0
	path := bearerPlaceholder.ReplaceAllStringFunc(pattern, func(string) string {
		i++
		return fmt.Sprintf("live-%d", i)
	})
	if isAgentSubRoutePath(path) {
		_, ok := ResolveAgentSubRoute(method, path)
		return ok
	}
	if rest, ok := strings.CutPrefix(path, "/api/v1/projects/"); ok && rest != "register" {
		_, sub, _ := strings.Cut(rest, "/")
		return projectSubRouteListed(sub)
	}
	if _, ok := routeMetadataTable[path]; ok {
		return true
	}
	if _, ok := routeMetadataTable[method+" "+path]; ok {
		return true
	}
	for key := range routeMetadataTable {
		_, kp := splitRouteKey(key)
		if strings.HasSuffix(kp, "/") && strings.HasPrefix(path, kp) && len(path) > len(kp) {
			return true
		}
	}
	return false
}

// hubFunctionDeclared reports whether a function or method named name is
// declared in a non-test Go file of this package.
func hubFunctionDeclared(t *testing.T, name string) bool {
	t.Helper()
	re := regexp.MustCompile(`(?m)^func (\([^)]*\) )?` + regexp.QuoteMeta(name) + `\(`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing package files: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		if re.Match(src) {
			return true
		}
	}
	return false
}

// pendingEntryProblems returns every reason a pending entry is not
// acceptable: an invalid kind, method or area, a duplicate, an entry point
// that is already catalogued, or one that does not name a live entry point.
func pendingEntryProblems(t *testing.T, entries []authzop.PendingEntry) []string {
	t.Helper()
	c := newBearerCoverage()
	seen := map[bearerEntryKey]bool{}
	var problems []string
	for _, pe := range entries {
		label := fmt.Sprintf("%s %s %s", pe.Kind, pe.Method, pe.Pattern)
		if pe.Variant != "" {
			label += " [" + pe.Variant + "]"
		}
		if !authzop.ValidAreas[pe.Area] {
			problems = append(problems, label+": unknown area "+string(pe.Area))
		}
		if pe.Pattern == "" {
			problems = append(problems, label+": pattern is required")
			continue
		}
		key := bearerEntryKey{pe.Method, normalizeBearerPattern(pe.Pattern), pe.Variant}
		if seen[key] {
			problems = append(problems, label+": listed twice")
		}
		seen[key] = true
		if op, ok := c.catalog[key]; ok {
			problems = append(problems, label+": already catalogued by "+string(op))
		}

		if bearerHTTPLikeKinds[pe.Kind] {
			if !bearerHTTPMethods[pe.Method] {
				problems = append(problems, label+": unknown HTTP method")
				continue
			}
			if !livePendingPath(pe.Method, pe.Pattern) {
				problems = append(problems, label+": does not reach a live route")
			}
			continue
		}
		switch pe.Kind {
		case authzop.EntryPointSchedulerJob, authzop.EntryPointBrokerCall, authzop.EntryPointBackgroundJob, authzop.EntryPointInternalDispatch:
		default:
			problems = append(problems, label+": unknown kind")
			continue
		}
		if pe.Method != "" {
			problems = append(problems, label+": method must be empty for a non-HTTP entry point")
		}
		fn, _, ok := strings.Cut(pe.Pattern, ":")
		if !ok || !hubFunctionDeclared(t, fn) {
			problems = append(problems, label+": pattern must be <function>:<detail> naming a function declared in pkg/hub")
		}
	}
	return problems
}

// TestBearerDisposition_PendingEntriesAreLiveAndUncatalogued requires every
// PendingBearerDispositions entry to name a live entry point that is not
// catalogued. A stale entry fails, and so does one that is also
// catalogued.
func TestBearerDisposition_PendingEntriesAreLiveAndUncatalogued(t *testing.T) {
	if len(authzop.PendingBearerDispositions) == 0 {
		t.Log("PendingBearerDispositions is empty")
	}
	for _, p := range pendingEntryProblems(t, authzop.PendingBearerDispositions) {
		t.Error(p)
	}

	// The same rule rejects each kind of bad entry.
	bad := []struct {
		name  string
		entry authzop.PendingEntry
	}{
		{"stale project sub-route", authzop.PendingEntry{Kind: authzop.EntryPointHTTPRoute, Method: "GET", Pattern: "/api/v1/projects/{id}/not-a-route", Area: authzop.AreaProjects}},
		{"stale agent sub-route", authzop.PendingEntry{Kind: authzop.EntryPointHTTPRoute, Method: "GET", Pattern: "/api/v1/agents/{id}/not-a-route", Area: authzop.AreaAgents}},
		{"stale top-level route", authzop.PendingEntry{Kind: authzop.EntryPointHTTPRoute, Method: "GET", Pattern: "/api/v1/not-a-route", Area: authzop.AreaHub}},
		{"stale scheduler job", authzop.PendingEntry{Kind: authzop.EntryPointSchedulerJob, Pattern: "noSuchSchedulerFunction:message", Area: authzop.AreaProjects}},
		{"catalogued entry", authzop.PendingEntry{Kind: authzop.EntryPointHTTPRoute, Method: "GET", Pattern: "/api/v1/agents/{agentId}", Area: authzop.AreaAgents}},
		{"unknown area", authzop.PendingEntry{Kind: authzop.EntryPointHTTPRoute, Method: "GET", Pattern: "/api/v1/agents/{id}/logs", Area: "elsewhere"}},
		{"unknown method", authzop.PendingEntry{Kind: authzop.EntryPointHTTPRoute, Method: "FETCH", Pattern: "/api/v1/agents/{id}/logs", Area: authzop.AreaAgents}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if p := pendingEntryProblems(t, []authzop.PendingEntry{tc.entry}); len(p) == 0 {
				t.Errorf("entry %+v must be rejected", tc.entry)
			}
		})
	}
	t.Run("duplicate", func(t *testing.T) {
		e := authzop.PendingEntry{Kind: authzop.EntryPointHTTPRoute, Method: "GET", Pattern: "/api/v1/agents/{id}/logs", Area: authzop.AreaAgents}
		if p := pendingEntryProblems(t, []authzop.PendingEntry{e, e}); len(p) == 0 {
			t.Error("a duplicate entry must be rejected")
		}
	})
}

// TestProjectSubRoutes_UnlistedSegmentIsNotFound requires handleProjectRoutes
// to answer 404 for a first segment after the project ID that is not in
// projectSubRouteTable, and to dispatch every listed segment.
func TestProjectSubRoutes_UnlistedSegmentIsNotFound(t *testing.T) {
	srv, _ := testServer(t)
	const unlisted = "Project route not found"

	for _, sub := range []string{"bogus", "envX", "membersX", "davX", "secretsX", "settingsX", "metricsX/summary", "sync-status"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			path := "/api/v1/projects/proj-unlisted/" + sub
			rec := doRequest(t, srv, method, path, nil)
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), unlisted) {
				t.Errorf("%s %s: got %d %s, want 404 %q", method, path, rec.Code, rec.Body.String(), unlisted)
			}
		}
	}

	for seg := range projectSubRouteTable {
		path := "/api/v1/projects/proj-unlisted/" + seg
		rec := doRequest(t, srv, http.MethodGet, path, nil)
		if strings.Contains(rec.Body.String(), unlisted) {
			t.Errorf("GET %s: listed segment answered as unlisted", path)
		}
	}
}

// projectRecordedPaths returns every HTTP-like catalog and pending entry
// point under /api/v1/projects/{id}/, as method and pattern pairs.
func projectRecordedPaths() [][2]string {
	var out [][2]string
	add := func(kind authzop.EntryPointKind, method, pattern string) {
		if !bearerHTTPLikeKinds[kind] {
			return
		}
		rest, ok := strings.CutPrefix(pattern, "/api/v1/projects/{")
		if !ok {
			return
		}
		if _, sub, ok := strings.Cut(rest, "}/"); !ok || sub == "" {
			return
		}
		out = append(out, [2]string{method, pattern})
	}
	for _, spec := range authzop.Catalog {
		for _, ep := range spec.EntryPoints {
			add(ep.Kind, ep.Method, ep.Pattern)
		}
	}
	for _, pe := range authzop.PendingBearerDispositions {
		add(pe.Kind, pe.Method, pe.Pattern)
	}
	return out
}

// TestProjectSubRoutes_EveryRecordedPathReachesItsBranch requires every
// catalog and pending entry point under /api/v1/projects/{id}/ to reach a
// dispatch branch in handleProjectRoutes: sent through the server with
// its own method, the response is neither the unlisted-segment 404 nor
// the project resource 404 that a sub-path with no dispatch branch gets.
// Together with TestBearerDisposition_EveryRoutePatternCovered this keeps
// projectSubRouteTable and the dispatcher equal.
func TestProjectSubRoutes_EveryRecordedPathReachesItsBranch(t *testing.T) {
	srv, _ := testServer(t)
	notDispatched := []string{"Project route not found", "Project resource not found"}

	paths := projectRecordedPaths()
	if len(paths) == 0 {
		t.Fatal("no catalog or pending entry point under /api/v1/projects/{id}/")
	}
	for _, mp := range paths {
		method, pattern := mp[0], mp[1]
		i := 0
		path := bearerPlaceholder.ReplaceAllStringFunc(pattern, func(string) string {
			i++
			return fmt.Sprintf("live-%d", i)
		})
		// Streaming handlers hold the request open; the deadline ends them.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req := httptest.NewRequest(method, path, nil).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		cancel()
		body := rec.Body.String()
		for _, msg := range notDispatched {
			if rec.Code == http.StatusNotFound && strings.Contains(body, msg) {
				t.Errorf("%s %s: got 404 %q; the pattern reaches no dispatch branch in handleProjectRoutes", method, pattern, msg)
			}
		}
	}
}
