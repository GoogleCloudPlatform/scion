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
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// getAttachmentFaultWCS fails GetAttachment.
type getAttachmentFaultWCS struct {
	WebChatStore
}

func (getAttachmentFaultWCS) GetAttachment(context.Context, string) (*AttachmentMeta, error) {
	return nil, errRefStoreFault
}

func TestBrokerUserMessage_LinksOnlySenderOwnedAttachments(t *testing.T) {
	srv, s, project, sender, _, _, dispatcher, _ := paritySetup(t)
	ctx := context.Background()
	wcs := newChatV2WebChatStore(t, srv, s)
	owner, err := s.GetUser(ctx, project.OwnerID)
	require.NoError(t, err)

	newAttachment := func(name, projectID, uploader string) AttachmentRef {
		meta := AttachmentMeta{ID: tid("broker-link-" + name), ProjectID: projectID, Filename: name + ".txt",
			MimeType: "text/plain", Size: 1, UploadedBy: uploader, CreatedAt: time.Now().UTC()}
		require.NoError(t, wcs.CreateAttachment(ctx, meta))
		return AttachmentRef{ID: meta.ID, Name: meta.Filename, MimeType: meta.MimeType, Size: meta.Size}
	}
	own := newAttachment("own", project.ID, sender.ID)
	ownDMUpload := newAttachment("own-dm", "", sender.ID)
	otherProject := newAttachment("other-project", tid("broker-link-other-proj"), tid("broker-link-other-agent"))
	otherUsersUpload := newAttachment("other-user", "", tid("broker-link-other-user"))

	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(nil, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())

	deliver := func(subscriberStore WebChatStore, text string, refs ...AttachmentRef) string {
		t.Helper()
		proxy.webChatStore = subscriberStore
		encoded, ok := attachmentRefsMetadata(refs)
		require.True(t, ok)
		// Delivered straight to the subscriber, as if the metadata had
		// reached it unfiltered.
		proxy.deliverToUser(ctx, project.ID, "topic", &messages.StructuredMessage{
			Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
			Type: messages.TypeInstruction, Sender: "agent:" + sender.Slug, SenderID: sender.ID,
			Recipient: "user:" + owner.Email, RecipientID: owner.ID, Msg: text,
			Metadata: map[string]string{attachmentsMetadataKey: encoded},
		})
		rows, err := s.ListMessages(ctx, store.MessageFilter{SenderID: sender.ID, RecipientID: owner.ID}, store.ListOptions{Limit: 50})
		require.NoError(t, err)
		for _, r := range rows.Items {
			if r.Msg == text {
				return r.ID
			}
		}
		t.Fatalf("message %q was not persisted", text)
		return ""
	}
	linkedIDs := func(msgID string) []string {
		got, err := wcs.GetAttachmentsByMessage(ctx, msgID)
		require.NoError(t, err)
		ids := make([]string, 0, len(got))
		for _, a := range got {
			ids = append(ids, a.ID)
		}
		return ids
	}

	msgID := deliver(wcs, "mixed files", own, ownDMUpload, otherProject, otherUsersUpload)
	assert.ElementsMatch(t, []string{own.ID, ownDMUpload.ID}, linkedIDs(msgID),
		"only the sender's own files are linked")

	msgID = deliver(getAttachmentFaultWCS{WebChatStore: wcs}, "lookup fails", own)
	assert.Empty(t, linkedIDs(msgID), "a lookup error links nothing")
}
