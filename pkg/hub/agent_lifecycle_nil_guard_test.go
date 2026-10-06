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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nilPageStore answers ListAgents with a nil page and no error.
type nilPageStore struct {
	store.Store
}

func (nilPageStore) ListAgents(context.Context, store.AgentFilter, store.ListOptions) (*store.ListResult[store.Agent], error) {
	return nil, nil
}

// The lifecycle transactions and the reincarnate authority check refuse a
// nil agent (or, for the project edge walk, a nil page) with a wrapped
// store.ErrInvalidInput, or a 500, and write nothing.
func TestLifecycleNilAgentGuards(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	ctx := context.Background()
	bystander := mustGetAgent(t, s, setupBrokerAgentInPhase(t, s, "nil-guard", state.PhaseRunning).ID)

	audits := func() int {
		recs, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{})
		require.NoError(t, err)
		return len(recs)
	}

	cases := []struct {
		name string
		run  func(t *testing.T) error
	}{
		{"softDeleteAgentTx", func(t *testing.T) error {
			return s.WithTx(ctx, func(tx store.Store) error { return srv.softDeleteAgentTx(ctx, tx, nil, AuditActor{}) })
		}},
		{"hardDeleteAgentTx", func(t *testing.T) error {
			return s.WithTx(ctx, func(tx store.Store) error { return srv.hardDeleteAgentTx(ctx, tx, nil, AuditActor{}) })
		}},
		{"restoreAgentTx", func(t *testing.T) error {
			return srv.restoreAgentTx(ctx, nil, AuditActor{})
		}},
		{"reincarnateClaimTx", func(t *testing.T) error {
			return srv.reincarnateClaimTx(ctx, nil, &store.AgentReincarnation{}, nil, AuditActor{})
		}},
		{"deactivateProjectAgentEdges", func(t *testing.T) error {
			_, _, err := deactivateProjectAgentEdges(ctx, nilPageStore{Store: s}, bystander.ProjectID, time.Now())
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auditsBefore := audits()
			assert.ErrorIs(t, tc.run(t), store.ErrInvalidInput)
			got := mustGetAgent(t, s, bystander.ID)
			assert.Equal(t, bystander.StateVersion, got.StateVersion, "no agent write")
			assert.True(t, got.DeletedAt.IsZero())
			assert.Equal(t, auditsBefore, audits(), "no audit record")
		})
	}

	t.Run("reincarnateAuthorityFor", func(t *testing.T) {
		auditsBefore := audits()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/x/reincarnate", nil)
		auth, ok := srv.reincarnateAuthorityFor(rec, req, nil)
		assert.False(t, ok)
		assert.Nil(t, auth)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Equal(t, auditsBefore, audits(), "no audit record")
	})
}
