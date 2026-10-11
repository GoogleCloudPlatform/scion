// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !no_sqlite && (!hubshard || hubshard_6)

package hub

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setEdgeUpdatedText rewrites the stored updated text of edgeID and checks
// that it is what the database holds.
func setEdgeUpdatedText(t *testing.T, s store.Store, edgeID, text string) {
	t.Helper()
	dbs, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "the test store exposes its database")
	_, err := dbs.DB().ExecContext(context.Background(),
		"UPDATE delegation_edges SET updated = ? WHERE id = ?", text, edgeID)
	require.NoError(t, err)
	var raw string
	require.NoError(t, dbs.DB().QueryRowContext(context.Background(),
		"SELECT CAST(updated AS TEXT) FROM delegation_edges WHERE id = ?", edgeID).Scan(&raw))
	require.Equal(t, text, raw)
}

// A re-issue of an edge whose stored updated text (SQLite) names its instant
// in a non-canonical form succeeds: the edge guard compares instants.
func TestScopeReissue_NonCanonicalEdgeUpdatedText(t *testing.T) {
	cest := time.FixedZone("CEST", 2*3600)
	for name, form := range map[string]func(time.Time) string{
		"rfc3339-z":           func(i time.Time) string { return i.UTC().Format(time.RFC3339Nano) },
		"rfc3339-offset":      func(i time.Time) string { return i.In(cest).Format(time.RFC3339Nano) },
		"go-string-monotonic": func(i time.Time) string { return i.UTC().String() + " m=+12.345678901" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newReissueFixture(t, "rs-ts-"+name, store.ProjectRoleOwner)
			f.run(t, f.parent, false)
			old := f.activeEdge(t, f.child)
			setEdgeUpdatedText(t, f.store, old.ID, form(old.UpdatedAt))
			require.True(t, old.UpdatedAt.Equal(f.activeEdge(t, f.child).UpdatedAt), "the driver reads the same instant back")

			resp, err := f.srv.runScopeReissue(context.Background(), f.reload(t, f.child), f.operator, false, "")
			require.NoError(t, err)
			require.NotNil(t, resp)
			assert.NotEmpty(t, resp.EdgeNew)
			assert.Equal(t, resp.EdgeNew, f.activeEdge(t, f.child).ID, "the replacement is the active edge")
			got, err := f.store.GetDelegationEdge(context.Background(), old.ID)
			require.NoError(t, err)
			assert.False(t, got.Active)
			assert.Equal(t, store.EdgeDeactivationScopeReissueReplaced, got.Cause)
			assert.Len(t, reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissued), 1)
		})
	}
}

// A re-issue whose edge changed after the plan read it, and before the
// commit, is refused with the conflict, and nothing is written: no new
// edge, no revoked credential, no audit row.
func TestScopeReissue_EdgeChangedAfterPlanRefuses(t *testing.T) {
	ctx := context.Background()
	for name, change := range map[string]func(t *testing.T, f *reissueFixture, e *store.DelegationEdge){
		"updated-instant": func(t *testing.T, f *reissueFixture, e *store.DelegationEdge) {
			setEdgeUpdatedText(t, f.store, e.ID, e.UpdatedAt.Add(time.Second).UTC().Format(time.RFC3339Nano))
		},
		"deactivated-reactivated": func(t *testing.T, f *reissueFixture, e *store.DelegationEdge) {
			ok, err := f.store.DeactivateDelegationEdgeGuarded(ctx, e.ID, store.DelegationEdgeDeactivateGuard{}, store.EdgeDeactivationScopeReissueReplaced, "other-op")
			require.NoError(t, err)
			require.True(t, ok)
			require.NoError(t, f.store.ReactivateDelegationEdge(ctx, e.ID, store.EdgeDeactivationScopeReissueReplaced))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newReissueFixture(t, "rs-chg-"+name, store.ProjectRoleOwner)
			f.run(t, f.parent, false)
			jti := "rs-chg-jti-" + name
			insertTestAgentCredential(t, f.store, f.child.ID, f.projectID, jti)
			credBefore := getTestAgentCredential(t, f.store, jti)
			old := f.activeEdge(t, f.child)
			edgeCount := len(f.allEdges(t, f.child))
			fired := false
			f.faults.beforeTx = func() {
				fired = true
				change(t, f, old)
			}
			f.faults.arm()
			f.client.resetAuthCalled = false

			resp, err := f.srv.runScopeReissue(ctx, f.reload(t, f.child), f.operator, false, "")
			require.True(t, fired, "the edge changed between the plan and the commit")
			require.ErrorIs(t, err, errReissueConflict)
			assert.Nil(t, resp)

			edges := f.allEdges(t, f.child)
			assert.Len(t, edges, edgeCount, "no edge written")
			var active []*store.DelegationEdge
			for _, e := range edges {
				if e.Active {
					active = append(active, e)
				}
			}
			require.Len(t, active, 1, "no replacement edge")
			assert.Equal(t, old.ID, active[0].ID, "the planned edge stays the active edge")
			assertCredentialUnrevoked(t, f.store, jti, credBefore)
			assert.False(t, f.client.resetAuthCalled, "nothing pushed")
			assert.Empty(t, reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissued))
		})
	}
}
