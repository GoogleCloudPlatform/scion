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
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
)

// compressPlaceholderRE matches catalog path parameters such as {id}. It is
// declared here, not shared with web_api_compress_test.go, because that file
// is excluded from no_sqlite builds and this one is not.
var compressPlaceholderRE = regexp.MustCompile(`\{[^}]+\}`)

// compressCatalogRoute is one /api/v1/ HTTP route from the authz catalog
// (an operation entry point, an entry point exemption or a pending bearer
// entry), reduced to what the MountHubAPI compression decision sees.
type compressCatalogRoute struct {
	// key names the route in failures and in compressBypassedUntagged:
	// "METHOD pattern" for an operation entry point, "pending METHOD
	// pattern" for a pending bearer entry, and "exemption pattern" (the
	// exemption's own pattern, which may carry a method) for an exemption.
	key      string
	owner    string
	method   string
	paths    []string
	response authzop.ResponseSensitivity
}

// compressExemptionSamplePaths gives the request paths checked for an
// exemption whose pattern is a subtree ("/.../") with a single real sub-route,
// so the check uses that sub-route instead of the generic "x1" child.
var compressExemptionSamplePaths = map[string][]string{
	// handleBrokerByIDRoutes serves only POST {id}/rotate-secret.
	"/api/v1/brokers/": {"/api/v1/brokers/x1/rotate-secret"},
}

