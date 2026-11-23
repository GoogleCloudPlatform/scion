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
