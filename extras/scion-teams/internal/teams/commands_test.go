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

package teams

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testBrokerWithStore creates a TeamsBroker wired with an in-memory SQLite
// store, a mock sender, and optionally a mock hub server. The broker is ready
// for command/callback handler use.
func testBrokerWithStore(t *testing.T, hubHandler http.HandlerFunc) (*TeamsBroker, *mockSender) {
	t.Helper()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	broker := NewBroker(log)
	broker.config = &Config{
		AppID:          "test-bot-id",
		MentionRouting: true,
	}

	store, err := NewSQLiteStore(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	broker.store = store

	ms := &mockSender{}

	// Set up mock hub server if provided.
	if hubHandler != nil {
		hubServer := httptest.NewServer(hubHandler)
		t.Cleanup(hubServer.Close)
		broker.hubClient = NewHubClient(hubServer.URL, "", "", log)
	}

	// Wire a sender that captures sent activities.
	senderServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var activity Activity
		if err := json.NewDecoder(r.Body).Decode(&activity); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ms.addSent(activity)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ActivityResponse{ID: "resp-1"})
	}))
	t.Cleanup(senderServer.Close)

	broker.sender = &Sender{
		tokenProvider: testTokenProvider(t),
		httpClient:    senderServer.Client(),
		log:           log,
	}
	// Override the sender's httpClient to point to our test server.
	broker.sender.httpClient = &http.Client{
		Timeout:   5 * time.Second,
		Transport: &testTransport{baseURL: senderServer.URL},
	}

	broker.commandHandler = NewCommandHandler(broker, log)
	broker.callbackHandler = NewCallbackHandler(broker, log)

	return broker, ms
}

// testTokenProvider creates a TokenProvider backed by a mock token server.
// Uses the existing newTestTokenProvider from send_test.go pattern.
func testTokenProvider(t *testing.T) *TokenProvider {
	t.Helper()
	tp, ts := newTestTokenProvider(t)
	t.Cleanup(ts.Close)
	return tp
}

// mockSender captures activities sent via the sender.
type mockSender struct {
	sent []Activity
}

func (m *mockSender) addSent(a Activity) {
	m.sent = append(m.sent, a)
}

// testTransport redirects all requests to a test server URL.
type testTransport struct {
	baseURL string
}

func (t *testTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Redirect the request to our test server.
	testURL := t.baseURL + req.URL.Path
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, testURL, req.Body)
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header
	return http.DefaultTransport.RoundTrip(newReq)
}

func testActivity(text string) *Activity {
	return &Activity{
		Type: "message",
		ID:   "act-1",
		From: ChannelAccount{
			ID:          "user-1",
			Name:        "Test User",
			AadObjectID: "aad-user-1",
		},
		Conversation: ConversationAccount{
			ID:               "conv-1",
			ConversationType: "channel",
		},
		Recipient: ChannelAccount{
			ID:   "test-bot-id",
			Name: "Scion",
		},
		ServiceURL: "https://smba.trafficmanager.net/test/",
		Text:       text,
	}
}

func TestCommandDispatch_KnownCommands(t *testing.T) {
	broker, _ := testBrokerWithStore(t, nil)
	handler := broker.commandHandler

	knownCommands := []string{"setup", "unlink", "agents", "status", "register", "unregister", "help"}
	for _, cmd := range knownCommands {
		activity := testActivity(cmd)
		handled, err := handler.Handle(context.Background(), activity)
		// All known commands should be handled (handled=true).
		// Some may return errors due to missing hub/store state, but
		// they should still be recognized as commands.
		assert.True(t, handled, "command %q should be handled", cmd)
		_ = err // errors expected for commands needing hub/store
	}
}

func TestCommandDispatch_UnknownCommand(t *testing.T) {
	broker, _ := testBrokerWithStore(t, nil)
	handler := broker.commandHandler

	activity := testActivity("hello world")
	handled, err := handler.Handle(context.Background(), activity)
	assert.False(t, handled)
	assert.NoError(t, err)
}

