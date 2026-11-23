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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
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

// flushingChunkBuffer records each flushed snapshot to assert per-event flushing.
type flushingChunkBuffer struct {
	bytes.Buffer
	flushes int
	chunks  []string
}

func (b *flushingChunkBuffer) Flush() error {
	b.flushes++
	b.chunks = append(b.chunks, b.String())
	return nil
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

		err := executeConversationWatch(context.Background(), svc, watchParams{
			ConversationID: "conv-1",
			Once:           true,
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

		err := executeConversationWatch(context.Background(), svc, watchParams{
			ConversationID: "conv-1",
			Once:           true,
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

		err := executeConversationWatch(context.Background(), svc, watchParams{
			ConversationID: "conv-1",
			Once:           true,
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

	t.Run("context cancel exits 130 in --once mode", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		svc := &fakeWatchConvService{
			fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
				return nil, context.Canceled
			},
		}

		err := executeConversationWatch(ctx, svc, watchParams{
			ConversationID: "conv-1",
			Once:           true,
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

	err := executeConversationWatch(context.Background(), svc, watchParams{
		ConversationID: "conv-1",
		Once:           true,
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
	_, isInteractive := interactiveOnlyCommands["scion conversation watch"]
	assert.False(t, isInteractive, "conversation watch must not be in interactiveOnlyCommands")
}

// Regression test (O3): when the --timeout budget has already elapsed after
// the first poll, executeConversationWatch exits 124 without issuing another
// Watch call with Wait == 0 (which would omit wait and block for 30s).
func TestConversationWatchOnce_ExpiredBudgetDoesNotPollWithZeroWait(t *testing.T) {
	var stdout, stderr bytes.Buffer
	calls := 0
	svc := &fakeWatchConvService{
		fn: func(_ context.Context, _ string, opts hubclient.WatchOptions) (*hubclient.WatchResult, error) {
			calls++
			assert.Greater(t, opts.Wait, time.Duration(0), "Watch must never be called with Wait==0 when budget remains")
			time.Sleep(15 * time.Millisecond)
			return &hubclient.WatchResult{
				Events:     []hubclient.WatchEvent{},
				NextCursor: "cur-1",
				TimedOut:   true,
			}, nil
		},
	}

	err := executeConversationWatch(context.Background(), svc, watchParams{
		ConversationID: "conv-1",
		Once:           true,
		Cursor:         "cur-0",
		Filter:         "others",
		Limit:          50,
		Timeout:        10 * time.Millisecond,
		JSON:           true,
		Stdout:         &stdout,
		Stderr:         &stderr,
	})
	require.Error(t, err)
	assert.Equal(t, 124, exitCodeFor(err))
	assert.Equal(t, 1, calls)
}

// Regression test (Optional 2): truncated=true from an earlier empty poll is
// preserved across subsequent polls in --once mode (both on event delivery and
// on timeout).
func TestConversationWatchOnce_AccumulatesTruncatedAcrossPolls(t *testing.T) {
	t.Run("preserved on subsequent event delivery", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		call := 0
		svc := &fakeWatchConvService{
			fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
				call++
				if call == 1 {
					return &hubclient.WatchResult{
						Events:     []hubclient.WatchEvent{},
						NextCursor: "cur-1",
						Truncated:  true,
						TimedOut:   true,
					}, nil
				}
				return &hubclient.WatchResult{
					Events: []hubclient.WatchEvent{
						{
							EventID: "msg:m2",
							Type:    "conversation.message.created",
							Cursor:  "cur-2",
							Data:    &store.Message{ID: "m2", Sender: "agent:a", Msg: "hi"},
						},
					},
					NextCursor: "cur-2",
					Truncated:  false,
				}, nil
			},
		}

		err := executeConversationWatch(context.Background(), svc, watchParams{
			ConversationID: "conv-1",
			Once:           true,
			Cursor:         "cur-0",
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
		assert.True(t, got.Truncated)
		require.Len(t, got.Events, 1)
	})

	t.Run("preserved on subsequent timeout", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		call := 0
		svc := &fakeWatchConvService{
			fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
				call++
				if call == 1 {
					return &hubclient.WatchResult{
						Events:     []hubclient.WatchEvent{},
						NextCursor: "cur-1",
						Truncated:  true,
						TimedOut:   true,
					}, nil
				}
				time.Sleep(25 * time.Millisecond)
				return &hubclient.WatchResult{
					Events:     []hubclient.WatchEvent{},
					NextCursor: "cur-1",
					Truncated:  false,
					TimedOut:   true,
				}, nil
			},
		}

		err := executeConversationWatch(context.Background(), svc, watchParams{
			ConversationID: "conv-1",
			Once:           true,
			Cursor:         "cur-0",
			Filter:         "others",
			Limit:          50,
			Timeout:        20 * time.Millisecond,
			JSON:           true,
			Stdout:         &stdout,
			Stderr:         &stderr,
		})
		require.Error(t, err)
		assert.Equal(t, 124, exitCodeFor(err))
		var got hubclient.WatchResult
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
		assert.True(t, got.TimedOut)
		assert.True(t, got.Truncated)
	})
}

// Criterion 18: Stream --json writes valid NDJSON, one object per line,
// flushed per event, plus cursor heartbeat lines on empty timed-out polls.
func TestConversationWatchStream_NDJSONAndHeartbeats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout flushingChunkBuffer
	var stderr bytes.Buffer
	var seenCursors []string
	call := 0

	svc := &fakeWatchConvService{
		fn: func(_ context.Context, _ string, opts hubclient.WatchOptions) (*hubclient.WatchResult, error) {
			call++
			seenCursors = append(seenCursors, opts.Cursor)
			assert.Equal(t, watchMaxServerWait, opts.Wait, "without --timeout, wait should be 55s")
			switch call {
			case 1:
				// Batch of two events: each must be written as its own NDJSON line and flushed individually.
				return &hubclient.WatchResult{
					Events: []hubclient.WatchEvent{
						{
							EventID:     "msg:m1",
							Type:        "conversation.message.created",
							OccurredAt:  time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC),
							Cursor:      "cur-1",
							ResourceURI: "scion://conversations/conv-1/messages/m1",
							Data:        &store.Message{ID: "m1", Sender: "agent:a", Recipient: "user:u", Msg: "first"},
						},
						{
							EventID:     "msg:m2",
							Type:        "conversation.message.created",
							OccurredAt:  time.Date(2026, 11, 21, 10, 0, 1, 0, time.UTC),
							Cursor:      "cur-2",
							ResourceURI: "scion://conversations/conv-1/messages/m2",
							Data:        &store.Message{ID: "m2", Sender: "agent:a", Recipient: "user:u", Msg: "second"},
						},
					},
					NextCursor: "cur-2",
				}, nil
			case 2:
				// Empty poll timeout: must emit a cursor heartbeat line.
				return &hubclient.WatchResult{
					Events:     []hubclient.WatchEvent{},
					NextCursor: "cur-2",
					TimedOut:   true,
				}, nil
			case 3:
				// One more event, then cancel to end stream cleanly.
				cancel()
				return &hubclient.WatchResult{
					Events: []hubclient.WatchEvent{
						{
							EventID:     "msg:m3",
							Type:        "conversation.message.created",
							OccurredAt:  time.Date(2026, 11, 21, 10, 0, 5, 0, time.UTC),
							Cursor:      "cur-3",
							ResourceURI: "scion://conversations/conv-1/messages/m3",
							Data:        &store.Message{ID: "m3", Sender: "agent:a", Recipient: "user:u", Msg: "third"},
						},
					},
					NextCursor: "cur-3",
				}, nil
			default:
				return nil, context.Canceled
			}
		},
	}

	err := executeConversationWatch(ctx, svc, watchParams{
		ConversationID: "conv-1",
		Once:           false,
		Filter:         "others",
		Limit:          50,
		JSON:           true,
		Stdout:         &stdout,
		Stderr:         &stderr,
	})
	require.NoError(t, err, "SIGINT/SIGTERM in stream mode must exit 0")
	assert.Empty(t, stderr.String(), "stream --json must not write next-cursor to stderr")
	assert.Equal(t, []string{"", "cur-2", "cur-2"}, seenCursors)

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 4, "expected 3 event lines + 1 heartbeat line")
	assert.Equal(t, 4, stdout.flushes, "must flush once per emitted NDJSON line")

	var e1, e2, e3 hubclient.WatchEvent
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &e1))
	assert.Equal(t, "msg:m1", e1.EventID)
	assert.Equal(t, "cur-1", e1.Cursor)

	require.NoError(t, json.Unmarshal([]byte(lines[1]), &e2))
	assert.Equal(t, "msg:m2", e2.EventID)
	assert.Equal(t, "cur-2", e2.Cursor)

	var hb map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[2]), &hb))
	assert.Equal(t, "cursor", hb["type"])
	assert.Equal(t, "cur-2", hb["nextCursor"])

	require.NoError(t, json.Unmarshal([]byte(lines[3]), &e3))
	assert.Equal(t, "msg:m3", e3.EventID)
	assert.Equal(t, "cur-3", e3.Cursor)
}

