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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type staticHMACSigner struct {
	secretKey []byte
}

func (s staticHMACSigner) Sign(_ context.Context, req *http.Request, brokerID string) error {
	return (&apiclient.HMACAuth{
		BrokerID:  brokerID,
		SecretKey: s.secretKey,
	}).ApplyAuth(req)
}

// TestBrokerHTTPTransport_MessageAgent_HMACNearLimitDelivered verifies that
// hub-to-broker message delivery succeeds with HMAC signing enabled for
// messages right up to the hub's 1 MiB intake limit, including markup-heavy
// text containing '<', '>', and '&' (ptone/scion#9877).
func TestBrokerHTTPTransport_MessageAgent_HMACNearLimitDelivered(t *testing.T) {
	secret := []byte("test-hmac-secret-key-32-bytes!!!")
	authMW := runtimebroker.NewBrokerAuthMiddleware(runtimebroker.BrokerAuthConfig{
		Enabled:              true,
		MaxClockSkew:         5 * time.Minute,
		SecretKey:            secret,
		AllowUnauthenticated: false,
	})

	const nearLimit = (1 << 20) - 4096 // just under hub's 1 MiB intake cap
	cases := []struct {
		name string
		text string
	}{
		{
			name: "plain text near 1 MiB",
			text: strings.Repeat("x", nearLimit),
		},
		{
			name: "markup heavy near 1 MiB",
			text: strings.Repeat("<div>a&b</div>", nearLimit/len("<div>a&b</div>")),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				gotRawLen int
				gotReq    runtimebroker.MessageRequest
			)
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				gotRawLen = len(raw)
				if err := json.Unmarshal(raw, &gotReq); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusOK)
			})

			srv := httptest.NewServer(authMW.Middleware(inner))
			defer srv.Close()

			structuredMsg := messages.NewInstruction("user:alice", "agent:test-agent", tc.text)
			structuredMsg.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{
				MessageID: "msg-9877",
				Msg:       structuredMsg,
				CreatedAt: time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC),
			})
			wantDelivery := structuredMsg.DeliveryText
			wantMsg := structuredMsg.Msg

			tr := newBrokerHTTPTransport(false, staticHMACSigner{secretKey: secret})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			err := tr.MessageAgent(ctx, "broker-1", srv.URL, "test-agent", "proj-1", "", false, structuredMsg)
			require.NoError(t, err, "near-1 MiB message must pass broker HMAC 2 MiB signed-body cap")

			assert.LessOrEqual(t, int64(gotRawLen), apiclient.MaxSignedRequestBodyBytes)
			assert.Equal(t, wantDelivery, gotReq.DeliveryText)
			require.NotNil(t, gotReq.StructuredMessage)
			assert.Equal(t, "user:alice", gotReq.StructuredMessage.Sender)
			assert.Equal(t, "agent:test-agent", gotReq.StructuredMessage.Recipient)
			assert.Equal(t, messages.TypeInstruction, gotReq.StructuredMessage.Type)
			assert.Empty(t, gotReq.StructuredMessage.Msg, "wire structured_message.msg must not duplicate delivery_text")
			assert.Empty(t, gotReq.StructuredMessage.DeliveryText, "wire structured_message.delivery_text must not duplicate top-level delivery_text")

			// Caller's StructuredMessage must not be mutated.
			assert.Equal(t, wantMsg, structuredMsg.Msg)
			assert.Equal(t, wantDelivery, structuredMsg.DeliveryText)
		})
	}
}

type capturingControlTunnel struct {
	lastReq *wsprotocol.RequestEnvelope
}

func (c *capturingControlTunnel) IsConnected(string) bool { return true }

func (c *capturingControlTunnel) TunnelRequest(_ context.Context, _ string, req *wsprotocol.RequestEnvelope) (*wsprotocol.ResponseEnvelope, error) {
	c.lastReq = req
	return &wsprotocol.ResponseEnvelope{StatusCode: http.StatusOK}, nil
}

// TestControlChannelBrokerClient_MessageAgent_SingleCopyNoHTMLEscape verifies
// that the control-channel message builder also sends the text once without
// HTML-escape expansion, and preserves structured_message.msg when
// DeliveryText is empty (ptone/scion#9877).
func TestControlChannelBrokerClient_MessageAgent_SingleCopyNoHTMLEscape(t *testing.T) {
	t.Run("rendered DeliveryText sent once without HTML escaping", func(t *testing.T) {
		tunnel := &capturingControlTunnel{}
		client := &ControlChannelBrokerClient{manager: tunnel}

		// ~500 KiB of markup-heavy text: 3x duplication or 6x HTML escaping
		// would exceed maxControlChannelBodySize (768 KiB), whereas a single
		// unescaped copy fits comfortably.
		const size = 500 << 10
		text := strings.Repeat("<a>&</a>", size/len("<a>&</a>"))
		structuredMsg := messages.NewInstruction("user:alice", "agent:test-agent", text)
		structuredMsg.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{
			MessageID: "msg-cc-9877",
			Msg:       structuredMsg,
			CreatedAt: time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC),
		})
		wantDelivery := structuredMsg.DeliveryText
		wantMsg := structuredMsg.Msg

		err := client.MessageAgent(context.Background(), "broker-1", "", "test-agent", "proj-1", "", false, structuredMsg)
		require.NoError(t, err)
		require.NotNil(t, tunnel.lastReq)
		assert.LessOrEqual(t, len(tunnel.lastReq.Body), maxControlChannelBodySize)

		var gotReq runtimebroker.MessageRequest
		require.NoError(t, json.Unmarshal(tunnel.lastReq.Body, &gotReq))
		assert.Equal(t, wantDelivery, gotReq.DeliveryText)
		require.NotNil(t, gotReq.StructuredMessage)
		assert.Equal(t, "user:alice", gotReq.StructuredMessage.Sender)
		assert.Empty(t, gotReq.StructuredMessage.Msg)
		assert.Empty(t, gotReq.StructuredMessage.DeliveryText)

		// Caller's StructuredMessage must not be mutated.
		assert.Equal(t, wantMsg, structuredMsg.Msg)
		assert.Equal(t, wantDelivery, structuredMsg.DeliveryText)
	})

	t.Run("unrendered StructuredMessage preserves msg", func(t *testing.T) {
		tunnel := &capturingControlTunnel{}
		client := &ControlChannelBrokerClient{manager: tunnel}

		structuredMsg := messages.NewInstruction("user:alice", "agent:test-agent", "hello <world> & friends")
		err := client.MessageAgent(context.Background(), "broker-1", "", "test-agent", "proj-1", "", false, structuredMsg)
		require.NoError(t, err)
		require.NotNil(t, tunnel.lastReq)
		assert.NotContains(t, string(tunnel.lastReq.Body), `\u003c`)

		var gotReq runtimebroker.MessageRequest
		require.NoError(t, json.Unmarshal(tunnel.lastReq.Body, &gotReq))
		assert.Empty(t, gotReq.DeliveryText)
		require.NotNil(t, gotReq.StructuredMessage)
		assert.Equal(t, "hello <world> & friends", gotReq.StructuredMessage.Msg)
	})
}
