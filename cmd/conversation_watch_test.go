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
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeWatchConvService struct {
	hubclient.ConversationService
	fn func(ctx context.Context, conversationID string, opts hubclient.WatchOptions) (*hubclient.WatchResult, error)
}

func (f *fakeWatchConvService) Watch(ctx context.Context, conversationID string, opts hubclient.WatchOptions) (*hubclient.WatchResult, error) {
	return f.fn(ctx, conversationID, opts)
}

// Criterion 16: --once exits 0 and prints the message; timeout exits 124 and
// prints {"events":[],"timedOut":true,"nextCursor":...} under --json; 410 exits 3.
func TestConversationWatchOnce_ExitCodesAndJSON(t *testing.T) {
	t.Run("message delivered exits 0 and writes compact JSON", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		svc := &fakeWatchConvService{
			fn: func(_ context.Context, convID string, opts hubclient.WatchOptions) (*hubclient.WatchResult, error) {
				assert.Equal(t, "conv-1", convID)
				assert.Equal(t, "others", opts.Filter)
				return &hubclient.WatchResult{
					Events: []hubclient.WatchEvent{
						{
							EventID:     "msg:m1",
							Type:        "conversation.message.created",
							OccurredAt:  time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC),
							Cursor:      "cur-1",
							ResourceURI: "scion://conversations/conv-1/messages/m1",
							Data: &store.Message{
								ID:     "m1",
								Sender: "agent:a",
								Msg:    "done",
							},
						},
					},
					NextCursor: "cur-1",
				}, nil
			},
		}

		err := executeConversationWatchOnce(context.Background(), svc, watchOnceParams{
			ConversationID: "conv-1",
			Filter:         "others",
			Limit:          50,
			Timeout:        5 * time.Second,
			JSON:           true,
			Stdout:         &stdout,
			Stderr:         &stderr,
		})
		require.NoError(t, err)

		var got hubclient.WatchResult
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
		require.Len(t, got.Events, 1)
		assert.Equal(t, "msg:m1", got.Events[0].EventID)
		assert.Equal(t, "cur-1", got.NextCursor)
		assert.False(t, got.TimedOut)
	})

	t.Run("timeout exits 124 and writes timeout JSON", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		svc := &fakeWatchConvService{
			fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
				time.Sleep(25 * time.Millisecond)
				return &hubclient.WatchResult{
					Events:     []hubclient.WatchEvent{},
					NextCursor: "cur-head",
					TimedOut:   true,
				}, nil
			},
		}

		err := executeConversationWatchOnce(context.Background(), svc, watchOnceParams{
			ConversationID: "conv-1",
			Filter:         "others",
			Limit:          50,
			Timeout:        20 * time.Millisecond,
			JSON:           true,
			Stdout:         &stdout,
			Stderr:         &stderr,
		})
		require.Error(t, err)
		assert.Equal(t, 124, exitCodeFor(err))
		assert.True(t, isReportedInJSON(err))

		var got map[string]any
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
		assert.Equal(t, true, got["timedOut"])
		assert.Equal(t, "cur-head", got["nextCursor"])
		events, ok := got["events"].([]any)
		require.True(t, ok)
		assert.Empty(t, events)
	})

	t.Run("410 cursor expired exits 3", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		svc := &fakeWatchConvService{
			fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
				return nil, hubclient.ErrCursorExpired
			},
		}

		err := executeConversationWatchOnce(context.Background(), svc, watchOnceParams{
			ConversationID: "conv-1",
			Cursor:         "stale",
			Filter:         "others",
			Limit:          50,
			JSON:           true,
			Stdout:         &stdout,
			Stderr:         &stderr,
		})
		require.Error(t, err)
		assert.Equal(t, 3, exitCodeFor(err))
	})

	t.Run("context cancel exits 130", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		svc := &fakeWatchConvService{
			fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
				return nil, context.Canceled
			},
		}

		err := executeConversationWatchOnce(ctx, svc, watchOnceParams{
			ConversationID: "conv-1",
			Filter:         "others",
			Limit:          50,
			Stdout:         &stdout,
			Stderr:         &stderr,
		})
		require.Error(t, err)
		assert.Equal(t, 130, exitCodeFor(err))
	})
}

// Criterion 17: --after-message passes after_message so a reply committed
// before watch started is returned immediately.
func TestConversationWatchOnce_AfterMessagePassed(t *testing.T) {
	var stdout, stderr bytes.Buffer
	var gotAfter string
	svc := &fakeWatchConvService{
		fn: func(_ context.Context, _ string, opts hubclient.WatchOptions) (*hubclient.WatchResult, error) {
			gotAfter = opts.AfterMessage
			return &hubclient.WatchResult{
				Events: []hubclient.WatchEvent{
					{
						EventID: "msg:reply-1",
						Type:    "conversation.message.created",
						Cursor:  "cur-2",
						Data:    &store.Message{ID: "reply-1", Sender: "agent:a", Msg: "already committed"},
					},
				},
				NextCursor: "cur-2",
			}, nil
		},
	}

	err := executeConversationWatchOnce(context.Background(), svc, watchOnceParams{
		ConversationID: "conv-1",
		AfterMessage:   "sent-msg-1",
		Filter:         "others",
		Limit:          50,
		JSON:           true,
		Stdout:         &stdout,
		Stderr:         &stderr,
	})
	require.NoError(t, err)
	assert.Equal(t, "sent-msg-1", gotAfter)
}

// Criterion 19: conversation.watch is in windowsAllowed and absent from agentAllowed.
func TestConversationWatch_Gating(t *testing.T) {
	assert.True(t, windowsAllowed["conversation.watch"], "conversation.watch must be in windowsAllowed")
	assert.False(t, agentAllowed["conversation.watch"], "conversation.watch must be absent from agentAllowed (D2 host-only)")
}
