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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInboxSummaryClientTypeMatchesHub keeps hubclient's inbox wire types in
// step with pkg/hub across the summary envelope and all five preview shapes.
func TestInboxSummaryClientTypeMatchesHub(t *testing.T) {
	pairs := []struct {
		name       string
		hubType    reflect.Type
		clientType reflect.Type
	}{
		{
			name:       "InboxSummaryResponse",
			hubType:    reflect.TypeOf(hub.InboxSummaryResponse{}),
			clientType: reflect.TypeOf(hubclient.InboxSummaryResponse{}),
		},
		{
			name:       "InboxCategory",
			hubType:    reflect.TypeOf(hub.InboxCategory{}),
			clientType: reflect.TypeOf(hubclient.InboxCategory{}),
		},
		{
			name:       "InboxPendingSecretPreview",
			hubType:    reflect.TypeOf(hub.InboxPendingSecretPreview{}),
			clientType: reflect.TypeOf(hubclient.InboxPendingSecretPreview{}),
		},
		{
			name:       "InboxWaitingAgentPreview",
			hubType:    reflect.TypeOf(hub.InboxWaitingAgentPreview{}),
			clientType: reflect.TypeOf(hubclient.InboxWaitingAgentPreview{}),
		},
		{
			name:       "InboxDecisionPreview",
			hubType:    reflect.TypeOf(hub.InboxDecisionPreview{}),
			clientType: reflect.TypeOf(hubclient.InboxDecisionPreview{}),
		},
		{
			name:       "InboxDecisionInline",
			hubType:    reflect.TypeOf(hub.InboxDecisionInline{}),
			clientType: reflect.TypeOf(hubclient.InboxDecisionInline{}),
		},
		{
			name:       "InboxReviewPreview",
			hubType:    reflect.TypeOf(hub.InboxReviewPreview{}),
			clientType: reflect.TypeOf(hubclient.InboxReviewPreview{}),
		},
		{
			name:       "InboxChatPreview",
			hubType:    reflect.TypeOf(hub.InboxChatPreview{}),
			clientType: reflect.TypeOf(hubclient.InboxChatPreview{}),
		},
	}

	for _, tc := range pairs {
		t.Run(tc.name, func(t *testing.T) {
			hubPaths := map[string]bool{}
			jsonFieldPaths(tc.hubType, "", hubPaths)
			clientPaths := map[string]bool{}
			jsonFieldPaths(tc.clientType, "", clientPaths)

			var missing, extra []string
			for p := range hubPaths {
				if !clientPaths[p] {
					missing = append(missing, p)
				}
			}
			for p := range clientPaths {
				if !hubPaths[p] {
					extra = append(extra, p)
				}
			}
			sort.Strings(missing)
			sort.Strings(extra)
			assert.Empty(t, missing, "hub fields not mirrored in hubclient.%s", tc.name)
			assert.Empty(t, extra, "hubclient.%s fields the hub does not send", tc.name)
			assert.NotEmpty(t, hubPaths)
		})
	}
}

func TestInboxConstantsMatchHub(t *testing.T) {
	assert.Equal(t, hub.InboxCategoryDecisions, hubclient.InboxCategoryDecisions)
	assert.Equal(t, hub.InboxCategoryWaiting, hubclient.InboxCategoryWaiting)
	assert.Equal(t, hub.InboxCategoryReviews, hubclient.InboxCategoryReviews)
	assert.Equal(t, hub.InboxCategoryPendingSecrets, hubclient.InboxCategoryPendingSecrets)
	assert.Equal(t, hub.InboxCategoryChat, hubclient.InboxCategoryChat)

	assert.Equal(t, hub.InboxBreakdownAgents, hubclient.InboxBreakdownAgents)
	assert.Equal(t, hub.InboxBreakdownProjects, hubclient.InboxBreakdownProjects)
	assert.Equal(t, hub.InboxBreakdownToAddress, hubclient.InboxBreakdownToAddress)
	assert.Equal(t, hub.InboxBreakdownToAddressCapped, hubclient.InboxBreakdownToAddressCapped)
	assert.Equal(t, hub.InboxBreakdownRequested, hubclient.InboxBreakdownRequested)
	assert.Equal(t, hub.InboxBreakdownRequestedCapped, hubclient.InboxBreakdownRequestedCapped)
	assert.Equal(t, hub.InboxBreakdownDrafts, hubclient.InboxBreakdownDrafts)
	assert.Equal(t, hub.InboxBreakdownDraftsCapped, hubclient.InboxBreakdownDraftsCapped)
	assert.Equal(t, hub.InboxBreakdownUnreadConversations, hubclient.InboxBreakdownUnreadConversations)
	assert.Equal(t, hub.InboxBreakdownDMs, hubclient.InboxBreakdownDMs)
	assert.Equal(t, hub.InboxBreakdownMentions, hubclient.InboxBreakdownMentions)

	assert.Equal(t, hub.InboxStateOK, hubclient.InboxStateOK)
	assert.Equal(t, hub.InboxStateDisabled, hubclient.InboxStateDisabled)
	assert.Equal(t, hub.InboxStateUnavailable, hubclient.InboxStateUnavailable)
	assert.Equal(t, hub.InboxStateError, hubclient.InboxStateError)

	assert.Equal(t, hub.InboxErrorTimeout, hubclient.InboxErrorTimeout)
	assert.Equal(t, hub.InboxErrorInternal, hubclient.InboxErrorInternal)

	assert.Equal(t, hub.InboxChatFilterAll, hubclient.InboxChatFilterAll)
	assert.Equal(t, hub.InboxChatFilterDMs, hubclient.InboxChatFilterDMs)
	assert.Equal(t, hub.InboxChatFilterMentions, hubclient.InboxChatFilterMentions)

	assert.Equal(t, hub.InboxPendingSecretKind, hubclient.InboxPendingSecretKind)
}

