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

package hub

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sseFlushWriter injects client activity at the first header flush, exactly
// where EventSource can announce an open connection to a snapshot consumer.
type sseFlushWriter struct {
	*httptest.ResponseRecorder
	onFirstFlush func()
	onWrite      func() error
	flushed      bool
}

func (w *sseFlushWriter) Flush() {
	w.ResponseRecorder.Flush()
	if !w.flushed {
		w.flushed = true
		w.onFirstFlush()
	}
}

func (w *sseFlushWriter) Write(p []byte) (int, error) {
	if w.onWrite != nil {
		if err := w.onWrite(); err != nil {
			return 0, err
		}
	}
	return w.ResponseRecorder.Write(p)
}

func newSSEOrderingRequest(t *testing.T) (*WebServer, *ChannelEventPublisher, *http.Request) {
	t.Helper()
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	ws := &WebServer{
		events:       pub,
		authzService: NewAuthzService(&mockAuthzStore{}, nil),
	}
	req := httptest.NewRequest(http.MethodGet, "/events?sub=user.user-1.message", nil)
	user := &webSessionUser{UserID: "user-1", Role: "user"}
	req = req.WithContext(context.WithValue(req.Context(), webUserContextKey{}, user))
	return ws, pub, req
}

func assertNoSSESubscribers(t *testing.T, pub *ChannelEventPublisher) {
	t.Helper()
	pub.mu.RLock()
	defer pub.mu.RUnlock()
	for pattern, subscribers := range pub.subscribers {
		assert.Empty(t, subscribers, "subscription leaked for %s", pattern)
	}
}

func TestSSEHandler_EventAtFirstFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ws, pub, req := newSSEOrderingRequest(t)
		ctx, cancel := context.WithCancel(req.Context())
		defer cancel()
		w := &sseFlushWriter{
			ResponseRecorder: httptest.NewRecorder(),
			onFirstFlush: func() {
				pub.publish("user.user-1.message", map[string]string{"text": "first-flush"})
			},
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			ws.handleSSE(w, req.WithContext(ctx))
		}()

		// Wait until the handler has drained any buffered event and is blocked
		// waiting for more. No timer, retry publication, or scheduling race.
		synctest.Wait()
		assert.Equal(t, "id: 1\nevent: update\ndata: {\"subject\":\"user.user-1.message\",\"data\":{\"text\":\"first-flush\"}}\n\n", w.Body.String())
		assert.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
		cancel()
		<-done
		assertNoSSESubscribers(t, pub)
	})
}

func TestSSEHandler_SubscriptionCleanup(t *testing.T) {
	for _, exit := range []string{"cancel at first flush", "publisher closed", "write failure and cancellation", "first flush panic"} {
		t.Run(exit, func(t *testing.T) {
			ws, pub, req := newSSEOrderingRequest(t)
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			w := &sseFlushWriter{ResponseRecorder: httptest.NewRecorder()}
			w.onFirstFlush = func() {
				pub.mu.RLock()
				count := len(pub.subscribers["user.user-1.message"])
				pub.mu.RUnlock()
				require.Equal(t, 1, count, "subscription must exist before first flush")
				switch exit {
				case "cancel at first flush":
					cancel()
				case "publisher closed":
					pub.Close()
				case "write failure and cancellation":
					pub.publish("user.user-1.message", map[string]string{"text": "disconnect"})
				case "first flush panic":
					panic(http.ErrAbortHandler)
				}
			}
			if exit == "write failure and cancellation" {
				w.onWrite = func() error {
					cancel() // net/http cancels the request when the client disconnects.
					return io.ErrClosedPipe
				}
			}
			if exit == "first flush panic" {
				assert.PanicsWithValue(t, http.ErrAbortHandler, func() { ws.handleSSE(w, req.WithContext(ctx)) })
			} else {
				ws.handleSSE(w, req.WithContext(ctx))
			}
			assertNoSSESubscribers(t, pub)
		})
	}
}

func TestSSEHandler_RejectedBeforeSubscription(t *testing.T) {
	for _, tc := range []struct {
		query string
		code  int
	}{
		{"", http.StatusBadRequest},
		{"?sub=user..message", http.StatusBadRequest},
		{"?sub=user.other-user.message", http.StatusForbidden},
	} {
		t.Run(tc.query, func(t *testing.T) {
			ws, pub, req := newSSEOrderingRequest(t)
			req.URL = httptest.NewRequest(http.MethodGet, "/events"+tc.query, nil).URL
			w := httptest.NewRecorder()
			ws.handleSSE(w, req)
			assert.Equal(t, tc.code, w.Code)
			assert.False(t, w.Flushed)
			assertNoSSESubscribers(t, pub)
		})
	}
}