func TestCommandDispatch_CaseInsensitive(t *testing.T) {
	broker, _ := testBrokerWithStore(t, nil)
	handler := broker.commandHandler

	// Commands should be case-insensitive.
	for _, cmd := range []string{"HELP", "Help", "hElP"} {
		activity := testActivity(cmd)
		handled, err := handler.Handle(context.Background(), activity)
		assert.True(t, handled, "command %q should be handled (case-insensitive)", cmd)
		_ = err
	}
}

func TestCommandDispatch_EmptyText(t *testing.T) {
	broker, _ := testBrokerWithStore(t, nil)
	handler := broker.commandHandler

	activity := testActivity("")
	handled, err := handler.Handle(context.Background(), activity)
	assert.False(t, handled)
	assert.NoError(t, err)
}

func TestCommandDispatch_StripsBotMention(t *testing.T) {
	broker, _ := testBrokerWithStore(t, nil)
	handler := broker.commandHandler

	// Activity with bot mention in entities.
	activity := testActivity("<at>Scion</at> help")
	activity.Entities = []Entity{
		{
			Type:      "mention",
			Mentioned: ChannelAccount{ID: "test-bot-id", Name: "Scion"},
			Text:      "<at>Scion</at>",
		},
	}

	handled, err := handler.Handle(context.Background(), activity)
	assert.True(t, handled)
	_ = err
}