// compressBypassedUntagged lists the catalog routes that the compression
// bypass covers although their response is not tagged with a
// ResponseSensitivity. Each falls under a broad bypass rule in
// isCompressBypassPath. A route listed here that is no longer bypassed, or
// that is now tagged, fails TestCompressBypass_SensitiveResponsesFromCatalog,
// as does a bypassed, untagged route missing from this list.
var compressBypassedUntagged = map[string]string{
	// Agent item routes: every method on /api/v1/agents/{id} is bypassed; the
	// agent paths stop-all also match that rule.
	"DELETE /api/v1/agents/{id}":                        "item route",
	"DELETE /api/v1/projects/{projectId}/agents/{id}":   "item route",
	"POST /api/v1/agents/stop-all":                      "item route",
	"POST /api/v1/projects/{projectId}/agents/stop-all": "item route",

	// Agent port routes (proxy, list, register): connection routes, bypassed as
	// a whole.
	"GET /api/v1/agents/{id}/ports/{port}/proxy":           "port route",
	"POST /api/v1/agents/{id}/ports/{port}/proxy":          "port route",
	"PUT /api/v1/agents/{id}/ports/{port}/proxy":           "port route",
	"DELETE /api/v1/agents/{id}/ports/{port}/proxy":        "port route",
	"GET /api/v1/agents/{id}/ports/{port}/proxy/{subpath}": "port route",
	"GET /api/v1/agents/{id}/ports":                        "port route",
	"POST /api/v1/agents/{id}/ports":                       "port route",
	"DELETE /api/v1/agents/{id}/ports/{port}":              "port route",
	"DELETE /api/v1/agents/{id}/ports":                     "port route",

	// Env and secrets routes: every env or secrets segment is bypassed. Secret
	// responses carry metadata only, and env deletes carry no value.
	"GET /api/v1/agents/{id}/secrets":                   "env/secrets segment",
	"PUT /api/v1/agents/{id}/secrets/{key}":             "env/secrets segment",
	"DELETE /api/v1/projects/{id}/env/{key}":            "env/secrets segment",
	"GET /api/v1/projects/{id}/secrets":                 "env/secrets segment",
	"GET /api/v1/projects/{id}/secrets/{key}":           "env/secrets segment",
	"PUT /api/v1/projects/{id}/secrets/{key}":           "env/secrets segment",
	"PATCH /api/v1/projects/{id}/secrets/{key}":         "env/secrets segment",
	"DELETE /api/v1/projects/{id}/secrets/{key}":        "env/secrets segment",
	"GET /api/v1/runtime-brokers/{id}/secrets":          "env/secrets segment",
	"GET /api/v1/runtime-brokers/{id}/secrets/{key}":    "env/secrets segment",
	"DELETE /api/v1/runtime-brokers/{id}/env/{key}":     "env/secrets segment",
	"PUT /api/v1/runtime-brokers/{id}/secrets/{key}":    "env/secrets segment",
	"PATCH /api/v1/runtime-brokers/{id}/secrets/{key}":  "env/secrets segment",
	"DELETE /api/v1/runtime-brokers/{id}/secrets/{key}": "env/secrets segment",
	"GET /api/v1/secrets":                               "env/secrets segment",
	"GET /api/v1/secrets/{key}":                         "env/secrets segment",
	"PUT /api/v1/secrets/{key}":                         "env/secrets segment",
	"DELETE /api/v1/secrets/{key}":                      "env/secrets segment",
	"pending DELETE /api/v1/env/{key}":                  "env/secrets segment",
	"pending PATCH /api/v1/secrets/{key}":               "env/secrets segment",
	// The session-only user- and hub-scope variants of the same routes.
	"DELETE /api/v1/env/{key} #scope=user":     "env/secrets segment",
	"GET /api/v1/secrets #scope=user":          "env/secrets segment",
	"GET /api/v1/secrets/{key} #scope=user":    "env/secrets segment",
	"PUT /api/v1/secrets/{key} #scope=user":    "env/secrets segment",
	"PATCH /api/v1/secrets/{key} #scope=user":  "env/secrets segment",
	"DELETE /api/v1/secrets/{key} #scope=user": "env/secrets segment",
	"DELETE /api/v1/env/{key} #scope=hub":      "env/secrets segment",
	"GET /api/v1/secrets #scope=hub":           "env/secrets segment",
	"GET /api/v1/secrets/{key} #scope=hub":     "env/secrets segment",
	"PUT /api/v1/secrets/{key} #scope=hub":     "env/secrets segment",
	"PATCH /api/v1/secrets/{key} #scope=hub":   "env/secrets segment",
	"DELETE /api/v1/secrets/{key} #scope=hub":  "env/secrets segment",

	// Auth routes: /api/v1/auth/ is bypassed as a whole. Token list and get
	// return the token prefix only.
	"GET /api/v1/auth/tokens":              "auth prefix",
	"GET /api/v1/auth/tokens/{id}":         "auth prefix",
	"POST /api/v1/auth/tokens/{id}/revoke": "auth prefix",
	"DELETE /api/v1/auth/tokens/{id}":      "auth prefix",
	"POST /api/v1/auth/logout":             "auth prefix",
	"GET /api/v1/auth/me":                  "auth prefix",
	"GET /api/v1/auth/scopes":              "auth prefix",
	"GET /api/v1/auth/admin-status":        "auth prefix",
	"POST /api/v1/auth/invite/redeem":      "auth prefix",
	"exemption /api/v1/auth/validate":      "auth prefix",
	"exemption /api/v1/auth/providers":     "auth prefix",
	"exemption /api/v1/auth/cli/authorize": "auth prefix",

	// Invite routes: /api/v1/admin/invites is bypassed as a whole. Only create
	// returns the code.
	"GET /api/v1/admin/invites":              "invites prefix",
	"GET /api/v1/admin/invites/stats":        "invites prefix",
	"GET /api/v1/admin/invites/{id}":         "invites prefix",
	"POST /api/v1/admin/invites/{id}/revoke": "invites prefix",
	"DELETE /api/v1/admin/invites/{id}":      "invites prefix",

	// Test identity routes: /api/v1/test-identities is bypassed as a whole. Only
	// create, token, web login code and admin create return a credential.
	"GET /api/v1/test-identities":                    "test identities prefix",
	"DELETE /api/v1/test-identities/{id}":            "test identities prefix",
	"POST /api/v1/test-identities/cleanup":           "test identities prefix",
	"GET /api/v1/test-identities/provenance":         "test identities prefix",
	"POST /api/v1/test-identities/admin/revoke-all":  "test identities prefix",
	"GET /api/v1/test-identities/admin/{id}/residue": "test identities prefix",

	// Signed-URL routes: the workspace, artifact links, upload, download,
	// versions and resolve paths are bypassed as a whole. These carry no
	// signed URL: workspace status and finalize, the link list and revoke,
	// skill version reads and deprecation, and the per-skill resolve.
	"GET /api/v1/agents/{id}/workspace":                   "signed-URL path",
	"POST /api/v1/agents/{id}/workspace/sync-to/finalize": "signed-URL path",
	"GET /api/v1/artifacts/{id}/links":                    "signed-URL path",
	"DELETE /api/v1/artifacts/{id}/links/{linkId}":        "signed-URL path",
	"GET /api/v1/skills/{id}/resolve":                     "signed-URL path",
	"GET /api/v1/skills/{id}/versions":                    "signed-URL path",
	"GET /api/v1/skills/{id}/versions/{vid}":              "signed-URL path",
	"POST /api/v1/skills/{id}/versions/{vid}/deprecate":   "signed-URL path",

	// Artifact capability routes: the shared link answers with a redirect
	// whose Location header carries the view capability, and the view route
	// serves the artifact's own files under that capability; neither body
	// carries a credential.
	"pending GET /api/v1/artifacts/shared/{token}": "artifact capability route",
	"exemption /api/v1/artifacts/shared/":          "artifact capability route",
	"exemption /api/v1/artifacts/view/":            "artifact capability route",

	// Agent auth reset: the path ends in agents/{segment} like an agent item.
	"POST /api/v1/admin/agents/reset-auth-all": "item route",
}

