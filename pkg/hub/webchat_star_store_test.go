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
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/internal/hubtestutil"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// starStoreEnv is what the dialect-independent star store contract needs
// from a dialect: the store, and ways to create messages and topics in the
// same database.
type starStoreEnv struct {
	wcs   WebChatStore
	stars MessageStarStore
	// addMsg stores a message; conversationID may be "".
	addMsg func(t *testing.T, id, channel, threadID, conversationID string, created time.Time)
	// moveMsg changes a message's thread ID (as a promotion would).
	moveMsg func(t *testing.T, id, threadID string)
	// addTopic creates a live topic.
	addTopic func(t *testing.T, id string)
	// deleteTopic soft-deletes a topic.
	deleteTopic func(t *testing.T, id string)
	// setStarredAt overwrites a star's save time (to make ties).
	setStarredAt func(t *testing.T, userID, messageID string, at time.Time)
	// reinit runs Init again on a new store over the same database.
	reinit func(t *testing.T)
	// migrationRows counts the star migration's webchat_migrations rows.
	migrationRows func(t *testing.T) int

	// The lifecycle contract (webchat_star_lifecycle_test.go) also needs:
	// projectID is an existing project; addTopic creates topics in it.
	projectID string
	// addUser creates a user row.
	addUser func(t *testing.T, id string)
	// insertStar stores an association directly, without the add guard
	// (rows an older binary or a lost race could leave behind).
	insertStar func(t *testing.T, userID, messageID, key, projectID string, at time.Time)
	// failDMDelete installs a trigger that refuses every DELETE on
	// webchat_dm (a promotion's last step, after its star update), and
	// returns the function that removes it.
	failDMDelete func(t *testing.T) func()

	// The Starred rail contract (webchat_star_conversations_test.go) also
	// needs:
	// setTopic sets a topic's project, name and last activity (nil: none).
	setTopic func(t *testing.T, id, projectID, name string, activity *time.Time)
	// addDMRow stores a participant's webchat_dm row.
	addDMRow func(t *testing.T, participantID, key, peerKind, peerID string, activity time.Time)
	// addAgent creates an agent record.
	addAgent func(t *testing.T, id, name, slug string)
	// addNamedUser creates a user with a display name.
	addNamedUser func(t *testing.T, id, displayName string)
	// addNativeConversation creates a native conversation row whose
	// external_ref is ref (a DM's conversation under the read switch).
	addNativeConversation func(t *testing.T, id, ref string)
	// setTopicConversation sets a topic's conversation_id.
	setTopicConversation func(t *testing.T, topicID, conversationID string)
	// deleteConversation soft-deletes a conversation row.
	deleteConversation func(t *testing.T, id string)
}

func threadSel(key string) StarSelector {
	return StarSelector{ConversationKey: key, Channel: "web", ThreadID: key}
}

