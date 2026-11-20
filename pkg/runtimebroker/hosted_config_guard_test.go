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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hostedguard"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// hostedSocketVolume is a member-written host bind that no Runtime Broker
// control path covers.
const hostedSocketVolume = `{"source": "/var/run/docker.sock", "target": "/var/run/docker.sock"}`

func decodeError(t *testing.T, w *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var resp ErrorResponse
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&resp); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, w.Body.String())
	}
	return resp.Error.Code, resp.Error.Message
}

func TestCreateAgent_HostedRefusesMemberVolumes(t *testing.T) {
	for _, vols := range []string{
		`"config": {"volumes": [` + hostedSocketVolume + `]}`,
		`"inlineConfig": {"volumes": [` + hostedSocketVolume + `]}`,
		`"inlineConfig": {"volumes": [{"source": "/etc", "target": "/host-etc", "read_only": true}]}`,
		`"inlineConfig": {"volumes": [{"type": "nfs", "server": "10.1.2.3", "source": "/export", "target": "/n"}]}`,
		`"inlineConfig": {"volumes": [{"type": "gcs", "bucket": "b", "target": "/b"}]}`,
	} {
		mgr := &mockManager{}
		srv := newTestServerWithManager(t, mgr)
		body := `{"name": "a1", ` + vols + `}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400: %s", vols, w.Code, w.Body.String())
		}
		code, msg := decodeError(t, w)
		if code != hostedguard.ErrorCode {
			t.Fatalf("%s: code = %q, want %q", vols, code, hostedguard.ErrorCode)
		}
		// The message names the target, never the host source or server.
		if strings.Contains(msg, "10.1.2.3") || strings.Contains(msg, "/export") || strings.Contains(msg, "/etc\"") {
			t.Errorf("%s: message echoes a source: %q", vols, msg)
		}
		if mgr.startCalls != 0 {
			t.Fatalf("%s: agent was started after a refusal", vols)
		}
	}
}

// A gcs volume is let through on a hosted broker whose agents run on
// Kubernetes, under each name a Kubernetes runtime type may have in
// settings ("kubernetes", and the legacy "k8s" and "remote"); the create or
// start then runs.
func TestHostedAllowsGCSOnKubernetes(t *testing.T) {
	newServer := func(t *testing.T, rtName string, mgr *mockManager) *Server {
		t.Helper()
		setupTestScionEnv(t)
		cfg := testServerConfig()
		cfg.BrokerID = "test-broker-id"
		cfg.ForceRuntime = rtName
		rt := &runtime.MockRuntime{NameFunc: func() string { return rtName }}
		srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})
		endAsyncLaunchesAtCleanup(t, srv)
		return srv
	}
	const gcs = `{"volumes": [{"type": "gcs", "bucket": "b", "target": "/b"}]}`
	for _, rtName := range []string{"kubernetes", "k8s", "remote"} {
		t.Run("create on "+rtName, func(t *testing.T) {
			mgr := &mockManager{}
			srv := newServer(t, rtName, mgr)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(`{"name": "a1", "inlineConfig": `+gcs+`}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code >= 400 {
				t.Fatalf("status = %d, want success: %s", w.Code, w.Body.String())
			}
			if mgr.startCalls != 1 {
				t.Fatalf("Start was called %d times, want 1", mgr.startCalls)
			}
		})
		t.Run("start on "+rtName, func(t *testing.T) {
			mgr := &mockManager{agents: []api.AgentInfo{{ID: "container-1", Name: "test-agent-1", Phase: "stopped"}}}
			srv := newServer(t, rtName, mgr)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start", strings.NewReader(`{"inlineConfig": `+gcs+`}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code >= 400 {
				t.Fatalf("status = %d, want success: %s", w.Code, w.Body.String())
			}
			if mgr.startCalls != 1 {
				t.Fatalf("Start was called %d times, want 1", mgr.startCalls)
			}
		})
	}
}

