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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// TestResolveHubNameForLaunch covers design t1-async-create-v11.md §3.8.5
// routing rules 1-3 (B-5): the X-Scion-Hub-Connection header, then the
// authenticating connection, then the sole connection; "" when ambiguous.
func TestResolveHubNameForLaunch(t *testing.T) {
	newServerWithConns := func(names ...string) *Server {
		srv := newTestServer(t)
		srv.hubMu.Lock()
		for _, name := range names {
			srv.hubConnections[name] = &HubConnection{Name: name, BrokerID: "broker-" + name, HubClient: &stubBrokerHubClient{brokers: &mockRuntimeBrokerService{}}}
		}
		srv.hubMu.Unlock()
		return srv
	}

	t.Run("header wins", func(t *testing.T) {
		srv := newServerWithConns("hub-a", "hub-b")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
		req.Header.Set("X-Scion-Hub-Connection", "hub-b")
		req = req.WithContext(context.WithValue(req.Context(), authenticatingHubConnCtxKey{}, "hub-a"))
		if got := srv.resolveHubNameForLaunch(req); got != "hub-b" {
			t.Fatalf("got %q, want hub-b", got)
		}
	})

	t.Run("authenticating connection when no header", func(t *testing.T) {
		srv := newServerWithConns("hub-a", "hub-b")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
		req = req.WithContext(context.WithValue(req.Context(), authenticatingHubConnCtxKey{}, "hub-a"))
		if got := srv.resolveHubNameForLaunch(req); got != "hub-a" {
			t.Fatalf("got %q, want hub-a", got)
		}
	})

	t.Run("sole connection when neither resolves", func(t *testing.T) {
		srv := newServerWithConns("hub-only")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
		if got := srv.resolveHubNameForLaunch(req); got != "hub-only" {
			t.Fatalf("got %q, want hub-only", got)
		}
	})

	t.Run("ambiguous with multiple connections and no hint", func(t *testing.T) {
		srv := newServerWithConns("hub-a", "hub-b")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
		if got := srv.resolveHubNameForLaunch(req); got != "" {
			t.Fatalf("got %q, want \"\" (fan out)", got)
		}
	})

	t.Run("unknown header value falls through", func(t *testing.T) {
		srv := newServerWithConns("hub-a")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil)
		req.Header.Set("X-Scion-Hub-Connection", "hub-does-not-exist")
		if got := srv.resolveHubNameForLaunch(req); got != "hub-a" {
			t.Fatalf("got %q, want hub-a (falls back to the sole connection)", got)
		}
	})
}

// TestLaunchSender_FanOutPinsOwnerOnFirstDefinitiveAnswer covers design
// §3.8.5 routing rule 4 (B-5): with no resolved HubName, the sender fans out
// and the first 2xx/409 pins OwnerHub; later reports go only there.
func TestLaunchSender_FanOutPinsOwnerOnFirstDefinitiveAnswer(t *testing.T) {
	srv := newTestServer(t)
	hubA := &mockRuntimeBrokerService{
		launchReportFunc: func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			return nil, errUnreachableForTest
		},
	}
	hubB := &mockRuntimeBrokerService{} // default: always "applied"
	srv.hubMu.Lock()
	srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a", BrokerID: "broker-a", HubClient: &stubBrokerHubClient{brokers: hubA}}
	srv.hubConnections["hub-b"] = &HubConnection{Name: "hub-b", BrokerID: "broker-b", HubClient: &stubBrokerHubClient{brokers: hubB}}
	srv.hubMu.Unlock()

	rec := newLaunchRecord("L1", "agent-1", "create", "", time.Now().Add(time.Minute), func() {})
	sender := newLaunchSender(srv, rec, "agent-1", "instance-1", 15*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := sender.SendClaim(ctx)
	if err != nil {
		t.Fatalf("SendClaim: %v", err)
	}
	if result.Result != hubclient.AgentLaunchReportResultApplied {
		t.Fatalf("result = %+v, want applied", result)
	}
	if got := rec.OwnerHub(); got != "hub-b" {
		t.Fatalf("OwnerHub = %q, want hub-b (the connection that answered)", got)
	}
	// sendOnce returns as soon as hub-b's qualifying answer arrives, without
	// waiting for hub-a's concurrent attempt (the fan-out is concurrent, not
	// sequential), so hub-a's record may land a moment later; poll instead
	// of asserting immediately.
	if !waitUntil(t, time.Second, func() bool { return len(hubA.getLaunchReports()) > 0 }) {
		t.Fatal("expected the fan-out to have tried hub-a")
	}

	// A second report must go only to the pinned owner now.
	if _, err := sender.sendOnce(ctx, &hubclient.AgentLaunchReport{LaunchID: "L1", State: hubclient.AgentLaunchReportStateProgress}, time.Second); err != nil {
		t.Fatalf("sendOnce after pinning: %v", err)
	}
	if got := len(hubA.getLaunchReports()); got != 1 {
		t.Fatalf("hub-a got %d reports after pinning, want 1 (only the initial fan-out attempt)", got)
	}
}

var errUnreachableForTest = &testUnreachableError{}

type testUnreachableError struct{}

func (*testUnreachableError) Error() string { return "simulated unreachable" }
