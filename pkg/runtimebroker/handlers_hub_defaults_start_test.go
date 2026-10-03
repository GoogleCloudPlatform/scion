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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestStartAndRestart_WireHubAgentDefaultsOntoStartContext pins the join
// between the start/restart request body's hubAgentDefaults field and the
// context Manager.Start receives. buildAgentEnv reads the hub auto-expose
// default from that context, so without the join a start would never apply
// the hub default and a change to it would not reach an agent at its next
// start.
func TestStartAndRestart_WireHubAgentDefaultsOntoStartContext(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"start", "/api/v1/agents/test-agent-1/start"},
		{"restart", "/api/v1/agents/test-agent-1/restart"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			mgr := srv.manager.(*mockManager)

			body := `{"resolvedEnv": {"FOO": "bar"}, "hubAgentDefaults": {"autoExposePorts": true}}`
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusAccepted {
				t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
			}
			if mgr.startCalls != 1 {
				t.Fatalf("expected Start to be called once, got %d", mgr.startCalls)
			}
			hd := api.HubAgentDefaultsFromContext(mgr.lastStartCtx)
			if hd == nil || hd.AutoExposePorts == nil || !*hd.AutoExposePorts {
				t.Fatalf("Start context hub defaults = %+v, want autoExposePorts=true", hd)
			}
		})
	}
}

// TestStartAndRestart_NoHubAgentDefaultsLeavesStartContextClean is the other
// half: a body without the field (every older hub) leaves nothing on the
// context, so buildAgentEnv adds no default.
func TestStartAndRestart_NoHubAgentDefaultsLeavesStartContextClean(t *testing.T) {
	for _, path := range []string{"/api/v1/agents/test-agent-1/start", "/api/v1/agents/test-agent-1/restart"} {
		srv := newTestServer(t)
		mgr := srv.manager.(*mockManager)

		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"resolvedEnv": {"FOO": "bar"}}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("%s: expected status %d, got %d: %s", path, http.StatusAccepted, w.Code, w.Body.String())
		}
		if hd := api.HubAgentDefaultsFromContext(mgr.lastStartCtx); hd != nil {
			t.Errorf("%s: want no hub defaults on the start context, got %+v", path, hd)
		}
	}
}

// TestCreateAgent_WiresHubAutoExposeDefaultOntoProvisionContext covers the
// create path for the auto-expose field: a hubAgentDefaults carrying only
// autoExposePorts is not empty and must reach the provisioning context.
func TestCreateAgent_WiresHubAutoExposeDefaultOntoProvisionContext(t *testing.T) {
	srv, mgr := newHubDefaultsWiringServer()

	postCreateAgent(t, srv, `{
		"name": "hubdefaults-ae-agent",
		"id": "agent-uuid-hd-ae",
		"slug": "hubdefaults-ae-agent",
		"provisionOnly": true,
		"config": {
			"template": "claude",
			"hubAgentDefaults": {"autoExposePorts": false}
		}
	}`)

	if !mgr.provisionCalled {
		t.Fatal("Provision was never called; the test proves nothing")
	}
	hd := mgr.seenOnContext
	if hd == nil || hd.AutoExposePorts == nil || *hd.AutoExposePorts {
		t.Fatalf("provision context hub defaults = %+v, want autoExposePorts=false", hd)
	}
}

// TestCreateAgent_FullStartSeesHubAutoExposeDefault pins the synchronous
// create path: without provisionOnly, createAgent calls Manager.Start directly,
// and the hub defaults must be on that context, where buildAgentEnv reads the
// auto-expose default.
func TestCreateAgent_FullStartSeesHubAutoExposeDefault(t *testing.T) {
	srv, mgr := newHubDefaultsWiringServer()

	postCreateAgent(t, srv, `{
		"name": "hubdefaults-ae-start",
		"id": "agent-uuid-hd-ae-start",
		"slug": "hubdefaults-ae-start",
		"config": {
			"template": "claude",
			"hubAgentDefaults": {"autoExposePorts": true}
		}
	}`)

	if mgr.startCalls != 1 {
		t.Fatalf("expected Start to be called once on the synchronous create path, got %d", mgr.startCalls)
	}
	hd := api.HubAgentDefaultsFromContext(mgr.lastStartCtx)
	if hd == nil || hd.AutoExposePorts == nil || !*hd.AutoExposePorts {
		t.Fatalf("Start context hub defaults = %+v, want autoExposePorts=true", hd)
	}
}