func withInboxFakeHub(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	stubNotOnGCE(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := hubclient.New(srv.URL, hubclient.WithBearerToken("test-token"))
	require.NoError(t, err)

	origTarget := inboxClientTarget
	origFormat := outputFormat
	inboxClientTarget = func() (hubclient.Client, string, error) {
		return c, srv.URL, nil
	}
	t.Cleanup(func() {
		inboxClientTarget = origTarget
		outputFormat = origFormat
	})
}

func runInboxCmdForTest(t *testing.T, format string, args ...string) (string, error) {
	t.Helper()
	outputFormat = format
	cmd := newInboxCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func TestInboxCmd_HumanOutput_AllStatesAndCategories(t *testing.T) {
	fixedNow := time.Date(2026, 11, 24, 18, 2, 0, 0, time.UTC)
	restoreNow := clitime.SetNow(func() time.Time { return fixedNow })
	defer restoreNow()
	clitime.SetZone(time.UTC)
	defer clitime.SetZone(nil)

	t.Run("session-like view with all five categories and capped", func(t *testing.T) {
		fixture := `{
  "generatedAt": "2026-11-24T18:02:00Z",
  "total": 22,
  "totalCapped": true,
  "categories": {
    "decisions": {
      "state": "ok",
      "count": 3,
      "capped": false,
      "countsTowardTotal": true,
      "href": "/profile/decisions?tab=pending",
      "breakdown": {"agents": 2, "projects": 1},
      "previews": [
        {
          "id": "d1",
          "projectId": "p1",
          "projectName": "core-api",
          "createdAt": "2026-11-24T16:02:00Z",
          "href": "/projects/p1/chat?msg=m1",
          "promptMessageId": "m1",
          "conversationLabel": "#deploy",
          "askerAgentName": "deploy-bot",
          "question": "Pick migration strategy?",
          "decisionCount": 1,
          "answerMode": "single",
          "canAnswer": true
        }
      ]
    },
    "waiting": {
      "state": "ok",
      "count": 1,
      "capped": false,
      "countsTowardTotal": true,
      "href": "/agents?q=activity%3Awaiting_for_input",
      "agentIds": ["a1"],
      "breakdown": {"projects": 1},
      "previews": [
        {
          "id": "a1",
          "projectId": "p2",
          "projectName": "web",
          "createdAt": "2026-11-24T17:48:00Z",
          "href": "/projects/p2/agents/a1",
          "agentId": "a1",
          "agentName": "Frontend Dev",
          "agentSlug": "fe-dev",
          "question": "Need staging creds?",
          "since": "2026-11-24T17:48:00Z"
        }
      ]
    },
    "pending_secrets": {
      "state": "ok",
      "count": 2,
      "capped": true,
      "countsTowardTotal": true,
      "href": "/profile/secrets",
      "breakdown": {"agents": 1, "projects": 1},
      "nextExpiresAt": "2026-11-24T20:02:00Z",
      "previews": [
        {
          "id": "ps1",
          "kind": "pending_secret",
          "projectId": "p1",
          "projectName": "core-api",
          "createdAt": "2026-11-24T17:30:00Z",
          "href": "/profile/secrets",
          "key": "STRIPE_API_KEY",
          "agentSlug": "billing-dev",
          "injectionMode": "env",
          "replacesExisting": false,
          "allowProgeny": false,
          "expiresAt": "2026-11-24T20:02:00Z"
        }
      ]
    },
    "reviews": {
      "state": "ok",
      "count": 2,
      "capped": false,
      "countsTowardTotal": true,
      "href": "/artifacts?review_on_latest=1&owner=me",
      "breakdown": {"toAddress": 1, "requested": 1},
      "previews": [
        {
          "id": "r1",
          "projectId": "p1",
          "projectName": "core-api",
          "createdAt": "2026-11-24T17:02:00Z",
          "href": "/artifacts/art-1",
          "kind": "to_address",
          "artifactId": "art-1",
          "title": "API Migration Spec",
          "scopeRef": "core-api"
        },
        {
          "id": "r2",
          "projectId": "p2",
          "projectName": "web",
          "createdAt": "2026-11-24T17:32:00Z",
          "href": "/artifacts/art-2",
          "kind": "requested",
          "artifactId": "art-2",
          "title": "UI Design Doc",
          "scopeRef": "web",
          "requestedBy": "alice",
          "notificationId": "n-2"
        }
      ]
    },
    "chat": {
      "state": "ok",
      "count": 14,
      "capped": false,
      "countsTowardTotal": true,
      "href": "/chat?filter=unread",
      "breakdown": {"dms": 2, "mentions": 1, "unreadConversations": 14},
      "previews": [
        {
          "id": "c1",
          "projectId": "p1",
          "projectName": "core-api",
          "createdAt": "2026-11-24T17:55:00Z",
          "href": "/chat/dm/u2",
          "kind": "dm",
          "conversationKey": "dm:u2",
          "label": "bob",
          "lastMessageAt": "2026-11-24T17:55:00Z"
        }
      ]
    }
  }
}`

		withInboxFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/users/me/inbox/summary", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fixture))
		})

		out, err := runInboxCmdForTest(t, "")
		require.NoError(t, err)

		assert.Contains(t, out, "INBOX  22+ waiting on you  (generated 18:02:00 UTC)")
		assert.Contains(t, out, "Decisions           3   scion decisions list")
		assert.Contains(t, out, "  3 decisions from 2 agents in 1 project")
		assert.Contains(t, out, `• core-api · "Pick migration strategy?" · deploy-bot · 2h ago`)
		assert.Contains(t, out, "Secrets            2+")
		assert.Contains(t, out, "  2+ pending secrets to approve, across 1 project")
		assert.Contains(t, out, "/profile/secrets")
		assert.Contains(t, out, "• STRIPE_API_KEY · billing-dev · core-api · expires in 2h")
		assert.Contains(t, out, "Agents Waiting      1   scion look <agent>")
		assert.Contains(t, out, "  1 agent waiting in 1 project")
		assert.Contains(t, out, `• web / fe-dev · "Need staging creds?" · 14m ago`)
		assert.Contains(t, out, "Artifact Reviews    2   scion notifications")
		assert.Contains(t, out, "  1 review to address, 1 review requested")
		assert.Contains(t, out, "/artifacts?review_on_latest=1&owner=me")
		assert.Contains(t, out, "• [to address] API Migration Spec · core-api · 1h ago")
		assert.Contains(t, out, "• [requested] UI Design Doc · web · from alice · 30m ago")
		assert.Contains(t, out, "Chat               14")
		assert.Contains(t, out, "  14 unread conversations, 1 with mentions")
		assert.Contains(t, out, "• [DM] bob · core-api · 7m ago")

		// Verify canonical order: Decisions -> Secrets -> Agents Waiting -> Artifact Reviews -> Chat.
		idxDec := strings.Index(out, "Decisions")
		idxSec := strings.Index(out, "Secrets")
		idxWait := strings.Index(out, "Agents Waiting")
		idxRev := strings.Index(out, "Artifact Reviews")
		idxChat := strings.Index(out, "Chat")
		assert.True(t, idxDec < idxSec && idxSec < idxWait && idxWait < idxRev && idxRev < idxChat, "unexpected category order:\n%s", out)
	})

	t.Run("merged Secrets line combines agent_secrets and pending_secrets with union projects and previews", func(t *testing.T) {
		fixture := `{
  "generatedAt": "2026-11-24T18:02:00Z",
  "total": 7,
  "totalCapped": false,
  "categories": {
    "agent_secrets": {
      "state": "ok",
      "count": 5,
      "capped": false,
      "countsTowardTotal": true,
      "href": "/profile/agent-secrets",
      "breakdown": {"agents": 2, "projects": 1, "secretsProjects": 2},
      "previews": [
        {
          "id": "sr1",
          "projectName": "core-api",
          "key": "GITHUB_TOKEN",
          "createdAt": "2026-11-24T17:42:00Z"
        }
      ]
    },
    "pending_secrets": {
      "state": "ok",
      "count": 2,
      "capped": false,
      "countsTowardTotal": true,
      "href": "/profile/secrets",
      "breakdown": {"agents": 1, "projects": 1},
      "previews": [
        {
          "id": "ps1",
          "kind": "pending_secret",
          "projectId": "p2",
          "projectName": "web",
          "createdAt": "2026-11-24T17:30:00Z",
          "href": "/profile/secrets",
          "key": "STRIPE_API_KEY",
          "agentSlug": "billing-dev",
          "expiresAt": "2026-11-24T20:02:00Z"
        }
      ]
    }
  }
}`
		withInboxFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fixture))
		})

		out, err := runInboxCmdForTest(t, "")
		require.NoError(t, err)
		assert.Contains(t, out, "Secrets             7")
		assert.Contains(t, out, "  5 secret requests from 2 agents, 2 pending secrets to approve, across 2 projects")
		assert.Contains(t, out, "/profile/agent-secrets")
		assert.Contains(t, out, "/profile/secrets")
		assert.Contains(t, out, "• core-api · GITHUB_TOKEN · 20m ago")
		assert.Contains(t, out, "• STRIPE_API_KEY · billing-dev · web · expires in 2h")
		assert.NotContains(t, out, "Agent Secrets")
		assert.NotContains(t, out, "Pending Secrets")
	})

	t.Run("token-like view with unavailable pending_secrets, disabled omitted, and error category exits 0", func(t *testing.T) {
		fixture := `{
  "generatedAt": "2026-11-24T18:02:00Z",
  "total": 4,
  "totalCapped": false,
  "categories": {
    "decisions": {
      "state": "ok",
      "count": 3,
      "capped": false,
      "countsTowardTotal": true,
      "href": "/profile/decisions?tab=pending",
      "breakdown": {"agents": 2, "projects": 1},
      "previews": []
    },
    "waiting": {
      "state": "ok",
      "count": 1,
      "capped": false,
      "countsTowardTotal": true,
      "href": "/agents?q=activity%3Awaiting_for_input",
      "previews": []
    },
    "pending_secrets": {
      "state": "unavailable",
      "count": 0,
      "capped": false,
      "countsTowardTotal": false,
      "href": "/profile/secrets",
      "previews": []
    },
    "reviews": {
      "state": "disabled",
      "count": 0,
      "capped": false,
      "countsTowardTotal": true,
      "href": "/artifacts?review_on_latest=1&owner=me",
      "previews": []
    },
    "chat": {
      "state": "error",
      "count": 0,
      "capped": false,
      "countsTowardTotal": true,
      "href": "/chat",
      "errorCode": "timeout",
      "previews": []
    }
  }
}`

		withInboxFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fixture))
		})

		out, err := runInboxCmdForTest(t, "")
		require.NoError(t, err, "category-level errors must still exit 0")
		assert.Contains(t, out, "INBOX  4 waiting on you")
		assert.Contains(t, out, "Secrets             –   unavailable with token auth (open ")
		assert.Contains(t, out, "/profile/secrets in the browser)")
		assert.NotContains(t, out, "Artifact Reviews", "disabled category must be omitted")
		assert.Contains(t, out, "Chat                !   error (timeout)")
	})
}

