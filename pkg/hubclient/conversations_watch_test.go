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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConversationService_Watch_SuccessAndQueryParams(t *testing.T) {
	var gotPath string
	var gotQuery map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = map[string]string{
			"cursor":        r.URL.Query().Get("cursor"),
			"after_message": r.URL.Query().Get("after_message"),
			"start":         r.URL.Query().Get("start"),
			"filter":        r.URL.Query().Get("filter"),
			"limit":         r.URL.Query().Get("limit"),
			"wait":          r.URL.Query().Get("wait"),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"events": []map[string]any{
				{
					"eventId":     "msg:m1",
					"type":        "conversation.message.created",
					"occurredAt":  "2026-11-21T10:00:00Z",
					"cursor":      "c1",
					"resourceUri": "scion://conversations/conv-1/messages/m1",
					"data": map[string]any{
						"id":     "m1",
						"sender": "agent:a",
						"msg":    "hello",
					},
				},
			},
			"nextCursor":   "c1",
			"hasMore":      false,
			"truncated":    false,
			"timedOut":     false,
			"retryAfterMs": 250,
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	require.NoError(t, err)

	res, err := c.Conversations().Watch(context.Background(), "conv-1", WatchOptions{
		Cursor:       "c0",
		AfterMessage: "m0",
		Start:        "head",
		Filter:       "addressed",
		Limit:        10,
		Wait:         5 * time.Second,
	})
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/conversations/conv-1/watch", gotPath)
	assert.Equal(t, "c0", gotQuery["cursor"])
	assert.Equal(t, "m0", gotQuery["after_message"])
	assert.Equal(t, "head", gotQuery["start"])
	assert.Equal(t, "addressed", gotQuery["filter"])
	assert.Equal(t, "10", gotQuery["limit"])
	assert.Equal(t, "5s", gotQuery["wait"])

	require.Len(t, res.Events, 1)
	assert.Equal(t, "msg:m1", res.Events[0].EventID)
	assert.Equal(t, "hello", res.Events[0].Data.Msg)
	assert.Equal(t, "c1", res.NextCursor)
	assert.Equal(t, 250*time.Millisecond, res.RetryAfter)
}

func TestConversationService_Watch_ImmediateWaitZero(t *testing.T) {
	var seenWaits []string
	var hasWait []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		_, present := q["wait"]
		hasWait = append(hasWait, present)
		seenWaits = append(seenWaits, q.Get("wait"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"events":     []any{},
			"nextCursor": "cur-head",
			"timedOut":   true,
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	require.NoError(t, err)

	// Immediate: true sends explicit wait=0.
	res, err := c.Conversations().Watch(context.Background(), "conv-1", WatchOptions{
		Immediate: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "cur-head", res.NextCursor)
	require.Len(t, seenWaits, 1)
	assert.True(t, hasWait[0])
	assert.Equal(t, "0", seenWaits[0])

	// Default (Immediate: false, Wait: 0) omits wait so the server applies its default.
	_, err = c.Conversations().Watch(context.Background(), "conv-1", WatchOptions{})
	require.NoError(t, err)
	require.Len(t, seenWaits, 2)
	assert.False(t, hasWait[1])
}

func TestConversationService_Watch_ErrorMapping(t *testing.T) {
	t.Run("410 maps to ErrCursorExpired", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    "cursor_expired",
					"message": "cursor no longer valid",
				},
			})
		}))
		defer srv.Close()

		c, err := New(srv.URL)
		require.NoError(t, err)
		_, err = c.Conversations().Watch(context.Background(), "conv-1", WatchOptions{Cursor: "old"})
		assert.ErrorIs(t, err, ErrCursorExpired)
	})

	t.Run("400 invalid_cursor maps to ErrInvalidCursor", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    "invalid_cursor",
					"message": "bad cursor",
				},
			})
		}))
		defer srv.Close()

		c, err := New(srv.URL)
		require.NoError(t, err)
		_, err = c.Conversations().Watch(context.Background(), "conv-1", WatchOptions{Cursor: "bad"})
		assert.ErrorIs(t, err, ErrInvalidCursor)
	})
}