// testMessageStarStoreContract is the contract both dialects must meet
// identically.
func testMessageStarStoreContract(t *testing.T, env starStoreEnv) {
	ctx := context.Background()
	st := env.stars
	userA, userB := uuid.NewString(), uuid.NewString()
	topic, topic2 := uuid.NewString(), uuid.NewString()
	env.addTopic(t, topic)
	env.addTopic(t, topic2)
	conv := uuid.NewString()
	base := time.Now().UTC().Add(-time.Hour)

	m1 := uuid.NewString()
	env.addMsg(t, m1, "web", topic, conv, base)
	add := func(user, id string, sel StarSelector) (StarRecord, bool, error) {
		return st.AddMessageStar(ctx, user, StarTarget{MessageID: id, ProjectID: "proj-1", Selector: sel})
	}

	// Add, then an identical add keeps the association and its time.
	rec, changed, err := add(userA, m1, threadSel(topic))
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, userA, rec.UserID)
	assert.Equal(t, m1, rec.MessageID)
	assert.Equal(t, topic, rec.ConversationKey)
	assert.Equal(t, "proj-1", rec.ProjectID)
	assert.True(t, rec.StarredAt.Equal(rec.StarredAt.Truncate(starTimePrecision)), "stored precision")
	again, changed, err := add(userA, m1, threadSel(topic))
	require.NoError(t, err)
	assert.False(t, changed)
	assert.True(t, again.StarredAt.Equal(rec.StarredAt))
	got, err := st.GetMessageStar(ctx, userA, m1)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.StarredAt.Equal(rec.StarredAt))
	got, err = st.GetMessageStar(ctx, userB, m1)
	require.NoError(t, err)
	assert.Nil(t, got, "another user's association is not visible")

	// The conversation-ID selector matches the same message.
	_, changed, err = add(userB, m1, StarSelector{ConversationKey: topic, Channel: "web", ConversationID: conv})
	require.NoError(t, err)
	assert.True(t, changed)

	// Guard: wrong thread, wrong conversation, other channel, missing,
	// deleted and deleted topic are not found, and nothing is inserted.
	mOther := uuid.NewString()
	env.addMsg(t, mOther, "telegram", topic, conv, base)
	mDel := uuid.NewString()
	env.addMsg(t, mDel, "web", topic, conv, base)
	require.NoError(t, env.wcs.SetMessageDeleted(ctx, mDel, time.Now()))
	for name, c := range map[string]struct {
		id  string
		sel StarSelector
	}{
		"wrong thread":       {m1, threadSel(topic2)},
		"wrong conversation": {m1, StarSelector{ConversationKey: topic, Channel: "web", ConversationID: uuid.NewString()}},
		"other channel":      {mOther, threadSel(topic)},
		"missing":            {uuid.NewString(), threadSel(topic)},
		"deleted":            {mDel, threadSel(topic)},
		"missing topic":      {m1, StarSelector{ConversationKey: uuid.NewString(), Channel: "web", ThreadID: topic}},
	} {
		_, _, err := add(userB, c.id, c.sel)
		assert.ErrorIs(t, err, store.ErrNotFound, name)
		got, gerr := st.GetMessageStar(ctx, userB, c.id)
		require.NoError(t, gerr)
		if c.id != m1 {
			assert.Nil(t, got, name)
		}
	}
	// An existing association does not make a deleted target valid.
	m2 := uuid.NewString()
	env.addMsg(t, m2, "web", topic, conv, base.Add(time.Minute))
	_, _, err = add(userA, m2, threadSel(topic))
	require.NoError(t, err)
	require.NoError(t, env.wcs.SetMessageDeleted(ctx, m2, time.Now()))
	_, _, err = add(userA, m2, threadSel(topic))
	assert.ErrorIs(t, err, store.ErrNotFound)

	// State: live, owned, with routing fields.
	refs, err := st.ListStarredMessageIDs(ctx, userA, []string{m1, m2, mOther, uuid.NewString()})
	require.NoError(t, err)
	require.Len(t, refs, 1, "deleted and unstarred are omitted")
	assert.Equal(t, StarredMessageRef{MessageID: m1, ConversationKey: topic, ProjectID: "proj-1",
		Channel: "web", ThreadID: topic, ConversationID: conv}, refs[0])
	assert.True(t, refs[0].matches(threadSel(topic)))
	assert.True(t, refs[0].matches(StarSelector{ConversationKey: topic, Channel: "web", ConversationID: conv}))
	assert.False(t, refs[0].matches(threadSel(topic2)))
	refs, err = st.ListStarredMessageIDs(ctx, uuid.NewString(), []string{m1})
	require.NoError(t, err)
	assert.Empty(t, refs)

	// List: keyset over ties, newest first, deleted and moved omitted.
	user := uuid.NewString()
	var ids []string
	for i := 0; i < 6; i++ {
		id := uuid.NewString()
		env.addMsg(t, id, "web", topic, conv, base.Add(time.Duration(i)*time.Second))
		_, _, err := add(user, id, threadSel(topic))
		require.NoError(t, err)
		ids = append(ids, id)
	}
	tie := time.Now().UTC().Add(time.Hour).Truncate(starTimePrecision)
	for _, id := range ids[:3] {
		env.setStarredAt(t, user, id, tie)
	}
	for i, id := range ids[3:] {
		env.setStarredAt(t, user, id, tie.Add(-time.Duration(i+1)*time.Second))
	}
	tied := append([]string(nil), ids[:3]...)
	sort.Sort(sort.Reverse(sort.StringSlice(tied)))
	want := append(tied, ids[3], ids[4], ids[5])

	var walked []string
	var after *StarPosition
	for i := 0; i < 10; i++ {
		page, err := st.ListMessageStarCandidates(ctx, StarFilter{UserID: user, Selector: threadSel(topic), After: after, Limit: 2})
		require.NoError(t, err)
		for _, it := range page.Items {
			walked = append(walked, it.MessageID)
		}
		if page.Next == nil {
			break
		}
		after = page.Next
	}
	assert.Equal(t, want, walked)

	require.NoError(t, env.wcs.SetMessageDeleted(ctx, ids[4], time.Now()))
	env.moveMsg(t, ids[5], topic2)
	page, err := st.ListMessageStarCandidates(ctx, StarFilter{UserID: user, Selector: threadSel(topic), Limit: 100})
	require.NoError(t, err)
	assert.Nil(t, page.Next)
	assert.Equal(t, append(append([]string(nil), tied...), ids[3]), func() []string {
		out := []string{}
		for _, it := range page.Items {
			out = append(out, it.MessageID)
		}
		return out
	}(), "deleted and moved messages are omitted")
	page, err = st.ListMessageStarCandidates(ctx, StarFilter{UserID: userB, Selector: threadSel(topic), Limit: 100})
	require.NoError(t, err)
	assert.Equal(t, 1, len(page.Items))

	// A deleted topic hides nothing by itself in the store (the handler's
	// access check does) but refuses new stars.
	m3 := uuid.NewString()
	env.addMsg(t, m3, "web", topic2, "", base)
	env.deleteTopic(t, topic2)
	_, _, err = add(userA, m3, threadSel(topic2))
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Remove reports whether a row existed.
	ok, err := st.RemoveMessageStar(ctx, userA, m1)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = st.RemoveMessageStar(ctx, userA, m1)
	require.NoError(t, err)
	assert.False(t, ok)
	got, err = st.GetMessageStar(ctx, userB, m1)
	require.NoError(t, err)
	assert.NotNil(t, got, "removing one user's star keeps the other's")

	// Re-adding after removal creates a fresh save time.
	time.Sleep(2 * time.Millisecond)
	re, changed, err := add(userA, m1, threadSel(topic))
	require.NoError(t, err)
	assert.True(t, changed)
	assert.True(t, re.StarredAt.After(rec.StarredAt))

	// Invalid input.
	_, _, err = st.AddMessageStar(ctx, "", StarTarget{MessageID: m1, Selector: threadSel(topic)})
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = st.ListMessageStarCandidates(ctx, StarFilter{UserID: userA, Selector: threadSel(topic), Limit: 0})
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	// Init is repeatable; the migration is recorded once; rows survive.
	env.reinit(t)
	env.reinit(t)
	assert.Equal(t, 1, env.migrationRows(t))
	got, err = st.GetMessageStar(ctx, userA, m1)
	require.NoError(t, err)
	assert.NotNil(t, got)
}