func TestSetupCommand_WithProjectSlug(t *testing.T) {
	// Mock hub that returns projects via the broker endpoint.
	hubHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/broker/projects":
			json.NewEncoder(w).Encode(hubProjectsResponse{
				Projects: []hubProject{
					{ID: "proj-1", Name: "My Project", Slug: "my-project"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	broker, ms := testBrokerWithStore(t, hubHandler)
	handler := broker.commandHandler

	// Pre-create a user mapping (registration required before setup).
	err := broker.store.CreateUserMapping(context.Background(), &TeamsUserMapping{
		TeamsUserID:      "aad-user-1",
		TeamsDisplayName: "Test User",
		ScionUserID:      "scion-1",
		ScionEmail:       "user@example.com",
		LinkedAt:         time.Now(),
	})
	require.NoError(t, err)

	activity := testActivity("setup my-project")
	handled, cmdErr := handler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, cmdErr)

	// Verify channel link was created.
	link, lookupErr := broker.store.GetChannelLink(context.Background(), "conv-1")
	require.NoError(t, lookupErr)
	require.NotNil(t, link)
	assert.Equal(t, "my-project", link.ProjectSlug)
	assert.Equal(t, "proj-1", link.ProjectID)
	assert.True(t, link.Active)

	// Verify a reply was sent.
	assert.NotEmpty(t, ms.sent, "expected a reply to be sent")
}

func TestSetupCommand_AlreadyLinked(t *testing.T) {
	broker, ms := testBrokerWithStore(t, nil)
	handler := broker.commandHandler

	// Pre-create a channel link.
	err := broker.store.CreateChannelLink(context.Background(), &ChannelLink{
		ConversationID: "conv-1",
		ProjectID:      "proj-1",
		ProjectSlug:    "existing-project",
		LinkedAt:       time.Now(),
		Active:         true,
	})
	require.NoError(t, err)

	activity := testActivity("setup new-project")
	handled, cmdErr := handler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, cmdErr)

	// Should reply saying already linked.
	require.NotEmpty(t, ms.sent)
	assert.Contains(t, ms.sent[0].Text, "already linked")
}

func TestSetupCommand_NoSlug(t *testing.T) {
	// Mock hub that returns projects via the broker endpoint.
	hubHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/broker/projects":
			json.NewEncoder(w).Encode(hubProjectsResponse{
				Projects: []hubProject{
					{ID: "proj-1", Name: "My Project", Slug: "my-project"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	broker, ms := testBrokerWithStore(t, hubHandler)
	handler := broker.commandHandler

	// Pre-create a user mapping (registration required before setup).
	err := broker.store.CreateUserMapping(context.Background(), &TeamsUserMapping{
		TeamsUserID:      "aad-user-1",
		TeamsDisplayName: "Test User",
		ScionUserID:      "scion-1",
		ScionEmail:       "user@example.com",
		LinkedAt:         time.Now(),
	})
	require.NoError(t, err)

	activity := testActivity("setup")
	handled, cmdErr := handler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, cmdErr)

	// Should send a card with project buttons.
	require.NotEmpty(t, ms.sent)
	assert.NotEmpty(t, ms.sent[0].Attachments, "expected an Adaptive Card reply with project buttons")
}

func TestSetupCommand_NotRegistered(t *testing.T) {
	broker, ms := testBrokerWithStore(t, nil)
	handler := broker.commandHandler

	activity := testActivity("setup")
	handled, err := handler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, err)

	// Should tell user to register first.
	require.NotEmpty(t, ms.sent)
	assert.Contains(t, ms.sent[0].Text, "register")
}

func TestUnlinkCommand(t *testing.T) {
	broker, ms := testBrokerWithStore(t, nil)
	handler := broker.commandHandler

	// Pre-create a channel link with same user who will unlink.
	err := broker.store.CreateChannelLink(context.Background(), &ChannelLink{
		ConversationID: "conv-1",
		ProjectID:      "proj-1",
		ProjectSlug:    "test-project",
		LinkedBy:       "aad-user-1", // matches testActivity's AadObjectID
		LinkedAt:       time.Now(),
		Active:         true,
	})
	require.NoError(t, err)

	activity := testActivity("unlink")
	handled, cmdErr := handler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, cmdErr)

	// Verify link was deleted.
	link, err := broker.store.GetChannelLink(context.Background(), "conv-1")
	require.NoError(t, err)
	assert.Nil(t, link)

	// Should reply confirming unlink.
	require.NotEmpty(t, ms.sent)
	assert.Contains(t, ms.sent[0].Text, "unlinked")
}

func TestUnlinkCommand_Unauthorized(t *testing.T) {
	broker, ms := testBrokerWithStore(t, nil)
	handler := broker.commandHandler

	// Pre-create a channel link with a different user.
	err := broker.store.CreateChannelLink(context.Background(), &ChannelLink{
		ConversationID: "conv-1",
		ProjectID:      "proj-1",
		ProjectSlug:    "test-project",
		LinkedBy:       "aad-other-user",
		LinkedAt:       time.Now(),
		Active:         true,
	})
	require.NoError(t, err)

	// Activity from a different user.
	activity := testActivity("unlink")
	handled, cmdErr := handler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, cmdErr)

	// Link should NOT be deleted.
	link, err := broker.store.GetChannelLink(context.Background(), "conv-1")
	require.NoError(t, err)
	assert.NotNil(t, link)

	// Should reply with authorization error.
	require.NotEmpty(t, ms.sent)
	assert.Contains(t, ms.sent[0].Text, "Only the user who linked")
}

func TestUnlinkCommand_NotLinked(t *testing.T) {
	broker, ms := testBrokerWithStore(t, nil)
	handler := broker.commandHandler

	activity := testActivity("unlink")
	handled, err := handler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, err)

	require.NotEmpty(t, ms.sent)
	assert.Contains(t, ms.sent[0].Text, "not linked")
}

func TestAgentsCommand(t *testing.T) {
	hubHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/proj-1/agents" {
			json.NewEncoder(w).Encode(hubAgentsResponse{
				Agents: []hubAgent{
					{ID: "a1", Slug: "dev-1", Activity: "coding", Phase: "running"},
					{ID: "a2", Slug: "reviewer", Activity: "reviewing", Phase: "running"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	broker, ms := testBrokerWithStore(t, hubHandler)
	handler := broker.commandHandler

	// Pre-create a channel link.
	err := broker.store.CreateChannelLink(context.Background(), &ChannelLink{
		ConversationID: "conv-1",
		ProjectID:      "proj-1",
		ProjectSlug:    "test-project",
		LinkedAt:       time.Now(),
		Active:         true,
	})
	require.NoError(t, err)

	activity := testActivity("agents")
	handled, cmdErr := handler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, cmdErr)

	// Should send a card with agent info.
	require.NotEmpty(t, ms.sent)

	// Verify agents were cached.
	cached, err := broker.store.GetProjectAgents(context.Background(), "proj-1")
	require.NoError(t, err)
	require.NotNil(t, cached)
	assert.ElementsMatch(t, []string{"dev-1", "reviewer"}, cached.AgentSlugs)
}

func TestStatusCommand_ProjectOverview(t *testing.T) {
	hubHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/proj-1/agents" {
			json.NewEncoder(w).Encode(hubAgentsResponse{
				Agents: []hubAgent{
					{ID: "a1", Slug: "dev-1", Phase: "running"},
					{ID: "a2", Slug: "dev-2", Phase: "stopped"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	broker, ms := testBrokerWithStore(t, hubHandler)

	err := broker.store.CreateChannelLink(context.Background(), &ChannelLink{
		ConversationID: "conv-1",
		ProjectID:      "proj-1",
		ProjectSlug:    "test-project",
		LinkedAt:       time.Now(),
		Active:         true,
	})
	require.NoError(t, err)

	activity := testActivity("status")
	handled, cmdErr := broker.commandHandler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, cmdErr)

	require.NotEmpty(t, ms.sent)
}

func TestStatusCommand_SpecificAgent(t *testing.T) {
	hubHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/proj-1/agents" {
			json.NewEncoder(w).Encode(hubAgentsResponse{
				Agents: []hubAgent{
					{ID: "a1", Slug: "dev-1", Activity: "coding feature X", Phase: "running"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	broker, ms := testBrokerWithStore(t, hubHandler)

	err := broker.store.CreateChannelLink(context.Background(), &ChannelLink{
		ConversationID: "conv-1",
		ProjectID:      "proj-1",
		ProjectSlug:    "test-project",
		LinkedAt:       time.Now(),
		Active:         true,
	})
	require.NoError(t, err)

	activity := testActivity("status dev-1")
	handled, cmdErr := broker.commandHandler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, cmdErr)

	require.NotEmpty(t, ms.sent)
}

func TestHelpCommand(t *testing.T) {
	broker, ms := testBrokerWithStore(t, nil)
	handler := broker.commandHandler

	activity := testActivity("help")
	handled, err := handler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, err)

	// Should send a card with command list.
	require.NotEmpty(t, ms.sent)
	// The reply should have an adaptive card attachment.
	assert.NotEmpty(t, ms.sent[0].Attachments)
}

func TestRegisterCommand_NoHubClient(t *testing.T) {
	broker, ms := testBrokerWithStore(t, nil)
	handler := broker.commandHandler

	activity := testActivity("register")
	handled, err := handler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, err)

	require.NotEmpty(t, ms.sent)
	assert.Contains(t, ms.sent[0].Text, "Hub client not configured")
}

func TestRegisterCommand_Success(t *testing.T) {
	hubHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/teams/link" {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"status": "registered"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	broker, ms := testBrokerWithStore(t, hubHandler)
	handler := broker.commandHandler

	activity := testActivity("register")
	handled, err := handler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, err)

	// Should send an Adaptive Card with the link code.
	require.NotEmpty(t, ms.sent)
	assert.NotEmpty(t, ms.sent[0].Attachments, "expected an Adaptive Card reply")
}

func TestRegisterCommand_AlreadyLinked(t *testing.T) {
	broker, ms := testBrokerWithStore(t, nil)
	handler := broker.commandHandler

	// Pre-create a user mapping.
	err := broker.store.CreateUserMapping(context.Background(), &TeamsUserMapping{
		TeamsUserID:      "aad-user-1",
		TeamsDisplayName: "Test User",
		ScionUserID:      "scion-1",
		ScionEmail:       "user@example.com",
		LinkedAt:         time.Now(),
	})
	require.NoError(t, err)

	activity := testActivity("register")
	handled, cmdErr := handler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, cmdErr)

	require.NotEmpty(t, ms.sent)
	assert.Contains(t, ms.sent[0].Text, "already linked")
}

func TestUnregisterCommand_NotLinked(t *testing.T) {
	broker, ms := testBrokerWithStore(t, nil)
	handler := broker.commandHandler

	activity := testActivity("unregister")
	handled, err := handler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, err)

	require.NotEmpty(t, ms.sent)
	assert.Contains(t, ms.sent[0].Text, "not linked")
}

func TestUnregisterCommand_Success(t *testing.T) {
	broker, ms := testBrokerWithStore(t, nil)
	handler := broker.commandHandler

	// Pre-create a user mapping.
	err := broker.store.CreateUserMapping(context.Background(), &TeamsUserMapping{
		TeamsUserID:      "aad-user-1",
		TeamsDisplayName: "Test User",
		ScionUserID:      "scion-1",
		ScionEmail:       "user@example.com",
		LinkedAt:         time.Now(),
	})
	require.NoError(t, err)

	activity := testActivity("unregister")
	handled, cmdErr := handler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, cmdErr)

	require.NotEmpty(t, ms.sent)
	assert.Contains(t, ms.sent[0].Text, "unlinked")

	// Verify mapping was deleted.
	mapping, err := broker.store.GetUserMapping(context.Background(), "aad-user-1")
	require.NoError(t, err)
	assert.Nil(t, mapping)
}

func TestRegisterCommand_PollConfirmation(t *testing.T) {
	// Track number of status poll requests so we can respond differently.
	pollCount := 0

	hubHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/teams/link":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"status": "registered"})
		case r.URL.Path == "/api/v1/teams/link/status":
			pollCount++
			if pollCount >= 2 {
				// Second poll: confirmed.
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status": "confirmed",
					"user": map[string]string{
						"id":    "scion-user-42",
						"email": "test@example.com",
					},
				})
			} else {
				// First poll: still pending.
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status": "pending",
				})
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	broker, ms := testBrokerWithStore(t, hubHandler)
	handler := broker.commandHandler

	activity := testActivity("register")
	handled, err := handler.Handle(context.Background(), activity)
	assert.True(t, handled)
	assert.NoError(t, err)

	// Should have sent the Adaptive Card with the code.
	require.NotEmpty(t, ms.sent)
	assert.NotEmpty(t, ms.sent[0].Attachments, "expected an Adaptive Card reply")

	// Wait for the polling goroutine to complete confirmation.
	// The ticker fires every 10s, but we can't wait that long in tests.
	// Instead, verify that the pending link was registered.
	handler.pendingMu.Lock()
	assert.Len(t, handler.pendingLinks, 1, "expected one pending link")
	handler.pendingMu.Unlock()
}

func TestRegisterCommand_CancelsPreviousPending(t *testing.T) {
	hubHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/teams/link":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"status": "registered"})
		case r.URL.Path == "/api/v1/teams/link/status":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "pending",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	broker, _ := testBrokerWithStore(t, hubHandler)
	handler := broker.commandHandler

	// Register twice — the second should cancel the first.
	activity1 := testActivity("register")
	handled, err := handler.Handle(context.Background(), activity1)
	assert.True(t, handled)
	assert.NoError(t, err)

	handler.pendingMu.Lock()
	firstPending := handler.pendingLinks["aad-user-1"]
	firstCode := firstPending.Code
	handler.pendingMu.Unlock()

	// Register again.
	activity2 := testActivity("register")
	handled, err = handler.Handle(context.Background(), activity2)
	assert.True(t, handled)
	assert.NoError(t, err)

	handler.pendingMu.Lock()
	secondPending := handler.pendingLinks["aad-user-1"]
	handler.pendingMu.Unlock()

	// The new pending link should have a different code.
	assert.NotEqual(t, firstCode, secondPending.Code, "second registration should have a new code")
}

func TestPollForConfirmation_SavesMapping(t *testing.T) {
	confirmed := false
	hubHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/teams/link/status":
			if !confirmed {
				confirmed = true
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status": "confirmed",
					"user": map[string]string{
						"id":    "scion-user-42",
						"email": "confirmed@example.com",
					},
				})
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	broker, _ := testBrokerWithStore(t, hubHandler)
	handler := broker.commandHandler

	activity := testActivity("register")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Directly call pollForConfirmation to test it in isolation
	// (bypasses the 10s ticker by calling it directly).
	go handler.pollForConfirmation(ctx, activity, "aad-user-1", "TESTCODE", cancel)

	// Wait for the goroutine to process.
	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("Timed out waiting for user mapping to be created")
		default:
			mapping, err := broker.store.GetUserMapping(context.Background(), "aad-user-1")
			require.NoError(t, err)
			if mapping != nil {
				assert.Equal(t, "scion-user-42", mapping.ScionUserID)
				assert.Equal(t, "confirmed@example.com", mapping.ScionEmail)
				assert.Equal(t, "Test User", mapping.TeamsDisplayName)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func TestCheckTeamsLinkStatus_ReturnsEmail(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/teams/link/status", r.URL.Path)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "confirmed",
			"user": map[string]string{
				"id":    "user-123",
				"email": "hello@example.com",
			},
		})
	}))
	defer ts.Close()

	client := NewHubClient(ts.URL, "", "", slog.New(slog.NewTextHandler(os.Stderr, nil)))
	client.httpClient = ts.Client()

	status, userID, email, err := client.CheckTeamsLinkStatus(context.Background(), "teams-user-1")
	require.NoError(t, err)
	assert.Equal(t, "confirmed", status)
	assert.Equal(t, "user-123", userID)
	assert.Equal(t, "hello@example.com", email)
}

