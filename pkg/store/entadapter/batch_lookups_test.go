// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !no_sqlite

package entadapter

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withBatchLookupSize shrinks the IN-list chunk size for one test so small
// inputs exercise the multi-statement path.
func withBatchLookupSize(t *testing.T, n int) {
	t.Helper()
	prev := batchLookupSize
	batchLookupSize = n
	t.Cleanup(func() { batchLookupSize = prev })
}

func TestChunkKeys(t *testing.T) {
	assert.Nil(t, chunkKeys([]string(nil), 2))
	assert.Equal(t, [][]int{{1, 2}, {3, 4}, {5}}, chunkKeys([]int{1, 2, 3, 4, 5}, 2))
	assert.Equal(t, [][]int{{1, 2, 3}}, chunkKeys([]int{1, 2, 3}, 0))
}

// seedLatestMessageMix writes, per group key, a spread of messages that the
// latest-message lookups must tell apart: other channels, mention copies,
// same-instant ties, and groups with nothing visible. key sets ThreadID or
// ConversationID on each message.
func seedLatestMessageMix(t *testing.T, s *MessageStore, keys []string, key func(m *store.Message, k string)) {
	t.Helper()
	ctx := context.Background()
	projectID := uuid.NewString()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	add := func(k, channel, kind string, at time.Time) {
		m := &store.Message{ID: uuid.NewString(), ProjectID: projectID, Sender: "agent:a", Recipient: "user:u",
			RecipientID: "u", Msg: "m", Type: kind, Channel: channel, CreatedAt: at}
		key(m, k)
		require.NoError(t, s.CreateMessage(ctx, m))
	}
	for i, k := range keys {
		at := base.Add(time.Duration(i) * time.Minute)
		switch i % 5 {
		case 0: // visible, then newer on another channel and a newer mention
			add(k, "web", messages.TypeChat, at)
			add(k, "discord", messages.TypeChat, at.Add(time.Second))
			add(k, "web", messages.TypeMention, at.Add(2*time.Second))
		case 1: // several visible, newest wins
			add(k, "web", messages.TypeChat, at)
			add(k, "web", messages.TypeChat, at.Add(time.Second))
			add(k, "web", messages.TypeChat, at.Add(2*time.Second))
		case 2: // same-instant tie, broken by id
			add(k, "web", messages.TypeChat, at)
			add(k, "web", messages.TypeChat, at)
			add(k, "web", messages.TypeChat, at)
		case 3: // nothing visible
			add(k, "web", messages.TypeMention, at)
			add(k, "discord", messages.TypeChat, at)
		case 4: // no messages at all
		}
	}
}

func TestLatestMessagesByThreadIDs_MatchesListMessages(t *testing.T) {
	withBatchLookupSize(t, 2)
	s := newTestMessageStore(t)
	ctx := context.Background()

	keys := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		keys = append(keys, fmt.Sprintf("dm:agent:%d:user:u", i))
	}
	seedLatestMessageMix(t, s, keys, func(m *store.Message, k string) { m.ThreadID = k })

	for _, opts := range []store.LatestMessageOptions{
		{Channel: "web", ExcludeType: messages.TypeMention},
		{Channel: "web"},
		{},
	} {
		got, err := s.LatestMessagesByThreadIDs(ctx, append(keys, "", "dm:none"), opts)
		require.NoError(t, err)
		for _, k := range keys {
			want, err := s.ListMessages(ctx, store.MessageFilter{Channel: opts.Channel,
				ExcludeType: opts.ExcludeType, ThreadID: k}, store.ListOptions{Limit: 1, SkipTotalCount: true})
			require.NoError(t, err)
			if len(want.Items) == 0 {
				assert.NotContains(t, got, k, "opts %+v key %s", opts, k)
				continue
			}
			require.Contains(t, got, k, "opts %+v key %s", opts, k)
			assert.Equal(t, want.Items[0].ID, got[k].ID, "opts %+v key %s", opts, k)
		}
		assert.NotContains(t, got, "")
		assert.NotContains(t, got, "dm:none")
	}

	// The default scope hides groups 3 and 4 (mod 5) entirely.
	got, err := s.LatestMessagesByThreadIDs(ctx, keys, store.LatestMessageOptions{Channel: "web", ExcludeType: messages.TypeMention})
	require.NoError(t, err)
	assert.Len(t, got, 6)

	empty, err := s.LatestMessagesByThreadIDs(ctx, nil, store.LatestMessageOptions{})
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestLatestMessagesByConversationIDs_MatchesListMessages(t *testing.T) {
	withBatchLookupSize(t, 3)
	s := newTestMessageStore(t)
	ctx := context.Background()

	keys := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		keys = append(keys, uuid.NewString())
	}
	// A shared thread ID across conversations must not leak between them.
	seedLatestMessageMix(t, s, keys, func(m *store.Message, k string) { m.ConversationID = k; m.ThreadID = "shared" })

	opts := store.LatestMessageOptions{Channel: "web", ExcludeType: messages.TypeMention}
	got, err := s.LatestMessagesByConversationIDs(ctx, keys, opts)
	require.NoError(t, err)
	for _, k := range keys {
		want, err := s.ListMessages(ctx, store.MessageFilter{Channel: opts.Channel,
			ExcludeType: opts.ExcludeType, ConversationID: k}, store.ListOptions{Limit: 1, SkipTotalCount: true})
		require.NoError(t, err)
		if len(want.Items) == 0 {
			assert.NotContains(t, got, k)
			continue
		}
		require.Contains(t, got, k)
		assert.Equal(t, want.Items[0].ID, got[k].ID, "key %s", k)
		assert.Equal(t, k, got[k].ConversationID)
	}
	assert.Len(t, got, 6)

	_, err = s.LatestMessagesByConversationIDs(ctx, []string{"not-a-uuid"}, opts)
	assert.Error(t, err, "a malformed conversation ID is rejected, as ListMessages rejects it")
}

