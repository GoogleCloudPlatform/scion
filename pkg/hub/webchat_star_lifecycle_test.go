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
	"database/sql"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// testMessageStarLifecycleContract is the lifecycle part of the star store
// contract both dialects must meet identically: deletion with owner
// collection, promotion of the modern and legacy DM populations (and its
// rollback), conversation/user/project cleanup, and the upkeep scan.
func testMessageStarLifecycleContract(t *testing.T, env starStoreEnv) {
	ctx := context.Background()
	st := env.stars
	base := time.Now().UTC().Add(-time.Hour)
	add := func(user, id string, sel StarSelector) StarRecord {
		t.Helper()
		rec, _, err := st.AddMessageStar(ctx, user, StarTarget{MessageID: id, ProjectID: env.projectID, Selector: sel})
		require.NoError(t, err)
		return rec
	}
	get := func(user, id string) *StarRecord {
		t.Helper()
		rec, err := st.GetMessageStar(ctx, user, id)
		require.NoError(t, err)
		return rec
	}

	t.Run("deletion clears every owner's star in the deletion transaction", func(t *testing.T) {
		u1, u2 := uuid.NewString(), uuid.NewString()
		topic := uuid.NewString()
		env.addTopic(t, topic)
		m, m2 := uuid.NewString(), uuid.NewString()
		env.addMsg(t, m, "web", topic, "", base)
		env.addMsg(t, m2, "web", topic, "", base.Add(time.Second))
		add(u1, m, threadSel(topic))
		add(u2, m, threadSel(topic))
		add(u1, m2, threadSel(topic))

		owners, err := st.MarkMessageDeletedAndClearStars(ctx, m, time.Now())
		require.NoError(t, err)
		want := []string{u1, u2}
		sort.Strings(want)
		assert.Equal(t, want, owners, "the distinct owners, collected in the transaction")
		assert.Nil(t, get(u1, m))
		assert.Nil(t, get(u2, m))
		assert.NotNil(t, get(u1, m2), "another message's star is kept")
		_, _, err = st.AddMessageStar(ctx, u1, StarTarget{MessageID: m, Selector: threadSel(topic)})
		assert.ErrorIs(t, err, store.ErrNotFound, "a deleted message cannot be starred again")
		exts, err := env.wcs.GetMessageExts(ctx, []string{m})
		require.NoError(t, err)
		require.NotNil(t, exts[m])
		assert.NotNil(t, exts[m].DeletedAt, "the soft delete landed with the star cleanup")

		owners, err = st.MarkMessageDeletedAndClearStars(ctx, m, time.Now())
		require.NoError(t, err)
		assert.Empty(t, owners, "a repeated deletion has no owners left")

		// The error-only method is a delegate of the same transaction.
		require.NoError(t, env.wcs.SetMessageDeleted(ctx, m2, time.Now()))
		assert.Nil(t, get(u1, m2))

		// A message that does not exist, or an ID that is not a UUID, still
		// deletes cleanly.
		owners, err = st.MarkMessageDeletedAndClearStars(ctx, uuid.NewString(), time.Now())
		require.NoError(t, err)
		assert.Empty(t, owners)
		_, err = st.MarkMessageDeletedAndClearStars(ctx, "not-a-uuid", time.Now())
		require.NoError(t, err)
	})

	t.Run("promotion moves stars of the modern and legacy populations", func(t *testing.T) {
		u1, u2 := uuid.NewString(), uuid.NewString()
		dmKey := "dm:agent:" + uuid.NewString() + ":user:" + u1
		otherKey := "dm:agent:" + uuid.NewString() + ":user:" + u1
		conv := uuid.NewString()
		modern, legacy, other := uuid.NewString(), uuid.NewString(), uuid.NewString()
		// The modern message is matched only by its conversation ID, the
		// legacy one only by its thread ID: promotion's two arms.
		env.addMsg(t, modern, "web", "", conv, base)
		env.addMsg(t, legacy, "web", dmKey, "", base.Add(time.Second))
		env.addMsg(t, other, "web", otherKey, "", base)
		modernSel := StarSelector{ConversationKey: dmKey, Channel: "web", ConversationID: conv}
		legacySel := StarSelector{ConversationKey: dmKey, Channel: "web", ThreadID: dmKey}
		addDM := func(user, id string, sel StarSelector) StarRecord {
			t.Helper()
			rec, _, err := st.AddMessageStar(ctx, user, StarTarget{MessageID: id, Selector: sel})
			require.NoError(t, err)
			return rec
		}
		before := map[string]StarRecord{
			u1 + modern: addDM(u1, modern, modernSel),
			u1 + legacy: addDM(u1, legacy, legacySel),
			u2 + legacy: addDM(u2, legacy, legacySel),
		}
		otherRec := addDM(u1, other, StarSelector{ConversationKey: otherKey, Channel: "web", ThreadID: otherKey})

		// A promotion that fails at its first step (a thread name already
		// taken) moves no star.
		name := "Promoted " + uuid.NewString()[:8]
		now := time.Now().UTC()
		first := WebChatTopic{ID: uuid.NewString(), ProjectID: env.projectID, Name: name, CreatedBy: u1, CreatedAt: now, LastActivityAt: now}
		blockKey := "dm:agent:" + uuid.NewString() + ":user:" + u1
		block := uuid.NewString()
		env.addMsg(t, block, "web", blockKey, "", base)
		_, err := env.wcs.PromoteDM(ctx, first, PromoteKeys{DMKey: blockKey})
		require.NoError(t, err)
		conflict := WebChatTopic{ID: uuid.NewString(), ProjectID: env.projectID, Name: name, CreatedBy: u1, CreatedAt: now, LastActivityAt: now}
		_, err = env.wcs.PromoteDM(ctx, conflict, PromoteKeys{DMKey: dmKey, DirectConversationID: conv})
		require.Error(t, err, "the name conflict rolls the promotion back")
		for k, rec := range before {
			got := get(rec.UserID, rec.MessageID)
			require.NotNil(t, got, k)
			assert.Equal(t, dmKey, got.ConversationKey, "rolled back: %s", k)
			assert.Equal(t, "", got.ProjectID, "rolled back: %s", k)
		}

		// A promotion that fails AFTER its star update (the DM registry
		// delete, its last step, is refused) rolls the star update back with
		// everything else: stars keep the DM key and no project, and the
		// messages stay in the DM (both arms still list them).
		require.NoError(t, env.wcs.UpsertDM(ctx, WebChatDM{ConversationKey: dmKey, ParticipantID: u1,
			PeerID: strings.Split(dmKey, ":")[2], PeerKind: "agent"}))
		undo := env.failDMDelete(t)
		late := WebChatTopic{ID: uuid.NewString(), ProjectID: env.projectID, Name: "Promoted " + uuid.NewString()[:8],
			CreatedBy: u1, CreatedAt: now, LastActivityAt: now}
		_, err = env.wcs.PromoteDM(ctx, late, PromoteKeys{DMKey: dmKey, DirectConversationID: conv})
		undo()
		require.Error(t, err, "the refused DM delete fails the promotion after its star update")
		for k, rec := range before {
			got := get(rec.UserID, rec.MessageID)
			require.NotNil(t, got, k)
			assert.Equal(t, dmKey, got.ConversationKey, "rolled back after the star update: %s", k)
			assert.Equal(t, "", got.ProjectID, "rolled back after the star update: %s", k)
			assert.True(t, got.StarredAt.Equal(rec.StarredAt), k)
		}
		for name, sel := range map[string]StarSelector{"modern": modernSel, "legacy": legacySel} {
			page, err := st.ListMessageStarCandidates(ctx, StarFilter{UserID: u1, Selector: sel, Limit: 10})
			require.NoError(t, err)
			assert.Len(t, page.Items, 1, "the %s message is still in the DM after the rollback", name)
		}
		lateTopic, err := env.wcs.GetTopic(ctx, late.ID)
		require.NoError(t, err)
		assert.Nil(t, lateTopic, "the rolled-back topic does not exist")

		topic := WebChatTopic{ID: uuid.NewString(), ProjectID: env.projectID, Name: "Promoted " + uuid.NewString()[:8],
			CreatedBy: u1, CreatedAt: now, LastActivityAt: now}
		res, err := env.wcs.PromoteDM(ctx, topic, PromoteKeys{DMKey: dmKey, DirectConversationID: conv})
		require.NoError(t, err)
		assert.Equal(t, 2, res.MessageCount)
		for k, rec := range before {
			got := get(rec.UserID, rec.MessageID)
			require.NotNil(t, got, k)
			assert.Equal(t, topic.ID, got.ConversationKey, "moved: %s", k)
			assert.Equal(t, env.projectID, got.ProjectID, "moved: %s", k)
			assert.True(t, got.StarredAt.Equal(rec.StarredAt), "save time kept: %s", k)
			assert.Equal(t, rec.UserID, got.UserID, k)
		}
		got := get(u1, other)
		require.NotNil(t, got)
		assert.Equal(t, otherRec, *got, "a star of a message that did not move is untouched")

		// The moved stars list under the new thread.
		page, err := st.ListMessageStarCandidates(ctx, StarFilter{UserID: u1, Selector: threadSel(topic.ID), Limit: 10})
		require.NoError(t, err)
		ids := []string{}
		for _, it := range page.Items {
			ids = append(ids, it.MessageID)
		}
		assert.ElementsMatch(t, []string{modern, legacy}, ids)
		page, err = st.ListMessageStarCandidates(ctx, StarFilter{UserID: u1, Selector: legacySel, Limit: 10})
		require.NoError(t, err)
		assert.Empty(t, page.Items, "nothing is left under the DM key")
	})

	t.Run("conversation, user and project cleanup", func(t *testing.T) {
		u1, u2 := uuid.NewString(), uuid.NewString()
		topic, topic2 := uuid.NewString(), uuid.NewString()
		env.addTopic(t, topic)
		env.addTopic(t, topic2)
		dmKey := "dm:user:" + u1 + ":user:" + u2
		a, b, c := uuid.NewString(), uuid.NewString(), uuid.NewString()
		env.addMsg(t, a, "web", topic, "", base)
		env.addMsg(t, b, "web", topic2, "", base)
		env.addMsg(t, c, "web", dmKey, "", base)
		add(u1, a, threadSel(topic))
		add(u2, a, threadSel(topic))
		add(u1, b, threadSel(topic2))
		_, _, err := st.AddMessageStar(ctx, u1, StarTarget{MessageID: c, Selector: threadSel(dmKey)})
		require.NoError(t, err)

		n, err := st.DeleteMessageStarsForConversation(ctx, topic)
		require.NoError(t, err)
		assert.EqualValues(t, 2, n)
		assert.Nil(t, get(u1, a))
		assert.NotNil(t, get(u1, b))

		n, err = st.DeleteMessageStarsForUser(ctx, u1)
		require.NoError(t, err)
		assert.EqualValues(t, 2, n, "the user's thread and DM stars")
		assert.Nil(t, get(u1, b))
		assert.Nil(t, get(u1, c))

		add(u2, b, threadSel(topic2))
		n, err = st.DeleteMessageStarsForProject(ctx, env.projectID)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, n, int64(1))
		assert.Nil(t, get(u2, b))

		for _, f := range []func(context.Context, string) (int64, error){
			st.DeleteMessageStarsForConversation, st.DeleteMessageStarsForUser, st.DeleteMessageStarsForProject,
		} {
			_, err := f(ctx, "")
			assert.ErrorIs(t, err, store.ErrInvalidInput)
		}
	})

	t.Run("upkeep repairs moved stars and purges only permanently gone targets", func(t *testing.T) {
		live, gone := uuid.NewString(), uuid.NewString()
		env.addUser(t, live)
		liveTopic, deletedTopic := uuid.NewString(), uuid.NewString()
		env.addTopic(t, liveTopic)
		env.addTopic(t, deletedTopic)
		goneProject := uuid.NewString()
		agentDM := "dm:agent:" + uuid.NewString() + ":user:" + live
		humanDM := "dm:user:" + live + ":user:" + uuid.NewString()
		staleDM := "dm:agent:" + uuid.NewString() + ":user:" + live

		msg := func(thread string) string {
			id := uuid.NewString()
			env.addMsg(t, id, "web", thread, "", base)
			return id
		}
		mLive, mLive2, mDeleted, mMoved := msg(liveTopic), msg(liveTopic), msg(liveTopic), msg(liveTopic)
		mInDeletedTopic, mAgentDM, mHumanDM := msg(deletedTopic), msg(agentDM), msg(humanDM)
		require.NoError(t, env.wcs.SetMessageDeleted(ctx, mDeleted, time.Now()))
		env.deleteTopic(t, deletedTopic)
		missingMsg := uuid.NewString()

		at := time.Now().UTC().Add(-time.Minute).Truncate(starTimePrecision)
		type row struct{ user, msg, key, project string }
		keep := []row{
			{live, mLive, liveTopic, env.projectID},
			{live, mHumanDM, humanDM, ""},
		}
		purge := []row{
			{gone, mLive, liveTopic, env.projectID},      // owner deleted
			{live, mDeleted, liveTopic, env.projectID},   // message soft-deleted
			{live, missingMsg, liveTopic, env.projectID}, // message gone
			{live, mInDeletedTopic, deletedTopic, env.projectID},
			{live, mAgentDM, agentDM, goneProject}, // the agent's project is gone
		}
		repair := []row{
			{live, mMoved, staleDM, ""},            // moved by a promotion an older binary ran
			{live, mLive2, liveTopic, goneProject}, // stale project of a live topic
		}
		for _, set := range [][]row{keep, purge, repair} {
			for _, r := range set {
				env.insertStar(t, r.user, r.msg, r.key, r.project, at)
			}
		}

		_, err := st.PurgeMessageStarCandidates(ctx, StarCleanupPosition{}, 0)
		assert.ErrorIs(t, err, store.ErrInvalidInput)

		// Small batches walk the whole table once and wrap.
		var pos StarCleanupPosition
		examined := 0
		var purged, repaired int64
		for i := 0; ; i++ {
			require.Less(t, i, 1000, "the scan must reach the end")
			res, err := st.PurgeMessageStarCandidates(ctx, pos, 2)
			require.NoError(t, err)
			assert.LessOrEqual(t, res.Examined, 2)
			examined += res.Examined
			purged += res.Purged
			repaired += res.Repaired
			if res.Next == (StarCleanupPosition{}) {
				break
			}
			require.True(t, res.Next.UserID > pos.UserID || (res.Next.UserID == pos.UserID && res.Next.MessageID > pos.MessageID),
				"the position advances")
			pos = res.Next
		}
		assert.GreaterOrEqual(t, examined, len(keep)+len(purge)+len(repair))
		assert.GreaterOrEqual(t, purged, int64(len(purge)))
		assert.GreaterOrEqual(t, repaired, int64(len(repair)))

		for _, r := range keep {
			got := get(r.user, r.msg)
			require.NotNil(t, got, "kept: %+v", r)
			assert.Equal(t, r.key, got.ConversationKey)
			assert.Equal(t, r.project, got.ProjectID)
		}
		for _, r := range purge {
			assert.Nil(t, get(r.user, r.msg), "purged: %+v", r)
		}
		for _, r := range repair {
			got := get(r.user, r.msg)
			require.NotNil(t, got, "repaired, not purged: %+v", r)
			assert.Equal(t, liveTopic, got.ConversationKey, "follows its message: %+v", r)
			assert.Equal(t, env.projectID, got.ProjectID, "the topic's project: %+v", r)
			assert.True(t, got.StarredAt.Equal(at), "save time kept: %+v", r)
		}

		// A second full pass changes nothing.
		res, err := st.PurgeMessageStarCandidates(ctx, StarCleanupPosition{}, 10000)
		require.NoError(t, err)
		assert.Zero(t, res.Purged)
		assert.Zero(t, res.Repaired)
		assert.Equal(t, StarCleanupPosition{}, res.Next, "fewer rows than the limit: the scan wrapped")
	})

	t.Run("reconciliation sweeps until nothing is left, between another replica's batches", func(t *testing.T) {
		owner := uuid.NewString()
		env.addUser(t, owner)
		topic := uuid.NewString()
		env.addTopic(t, topic)
		at := time.Now().UTC().Add(-time.Minute).Truncate(starTimePrecision)
		moved, kept := uuid.NewString(), uuid.NewString()
		env.addMsg(t, moved, "web", topic, "", base)
		env.addMsg(t, kept, "web", topic, "", base)
		env.insertStar(t, owner, moved, "dm:agent:"+uuid.NewString()+":user:"+owner, "", at)
		env.insertStar(t, owner, kept, topic, env.projectID, at)
		gone := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
		for _, m := range gone {
			env.insertStar(t, owner, m, topic, env.projectID, at) // message missing
		}
		lim := messageStarReconcileLimits{Batch: 2, BatchTimeout: 10 * time.Second, MaxSweeps: 3}

		// Another replica has run one batch of its own pass when this run
		// starts, and finishes its pass after the run (one after another, not
		// concurrently).
		other, err := st.PurgeMessageStarCandidates(ctx, StarCleanupPosition{}, 1)
		require.NoError(t, err)

		var sweeps []messageStarSweep
		report, err := reconcileMessageStars(ctx, st, lim, func(n int, sw messageStarSweep) {
			assert.Equal(t, len(sweeps)+1, n)
			sweeps = append(sweeps, sw)
		})
		require.NoError(t, err)
		assert.True(t, report.Exhausted)
		require.Len(t, sweeps, report.Sweeps)
		assert.Equal(t, 2, report.Sweeps, "a sweep fixes everything it examines; the second finds nothing")
		assert.False(t, sweeps[len(sweeps)-1].changed())
		assert.GreaterOrEqual(t, report.Repaired+other.Repaired, int64(1))
		assert.GreaterOrEqual(t, report.Purged+other.Purged, int64(len(gone)))
		assert.GreaterOrEqual(t, sweeps[0].Examined, 2, "the kept and the repaired star")

		// The other replica finishes its pass afterwards: nothing is left for it.
		pos := other.Next
		for i := 0; pos != (StarCleanupPosition{}); i++ {
			require.Less(t, i, 1000)
			res, err := st.PurgeMessageStarCandidates(ctx, pos, 1)
			require.NoError(t, err)
			assert.Zero(t, res.Repaired+res.Purged, "nothing left after the run")
			pos = res.Next
		}

		got := get(owner, moved)
		require.NotNil(t, got)
		assert.Equal(t, topic, got.ConversationKey)
		assert.Equal(t, env.projectID, got.ProjectID)
		assert.True(t, got.StarredAt.Equal(at), "save time kept")
		require.NotNil(t, get(owner, kept))
		for _, m := range gone {
			assert.Nil(t, get(owner, m))
		}

		// A second run changes nothing and stops after one sweep.
		again, err := reconcileMessageStars(ctx, st, lim, nil)
		require.NoError(t, err)
		assert.Equal(t, messageStarReconcileReport{Sweeps: 1, Examined: again.Examined, Exhausted: true}, again)
	})
}

