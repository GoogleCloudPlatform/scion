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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestValidateAttributed_LegacyPath_ConversationIDStamped verifies that the
// conversation_id check (ValidateAttributed) fires on the legacy user-to-agent
// message path. Before the D1 sentinel removal (DEF-41, #1401), the legacy
// path injected a synthetic ConversationID that masked the check. After D1,
// ValidateLegacyMessage no longer checks ConversationID at all; that
// responsibility moved to ValidateAttributed, which runs after the
// conversation attribution layer sets a real ConversationID.
//
// This test proves the full chain is wired:
//
//  1. ValidateLegacyMessage accepts the message without a ConversationID.
//  2. Conversation attribution resolves or creates a conversation.
//  3. ValidateAttributed runs and accepts the (non-empty) ConversationID.
//  4. The message is persisted with the attributed ConversationID.
//
// If ValidateAttributed were removed or bypassed, a message could be
// persisted with an empty ConversationID after attribution fails (Tranche G).
// The companion assertion (TestValidateAttributed_RejectsEmpty) proves the
// function body is load-bearing: it rejects "" today and will reject it on
// the production path once B10's nil-guard is lifted.
func TestValidateAttributed_LegacyPath_ConversationIDStamped(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("d4-project")
	agentID := tid("d4-agent")
	userID := DevUserID // must match the always-override sender identity
	agentSlug := "d4-agent"

	if err := s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "d4-project",
		Slug: "d4-project",
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	brokerID := tid("d4-broker")
	if err := s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "d4-broker",
		Slug:   "d4-broker",
		Status: store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("CreateRuntimeBroker: %v", err)
	}
	if err := s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  projectID,
		BrokerID:   brokerID,
		BrokerName: "d4-broker",
		Status:     store.BrokerStatusOnline,
	}); err != nil {
		t.Fatalf("AddProjectProvider: %v", err)
	}
	if err := s.CreateAgent(ctx, &store.Agent{
		ID:              agentID,
		Name:            "d4-agent",
		Slug:            agentSlug,
		ProjectID:       projectID,
		RuntimeBrokerID: brokerID,
		Phase:           "running",
		Visibility:      store.VisibilityPrivate,
	}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	_ = s.CreateUser(ctx, &store.User{
		ID:          userID,
		Email:       "dev@localhost",
		DisplayName: "Development User",
	})
	srv.SetDispatcher(&recordingDispatcher{})

	// Send a message WITHOUT a pre-resolved ConversationID. This forces the
	// handler to run server-side conversation attribution, which is the
	// legacy path where ValidateAttributed fires after D1.
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+agentSlug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Sender:    "user:dev",
				SenderID:  userID,
				Recipient: "agent:" + agentSlug,
				Msg:       "D4 validation choke point test",
				Type:      messages.TypeInstruction,
				// ConversationID intentionally omitted — server must attribute.
			},
		})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify the persisted message has a non-empty ConversationID. This
	// proves: (a) conversation attribution ran, (b) ValidateAttributed
	// accepted the result, and (c) the message was persisted with the
	// attributed ConversationID.
	res, err := s.ListMessages(ctx, store.MessageFilter{AgentID: agentID}, store.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(res.Items) == 0 {
		t.Fatal("expected at least one persisted message, got none")
	}

	found := false
	for _, m := range res.Items {
		if m.Msg == "D4 validation choke point test" {
			found = true
			if m.ConversationID == "" {
				t.Fatalf("persisted message has empty ConversationID; "+
					"the validation choke point (ValidateAttributed) "+
					"should have ensured a non-empty ConversationID "+
					"after attribution. message_id=%s", m.ID)
			}
			break
		}
	}
	if !found {
		t.Fatal("test message not found in persisted messages")
	}
}

// TestValidateAttributed_RejectsEmpty proves that ValidateAttributed rejects
// an empty ConversationID. This is the load-bearing assertion that pairs with
// TestValidateAttributed_LegacyPath_ConversationIDStamped: if the function
// body were replaced with `return nil`, this test would fail.
//
// Under B10, every production call site guards ValidateAttributed behind
// `if convResult != nil`, and every non-nil convResult carries a uuid.UUID
// ConversationID that is never empty. The check is structurally pre-placed;
// it becomes reachable at Tranche G when derivation failure becomes fatal.
func TestValidateAttributed_RejectsEmpty(t *testing.T) {
	err := messaging.ValidateAttributed("")
	if err == nil {
		t.Fatal("ValidateAttributed must reject an empty conversation_id; " +
			"if this check is removed, the legacy path can persist messages " +
			"without conversation attribution (Tranche G regression)")
	}
}
