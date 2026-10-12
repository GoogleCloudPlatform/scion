// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
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
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestChatDMs_UnreadMatchesNativeHistory(t *testing.T) {
	for _, envelope := range []bool{false, true} {
		mode := "legacy"
		if envelope {
			mode = "envelope"
		}
		for _, tc := range []struct {
			name        string
			visible     bool
			hidden      string
			absent      bool
			noWatermark bool
			alreadyRead bool
		}{
			{name: "empty with deleted watermark"},
			{name: "external only", hidden: "external"},
			{name: "mention only", hidden: "mention"},
			{name: "promoted only", hidden: "promoted"},
			{name: "never used", hidden: "unstamped", absent: true},
			{name: "visible unread", visible: true},
			{name: "visible read", visible: true, alreadyRead: true},
			{name: "visible without activity watermark", visible: true, noWatermark: true},
			{name: "visible then external", visible: true, hidden: "external"},
			{name: "visible then mention", visible: true, hidden: "mention"},
			{name: "read then external", visible: true, hidden: "external", alreadyRead: true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				srv, s, wcs, project, _ := setupSendTest(t)
				ctx := context.Background()
				settings := newFakeHubSettingStore()
				ops := NewOperationalSettings(settings, emptyKoanf(), emptyKoanf())
				settings.seed("messaging", json.RawMessage(`{"conversation_envelope_switch":`+strconv.FormatBool(envelope)+`}`))
				if _, err := ops.Refresh(ctx); err != nil {
					t.Fatal(err)
				}
				srv.SetOperationalSettings(ops)
				peer := rsAgent(t, s, "unread-peer", project.ID)
				key, err := messages.DMConversationKey("agent", peer, "user", DevUserID)
				if err != nil {
					t.Fatal(err)
				}
				convID := ""
				if !tc.absent {
					convID = seedConversation(t, s, "native", key, "direct")
				}
				base := time.Now().UTC().Add(-time.Minute)
				create := func(id, channel, kind, thread, conversation string, at time.Time) string {
					t.Helper()
					msg := &store.Message{ID: tid(id), ProjectID: project.ID, Sender: "agent:unread-peer",
						SenderID: peer, Recipient: "user:dev@localhost", RecipientID: DevUserID,
						Msg: id, Type: kind, Channel: channel, ThreadID: thread, ConversationID: conversation, CreatedAt: at}
					if err := s.CreateMessage(ctx, msg); err != nil {
						t.Fatal(err)
					}
					return msg.ID
				}
				wantLast := ""
				watermark := tid("deleted-message")
				if tc.visible {
					thread := key
					if envelope {
						// Native agent replies can carry only the conversation ID.
						thread = ""
					}
					wantLast = create("visible", "web", messages.TypeChat, thread, convID, base)
					watermark = wantLast
				}
				switch tc.hidden {
				case "external":
					watermark = create("external", "discord", messages.TypeChat, key, convID, base.Add(time.Second))
				case "mention":
					watermark = create("mention", "web", messages.TypeMention, key, convID, base.Add(time.Second))
				case "promoted":
					other := seedConversation(t, s, "native", "promoted", "group")
					watermark = create("promoted", "web", messages.TypeChat, "promoted", other, base.Add(time.Second))
				case "unstamped":
					watermark = create("unstamped", "web", messages.TypeChat, key, "", base)
					if !envelope {
						wantLast = watermark
					}
				}
				if tc.noWatermark {
					watermark = ""
				}
				if err := wcs.UpsertDM(ctx, WebChatDM{ConversationKey: key, ParticipantID: DevUserID,
					PeerID: peer, PeerKind: "agent", LastMessageID: watermark}); err != nil {
					t.Fatal(err)
				}
				if tc.alreadyRead {
					if err := wcs.SetReadState(ctx, DevUserID, key, wantLast); err != nil {
						t.Fatal(err)
					}
				}
				list := func(wantUnread bool) {
					t.Helper()
					rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/dms", nil)
					if rec.Code != http.StatusOK {
						t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
					}
					var resp chatDMListResponse
					if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
						t.Fatal(err)
					}
					if len(resp.DMs) != 1 || resp.DMs[0].HasUnread != wantUnread || resp.DMs[0].LastMessageID != wantLast {
						t.Fatalf("DMs = %+v; want unread=%v last=%q", resp.DMs, wantUnread, wantLast)
					}
					if wantLast == "" && (resp.DMs[0].LastMessagePreview != "" || resp.DMs[0].LastMessageSender != "") {
						t.Fatalf("invisible message leaked into preview: %+v", resp.DMs[0])
					}
				}
				list(wantLast != "" && !tc.alreadyRead)
				if wantLast != "" {
					rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+key+"/read", map[string]string{"messageId": wantLast})
					if rec.Code != http.StatusOK {
						t.Fatalf("read: %d %s", rec.Code, rec.Body.String())
					}
					list(false)
				}
			})
		}
	}
}
