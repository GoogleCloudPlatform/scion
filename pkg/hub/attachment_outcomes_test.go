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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestBrokerAttachmentOutcomes_Counted: per-file attachment delivery
// outcomes a broker reports next to its failure report are counted under a
// fixed set of keys and change no message (ptone/scion#4596).
func TestBrokerAttachmentOutcomes_Counted(t *testing.T) {
	srv, _ := testServer(t)
	before := attachmentDeliveryOutcomes.Snapshot()
	body, err := json.Marshal(messageDeliveryFailuresRequest{AttachmentOutcomes: []attachmentDeliveryOutcome{
		{MessageID: "m1", AttachmentID: "a1", Outcome: "delivered"},
		{MessageID: "m1", AttachmentID: "a2", Outcome: "not_delivered", Reason: "failed verification"},
		{MessageID: "m1", AttachmentID: "a3", Outcome: "not_delivered", Reason: "anything else"},
	}})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/b1/message-failures", bytes.NewReader(body))
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("b1")))
	rr := httptest.NewRecorder()
	srv.handleRuntimeBrokerByIDInternal(rr, req, "b1", "message-failures")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	after := attachmentDeliveryOutcomes.Snapshot()
	require.Equal(t, before["delivered"]+1, after["delivered"])
	require.Equal(t, before["not_delivered: failed verification"]+1, after["not_delivered: failed verification"])
	require.Equal(t, before["not_delivered: other"]+1, after["not_delivered: other"])
	require.NotContains(t, after, "not_delivered: anything else", "a report cannot add keys")
}

// TestDispatch_SendsInboxRetention: a dispatch that carries attachments
// sends the inbox retention setting with them.
func TestDispatch_SendsInboxRetention(t *testing.T) {
	f := newSendFixture(t)
	b, err := f.st.GetRuntimeBroker(context.Background(), f.brokerID)
	require.NoError(t, err)
	b.Capabilities = &store.BrokerCapabilities{AttachmentDelivery: true}
	require.NoError(t, f.st.UpdateRuntimeBroker(context.Background(), b))
	f.srv.SetOperationalSettings(artifactsOpsWith(t, "", `{"attachment_inbox_retention_days": 3}`))
	id := f.upload(t, f.sender, "x.txt", "x")
	rec := f.message(t, f.sender, f.target, "see", id)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	call := f.lastDispatch(t, f.target)
	client := &retentionCaptureClient{}
	d := NewHTTPAgentDispatcherWithClient(f.st, client, false, slog.Default())
	f.srv.SetDispatcher(d)
	require.NoError(t, d.DispatchAgentMessage(context.Background(), f.target, call.Message, false, call.StructuredMessage))
	require.Equal(t, 3, client.retention)
	require.Len(t, client.wire, 1)
}

// retentionCaptureClient records the retention and wire attachments of a
// message dispatch.
type retentionCaptureClient struct {
	mockRuntimeBrokerClient
	retention int
	wire      []messages.WireAttachment
}

func (c *retentionCaptureClient) MessageAgent(ctx context.Context, _, _, _, _, _ string, _ bool, _ *messages.StructuredMessage) error {
	c.retention = dispatchRetentionFromContext(ctx)
	c.wire = dispatchAttachmentsFromContext(ctx)
	return nil
}
