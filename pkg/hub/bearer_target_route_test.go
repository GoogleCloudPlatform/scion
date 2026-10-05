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
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// routeSpecs returns the catalog operations with an HTTP, SSE or WebSocket
// entry point on the route metadata key: the same method when the key names
// one, and the same path, or a path below it for a prefix key.
func routeSpecs(key string, specs []authzop.OperationSpec) []authzop.OperationSpec {
	method, path := splitRouteKey(key)
	path = normalizeBearerPattern(path)
	prefix := strings.HasSuffix(path, "/")
	var out []authzop.OperationSpec
	for _, spec := range specs {
		for _, ep := range spec.EntryPoints {
			switch ep.Kind {
			case authzop.EntryPointHTTPRoute, authzop.EntryPointSSE, authzop.EntryPointWebSocket:
			default:
				continue
			}
			if method != "" && ep.Method != method {
				continue
			}
			p := normalizeBearerPattern(ep.Pattern)
			if p == path || (prefix && strings.HasPrefix(p, path)) {
				out = append(out, spec)
				break
			}
		}
	}
	return out
}

// admitsHub reports whether spec admits a token on the hub boundary.
func admitsHub(spec authzop.OperationSpec) bool {
	if spec.Bearer.Kind != authzop.BearerAdmit {
		return false
	}
	for _, b := range spec.Bearer.Boundaries {
		if b == authzop.BearerBoundaryHub {
			return true
		}
	}
	return false
}

// bearerTargetViolations checks the BearerTarget rule over a route table:
//
//   - BearerTarget is "", "hub_instance" or "hub_collection";
//   - it is set only on a hub-admin guard route that declares a Permission
//     on a resource other than "hub", and only when the route's catalog
//     operations admit a token on the hub boundary, each with the same
//     target resolver;
//   - "hub_collection" requires a hub collection class for the Permission;
//   - a hub-admin guard route (resource other than "hub") whose catalog
//     operations admit a token on the hub boundary sets it.
func bearerTargetViolations(table map[string]RouteMetadata, specs []authzop.OperationSpec) []string {
	var problems []string
	for key, meta := range table {
		guardRoute := meta.Classification == RouteHubAdmin && meta.Permission != "" && meta.Resource != permissions.ResourceHub
		var hubSpecs []authzop.OperationSpec
		for _, spec := range routeSpecs(key, specs) {
			if admitsHub(spec) {
				hubSpecs = append(hubSpecs, spec)
			}
		}
		switch meta.BearerTarget {
		case routeBearerTargetNone:
			if guardRoute && len(hubSpecs) > 0 {
				problems = append(problems, fmt.Sprintf("%s: catalog admits a hub token (%s) but BearerTarget is not set", key, hubSpecs[0].ID))
			}
			continue
		case routeBearerTargetHubInstance, routeBearerTargetHubCollection:
		default:
			problems = append(problems, fmt.Sprintf("%s: unknown BearerTarget %q", key, meta.BearerTarget))
			continue
		}
		if !guardRoute {
			problems = append(problems, fmt.Sprintf("%s: BearerTarget is set on a route that is not a hub-admin permission route on a non-hub resource", key))
			continue
		}
		if len(hubSpecs) == 0 {
			problems = append(problems, fmt.Sprintf("%s: BearerTarget is set but no catalog operation admits a hub token", key))
			continue
		}
		for _, spec := range hubSpecs {
			if string(spec.Bearer.Target) != meta.BearerTarget {
				problems = append(problems, fmt.Sprintf("%s: BearerTarget %q differs from %s target %q", key, meta.BearerTarget, spec.ID, spec.Bearer.Target))
			}
		}
		if meta.BearerTarget == routeBearerTargetHubCollection {
			classes, _ := permissions.CollectionTargetClassesFor(meta.Permission)
			if !classesInclude(classes, permissions.TargetClassKindHubResource, permissions.TargetClassKindGlobalCatalog) {
				problems = append(problems, fmt.Sprintf("%s: hub_collection needs a hub collection class for %s", key, meta.Permission))
			}
		}
	}
	sort.Strings(problems)
	return problems
}

