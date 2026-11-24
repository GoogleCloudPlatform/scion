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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/firewallpolicy"
)

func TestHandlers_InitFirewallTopLevelUnion(t *testing.T) {
	brokerPolicy, err := firewallpolicy.Normalize([]string{"pypi.org", "shared.example.com"}, false)
	if err != nil {
		t.Fatalf("Normalize broker: %v", err)
	}
	wantUnion := firewallpolicy.Policy{
		AllowDomains: []string{"artifactory.corp.example", "pypi.org", "shared.example.com"},
		AllowSSH:     true,
	}

	paths := []struct {
		name string
		path string
		body string
	}{
		{
			name: "create",
			path: "/api/v1/agents",
			body: `{
				"name": "fw-agent",
				"config": {"template": "claude"},
				"initFirewall": {"allowDomains": ["Artifactory.Corp.Example.", "shared.example.com"], "allowSsh": true}
			}`,
		},
		{
			name: "start",
			path: "/api/v1/agents/test-agent-1/start",
			body: `{
				"initFirewall": {"allowDomains": ["Artifactory.Corp.Example.", "shared.example.com"], "allowSsh": true}
			}`,
		},
		{
			name: "restart",
			path: "/api/v1/agents/test-agent-1/restart",
			body: `{
				"initFirewall": {"allowDomains": ["Artifactory.Corp.Example.", "shared.example.com"], "allowSsh": true}
			}`,
		},
	}

	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			srv.config.InitFirewall = brokerPolicy
			mgr := srv.manager.(*mockManager)

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusOK && w.Code != http.StatusCreated && w.Code != http.StatusAccepted {
				t.Fatalf("%s: expected 200/201/202, got %d: %s", tc.name, w.Code, w.Body.String())
			}
			if mgr.startCalls != 1 {
				t.Fatalf("%s: startCalls = %d, want 1", tc.name, mgr.startCalls)
			}
			if !reflect.DeepEqual(mgr.lastStartOpts.InitFirewall, wantUnion) {
				t.Fatalf("%s: InitFirewall = %#v, want %#v", tc.name, mgr.lastStartOpts.InitFirewall, wantUnion)
			}
		})
	}
}

func TestHandlers_InitFirewallIgnoredInConfigAndInlineConfig(t *testing.T) {
	brokerPolicy, err := firewallpolicy.Normalize([]string{"pypi.org"}, false)
	if err != nil {
		t.Fatalf("Normalize broker: %v", err)
	}

	cases := []struct {
		name string
		path string
		body string
	}{
		{
			name: "create config and inlineConfig",
			path: "/api/v1/agents",
			body: `{
				"name": "fw-nested",
				"config": {
					"template": "claude",
					"initFirewall": {"allowDomains": ["evil-config.example.com"], "allowSsh": true}
				},
				"inlineConfig": {
					"initFirewall": {"allowDomains": ["evil-inline.example.com"], "allowSsh": true}
				}
			}`,
		},
		{
			name: "start inlineConfig",
			path: "/api/v1/agents/test-agent-1/start",
			body: `{
				"inlineConfig": {
					"initFirewall": {"allowDomains": ["evil-inline.example.com"], "allowSsh": true}
				}
			}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			srv.config.InitFirewall = brokerPolicy
			mgr := srv.manager.(*mockManager)

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusOK && w.Code != http.StatusCreated && w.Code != http.StatusAccepted {
				t.Fatalf("%s: expected 200/201/202, got %d: %s", tc.name, w.Code, w.Body.String())
			}
			if !reflect.DeepEqual(mgr.lastStartOpts.InitFirewall, brokerPolicy) {
				t.Fatalf("%s: InitFirewall = %#v, want broker-only %#v", tc.name, mgr.lastStartOpts.InitFirewall, brokerPolicy)
			}
		})
	}
}

func TestHandlers_InvalidHubInitFirewallReturns400WithoutContainerSideEffects(t *testing.T) {
	tooMany := make([]string, 65)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("h-%02d.example.com", i)
	}
	tooManyJSON, err := json.Marshal(tooMany)
	if err != nil {
		t.Fatalf("marshal 65 domains: %v", err)
	}

	invalidPolicies := []struct {
		name string
		json string
	}{
		{"metacharacter", `{"allowDomains":["bad;name"]}`},
		{"wildcard", `{"allowDomains":["*.example.com"]}`},
		{"ip literal", `{"allowDomains":["1.2.3.4"]}`},
		{"over 64 hub domains", `{"allowDomains":` + string(tooManyJSON) + `}`},
	}

	endpoints := []struct {
		name     string
		path     string
		wrapBody func(fwJSON string) string
	}{
		{
			name: "create",
			path: "/api/v1/agents",
			wrapBody: func(fwJSON string) string {
				return `{"name":"fw-bad","config":{"template":"claude"},"initFirewall":` + fwJSON + `}`
			},
		},
		{
			name: "start",
			path: "/api/v1/agents/test-agent-1/start",
			wrapBody: func(fwJSON string) string {
				return `{"initFirewall":` + fwJSON + `}`
			},
		},
		{
			name: "restart",
			path: "/api/v1/agents/test-agent-1/restart",
			wrapBody: func(fwJSON string) string {
				return `{"initFirewall":` + fwJSON + `}`
			},
		},
	}

	for _, ep := range endpoints {
		for _, tc := range invalidPolicies {
			t.Run(ep.name+"/"+tc.name, func(t *testing.T) {
				srv := newTestServer(t)
				mgr := srv.manager.(*mockManager)

				req := httptest.NewRequest(http.MethodPost, ep.path, strings.NewReader(ep.wrapBody(tc.json)))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, req)

				if w.Code != http.StatusBadRequest {
					t.Fatalf("%s/%s: expected 400, got %d: %s", ep.name, tc.name, w.Code, w.Body.String())
				}
				if mgr.startCalls != 0 {
					t.Errorf("%s/%s: startCalls = %d, want 0", ep.name, tc.name, mgr.startCalls)
				}
				if mgr.stopCalls != 0 {
					t.Errorf("%s/%s: stopCalls = %d, want 0 (restart must not stop on invalid policy)", ep.name, tc.name, mgr.stopCalls)
				}
			})
		}
	}
}
