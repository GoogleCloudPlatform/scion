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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// hubDefaultsCapturingManager records what the hub defaults looked like from
// inside Provision — i.e. on the context the production handler actually built,
// not one a test assembled.
//
// This deliberately does not extend the existing provisionCapturingManager in
// handlers_test.go: that file predates Phase 8 and this follow-up is additive
// only. Embedding mockManager (same package) gets the same behaviour without
// touching it.
type hubDefaultsCapturingManager struct {
	mockManager
	provisionCalled bool
	// seenOnContext is what api.HubAgentDefaultsFromContext returned inside
	// Provision. nil is a meaningful value here: it is what every file-mode and
	// local dispatch must produce.
	seenOnContext *api.HubAgentDefaults
}

func (m *hubDefaultsCapturingManager) Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error) {
	m.provisionCalled = true
	m.seenOnContext = api.HubAgentDefaultsFromContext(ctx)
	return &api.ScionConfig{Harness: "claude", HarnessConfig: "claude"}, nil
}

func newHubDefaultsWiringServer() (*Server, *hubDefaultsCapturingManager) {
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.ForceRuntime = "mock"

	mgr := &hubDefaultsCapturingManager{}
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}

	return New(cfg, mgr, rt), mgr
}

// postCreateAgent drives the real HTTP handler, so the request travels the
// production path: decode -> createAgent -> ctx decoration -> Manager.Provision.
func postCreateAgent(t *testing.T, srv *Server, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("want %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}
}

// TestCreateAgent_WiresHubAgentDefaultsOntoProvisionContext pins the single line
// in createAgent that joins the wire field to the provisioning context.
//
// WHY THIS EXISTS, since every tier of Gap 2c already has its own test: review
// found that deleting `ctx = withHubAgentDefaults(ctx, req.Config)` left the
// whole of pkg/runtimebroker green. The decode is tested, the context helper is
// tested, the application in provision.go is tested — and the feature was still
// inert in production, because nothing exercised the join. That is the one
// failure mode a green suite does not catch, so it gets an explicit test rather
// than being implied by the others.
//
// The assertion is made from INSIDE Provision on purpose. Reading the context
// anywhere else would test a context this test built.
func TestCreateAgent_WiresHubAgentDefaultsOntoProvisionContext(t *testing.T) {
	srv, mgr := newHubDefaultsWiringServer()

	postCreateAgent(t, srv, `{
		"name": "hubdefaults-agent",
		"id": "agent-uuid-hd-1",
		"slug": "hubdefaults-agent",
		"provisionOnly": true,
		"config": {
			"template": "claude",
			"hubAgentDefaults": {
				"maxTurns": 50,
				"maxModelCalls": 200,
				"maxDuration": "2h",
				"resources": {"limits": {"cpu": "3"}, "disk": "20Gi"}
			}
		}
	}`)

	if !mgr.provisionCalled {
		t.Fatal("Provision was never called; the test proves nothing")
	}
	hd := mgr.seenOnContext
	if hd == nil {
		t.Fatal("hub agent defaults never reached the provisioning context: " +
			"the wire field decoded but withHubAgentDefaults is not wired into createAgent")
	}
	if hd.MaxTurns != 50 {
		t.Errorf("MaxTurns: want 50, got %d", hd.MaxTurns)
	}
	if hd.MaxModelCalls != 200 {
		t.Errorf("MaxModelCalls: want 200, got %d", hd.MaxModelCalls)
	}
	if hd.MaxDuration != "2h" {
		t.Errorf("MaxDuration: want 2h, got %q", hd.MaxDuration)
	}
	if hd.Resources == nil || hd.Resources.Limits.CPU != "3" || hd.Resources.Disk != "20Gi" {
		t.Errorf("Resources: want cpu=3 disk=20Gi, got %+v", hd.Resources)
	}
}

// TestCreateAgent_NoHubAgentDefaultsLeavesContextClean is the other half of the
// same pin, and the one that keeps criterion 12 honest end to end: a create
// request without the field must leave the provisioning context with nothing on
// it, so the broker's own tiers stay in charge. This is what every file-mode
// dispatch and every old hub produces.
func TestCreateAgent_NoHubAgentDefaultsLeavesContextClean(t *testing.T) {
	srv, mgr := newHubDefaultsWiringServer()

	postCreateAgent(t, srv, `{
		"name": "nohubdefaults-agent",
		"id": "agent-uuid-hd-2",
		"slug": "nohubdefaults-agent",
		"provisionOnly": true,
		"config": {"template": "claude"}
	}`)

	if !mgr.provisionCalled {
		t.Fatal("Provision was never called; the test proves nothing")
	}
	if mgr.seenOnContext != nil {
		t.Errorf("want nothing on the provisioning context, got %+v", mgr.seenOnContext)
	}
}