func TestMessageStarStore_SQLite(t *testing.T) {
	s, err := hubtestutil.OpenMigratedTestStore(t)
	require.NoError(t, err)
	dbp, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	db := dbp.DB()
	wcs := NewWebChatStore(db, "sqlite")
	require.NoError(t, wcs.Init())
	projectID := uuid.NewString()
	require.NoError(t, s.CreateProject(context.Background(), &store.Project{ID: projectID, Name: "Stars", Slug: "stars-" + projectID[:8]}))
	env := starStoreEnv{
		wcs:   wcs,
		stars: messageStarStoreFrom(wcs),
		addMsg: func(t *testing.T, id, channel, threadID, conversationID string, created time.Time) {
			require.NoError(t, s.CreateMessage(context.Background(), &store.Message{
				ID: id, ProjectID: projectID, Sender: "user:x@test.com", Recipient: "chat", Msg: "m " + id,
				Type: "chat", Channel: channel, ThreadID: threadID, ConversationID: conversationID, CreatedAt: created,
			}))
		},
		moveMsg: func(t *testing.T, id, threadID string) {
			_, err := db.Exec(`UPDATE messages SET thread_id = ? WHERE id = ?`, threadID, id)
			require.NoError(t, err)
		},
		addTopic: func(t *testing.T, id string) {
			_, err := db.Exec(`INSERT INTO webchat_topic (id, project_id, name, created_by, created_at) VALUES (?, ?, ?, ?, ?)`,
				id, projectID, "t-"+id[:8], "u", time.Now().UTC().Format(time.RFC3339Nano))
			require.NoError(t, err)
		},
		deleteTopic: func(t *testing.T, id string) {
			_, err := db.Exec(`UPDATE webchat_topic SET deleted_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339Nano), id)
			require.NoError(t, err)
		},
		setStarredAt: func(t *testing.T, userID, messageID string, at time.Time) {
			_, err := db.Exec(`UPDATE webchat_message_star SET starred_at = ? WHERE user_id = ? AND message_id = ?`,
				sqliteStarTime(at), userID, messageID)
			require.NoError(t, err)
		},
		reinit: func(t *testing.T) { require.NoError(t, NewWebChatStore(db, "sqlite").Init()) },
		migrationRows: func(t *testing.T) int {
			var n int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM webchat_migrations WHERE name = ?`, messageStarTableMigration).Scan(&n))
			return n
		},
		projectID: projectID,
		addUser: func(t *testing.T, id string) {
			require.NoError(t, s.CreateUser(context.Background(), &store.User{
				ID: id, Email: id + "@stars.test", DisplayName: "u", Role: "member", Status: store.UserStatusActive,
			}))
		},
		insertStar: func(t *testing.T, userID, messageID, key, projectID string, at time.Time) {
			_, err := db.Exec(`INSERT INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at) VALUES (?, ?, ?, ?, ?)`,
				userID, messageID, key, nullableString(projectID), sqliteStarTime(at))
			require.NoError(t, err)
		},
		failDMDelete: func(t *testing.T) func() {
			_, err := db.Exec(`CREATE TRIGGER star_promote_fails BEFORE DELETE ON webchat_dm BEGIN SELECT RAISE(ABORT, 'dm delete refused'); END`)
			require.NoError(t, err)
			return func() {
				_, err := db.Exec(`DROP TRIGGER star_promote_fails`)
				require.NoError(t, err)
			}
		},
		setTopic: func(t *testing.T, id, projectID, name string, activity *time.Time) {
			var at any
			if activity != nil {
				at = activity.UTC().Format(time.RFC3339Nano)
			}
			_, err := db.Exec(`UPDATE webchat_topic SET project_id = ?, name = ?, last_activity_at = ? WHERE id = ?`, projectID, name, at, id)
			require.NoError(t, err)
		},
		addDMRow: func(t *testing.T, participantID, key, peerKind, peerID string, activity time.Time) {
			_, err := db.Exec(`INSERT INTO webchat_dm (conversation_key, participant_id, peer_id, peer_kind, last_activity_at) VALUES (?, ?, ?, ?, ?)`,
				key, participantID, peerID, peerKind, activity.UTC().Format(time.RFC3339Nano))
			require.NoError(t, err)
		},
		addAgent: func(t *testing.T, id, name, slug string) {
			require.NoError(t, s.CreateAgent(context.Background(), &store.Agent{ID: id, Slug: slug, Name: name, ProjectID: projectID}))
		},
		addNamedUser: func(t *testing.T, id, displayName string) {
			require.NoError(t, s.CreateUser(context.Background(), &store.User{
				ID: id, Email: id + "@stars.test", DisplayName: displayName, Role: "member", Status: store.UserStatusActive,
			}))
		},
		addNativeConversation: func(t *testing.T, id, ref string) {
			at := time.Now().UTC()
			require.NoError(t, s.CreateConversation(context.Background(), &store.Conversation{
				ID: id, Kind: "direct", Surface: "native", ExternalRef: ref, DriftState: "active", LastActivityAt: at, CreatedAt: at,
			}))
		},
		setTopicConversation: func(t *testing.T, topicID, conversationID string) {
			_, err := db.Exec(`UPDATE webchat_topic SET conversation_id = ? WHERE id = ?`, conversationID, topicID)
			require.NoError(t, err)
		},
		deleteConversation: func(t *testing.T, id string) {
			require.NoError(t, s.DeleteConversation(context.Background(), id))
		},
	}
	require.NotNil(t, env.stars)
	testMessageStarStoreContract(t, env)
	testMessageStarLifecycleContract(t, env)
	testStarredHistoryContract(t, env)
	testStarredConversationsContract(t, env)
}

