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
	"maps"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
)

func TestValidateSessionOnlyRoutes(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(m *RouteMetadata)
		want []string // substrings of the error; nil = valid
	}{
		{name: "guarded hub-admin route", mod: func(*RouteMetadata) {}},
		{name: "no session-only on a policy route", mod: func(m *RouteMetadata) {
			*m = RouteMetadata{Pattern: m.Pattern, RouteID: m.RouteID, Classification: RoutePolicy}
		}},
		{name: "policy classification", mod: func(m *RouteMetadata) { m.Classification = RoutePolicy },
			want: []string{`classification "policy"`}},
		{name: "public classification", mod: func(m *RouteMetadata) { m.Classification = RoutePublic },
			want: []string{`classification "public"`}},
		{name: "agent-token classification", mod: func(m *RouteMetadata) { m.Classification = RouteAgentToken },
			want: []string{`classification "agent-token"`}},
		{name: "no permission", mod: func(m *RouteMetadata) { m.Permission = "" },
			want: []string{"no Permission"}},
		{name: "no resource", mod: func(m *RouteMetadata) { m.Resource = "" },
			want: []string{"no Resource or Action"}},
		{name: "no action", mod: func(m *RouteMetadata) { m.Action = "" },
			want: []string{"no Resource or Action"}},
		{name: "unknown reason", mod: func(m *RouteMetadata) { m.SessionOnly = "NOT_A_REASON" },
			want: []string{`unknown session-only reason "NOT_A_REASON"`}},
		{name: "every problem reported", mod: func(m *RouteMetadata) {
			m.Classification, m.Permission, m.Resource, m.SessionOnly = RoutePolicy, "", "", "NOT_A_REASON"
		}, want: []string{"unknown session-only reason", `classification "policy"`, "no Permission", "no Resource or Action"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := guardedSessionOnly()
			tc.mod(&m)
			err := validateSessionOnlyRoutes(map[string]RouteMetadata{m.Pattern: m})
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("err = nil, want a session-only metadata error")
			}
			for _, w := range append(tc.want, `"POST /api/v1/test/op" (test.op)`) {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v; want it to contain %q", err, w)
				}
			}
		})
	}
}

// TestValidateSessionOnlyRoutesReportsEveryRoute: errors name every
// offending route, in pattern order.
func TestValidateSessionOnlyRoutesReportsEveryRoute(t *testing.T) {
	ok, b, a := guardedSessionOnly(), guardedSessionOnly(), guardedSessionOnly()
	ok.Pattern = "POST /ok"
	b.Pattern, b.Permission = "POST /b", ""
	a.Pattern, a.Classification = "POST /a", RoutePolicy
	err := validateSessionOnlyRoutes(map[string]RouteMetadata{ok.Pattern: ok, b.Pattern: b, a.Pattern: a})
	if err == nil {
		t.Fatal("err = nil")
	}
	msg := err.Error()
	ia, ib := strings.Index(msg, `"POST /a"`), strings.Index(msg, `"POST /b"`)
	if ia < 0 || ib < 0 || ia > ib || strings.Contains(msg, `"POST /ok"`) {
		t.Fatalf("err = %v; want POST /a then POST /b, and not POST /ok", msg)
	}
}

// TestRouteMetadataTableSessionOnlyRoutesAreGuarded: the route table
// passes the startup check, and it has session-only routes to check.
func TestRouteMetadataTableSessionOnlyRoutesAreGuarded(t *testing.T) {
	var sessionOnly []string
	for p, meta := range routeMetadataTable {
		if meta.SessionOnly != "" {
			sessionOnly = append(sessionOnly, p)
		}
	}
	if len(sessionOnly) == 0 {
		t.Fatal("no route sets SessionOnly; the startup check has nothing to validate")
	}
	if err := validateSessionOnlyRoutes(routeMetadataTable); err != nil {
		t.Fatal(err)
	}
}

