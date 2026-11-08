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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// errChatProjectLookup is the store error the lookup wrapper returns. Its
// text must never reach a response body.
var errChatProjectLookup = errors.New("chat project lookup: connection refused")

// chatProjectLookupStore answers GetProject with err while its fault switch
// is armed, and delegates to the inner store otherwise. It is installed with
// testServerWithStoreFault, so the test never reassigns srv.store.
type chatProjectLookupStore struct {
	store.Store
	fault *storeFaultSwitch
	err   error
}

func (s *chatProjectLookupStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if !s.fault.Active() {
		return s.Store.GetProject(ctx, id)
	}
	return nil, s.err
}

// chatTopicStore answers GetTopic with a fixed topic; no other method is used.
type chatTopicStore struct {
	WebChatStore
	topic *WebChatTopic
}

func (s chatTopicStore) GetTopic(ctx context.Context, id string) (*WebChatTopic, error) {
	return s.topic, nil
}

// TestRequireChatActions_ProjectLookupErrors: when the project lookup of a
// chat action check fails, a store error answers a 500 with the generic text
// (not a 404 that hides it, and not the wrapped error), and a missing
// project answers a 404. On the message path that 404 says "Thread not
// found", the same answer a missing topic gets.
func TestRequireChatActions_ProjectLookupErrors(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		want     int
		threadIn string // body text of the requireChatMessageAction answer
		convIn   string // body text of the requireChatConversationAction answer
	}{
		{name: "store error", err: errChatProjectLookup, want: http.StatusInternalServerError,
			threadIn: "Internal server error", convIn: "Internal server error"},
		{name: "missing project", err: store.ErrNotFound, want: http.StatusNotFound,
			threadIn: "Thread not found", convIn: "Resource not found"},
	}
	user := NewAuthenticatedUser("chat-lookup-user", "chat-lookup@example.com", "Chat Lookup", "member", "cli")
	projectID := "chat-lookup-project"
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *chatProjectLookupStore {
				return &chatProjectLookupStore{Store: inner, fault: f, err: c.err}
			})
			fault.Arm()
			check := func(t *testing.T, rec *httptest.ResponseRecorder, ok bool, in string) {
				t.Helper()
				assert.False(t, ok)
				assert.Equal(t, c.want, rec.Code, rec.Body.String())
				assert.NotContains(t, rec.Body.String(), "connection refused")
				assert.Contains(t, rec.Body.String(), in)
			}

			t.Run("requireChatMessageAction", func(t *testing.T) {
				wcs := chatTopicStore{topic: &WebChatTopic{ID: "chat-lookup-topic", ProjectID: projectID}}
				req := httptest.NewRequest(http.MethodPost, "/", nil)
				req = req.WithContext(contextWithIdentity(req.Context(), user))
				rec := httptest.NewRecorder()
				ok := srv.requireChatMessageAction(rec, req, user, wcs, "chat-lookup-topic", ChatActionPost)
				check(t, rec, ok, c.threadIn)
			})
			t.Run("requireChatConversationAction", func(t *testing.T) {
				conv := &store.Conversation{ID: "chat-lookup-conversation", ProjectID: &projectID}
				req := httptest.NewRequest(http.MethodPost, "/", nil)
				req = req.WithContext(contextWithIdentity(req.Context(), user))
				rec := httptest.NewRecorder()
				ok := srv.requireChatConversationAction(rec, req, conv, ChatActionPost)
				check(t, rec, ok, c.convIn)
				// The resource name is not passed as the request id.
				assert.NotContains(t, rec.Body.String(), "requestId")
			})
		})
	}
}

