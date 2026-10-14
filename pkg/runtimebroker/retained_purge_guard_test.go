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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestRetainedPurgeGuard(t *testing.T) {
	a := launchKey{ProjectID: "p1", Slug: "dev"}
	b := launchKey{ProjectID: "p1", Slug: "other"}
	g := newRetainedPurgeGuard()

	releasePurge, ok := g.beginPurge(a, b)
	if !ok {
		t.Fatal("purge refused with nothing registered")
	}
	if _, ok := g.beginProvision(a); ok {
		t.Fatal("provision allowed during a purge of its key")
	}
	if _, ok := g.beginProvision(b); ok {
		t.Fatal("provision allowed during a purge of its second key")
	}
	releaseProv, ok := g.beginProvision(launchKey{ProjectID: "p2", Slug: "dev"})
	if !ok {
		t.Fatal("provision of another project's agent refused")
	}
	releaseProv()
	releasePurge()
	releasePurge() // idempotent
	if len(g.purges) != 0 {
		t.Fatalf("purge registrations left after release: %v", g.purges)
	}

	releaseProv, ok = g.beginProvision(a)
	if !ok {
		t.Fatal("provision refused after the purge ended")
	}
	if _, ok := g.beginPurge(b, a); ok {
		t.Fatal("purge allowed while a provision of one of its keys runs")
	}
	if len(g.purges) != 0 {
		t.Fatalf("a refused purge left a registration: %v", g.purges)
	}
	releaseProv()
	if len(g.provisions) != 0 {
		t.Fatalf("provision registrations left after release: %v", g.provisions)
	}

	var nilGuard *retainedPurgeGuard
	if r, ok := nilGuard.beginPurge(a); !ok {
		t.Fatal("nil guard refused a purge")
	} else {
		r()
	}
	if r, ok := nilGuard.beginProvision(a); !ok {
		t.Fatal("nil guard refused a provision")
	} else {
		r()
	}
}

// guardedRequest builds one of the requests the guard refuses during a
// purge of agent "test-agent-2" in project "p1".
func guardedRequest(t *testing.T, kind string) *http.Request {
	t.Helper()
	var req *http.Request
	switch kind {
	case "start", "restart":
		req = httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-2/"+kind+"?projectId=p1", strings.NewReader(`{}`))
	case "create (ProvisionOnly)":
		body, err := json.Marshal(CreateAgentRequest{
			Name:          "test-agent-2",
			ID:            "agent-uuid-2",
			Slug:          "test-agent-2",
			ProjectID:     "p1",
			ProvisionOnly: true,
			Config:        &CreateAgentConfig{Template: "claude"},
		})
		if err != nil {
			t.Fatal(err)
		}
		req = httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(body)))
	default:
		t.Fatalf("unknown request kind %q", kind)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

// isPurgeInProgress reports whether rec is the guard's refusal.
func isPurgeInProgress(t *testing.T, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	if rec.Code != http.StatusConflict {
		return false
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return false
	}
	return resp.Error.Code == api.BrokerErrCodeRetainedPurgeInProgress
}

// A start, restart or create (ProvisionOnly included) of an agent whose
// retained files are being removed is refused with a retryable 409 and
// starts nothing; once the removal ends, the same request proceeds.
func TestRetainedPurgeGuard_RefusesStartRestartCreateDuringPurge(t *testing.T) {
	for _, kind := range []string{"start", "restart", "create (ProvisionOnly)"} {
		t.Run(kind, func(t *testing.T) {
			mgr := &mockManager{agents: []api.AgentInfo{{ID: "container-2", Name: "test-agent-2", Phase: "stopped"}}}
			srv := newTestServerWithManager(t, mgr)

			release, ok := srv.retainedPurges.beginPurge(launchKey{ProjectID: "p1", Slug: "test-agent-2"})
			if !ok {
				t.Fatal("could not register a purge")
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, guardedRequest(t, kind))
			if !isPurgeInProgress(t, rec) {
				release()
				t.Fatalf("expected 409 %s during a purge, got %d: %s", api.BrokerErrCodeRetainedPurgeInProgress, rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Retry-After") == "" {
				t.Error("refusal carries no Retry-After")
			}
			var resp ErrorResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if reason, _ := resp.Error.Details[api.BrokerErrDetailReason].(string); !strings.Contains(reason, "retained files") {
				t.Errorf("details reason = %q", reason)
			}
			mgr.mu.Lock()
			starts := mgr.startCalls
			mgr.mu.Unlock()
			if starts != 0 {
				t.Errorf("Start called %d times during a purge", starts)
			}
			if len(srv.retainedPurges.provisions) != 0 {
				t.Errorf("a refused request left a provision registration: %v", srv.retainedPurges.provisions)
			}
			release()

			rec = httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, guardedRequest(t, kind))
			if isPurgeInProgress(t, rec) {
				t.Fatalf("still refused after the purge ended: %s", rec.Body.String())
			}
			if len(srv.retainedPurges.provisions) != 0 {
				t.Errorf("provision registration left after the handler returned: %v", srv.retainedPurges.provisions)
			}
		})
	}
}
