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
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hostedguard"
)

// hostedEnvSecret marks a value that must never appear in a refusal.
const hostedEnvSecret = "p3-secret-value"

// Member env and hub config the hosted broker refuses, as create body
// fragments, with the message prefix each must produce.
var hostedRefusedCreateConfig = []struct {
	name, fragment, wantPrefix string
}{
	{"inline env reference", `"inlineConfig": {"env": {"K": "a$` + hostedEnvSecret + `"}}`, `env "K": host environment references`},
	{"inline env empty", `"inlineConfig": {"env": {"GITHUB_TOKEN": ""}}`, `env "GITHUB_TOKEN": host environment references`},
	{"inline env reserved", `"inlineConfig": {"env": {"GCE_METADATA_HOST": "` + hostedEnvSecret + `"}}`, `env "GCE_METADATA_HOST": names reserved`},
	{"inline hub endpoint", `"inlineConfig": {"hub": {"endpoint": "http://127.0.0.1:9810/` + hostedEnvSecret + `"}}`, `hub "endpoint": a hub endpoint in agent config`},
}

func postAgentRequest(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func assertHostedEnvRefusal(t *testing.T, w *httptest.ResponseRecorder, wantPrefix string) {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	code, msg := decodeError(t, w)
	if code != hostedguard.ErrorCode {
		t.Fatalf("code = %q, want %q", code, hostedguard.ErrorCode)
	}
	if !strings.HasPrefix(msg, wantPrefix) {
		t.Errorf("message = %q, want prefix %q", msg, wantPrefix)
	}
	if strings.Contains(w.Body.String(), hostedEnvSecret) || strings.Contains(w.Body.String(), "127.0.0.1") {
		t.Errorf("response echoes a value: %s", w.Body.String())
	}
}

// A hosted broker refuses member env that reaches its own environment or
// names reserved variables, and a member hub block, at create (inline
// config), before anything starts. No volume is in
// these requests: the check runs without one.
func TestCreateAgent_HostedRefusesMemberEnvAndHub(t *testing.T) {
	for _, tc := range hostedRefusedCreateConfig {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &mockManager{}
			srv := newTestServerWithManager(t, mgr)
			w := postAgentRequest(t, srv, "/api/v1/agents", `{"name": "a1", `+tc.fragment+`}`)
			assertHostedEnvRefusal(t, w, tc.wantPrefix)
			if mgr.startCalls != 0 {
				t.Fatal("agent was started after a refusal")
			}
		})
	}
}

// A workstation broker keeps member env and hub config as before.
func TestCreateAgent_WorkstationKeepsMemberEnvAndHub(t *testing.T) {
	for _, tc := range hostedRefusedCreateConfig {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &mockManager{}
			srv := newTestServerWithManager(t, mgr)
			srv.config.WorkstationMode = true
			w := postAgentRequest(t, srv, "/api/v1/agents", `{"name": "a1", `+tc.fragment+`}`)
			if w.Code >= 400 {
				t.Fatalf("status = %d, want success in workstation mode: %s", w.Code, w.Body.String())
			}
			if mgr.startCalls != 1 {
				t.Fatalf("Start was called %d times, want 1", mgr.startCalls)
			}
		})
	}
}

// Literal member env and the reserved names member config may set are
// accepted on a hosted broker and reach Start.
func TestCreateAgent_HostedAcceptsLiteralMemberEnv(t *testing.T) {
	mgr := &mockManager{}
	srv := newTestServerWithManager(t, mgr)
	body := `{"name": "a1", "inlineConfig": {"env": {` +
		`"PLAIN": "x", "SCION_AUTO_EXPOSE_PORTS": "true", "SCION_LOG_LEVEL": "debug", "SCION_TELEMETRY_ENABLED": "false"}}}`
	w := postAgentRequest(t, srv, "/api/v1/agents", body)
	if w.Code >= 400 {
		t.Fatalf("status = %d, want success: %s", w.Code, w.Body.String())
	}
	if mgr.startCalls != 1 {
		t.Fatalf("Start was called %d times, want 1", mgr.startCalls)
	}
}