func TestGetConversationsByExternalRefs(t *testing.T) {
	withBatchLookupSize(t, 2)
	s := newTestConversationStore(t)
	ctx := context.Background()

	refs := make([]string, 0, 5)
	ids := map[string]string{}
	for i := 0; i < 5; i++ {
		c := newTestDMConversation("agent", uuid.NewString(), "user", uuid.NewString())
		c.Kind = "direct"
		got, err := s.UpsertConversationByExternalRef(ctx, c)
		require.NoError(t, err)
		refs = append(refs, got.ExternalRef)
		ids[got.ExternalRef] = got.ID
	}
	// Soft-deleted conversations are not returned, as with the single lookup.
	require.NoError(t, s.DeleteConversation(ctx, ids[refs[4]]))
	// Same ref on another surface is not returned.
	other := &store.Conversation{Kind: "direct", Surface: "discord", ExternalRef: refs[0]}
	_, err := s.UpsertConversationByExternalRef(ctx, other)
	require.NoError(t, err)

	got, err := s.GetConversationsByExternalRefs(ctx, "", append(refs, "", "dm:missing"))
	require.NoError(t, err)
	require.Len(t, got, 4)
	for _, ref := range refs[:4] {
		require.Contains(t, got, ref)
		assert.Equal(t, ids[ref], got[ref].ID)
		assert.Equal(t, "native", got[ref].Surface)
		single, err := s.GetConversationByExternalRef(ctx, "native", ref)
		require.NoError(t, err)
		assert.Equal(t, single, got[ref])
	}
	assert.NotContains(t, got, refs[4])

	none, err := s.GetConversationsByExternalRefs(ctx, "native", nil)
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestGetUsersByIDs(t *testing.T) {
	withBatchLookupSize(t, 2)
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	var ids []string
	for i := 0; i < 3; i++ {
		u := &store.User{ID: uuid.NewString(), Email: fmt.Sprintf("u%d@example.com", i),
			DisplayName: fmt.Sprintf("User %d", i), Role: store.UserRoleMember, Status: "active", Created: time.Now()}
		require.NoError(t, cs.CreateUser(ctx, u))
		ids = append(ids, u.ID)
	}

	got, err := cs.GetUsersByIDs(ctx, append(ids, uuid.NewString(), "not-a-uuid", ""))
	require.NoError(t, err)
	require.Len(t, got, 3)
	for _, id := range ids {
		single, err := cs.GetUser(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, single, got[id])
	}

	none, err := cs.GetUsersByIDs(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestGetAgentsByIDsIncludingDeleted(t *testing.T) {
	withBatchLookupSize(t, 2)
	s, projectID := newTestAgentStore(t)
	ctx := context.Background()

	var ids []string
	for i := 0; i < 3; i++ {
		a := makeAgent(projectID, fmt.Sprintf("batch-%d", i))
		require.NoError(t, s.CreateAgent(ctx, a))
		ids = append(ids, a.ID)
	}
	deleted, err := s.GetAgent(ctx, ids[2])
	require.NoError(t, err)
	deleted.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, deleted))

	got, err := s.GetAgentsByIDsIncludingDeleted(ctx, append(ids, uuid.NewString(), "not-a-uuid"))
	require.NoError(t, err)
	require.Len(t, got, 3, "soft-deleted agents are included, as GetAgent includes them")
	for _, id := range ids {
		single, err := s.GetAgent(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, single.Name, got[id].Name)
		assert.Equal(t, single.Slug, got[id].Slug)
		assert.Equal(t, single.DeletedAt.IsZero(), got[id].DeletedAt.IsZero())
	}

	live, err := s.GetAgentsByIDs(ctx, ids)
	require.NoError(t, err)
	assert.Len(t, live, 2, "GetAgentsByIDs keeps excluding soft-deleted agents")
}
