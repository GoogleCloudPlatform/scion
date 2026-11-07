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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// errChatProjectLookup is the store error the lookup wrapper returns. Its
// text must never reach a response body.
var errChatProjectLookup = errors.New("chat project lookup: connection refused")

// chatProjectLookupStore answers GetProject with err when err is set.
type chatProjectLookupStore struct {
	store.Store
	err error
}

func (s *chatProjectLookupStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.Store.GetProject(ctx, id)
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
			srv, _ := testServer(t)
			srv.store = &chatProjectLookupStore{Store: srv.store, err: c.err}
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
