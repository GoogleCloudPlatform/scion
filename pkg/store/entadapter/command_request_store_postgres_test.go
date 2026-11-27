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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/txdiag"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// TestCommandRequestPendingCap_Postgres verifies R1 (ptone/scion#9285):
// CreateCommandRequestPrompt takes a per-agent transaction-scoped advisory lock
// (pg_advisory_xact_lock(LockCommandRequestQuota, StableProjectHash(agentID)))
// under PostgreSQL READ COMMITTED so concurrent sends with distinct keys cannot
// phantom-insert past MaxPendingCommandRequestsPerAgent (10).
func TestCommandRequestPendingCap_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("Postgres enttest harness not active (set SCION_TEST_POSTGRES_URL with -tags integration)")
	}
	ctx := context.Background()
	url := enttest.NewSchemaURL(t)
	client, err := entc.OpenPostgres(url, entc.PoolConfig{
		MaxOpenConns: 16,
		MaxIdleConns: 8,
		Diag:         txdiag.QuietConfig(),
	})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	require.NoError(t, cs.DB().PingContext(ctx))

	projID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Slug: "cr-pg-cap-" + projID[:8],
		Name: "CR Postgres Cap",
	}))
	approverID := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)

	runBurst := func(t *testing.T, agentSlug string, preSeed int, burst int) {
		t.Helper()
		agentID := uuid.NewString()
		require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
			ID:        agentID,
			Slug:      agentSlug,
			Name:      agentSlug,
			ProjectID: projID,
		}))
		for i := 0; i < preSeed; i++ {
			_, err := cs.CreateCommandRequestPrompt(ctx, makeCommandRequestPromptInput(t, projID, agentID, approverID, fmt.Sprintf("seed-%02d", i), "echo seed", false, now.Add(time.Duration(i)*time.Millisecond)))
			require.NoError(t, err)
		}

		start := make(chan struct{})
		var succeeded, limited, otherErr atomic.Int32
		var wg sync.WaitGroup
		wg.Add(burst)
		for i := 0; i < burst; i++ {
			key := fmt.Sprintf("burst-%02d", i)
			go func() {
				defer wg.Done()
				<-start
				_, cerr := cs.CreateCommandRequestPrompt(ctx, makeCommandRequestPromptInput(t, projID, agentID, approverID, key, "echo burst", false, now.Add(time.Second)))
				if cerr == nil {
					succeeded.Add(1)
					return
				}
				var limErr *store.CommandRequestLimitError
				if errors.As(cerr, &limErr) {
					limited.Add(1)
					return
				}
				otherErr.Add(1)
				t.Errorf("unexpected error for %s: %v", key, cerr)
			}()
		}
		close(start)
		wg.Wait()

		wantAllowed := int32(messages.MaxPendingCommandRequestsPerAgent - preSeed)
		assert.Equal(t, int32(0), otherErr.Load(), "expected zero unexpected errors")
		assert.Equal(t, wantAllowed, succeeded.Load(), "expected exactly %d concurrent inserts to succeed", wantAllowed)
		assert.Equal(t, int32(burst)-wantAllowed, limited.Load(), "expected excess concurrent inserts to hit CommandRequestLimitError")

		listed, err := cs.ListAgentCommandRequests(ctx, &store.ListAgentCommandRequestsOptions{
			AgentID: agentID,
			Limit:   store.MaxCommandRequestListLimit,
		})
		require.NoError(t, err)
		assert.Len(t, listed.Items, messages.MaxPendingCommandRequestsPerAgent)
	}

	t.Run("starting at 9 pending with 8 concurrent distinct keys", func(t *testing.T) {
		runBurst(t, "pg-cap-from-9", 9, 8)
	})
	t.Run("starting at 0 pending with 15 concurrent distinct keys", func(t *testing.T) {
		runBurst(t, "pg-cap-from-0", 0, 15)
	})
}
