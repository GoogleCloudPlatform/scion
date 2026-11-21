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

package entadapter

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/message"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedFeedConversation(t *testing.T, cs *CompositeStore, projectID string) *store.Conversation {
	t.Helper()
	conv := &store.Conversation{
		ID:          uuid.NewString(),
		ProjectID:   &projectID,
		Kind:        "group",
		Surface:     "native",
		DisplayName: "feed-test",
	}
	require.NoError(t, cs.CreateConversation(context.Background(), conv))
	return conv
}

// Criterion 9 (SQLite): N concurrent CreateMessage calls into one conversation
// produce gap-free, duplicate-free seqs 1..N, and a reader holding head H sees
// every seq <= H.
func TestCreateMessage_ConcurrentSeqAllocation_SQLite(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	projectID := uuid.NewString()
	conv := seedFeedConversation(t, cs, projectID)

	const n = 20
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seqs = make([]int64, 0, n)
		errs = make([]error, 0)
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			msg := &store.Message{
				ID:             uuid.NewString(),
				ProjectID:      projectID,
				ConversationID: conv.ID,
				Sender:         "user:alice",
				SenderID:       "user-1",
				Recipient:      "agent:coder",
				RecipientID:    "agent-1",
				Msg:            "hello",
				Type:           "instruction",
				DispatchState:  store.MessageDispatchDispatched,
			}
			err := cs.CreateMessage(ctx, msg)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			seqs = append(seqs, msg.ConversationSeq)
		}(i)
	}
	wg.Wait()
	require.Empty(t, errs)
	require.Len(t, seqs, n)

	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i := 0; i < n; i++ {
		assert.Equal(t, int64(i+1), seqs[i])
	}

	head, err := cs.ConversationHeadSeq(ctx, conv.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(n), head)

	page, err := cs.ListConversationFeed(ctx, store.ConversationFeedQuery{
		ConversationID: conv.ID,
		AfterSeq:       0,
		Limit:          n + 5,
		Filter:         store.ConversationFeedFilterAll,
	})
	require.NoError(t, err)
	require.Len(t, page.Messages, n)
	assert.Equal(t, int64(n), page.ScannedTo)
	assert.False(t, page.HasMore)
	assert.False(t, page.Truncated)
	for i, m := range page.Messages {
		assert.Equal(t, int64(i+1), m.ConversationSeq)
	}
}

// Criterion 10: Backfill assigns seqs in (created, id) order, is idempotent,
// and repairs NULL rows interleaved after existing seqs.
func TestBackfillConversationMessageSeqs(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	projectID := uuid.NewString()
	conv := seedFeedConversation(t, cs, projectID)
	convUUID := uuid.MustParse(conv.ID)
	projUUID := uuid.MustParse(projectID)

	t1 := time.Date(2026, 11, 21, 10, 0, 1, 0, time.UTC)
	t2 := time.Date(2026, 11, 21, 10, 0, 2, 0, time.UTC)
	// Same timestamp for m2 and m3; order by UUID string/bytes.
	idA := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	idB := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	idC := uuid.MustParse("00000000-0000-0000-0000-000000000003")

	insertNullSeq := func(id uuid.UUID, created time.Time, body string) {
		_, err := client.Message.Create().
			SetID(id).
			SetProjectID(projUUID).
			SetConversationID(convUUID).
			SetSender("user:alice").
			SetSenderID("u1").
			SetRecipient("agent:a").
			SetRecipientID("a1").
			SetMsg(body).
			SetType("chat").
			SetDispatchState(store.MessageDispatchDispatched).
			SetCreated(created).
			Save(ctx)
		require.NoError(t, err)
	}

	// Insert out of order to prove ORDER BY created ASC, id ASC.
	insertNullSeq(idC, t2, "third")
	insertNullSeq(idB, t2, "second")
	insertNullSeq(idA, t1, "first")

	require.NoError(t, cs.BackfillConversationMessageSeqs(ctx))

	sA, err := cs.MessageConversationSeq(ctx, conv.ID, idA.String())
	require.NoError(t, err)
	sB, err := cs.MessageConversationSeq(ctx, conv.ID, idB.String())
	require.NoError(t, err)
	sC, err := cs.MessageConversationSeq(ctx, conv.ID, idC.String())
	require.NoError(t, err)
	assert.Equal(t, int64(1), sA)
	assert.Equal(t, int64(2), sB)
	assert.Equal(t, int64(3), sC)

	head, err := cs.ConversationHeadSeq(ctx, conv.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), head)

	// Idempotent re-run leaves seqs and head unchanged.
	require.NoError(t, cs.BackfillConversationMessageSeqs(ctx))
	head2, err := cs.ConversationHeadSeq(ctx, conv.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), head2)

	// Simulate downgrade inserting a NULL-seq row after existing seqs 1..3.
	idD := uuid.MustParse("00000000-0000-0000-0000-000000000004")
	insertNullSeq(idD, t2.Add(time.Second), "downgraded-row")

	require.NoError(t, cs.BackfillConversationMessageSeqs(ctx))
	sD, err := cs.MessageConversationSeq(ctx, conv.ID, idD.String())
	require.NoError(t, err)
	assert.Equal(t, int64(4), sD)

	head3, err := cs.ConversationHeadSeq(ctx, conv.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(4), head3)
}