// TestMessageStarStore_SQLite_FixedWidthTime pins the SQLite TEXT form:
// fixed width with nine fractional digits, so lexical order is
// chronological even for whole seconds.
func TestMessageStarStore_SQLite_FixedWidthTime(t *testing.T) {
	whole := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	frac := whole.Add(1500 * time.Microsecond)
	assert.Equal(t, "2026-10-22T10:00:00.000000000Z", sqliteStarTime(whole))
	assert.Equal(t, "2026-10-22T10:00:00.001500000Z", sqliteStarTime(frac))
	assert.Less(t, sqliteStarTime(whole), sqliteStarTime(frac))
	assert.Equal(t, sqliteStarTime(whole.Add(999)), sqliteStarTime(whole), "truncated to microseconds")
	back, err := parseSQLiteStarTime(sqliteStarTime(frac))
	require.NoError(t, err)
	assert.True(t, back.Equal(frac))
}

// TestMessageStarStore_Postgres runs the same contract against Postgres
// when SCION_TEST_POSTGRES_DSN is set, in a throwaway schema.
func TestMessageStarStore_Postgres(t *testing.T) {
	dsn := hubtestutil.RequirePostgresDSN(t)
	ctx := context.Background()
	schema := fmt.Sprintf("star_store_test_%d", time.Now().UnixNano())
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec("DROP SCHEMA " + schema + " CASCADE") })
	// The Ent-managed messages columns the chat store reads.
	_, err = db.ExecContext(ctx, `CREATE TABLE messages (
    id uuid PRIMARY KEY, project_id uuid, created timestamptz NOT NULL, channel text, thread_id text,
    conversation_id uuid, msg text, sender text, sender_id text, recipient text, recipient_id text)`)
	require.NoError(t, err)
	// The Ent-managed tables the lifecycle contract reaches: users and
	// projects (existence only) and conversations (a promotion creates one).
	// users and agents also carry the names the Starred rail sorts DMs by.
	_, err = db.ExecContext(ctx, `CREATE TABLE users (id uuid PRIMARY KEY, display_name text);
CREATE TABLE agents (id uuid PRIMARY KEY, name text, slug text);
CREATE TABLE projects (id uuid PRIMARY KEY);
CREATE TABLE conversations (
    id uuid PRIMARY KEY, project_id uuid, kind text, surface text, external_ref text, parent_ref text,
    display_name text, default_agent_id uuid, drift_state text, last_activity_at timestamptz, created_at timestamptz,
    deleted_at timestamptz)`)
	require.NoError(t, err)
	projectID := uuid.NewString()
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id) VALUES ($1)`, projectID)
	require.NoError(t, err)
	wcs := NewWebChatStore(db, "postgres")
	require.NoError(t, wcs.Init())
	env := starStoreEnv{
		wcs:   wcs,
		stars: messageStarStoreFrom(wcs),
		addMsg: func(t *testing.T, id, channel, threadID, conversationID string, created time.Time) {
			var conv any
			if conversationID != "" {
				conv = conversationID
			}
			_, err := db.Exec(`INSERT INTO messages (id, created, channel, thread_id, conversation_id, msg, sender)
