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

package runtimebroker

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// flatCredMarker is the unique marker planted in every content field of the
// fixture credentials file.
const flatCredMarker = "zq7credmarker4f2c"

// flatCredContents writes a fixture credentials file whose content fields
// each carry flatCredMarker and returns its path and the content values.
func flatCredContents(t *testing.T) (path string, values []string) {
	t.Helper()
	values = []string{
		flatCredMarker + "privatekeybody",
		flatCredMarker + "keyid",
		flatCredMarker + "clientid",
		flatCredMarker + "-broker@example-project.iam.gserviceaccount.com",
	}
	b, err := json.Marshal(map[string]string{
		"type": "service_account", "private_key": values[0], "private_key_id": values[1],
		"client_id": values[2], "client_email": values[3], "token_uri": "https://oauth2.googleapis.com/token",
	})
	require.NoError(t, err)
	path = filepath.Join(t.TempDir(), "cr-credentials.json")
	require.NoError(t, os.WriteFile(path, b, 0o600))
	return path, values
}

// credentialHits names every needle found in body: the marker, each content
// value, and the credentials-file path.
func credentialHits(body string, needles []string) []string {
	var hits []string
	for _, n := range needles {
		if n != "" && strings.Contains(body, n) {
			hits = append(hits, n)
		}
	}
	return hits
}

// flatRouteCheck is one request the coverage test sends, for one pattern
// registered in (*Server).registerRoutes.
type flatRouteCheck struct {
	pattern string // the registered pattern
	method  string
	path    string // the request path (and query)
	// data: the route answers 200 with a non-empty body in-process, which
	// the test asserts before it looks for the marker. Otherwise it is
	// method-only here: reason says why, and wantCode pins its status.
	data     bool
	wantCode int
	reason   string
	// contains, for a data route, must appear in the body (proof that the
	// route rendered this instance's data).
	contains string
}

// flatCredSlug is the seeded agent's slug: the fixture instance and the agent
// routes both use it.
const flatCredSlug = "worker"

// flatRouteChecks covers every pattern registerRoutes registers
// (requireRouteSetMatchesServer keeps the two equal).
func flatRouteChecks() []flatRouteCheck {
	return []flatRouteCheck{
		// /healthz has a fixed shape and renders no instance data; it is checked for status only.
		{pattern: "/healthz", method: http.MethodGet, path: "/healthz", data: true},
		// GET data routes that render this instance's data.
		{pattern: "/api/v1/info", method: http.MethodGet, path: "/api/v1/info", data: true, contains: crOwnBroker},
		{pattern: "/api/v1/agents", method: http.MethodGet, path: "/api/v1/agents", data: true, contains: flatCredSlug},
		{pattern: "/api/v1/agents/", method: http.MethodGet, path: "/api/v1/agents/" + flatCredSlug + "?projectId=" + crOwnProject, data: true, contains: flatCredSlug},
		// GET data routes with a fixed shape and no instance data in this
		// fixture (no hub connection is registered; image status reports
		// existence only). They must still answer 200 with a non-empty body.
		{pattern: "/readyz", method: http.MethodGet, path: "/readyz", data: true},
		{pattern: "/api/v1/hub-connections", method: http.MethodGet, path: "/api/v1/hub-connections", data: true},
		{pattern: "/api/v1/images/status", method: http.MethodGet, path: "/api/v1/images/status?short=example-agent-image", data: true},
		{pattern: "/api/v1/projects/", method: http.MethodGet, path: "/api/v1/projects/" + crOwnProject, wantCode: http.StatusMethodNotAllowed,
			reason: "DELETE only (it deletes the project's files); it serves no GET data"},
		{pattern: "/api/v1/images/pull", method: http.MethodGet, path: "/api/v1/images/pull", wantCode: http.StatusMethodNotAllowed,
			reason: "POST only (it pulls an image); it serves no GET data"},
		{pattern: "/api/v1/images/local", method: http.MethodGet, path: "/api/v1/images/local", wantCode: http.StatusMethodNotAllowed,
			reason: "DELETE only (it removes a local image); it serves no GET data"},
		{pattern: "/api/v1/workspace/upload", method: http.MethodGet, path: "/api/v1/workspace/upload", wantCode: http.StatusMethodNotAllowed,
			reason: "POST only (it needs a Hub workspace transfer request); it serves no GET data"},
		{pattern: "/api/v1/workspace/apply", method: http.MethodGet, path: "/api/v1/workspace/apply", wantCode: http.StatusMethodNotAllowed,
			reason: "POST only (it needs a Hub workspace transfer request); it serves no GET data"},
	}
}