// TestAuthorizeChatConversation_ProjectLookupErrors: when the topic's
// project lookup in authorizeChatSend, authorizeChatSendRead or
// authorizeConversationAccess fails, a missing project answers "Thread not
// found" (404), and any other store error answers the generic 500 with no
// store error text.
func TestAuthorizeChatConversation_ProjectLookupErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		msg  string
	}{
		{name: "store error", err: errChatProjectLookup, want: http.StatusInternalServerError, msg: "Internal server error"},
		{name: "missing project", err: store.ErrNotFound, want: http.StatusNotFound, msg: "Thread not found"},
	}
	user := NewAuthenticatedUser("chat-lookup-user", "chat-lookup@example.com", "Chat Lookup", "member", "cli")
	key := "chat-lookup-topic"
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *chatProjectLookupStore {
				return &chatProjectLookupStore{Store: inner, fault: f, err: c.err}
			})
			wcs := chatTopicStore{topic: &WebChatTopic{ID: key, ProjectID: "chat-lookup-project"}}
			srv.mu.Lock()
			srv.webChatStore = wcs
			srv.mu.Unlock()
			fault.Arm()
			ctx := contextWithIdentity(context.Background(), user)

			for name, authorize := range map[string]func(context.Context, UserIdentity, string) (*chatSendTarget, *chatSendError){
				"authorizeChatSend":     srv.authorizeChatSend,
				"authorizeChatSendRead": srv.authorizeChatSendRead,
			} {
				t.Run(name, func(t *testing.T) {
					target, serr := authorize(ctx, user, key)
					assert.Nil(t, target)
					if !assert.NotNil(t, serr) {
						return
					}
					assert.Equal(t, c.want, serr.Status)
					assert.Equal(t, c.msg, serr.Message)
					assert.False(t, serr.accessRefused)

					rec := httptest.NewRecorder()
					serr.write(rec)
					assert.Equal(t, c.want, rec.Code)
					assert.Contains(t, rec.Body.String(), c.msg)
					assert.NotContains(t, rec.Body.String(), "connection refused")
				})
			}
			t.Run("authorizeConversationAccess", func(t *testing.T) {
				req := httptest.NewRequest(http.MethodPost, "/", nil)
				req = req.WithContext(ctx)
				rec := httptest.NewRecorder()
				ok := srv.authorizeConversationAccess(rec, req, wcs, key, user.ID(), "")
				assert.False(t, ok)
				assert.Equal(t, c.want, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), c.msg)
				assert.NotContains(t, rec.Body.String(), "connection refused")
			})
		})
	}
}

// chatTopicErrStore answers GetTopic with (nil, err); no other method is used.
type chatTopicErrStore struct {
	WebChatStore
	err error
}

func (s chatTopicErrStore) GetTopic(ctx context.Context, id string) (*WebChatTopic, error) {
	return nil, s.err
}

// TestChatKeyReadAudience_TopicLookupErrors: a topic that does not exist
// (a nil topic, or store.ErrNotFound) is no audience with no error, and
// answers "Thread not found" as before; any other GetTopic error is
// returned, and the send authorization and the conversation access check
// answer it with the generic 500 and no store error text.
func TestChatKeyReadAudience_TopicLookupErrors(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantErr bool
		want    int
		msg     string
	}{
		{name: "store error", err: errChatProjectLookup, wantErr: true, want: http.StatusInternalServerError, msg: "Internal server error"},
		{name: "not found error", err: store.ErrNotFound, want: http.StatusNotFound, msg: "Thread not found"},
		{name: "missing topic", err: nil, want: http.StatusNotFound, msg: "Thread not found"},
	}
	user := NewAuthenticatedUser("chat-lookup-user", "chat-lookup@example.com", "Chat Lookup", "member", "cli")
	key := "chat-lookup-topic"
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wcs := chatTopicErrStore{err: c.err}
			ctx := contextWithIdentity(context.Background(), user)

			aud, topic, err := chatKeyReadAudience(ctx, wcs, key)
			assert.Equal(t, chatAudienceNone, aud)
			assert.Nil(t, topic)
			if c.wantErr {
				assert.ErrorIs(t, err, c.err)
			} else {
				assert.NoError(t, err)
			}

			srv, _ := testServer(t)
			srv.mu.Lock()
			srv.webChatStore = wcs
			srv.mu.Unlock()

			t.Run("authorizeChatSend", func(t *testing.T) {
				target, serr := srv.authorizeChatSend(ctx, user, key)
				assert.Nil(t, target)
				if !assert.NotNil(t, serr) {
					return
				}
				assert.Equal(t, c.want, serr.Status)
				assert.Equal(t, c.msg, serr.Message)
				rec := httptest.NewRecorder()
				serr.write(rec)
				assert.Equal(t, c.want, rec.Code)
				assert.NotContains(t, rec.Body.String(), "connection refused")
			})
			t.Run("authorizeConversationAccess", func(t *testing.T) {
				req := httptest.NewRequest(http.MethodPost, "/", nil)
				req = req.WithContext(ctx)
				rec := httptest.NewRecorder()
				ok := srv.authorizeConversationAccess(rec, req, wcs, key, user.ID(), "")
				assert.False(t, ok)
				assert.Equal(t, c.want, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), c.msg)
				assert.NotContains(t, rec.Body.String(), "connection refused")
			})
			// A fail-closed guard, not a regression check: the projection
			// answers false for every case here with or without the change
			// (a missing topic and a store fault both mean no copy).
			t.Run("projectionAudienceIsProject", func(t *testing.T) {
				assert.False(t, srv.projectionAudienceIsProject(ctx, key, "chat-lookup-project"))
			})
		})
	}
}