VALUES ($1, $2, $3, $4, $5, $6, 'user:x@test.com')`, id, created, channel, threadID, conv, "m "+id)
			require.NoError(t, err)
		},
		moveMsg: func(t *testing.T, id, threadID string) {
			_, err := db.Exec(`UPDATE messages SET thread_id = $1 WHERE id = $2`, threadID, id)
			require.NoError(t, err)
		},
		addTopic: func(t *testing.T, id string) {
			_, err := db.Exec(`INSERT INTO webchat_topic (id, project_id, name, created_by, created_at) VALUES ($1, $2, $3, $4, NOW())`,
				id, projectID, "t-"+id[:8], "u")
			require.NoError(t, err)
		},
		deleteTopic: func(t *testing.T, id string) {
			_, err := db.Exec(`UPDATE webchat_topic SET deleted_at = NOW() WHERE id = $1`, id)
			require.NoError(t, err)
		},
		setStarredAt: func(t *testing.T, userID, messageID string, at time.Time) {
			_, err := db.Exec(`UPDATE webchat_message_star SET starred_at = $1 WHERE user_id = $2 AND message_id = $3`,
				at, userID, messageID)
			require.NoError(t, err)
		},
		reinit: func(t *testing.T) { require.NoError(t, NewWebChatStore(db, "postgres").Init()) },
		migrationRows: func(t *testing.T) int {
			var n int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM webchat_migrations WHERE name = $1`, messageStarTableMigration).Scan(&n))
			return n
		},
		projectID: projectID,
		addUser: func(t *testing.T, id string) {
			_, err := db.Exec(`INSERT INTO users (id) VALUES ($1)`, id)
			require.NoError(t, err)
		},
		insertStar: func(t *testing.T, userID, messageID, key, projectID string, at time.Time) {
			_, err := db.Exec(`INSERT INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at) VALUES ($1, $2, $3, $4, $5)`,
				userID, messageID, key, nullableString(projectID), at.UTC().Truncate(starTimePrecision))
			require.NoError(t, err)
		},
		failDMDelete: func(t *testing.T) func() {
			_, err := db.Exec(`CREATE FUNCTION star_promote_fails() RETURNS trigger AS $f$ BEGIN RAISE EXCEPTION 'dm delete refused'; END $f$ LANGUAGE plpgsql;
CREATE TRIGGER star_promote_fails BEFORE DELETE ON webchat_dm FOR EACH ROW EXECUTE FUNCTION star_promote_fails()`)
			require.NoError(t, err)
			return func() {
				_, err := db.Exec(`DROP TRIGGER star_promote_fails ON webchat_dm; DROP FUNCTION star_promote_fails()`)
				require.NoError(t, err)
			}
		},
		setTopic: func(t *testing.T, id, projectID, name string, activity *time.Time) {
			var at any
			if activity != nil {
				at = activity.UTC()
			}
			_, err := db.Exec(`UPDATE webchat_topic SET project_id = $1, name = $2, last_activity_at = $3 WHERE id = $4`, projectID, name, at, id)
			require.NoError(t, err)
		},
		addDMRow: func(t *testing.T, participantID, key, peerKind, peerID string, activity time.Time) {
			_, err := db.Exec(`INSERT INTO webchat_dm (conversation_key, participant_id, peer_id, peer_kind, last_activity_at) VALUES ($1, $2, $3, $4, $5)`,
				key, participantID, peerID, peerKind, activity.UTC())
			require.NoError(t, err)
		},
		addAgent: func(t *testing.T, id, name, slug string) {
			_, err := db.Exec(`INSERT INTO agents (id, name, slug) VALUES ($1, $2, $3)`, id, name, slug)
			require.NoError(t, err)
		},
		addNamedUser: func(t *testing.T, id, displayName string) {
			_, err := db.Exec(`INSERT INTO users (id, display_name) VALUES ($1, $2)`, id, displayName)
			require.NoError(t, err)
		},
		addNativeConversation: func(t *testing.T, id, ref string) {
			_, err := db.Exec(`INSERT INTO conversations (id, kind, surface, external_ref, drift_state, last_activity_at, created_at)
VALUES ($1, 'direct', 'native', $2, 'active', NOW(), NOW())`, id, ref)
			require.NoError(t, err)
		},
		setTopicConversation: func(t *testing.T, topicID, conversationID string) {
			_, err := db.Exec(`UPDATE webchat_topic SET conversation_id = $1 WHERE id = $2`, conversationID, topicID)
			require.NoError(t, err)
		},
		deleteConversation: func(t *testing.T, id string) {
			_, err := db.Exec(`UPDATE conversations SET deleted_at = NOW() WHERE id = $1`, id)
			require.NoError(t, err)
		},
	}
	require.NotNil(t, env.stars)
	testMessageStarStoreContract(t, env)
	testMessageStarLifecycleContract(t, env)
	testStarredHistoryContract(t, env)
	testStarredConversationsContract(t, env)
}