// TestMessageStarStore_SQLite_SingleConnection runs deletion and
// promotion on a SQLite pool pinned to one connection (the production
// SQLite pool), where any ambient pool use inside a transaction would
// deadlock.
func TestMessageStarStore_SQLite_SingleConnection(t *testing.T) {
	wcs, db := newPromoteTestStore(t)
	defer db.Close() //nolint:errcheck
	require.Equal(t, 1, db.Stats().MaxOpenConnections)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stars := messageStarStoreFrom(wcs)
	require.NotNil(t, stars)
	seedDMData(t, db, wcs)
	dmKey := "dm:agent:agent-uuid-1:user:user-uuid-1"
	sel := StarSelector{ConversationKey: dmKey, Channel: "web", ThreadID: dmKey}

	rec, _, err := stars.AddMessageStar(ctx, "user-uuid-1", StarTarget{MessageID: "msg-1", Selector: sel})
	require.NoError(t, err)
	_, _, err = stars.AddMessageStar(ctx, "user-uuid-1", StarTarget{MessageID: "msg-3", Selector: sel})
	require.NoError(t, err)

	owners, err := stars.MarkMessageDeletedAndClearStars(ctx, "msg-3", time.Now())
	require.NoError(t, err)
	assert.Equal(t, []string{"user-uuid-1"}, owners)

	// A promotion refused at its last step (after the star update) rolls the
	// star update back, on the single connection too.
	_, err = db.Exec(`CREATE TRIGGER star_promote_fails BEFORE DELETE ON webchat_dm BEGIN SELECT RAISE(ABORT, 'dm delete refused'); END`)
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = wcs.PromoteDM(ctx, WebChatTopic{ID: "topic-stars-late", ProjectID: "proj-1", Name: "Late", CreatedBy: "user-uuid-1",
		CreatedAt: now, LastActivityAt: now}, PromoteKeys{DMKey: dmKey})
	require.Error(t, err)
	_, err = db.Exec(`DROP TRIGGER star_promote_fails`)
	require.NoError(t, err)
	kept, err := stars.GetMessageStar(ctx, "user-uuid-1", "msg-1")
	require.NoError(t, err)
	require.NotNil(t, kept)
	assert.Equal(t, dmKey, kept.ConversationKey, "the star update rolled back")
	assert.Equal(t, "", kept.ProjectID)
	var threadID string
	require.NoError(t, db.QueryRow(`SELECT thread_id FROM messages WHERE id = 'msg-1'`).Scan(&threadID))
	assert.Equal(t, dmKey, threadID, "the message re-key rolled back")

	_, err = wcs.PromoteDM(ctx, WebChatTopic{ID: "topic-stars-1", ProjectID: "proj-1", Name: "Stars", CreatedBy: "user-uuid-1",
		CreatedAt: now, LastActivityAt: now}, PromoteKeys{DMKey: dmKey})
	require.NoError(t, err)
	got, err := stars.GetMessageStar(ctx, "user-uuid-1", "msg-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "topic-stars-1", got.ConversationKey)
	assert.Equal(t, "proj-1", got.ProjectID)
	assert.True(t, got.StarredAt.Equal(rec.StarredAt))
	gone, err := stars.GetMessageStar(ctx, "user-uuid-1", "msg-3")
	require.NoError(t, err)
	assert.Nil(t, gone)

	n, err := stars.DeleteMessageStarsForConversation(ctx, "topic-stars-1")
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
}

// TestMessageStarStore_SQLite_DeleteDMClearsStars: removing a DM removes
// the stars listed under it, as it removes its scheduled messages.
func TestMessageStarStore_SQLite_DeleteDMClearsStars(t *testing.T) {
	wcs, db := newPromoteTestStore(t)
	defer db.Close() //nolint:errcheck
	ctx := context.Background()
	stars := messageStarStoreFrom(wcs)
	seedDMData(t, db, wcs)
	dmKey := "dm:agent:agent-uuid-1:user:user-uuid-1"
	_, _, err := stars.AddMessageStar(ctx, "user-uuid-1", StarTarget{MessageID: "msg-2",
		Selector: StarSelector{ConversationKey: dmKey, Channel: "web", ThreadID: dmKey}})
	require.NoError(t, err)
	require.NoError(t, wcs.DeleteDM(ctx, dmKey))
	got, err := stars.GetMessageStar(ctx, "user-uuid-1", "msg-2")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestMessageStarStore_SQLite_DeleteRollbackKeepsStars: when the deletion
// transaction fails after its first statement, nothing of it lands: the
// message is not deleted and its stars stay.
func TestMessageStarStore_SQLite_DeleteRollbackKeepsStars(t *testing.T) {
	wcs, db := newPromoteTestStore(t)
	defer db.Close() //nolint:errcheck
	ctx := context.Background()
	stars := messageStarStoreFrom(wcs)
	seedDMData(t, db, wcs)
	dmKey := "dm:agent:agent-uuid-1:user:user-uuid-1"
	_, _, err := stars.AddMessageStar(ctx, "user-uuid-1", StarTarget{MessageID: "msg-2",
		Selector: StarSelector{ConversationKey: dmKey, Channel: "web", ThreadID: dmKey}})
	require.NoError(t, err)
	// Make the star cleanup statement fail inside the transaction.
	_, err = db.Exec(`CREATE TRIGGER star_delete_fails BEFORE DELETE ON webchat_message_star
BEGIN SELECT RAISE(ABORT, 'star delete refused'); END`)
	require.NoError(t, err)
	_, err = stars.MarkMessageDeletedAndClearStars(ctx, "msg-2", time.Now())
	require.Error(t, err)
	_, err = db.Exec(`DROP TRIGGER star_delete_fails`)
	require.NoError(t, err)

	got, err := stars.GetMessageStar(ctx, "user-uuid-1", "msg-2")
	require.NoError(t, err)
	assert.NotNil(t, got, "the star stays")
	var deleted sql.NullString
	err = db.QueryRow(`SELECT deleted_at FROM webchat_message_ext WHERE message_id = 'msg-2'`).Scan(&deleted)
	if err != sql.ErrNoRows {
		require.NoError(t, err)
		assert.False(t, deleted.Valid, "the soft delete rolled back with it")
	}
}
