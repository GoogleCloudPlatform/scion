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
	"errors"
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
	want := []string{"docker", "podman", "container", "cloudrun", "cloudrun-instances"}
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

// homeResetDetail returns the home reset detail of an error answer, or "".
func homeResetDetail(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode error answer: %v (%s)", err, body)
	}
	v, _ := resp.Error.Details[api.BrokerErrDetailHomeReset].(string)
	return v
}

func postResetCreate(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// TestCreateAgentReprovision_ResetHome_ErrorDetail: every error answer to a
// home-resetting request, whatever its status, says whether the home was
// moved: "applied" only when the reprovision failed after the move.
func TestCreateAgentReprovision_ResetHome_ErrorDetail(t *testing.T) {
	refused := fmt.Errorf("%w: container is still running", agent.ErrReprovisionRefused)
	for name, tc := range map[string]struct {
		err        error
		wantStatus int
		want       string
	}{
		"409 refusal before the reset": {err: refused, wantStatus: http.StatusConflict, want: api.BrokerHomeResetIntact},
		"500 failure before the reset": {err: errors.New("load effective settings: boom"), wantStatus: http.StatusInternalServerError, want: api.BrokerHomeResetIntact},
		"409 refusal after the reset":  {err: fmt.Errorf("%w; %w", refused, agent.ErrHomeResetApplied), wantStatus: http.StatusConflict, want: api.BrokerHomeResetApplied},
		"500 failure after the reset":  {err: fmt.Errorf("skill: %w", agent.ErrHomeResetApplied), wantStatus: http.StatusInternalServerError, want: api.BrokerHomeResetApplied},
	} {
		t.Run(name, func(t *testing.T) {
			srv, mgr := newTestServerWithProvisionCapture()
			mgr.reprovisionErr = tc.err
			w := postResetCreate(t, srv, `{"name": "a", "id": "agent-uuid-a", "slug": "a", "provisionOnly": true, "reprovision": true, "resetHome": true, "resetHomeId": "r1", "config": {"template": "claude"}}`)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			if got := homeResetDetail(t, w.Body.Bytes()); got != tc.want {
				t.Errorf("homeReset detail = %q, want %q (body %s)", got, tc.want, w.Body.String())
			}
		})
	}

	t.Run("400 validation", func(t *testing.T) {
		srv, _ := newTestServerWithProvisionCapture()
		w := postResetCreate(t, srv, `{"name": "a", "id": "agent-uuid-a", "slug": "a", "provisionOnly": true, "reprovision": true, "resetHome": true, "resetHomeId": "../x", "config": {"template": "claude"}}`)
		if w.Code != http.StatusBadRequest || homeResetDetail(t, w.Body.Bytes()) != api.BrokerHomeResetIntact {
			t.Fatalf("got %d %s, want 400 with the intact detail", w.Code, w.Body.String())
		}
	})
	t.Run("plain reprovision error has no detail", func(t *testing.T) {
		srv, mgr := newTestServerWithProvisionCapture()
		mgr.reprovisionErr = refused
		w := postResetCreate(t, srv, `{"name": "a", "id": "agent-uuid-a", "slug": "a", "provisionOnly": true, "reprovision": true, "config": {"template": "claude"}}`)
		if w.Code != http.StatusConflict || homeResetDetail(t, w.Body.Bytes()) != "" {
			t.Fatalf("got %d %s, want 409 without the detail", w.Code, w.Body.String())
		}
	})
}

// resetHomeStampedBody is dispatchStampedAgent's create as a home-resetting
// reprovision.
const resetHomeStampedBody = `{
	"name": "gate-agent", "id": "agent-uuid-gate", "slug": "gate-agent", "requestId": "req-gate-reset",
	"provisionOnly": true, "reprovision": true, "resetHome": true, "resetHomeId": "r1",
	"config": {"harnessConfig": "hub-only", "harnessConfigId": "hc-id", "harnessConfigHash": "sha256:dispatch"}
}`

// Early create-path failures of a home-resetting request (the target
// harness config's hydration, the broker's harness-config policy) say the
// home is intact, and the reprovision never runs.
func TestCreateAgentReprovision_ResetHome_EarlyFailuresAreIntact(t *testing.T) {
	t.Run("hydration failure", func(t *testing.T) {
		srv, mgr, _ := dispatchTestEnv(t, false)
		attachHubHC(t, srv, "", errors.New("record vanished"))
		w := postResetCreate(t, srv, resetHomeStampedBody)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		if got := homeResetDetail(t, w.Body.Bytes()); got != api.BrokerHomeResetIntact {
			t.Errorf("homeReset detail = %q, want intact (%s)", got, w.Body.String())
		}
		if mgr.reprovisionCalled {
			t.Error("Reprovision ran after a hydration failure")
		}
	})
	t.Run("harness-config policy refusal", func(t *testing.T) {
		srv, mgr, _ := dispatchTestEnv(t, false)
		attachHubHC(t, srv, scriptedHarnessYAML, nil)
		w := postResetCreate(t, srv, resetHomeStampedBody)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		if got := homeResetDetail(t, w.Body.Bytes()); got != api.BrokerHomeResetIntact {
			t.Errorf("homeReset detail = %q, want intact (%s)", got, w.Body.String())
		}
		if mgr.reprovisionCalled {
			t.Error("Reprovision ran after a policy refusal")
		}
	})
}
