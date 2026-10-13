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
	"errors"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// failingControlChannelSigner always fails, for testing that a signing
// failure — proven to occur before the tunnel is ever used — classifies as
// agentkeys.ErrNotDispatched rather than an uncertain outcome.
type failingControlChannelSigner struct{}

type mockControlChannelTunnel struct {
	connected   bool
	lastBroker  string
	lastRequest *wsprotocol.RequestEnvelope
	status      int               // response status; 0 means 200
	body        []byte            // response body; nil means no body
	headers     map[string]string // response headers; nil means none
	// err, when non-nil, makes TunnelRequest fail instead of returning a
	// response — used by keys fault-injection tests (e.g. a broker
	// reconnect or response-loss mid-flight, after the pre-send connection
	// check already passed).
	err error
	// calls counts TunnelRequest invocations, so tests can prove a caller
	// made at most one dispatch attempt (no reconnect/retry).
	calls int
}

type mockBrokerSigner struct {
	called bool
}

func (failingControlChannelSigner) Sign(context.Context, *http.Request, string) error {
	return errors.New("boom: no broker secret")
}

func (m *mockControlChannelTunnel) IsConnected(string) bool {
	return m.connected
}

func (m *mockControlChannelTunnel) TunnelRequest(_ context.Context, brokerID string, req *wsprotocol.RequestEnvelope) (*wsprotocol.ResponseEnvelope, error) {
	m.calls++
	m.lastBroker = brokerID
	m.lastRequest = req
	if m.err != nil {
		return nil, m.err
	}
	status := m.status
	if status == 0 {
		status = http.StatusOK
	}
	return wsprotocol.NewResponseEnvelope(req.RequestID, status, m.headers, m.body), nil
}

func (m *mockBrokerSigner) Sign(_ context.Context, req *http.Request, brokerID string) error {
	m.called = true
	req.Header.Set(apiclient.HeaderBrokerID, brokerID)
	req.Header.Set(apiclient.HeaderTimestamp, "1700000000")
	req.Header.Set(apiclient.HeaderNonce, "nonce")
	req.Header.Set(apiclient.HeaderSignature, "signature")
	return nil
}
