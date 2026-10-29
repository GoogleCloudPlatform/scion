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
	"database/sql"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/internal/hubtestutil"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// starredConversationKeys is the conversation keys of a page, in order.
func starredConversationKeys(p StarredConversationPage) []string {
	keys := []string{}
	for _, c := range p.Items {
		keys = append(keys, c.ConversationKey)
	}
	return keys
}

// walkStarredConversations reads every page of f with page size limit and
// returns the keys in order; it fails on a repeated key.
func walkStarredConversations(t *testing.T, st MessageStarStore, f StarredConversationFilter, limit int) []string {
	t.Helper()
	var keys []string
	seen := map[string]bool{}
	f.Limit = limit
	f.After = nil
	for i := 0; i < 100; i++ {
		page, err := st.ListStarredConversationCandidates(context.Background(), f)
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Items), limit)
		for _, c := range page.Items {
			require.False(t, seen[c.ConversationKey], "conversation %s repeated across pages", c.ConversationKey)
			seen[c.ConversationKey] = true
			keys = append(keys, c.ConversationKey)
		}
		if page.Next == nil {
			return keys
		}
		require.Equal(t, page.Items[len(page.Items)-1].Position, *page.Next, "Next is the last item's position")
		f.After = page.Next
	}
	t.Fatal("pagination did not end")
	return nil
}

