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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDecide_AuditRecordsDerivedPrincipalIDWhenRequestOmitsIt proves
// Decision.PrincipalID and the emitted audit record's PrincipalID are the
// identity Decide actually evaluated, not AuthzRequest.Principal.ID — which
// ordinary callers leave empty, since they build a request from only
// Principal.Identity (as AuthzRequestFromContext and every hand-built
// AuthzRequest in this file do). This holds on both the allow path and an
// early-deny path, and a supplied Principal.ID that matches the identity's
// own ID allows exactly the same way an omitted one does.
//
// This test lives apart from the rest of the identity classification suite
// (identity_classification_test.go) because it needs a real, store-backed
// test server (testServer, rs4Project); that dependency carries the
// !no_sqlite constraint this file declares, so it stays out of an
// identity_classification_test.go that must keep resolving under
// -tags no_sqlite.
func TestDecide_AuditRecordsDerivedPrincipalIDWhenRequestOmitsIt(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	ownerID := tid("principalid-owner")
	projectID := tid("principalid-project")
	rs4Project(t, s, projectID, ownerID)
	owner := NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "api")

	emitter := &capturingAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(emitter)

	t.Run("allow", func(t *testing.T) {
		emitter.records = nil
		req := AuthzRequest{
			Principal: PrincipalContext{Identity: owner},
			Resource:  Resource{Type: "project", ID: projectID},
			Action:    ActionRead,
		}
		require.Empty(t, req.Principal.ID)
		decision := srv.authzService.Decide(ctx, req)
		require.True(t, decision.Allowed)
		assert.Equal(t, ownerID, decision.PrincipalID)
		require.Len(t, emitter.records, 1)
		assert.Equal(t, ownerID, emitter.records[0].PrincipalID)
	})

	t.Run("early deny", func(t *testing.T) {
		emitter.records = nil
		req := AuthzRequest{
			Principal: PrincipalContext{Kind: PrincipalKindAgent, Identity: owner},
			Resource:  Resource{Type: "project", ID: projectID},
			Action:    ActionRead,
		}
		require.Empty(t, req.Principal.ID)
		decision := srv.authzService.Decide(ctx, req)
		require.False(t, decision.Allowed)
		assert.Equal(t, "principal kind does not match identity", decision.Reason)
		assert.Equal(t, ownerID, decision.PrincipalID)
		require.Len(t, emitter.records, 1)
		assert.Equal(t, ownerID, emitter.records[0].PrincipalID)
	})

	t.Run("allow with a supplied principal ID that matches", func(t *testing.T) {
		emitter.records = nil
		req := AuthzRequest{
			Principal: PrincipalContext{ID: ownerID, Identity: owner},
			Resource:  Resource{Type: "project", ID: projectID},
			Action:    ActionRead,
		}
		decision := srv.authzService.Decide(ctx, req)
		require.True(t, decision.Allowed)
		assert.Equal(t, ownerID, decision.PrincipalID)
		require.Len(t, emitter.records, 1)
		assert.Equal(t, ownerID, emitter.records[0].PrincipalID)
	})
}