// TestFlatCloudRun_CredentialContentsNeverInAPI (amendment P5-CR,
// E-8; the Runtime Broker half of the row, with the cmd test of the same name
// covering presentation, health and the identity file): with a fixture
// credentials file whose content fields carry a unique marker, and one owned
// agent instance on the scope, no body of any GET data route a flat cloudrun
// Runtime Broker serves, and no heartbeat it builds, contains the marker, any
// content value or the file's path. Each GET data route must first answer
// 200 with a non-empty body (the agent routes must show the agent), so the
// check never runs on an empty or error body. The routes that serve no
// GET data are listed by name with their reason and pinned to their status,
// and the route table is checked against the registered patterns first.
// Response bodies of POST and DELETE operations (for example agent create)
// are outside this test.
func TestFlatCloudRun_CredentialContentsNeverInAPI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// The route table covers exactly the registered patterns.
	requireRouteSetMatchesServer(t)
	path, values := flatCredContents(t)
	needles := append([]string{flatCredMarker, path, filepath.Dir(path)}, values...)

	// Control: the checker reports a body that carries the marker, so a
	// body that held one would fail this test.
	require.NotEmpty(t, credentialHits(`{"name":"x","email":"`+values[3]+`"}`, needles), "control: the checker must see a planted value")

	cfg := testServerConfig()
	cfg.Host, cfg.Port = "127.0.0.1", 0
	cfg.BrokerID = crOwnBroker
	cfg.StateDir = t.TempDir()
	cfg.FlatInstance = &FlatInstanceConfig{
		Identity: &brokeridentity.Identity{InstanceKey: "cr-a", RuntimeBrokerID: crOwnBroker,
			RuntimeTarget: api.RuntimeTargetDescriptor{ID: "target-1", Type: brokeridentity.TargetTypeCloudRun, DisplayName: "Cloud Run"}},
		Instance: config.V1RuntimeBrokerInstanceConfig{Key: "cr-a", Name: "cr-a", RuntimeTarget: &config.V1RuntimeTargetConfig{
			Type: config.RuntimeTargetTypeCloudRun, DisplayName: "Cloud Run", ProjectID: "example-project", Region: "us-central1",
			ServiceAccount: "agents@example-project.iam.gserviceaccount.com", Subnetwork: "agents-subnet", CredentialsFile: path,
		}},
		HubInProcess: true,
	}
	// One owned agent instance, so the agent routes render agent data.
	rt := crOwnRuntime(t, &crInstances{instances: []*runpb.Instance{
		crOwnedInstance(flatCredSlug+"-0123456789", flatCredSlug, "0f1e2d3c-4b5a-4968-8776-655443322110"),
	}})
	srv := New(cfg, agent.NewManager(rt), rt, config.RuntimeBuildRecord{})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	h := srv.Handler()
	dataRoutes := 0
	for _, c := range flatRouteChecks() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		body, err := io.ReadAll(rec.Result().Body)
		require.NoError(t, err)
		if c.data {
			dataRoutes++
			require.Equal(t, http.StatusOK, rec.Code, "%s %s must answer 200 (body %q)", c.method, c.path, string(body))
			require.NotEmpty(t, strings.TrimSpace(string(body)), "%s %s must answer a non-empty body", c.method, c.path)
			if c.contains != "" {
				require.Contains(t, string(body), c.contains, "%s %s must contain %q", c.method, c.path, c.contains)
			}
		} else {
			require.NotEmpty(t, c.reason, "%s: a route without a GET data check needs its reason", c.pattern)
			require.Equal(t, c.wantCode, rec.Code, "%s %s: %s", c.method, c.path, c.reason)
		}
		assert.Empty(t, credentialHits(string(body), needles), "%s %s (status %d)", c.method, c.path, rec.Code)
	}
	require.Equal(t, 7, dataRoutes, "the GET data routes checked")

	// The heartbeat the flat server builds (it carries the FC4-a
	// presentation).
	hb := srv.newHeartbeatService(&mockRuntimeBrokerService{}, crOwnBroker, "", time.Hour).buildHeartbeat(context.Background())
	require.NotNil(t, hb.RuntimeTargetPresentation, "a flat heartbeat carries the presentation")
	b, err := json.Marshal(hb)
	require.NoError(t, err)
	assert.Empty(t, credentialHits(string(b), needles), "heartbeat body")
}

// requireRouteSetMatchesServer keeps the coverage test's route set equal to
// the patterns (*Server).registerRoutes registers: it parses server.go and
// collects the string-literal first argument of any <x>.mux.Handle or
// <x>.mux.HandleFunc call inside registerRoutes. A parse that finds no
// pattern fails.
// Routes registered outside registerRoutes are out of its scope.
func requireRouteSetMatchesServer(t *testing.T) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "server.go", nil, 0)
	require.NoError(t, err)
	var registered []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "registerRoutes" || fn.Recv == nil || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
				return true
			}
			if mux, ok := sel.X.(*ast.SelectorExpr); !ok || mux.Sel.Name != "mux" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			require.True(t, ok && lit.Kind == token.STRING, "registerRoutes: a %s pattern that is not a string literal", sel.Sel.Name)
			v, err := strconv.Unquote(lit.Value)
			require.NoError(t, err)
			registered = append(registered, v)
			return true
		})
	}
	require.NotEmpty(t, registered, "no s.mux.HandleFunc or s.mux.Handle pattern found in registerRoutes: the guard would pass vacuously")
	var tested []string
	for _, c := range flatRouteChecks() {
		tested = append(tested, c.pattern)
	}
	sort.Strings(registered)
	sort.Strings(tested)
	require.Equal(t, registered, tested, "every registered pattern is covered by TestFlatCloudRun_CredentialContentsNeverInAPI, and nothing else")
}