// testStarredConversationsContract is the Starred rail query contract both
// dialects must meet identically: one row per conversation with a live
// star, deleted/moved/other-channel/deleted-topic stars ignored, the
// activity and name orders with ties, keyset pages that never repeat or
// skip a conversation, and the project scope.
func testStarredConversationsContract(t *testing.T, env starStoreEnv) {
	ctx := context.Background()
	st := env.stars
	user, other := uuid.NewString(), uuid.NewString()
	projA, projB := env.projectID, uuid.NewString()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	add := func(u, msgID, key, projectID string) {
		t.Helper()
		_, _, err := st.AddMessageStar(ctx, u, StarTarget{MessageID: msgID, ProjectID: projectID,
			Selector: StarSelector{ConversationKey: key, Channel: "web", ThreadID: key}})
		require.NoError(t, err)
	}
	msg := func(key string, at time.Time) string {
		t.Helper()
		id := uuid.NewString()
		env.addMsg(t, id, "web", key, "", at)
		return id
	}
	topic := func(projectID, name string, activity *time.Time) string {
		t.Helper()
		id := uuid.NewString()
		env.addTopic(t, id)
		env.setTopic(t, id, projectID, name, activity)
		return id
	}
	at := func(d time.Duration) *time.Time { v := base.Add(d); return &v }

	// Threads: "beta" newest; "Alpha" and "alpha two" share an activity
	// instant (the key breaks the tie); "Gamma" has no activity yet.
	beta := topic(projA, "beta", at(50*time.Minute))
	alpha := topic(projA, "Alpha", at(40*time.Minute))
	alphaTwo := topic(projB, "alpha two", at(40*time.Minute))
	gamma := topic(projA, "Gamma", nil)
	for _, k := range []string{beta, alpha, alpha, alphaTwo, gamma} {
		add(user, msg(k, base), k, projA)
	}

	// DMs: an agent DM stored under project B, a DM with a user, and a DM
	// with an agent that has no record.
	agentID, peerUser, goneAgent := uuid.NewString(), uuid.NewString(), uuid.NewString()
	env.addAgent(t, agentID, "Zed agent", "zed")
	env.addNamedUser(t, peerUser, "  Mia  ")
	agentDM := "dm:agent:" + agentID + ":user:" + user
	humanDM := "dm:user:" + user + ":user:" + peerUser
	goneDM := "dm:agent:" + goneAgent + ":user:" + user
	env.addDMRow(t, user, agentDM, "agent", agentID, base.Add(45*time.Minute))
	env.addDMRow(t, user, humanDM, "user", peerUser, base.Add(10*time.Minute))
	env.addDMRow(t, user, goneDM, "agent", goneAgent, base.Add(5*time.Minute))
	add(user, msg(agentDM, base), agentDM, projB)
	add(user, msg(humanDM, base), humanDM, "")
	add(user, msg(goneDM, base), goneDM, "")

	// Ignored: a deleted message, a moved message, another channel's
	// message, a deleted topic, and another user's star.
	deletedOnly := topic(projA, "deleted only", at(55*time.Minute))
	mDel := msg(deletedOnly, base)
	add(user, mDel, deletedOnly, projA)
	require.NoError(t, env.wcs.SetMessageDeleted(ctx, mDel, time.Now()))
	movedFrom := topic(projA, "moved from", at(56*time.Minute))
	movedTo := topic(projA, "moved to", at(57*time.Minute))
	mMoved := msg(movedFrom, base)
	add(user, mMoved, movedFrom, projA)
	env.moveMsg(t, mMoved, movedTo)
	otherChannel := topic(projA, "other channel", at(58*time.Minute))
	mOther := uuid.NewString()
	env.addMsg(t, mOther, "telegram", otherChannel, "", base)
	env.insertStar(t, user, mOther, otherChannel, projA, base)
	goneTopic := topic(projA, "gone topic", at(59*time.Minute))
	add(user, msg(goneTopic, base), goneTopic, projA)
	env.deleteTopic(t, goneTopic)
	othersTopic := topic(projA, "others", at(60*time.Minute))
	add(other, msg(othersTopic, base), othersTopic, projA)

	byActivity := StarredConversationFilter{UserID: user, Channel: "web", Sort: StarredConversationsByActivity}
	tied := []string{alpha, alphaTwo}
	sort.Strings(tied)
	wantActivity := []string{beta, agentDM, tied[0], tied[1], humanDM, goneDM, gamma}

	t.Run("activity order, one row per conversation", func(t *testing.T) {
		f := byActivity
		f.Limit = 100
		page, err := st.ListStarredConversationCandidates(ctx, f)
		require.NoError(t, err)
		assert.Nil(t, page.Next)
		assert.Equal(t, wantActivity, starredConversationKeys(page))
		rows := map[string]StarredConversationCandidate{}
		for _, c := range page.Items {
			rows[c.ConversationKey] = c
		}
		assert.Equal(t, StarredConversationCandidate{
			ConversationKey: beta, ProjectID: projA, TopicName: "beta", LastActivityAt: base.Add(50 * time.Minute),
			Position: StarredConversationPosition{Value: base.Add(50 * time.Minute).Format(starActivityKeyLayout), Key: beta},
		}, rows[beta])
		assert.True(t, rows[agentDM].IsDM)
		assert.Equal(t, projB, rows[agentDM].ProjectID, "an agent DM carries its stars' project")
		assert.Empty(t, rows[humanDM].ProjectID, "a DM between users has no project")
		assert.Empty(t, rows[humanDM].TopicName)
		assert.True(t, rows[humanDM].LastActivityAt.Equal(base.Add(10*time.Minute)), "a DM's activity is the caller's DM row")
		assert.True(t, rows[gamma].LastActivityAt.IsZero())
		assert.Equal(t, "", rows[gamma].Position.Value, "no activity sorts last")
		for _, c := range page.Items {
			if c.Position.Value != "" {
				_, err := parseStarActivityKey(c.Position.Value)
				assert.NoError(t, err, "fixed-width activity value %q", c.Position.Value)
			}
		}
	})

	t.Run("activity pages never repeat or skip", func(t *testing.T) {
		for _, limit := range []int{1, 2, 3} {
			assert.Equal(t, wantActivity, walkStarredConversations(t, st, byActivity, limit), "limit %d", limit)
		}
	})

	t.Run("name order folds case and labels DM peers", func(t *testing.T) {
		f := byActivity
		f.Sort = StarredConversationsByName
		// "alpha" < "alpha two" < "beta" < "deleted agent" < "gamma" < "mia" < "zed agent".
		want := []string{alpha, alphaTwo, beta, goneDM, gamma, humanDM, agentDM}
		f.Limit = 100
		page, err := st.ListStarredConversationCandidates(ctx, f)
		require.NoError(t, err)
		assert.Equal(t, want, starredConversationKeys(page))
		assert.Equal(t, "mia", page.Items[5].Position.Value, "trimmed, folded peer label")
		for _, limit := range []int{1, 2, 4} {
			assert.Equal(t, want, walkStarredConversations(t, st, f, limit), "limit %d", limit)
		}
	})

	t.Run("name ties break by key", func(t *testing.T) {
		u := uuid.NewString()
		// Topic names are unique per project regardless of case, so the
		// tied names live in two projects.
		k1 := topic(projA, "Same", at(time.Minute))
		k2 := topic(projB, "same", at(2*time.Minute))
		add(u, msg(k1, base), k1, projA)
		add(u, msg(k2, base), k2, projB)
		want := []string{k1, k2}
		sort.Strings(want)
		f := StarredConversationFilter{UserID: u, Channel: "web", Sort: StarredConversationsByName}
		assert.Equal(t, want, walkStarredConversations(t, st, f, 1))
	})

	t.Run("project scope", func(t *testing.T) {
		f := byActivity
		f.ProjectID = projB
		assert.Equal(t, []string{agentDM, alphaTwo}, walkStarredConversations(t, st, f, 1),
			"project B's thread and the DM stored under project B; never a DM between users")
		f.ProjectID = projA
		assert.Equal(t, []string{beta, alpha, gamma}, walkStarredConversations(t, st, f, 100))
	})

	t.Run("a new star and unstarring move rows", func(t *testing.T) {
		u := uuid.NewString()
		k := topic(projA, "late", at(time.Minute))
		m := msg(k, base)
		f := StarredConversationFilter{UserID: u, Channel: "web", Sort: StarredConversationsByActivity}
		assert.Empty(t, walkStarredConversations(t, st, f, 10))
		add(u, m, k, projA)
		assert.Equal(t, []string{k}, walkStarredConversations(t, st, f, 10))
		_, err := st.RemoveMessageStar(ctx, u, m)
		require.NoError(t, err)
		assert.Empty(t, walkStarredConversations(t, st, f, 10))
	})

	t.Run("channel filter", func(t *testing.T) {
		f := byActivity
		f.Channel = ""
		keys := walkStarredConversations(t, st, f, 100)
		assert.Contains(t, keys, otherChannel, "an empty channel matches every channel")
	})

	t.Run("non-ASCII names fold ASCII letters only, in byte order, on both dialects", func(t *testing.T) {
		u := uuid.NewString()
		eclair := topic(projA, "Éclair", at(time.Minute))
		ea := topic(projA, "éa", at(2*time.Minute))
		zed := topic(projA, "Zed", at(3*time.Minute))
		for _, k := range []string{eclair, ea, zed} {
			add(u, msg(k, base), k, projA)
		}
		f := StarredConversationFilter{UserID: u, Channel: "web", Sort: StarredConversationsByName, Limit: 10}
		page, err := st.ListStarredConversationCandidates(ctx, f)
		require.NoError(t, err)
		// "zed" (0x7a) < "Éclair" (0xc3 0x89) < "éa" (0xc3 0xa9): É is not folded.
		assert.Equal(t, []string{zed, eclair, ea}, starredConversationKeys(page))
		assert.Equal(t, []string{"zed", "Éclair", "éa"}, []string{
			page.Items[0].Position.Value, page.Items[1].Position.Value, page.Items[2].Position.Value})
		assert.Equal(t, []string{zed, eclair, ea}, walkStarredConversations(t, st, f, 1))
	})

	t.Run("a peer label trims the whitespace the displayed label trims", func(t *testing.T) {
		u := uuid.NewString()
		padded, blank := uuid.NewString(), uuid.NewString()
		env.addNamedUser(t, padded, "\u00a0\tNia\u3000")
		env.addNamedUser(t, blank, "\u00a0\u2003")
		for _, peer := range []string{padded, blank} {
			key := "dm:user:" + u + ":user:" + peer
			env.addDMRow(t, u, key, "user", peer, base)
			add(u, msg(key, base), key, "")
		}
		f := StarredConversationFilter{UserID: u, Channel: "web", Sort: StarredConversationsByName, Limit: 10}
		page, err := st.ListStarredConversationCandidates(ctx, f)
		require.NoError(t, err)
		var values []string
		for _, c := range page.Items {
			values = append(values, c.Position.Value)
		}
		assert.Equal(t, []string{"nia", "user"}, values, "as starUserLabel: the trimmed name, or User for a blank one")
		assert.Equal(t, "Nia", starUserLabel(&store.User{DisplayName: "\u00a0\tNia\u3000"}))
		assert.Equal(t, starLabelUser, starUserLabel(&store.User{DisplayName: "\u00a0\u2003"}))
	})

	t.Run("the read switch chooses one selector, as the other star paths", func(t *testing.T) {
		u := uuid.NewString()
		peer := uuid.NewString()
		// (b) A modern DM row: no thread ID, only the DM's live native
		// conversation; and a modern thread row in the topic's conversation.
		dmKey := "dm:user:" + u + ":user:" + peer
		dmConv := uuid.NewString()
		env.addNativeConversation(t, dmConv, dmKey)
		dmMsg := uuid.NewString()
		env.addMsg(t, dmMsg, "web", "", dmConv, base)
		_, _, err := st.AddMessageStar(ctx, u, StarTarget{MessageID: dmMsg,
			Selector: StarSelector{ConversationKey: dmKey, Channel: "web", ConversationID: dmConv}})
		require.NoError(t, err)
		modern := topic(projA, "modern", at(time.Minute))
		topicConv := uuid.NewString()
		env.setTopicConversation(t, modern, topicConv)
		threadMsg := uuid.NewString()
		env.addMsg(t, threadMsg, "web", "", topicConv, base)
		_, _, err = st.AddMessageStar(ctx, u, StarTarget{MessageID: threadMsg, ProjectID: projA,
			Selector: StarSelector{ConversationKey: modern, Channel: "web", ConversationID: topicConv}})
		require.NoError(t, err)
		// (a) A legacy thread row: the thread ID is the key, the conversation
		// ID another conversation's.
		legacy := topic(projA, "legacy", at(2*time.Minute))
		env.setTopicConversation(t, legacy, uuid.NewString())
		legacyMsg := uuid.NewString()
		env.addMsg(t, legacyMsg, "web", legacy, uuid.NewString(), base)
		add(u, legacyMsg, legacy, projA)
		// (c) A DM whose starred message is in a soft-deleted native
		// conversation that shares the key with a live one.
		oldKey := "dm:user:" + u + ":user:" + uuid.NewString()
		oldConv := uuid.NewString()
		env.addNativeConversation(t, oldConv, oldKey)
		oldMsg := uuid.NewString()
		env.addMsg(t, oldMsg, "web", "", oldConv, base)
		_, _, err = st.AddMessageStar(ctx, u, StarTarget{MessageID: oldMsg,
			Selector: StarSelector{ConversationKey: oldKey, Channel: "web", ConversationID: oldConv}})
		require.NoError(t, err)
		env.deleteConversation(t, oldConv)
		env.addNativeConversation(t, uuid.NewString(), oldKey)

		off := StarredConversationFilter{UserID: u, Channel: "web", Sort: StarredConversationsByActivity}
		on := off
		on.ByConversationID = true
		assert.ElementsMatch(t, []string{legacy}, walkStarredConversations(t, st, off, 10),
			"switch off: the thread ID selects; the modern rows and the deleted conversation's star do not count")
		assert.ElementsMatch(t, []string{dmKey, modern}, walkStarredConversations(t, st, on, 10),
			"switch on: the conversation ID selects; the legacy row is another conversation's, and a deleted conversation never counts")
	})

	t.Run("invalid input", func(t *testing.T) {
		for name, f := range map[string]StarredConversationFilter{
			"no user":  {Sort: StarredConversationsByActivity, Limit: 1},
			"no limit": {UserID: user, Sort: StarredConversationsByActivity},
			"bad sort": {UserID: user, Sort: "custom", Limit: 1},
		} {
			_, err := st.ListStarredConversationCandidates(ctx, f)
			assert.ErrorIs(t, err, store.ErrInvalidInput, name)
		}
	})
}

