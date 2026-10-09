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

package messaging

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A thread key names its project; the topic lookup answers only for a topic
// of that project.
func TestResolveConversationByKey_TopicInterceptRequiresSameProject(t *testing.T) {
	lookup := &mockTopicLookup{
		topics:   map[string]string{"topicB": "conv-of-project-b"},
		projects: map[string]string{"topicB": "projB"},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	// Same project: the topic's conversation.
	same := &mockConversationUpserter{returnConv: &store.Conversation{ID: "upserted"}}
	pidB := "projB"
	got, err := ResolveOrCreateConversationByKey(context.Background(), same, logger,
		"thread:projB:topicB", "group", &pidB, WithKeyTopicLookup(lookup))
	if err != nil || got == nil || got.ConversationID != "conv-of-project-b" {
		t.Fatalf("same project: got %+v, %v; want conv-of-project-b", got, err)
	}

	// Another project: never the other project's conversation. The key
	// falls through to its own project's thread:<project>:<id> row.
	other := &mockConversationUpserter{returnConv: &store.Conversation{ID: "row-of-project-a"}}
	pidA := "projA"
	got, err = ResolveOrCreateConversationByKey(context.Background(), other, logger,
		"thread:projA:topicB", "group", &pidA, WithKeyTopicLookup(lookup))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.ConversationID == "conv-of-project-b" {
		t.Fatalf("a key of project A must not resolve to project B's conversation, got %+v", got)
	}
	if other.lastConv == nil || other.lastConv.ExternalRef != "thread:projA:topicB" {
		t.Fatalf("expected the upsert of project A's own row, got %+v", other.lastConv)
	}
	if other.lastConv.ProjectID == nil || *other.lastConv.ProjectID != "projA" {
		t.Fatalf("the upserted row must belong to project A, got %+v", other.lastConv.ProjectID)
	}
}

// The read-side resolver applies the same rule.
func TestResolveThreadConversationForRead_TopicOfOtherProjectNotReturned(t *testing.T) {
	lookup := &mockTopicLookup{
		topics:   map[string]string{"topicB": "conv-of-project-b"},
		projects: map[string]string{"topicB": "projB"},
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	reader := &mockConversationReader{err: store.ErrNotFound}

	if got := ResolveThreadConversationForRead(context.Background(), reader, logger, "topicB", "projB",
		WithReadTopicLookup(lookup)); got == nil || got.ConversationID != "conv-of-project-b" {
		t.Fatalf("same project: got %+v", got)
	}
	if got := ResolveThreadConversationForRead(context.Background(), reader, logger, "topicB", "projA",
		WithReadTopicLookup(lookup)); got != nil && got.ConversationID == "conv-of-project-b" {
		t.Fatalf("a key of project A must not resolve to project B's conversation, got %+v", got)
	}
}