// chatTopicCallStore records whether GetTopic was called and answers it
// with err; no other method is used.
type chatTopicCallStore struct {
	WebChatStore
	called *bool
	err    error
}

func (s chatTopicCallStore) GetTopic(ctx context.Context, id string) (*WebChatTopic, error) {
	*s.called = true
	return nil, s.err
}

// TestChatKeyReadAudience_InvalidTopicKey: a topic key that is not valid
// UTF-8 or contains a NUL names no topic. It is answered as a missing topic
// ("Thread not found") without a store lookup, so a store that would reject
// it (Postgres refuses NUL and invalid UTF-8 in text) cannot turn it into a
// 500. A store fault on a valid key still answers the 500 when the client
// has gone away (it is only logged at Debug then).
func TestChatKeyReadAudience_InvalidTopicKey(t *testing.T) {
	user := NewAuthenticatedUser("chat-lookup-user", "chat-lookup@example.com", "Chat Lookup", "member", "cli")
	for name, key := range map[string]string{
		"invalid utf8": "chat-lookup-topic-\xff",
		"nul":          "chat-lookup\x00topic",
	} {
		t.Run(name, func(t *testing.T) {
			called := false
			wcs := chatTopicCallStore{called: &called, err: errChatProjectLookup}
			ctx := contextWithIdentity(context.Background(), user)

			aud, topic, err := chatKeyReadAudience(ctx, wcs, key)
			assert.Equal(t, chatAudienceNone, aud)
			assert.Nil(t, topic)
			assert.NoError(t, err)
			assert.False(t, called, "the key must not reach the store")

			srv, _ := testServer(t)
			srv.mu.Lock()
			srv.webChatStore = wcs
			srv.mu.Unlock()

			_, serr := srv.authorizeChatSend(ctx, user, key)
			if assert.NotNil(t, serr) {
				assert.Equal(t, http.StatusNotFound, serr.Status)
				assert.Equal(t, "Thread not found", serr.Message)
			}

			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req = req.WithContext(ctx)
			rec := httptest.NewRecorder()
			assert.False(t, srv.authorizeConversationAccess(rec, req, wcs, key, user.ID(), ""))
			assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "Thread not found")

			assert.False(t, srv.projectionAudienceIsProject(ctx, key, "chat-lookup-project"))
			assert.False(t, called, "no caller sends the key to the store")
		})
	}

	t.Run("store fault after the client went away", func(t *testing.T) {
		called := false
		wcs := chatTopicCallStore{called: &called, err: errChatProjectLookup}
		ctx, cancel := context.WithCancel(contextWithIdentity(context.Background(), user))
		cancel()

		srv, _ := testServer(t)
		srv.mu.Lock()
		srv.webChatStore = wcs
		srv.mu.Unlock()

		_, serr := srv.authorizeChatSend(ctx, user, "chat-lookup-topic")
		assert.True(t, called)
		if assert.NotNil(t, serr) {
			assert.Equal(t, http.StatusInternalServerError, serr.Status)
			assert.Equal(t, "Internal server error", serr.Message)
		}
	})
}