func TestConversationWatchStream_PlainRendererAndStderrCursor(t *testing.T) {
	prevZone := clitime.Zone()
	clitime.SetZone(time.UTC)
	defer clitime.SetZone(prevZone)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout flushingChunkBuffer
	var stderr bytes.Buffer
	call := 0
	ts := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

	svc := &fakeWatchConvService{
		fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
			call++
			if call == 1 {
				return &hubclient.WatchResult{
					Events: []hubclient.WatchEvent{
						{
							EventID:    "msg:m1",
							Type:       "conversation.message.created",
							OccurredAt: ts,
							Cursor:     "cur-1",
							Data: &store.Message{
								ID:        "m1",
								Sender:    "agent:reviewer",
								Recipient: "user:alice",
								Msg:       "looks good",
							},
						},
					},
					NextCursor: "cur-1",
				}, nil
			}
			cancel()
			return nil, context.Canceled
		},
	}

	err := executeConversationWatch(ctx, svc, watchParams{
		ConversationID: "conv-1",
		Once:           false,
		Filter:         "others",
		Limit:          50,
		JSON:           false,
		Stdout:         &stdout,
		Stderr:         &stderr,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, stdout.flushes)
	assert.Equal(t, "[2026-11-21 10:00:00 UTC] agent:reviewer -> user:alice\nlooks good\n", stdout.String())
	assert.Equal(t, "next-cursor: cur-1\n", stderr.String())
}