func TestCreateAgent_WorkstationKeepsMemberVolumes(t *testing.T) {
	mgr := &mockManager{}
	srv := newTestServerWithManager(t, mgr)
	srv.config.WorkstationMode = true
	body := `{"name": "a1", "inlineConfig": {"volumes": [` + hostedSocketVolume + `]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code >= 400 {
		t.Fatalf("status = %d, want success in workstation mode: %s", w.Code, w.Body.String())
	}
}

func TestStartAgent_HostedRefusesInlineMemberVolumes(t *testing.T) {
	mgr := &mockManager{agents: []api.AgentInfo{{ID: "container-1", Name: "test-agent-1", Phase: "stopped"}}}
	srv := newTestServerWithManager(t, mgr)
	body := `{"inlineConfig": {"volumes": [` + hostedSocketVolume + `]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	code, msg := decodeError(t, w)
	if code != hostedguard.ErrorCode {
		t.Fatalf("code = %q, want %q", code, hostedguard.ErrorCode)
	}
	want := `volume "/var/run/docker.sock": host volumes in agent config are not allowed on a hosted server`
	if !strings.HasPrefix(msg, want) {
		t.Errorf("message = %q, want prefix %q", msg, want)
	}
	if mgr.startCalls != 0 {
		t.Fatalf("Start was called %d times after a refused volume", mgr.startCalls)
	}
}

func TestStartAgent_WorkstationKeepsInlineMemberVolumes(t *testing.T) {
	mgr := &mockManager{agents: []api.AgentInfo{{ID: "container-1", Name: "test-agent-1", Phase: "stopped"}}}
	srv := newTestServerWithManager(t, mgr)
	srv.config.WorkstationMode = true
	body := `{"inlineConfig": {"volumes": [` + hostedSocketVolume + `]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code >= 400 {
		t.Fatalf("status = %d, want success in workstation mode: %s", w.Code, w.Body.String())
	}
	if mgr.startCalls != 1 {
		t.Fatalf("Start was called %d times, want 1", mgr.startCalls)
	}
	if mgr.lastStartOpts.HostedMode {
		t.Error("HostedMode = true on a workstation broker")
	}
}

// A start without inline config has no member config to check; volumes
// already stored on the broker come from earlier provisioning, which the
// Hub checks at dispatch.
func TestStartAgent_HostedWithoutInlineConfigStarts(t *testing.T) {
	mgr := &mockManager{agents: []api.AgentInfo{{ID: "container-1", Name: "test-agent-1", Phase: "stopped"}}}
	srv := newTestServerWithManager(t, mgr)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code >= 400 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !mgr.lastStartOpts.HostedMode || !mgr.lastStartOpts.BrokerMode {
		t.Errorf("HostedMode = %v, BrokerMode = %v on a hosted broker, want both true", mgr.lastStartOpts.HostedMode, mgr.lastStartOpts.BrokerMode)
	}
}

// hostedHCRefusal is the refusal pkg/agent raises for a member-written
// harness config's volume, wrapped as callers in pkg/agent may wrap it with
// text naming a broker path.
func hostedHCRefusal() error {
	re := hostedguard.CheckMemberConfig(&api.ScionConfig{Volumes: []api.VolumeMount{{Source: "/srv/broker-private", Target: "/hc-data"}}},
		hostedguard.Origin{Kind: hostedguard.OriginHarnessConfig, Name: "hc-vol"}, "docker")
	return fmt.Errorf("provision in /var/lib/broker-private-dir: %w", re)
}

func assertHostedHCResponse(t *testing.T, code int, body string) ErrorResponse {
	t.Helper()
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", code, body)
	}
	var resp ErrorResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("not an ErrorResponse: %v: %s", err, body)
	}
	if resp.Error.Code != hostedguard.ErrorCode {
		t.Errorf("code = %q, want %q", resp.Error.Code, hostedguard.ErrorCode)
	}
	if !strings.Contains(resp.Error.Message, `volume "/hc-data"`) || !strings.Contains(resp.Error.Message, `harness config "hc-vol"`) {
		t.Errorf("message %q does not name the volume and the harness config", resp.Error.Message)
	}
	if strings.Contains(body, "broker-private") {
		t.Errorf("response leaks a broker path or source: %s", body)
	}
	return resp
}

// Every synchronous dispatch path answers a harness config refused inside
// Manager.Provision or Start with 400 hosted_config_refused and the
// refusal's own text, not what wraps it.
func TestHostedHarnessConfigRefusal_DispatchPathsAnswer400(t *testing.T) {
	cases := []struct {
		name, at, path, body string
		startAttempted       bool
	}{
		{"create sync start", "start", "/api/v1/agents",
			`{"name": "h1", "id": "id-h1", "slug": "h1", "config": {"template": "none"}}`, false},
		{"create provision-only", "provision", "/api/v1/agents",
			`{"name": "h2", "id": "id-h2", "slug": "h2", "provisionOnly": true, "config": {"template": "none"}}`, false},
		{"start", "start", "/api/v1/agents/h3/start", `{"runId": "run-h3"}`, true},
		{"restart", "start", "/api/v1/agents/h4/restart", `{"runId": "run-h4"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := dispatchTestEnv(t, true)
			srv.manager = &unusableManager{at: tc.at, err: hostedHCRefusal()}
			code, body := postJSON(t, srv, tc.path, tc.body)
			resp := assertHostedHCResponse(t, code, body)
			if tc.startAttempted && resp.Error.Details[api.BrokerErrorDetailStartAttempted] != true {
				t.Errorf("expected %s=true in details, got %v", api.BrokerErrorDetailStartAttempted, resp.Error.Details)
			}
		})
	}
}

// An async create refused at admission (Manager.Preflight) answers the same.
func TestHostedHarnessConfigRefusal_AsyncAdmission(t *testing.T) {
	code, body, _ := asyncPreflightFailure(t, "h5", hostedHCRefusal())
	assertHostedHCResponse(t, code, body)
}

// An async launch reports the hosted_config_refused code with the
// refusal's own text, and the hub counts it as refused before any
// container was created.
func TestClassifyStartError_HostedHarnessConfigRefusal(t *testing.T) {
	code, _, msg := classifyStartError(context.Background(), hostedHCRefusal(), "", "", store.LaunchKindStart)
	if code != hostedguard.ErrorCode {
		t.Errorf("code = %q, want %q", code, hostedguard.ErrorCode)
	}
	if !strings.Contains(msg, `harness config "hc-vol"`) || strings.Contains(msg, "broker-private") {
		t.Errorf("message should name the harness config without broker paths, got %q", msg)
	}
	if !store.IsBrokerPreContainerLaunchRefusal(code) {
		t.Errorf("%q should be a pre-container launch refusal", code)
	}
}
