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

package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserService_InboxSummary_QueryEncoding(t *testing.T) {
	tests := []struct {
		name      string
		opts      *InboxSummaryOptions
		wantQuery string
	}{
		{
			name:      "nil options omits query params",
			opts:      nil,
			wantQuery: "",
		},
		{
			name:      "empty options omits query params",
			opts:      &InboxSummaryOptions{},
			wantQuery: "",
		},
		{
			name: "zero previews is encoded explicitly",
			opts: func() *InboxSummaryOptions {
				n := 0
				return &InboxSummaryOptions{Previews: &n}
			}(),
			wantQuery: "previews=0",
		},
		{
			name: "previews and chat filter",
			opts: func() *InboxSummaryOptions {
				n := 5
				return &InboxSummaryOptions{Previews: &n, Chat: InboxChatFilterMentions}
			}(),
			wantQuery: "chat=mentions&previews=5",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				gotPath = r.URL.Path
				gotQuery = r.URL.RawQuery
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"generatedAt":"2026-11-24T18:02:00Z","total":0,"totalCapped":false,"categories":{}}`))
			}))
			defer srv.Close()

			c, err := New(srv.URL)
			require.NoError(t, err)

			resp, err := c.Users().InboxSummary(context.Background(), tc.opts)
			require.NoError(t, err)
			assert.Equal(t, inboxSummaryPath, gotPath)
			assert.Equal(t, tc.wantQuery, gotQuery)
			assert.Equal(t, "2026-11-24T18:02:00Z", resp.GeneratedAt)
		})
	}
}

func TestUserService_InboxSummary_DecodeAndRawPreserved(t *testing.T) {
	const wire = `{
  "generatedAt": "2026-11-24T18:02:00Z",
  "total": 20,
  "totalCapped": true,
  "futureField": "keep-me",
  "categories": {
    "decisions": {
      "state": "ok",
      "count": 3,
      "capped": false,
      "countsTowardTotal": true,
      "href": "/profile/decisions?tab=pending",
      "previews": [
        {
          "id": "d1",
          "projectId": "p1",
          "projectName": "core-api",
          "createdAt": "2026-11-24T16:00:00Z",
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
      "href": "/agents",
      "agentIds": ["a1"],
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
      "state": "unavailable",
      "count": 0,
      "capped": false,
      "countsTowardTotal": false,
      "href": "/profile/secrets",
      "previews": []
    },
    "reviews": {
      "state": "error",
      "count": 0,
      "capped": false,
      "countsTowardTotal": true,
      "href": "/artifacts?review_on_latest=1&owner=me",
      "errorCode": "timeout",
      "previews": []
    },
    "chat": {
      "state": "ok",
      "count": 16,
      "capped": true,
      "countsTowardTotal": true,
      "href": "/chat?filter=unread",
      "breakdown": {"dms": 2, "mentions": 1, "unreadConversations": 16},
      "previews": []
    }
  }
}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(wire))
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	require.NoError(t, err)

	got, err := c.Users().InboxSummary(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 20, got.Total)
	assert.True(t, got.TotalCapped)
	assert.JSONEq(t, wire, string(got.Raw))

	dec := got.Categories[InboxCategoryDecisions]
	assert.Equal(t, InboxStateOK, dec.State)
	assert.Equal(t, 3, dec.Count)
	require.Len(t, dec.Previews, 1)
	var dp InboxDecisionPreview
	require.NoError(t, json.Unmarshal(dec.Previews[0], &dp))
	assert.Equal(t, "core-api", dp.ProjectName)
	assert.Equal(t, "Pick migration strategy?", dp.Question)
	assert.Equal(t, "deploy-bot", dp.AskerAgentName)

	wait := got.Categories[InboxCategoryWaiting]
	assert.Equal(t, []string{"a1"}, wait.AgentIDs)

	pending := got.Categories[InboxCategoryPendingSecrets]
	assert.Equal(t, InboxStateUnavailable, pending.State)

	rev := got.Categories[InboxCategoryReviews]
	assert.Equal(t, InboxStateError, rev.State)
	assert.Equal(t, InboxErrorTimeout, rev.ErrorCode)

	chat := got.Categories[InboxCategoryChat]
	assert.True(t, chat.Capped)
	assert.Equal(t, map[string]int{"dms": 2, "mentions": 1, "unreadConversations": 16}, chat.Breakdown)
}

func TestUserService_InboxSummary_Errors(t *testing.T) {
	t.Run("404 experiment disabled", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"experiment web.inbox_dashboard is not enabled"}}`))
		}))
		defer srv.Close()

		c, err := New(srv.URL)
		require.NoError(t, err)

		_, err = c.Users().InboxSummary(context.Background(), nil)
		require.Error(t, err)
		var apiErr *apiclient.APIError
		require.True(t, errors.As(err, &apiErr))
		assert.Equal(t, http.StatusNotFound, apiErr.StatusCode)
	})

	t.Run("null body rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`null`))
		}))
		defer srv.Close()

		c, err := New(srv.URL)
		require.NoError(t, err)

		_, err = c.Users().InboxSummary(context.Background(), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty or null")
	})
}