func TestConversationWatchStream_TimeoutExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	var seenWaits []time.Duration
	call := 0

	svc := &fakeWatchConvService{
		fn: func(_ context.Context, _ string, opts hubclient.WatchOptions) (*hubclient.WatchResult, error) {
			call++
			seenWaits = append(seenWaits, opts.Wait)
			time.Sleep(25 * time.Millisecond)
			return &hubclient.WatchResult{
				Events:     []hubclient.WatchEvent{},
				NextCursor: "cur-timeout",
				TimedOut:   true,
			}, nil
		},
	}

	err := executeConversationWatch(context.Background(), svc, watchParams{
		ConversationID: "conv-1",
		Once:           false,
		Filter:         "others",
		Limit:          50,
		Timeout:        20 * time.Millisecond,
		JSON:           false,
		Stdout:         &stdout,
		Stderr:         &stderr,
	})
	require.NoError(t, err, "stream mode reaching --timeout must exit 0")
	assert.Equal(t, 1, call)
	assert.LessOrEqual(t, seenWaits[0], 20*time.Millisecond)
	assert.Equal(t, "next-cursor: cur-timeout\n", stderr.String())
}

func TestConversationWatchStream_RetryBackoffWithFakeServer(t *testing.T) {
	var attempt atomic.Int32
	var receivedCursors []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempt.Add(1)
		receivedCursors = append(receivedCursors, r.URL.Query().Get("cursor"))
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			// First poll succeeds and establishes cur-1.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"events": []map[string]any{
					{
						"eventId": "msg:m1",
						"type":    "conversation.message.created",
						"cursor":  "cur-1",
						"data":    map[string]any{"id": "m1", "sender": "agent:a", "msg": "one"},
					},
				},
				"nextCursor": "cur-1",
			})
		case 2:
			// Transient 503.
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": "unavailable", "message": "hub restarting"},
			})
		case 3:
			// Transient 429 with Retry-After header.
			w.Header().Set("Retry-After", "4")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": "rate_limited", "message": "slow down"},
			})
		case 4:
			// Transient 500.
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": "internal_error", "message": "db blip"},
			})
		default:
			// Recovery: must resume from last good cursor cur-1.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"events": []map[string]any{
					{
						"eventId": "msg:m2",
						"type":    "conversation.message.created",
						"cursor":  "cur-2",
						"data":    map[string]any{"id": "m2", "sender": "agent:a", "msg": "two"},
					},
				},
				"nextCursor": "cur-2",
			})
		}
	}))
	defer srv.Close()

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr bytes.Buffer
	var sleepCalls []time.Duration
	var backoffAttempts []int
	var backoffRetryAfters []time.Duration

	// Wrap client.Conversations() so we cancel right after the 5th request delivers m2.
	wrapped := &fakeWatchConvService{
		fn: func(c context.Context, convID string, opts hubclient.WatchOptions) (*hubclient.WatchResult, error) {
			res, wErr := client.Conversations().Watch(c, convID, opts)
			if wErr == nil && res.NextCursor == "cur-2" {
				cancel()
			}
			return res, wErr
		},
	}

	err = executeConversationWatch(ctx, wrapped, watchParams{
		ConversationID: "conv-1",
		Once:           false,
		Filter:         "others",
		Limit:          50,
		JSON:           true,
		Stdout:         &stdout,
		Stderr:         &stderr,
		Backoff: func(a int, ra time.Duration) time.Duration {
			backoffAttempts = append(backoffAttempts, a)
			backoffRetryAfters = append(backoffRetryAfters, ra)
			return watchBackoffDuration(a, ra)
		},
		Sleep: func(_ context.Context, d time.Duration) error {
			sleepCalls = append(sleepCalls, d)
			return nil
		},
	})
	require.NoError(t, err)
	assert.Equal(t, int32(5), attempt.Load())
	// Every retry after m1 must pass cur-1 (last good cursor).
	assert.Equal(t, []string{"", "cur-1", "cur-1", "cur-1", "cur-1"}, receivedCursors)
	assert.Equal(t, []int{1, 2, 3}, backoffAttempts)
	assert.Equal(t, []time.Duration{0, 4 * time.Second, 0}, backoffRetryAfters)
	require.Len(t, sleepCalls, 3)
	assert.GreaterOrEqual(t, sleepCalls[1], 4*time.Second, "429 Retry-After must be honored")
	for _, d := range sleepCalls {
		assert.LessOrEqual(t, d, watchMaxBackoff)
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 2)
	// O1: retry diagnostics are written to stderr even under --json, without
	// corrupting stdout NDJSON.
	assert.Contains(t, stderr.String(), "watch error (retrying in")
}

