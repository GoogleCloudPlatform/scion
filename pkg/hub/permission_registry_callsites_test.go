//go:build !hubshard || hubshard_1

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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
)

// NonRouteUse on a registry row is documentation only. A row that has no
// Enforcement must show its check in one of two verified ways
// (ptone/scion#4064), or be Reserved, or sit in pendingNonRouteRows:
//
//   - route evidence: a routeMetadataTable entry classified RouteHubAdmin
//     whose Permission is the row ID. routeGuard decides that Permission
//     (routePermissionDecision) before the handler runs, so the entry is the
//     check itself.
//   - call-site evidence: an entry in nonRouteCallSites naming a function in
//     a pkg/hub file. The test parses that file and requires the function
//     body to reference the permission: a string literal, a pkg/hub
//     constant, or a permissions./store. constant whose value is the row's
//     ID, its UATScope or one of its AgentScopes. Agent-token checks name
//     the agent scope (ident.HasScope(ScopeX)); UAT checks name the UAT
//     scope.

// TestNonRouteCallSitesVerified pins that every nonRouteCallSites entry
// is verified against the current sources, and that the row it names
// declares NonRouteUse (an Enforcement row does not need one).
func TestNonRouteCallSitesVerified(t *testing.T) {
	ev := loadNonRouteEvidence(t)
	rows := map[string]permissions.Permission{}
	for _, p := range permissions.Registry {
		rows[p.ID] = p
	}
	for id := range nonRouteCallSites {
		if !ev.Verified[id] {
			t.Errorf("%s: call sites listed but not verified", id)
		}
		if len(rows[id].NonRouteUse) == 0 {
			t.Errorf("%s: call site entry for a row without NonRouteUse; record the check in Enforcement instead", id)
		}
	}
}

// TestVerifyCallSites_RejectsMissingReference pins the call-site verifier
// against a fixture: a reference removed, a function renamed, a constant
// reference, a scope reference and an unknown permission.
func TestVerifyCallSites_RejectsMissingReference(t *testing.T) {
	dir := t.TempDir()
	src := `package hub

const permX = "x.read"

type Server struct{}

func (s *Server) literal()  { check("x.read") }
func (s *Server) constant() { check(permX) }
func (s *Server) selector() { check(permissions.PermX) }
func scope()                { check("x:read") }
func agentScope()           { check(ScopeX) }
func none()                 { check("x.update") }
func check(string)          {}

type page[T any] struct{}
type pair[K, V any] struct{}

func (p page[T]) generic()        { check("x.read") }
func (p *pair[K, V]) genericList() { check("x.read") }
`
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := []permissions.Permission{{ID: "x.read", UATScope: "x:read", AgentScopes: []string{"project:x:read"}}}
	consts := referenceConstants{"permX": "x.read", "permissions.PermX": "x.read", "ScopeX": "project:x:read"}
	for _, fn := range []string{"Server.literal", "Server.constant", "Server.selector", "scope", "agentScope", "page.generic", "pair.genericList"} {
		got, problems := verifyCallSites(dir, registry, map[string][]permissionCallSite{"x.read": {{"x.go", fn}}}, consts)
		if !got["x.read"] || len(problems) > 0 {
			t.Errorf("%s: want verified, got %v %v", fn, got, problems)
		}
	}
	cases := []struct {
		name  string
		sites map[string][]permissionCallSite
		want  string
	}{
		{"reference removed", map[string][]permissionCallSite{"x.read": {{"x.go", "none"}}}, "does not reference the permission"},
		{"one of several sites lacks it", map[string][]permissionCallSite{"x.read": {{"x.go", "scope"}, {"x.go", "none"}}}, "x.go:none does not reference"},
		{"function renamed", map[string][]permissionCallSite{"x.read": {{"x.go", "Server.gone"}}}, "not found"},
		{"method named as func", map[string][]permissionCallSite{"x.read": {{"x.go", "literal"}}}, "not found"},
		{"generic method named as func", map[string][]permissionCallSite{"x.read": {{"x.go", "generic"}}}, "not found"},
		{"file missing", map[string][]permissionCallSite{"x.read": {{"y.go", "scope"}}}, "call site y.go"},
		{"unknown permission", map[string][]permissionCallSite{"x.delete": {{"x.go", "scope"}}}, "not in the registry"},
		{"no function", map[string][]permissionCallSite{"x.read": {}}, "names no function"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, problems := verifyCallSites(dir, registry, tc.sites, consts)
			if len(got) > 0 {
				t.Errorf("want nothing verified, got %v", got)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tc.want) {
				t.Errorf("got %v, want a problem containing %q", problems, tc.want)
			}
		})
	}
}

// TestRouteGuardPermissionIDs pins that only RouteHubAdmin routes count
// as route evidence: a RoutePolicy route's guard does not decide its
// Permission.
func TestRouteGuardPermissionIDs(t *testing.T) {
	got := routeGuardPermissionIDs(map[string]RouteMetadata{
		"a": {Classification: RouteHubAdmin, Permission: "x.read"},
		"b": {Classification: RoutePolicy, Permission: "x.update"},
		"c": {Classification: RouteAuthenticated, Permission: "x.delete"},
		"d": {Classification: RouteHubAdmin},
	})
	if len(got) != 1 || !got["x.read"] {
		t.Fatalf("got %v, want only x.read", got)
	}
}
