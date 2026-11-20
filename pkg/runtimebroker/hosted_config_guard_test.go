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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hostedguard"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
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
// Kubernetes; the create proceeds past the check.
func TestCreateAgent_HostedAllowsGCSOnKubernetes(t *testing.T) {
	mgr := &mockManager{}
	setupTestScionEnv(t)
	cfg := testServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.ForceRuntime = "kubernetes"
	rt := &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})
	endAsyncLaunchesAtCleanup(t, srv)

	body := `{"name": "a1", "inlineConfig": {"volumes": [{"type": "gcs", "bucket": "b", "target": "/b"}]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code == http.StatusBadRequest {
		if code, _ := decodeError(t, w); code == hostedguard.ErrorCode {
			t.Fatalf("gcs on Kubernetes refused: %s", w.Body.String())
		}
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
