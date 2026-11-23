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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// TestCreateMessage_ConcurrentSeqAllocation_Postgres verifies criterion 9 on
// Postgres: N concurrent CreateMessage calls into one conversation serialize on
// the conversation row lock, producing gap-free seqs 1..N with no duplicates,
// and a concurrent reader holding head H always sees every seq <= H.
//
// Always compiled; skips unless built with -tags integration and
// SCION_TEST_POSTGRES_URL is set.
func TestCreateMessage_ConcurrentSeqAllocation_Postgres(t *testing.T) {
	url := enttest.NewSchemaURL(t)
	ctx := context.Background()

	client, err := entc.OpenPostgres(url, entc.PoolConfig{MaxOpenConns: 16, MaxIdleConns: 4})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	cs := NewCompositeStore(client)
	projectID := uuid.NewString()
	conv := seedFeedConversation(t, cs, projectID)

	const n = 24
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seqs = make([]int64, 0, n)
		errs = make([]error, 0)
	)

	// Concurrent reader verifying commit-order visibility: whenever head = H
	// is observed, all seqs 1..H must already be visible.
	readDone := make(chan struct{})
	var readerErr error
	go func() {
		defer close(readDone)
		for {
			head, hErr := cs.ConversationHeadSeq(ctx, conv.ID)
			if hErr != nil {
				readerErr = hErr
				return
			}
			if head > 0 {
				page, pErr := cs.ListConversationFeed(ctx, store.ConversationFeedQuery{
					ConversationID: conv.ID,
					AfterSeq:       0,
					Limit:          n + 5,
					Filter:         store.ConversationFeedFilterAll,
				})
				if pErr != nil {
					readerErr = pErr
					return
				}
				if int64(len(page.Messages)) < head {
					readerErr = assert.AnError
					return
				}
				for i := int64(0); i < head; i++ {
					if page.Messages[i].ConversationSeq != i+1 {
						readerErr = assert.AnError
						return
					}
				}
			}
			if head >= n {
				return
			}
		}
	}()

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg := &store.Message{
				ID:             uuid.NewString(),
				ProjectID:      projectID,
				ConversationID: conv.ID,
				Sender:         "user:alice",
				SenderID:       "user-1",
				Recipient:      "agent:coder",
				RecipientID:    "agent-1",
				Msg:            "pg-concurrent",
				Type:           "instruction",
				DispatchState:  store.MessageDispatchDispatched,
			}
			cErr := cs.CreateMessage(ctx, msg)
			mu.Lock()
			defer mu.Unlock()
			if cErr != nil {
				errs = append(errs, cErr)
				return
			}
			seqs = append(seqs, msg.ConversationSeq)
		}()
	}
	wg.Wait()
	<-readDone

	require.NoError(t, readerErr)
	require.Empty(t, errs)
	require.Len(t, seqs, n)

	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i := 0; i < n; i++ {
		assert.Equal(t, int64(i+1), seqs[i])
	}
}