func TestCheckTeamsLinkStatus_Pending(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "pending",
		})
	}))
	defer ts.Close()

	client := NewHubClient(ts.URL, "", "", slog.New(slog.NewTextHandler(os.Stderr, nil)))
	client.httpClient = ts.Client()

	status, userID, email, err := client.CheckTeamsLinkStatus(context.Background(), "teams-user-1")
	require.NoError(t, err)
	assert.Equal(t, "pending", status)
	assert.Empty(t, userID)
	assert.Empty(t, email)
}

func TestStripThreadSuffix(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "channel without thread",
			input:    "19:e4e1805b28e142ce9ee9be354816a319@thread.tacv2",
			expected: "19:e4e1805b28e142ce9ee9be354816a319@thread.tacv2",
		},
		{
			name:     "channel with messageid thread suffix",
			input:    "19:e4e1805b28e142ce9ee9be354816a319@thread.tacv2;messageid=1786491412694",
			expected: "19:e4e1805b28e142ce9ee9be354816a319@thread.tacv2",
		},
		{
			name:     "personal chat",
			input:    "a]concat[b",
			expected: "a]concat[b",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, stripThreadSuffix(tc.input))
		})
	}
}

func TestAgentPhaseEmoji(t *testing.T) {
	tests := []struct {
		phase    string
		expected string
	}{
		{"running", "🟢"},
		{"starting", "🟡"},
		{"stopped", "🔴"},
		{"stopping", "🔴"},
		{"waiting", "🟠"},
		{"error", "❌"},
		{"completed", "✅"},
		{"unknown", "⚪"},
		{"", "⚪"},
	}

	for _, tc := range tests {
		assert.Equal(t, tc.expected, agentPhaseEmoji(tc.phase), "phase=%q", tc.phase)
	}
}
