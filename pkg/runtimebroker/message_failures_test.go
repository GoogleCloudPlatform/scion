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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// bufferingFailManager routes non-interrupt messages through a real
// MessageBuffer whose delivery always fails, reproducing the #1820 case of
// a container that is gone by the time the debounce timer fires.
type bufferingFailManager struct {
	*mockManager
	buf *agent.MessageBuffer
}

func (m *bufferingFailManager) Message(ctx context.Context, agentID, projectID string, message string, interrupt bool) error {
	m.buf.SendWithFailureHandler(agentID, projectID, message, agent.DeliveryFailureHandlerFromContext(ctx))
	return nil
}

// stubBrokerHubClient stubs hubclient.Client, exposing only RuntimeBrokers().
type stubBrokerHubClient struct {
	hubclient.Client
	brokers *mockRuntimeBrokerService
}

func (c *stubBrokerHubClient) RuntimeBrokers() hubclient.RuntimeBrokerService { return c.brokers }

func (m *mockRuntimeBrokerService) getMessageFailureReports() []*hubclient.MessageFailuresReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*hubclient.MessageFailuresReport{}, m.messageFailureReports...)
}

func setupFailingBufferServer(t *testing.T) (*Server, *mockRuntimeBrokerService, *mockRuntimeBrokerService) {
	t.Helper()
	srv := newTestServer(t)
	base := srv.manager.(*mockManager)
	fm := &bufferingFailManager{
		mockManager: base,
		buf: agent.NewMessageBuffer(20*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
			return errors.New("agent '" + agentID + "' not found or not running")
		}),
	}
	t.Cleanup(fm.buf.Close)
	srv.manager = fm

	hubA := &mockRuntimeBrokerService{}
	hubB := &mockRuntimeBrokerService{}
	srv.hubMu.Lock()
	srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a", BrokerID: "broker-on-a", HubClient: &stubBrokerHubClient{brokers: hubA}}
	srv.hubConnections["hub-b"] = &HubConnection{Name: "hub-b", BrokerID: "broker-on-b", HubClient: &stubBrokerHubClient{brokers: hubB}}
	srv.hubMu.Unlock()
	return srv, hubA, hubB
}

func postBrokerMessage(t *testing.T, srv *Server, connHeader string, req MessageRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/message", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if connHeader != "" {
		r.Header.Set("X-Scion-Hub-Connection", connHeader)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

func waitForReports(t *testing.T, svc *mockRuntimeBrokerService, n int) []*hubclient.MessageFailuresReport {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r := svc.getMessageFailureReports(); len(r) >= n {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d message failure report(s)", n)
	return nil
}

// TestSendMessage_BufferedFlushFailureReportedToHub asserts that a message
// the broker accepted with 200 and then failed to deliver from its buffer is
// reported back to the originating hub with the hub's message ID (#1820).
func TestSendMessage_BufferedFlushFailureReportedToHub(t *testing.T) {
	srv, hubA, hubB := setupFailingBufferServer(t)

	w := postBrokerMessage(t, srv, "hub-a", MessageRequest{Message: "hello", MessageID: "msg-1", ProjectID: "proj-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (buffered accept), got %d: %s", w.Code, w.Body.String())
	}

	reports := waitForReports(t, hubA, 1)
	if len(reports[0].Failures) != 1 {
		t.Fatalf("expected 1 failure in report, got %d", len(reports[0].Failures))
	}
	f := reports[0].Failures[0]
	if f.MessageID != "msg-1" || f.AgentID != "test-agent-1" || f.ProjectID != "proj-1" {
		t.Errorf("unexpected failure payload: %+v", f)
	}
	if f.Reason == "" {
		t.Error("failure reason must be populated")
	}

	// Only the originating hub connection is told.
	time.Sleep(50 * time.Millisecond)
	if got := len(hubB.getMessageFailureReports()); got != 0 {
		t.Errorf("non-originating hub received %d reports, want 0", got)
	}
}

// TestSendMessage_BufferedFlushFailure_CoalescedMessagesAllReported checks
// that every message coalesced into a failed flush is reported, and that
// without a connection header every hub connection is told.
func TestSendMessage_BufferedFlushFailure_CoalescedMessagesAllReported(t *testing.T) {
	srv, hubA, hubB := setupFailingBufferServer(t)

	for _, id := range []string{"m-1", "m-2"} {
		if w := postBrokerMessage(t, srv, "", MessageRequest{Message: "x", MessageID: id}); w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	}

	for _, svc := range []*mockRuntimeBrokerService{hubA, hubB} {
		reports := waitForReports(t, svc, 2)
		got := map[string]bool{}
		for _, r := range reports {
			for _, f := range r.Failures {
				got[f.MessageID] = true
			}
		}
		if !got["m-1"] || !got["m-2"] {
			t.Errorf("expected both coalesced messages reported, got %v", got)
		}
	}
}

// TestSendMessage_NoMessageIDNoReport keeps legacy callers (and the human
// send path, which does not attach a message ID) unchanged.
func TestSendMessage_NoMessageIDNoReport(t *testing.T) {
	srv, hubA, hubB := setupFailingBufferServer(t)

	if w := postBrokerMessage(t, srv, "hub-a", MessageRequest{Message: "x"}); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(hubA.getMessageFailureReports()) + len(hubB.getMessageFailureReports()); n != 0 {
		t.Errorf("expected no reports without a message ID, got %d", n)
	}
}