// compressRequestBypassed mirrors the MountHubAPI decision: the request
// reaches the hub handler without the compression wrapper when
// isCompressBypassPath matches its path, or when it is a non-GET request to
// a create collection path (isCreateCollectionPath). method "" stands for
// every method, so it is checked as GET, the compressible case.
func compressRequestBypassed(method, path string) bool {
	if isCompressBypassPath(path) {
		return true
	}
	return method != "" && method != http.MethodGet && isCreateCollectionPath(path)
}

// compressCatalogRoutes returns every /api/v1/ HTTP route entry point,
// pending bearer entry and exemption in the authz catalog. SSE and websocket entry points are
// checked by TestCompressBypass_StreamRoutesFromCatalog.
func compressCatalogRoutes() []compressCatalogRoute {
	var routes []compressCatalogRoute
	for _, spec := range authzop.Catalog {
		for _, ep := range spec.EntryPoints {
			if ep.Kind != authzop.EntryPointHTTPRoute || !strings.HasPrefix(ep.Pattern, "/api/v1/") {
				continue
			}
			key := ep.Method + " " + ep.Pattern
			if ep.Variant != "" {
				key += " #" + ep.Variant
			}
			routes = append(routes, compressCatalogRoute{
				key:      key,
				owner:    "operation " + string(spec.ID),
				method:   ep.Method,
				paths:    []string{compressPlaceholderRE.ReplaceAllString(ep.Pattern, "x1")},
				response: ep.Response,
			})
		}
	}
	for _, pe := range authzop.PendingBearerDispositions {
		if pe.Kind != authzop.EntryPointHTTPRoute || !strings.HasPrefix(pe.Pattern, "/api/v1/") {
			continue
		}
		key := "pending " + pe.Method + " " + pe.Pattern
		if pe.Variant != "" {
			key += " #" + pe.Variant
		}
		routes = append(routes, compressCatalogRoute{
			key:      key,
			owner:    "pending entry",
			method:   pe.Method,
			paths:    []string{compressPlaceholderRE.ReplaceAllString(pe.Pattern, "x1")},
			response: pe.Response,
		})
	}
	for _, ex := range authzop.EntryPointExemptions {
		method, pattern := "", ex.Pattern
		if m, p, ok := strings.Cut(ex.Pattern, " "); ok {
			method, pattern = m, p
		}
		if !strings.HasPrefix(pattern, "/api/v1/") {
			continue
		}
		paths := compressExemptionSamplePaths[ex.Pattern]
		if paths == nil {
			sample := compressPlaceholderRE.ReplaceAllString(pattern, "x1")
			paths = []string{sample}
			if strings.HasSuffix(sample, "/") {
				paths = append(paths, sample+"x1")
			}
		}
		routes = append(routes, compressCatalogRoute{
			key:      "exemption " + ex.Pattern,
			owner:    "exemption " + ex.Pattern,
			method:   method,
			paths:    paths,
			response: ex.Response,
		})
	}
	return routes
}