// TestNewRefusesInvalidSessionOnlyRoute: New refuses to start when the
// route table has a session-only route the guard would not apply.
func TestNewRefusesInvalidSessionOnlyRoute(t *testing.T) {
	var patterns []string
	for p, meta := range routeMetadataTable {
		if meta.SessionOnly != "" {
			patterns = append(patterns, p)
		}
	}
	if len(patterns) == 0 {
		t.Fatal("no session-only route in the table")
	}
	sort.Strings(patterns)
	sessionOnly := patterns[0]
	for _, tc := range []struct {
		name    string
		pattern string
		mod     func(m *RouteMetadata)
	}{
		{name: "session-only route loses its permission", pattern: sessionOnly,
			mod: func(m *RouteMetadata) { m.Permission = "" }},
		{name: "session-only route reclassified as policy", pattern: sessionOnly,
			mod: func(m *RouteMetadata) { m.Classification = RoutePolicy }},
		{name: "session-only set on a public route", pattern: "/healthz",
			mod: func(m *RouteMetadata) { m.SessionOnly = authzop.ReasonHostOperations }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := maps.Clone(routeMetadataTable)
			m, ok := table[tc.pattern]
			if !ok {
				t.Fatalf("no route %q in the table", tc.pattern)
			}
			tc.mod(&m)
			table[tc.pattern] = m
			prev := startupRouteMetadata
			startupRouteMetadata = func() map[string]RouteMetadata { return table }
			t.Cleanup(func() { startupRouteMetadata = prev })

			srv, err := New(ServerConfig{}, nil)
			if err == nil || srv != nil {
				t.Fatalf("New = %v, %v; want a refusal", srv, err)
			}
			if !strings.Contains(err.Error(), "session-only") || !strings.Contains(err.Error(), tc.pattern) {
				t.Fatalf("err = %v; want it to name the session-only route %q", err, tc.pattern)
			}
		})
	}
}

func TestValidateVisitorRoutes(t *testing.T) {
	base := RouteMetadata{Pattern: "/api/v1/test/visitor", RouteID: "test.visitor",
		VisitorMethods: []string{http.MethodGet}}
	for _, tc := range []struct {
		c       RouteClassification
		allowed bool
	}{
		{RouteAuthenticated, true},
		{RoutePolicy, true},
		{RoutePublic, false},
		{RouteHubAdmin, false},
		{RouteWorkstation, false},
		{RouteBrokerHMAC, false},
		{RouteAgentToken, false},
		{RouteWebhook, false},
	} {
		t.Run(string(tc.c), func(t *testing.T) {
			m := base
			m.Classification = tc.c
			err := validateVisitorRoutes(map[string]RouteMetadata{m.Pattern: m})
			if tc.allowed {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("err = nil, want a visitor route metadata error")
			}
			for _, w := range []string{`"/api/v1/test/visitor" (test.visitor)`, "visitor methods", `classification "` + string(tc.c) + `"`} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v; want it to contain %q", err, w)
				}
			}
		})
	}
	m := base
	m.Classification, m.VisitorMethods = RouteHubAdmin, nil
	if err := validateVisitorRoutes(map[string]RouteMetadata{m.Pattern: m}); err != nil {
		t.Fatalf("a route with no visitor methods: err = %v, want nil", err)
	}
}