// TestStarredConversations_LabelSpaceIsTrimSpace pins starLabelSpace to
// exactly the characters strings.TrimSpace strips.
func TestStarredConversations_LabelSpaceIsTrimSpace(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.IsSpace(r) != strings.ContainsRune(starLabelSpace, r) {
			t.Errorf("rune %U: IsSpace %t, in starLabelSpace %t", r, unicode.IsSpace(r), strings.ContainsRune(starLabelSpace, r))
		}
	}
}

// TestStarredConversations_SQLiteActivityKey pins the SQLite activity sort
// value: RFC3339 text padded to nine fractional digits, so a whole second
// sorts before its fractions; another layout is "" (no activity).
func TestStarredConversations_SQLiteActivityKey(t *testing.T) {
	db := hubtestutil.OpenTestMemorySQLite(t, "sqlite")
	for in, want := range map[string]string{
		"2026-10-22T10:00:00Z":              "2026-10-22T10:00:00.000000000Z",
		"2026-10-22T10:00:00.5Z":            "2026-10-22T10:00:00.500000000Z",
		"2026-10-22T10:00:00.123456789Z":    "2026-10-22T10:00:00.123456789Z",
		"2026-10-22 10:00:00 +0000 UTC":     "",
		"2026-10-22T10:00:00.123456789123Z": "",
	} {
		expr := sqliteStarActivityKey("x.v")
		args := make([]any, strings.Count(expr, "x.v"))
		for i := range args {
			args[i] = in
		}
		var got string
		require.NoError(t, db.QueryRow(`SELECT `+strings.ReplaceAll(expr, "x.v", "?"), args...).Scan(&got), in)
		assert.Equal(t, want, got, in)
	}
}

