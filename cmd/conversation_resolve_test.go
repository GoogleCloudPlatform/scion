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

package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveConversationRef_AtAgent_CanonicalResolveFound(t *testing.T) {
	const (
		convID    = "11111111-2222-3333-4444-555555555555"
		agentID   = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		projectID = "99999999-8888-7777-6666-555555555555"
	)

	var (
		gotRef       string
		gotProjectID string
		listCalled   bool
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/conversations/resolve":
			gotRef = r.URL.Query().Get("reference")
			gotProjectID = r.URL.Query().Get("project_id")
			_ = json.NewEncoder(w).Encode(hubclient.ConversationResolveResult{
				Exists: true,
				Conversation: &hubclient.ConversationDetail{
					Conversation: store.Conversation{
						ID:          convID,
						Kind:        "direct",
						DisplayName: "",
					},
				},
				PeerAgent: &hubclient.TargetAgentInfo{
					ID:        agentID,
					Slug:      "my-agent",
					ProjectID: projectID,
				},
			})
		case "/api/v1/conversations":
			listCalled = true
			http.Error(w, "list should not be called when canonical resolve succeeds", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	got, err := resolveConversationRef(context.Background(), client, projectID, "@my-agent")
	require.NoError(t, err)
	assert.Equal(t, convID, got)
	assert.Equal(t, "@my-agent", gotRef)
	assert.Equal(t, projectID, gotProjectID)
	assert.False(t, listCalled)
}

func TestResolveConversationRef_AtAgent_PeerExistsNoConversationYet(t *testing.T) {
	const (
		agentID   = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		projectID = "99999999-8888-7777-6666-555555555555"
	)

	listCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/conversations/resolve":
			_ = json.NewEncoder(w).Encode(hubclient.ConversationResolveResult{
				Exists: false,
				PeerAgent: &hubclient.TargetAgentInfo{
					ID:        agentID,
					Slug:      "quiet-agent",
					ProjectID: projectID,
				},
			})
		case "/api/v1/conversations":
			listCalled = true
			_ = json.NewEncoder(w).Encode(hubclient.ConversationListResult{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	_, err = resolveConversationRef(context.Background(), client, projectID, "@quiet-agent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no direct conversation exists with @quiet-agent yet")
	assert.Contains(t, err.Error(), "scion message @quiet-agent")
	assert.Contains(t, err.Error(), "scion conversation list")
	assert.False(t, listCalled, "should not scan list when hub already confirmed peer exists with no DM")
}

func TestResolveConversationRef_AtAgent_NotFound(t *testing.T) {
	listCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/conversations/resolve":
			_ = json.NewEncoder(w).Encode(hubclient.ConversationResolveResult{Exists: false})
		case "/api/v1/conversations":
			listCalled = true
			// Even if the caller has a DM with an agent of the same name in
			// another project, a project-scoped resolve miss must not silently
			// resolve to another project's agent.
			_ = json.NewEncoder(w).Encode(hubclient.ConversationListResult{
				Conversations: []hubclient.ConversationDetail{
					{
						Conversation: store.Conversation{ID: "99999999-aaaa-bbbb-cccc-dddddddddddd", Kind: "direct"},
						DMPeer:       &hubclient.ConversationPeer{Kind: "agent", ID: "other-proj-agent", Name: "missing-agent"},
					},
				},
				Total: 1,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	_, err = resolveConversationRef(context.Background(), client, "proj-1", "@missing-agent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no direct conversation found for @missing-agent")
	assert.Contains(t, err.Error(), "scion conversation list")
	assert.Contains(t, err.Error(), "conv:<uuid>")
	assert.False(t, listCalled, "project-scoped resolve miss must not fall back to unscoped DM listing")
}

func TestResolveConversationRef_AtAgent_FallbackToListByDMPeer(t *testing.T) {
	const (
		convID  = "22222222-3333-4444-5555-666666666666"
		agentID = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	)

	t.Run("missing_project_400_falls_back_to_list_and_resolves_unique_match_by_name_or_id", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/conversations/resolve":
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{
						"code":    "invalid_request",
						"message": "Project ID is required when resolving @agent references",
					},
				})
			case "/api/v1/conversations":
				assert.Equal(t, "direct", r.URL.Query().Get("kind"))
				_ = json.NewEncoder(w).Encode(hubclient.ConversationListResult{
					Conversations: []hubclient.ConversationDetail{
						{
							Conversation: store.Conversation{ID: "33333333-4444-5555-6666-777777777777", Kind: "direct"},
							DMPeer:       &hubclient.ConversationPeer{Kind: "agent", ID: "other-agent-id", Name: "other-agent"},
						},
						{
							Conversation: store.Conversation{ID: "44444444-5555-6666-7777-888888888888", Kind: "direct"},
							DMPeer:       &hubclient.ConversationPeer{Kind: "user", ID: "user-1", Name: "solo-agent"},
						},
						{
							Conversation: store.Conversation{ID: convID, Kind: "direct"},
							DMPeer:       &hubclient.ConversationPeer{Kind: "agent", ID: agentID, Name: "solo-agent"},
						},
					},
					Total: 3,
				})
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		client, err := hubclient.New(srv.URL)
		require.NoError(t, err)

		got, err := resolveConversationRef(context.Background(), client, "", "@solo-agent")
		require.NoError(t, err)
		assert.Equal(t, convID, got)

		// Also resolves by agent UUID via DMPeer.ID (e.g. a DM whose peer name
		// is empty because the agent was soft-deleted).
		gotByID, err := resolveConversationRef(context.Background(), client, "", "@"+agentID)
		require.NoError(t, err)
		assert.Equal(t, convID, gotByID)
	})

	t.Run("missing_project_400_ignores_different_agent_dm", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/conversations/resolve":
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{
						"code":    "invalid_request",
						"message": "Project ID is required when resolving @agent references",
					},
				})
			case "/api/v1/conversations":
				_ = json.NewEncoder(w).Encode(hubclient.ConversationListResult{
					Conversations: []hubclient.ConversationDetail{
						{
							Conversation: store.Conversation{ID: convID, Kind: "direct"},
							DMPeer:       &hubclient.ConversationPeer{Kind: "agent", ID: agentID, Name: "different-agent"},
						},
					},
					Total: 1,
				})
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		client, err := hubclient.New(srv.URL)
		require.NoError(t, err)

		_, err = resolveConversationRef(context.Background(), client, "", "@target-agent")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no direct conversation found for @target-agent")
	})

	t.Run("missing_project_400_ignores_user_peer_with_matching_name_or_id", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/conversations/resolve":
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{
						"code":    "invalid_request",
						"message": "Project ID is required when resolving @agent references",
					},
				})
			case "/api/v1/conversations":
				_ = json.NewEncoder(w).Encode(hubclient.ConversationListResult{
					Conversations: []hubclient.ConversationDetail{
						{
							Conversation: store.Conversation{ID: convID, Kind: "direct"},
							DMPeer:       &hubclient.ConversationPeer{Kind: "user", ID: agentID, Name: "target-agent"},
						},
					},
					Total: 1,
				})
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		client, err := hubclient.New(srv.URL)
		require.NoError(t, err)

		_, err = resolveConversationRef(context.Background(), client, "", "@target-agent")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no direct conversation found for @target-agent")

		_, err = resolveConversationRef(context.Background(), client, "", "@"+agentID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no direct conversation found for @"+agentID)
	})

	t.Run("missing_project_400_with_no_match_hints_project_flag", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/conversations/resolve":
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{
						"code":    "invalid_request",
						"message": "Project ID is required when resolving @agent references",
					},
				})
			case "/api/v1/conversations":
				_ = json.NewEncoder(w).Encode(hubclient.ConversationListResult{})
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		client, err := hubclient.New(srv.URL)
		require.NoError(t, err)

		_, err = resolveConversationRef(context.Background(), client, "", "@ghost")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--project")
		assert.Contains(t, err.Error(), "conv:<uuid>")
	})
}