// Criterion 11: ListConversationFeed applies others/addressed/all correctly,
// and user and agent principals with colliding UUID namespaces do not cross-match.
func TestListConversationFeed_FiltersAndNamespaceIsolation(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	projectID := uuid.NewString()
	conv := seedFeedConversation(t, cs, projectID)

	// Deliberately use the SAME UUID for a user and an agent to verify namespace isolation.
	sharedUUID := uuid.NewString()
	otherUserID := uuid.NewString()
	otherAgentID := uuid.NewString()

	createMsg := func(sender, senderID, recipient, recipientID, body string) *store.Message {
		m := &store.Message{
			ID:             uuid.NewString(),
			ProjectID:      projectID,
			ConversationID: conv.ID,
			Sender:         sender,
			SenderID:       senderID,
			Recipient:      recipient,
			RecipientID:    recipientID,
			Msg:            body,
			Type:           "chat",
			DispatchState:  store.MessageDispatchDispatched,
		}
		require.NoError(t, cs.CreateMessage(ctx, m))
		return m
	}

	// seq 1: sent by user:sharedUUID (caller's own send)
	m1 := createMsg("user:alice", sharedUUID, "agent:other", otherAgentID, "from-user-self")
	// seq 2: sent by agent:same-uuid with colliding SenderID == sharedUUID, addressed to otherUserID
	m2 := createMsg("agent:colliding", sharedUUID, "user:bob", otherUserID, "from-agent-same-uuid")
	// seq 3: sent by agent:other directly to user:sharedUUID (via recipient_id)
	m3 := createMsg("agent:other", otherAgentID, "user:alice", sharedUUID, "direct-to-user")
	// seq 4: sent by agent:other to agent:colliding (colliding RecipientID == sharedUUID, NOT user!)
	m4 := createMsg("agent:other", otherAgentID, "agent:colliding", sharedUUID, "direct-to-agent-same-uuid")
	// seq 5: sent by user:bob to group, with message_addressees row for (user, sharedUUID)
	m5 := createMsg("user:bob", otherUserID, "group:g", conv.ID, "mentioned-user")
	require.NoError(t, cs.AddAddressee(ctx, &store.MessageAddressee{
		ID:            uuid.NewString(),
		MessageID:     m5.ID,
		PrincipalKind: "user",
		PrincipalID:   sharedUUID,
		Via:           "body-mention",
		DeliveryState: "pending",
	}))
	// seq 6: sent by user:bob to group, with message_addressees row for (agent, sharedUUID) only
	m6 := createMsg("user:bob", otherUserID, "group:g", conv.ID, "mentioned-agent-same-uuid")
	require.NoError(t, cs.AddAddressee(ctx, &store.MessageAddressee{
		ID:            uuid.NewString(),
		MessageID:     m6.ID,
		PrincipalKind: "agent",
		PrincipalID:   sharedUUID,
		Via:           "body-mention",
		DeliveryState: "pending",
	}))

	ids := func(msgs []store.Message) []string {
		out := make([]string, len(msgs))
		for i, m := range msgs {
			out[i] = m.ID
		}
		return out
	}

	// FilterAll returns all 6 in seq order.
	allPage, err := cs.ListConversationFeed(ctx, store.ConversationFeedQuery{
		ConversationID: conv.ID,
		AfterSeq:       0,
		Filter:         store.ConversationFeedFilterAll,
		PrincipalKind:  "user",
		PrincipalID:    sharedUUID,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{m1.ID, m2.ID, m3.ID, m4.ID, m5.ID, m6.ID}, ids(allPage.Messages))
	assert.Equal(t, int64(6), allPage.ScannedTo)
	assert.False(t, allPage.HasMore)
	assert.False(t, allPage.Truncated)

	// FilterOthers for (user, sharedUUID): excludes m1, keeps m2 (agent with same UUID!).
	othersPage, err := cs.ListConversationFeed(ctx, store.ConversationFeedQuery{
		ConversationID: conv.ID,
		AfterSeq:       0,
		Filter:         store.ConversationFeedFilterOthers,
		PrincipalKind:  "user",
		PrincipalID:    sharedUUID,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{m2.ID, m3.ID, m4.ID, m5.ID, m6.ID}, ids(othersPage.Messages))
	assert.Equal(t, int64(6), othersPage.ScannedTo)

	// FilterAddressed for (user, sharedUUID): matches m3 (recipient user) and m5 (addressee user),
	// excludes m4 (recipient agent with same UUID) and m6 (addressee agent with same UUID).
	addrPage, err := cs.ListConversationFeed(ctx, store.ConversationFeedQuery{
		ConversationID: conv.ID,
		AfterSeq:       0,
		Filter:         store.ConversationFeedFilterAddressed,
		PrincipalKind:  "user",
		PrincipalID:    sharedUUID,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{m3.ID, m5.ID}, ids(addrPage.Messages))
	assert.Equal(t, int64(6), addrPage.ScannedTo)
	assert.False(t, addrPage.HasMore)

	// Pagination & HasMore: limit=1 on FilterAddressed returns m3 with HasMore=true, ScannedTo=3.
	page1, err := cs.ListConversationFeed(ctx, store.ConversationFeedQuery{
		ConversationID: conv.ID,
		AfterSeq:       0,
		Limit:          1,
		Filter:         store.ConversationFeedFilterAddressed,
		PrincipalKind:  "user",
		PrincipalID:    sharedUUID,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{m3.ID}, ids(page1.Messages))
	assert.Equal(t, int64(3), page1.ScannedTo)
	assert.True(t, page1.HasMore)

	// Next page from ScannedTo=3 returns m5 with HasMore=false, ScannedTo=6 (head).
	page2, err := cs.ListConversationFeed(ctx, store.ConversationFeedQuery{
		ConversationID: conv.ID,
		AfterSeq:       page1.ScannedTo,
		Limit:          1,
		Filter:         store.ConversationFeedFilterAddressed,
		PrincipalKind:  "user",
		PrincipalID:    sharedUUID,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{m5.ID}, ids(page2.Messages))
	assert.Equal(t, int64(6), page2.ScannedTo)
	assert.False(t, page2.HasMore)

	// Truncated gap detection: delete seq 2 (m2), then query after seq 1.
	_, err = client.Message.Delete().Where(message.IDEQ(uuid.MustParse(m2.ID))).Exec(ctx)
	require.NoError(t, err)

	gapPage, err := cs.ListConversationFeed(ctx, store.ConversationFeedQuery{
		ConversationID: conv.ID,
		AfterSeq:       1,
		Filter:         store.ConversationFeedFilterAll,
	})
	require.NoError(t, err)
	assert.True(t, gapPage.Truncated)
	assert.Equal(t, []string{m3.ID, m4.ID, m5.ID, m6.ID}, ids(gapPage.Messages))
}

// Regression test: SetMessageConversationID must re-allocate conversation_seq
// in the target conversation so moving or merging messages never collides on
// the (conversation_id, conversation_seq) unique index.
func TestSetMessageConversationID_ReallocatesSeqWithoutCollision(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	projectID := uuid.NewString()

	convA := seedFeedConversation(t, cs, projectID)
	convB := seedFeedConversation(t, cs, projectID)
	convC := seedFeedConversation(t, cs, projectID)

	newMsg := func(convID, body string) *store.Message {
		m := &store.Message{
			ID:             uuid.NewString(),
			ProjectID:      projectID,
			ConversationID: convID,
			Sender:         "user:alice",
			SenderID:       "u1",
			Recipient:      "agent:a",
			RecipientID:    "a1",
			Msg:            body,
			Type:           "chat",
			DispatchState:  store.MessageDispatchDispatched,
		}
		require.NoError(t, cs.CreateMessage(ctx, m))
		return m
	}

	// Case 1: Move msgA1 (seq 1 in A) into empty conv B, then CreateMessage in B.
	msgA1 := newMsg(convA.ID, "from-A")
	require.Equal(t, int64(1), msgA1.ConversationSeq)

	require.NoError(t, cs.SetMessageConversationID(ctx, msgA1.ID, convB.ID))
	seqInB, err := cs.MessageConversationSeq(ctx, convB.ID, msgA1.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), seqInB)
	headB, err := cs.ConversationHeadSeq(ctx, convB.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), headB)

	msgB2 := newMsg(convB.ID, "new-in-B")
	assert.Equal(t, int64(2), msgB2.ConversationSeq)

	// Case 2: Merge msgC1 (seq 1 in C) into conv B which already has seqs 1..2.
	msgC1 := newMsg(convC.ID, "from-C")
	require.Equal(t, int64(1), msgC1.ConversationSeq)

	require.NoError(t, cs.SetMessageConversationID(ctx, msgC1.ID, convB.ID))
	seqCInB, err := cs.MessageConversationSeq(ctx, convB.ID, msgC1.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), seqCInB)
	headB, err = cs.ConversationHeadSeq(ctx, convB.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), headB)

	// Idempotent call for a message already in conv B is a no-op.
	require.NoError(t, cs.SetMessageConversationID(ctx, msgC1.ID, convB.ID))
	headB2, err := cs.ConversationHeadSeq(ctx, convB.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), headB2)

	// Nonexistent message returns ErrNotFound without bumping head.
	err = cs.SetMessageConversationID(ctx, uuid.NewString(), convB.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	headB3, err := cs.ConversationHeadSeq(ctx, convB.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), headB3)
}
