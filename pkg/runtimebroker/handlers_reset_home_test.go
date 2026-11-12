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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestCreateAgentReprovision_ResetHome_PassedAndEchoed: a reprovision with
// resetHome passes the reset to Manager.Reprovision and echoes homeReset
// (ptone/scion#7757).
func TestCreateAgentReprovision_ResetHome_PassedAndEchoed(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()
	body := `{
		"name": "reset-agent", "id": "agent-uuid-reset", "slug": "reset-agent",
		"provisionOnly": true, "reprovision": true,
		"resetHome": true, "resetHomeId": "reinc-1234",
		"config": {"template": "claude"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !mgr.reprovisionCalled || !mgr.lastOpts.ResetHome || mgr.lastOpts.ResetHomeID != "reinc-1234" {
		t.Fatalf("Reprovision called=%v ResetHome=%v ResetHomeID=%q, want the reset passed through",
			mgr.reprovisionCalled, mgr.lastOpts.ResetHome, mgr.lastOpts.ResetHomeID)
	}
	var resp CreateAgentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Reprovisioned || !resp.HomeReset {
		t.Errorf("reprovisioned=%v homeReset=%v, want both true", resp.Reprovisioned, resp.HomeReset)
	}
}

// TestCreateAgentReprovision_NoResetHome_NoEcho: a plain reprovision never
// echoes homeReset.
func TestCreateAgentReprovision_NoResetHome_NoEcho(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()
	body := `{"name": "a", "id": "agent-uuid-a", "slug": "a", "provisionOnly": true, "reprovision": true, "config": {"template": "claude"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if mgr.lastOpts.ResetHome {
		t.Error("ResetHome set on a plain reprovision")
	}
	var resp CreateAgentResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.HomeReset {
		t.Error("homeReset echoed on a plain reprovision")
	}
}

// TestCreateAgent_ResetHome_Refused400: resetHome is refused, before any
// manager call, on anything but a provision-only reprovision, and with a
// missing or malformed reset ID.
func TestCreateAgent_ResetHome_Refused400(t *testing.T) {
	cases := map[string]string{
		"not provision-only":      `"reprovision": true, "resetHome": true, "resetHomeId": "r1"`,
		"not a reprovision":       `"provisionOnly": true, "resetHome": true, "resetHomeId": "r1"`,
		"missing id":              `"provisionOnly": true, "reprovision": true, "resetHome": true`,
		"id without reset":        `"provisionOnly": true, "reprovision": true, "resetHomeId": "r1"`,
		"id with a path":          `"provisionOnly": true, "reprovision": true, "resetHome": true, "resetHomeId": "../x"`,
		"id too long":             `"provisionOnly": true, "reprovision": true, "resetHome": true, "resetHomeId": "` + strings.Repeat("a", 65) + `"`,
		"plain start, no options": `"resetHome": true, "resetHomeId": "r1"`,
	}
	for name, fields := range cases {
		t.Run(name, func(t *testing.T) {
			srv, mgr := newTestServerWithProvisionCapture()
			body := `{"name": "a", "id": "agent-uuid-a", "slug": "a", ` + fields + `, "config": {"template": "claude"}}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			if mgr.reprovisionCalled || mgr.provisionCalled || mgr.startCalled {
				t.Error("manager called for a refused request")
			}
		})
	}
}

// TestHeartbeatService_ReportsResetHomeRuntimes pins the runtimes this
// broker resets a home on, in the heartbeat and in the registration names.
func TestHeartbeatService_ReportsResetHomeRuntimes(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc := NewHeartbeatService(client, "test-host", time.Hour, nil, nil, slog.Default())
	if err := svc.ForceHeartbeat(context.Background()); err != nil {
		t.Fatalf("ForceHeartbeat: %v", err)
	}
	calls := client.getHeartbeatCalls()
	if len(calls) != 1 || calls[0].Heartbeat.Capabilities == nil {
		t.Fatalf("expected 1 heartbeat with capabilities, got %d calls", len(calls))
	}
	want := []string{"docker", "podman", "container"}
	if got := calls[0].Heartbeat.Capabilities.ReprovisionResetHomeRuntimes; !slices.Equal(got, want) {
		t.Errorf("heartbeat ReprovisionResetHomeRuntimes = %v, want %v", got, want)
	}
	names := StaticCapabilityNames(nil)
	for _, rt := range want {
		if !slices.Contains(names, ReprovisionResetHomeCapabilityPrefix+rt) {
			t.Errorf("registration names %v lack %s", names, ReprovisionResetHomeCapabilityPrefix+rt)
		}
	}
}

// TestCreateAgentReprovision_ResetHome_RefusalCode: a refused home-resetting
// reprovision carries the home-intact code only when the refusal came
// before the home was moved aside.
func TestCreateAgentReprovision_ResetHome_RefusalCode(t *testing.T) {
	refused := fmt.Errorf("%w: container is still running", agent.ErrReprovisionRefused)
	for name, tc := range map[string]struct {
		err      error
		reset    bool
		wantCode string
	}{
		"before the reset":          {err: refused, reset: true, wantCode: api.BrokerErrCodeReprovisionRefusedHomeIntact},
		"after the reset":           {err: fmt.Errorf("%w; %w", refused, agent.ErrHomeResetApplied), reset: true, wantCode: ErrCodeConflict},
		"plain reprovision refusal": {err: refused, reset: false, wantCode: ErrCodeConflict},
	} {
		t.Run(name, func(t *testing.T) {
			srv, mgr := newTestServerWithProvisionCapture()
			mgr.reprovisionErr = tc.err
			extra := ""
			if tc.reset {
				extra = `"resetHome": true, "resetHomeId": "r1", `
			}
			body := `{"name": "a", "id": "agent-uuid-a", "slug": "a", "provisionOnly": true, "reprovision": true, ` + extra + `"config": {"template": "claude"}}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
			}
			var body409 struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &body409)
			if body409.Error.Code != tc.wantCode {
				t.Errorf("error code = %q, want %q (body %s)", body409.Error.Code, tc.wantCode, w.Body.String())
			}
		})
	}
}