func TestResolveConversationRef_AtAgent_Ambiguous(t *testing.T) {
	const (
		convID1 = "11111111-1111-1111-1111-111111111111"
		convID2 = "22222222-2222-2222-2222-222222222222"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/conversations/resolve":
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"code":    "invalid_request",
					"message": "Project ID is required when resolving @agent references",
				},
			})
		case "/api/v1/conversations":
			_ = json.NewEncoder(w).Encode(hubclient.ConversationListResult{
				Conversations: []hubclient.ConversationDetail{
					{
						Conversation: store.Conversation{ID: convID1, Kind: "direct"},
						DMPeer:       &hubclient.ConversationPeer{Kind: "agent", ID: "agent-1", Name: "reviewer"},
					},
					{
						Conversation: store.Conversation{ID: convID2, Kind: "direct"},
						DMPeer:       &hubclient.ConversationPeer{Kind: "agent", ID: "agent-2", Name: "reviewer"},
					},
				},
				Total: 2,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	_, err = resolveConversationRef(context.Background(), client, "", "@reviewer")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple direct conversations match @reviewer")
	assert.Contains(t, err.Error(), "conv:"+convID1)
	assert.Contains(t, err.Error(), "conv:"+convID2)
	assert.Contains(t, err.Error(), "--project")
}

func TestResolveConversationRef_AtAgent_ServerErrorPropagates(t *testing.T) {
	listCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/conversations/resolve":
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"code":    "internal_error",
					"message": "database unavailable",
				},
			})
		case "/api/v1/conversations":
			listCalled = true
			_ = json.NewEncoder(w).Encode(hubclient.ConversationListResult{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	_, err = resolveConversationRef(context.Background(), client, "proj-1", "@my-agent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to resolve conversation for @my-agent")
	assert.False(t, listCalled, "server errors should not be masked by list fallback")
}

func TestConversationSubcommands_ResolveAtAgentConsistently(t *testing.T) {
	const (
		convID    = "aaaaaaaa-1111-2222-3333-444444444444"
		msgID     = "bbbbbbbb-1111-2222-3333-444444444444"
		agentID   = "cccccccc-1111-2222-3333-444444444444"
		groupID   = "dddddddd-1111-2222-3333-444444444444"
		projectID = "eeeeeeee-1111-2222-3333-444444444444"
	)

	origProjectPath := projectPath
	origOutputFormat := outputFormat
	origConvMsgLimit := convMsgLimit
	origConvMsgAll := convMsgAll
	origConvCatchUpSince := convCatchUpSince
	origConvCatchUpLimit := convCatchUpLimit
	origConvCatchUpAll := convCatchUpAll
	origConvWatchOnce := convWatchOnce
	origConvWatchFilter := convWatchFilter
	origConvWatchLimit := convWatchLimit
	origConvWatchTimeout := convWatchTimeout
	origConvGroupProject := convGroupProject
	t.Cleanup(func() {
		projectPath = origProjectPath
		outputFormat = origOutputFormat
		convMsgLimit = origConvMsgLimit
		convMsgAll = origConvMsgAll
		convCatchUpSince = origConvCatchUpSince
		convCatchUpLimit = origConvCatchUpLimit
		convCatchUpAll = origConvCatchUpAll
		convWatchOnce = origConvWatchOnce
		convWatchFilter = origConvWatchFilter
		convWatchLimit = origConvWatchLimit
		convWatchTimeout = origConvWatchTimeout
		convGroupProject = origConvGroupProject
	})

	outputFormat = ""
	convMsgLimit = 25
	convMsgAll = false
	convCatchUpSince = "1h"
	convCatchUpLimit = 50
	convCatchUpAll = false
	convWatchOnce = true
	convWatchFilter = "others"
	convWatchLimit = 50
	convWatchTimeout = time.Nanosecond
	convGroupProject = projectID

	var (
		mu       sync.Mutex
		resolved []string
		hitPaths []string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		hitPaths = append(hitPaths, r.Method+" "+r.URL.Path)
		mu.Unlock()

		switch r.URL.Path {
		case "/healthz":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/api/v1/conversations/resolve":
			mu.Lock()
			resolved = append(resolved, r.URL.Query().Get("reference")+"|"+r.URL.Query().Get("project_id"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(hubclient.ConversationResolveResult{
				Exists: true,
				Conversation: &hubclient.ConversationDetail{
					Conversation: store.Conversation{ID: convID, Kind: "direct"},
				},
				PeerAgent: &hubclient.TargetAgentInfo{ID: agentID, Slug: "my-agent", ProjectID: projectID},
			})
		case "/api/v1/conversations/" + convID + "/messages":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []store.Message{}, "totalCount": 0})
		case "/api/v1/conversations/" + convID + "/messages/" + msgID:
			_ = json.NewEncoder(w).Encode(store.Message{ID: msgID, ConversationID: convID, Msg: "hello"})
		case "/api/v1/conversations/" + convID:
			_ = json.NewEncoder(w).Encode(hubclient.ConversationDetail{
				Conversation: store.Conversation{ID: convID, Kind: "direct"},
			})
		case "/api/v1/conversations/" + convID + "/default-agent":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case "/api/v1/conversations/" + convID + "/participants":
			_ = json.NewEncoder(w).Encode(store.ConversationParticipant{PrincipalKind: "agent", PrincipalID: agentID})
		case "/api/v1/conversations/" + convID + "/leave":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case "/api/v1/conversations/" + convID + "/watch":
			_ = json.NewEncoder(w).Encode(hubclient.WatchResult{
				Events: []hubclient.WatchEvent{
					{EventID: "ev-1", Type: "message", Cursor: "c1", Data: &store.Message{ID: msgID, Msg: "hi"}},
				},
				NextCursor: "c1",
			})
		case "/api/v1/thread-groups":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"threadGroups": []hubclient.ThreadGroup{{ID: groupID, Name: "ops", ProjectID: projectID}},
			})
		case "/api/v1/thread-groups/" + groupID + "/threads":
			_ = json.NewEncoder(w).Encode(map[string]any{"conversationId": convID})
		case "/api/v1/thread-groups/" + groupID + "/threads/" + convID:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	isolateHubEnvForTest(t, srv.URL, projectID)
	t.Setenv("SCION_AGENT_NAME", "caller-agent")
	t.Setenv("SCION_AUTH_TOKEN", "test-token")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Chdir(tmpHome)
	projectPath = setupConversationCreateProject(t, tmpHome, srv.URL, projectID)

	newCmd := func() *cobra.Command {
		c := &cobra.Command{}
		c.SetContext(context.Background())
		return c
	}

	cases := []struct {
		name string
		run  func() error
		want string
	}{
		{"messages", func() error { return runConversationMessages(newCmd(), []string{"@my-agent"}) }, "GET /api/v1/conversations/" + convID + "/messages"},
		{"get", func() error { return runConversationGet(newCmd(), []string{"@my-agent"}) }, "GET /api/v1/conversations/" + convID},
		{"get-message", func() error { return runConversationGetMessage(newCmd(), []string{"@my-agent", msgID}) }, "GET /api/v1/conversations/" + convID + "/messages/" + msgID},
		{"set-default", func() error { return runConversationSetDefault(newCmd(), []string{"@my-agent", agentID}) }, "PUT /api/v1/conversations/" + convID + "/default-agent"},
		{"participants", func() error { return runConversationParticipants(newCmd(), []string{"@my-agent"}) }, "GET /api/v1/conversations/" + convID},
		{"join", func() error { return runConversationJoin(newCmd(), []string{"@my-agent", "agent", agentID}) }, "POST /api/v1/conversations/" + convID + "/participants"},
		{"leave", func() error { return runConversationLeave(newCmd(), []string{"@my-agent"}) }, "POST /api/v1/conversations/" + convID + "/leave"},
		{"catch-up", func() error { return runConversationCatchUp(newCmd(), []string{"@my-agent"}) }, "GET /api/v1/conversations/" + convID + "/messages"},
		{"watch", func() error { return runConversationWatch(newCmd(), []string{"@my-agent"}) }, "GET /api/v1/conversations/" + convID + "/watch"},
		{"group add", func() error { return runConversationGroupAdd(newCmd(), []string{"ops", "@my-agent"}) }, "POST /api/v1/thread-groups/" + groupID + "/threads"},
		{"group remove", func() error { return runConversationGroupRemove(newCmd(), []string{"ops", "@my-agent"}) }, "DELETE /api/v1/thread-groups/" + groupID + "/threads/" + convID},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			resolved = nil
			hitPaths = nil
			mu.Unlock()

			err := tc.run()
			require.NoError(t, err)

			mu.Lock()
			gotResolved := append([]string(nil), resolved...)
			gotPaths := append([]string(nil), hitPaths...)
			mu.Unlock()

			require.Equal(t, []string{"@my-agent|" + projectID}, gotResolved)
			found := false
			for _, p := range gotPaths {
				if strings.HasPrefix(p, tc.want) {
					found = true
					break
				}
			}
			assert.True(t, found, "expected request %q in %v", tc.want, gotPaths)
		})
	}
}