// TestStarredConversations_SQLiteUsesUserIndex checks that the store's
// rail statement, in both orders, with a cursor and a project scope, and
// with the read switch on, reaches the caller's stars through an index on
// user_id rather than a scan of every user's stars, and (switch on) finds a
// DM's conversation through the partial unique index on (surface,
// external_ref) of the real ent schema, never by scanning conversations.
func TestStarredConversations_SQLiteUsesUserIndex(t *testing.T) {
	_, s := testServer(t)
	dbp, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	db := dbp.DB()
	require.NoError(t, NewWebChatStore(db, "sqlite").Init())
	for _, f := range []StarredConversationFilter{
		{UserID: "u", Channel: "web", Sort: StarredConversationsByActivity, Limit: 30},
		{UserID: "u", Channel: "web", Sort: StarredConversationsByName, Limit: 30, ProjectID: "p",
			After: &StarredConversationPosition{Value: "m", Key: "k"}},
		{UserID: "u", Channel: "web", Sort: StarredConversationsByActivity, Limit: 30, ByConversationID: true},
	} {
		query, args := sqliteStarredConversationsQuery(f)
		rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
		require.NoError(t, err)
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
			plan = append(plan, detail)
		}
		require.NoError(t, rows.Err())
		_ = rows.Close()
		joined := strings.Join(plan, "\n")
		assert.Regexp(t, `SEARCH st USING (COVERING )?INDEX \S+ \(user_id=\?`, joined, "sort %s plan:\n%s", f.Sort, joined)
		assert.NotContains(t, joined, "SCAN st", "sort %s plan:\n%s", f.Sort, joined)
		if f.ByConversationID {
			// The DM branch states the partial unique index's predicate, so it
			// is a lookup on that index, never a scan of every conversation.
			assert.NotContains(t, joined, "SCAN c", "switch-on plan:\n%s", joined)
			assert.Regexp(t, `SEARCH c USING (COVERING )?INDEX \S+ \(surface=\? AND external_ref=\?\)`, joined, "switch-on plan:\n%s", joined)
		}
	}
}
