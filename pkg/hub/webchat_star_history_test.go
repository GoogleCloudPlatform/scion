//go:build !no_sqlite

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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// starHistoryIDs returns the message IDs of a starred-history page.
func starHistoryIDs(p StarHistoryPage) []string {
	ids := []string{}
	for _, it := range p.Items {
		ids = append(ids, it.ID)
	}
	return ids
}

// testStarredHistoryContract is the ListStarredHistory contract both
// dialects must meet identically: only the user's live starred messages of
// the selector's history scope, in (created, id) order with ties broken by
// ID, keyset pages without duplicates in both directions (a cursor at a
// tied message finds its sibling), and a total of the conversation's live
// starred messages.
func testStarredHistoryContract(t *testing.T, env starStoreEnv) {
	ctx := context.Background()
	st := env.stars
	userA, userB := uuid.NewString(), uuid.NewString()
	topic, topic2 := uuid.NewString(), uuid.NewString()
	env.addTopic(t, topic)
	env.addTopic(t, topic2)
	conv := uuid.NewString()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }

	msg := func(channel, thread string, created time.Time) string {
		id := uuid.NewString()
		env.addMsg(t, id, channel, thread, conv, created)
		return id
	}
	m1 := msg("web", topic, at(1))
	m2 := msg("web", topic, at(2))
	// m3 and m4 share an instant: the ID breaks the tie.
	m3 := msg("web", topic, at(3))
	m4 := msg("web", topic, at(3))
	unstarred := msg("web", topic, at(4))
	m6 := msg("web", topic, at(5).Add(1500*time.Microsecond))
	deleted := msg("web", topic, at(6))
	telegram := msg("telegram", topic, at(7))
	elsewhere := msg("web", topic2, at(8))

	star := func(user, id, key string) {
		_, _, err := st.AddMessageStar(ctx, user, StarTarget{MessageID: id, ProjectID: "proj-1", Selector: threadSel(key)})
		require.NoError(t, err)
	}
	for _, id := range []string{m1, m2, m3, m4, m6, deleted} {
		star(userA, id, topic)
	}
	star(userA, elsewhere, topic2)
	star(userB, m2, topic)
	// A star of another channel's message listed under the same key (the
	// API would refuse it) is outside the history scope.
	env.insertStar(t, userA, telegram, topic, "proj-1", time.Now())
	require.NoError(t, env.wcs.SetMessageDeleted(ctx, deleted, time.Now()))
	_ = unstarred

	tied := []string{m4, m3}
	if m3 > m4 {
		tied = []string{m3, m4}
	}
	newestFirst := []string{m6, tied[0], tied[1], m2, m1}

	for _, sel := range []StarSelector{threadSel(topic), {ConversationKey: topic, Channel: "web", ConversationID: conv}} {
		q := StarHistoryQuery{UserID: userA, Selector: sel}

		all := q
		all.Limit = 50
		page, err := st.ListStarredHistory(ctx, all)
		require.NoError(t, err)
		assert.Equal(t, newestFirst, starHistoryIDs(page), "newest first, ties by ID, live starred web messages only")
		assert.Nil(t, page.Next)
		assert.Equal(t, 5, page.Total)
		assert.True(t, page.Items[0].Created.Equal(at(5).Add(1500*time.Microsecond)), "the message's own creation instant")

		// Keyset pages of two, through the tie, without duplicates.
		var got []string
		var cursor *MessagePosition
		for i := 0; i < 5; i++ {
			p := q
			p.Limit = 2
			p.Cursor = cursor
			page, err := st.ListStarredHistory(ctx, p)
			require.NoError(t, err)
			assert.LessOrEqual(t, len(page.Items), 2)
			assert.Equal(t, 5, page.Total, "the total ignores the cursor")
			got = append(got, starHistoryIDs(page)...)
			if page.Next == nil {
				break
			}
			cursor = page.Next
		}
		assert.Equal(t, newestFirst, got)

		// Oldest first.
		asc := all
		asc.Ascending = true
		page, err = st.ListStarredHistory(ctx, asc)
		require.NoError(t, err)
		assert.Equal(t, []string{m1, m2, tied[1], tied[0], m6}, starHistoryIDs(page))

		// The halves of an around page start at the anchor's (created, id)
		// position: with the newer tied message as the anchor, its tied
		// sibling is in the older half, and the reverse in the newer one.
		tieNew := &MessagePosition{Created: at(3), ID: tied[0]}
		tieOld := &MessagePosition{Created: at(3), ID: tied[1]}
		older := all
		older.Cursor = tieNew
		page, err = st.ListStarredHistory(ctx, older)
		require.NoError(t, err)
		assert.Equal(t, []string{tied[1], m2, m1}, starHistoryIDs(page))
		assert.Equal(t, 5, page.Total, "the total ignores the cursor")
		newer := asc
		newer.Cursor = tieOld
		page, err = st.ListStarredHistory(ctx, newer)
		require.NoError(t, err)
		assert.Equal(t, []string{tied[0], m6}, starHistoryIDs(page))
		// A page of one past the anchor reports that more remain.
		older.Limit = 1
		page, err = st.ListStarredHistory(ctx, older)
		require.NoError(t, err)
		assert.Equal(t, []string{tied[1]}, starHistoryIDs(page))
		require.NotNil(t, page.Next)
		assert.Equal(t, tied[1], page.Next.ID)
	}

	// Another user's stars are their own.
	page, err := st.ListStarredHistory(ctx, StarHistoryQuery{UserID: userB, Selector: threadSel(topic), Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, []string{m2}, starHistoryIDs(page))
	assert.Equal(t, 1, page.Total)

	// The other conversation lists its own star only.
	page, err = st.ListStarredHistory(ctx, StarHistoryQuery{UserID: userA, Selector: threadSel(topic2), Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, []string{elsewhere}, starHistoryIDs(page))

	// A message that left the thread (its star still listed under the old
	// key) is out of the thread's history scope.
	env.moveMsg(t, m1, topic2)
	page, err = st.ListStarredHistory(ctx, StarHistoryQuery{UserID: userA, Selector: threadSel(topic), Limit: 50})
	require.NoError(t, err)
	assert.Equal(t, []string{m6, tied[0], tied[1], m2}, starHistoryIDs(page))
	assert.Equal(t, 4, page.Total)

	// No stars: an empty page, not nil.
	page, err = st.ListStarredHistory(ctx, StarHistoryQuery{UserID: uuid.NewString(), Selector: threadSel(topic), Limit: 50})
	require.NoError(t, err)
	assert.NotNil(t, page.Items)
	assert.Empty(t, page.Items)
	assert.Zero(t, page.Total)

	// Invalid input.
	for _, bad := range []StarHistoryQuery{
		{UserID: userA, Selector: threadSel(topic)},
		{Selector: threadSel(topic), Limit: 1},
		{UserID: userA, Limit: 1},
	} {
		_, err := st.ListStarredHistory(ctx, bad)
		assert.ErrorIs(t, err, store.ErrInvalidInput)
	}
}

// TestStarredHistory_SQLiteLegacyTimeSuffix: on SQLite a message row
// written before the UTC store boundary may carry a monotonic-clock suffix
// in its created text. Starred history orders, bounds and parses such a
// row at its instant.
func TestStarredHistory_SQLiteLegacyTimeSuffix(t *testing.T) {
	f := newStarFixture(t, false)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	older := f.threadMsg(t, f.topicA, f.projA, "older", base)
	legacy := f.threadMsg(t, f.topicA, f.projA, "legacy", base.Add(time.Minute))
	newer := f.threadMsg(t, f.topicA, f.projA, "newer", base.Add(2*time.Minute))
	for _, id := range []string{older, legacy, newer} {
		_, _, err := f.stars.AddMessageStar(ctx, f.admin, StarTarget{MessageID: id, ProjectID: f.projA, Selector: threadSel(f.topicA)})
		require.NoError(t, err)
	}
	_, err := f.db.Exec(`UPDATE messages SET created = created || ' m=+0.0336' WHERE id = ?`, legacy)
	require.NoError(t, err)

	q := StarHistoryQuery{UserID: f.admin, Selector: threadSel(f.topicA), Limit: 50}
	page, err := f.stars.ListStarredHistory(ctx, q)
	require.NoError(t, err)
	assert.Equal(t, []string{newer, legacy, older}, starHistoryIDs(page))
	assert.True(t, page.Items[1].Created.Equal(base.Add(time.Minute)), "parsed without the suffix")

	q.Cursor = &MessagePosition{Created: base.Add(time.Minute), ID: legacy}
	q.Ascending = true
	page, err = f.stars.ListStarredHistory(ctx, q)
	require.NoError(t, err)
	assert.Equal(t, []string{newer}, starHistoryIDs(page), "the legacy row is at its own instant, not after it")
}