func TestInboxCmd_JSONOutputVerbatim(t *testing.T) {
	const wire = `{
  "generatedAt": "2026-11-24T18:02:00Z",
  "total": 1,
  "totalCapped": false,
  "extraHubField": {"v": 2},
  "categories": {
    "pending_secrets": {
      "state": "unavailable",
      "count": 0,
      "capped": false,
      "countsTowardTotal": false,
      "href": "/profile/secrets",
      "previews": []
    },
    "decisions": {
      "state": "ok",
      "count": 1,
      "capped": false,
      "countsTowardTotal": true,
      "href": "/profile/decisions?tab=pending",
      "previews": [{"id": "d1", "futurePreviewField": true}]
    }
  }
}`

	withInboxFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(wire))
	})

	out, err := runInboxCmdForTest(t, "json")
	require.NoError(t, err)
	assert.JSONEq(t, wire, out)
}

func TestInboxCmd_ChatMentionsAndPreviewsFlags(t *testing.T) {
	fixedNow := time.Date(2026, 11, 24, 18, 2, 0, 0, time.UTC)
	restoreNow := clitime.SetNow(func() time.Time { return fixedNow })
	defer restoreNow()

	var gotQuery string
	withInboxFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"generatedAt": "2026-11-24T18:02:00Z",
			"total":       5,
			"totalCapped": false,
			"categories": map[string]any{
				"decisions":       map[string]any{"state": "disabled", "count": 0, "capped": false, "countsTowardTotal": true, "href": "/profile/decisions?tab=pending", "previews": []any{}},
				"waiting":         map[string]any{"state": "disabled", "count": 0, "capped": false, "countsTowardTotal": true, "href": "/agents", "previews": []any{}},
				"pending_secrets": map[string]any{"state": "disabled", "count": 0, "capped": false, "countsTowardTotal": true, "href": "/profile/secrets", "previews": []any{}},
				"reviews":         map[string]any{"state": "disabled", "count": 0, "capped": false, "countsTowardTotal": true, "href": "/artifacts?review_on_latest=1&owner=me", "previews": []any{}},
				"chat": map[string]any{
					"state":             "ok",
					"count":             5,
					"capped":            false,
					"countsTowardTotal": true,
					"href":              "/chat?filter=unread",
					"breakdown":         map[string]int{"dms": 2, "mentions": 1, "unreadConversations": 5},
					"previews": []map[string]any{
						{
							"id":              "m1",
							"projectId":       "p1",
							"projectName":     "core-api",
							"createdAt":       "2026-11-24T17:50:00Z",
							"href":            "/projects/p1/chat/t1",
							"kind":            "mention",
							"conversationKey": "t1",
							"label":           "#release-plan",
							"lastMessageAt":   "2026-11-24T17:50:00Z",
						},
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	out, err := runInboxCmdForTest(t, "", "--chat", "mentions", "--previews", "5")
	require.NoError(t, err)
	assert.Equal(t, "chat=mentions&previews=5", gotQuery)
	assert.Contains(t, out, "• [mention] #release-plan · core-api · 12m ago")
	assert.NotContains(t, out, "[DM]")
	assert.NotContains(t, out, "[thread]")

	// --previews 0 explicitly sends previews=0 for counts-only mode.
	_, err = runInboxCmdForTest(t, "", "--previews", "0")
	require.NoError(t, err)
	assert.Equal(t, "previews=0", gotQuery)
}

func TestInboxCmd_ExperimentDisabled404(t *testing.T) {
	withInboxFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"experiment web.inbox_dashboard is not enabled"}}`))
	})

	_, err := runInboxCmdForTest(t, "")
	require.Error(t, err)
	assert.Equal(t, "Inbox is not enabled on this hub (experiment web.inbox_dashboard)", err.Error())
}

func TestInboxCmd_Forbidden403HintAndUnauthorized401NoHint(t *testing.T) {
	t.Run("403 appends neutral inbox:read / caller-kind hint", func(t *testing.T) {
		withInboxFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"missing required scope"}}`))
		})

		_, err := runInboxCmdForTest(t, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing required scope")
		assert.Contains(t, err.Error(), errHintInboxForbiddenScope)
		assert.Contains(t, err.Error(), "a user access token needs the inbox:read scope")
		assert.Contains(t, err.Error(), "agents, visitors and federated users cannot read the inbox")
	})

	t.Run("401 does not append 403 scope hint", func(t *testing.T) {
		withInboxFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"authentication required"}}`))
		})

		_, err := runInboxCmdForTest(t, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "authentication required")
		assert.NotContains(t, err.Error(), errHintInboxForbiddenScope)
	})
}

func TestInboxCmd_FlagValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "negative previews",
			args: []string{"--previews", "-1"},
			want: "--previews must be between 0 and 10",
		},
		{
			name: "previews above max",
			args: []string{"--previews", "11"},
			want: "--previews must be between 0 and 10",
		},
		{
			name: "invalid chat filter",
			args: []string{"--chat", "threads"},
			want: "--chat must be one of all, dms, mentions",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runInboxCmdForTest(t, "", tc.args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestInboxCmd_CLIModePlatformGateAndProjectRequirement(t *testing.T) {
	t.Run("does not require a local project", func(t *testing.T) {
		assert.False(t, commandRequiresProject(inboxCmd))
	})

	t.Run("absent in agent mode", func(t *testing.T) {
		assert.False(t, agentAllowed["inbox"], "inbox must stay out of agentAllowed")
		root, _ := gateTestTree(t)
		require.NotNil(t, resolveCommandPath(root, "inbox"), "inbox must exist on rootCmd")
		applyModeRestrictions(root, ModeAgent)
		assert.Nil(t, resolveCommandPath(root, "inbox"), "inbox must be removed in agent mode")
	})

	t.Run("allowed on Windows", func(t *testing.T) {
		assert.True(t, windowsAllowed["inbox"], "inbox must be in windowsAllowed")
		root, ran := gateTestTree(t)
		applyPlatformRestrictions(root, "windows")
		c := resolveCommandPath(root, "inbox")
		require.NotNil(t, c)
		assert.NoError(t, windowsRefusal(c))
		assert.False(t, c.Hidden)

		root.SetArgs([]string{"inbox"})
		require.NoError(t, root.Execute())
		assert.Equal(t, []string{"inbox"}, *ran)
	})
}

func TestInboxCmd_UnknownCategoryRendersAfterChat(t *testing.T) {
	fixedNow := time.Date(2026, 11, 24, 18, 2, 0, 0, time.UTC)
	restoreNow := clitime.SetNow(func() time.Time { return fixedNow })
	defer restoreNow()
	clitime.SetZone(time.UTC)
	defer clitime.SetZone(nil)

	const fixture = `{
  "generatedAt": "2026-11-24T18:02:00Z",
  "total": 2,
  "totalCapped": false,
  "categories": {
    "chat": {
      "state": "ok",
      "count": 1,
      "capped": false,
      "countsTowardTotal": true,
      "href": "/chat?filter=unread",
      "previews": []
    },
    "foo": {
      "state": "ok",
      "count": 1,
      "capped": false,
      "countsTowardTotal": true,
      "href": "/foo",
      "previews": [
        {
          "id": "f1",
          "projectName": "core-api",
          "title": "Custom future item",
          "createdAt": "2026-11-24T17:52:00Z"
        }
      ]
    }
  }
}`

	withInboxFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixture))
	})

	out, err := runInboxCmdForTest(t, "")
	require.NoError(t, err)
	assert.Contains(t, out, "foo                 1   http://")
	assert.Contains(t, out, "/foo")
	assert.Contains(t, out, "• core-api · Custom future item · 10m ago")
	assert.Less(t, strings.Index(out, "Chat"), strings.Index(out, "foo"),
		"unknown categories must render after Chat:\n%s", out)
}

func TestInboxCmd_SanitisesUntrustedPreviewText(t *testing.T) {
	fixedNow := time.Date(2026, 11, 24, 18, 2, 0, 0, time.UTC)
	restoreNow := clitime.SetNow(func() time.Time { return fixedNow })
	defer restoreNow()
	clitime.SetZone(time.UTC)
	defer clitime.SetZone(nil)

	fixture := map[string]any{
		"generatedAt": "2026-11-24T18:02:00Z",
		"total":       1,
		"totalCapped": false,
		"categories": map[string]any{
			"waiting": map[string]any{
				"state":             "ok",
				"count":             1,
				"capped":            false,
				"countsTowardTotal": true,
				"href":              "/agents",
				"previews": []map[string]any{
					{
						"id":          "a1",
						"projectId":   "p1",
						"projectName": "web",
						"createdAt":   "2026-11-24T17:48:00Z",
						"href":        "/agents/a1",
						"agentId":     "a1",
						"agentName":   "Frontend Dev",
						"agentSlug":   "fe-dev",
						"question":    "\x1b[2JNeed creds\nsecond line",
						"since":       "2026-11-24T17:48:00Z",
					},
				},
			},
		},
	}

	withInboxFakeHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fixture)
	})

	out, err := runInboxCmdForTest(t, "")
	require.NoError(t, err)
	assert.NotContains(t, out, "\x1b", "raw ANSI escape must be sanitised")

	var previewLines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  • ") {
			previewLines = append(previewLines, line)
		}
	}
	require.Len(t, previewLines, 1, "expected exactly one preview line in output:\n%s", out)
	assert.Contains(t, previewLines[0], "\uFFFD[2JNeed creds\uFFFDsecond line", "control chars and newlines must be replaced with U+FFFD on one line")
	assert.NotContains(t, out, "\nsecond line", "embedded newline must not split output onto a second line")
}