// TestValidateVisitorRoutesMethods: each listed method must be known,
// listed once, and served by the pattern; a method-qualified pattern serves
// only its method, and HEAD for GET.
func TestValidateVisitorRoutesMethods(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pattern string
		methods []string
		want    []string // substrings of the error; nil = valid
	}{
		{"unqualified reads", "/api/v1/test/visitor", []string{http.MethodGet, http.MethodHead}, nil},
		{"unqualified write", "/api/v1/test/visitor", []string{http.MethodPost}, nil},
		{"GET pattern with GET and HEAD", "GET /api/v1/test/visitor", []string{http.MethodGet, http.MethodHead}, nil},
		{"POST pattern with POST", "POST /api/v1/test/visitor", []string{http.MethodPost}, nil},
		{"GET pattern with POST", "GET /api/v1/test/visitor", []string{http.MethodPost},
			[]string{`method "POST" is not served by the pattern's method "GET"`}},
		{"POST pattern with GET", "POST /api/v1/test/visitor", []string{http.MethodGet},
			[]string{`method "GET" is not served by the pattern's method "POST"`}},
		{"POST pattern with HEAD", "POST /api/v1/test/visitor", []string{http.MethodHead},
			[]string{`method "HEAD" is not served by the pattern's method "POST"`}},
		{"lower-case method", "/api/v1/test/visitor", []string{"get"}, []string{`unknown method "get"`}},
		{"method listed twice", "/api/v1/test/visitor", []string{http.MethodGet, http.MethodGet},
			[]string{`method "GET" listed twice`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := RouteMetadata{Pattern: tc.pattern, RouteID: "test.visitor",
				Classification: RouteAuthenticated, VisitorMethods: tc.methods}
			err := validateVisitorRoutes(map[string]RouteMetadata{tc.pattern: m})
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("err = nil, want a visitor route metadata error")
			}
			for _, w := range append([]string{"visitor methods", `(test.visitor)`}, tc.want...) {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v; want it to contain %q", err, w)
				}
			}
		})
	}
}

// TestRouteMetadataTableVisitorRoutesAreValid: the route table passes the
// visitor route startup check.
func TestRouteMetadataTableVisitorRoutesAreValid(t *testing.T) {
	if err := validateVisitorRoutes(routeMetadataTable); err != nil {
		t.Fatal(err)
	}
}

// startNewWithRouteTable runs New with table as the startup route table
// and returns its error; New must refuse to start.
func startNewWithRouteTable(t *testing.T, table map[string]RouteMetadata) error {
	t.Helper()
	prev := startupRouteMetadata
	startupRouteMetadata = func() map[string]RouteMetadata { return table }
	t.Cleanup(func() { startupRouteMetadata = prev })

	srv, err := New(ServerConfig{}, nil)
	if err == nil || srv != nil {
		t.Fatalf("New = %v, %v; want a refusal", srv, err)
	}
	return err
}

// TestNewRefusesInvalidVisitorRoute: New refuses to start when a route the
// guard would not authorize for a visitor lists VisitorMethods.
func TestNewRefusesInvalidVisitorRoute(t *testing.T) {
	const pattern = "/healthz"
	table := maps.Clone(routeMetadataTable)
	m, ok := table[pattern]
	if !ok {
		t.Fatalf("no route %q in the table", pattern)
	}
	m.VisitorMethods = []string{http.MethodGet}
	table[pattern] = m

	err := startNewWithRouteTable(t, table)
	if !strings.Contains(err.Error(), "visitor methods") || !strings.Contains(err.Error(), pattern) {
		t.Fatalf("err = %v; want it to name the visitor route %q", err, pattern)
	}
}

// TestNewRefusesVisitorMethodNotServedByPattern: New refuses to start when
// a method-qualified authenticated or policy route lists a visitor method
// its pattern does not serve.
func TestNewRefusesVisitorMethodNotServedByPattern(t *testing.T) {
	table := maps.Clone(routeMetadataTable)
	patterns := make([]string, 0, len(table))
	for p := range table {
		patterns = append(patterns, p)
	}
	sort.Strings(patterns)
	var pattern, method string
	for _, p := range patterns {
		c := table[p].Classification
		pm, _, qualified := strings.Cut(p, " ")
		if !qualified || (c != RouteAuthenticated && c != RoutePolicy) {
			continue
		}
		pattern, method = p, http.MethodPost
		if pm == http.MethodPost {
			method = http.MethodGet
		}
		break
	}
	if pattern == "" {
		t.Fatal("no method-qualified authenticated or policy route in the table")
	}
	m := table[pattern]
	m.VisitorMethods = []string{method}
	table[pattern] = m

	err := startNewWithRouteTable(t, table)
	for _, w := range []string{"visitor methods", pattern, `method "` + method + `" is not served`} {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("err = %v; want it to contain %q", err, w)
		}
	}
}