func TestConversationWatchStream_NetworkErrorRetries(t *testing.T) {
	// Closed httptest server produces a real *url.Error wrapping *net.OpError.
	closedSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closedSrv.URL
	closedSrv.Close()

	closedClient, err := hubclient.New(closedURL)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr bytes.Buffer
	call := 0
	sleeps := 0

	svc := &fakeWatchConvService{
		fn: func(c context.Context, convID string, opts hubclient.WatchOptions) (*hubclient.WatchResult, error) {
			call++
			switch call {
			case 1:
				// Real transport dial error (*url.Error / net.Error).
				return closedClient.Conversations().Watch(c, convID, opts)
			case 2:
				// Per-request timeout while outer ctx is still live.
				return nil, context.DeadlineExceeded
			case 3:
				// Truncated response body.
				return nil, io.ErrUnexpectedEOF
			default:
				assert.Equal(t, "cur-0", opts.Cursor, "must retry from initial cursor")
				cancel()
				return &hubclient.WatchResult{
					Events: []hubclient.WatchEvent{
						{EventID: "msg:m1", Type: "conversation.message.created", Cursor: "cur-1"},
					},
					NextCursor: "cur-1",
				}, nil
			}
		},
	}

	err = executeConversationWatch(ctx, svc, watchParams{
		ConversationID: "conv-1",
		Cursor:         "cur-0",
		Filter:         "others",
		Limit:          50,
		JSON:           true,
		Stdout:         &stdout,
		Stderr:         &stderr,
		Sleep: func(_ context.Context, _ time.Duration) error {
			sleeps++
			return nil
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 4, call)
	assert.Equal(t, 3, sleeps)
}

// R1: non-transport, non-APIError failures (such as auth-apply or redirect
// errors) must be terminal with zero retries and emit next-cursor in plain
// stream mode.
func TestConversationWatchStream_NonTransportErrorsAreTerminal(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			name: "auth apply failure",
			err:  fmt.Errorf("failed to apply auth: %w", errors.New("no profile credential available")),
		},
		{
			name: "redirect loop url.Error",
			err:  &url.Error{Op: "Get", URL: "https://hub.example/api/v1/conversations/c/watch", Err: errors.New("stopped after 10 redirects")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			calls := 0
			svc := &fakeWatchConvService{
				fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
					calls++
					return nil, tc.err
				},
			}

			err := executeConversationWatch(context.Background(), svc, watchParams{
				ConversationID: "conv-1",
				Once:           false,
				Cursor:         "cur-last",
				Filter:         "others",
				Limit:          50,
				JSON:           false,
				Stdout:         &stdout,
				Stderr:         &stderr,
				Sleep: func(_ context.Context, _ time.Duration) error {
					t.Fatal("terminal client error must not trigger backoff sleep")
					return nil
				},
			})
			require.Error(t, err)
			assert.Equal(t, 1, calls)
			assert.Equal(t, 1, exitCodeFor(err))
			assert.Equal(t, "next-cursor: cur-last\n", stderr.String())
		})
	}
}