// TestBearerTarget_SetOnlyForAdmittedHubOperations pins that a route
// selects a hub-level guard target exactly when its catalog operations
// admit a token on the hub boundary.
func TestBearerTarget_SetOnlyForAdmittedHubOperations(t *testing.T) {
	assert.Empty(t, bearerTargetViolations(routeMetadataTable, authzop.Catalog))
	set := 0
	for _, meta := range routeMetadataTable {
		if meta.BearerTarget != "" {
			set++
		}
	}
	t.Logf("routes with a BearerTarget: %d", set)

	quota := RouteMetadata{Pattern: "/api/v1/test/quotas", RouteID: "test.quotas", Classification: RouteHubAdmin, Permission: "quota.read", Resource: "quota", Action: "read"}
	admitted := authzop.OperationSpec{
		ID:          "test.quota.read",
		EntryPoints: []authzop.EntryPoint{{Kind: authzop.EntryPointHTTPRoute, Method: "GET", Pattern: "/api/v1/test/quotas"}},
		Bearer:      authzop.AdmitOn(authzop.BearerTargetHubInstance, authzop.BearerBoundaryHub),
	}
	withTarget := func(m RouteMetadata, target string) RouteMetadata {
		m.BearerTarget = target
		return m
	}
	cases := []struct {
		name  string
		meta  RouteMetadata
		specs []authzop.OperationSpec
		want  string
	}{
		{"set and admitted", withTarget(quota, "hub_instance"), []authzop.OperationSpec{admitted}, ""},
		{"set without an admitting operation", withTarget(quota, "hub_instance"), nil, "no catalog operation admits a hub token"},
		{"admitted but not set", quota, []authzop.OperationSpec{admitted}, "BearerTarget is not set"},
		{"resolver differs", withTarget(quota, "hub_collection"), []authzop.OperationSpec{admitted}, "differs from"},
		{"unknown value", withTarget(quota, "project"), []authzop.OperationSpec{admitted}, "unknown BearerTarget"},
		{"hub resource route", withTarget(RouteMetadata{Pattern: "/api/v1/test/quotas", Classification: RouteHubAdmin, Permission: "hub.config.read", Resource: "hub", Action: "read"}, "hub_instance"), []authzop.OperationSpec{admitted}, "not a hub-admin permission route"},
		{"collection class missing", withTarget(RouteMetadata{Pattern: "/api/v1/test/quotas", Classification: RouteHubAdmin, Permission: "agent.attach", Resource: "agent", Action: "attach"}, "hub_collection"),
			[]authzop.OperationSpec{{ID: "test.attach", EntryPoints: admitted.EntryPoints, Bearer: authzop.AdmitOn(authzop.BearerTargetHubCollection, authzop.BearerBoundaryHub)}}, "needs a hub collection class"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bearerTargetViolations(map[string]RouteMetadata{"/api/v1/test/quotas": tc.meta}, tc.specs)
			if tc.want == "" {
				assert.Empty(t, got)
				return
			}
			require.Len(t, got, 1, "%v", got)
			assert.Contains(t, got[0], tc.want)
		})
	}
}

// TestRouteGuard_BearerTargetRequiresSelector pins that a route's hub-level
// target opt-in only makes the target resolvable: a hub token without the
// route permission's selector is still denied, at the ceiling stage, while
// a session keeps its access.
func TestRouteGuard_BearerTargetRequiresSelector(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	admin := seedRoleUser(t, s, tid("routetarget-super"), store.SystemRoleSuperAdmin, true)
	token := NewScopedUserIdentityWithBoundaryAndDecoration(admin, hubBoundary(), []string{"user:read"}, tid("routetarget-cred"), bearerCeiling(t, "user:read"), nil)

	run := func(meta RouteMetadata, identity Identity) (int, bool) {
		called := false
		req := httptest.NewRequest(http.MethodGet, "/api/v1/test/route", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), identity))
		rec := httptest.NewRecorder()
		srv.routeGuard(meta, func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		})(rec, req)
		return rec.Code, called
	}

	cases := []struct {
		name string
		meta RouteMetadata
	}{
		{"hub instance", RouteMetadata{Pattern: "/api/v1/test/route", RouteID: "test.route", Classification: RouteHubAdmin, Permission: "quota.read", Resource: "quota", Action: "read", BearerTarget: "hub_instance"}},
		{"hub collection", RouteMetadata{Pattern: "/api/v1/test/route", RouteID: "test.route", Classification: RouteHubAdmin, Permission: "group.list", Resource: "group", Action: "list", BearerTarget: "hub_collection"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, called := run(tc.meta, token)
			assert.Equal(t, http.StatusForbidden, code)
			assert.False(t, called)

			target, evidence, ok := routeGuardTarget(tc.meta)
			require.True(t, ok)
			eval := srv.authzService.EvaluateBearerCeiling(ctx, principalContextForIdentity(admin), hubBoundary(), token.Ceiling(), tc.meta.Permission, target, BearerOptions{Evidence: evidence})
			assert.Equal(t, BearerStageCeiling, eval.Stage, "with the opt-in, the ceiling denies: %q", eval.Decision.Reason)

			optedOut := tc.meta
			optedOut.BearerTarget = ""
			target, evidence, ok = routeGuardTarget(optedOut)
			require.True(t, ok)
			eval = srv.authzService.EvaluateBearerCeiling(ctx, principalContextForIdentity(admin), hubBoundary(), token.Ceiling(), tc.meta.Permission, target, BearerOptions{Evidence: evidence})
			assert.Equal(t, BearerStageTargetUnknown, eval.Stage, "without the opt-in, the target is unresolvable")

			code, called = run(tc.meta, admin)
			assert.Equal(t, http.StatusOK, code, "a session keeps access through the opted-in guard")
			assert.True(t, called)
		})
	}

	bogus := RouteMetadata{Pattern: "/api/v1/test/route", RouteID: "test.route", Classification: RouteHubAdmin, Permission: "quota.read", Resource: "quota", Action: "read", BearerTarget: "project"}
	code, called := run(bogus, admin)
	assert.Equal(t, http.StatusInternalServerError, code, "an unknown BearerTarget is a misconfigured route")
	assert.False(t, called)
}
