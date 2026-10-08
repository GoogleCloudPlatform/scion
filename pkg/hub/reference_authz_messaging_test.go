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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestStripClientAttachmentRefs(t *testing.T) {
	assert.Nil(t, stripClientAttachmentRefs(nil))

	md := map[string]string{"keep": "me"}
	assert.Equal(t, md, stripClientAttachmentRefs(md), "a map without the key is returned as is")

	in := map[string]string{"keep": "me", attachmentsMetadataKey: `[{"id":"x"}]`}
	out := stripClientAttachmentRefs(in)
	assert.Equal(t, map[string]string{"keep": "me"}, out)
	assert.Contains(t, in, attachmentsMetadataKey, "the caller's map is not changed")
}

// clientAttachmentMetadata stores two attachments the sending agent did not
// ingest (a direct-message upload and a file of another project) and
// returns metadata naming them the way the hub names attachments.
func clientAttachmentMetadata(t *testing.T, srv *Server) (map[string]string, []string) {
	t.Helper()
	ctx := context.Background()
	srv.mu.RLock()
	wcs := srv.webChatStore
	srv.mu.RUnlock()
	require.NotNil(t, wcs)
	var refs []AttachmentRef
	var ids []string
	for _, a := range []AttachmentMeta{
		{ID: tid("client-md-dm-upload"), ProjectID: "", Filename: "dm.txt", MimeType: "text/plain", Size: 1, UploadedBy: tid("client-md-someone")},
		{ID: tid("client-md-other-project"), ProjectID: tid("client-md-other-proj"), Filename: "b.txt", MimeType: "text/plain", Size: 1, UploadedBy: tid("client-md-someone")},
	} {
		a.CreatedAt = time.Now().UTC()
		require.NoError(t, wcs.CreateAttachment(ctx, a))
		refs = append(refs, AttachmentRef{ID: a.ID, Name: a.Filename, MimeType: a.MimeType, Size: a.Size})
		ids = append(ids, a.ID)
	}
	encoded, ok := attachmentRefsMetadata(refs)
	require.True(t, ok)
	return map[string]string{attachmentsMetadataKey: encoded, "keep": "me"}, ids
}

func postRefOutbound(t *testing.T, srv *Server, sender *store.Agent, identity Identity, req OutboundMessageRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+sender.ID+"/outbound-message", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(requestAuthCtx(r.Context(), identity))
	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, r, sender.ID)
	return rr
}

// Broker delivery to a user links attachments from message metadata; an
// agent cannot name attachments there itself.
func TestAgentOutbound_ClientAttachmentMetadataNotLinked(t *testing.T) {
	srv, s, project, sender, _, _, dispatcher, _ := paritySetup(t)
	wireWebBrokerForMentionTests(t, srv, s, project.ID, dispatcher)
	owner, err := s.GetUser(context.Background(), project.OwnerID)
	require.NoError(t, err)
	md, attachmentIDs := clientAttachmentMetadata(t, srv)

	rr := postRefOutbound(t, srv, sender, tokenBackedSender(t, s, sender), OutboundMessageRequest{
		Recipient: "user:" + owner.Email,
		Msg:       "files for you",
		Type:      "instruction",
		Metadata:  md,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var row *store.Message
	require.Eventually(t, func() bool {
		rows, err := s.ListMessages(context.Background(), store.MessageFilter{SenderID: sender.ID, RecipientID: owner.ID}, store.ListOptions{Limit: 10})
		if err != nil || len(rows.Items) != 1 {
			return false
		}
		row = &rows.Items[0]
		return true
	}, 5*time.Second, 20*time.Millisecond, "the broker path must persist the message")

	linked, err := srv.webChatStore.GetAttachmentsByMessage(context.Background(), row.ID)
	require.NoError(t, err)
	assert.Empty(t, linked, "attachments named in caller metadata are never linked")
	for _, id := range attachmentIDs {
		ids, err := srv.webChatStore.ListMessageIDsForAttachment(context.Background(), id, 20)
		require.NoError(t, err)
		assert.Empty(t, ids)
	}
}

// The agent-to-agent path of the outbound route drops the key as well, and
// keeps the rest of the caller's metadata.
func TestAgentMessage_ClientAttachmentMetadataRemoved(t *testing.T) {
	srv, s, _, sender, target, convID, dispatcher, _ := paritySetup(t)
	newChatV2WebChatStore(t, srv, s)
	md, _ := clientAttachmentMetadata(t, srv)

	rr := postRefOutbound(t, srv, sender, tokenBackedSender(t, s, sender), OutboundMessageRequest{
		ConversationRef: "conv:" + convID,
		Msg:             "files for you",
		Type:            "instruction",
		Metadata:        md,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	calls := dispatchesTo(dispatcher, target.ID)
	require.Len(t, calls, 1)
	assert.NotContains(t, calls[0].StructuredMessage.Metadata, attachmentsMetadataKey)
	assert.Equal(t, "me", calls[0].StructuredMessage.Metadata["keep"])
}