// TestBackfillConversationMessageSeqs_DuplicateSeqs_Postgres verifies that
// BackfillConversationMessageSeqs repairs both NULL and duplicate
// (conversation_id, conversation_seq) pairs on Postgres and is idempotent.
//
// Always compiled; skips unless built with -tags integration and
// SCION_TEST_POSTGRES_URL is set.
func TestBackfillConversationMessageSeqs_DuplicateSeqs_Postgres(t *testing.T) {
	url := enttest.NewSchemaURL(t)
	ctx := context.Background()

	client, err := entc.OpenPostgres(url, entc.PoolConfig{MaxOpenConns: 8, MaxIdleConns: 2})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	cs := NewCompositeStore(client)
	projectID := uuid.NewString()
	conv := seedFeedConversation(t, cs, projectID)
	convUUID := uuid.MustParse(conv.ID)
	projUUID := uuid.MustParse(projectID)

	t1 := time.Date(2026, 11, 21, 10, 0, 1, 0, time.UTC)
	t2 := time.Date(2026, 11, 21, 10, 0, 2, 0, time.UTC)
	t3 := time.Date(2026, 11, 21, 10, 0, 3, 0, time.UTC)

	idKeep1 := uuid.MustParse("00000000-0000-0000-0000-000000000101")
	idDup1 := uuid.MustParse("00000000-0000-0000-0000-000000000102")
	idNull := uuid.MustParse("00000000-0000-0000-0000-000000000103")

	insertMsg := func(id uuid.UUID, created time.Time, seq *int64, body string) {
		b := client.Message.Create().
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
			SetCreated(created)
		if seq != nil {
			b = b.SetConversationSeq(*seq)
		}
		_, saveErr := b.Save(ctx)
		require.NoError(t, saveErr)
	}

	seq1 := int64(1)
	insertMsg(idKeep1, t1, &seq1, "orig-1")
	insertMsg(idDup1, t2, &seq1, "dup-1")
	insertMsg(idNull, t3, nil, "null-row")

	_, err = cs.DB().ExecContext(ctx, "UPDATE conversations SET last_message_seq = 1 WHERE id = $1::uuid", conv.ID)
	require.NoError(t, err)

	require.NoError(t, cs.BackfillConversationMessageSeqs(ctx))

	s1, err := cs.MessageConversationSeq(ctx, conv.ID, idKeep1.String())
	require.NoError(t, err)
	s2, err := cs.MessageConversationSeq(ctx, conv.ID, idDup1.String())
	require.NoError(t, err)
	s3, err := cs.MessageConversationSeq(ctx, conv.ID, idNull.String())
	require.NoError(t, err)
	assert.Equal(t, int64(1), s1)
	assert.Equal(t, int64(2), s2)
	assert.Equal(t, int64(3), s3)

	head, err := cs.ConversationHeadSeq(ctx, conv.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), head)

	// Idempotent re-run.
	require.NoError(t, cs.BackfillConversationMessageSeqs(ctx))
	head2, err := cs.ConversationHeadSeq(ctx, conv.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), head2)

	// Duplicate-only conversation (NO NULL rows): exercises the UNION ... HAVING COUNT(*) > 1
	// branch independently of NULL-seq detection on Postgres.
	convDupOnly := seedFeedConversation(t, cs, projectID)
	convDupOnlyUUID := uuid.MustParse(convDupOnly.ID)
	idOnly1 := uuid.MustParse("00000000-0000-0000-0000-000000000201")
	idOnly2 := uuid.MustParse("00000000-0000-0000-0000-000000000202")
	for _, tc := range []struct {
		id      uuid.UUID
		created time.Time
		body    string
	}{
		{idOnly1, t1, "pg-dup-only-orig"},
		{idOnly2, t2, "pg-dup-only-collision"},
	} {
		_, saveErr := client.Message.Create().
			SetID(tc.id).
			SetProjectID(projUUID).
			SetConversationID(convDupOnlyUUID).
			SetSender("user:alice").
			SetSenderID("u1").
			SetRecipient("agent:a").
			SetRecipientID("a1").
			SetMsg(tc.body).
			SetType("chat").
			SetDispatchState(store.MessageDispatchDispatched).
			SetCreated(tc.created).
			SetConversationSeq(1).
			Save(ctx)
		require.NoError(t, saveErr)
	}
	_, err = cs.DB().ExecContext(ctx, "UPDATE conversations SET last_message_seq = 1 WHERE id = $1::uuid", convDupOnly.ID)
	require.NoError(t, err)

	require.NoError(t, cs.BackfillConversationMessageSeqs(ctx))

	sOnly1, err := cs.MessageConversationSeq(ctx, convDupOnly.ID, idOnly1.String())
	require.NoError(t, err)
	sOnly2, err := cs.MessageConversationSeq(ctx, convDupOnly.ID, idOnly2.String())
	require.NoError(t, err)
	assert.Equal(t, int64(1), sOnly1)
	assert.Equal(t, int64(2), sOnly2)

	dupOnlyHead, err := cs.ConversationHeadSeq(ctx, convDupOnly.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), dupOnlyHead)
}
