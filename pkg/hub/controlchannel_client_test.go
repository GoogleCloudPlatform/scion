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

package hub

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

type mockControlChannelTunnel struct {
	connected   bool
	lastBroker  string
	lastRequest *wsprotocol.RequestEnvelope
}

func (m *mockControlChannelTunnel) IsConnected(string) bool {
	return m.connected
}

func (m *mockControlChannelTunnel) TunnelRequest(_ context.Context, brokerID string, req *wsprotocol.RequestEnvelope) (*wsprotocol.ResponseEnvelope, error) {
	m.lastBroker = brokerID
	m.lastRequest = req
	return wsprotocol.NewResponseEnvelope(req.RequestID, http.StatusOK, nil, nil), nil
}

type mockBrokerSigner struct {
	called bool
}

func (m *mockBrokerSigner) Sign(_ context.Context, req *http.Request, brokerID string) error {
	m.called = true
	req.Header.Set(apiclient.HeaderBrokerID, brokerID)
	req.Header.Set(apiclient.HeaderTimestamp, "1700000000")
	req.Header.Set(apiclient.HeaderNonce, "nonce")
	req.Header.Set(apiclient.HeaderSignature, "signature")
	return nil
}

func TestControlChannelBrokerClient_DeleteAgentSignsTunneledRequest(t *testing.T) {
	tunnel := &mockControlChannelTunnel{connected: true}
	signer := &mockBrokerSigner{}
	client := &ControlChannelBrokerClient{
		manager: tunnel,
		signer:  signer,
	}

	err := client.DeleteAgent(context.Background(), "broker-1", "unused", "agent-1", "", true, false, false, time.Time{})
	if err != nil {
		t.Fatalf("DeleteAgent returned error: %v", err)
	}

	if !signer.called {
		t.Fatal("expected signer to be called")
	}
	if tunnel.lastRequest == nil {
		t.Fatal("expected tunneled request to be captured")
	}
	if got := headerValue(tunnel.lastRequest.Headers, apiclient.HeaderBrokerID); got != "broker-1" {
		t.Fatalf("expected %s header to be set, got %q", apiclient.HeaderBrokerID, got)
	}
	if got := headerValue(tunnel.lastRequest.Headers, apiclient.HeaderTimestamp); got == "" {
		t.Fatalf("expected %s header to be set", apiclient.HeaderTimestamp)
	}
	if got := headerValue(tunnel.lastRequest.Headers, apiclient.HeaderSignature); got == "" {
		t.Fatalf("expected %s header to be set", apiclient.HeaderSignature)
	}
	if got := tunnel.lastRequest.Method; got != http.MethodDelete {
		t.Fatalf("expected DELETE method, got %s", got)
	}
	if got := tunnel.lastRequest.Path; got != "/api/v1/agents/agent-1" {
		t.Fatalf("unexpected path: %s", got)
	}
	if got := tunnel.lastRequest.Query; got != "deleteFiles=true&removeBranch=false" {
		t.Fatalf("unexpected query: %s", got)
	}
}

func TestControlChannelBrokerClient_StartAgentSignsTunneledRequest(t *testing.T) {
	tunnel := &mockControlChannelTunnel{connected: true}
	signer := &mockBrokerSigner{}
	client := &ControlChannelBrokerClient{
		manager: tunnel,
		signer:  signer,
	}

	_, err := client.StartAgent(
		context.Background(),
		"broker-1",
		"unused",
		"agent-1",
		"project-id-1",
		"run task",
		"/tmp/project",
		"project-slug",
		"",
		nil,
		nil,
		nil,
		nil,
		false,
	)
	if err != nil {
		t.Fatalf("StartAgent returned error: %v", err)
	}

	if !signer.called {
		t.Fatal("expected signer to be called")
	}
	if tunnel.lastRequest == nil {
		t.Fatal("expected tunneled request to be captured")
	}
	if got := headerValue(tunnel.lastRequest.Headers, apiclient.HeaderBrokerID); got != "broker-1" {
		t.Fatalf("expected %s header to be set, got %q", apiclient.HeaderBrokerID, got)
	}
	if got := tunnel.lastRequest.Method; got != http.MethodPost {
		t.Fatalf("expected POST method, got %s", got)
	}
	expectedPath := "/api/v1/agents/agent-1/start?projectId=project-id-1"
	if got := tunnel.lastRequest.Path; got != expectedPath {
		t.Fatalf("unexpected path: %s (expected %s)", got, expectedPath)
	}
}

func headerValue(headers map[string]string, name string) string {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}