// TestCompressBypass_SensitiveResponsesFromCatalog requires every authz
// catalog route whose response is tagged with a ResponseSensitivity to be
// served without the MountHubAPI compression wrapper, and every bypassed,
// untagged route to be listed in compressBypassedUntagged. A new route that
// returns a credential or caller-specific configuration is tagged in the
// catalog; this test then fails until the bypass covers it. It is a
// regression guard, not a discovery tool: it cannot find a route that nobody
// tagged, so tagging stays a manual audit of each new route's response.
func TestCompressBypass_SensitiveResponsesFromCatalog(t *testing.T) {
	routes := compressCatalogRoutes()
	seen := make(map[string]bool, len(routes))
	tagged := make(map[string]bool)
	for _, rt := range routes {
		if seen[rt.key] {
			// The same pattern can appear in more than one operation with
			// different variants; keys include the variant, so a repeat
			// means the catalog itself has a duplicate.
			t.Errorf("duplicate catalog route %s (%s)", rt.key, rt.owner)
		}
		seen[rt.key] = true

		bypassed := true
		for _, p := range rt.paths {
			if !compressRequestBypassed(rt.method, p) {
				bypassed = false
				if rt.response != "" {
					t.Errorf("%s (%s) has response sensitivity %q but %s %s is compressed; add it to isCompressBypassPath",
						rt.key, rt.owner, rt.response, methodOrAll(rt.method), p)
				}
			}
		}
		if rt.response != "" {
			tagged[rt.key] = true
			if _, listed := compressBypassedUntagged[rt.key]; listed {
				t.Errorf("%s (%s) is tagged %q; remove it from compressBypassedUntagged", rt.key, rt.owner, rt.response)
			}
			continue
		}
		_, listed := compressBypassedUntagged[rt.key]
		switch {
		case bypassed && !listed:
			t.Errorf("%s (%s) is bypassed by isCompressBypassPath but has no response sensitivity; tag it in the catalog or list it in compressBypassedUntagged",
				rt.key, rt.owner)
		case !bypassed && listed:
			t.Errorf("%s (%s) is listed in compressBypassedUntagged but is compressed; remove it from the list", rt.key, rt.owner)
		}
	}

	var stale []string
	for key := range compressBypassedUntagged {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("compressBypassedUntagged entry %q matches no catalog route; remove it", key)
	}
	for key := range compressExemptionSamplePaths {
		if !seen["exemption "+key] {
			t.Errorf("compressExemptionSamplePaths entry %q matches no exemption; remove it", key)
		}
	}

	// Guard against the iteration or the tags silently going missing.
	for _, want := range []string{
		"GET /api/v1/agents/{id}",
		"POST /api/v1/agents",
		"POST /api/v1/agents/{id}/token/refresh",
		"GET /api/v1/env",
		"exemption /api/v1/auth/token",
		"exemption POST /api/v1/agent/secrets",
		"exemption /api/v1/brokers/",
		"pending PUT /api/v1/env/{key}",
		"POST /api/v1/artifacts/{id}/links",
		"POST /api/v1/artifacts/{id}/versions/{seq}/view",
		"POST /api/v1/agents/{id}/workspace/sync-from",
		"POST /api/v1/skills/resolve #ref=skill",
		"pending POST /api/v1/skills/resolve #ref=registry",
	} {
		assert.True(t, tagged[want], "expected tagged catalog route %s is missing or untagged", want)
	}
}

func methodOrAll(method string) string {
	if method == "" {
		return "(any method)"
	}
	return method
}
