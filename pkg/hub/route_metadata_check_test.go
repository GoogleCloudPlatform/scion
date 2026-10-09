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
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
)

// guardedSessionOnly is a well-formed session-only route.
func guardedSessionOnly() RouteMetadata {
	return RouteMetadata{
		Pattern: "POST /api/v1/test/op", RouteID: "test.op",
		Classification: RouteHubAdmin,
		Permission:     "hub.test.op", Resource: "hub", Action: "update",
		SessionOnly: authzop.ReasonCredentialManagement,
	}
}

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