// O2 + N2: in --once mode, when --timeout expires while retrying a transient
// error after at least one successful poll, write the timeout JSON with
// lastCursor and exit 124; if no poll succeeded yet, return the error (exit 1).
func TestConversationWatchOnce_TimeoutDuringRetry(t *testing.T) {
	t.Run("after successful poll returns timeout JSON and exit 124", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		call := 0
		svc := &fakeWatchConvService{
			fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
				call++
				if call == 1 {
					return &hubclient.WatchResult{
						Events:     []hubclient.WatchEvent{},
						NextCursor: "cur-advanced",
						TimedOut:   true,
					}, nil
				}
				return nil, &apiclient.APIError{StatusCode: http.StatusServiceUnavailable, Message: "hub restarting"}
			},
		}

		err := executeConversationWatch(context.Background(), svc, watchParams{
			ConversationID: "conv-1",
			Once:           true,
			Cursor:         "cur-0",
			Filter:         "others",
			Limit:          50,
			Timeout:        25 * time.Millisecond,
			JSON:           true,
			Stdout:         &stdout,
			Stderr:         &stderr,
			Sleep: func(_ context.Context, _ time.Duration) error {
				time.Sleep(35 * time.Millisecond)
				return nil
			},
		})
		require.Error(t, err)
		assert.Equal(t, 124, exitCodeFor(err))
		assert.True(t, isReportedInJSON(err))

		var got hubclient.WatchResult
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
		assert.True(t, got.TimedOut)
		assert.Equal(t, "cur-advanced", got.NextCursor)
	})

	t.Run("before any successful poll returns transport error and exit 1", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		svc := &fakeWatchConvService{
			fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
				return nil, &apiclient.APIError{StatusCode: http.StatusServiceUnavailable, Message: "hub unreachable"}
			},
		}

		err := executeConversationWatch(context.Background(), svc, watchParams{
			ConversationID: "conv-1",
			Once:           true,
			Filter:         "others",
			Limit:          50,
			Timeout:        25 * time.Millisecond,
			JSON:           true,
			Stdout:         &stdout,
			Stderr:         &stderr,
			Sleep: func(_ context.Context, _ time.Duration) error {
				time.Sleep(35 * time.Millisecond)
				return nil
			},
		})
		require.Error(t, err)
		assert.Equal(t, 1, exitCodeFor(err))
		assert.Contains(t, err.Error(), "hub unreachable")
	})
}

func TestConversationWatchStream_TerminalStatusesDoNotRetry(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		code         string
		message      string
		wantExitCode int
		wantContains string
	}{
		{
			name:         "400 invalid_cursor",
			status:       http.StatusBadRequest,
			code:         "invalid_cursor",
			message:      "malformed cursor",
			wantExitCode: 1,
			wantContains: "invalid cursor",
		},
		{
			name:         "400 bad request",
			status:       http.StatusBadRequest,
			code:         "invalid_request",
			message:      "bad filter",
			wantExitCode: 1,
			wantContains: "bad filter",
		},
		{
			name:         "401 unauthorized",
			status:       http.StatusUnauthorized,
			code:         "unauthorized",
			message:      "token expired",
			wantExitCode: 1,
			wantContains: "token expired",
		},
		{
			name:         "403 forbidden",
			status:       http.StatusForbidden,
			code:         "forbidden",
			message:      "access denied",
			wantExitCode: 1,
			wantContains: "access denied",
		},
		{
			name:         "404 conversation not found",
			status:       http.StatusNotFound,
			code:         "not_found",
			message:      "Conversation not found",
			wantExitCode: 1,
			wantContains: "Conversation not found",
		},
		{
			name:         "410 cursor expired",
			status:       http.StatusGone,
			code:         "cursor_expired",
			message:      "cursor from future",
			wantExitCode: 3,
			wantContains: "cursor expired",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"code": tc.code, "message": tc.message},
				})
			}))
			defer srv.Close()

			client, err := hubclient.New(srv.URL)
			require.NoError(t, err)

			var stdout, stderr bytes.Buffer
			err = executeConversationWatch(context.Background(), client.Conversations(), watchParams{
				ConversationID: "conv-1",
				Once:           false,
				Cursor:         "cur-x",
				Filter:         "others",
				Limit:          50,
				JSON:           true,
				Stdout:         &stdout,
				Stderr:         &stderr,
				Sleep: func(_ context.Context, _ time.Duration) error {
					t.Fatal("terminal error must not trigger backoff sleep")
					return nil
				},
			})
			require.Error(t, err)
			assert.Equal(t, int32(1), calls.Load(), "terminal status must not retry")
			assert.Equal(t, tc.wantExitCode, exitCodeFor(err))
			assert.Contains(t, err.Error(), tc.wantContains)
		})
	}
}