func TestStartAgent_HostedRefusesInlineMemberEnvAndHub(t *testing.T) {
	for _, tc := range []struct{ name, inline, wantPrefix string }{
		{"env reference", `{"env": {"K": "${` + hostedEnvSecret + `}"}}`, `env "K": host environment references`},
		{"env empty", `{"env": {"K": ""}}`, `env "K": host environment references`},
		{"env reserved", `{"env": {"SCION_AUTH_TOKEN": "` + hostedEnvSecret + `"}}`, `env "SCION_AUTH_TOKEN": names reserved`},
		{"hub endpoint", `{"hub": {"endpoint": "http://127.0.0.1:1/` + hostedEnvSecret + `"}}`, `hub "endpoint": `},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, hosted := range []bool{true, false} {
				mgr := &mockManager{agents: []api.AgentInfo{{ID: "container-1", Name: "test-agent-1", Phase: "stopped"}}}
				srv := newTestServerWithManager(t, mgr)
				srv.config.WorkstationMode = !hosted
				w := postAgentRequest(t, srv, "/api/v1/agents/test-agent-1/start", `{"inlineConfig": `+tc.inline+`}`)
				if hosted {
					assertHostedEnvRefusal(t, w, tc.wantPrefix)
					if mgr.startCalls != 0 {
						t.Fatal("Start was called after a refusal")
					}
					continue
				}
				if w.Code >= 400 || mgr.startCalls != 1 {
					t.Fatalf("workstation: status = %d, starts = %d: %s", w.Code, mgr.startCalls, w.Body.String())
				}
			}
		})
	}
}

// A create's config env is not checked by hostedguard: its values are used
// literally, and the broker handles the reserved names it reads from it on
// its own (clone URL, token remote, auth token, the native Cloud Run marker).
func TestCreateAgent_HostedLeavesRequestConfigEnvToTheBroker(t *testing.T) {
	mgr := &mockManager{}
	srv := newTestServerWithManager(t, mgr)
	w := postAgentRequest(t, srv, "/api/v1/agents", `{"name": "a1", "config": {"env": ["K=${X}", "E="]}}`)
	if w.Code >= 400 {
		t.Fatalf("status = %d, want success: %s", w.Code, w.Body.String())
	}
}

// On a hosted broker an env-gather create with a project-tier harness config
// whose env references the broker's environment is refused before that env
// is evaluated: the answer is the same 400 whether or not the referenced
// variable is set on the broker, so env-gather cannot reveal it. A
// workstation broker evaluates it as before.
func TestCreateAgent_HostedEnvGatherRefusesHarnessConfigEnvBeforeExpanding(t *testing.T) {
	const hostVar = "P3_BROKER_ORACLE_VAR"
	hcYAML := "harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n" + claudeAuthBlock +
		"env:\n  GOOGLE_CLOUD_PROJECT: \"${" + hostVar + "}\"\n"
	settings := "schema_version: \"1\"\nprofiles:\n  default:\n    runtime: mock\n"
	create := func(t *testing.T, workstation bool) *httptest.ResponseRecorder {
		t.Helper()
		srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude", hcYAML, settings)
		srv.config.WorkstationMode = workstation
		body := `{"name": "oracle", "id": "agent-uuid-oracle", "gatherEnv": true, "projectPath": "` + projectDir + `",
			"config": {"harnessConfig": "claude", "profile": "default"}}`
		return postAgentRequest(t, srv, "/api/v1/agents", body)
	}
	var bodies []string
	for _, set := range []bool{true, false} {
		if set {
			t.Setenv(hostVar, "broker-secret-project")
		} else {
			t.Setenv(hostVar, "")
			_ = os.Unsetenv(hostVar)
		}
		w := create(t, false)
		assertHostedEnvRefusal(t, w, `env "GOOGLE_CLOUD_PROJECT": host environment references`)
		if strings.Contains(w.Body.String(), "broker-secret-project") {
			t.Fatalf("response echoes the broker value: %s", w.Body.String())
		}
		bodies = append(bodies, w.Body.String())
	}
	if bodies[0] != bodies[1] {
		t.Errorf("the answer depends on the broker env:\nset:   %s\nunset: %s", bodies[0], bodies[1])
	}
	if w := create(t, true); w.Code == http.StatusBadRequest {
		code, _ := decodeError(t, w)
		if code == hostedguard.ErrorCode {
			t.Fatalf("workstation broker refused: %s", w.Body.String())
		}
	}
}
