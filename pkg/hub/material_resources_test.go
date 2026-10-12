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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestMaterialResources_TypeStringsMatchRegistry pins material_resources.go's
// scope-to-resource-type mapping against the registry (ptone/scion#2129):
// the three material resource type strings match live Registry rows, and
// scopeToResourceType's four reviewed scope values map to the same resource
// type strings used elsewhere in the kernel -- including the broker rename
// (the scope string is "runtime_broker"; the registry's resource type is
// "broker"). An unrecognized scope maps to the unknown sentinel, never to
// "hub" or to one of the four reviewed classes.
func TestMaterialResources_TypeStringsMatchRegistry(t *testing.T) {
	for _, rt := range []string{"secret", "env_var", "skill_injection"} {
		found := false
		for _, p := range permissions.Registry {
			if p.Resource == rt {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("resource type %q has no live Registry permission", rt)
		}
	}

	cases := []struct{ scope, want string }{
		{store.ScopeRuntimeBroker, permissions.ResourceBroker},
		{store.ScopeProject, permissions.ResourceProject},
		{store.ScopeHub, permissions.ResourceHub},
		{store.ScopeUser, permissions.ResourceUser},
		{"bogus-scope", scopeUnknownResourceType},
		{"", scopeUnknownResourceType},
	}
	for _, c := range cases {
		if got := scopeToResourceType(c.scope); got != c.want {
			t.Errorf("scopeToResourceType(%q) = %q, want %q", c.scope, got, c.want)
		}
	}
	// Literal-pinning: the table above uses the same constants the function
	// returns, so it wouldn't catch a constant's value changing. Pin all four
	// scopes against their raw wire strings instead: a value change must not
	// silently change the wire value this test guards.
	literalCases := []struct{ scope, want string }{
		{store.ScopeRuntimeBroker, "broker"},
		{store.ScopeProject, "project"},
		{store.ScopeHub, "hub"},
		{store.ScopeUser, "user"},
	}
	for _, c := range literalCases {
		if got := scopeToResourceType(c.scope); got != c.want {
			t.Errorf("scopeToResourceType(%q) must equal the literal wire value %q, got %q",
				c.scope, c.want, got)
		}
	}
}