// Deferred P1 item 6a: detect both "route" (experiment off) and
// "Conversation action" (older Hub) 404s as unsupported Hub, while preserving
// genuine "Conversation not found" 404s.
func TestConversationWatch_OldHubUnsupported404Detection(t *testing.T) {
	cases := []struct {
		name            string
		message         string
		wantUnsupported bool
	}{
		{
			name:            "experiment disabled route not found",
			message:         "route not found",
			wantUnsupported: true,
		},
		{
			name:            "older hub conversation action not found",
			message:         "Conversation action not found",
			wantUnsupported: true,
		},
		{
			name:            "plain 404 page not found",
			message:         "404 page not found",
			wantUnsupported: true,
		},
		{
			name:            "genuine missing conversation",
			message:         "Conversation not found",
			wantUnsupported: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			svc := &fakeWatchConvService{
				fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
					return nil, &apiclient.APIError{
						StatusCode: http.StatusNotFound,
						Code:       apiclient.ErrCodeNotFound,
						Message:    tc.message,
					}
				},
			}

			err := executeConversationWatch(context.Background(), svc, watchParams{
				ConversationID: "conv-1",
				Once:           true,
				Filter:         "others",
				Limit:          50,
				Stdout:         &stdout,
				Stderr:         &stderr,
			})
			require.Error(t, err)
			if tc.wantUnsupported {
				assert.Contains(t, err.Error(), "this Hub does not support conversation watch")
			} else {
				assert.NotContains(t, err.Error(), "this Hub does not support conversation watch")
				assert.Contains(t, err.Error(), tc.message)
			}
		})
	}
}

// Deferred P1 item 6b: a tiny --timeout (e.g. 1ns) with no --cursor still
// performs one immediate (wait=0) poll so the caller always receives a cursor.
func TestConversationWatch_TinyTimeoutStillPollsOnceForCursor(t *testing.T) {
	t.Run("--once mode with 1ns timeout gets nextCursor on timeout", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		calls := 0
		svc := &fakeWatchConvService{
			fn: func(_ context.Context, _ string, opts hubclient.WatchOptions) (*hubclient.WatchResult, error) {
				calls++
				assert.True(t, opts.Immediate, "tiny expired timeout on first poll must set Immediate=true (wait=0)")
				assert.Equal(t, time.Duration(0), opts.Wait)
				return &hubclient.WatchResult{
					Events:     []hubclient.WatchEvent{},
					NextCursor: "cur-initial-head",
					TimedOut:   true,
				}, nil
			},
		}

		err := executeConversationWatch(context.Background(), svc, watchParams{
			ConversationID: "conv-1",
			Once:           true,
			Filter:         "others",
			Limit:          50,
			Timeout:        time.Nanosecond,
			JSON:           true,
			Stdout:         &stdout,
			Stderr:         &stderr,
		})
		require.Error(t, err)
		assert.Equal(t, 124, exitCodeFor(err))
		assert.Equal(t, 1, calls)

		var got hubclient.WatchResult
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
		assert.True(t, got.TimedOut)
		assert.Equal(t, "cur-initial-head", got.NextCursor)
	})

	t.Run("stream mode with 1ns timeout gets heartbeat on real wait=0 shape and exits 0", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		calls := 0
		svc := &fakeWatchConvService{
			fn: func(_ context.Context, _ string, opts hubclient.WatchOptions) (*hubclient.WatchResult, error) {
				calls++
				assert.True(t, opts.Immediate)
				assert.Equal(t, time.Duration(0), opts.Wait)
				// Real Hub returns TimedOut: false when wait=0.
				return &hubclient.WatchResult{
					Events:     []hubclient.WatchEvent{},
					NextCursor: "cur-stream-head",
					TimedOut:   false,
				}, nil
			},
		}

		err := executeConversationWatch(context.Background(), svc, watchParams{
			ConversationID: "conv-1",
			Once:           false,
			Filter:         "others",
			Limit:          50,
			Timeout:        time.Nanosecond,
			JSON:           true,
			Stdout:         &stdout,
			Stderr:         &stderr,
		})
		require.NoError(t, err)
		assert.Equal(t, 1, calls)

		var hb map[string]any
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &hb))
		assert.Equal(t, "cursor", hb["type"])
		assert.Equal(t, "cur-stream-head", hb["nextCursor"])
	})
}