// loggedChatKeyRecord is the part of a JSON log record chatKeyLogArgs
// writes.
type loggedChatKeyRecord struct {
	Msg    string `json:"msg"`
	Key    string `json:"key"`
	KeyLen int    `json:"key_len"`
}

// TestChatKeyLogArgs: the caller-supplied chat key is logged whole when it
// fits, cut to at most maxLoggedChatKeyBytes bytes on a rune boundary
// otherwise, always with its full length as key_len; a control byte in it
// cannot split the log entry.
func TestChatKeyLogArgs(t *testing.T) {
	long := strings.Repeat("k", 300)
	// A two-byte rune straddles the 128-byte cut.
	straddle := strings.Repeat("k", maxLoggedChatKeyBytes-1) + strings.Repeat("é", 10)
	cases := []struct {
		name string
		key  string
		want string
	}{
		{name: "short key", key: "chat-topic", want: "chat-topic"},
		{name: "exactly the bound", key: long[:maxLoggedChatKeyBytes], want: long[:maxLoggedChatKeyBytes]},
		{name: "over the bound", key: long, want: long[:maxLoggedChatKeyBytes]},
		{name: "cut would split a rune", key: straddle, want: straddle[:maxLoggedChatKeyBytes-1]},
		{name: "control bytes", key: "chat\ntopic\r\x1b", want: "chat\ntopic\r\x1b"},
		// A four-byte rune starts at byte 127, so the cut backs off to it.
		{name: "four-byte rune at the bound", key: long[:maxLoggedChatKeyBytes-1] + "\U0001F600", want: long[:maxLoggedChatKeyBytes-1]},
		// Invalid UTF-8, all continuation bytes: no rune start, so nothing is kept.
		{name: "continuation bytes only", key: strings.Repeat("\x80", 200), want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			slog.New(slog.NewJSONHandler(&buf, nil)).Warn("m", chatKeyLogArgs(c.key)...)
			var rec loggedChatKeyRecord
			require.NoError(t, json.Unmarshal(buf.Bytes(), &rec), buf.String())
			assert.Equal(t, c.want, rec.Key)
			assert.LessOrEqual(t, len(rec.Key), maxLoggedChatKeyBytes)
			assert.True(t, utf8.ValidString(rec.Key))
			assert.Equal(t, len(c.key), rec.KeyLen)

			// One log entry is one line, in either handler.
			var text bytes.Buffer
			slog.New(slog.NewTextHandler(&text, nil)).Warn("m", chatKeyLogArgs(c.key)...)
			for _, out := range []string{buf.String(), text.String()} {
				assert.Equal(t, 1, strings.Count(out, "\n"), out)
				assert.True(t, strings.HasSuffix(out, "\n"), out)
			}
		})
	}
}

// TestChatStoreFault_LogsBoundedKey: a store fault on the topic lookup of
// an over-long caller-supplied key logs the key bounded, with its full
// length as key_len. Not parallel: it swaps slog.Default.
func TestChatStoreFault_LogsBoundedKey(t *testing.T) {
	user := NewAuthenticatedUser("chat-lookup-user", "chat-lookup@example.com", "Chat Lookup", "member", "cli")
	key := strings.Repeat("k", maxLoggedChatKeyBytes-1) + strings.Repeat("é", 500)
	wcs := chatTopicErrStore{err: errChatProjectLookup}
	srv, _ := testServer(t)
	srv.mu.Lock()
	srv.webChatStore = wcs
	srv.mu.Unlock()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	_, serr := srv.authorizeChatSend(contextWithIdentity(context.Background(), user), user, key)
	slog.SetDefault(prev)
	require.NotNil(t, serr)
	assert.Equal(t, http.StatusInternalServerError, serr.Status)

	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec loggedChatKeyRecord
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Msg != "chat send: topic lookup failed" {
			continue
		}
		found = true
		assert.Equal(t, key[:maxLoggedChatKeyBytes-1], rec.Key)
		assert.Equal(t, len(key), rec.KeyLen)
		assert.NotContains(t, line, key)
	}
	assert.True(t, found, "no topic lookup fault was logged: %s", buf.String())
}