// O2: in stream mode, if no poll ever succeeded and --timeout expires while
// retrying a transient error, exit 1 with that error; if at least one poll
// succeeded, exit 0 cleanly with next-cursor on stderr.
func TestConversationWatchStream_TimeoutDuringRetry(t *testing.T) {
	t.Run("never succeeded returns error and exit 1", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		svc := &fakeWatchConvService{
			fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
				return nil, context.DeadlineExceeded
			},
		}

		err := executeConversationWatch(context.Background(), svc, watchParams{
			ConversationID: "conv-1",
			Once:           false,
			Filter:         "others",
			Limit:          50,
			Timeout:        25 * time.Millisecond,
			JSON:           true,
			Stdout:         &stdout,
			Stderr:         &stderr,
			Sleep: func(_ context.Context, _ time.Duration) error {
				time.Sleep(35 * time.Millisecond)
				return nil
			},
		})
		require.Error(t, err)
		assert.Equal(t, 1, exitCodeFor(err))
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("after successful poll exits 0 with next-cursor", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		call := 0
		svc := &fakeWatchConvService{
			fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
				call++
				if call == 1 {
					return &hubclient.WatchResult{
						Events:     []hubclient.WatchEvent{},
						NextCursor: "cur-ok",
						TimedOut:   true,
					}, nil
				}
				return nil, &apiclient.APIError{StatusCode: http.StatusServiceUnavailable, Message: "restarting"}
			},
		}

		err := executeConversationWatch(context.Background(), svc, watchParams{
			ConversationID: "conv-1",
			Once:           false,
			Filter:         "others",
			Limit:          50,
			Timeout:        25 * time.Millisecond,
			JSON:           false,
			Stdout:         &stdout,
			Stderr:         &stderr,
			Sleep: func(_ context.Context, _ time.Duration) error {
				time.Sleep(35 * time.Millisecond)
				return nil
			},
		})
		require.NoError(t, err)
		assert.Contains(t, stderr.String(), "next-cursor: cur-ok\n")
	})
}

// Cross-PR pacing: early empty 200 responses with TimedOut=false (e.g. hub
// shutdown drain or membership revocation) pace before re-polling (1s default,
// or server RetryAfter when > 0) instead of spinning.
func TestConversationWatchStream_EarlyEmptyReturnPacing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr bytes.Buffer
	var sleepCalls []time.Duration
	call := 0

	svc := &fakeWatchConvService{
		fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
			call++
			switch call {
			case 1:
				// Early empty return with RetryAfter==0 -> waits watchEmptyEarlyReturnDelay (1s).
				return &hubclient.WatchResult{
					Events:     []hubclient.WatchEvent{},
					NextCursor: "cur-1",
					TimedOut:   false,
				}, nil
			case 2:
				// Early empty return with RetryAfter>0 -> honors RetryAfter.
				return &hubclient.WatchResult{
					Events:     []hubclient.WatchEvent{},
					NextCursor: "cur-1",
					TimedOut:   false,
					RetryAfter: 3 * time.Second,
				}, nil
			default:
				cancel()
				return nil, context.Canceled
			}
		},
	}

	err := executeConversationWatch(ctx, svc, watchParams{
		ConversationID: "conv-1",
		Once:           false,
		Filter:         "others",
		Limit:          50,
		JSON:           true,
		Stdout:         &stdout,
		Stderr:         &stderr,
		Sleep: func(_ context.Context, d time.Duration) error {
			sleepCalls = append(sleepCalls, d)
			return nil
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []time.Duration{watchEmptyEarlyReturnDelay, 3 * time.Second}, sleepCalls)
	// Both early empty polls emitted cursor heartbeats under --json.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 2)
}

// N2: consecutiveErrors resets to 0 after a successful poll so subsequent
// transient errors restart backoff at attempt 1.
func TestConversationWatchStream_BackoffResetsAfterSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr bytes.Buffer
	var backoffAttempts []int
	call := 0

	svc := &fakeWatchConvService{
		fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
			call++
			switch call {
			case 1:
				return nil, &apiclient.APIError{StatusCode: http.StatusServiceUnavailable, Message: "blip 1"}
			case 2:
				return &hubclient.WatchResult{
					Events:     []hubclient.WatchEvent{},
					NextCursor: "cur-1",
					TimedOut:   true,
				}, nil
			case 3:
				return nil, &apiclient.APIError{StatusCode: http.StatusBadGateway, Message: "blip 2"}
			default:
				cancel()
				return &hubclient.WatchResult{
					Events:     []hubclient.WatchEvent{},
					NextCursor: "cur-1",
					TimedOut:   true,
				}, nil
			}
		},
	}

	err := executeConversationWatch(ctx, svc, watchParams{
		ConversationID: "conv-1",
		Once:           false,
		Filter:         "others",
		Limit:          50,
		JSON:           true,
		Stdout:         &stdout,
		Stderr:         &stderr,
		Backoff: func(attempt int, ra time.Duration) time.Duration {
			backoffAttempts = append(backoffAttempts, attempt)
			return watchBackoffDuration(attempt, ra)
		},
		Sleep: func(_ context.Context, _ time.Duration) error {
			return nil
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{1, 1}, backoffAttempts, "backoff attempt counter must reset after a successful poll")
}

func TestConversationWatch_SignalDuringBackoffSleep(t *testing.T) {
	t.Run("stream mode exits 0 and emits last good cursor", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var stdout, stderr bytes.Buffer
		call := 0
		svc := &fakeWatchConvService{
			fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
				call++
				if call == 1 {
					return &hubclient.WatchResult{
						Events:     []hubclient.WatchEvent{},
						NextCursor: "cur-good",
						TimedOut:   true,
					}, nil
				}
				return nil, &apiclient.APIError{StatusCode: http.StatusBadGateway, Message: "bad gateway"}
			},
		}

		err := executeConversationWatch(ctx, svc, watchParams{
			ConversationID: "conv-1",
			Once:           false,
			Filter:         "others",
			Limit:          50,
			JSON:           false,
			Stdout:         &stdout,
			Stderr:         &stderr,
			Sleep: func(_ context.Context, _ time.Duration) error {
				cancel()
				return context.Canceled
			},
		})
		require.NoError(t, err)
		assert.Contains(t, stderr.String(), "next-cursor: cur-good\n")
	})

	t.Run("--once mode exits 130", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var stdout, stderr bytes.Buffer
		svc := &fakeWatchConvService{
			fn: func(_ context.Context, _ string, _ hubclient.WatchOptions) (*hubclient.WatchResult, error) {
				return nil, &apiclient.APIError{StatusCode: http.StatusServiceUnavailable, Message: "unavailable"}
			},
		}

		err := executeConversationWatch(ctx, svc, watchParams{
			ConversationID: "conv-1",
			Once:           true,
			Filter:         "others",
			Limit:          50,
			JSON:           true,
			Stdout:         &stdout,
			Stderr:         &stderr,
			Sleep: func(_ context.Context, _ time.Duration) error {
				cancel()
				return context.Canceled
			},
		})
		require.Error(t, err)
		assert.Equal(t, 130, exitCodeFor(err))
	})
}

func TestWatchBackoffDuration_JitterAndCap(t *testing.T) {
	for attempt := 1; attempt <= 10; attempt++ {
		d := watchBackoffDuration(attempt, 0)
		assert.Greater(t, d, time.Duration(0), "attempt %d must be > 0", attempt)
		assert.LessOrEqual(t, d, watchMaxBackoff, "attempt %d must be <= 30s", attempt)
	}
	// Retry-After is honored up to watchMaxBackoff.
	assert.Equal(t, 12*time.Second, watchBackoffDuration(1, 12*time.Second))
	assert.Equal(t, watchMaxBackoff, watchBackoffDuration(1, 90*time.Second))
}
