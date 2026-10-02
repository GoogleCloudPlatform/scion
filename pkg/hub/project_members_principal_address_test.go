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
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mmrErrorCode decodes an error response's code.
func mmrErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var errBody ErrorResponse
	require.NoError(t, json.Unmarshal(body, &errBody), string(body))
	return errBody.Error.Code
}

// TestSetMemberRoles_NonexistentPrincipalID: a well-formed user or agent ID
// that names no record is, on PUT, the same 400 invalid_request an unknown
// email gets; it used to reach the binding create inside the transaction and
// surface as a 500 (ptone/scion#2529). DELETE keeps its "no bindings" 404.
// Nothing is written.
func TestSetMemberRoles_NonexistentPrincipalID(t *testing.T) {
	f := setupMMRFixture(t)

	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "user", "nobody-fix3@test.com", []string{f.memberRD.ID}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	emailCode := mmrErrorCode(t, rec.Body.Bytes())
	require.Equal(t, ErrCodeInvalidRequest, emailCode)

	for _, principalType := range []string{"user", "agent"} {
		missing := tid(t.Name() + "-missing-" + principalType)

		rec = putMemberRoles(t, f.srv, f.owner, f.projectID, principalType, missing, []string{f.memberRD.ID}, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "PUT %s: %s", principalType, rec.Body.String())
		assert.Equal(t, emailCode, mmrErrorCode(t, rec.Body.Bytes()), "PUT %s", principalType)
		assert.Contains(t, rec.Body.String(), principalType+" not found: "+missing)

		rec = deleteMemberRoles(t, f.srv, f.owner, f.projectID, principalType, missing)
		assert.Equal(t, http.StatusNotFound, rec.Code, "DELETE %s: %s", principalType, rec.Body.String())
		assert.Equal(t, ErrCodeNotFound, mmrErrorCode(t, rec.Body.Bytes()), "DELETE %s", principalType)

		assert.Empty(t, mmrBindingsFor(t, f.store, principalType, missing, f.projectID))
	}
}

// TestSetMemberRoles_ExistingAgentPrincipalStillAddressable: the existence
// check does not refuse an agent that exists.
func TestSetMemberRoles_ExistingAgentPrincipalStillAddressable(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()
	agentID := tid(t.Name() + "-agent")
	require.NoError(t, f.store.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: agentID, Name: "fix3-agent", ProjectID: f.projectID,
		Phase: "running", CreatedBy: f.owner.ID, OwnerID: f.owner.ID, Ancestry: []string{f.owner.ID},
	}))

	rec := putMemberRoles(t, f.srv, f.owner, f.projectID, "agent", agentID, []string{f.memberRD.ID}, nil)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Len(t, mmrBindingsFor(t, f.store, "agent", agentID, f.projectID), 1)

	rec = deleteMemberRoles(t, f.srv, f.owner, f.projectID, "agent", agentID)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// principalLookupErrorStore fails the user and agent lookups for one ID with
// a store error that is not a not-found.
type principalLookupErrorStore struct {
	store.Store
	failID string
	err    error
}

func (s *principalLookupErrorStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == s.failID {
		return nil, s.err
	}
	return s.Store.GetUser(ctx, id)
}

func (s *principalLookupErrorStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if id == s.failID {
		return nil, s.err
	}
	return s.Store.GetAgent(ctx, id)
}

// TestSetMemberRoles_PrincipalLookupStoreErrorIs500: only the addressed
// principal's not-found maps to a 400; any other store error on that lookup
// is still a 500.
func TestSetMemberRoles_PrincipalLookupStoreErrorIs500(t *testing.T) {
	f := setupMMRFixture(t)
	failID := tid(t.Name() + "-fail")
	f.srv.membershipService.store = &principalLookupErrorStore{Store: f.store, failID: failID, err: errors.New("simulated store failure")}

	for _, principalType := range []string{"user", "agent"} {
		rec := putMemberRoles(t, f.srv, f.owner, f.projectID, principalType, failID, []string{f.memberRD.ID}, nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, "PUT %s: %s", principalType, rec.Body.String())
		assert.Equal(t, ErrCodeInternalError, mmrErrorCode(t, rec.Body.Bytes()), "PUT %s", principalType)
	}
}

// TestSetMemberRoles_NonCanonicalPrincipalIDIsCanonicalised: uuid.Parse
// accepts several spellings of one ID (upper case, "urn:uuid:", braces, no
// dashes). The members PUT and DELETE address the principal by the canonical
// lower-case dashed ID whichever spelling the path uses, so no second,
// non-canonical binding is ever stored: such a binding granted nothing,
// slipped past the one-built-in check and survived a DELETE by the canonical
// ID (ptone/scion#2529).
func TestSetMemberRoles_NonCanonicalPrincipalIDIsCanonicalised(t *testing.T) {
	f := setupMMRFixture(t)
	ctx := context.Background()

	userID := tid(t.Name() + "-user")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: userID, Email: userID + "@test.com", DisplayName: "Canon", Role: "member", Status: "active",
	}))
	agentID := tid(t.Name() + "-agent")
	require.NoError(t, f.store.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: agentID, Name: "canon-agent", ProjectID: f.projectID,
		Phase: "running", CreatedBy: f.owner.ID, OwnerID: f.owner.ID, Ancestry: []string{f.owner.ID},
	}))

	for principalType, id := range map[string]string{"user": userID, "agent": agentID} {
		upper := strings.ToUpper(id)
		variants := []string{
			"urn:uuid:" + id,
			"{" + id + "}",
			strings.ReplaceAll(id, "-", ""),
		}

		assertCanonical := func(rec int, body []byte, wantStatus int, label string) {
			t.Helper()
			require.Equal(t, wantStatus, rec, "%s %s: %s", principalType, label, body)
			var resp projectMemberGroupMutationResponse
			require.NoError(t, json.Unmarshal(body, &resp), string(body))
			assert.Equal(t, id, resp.PrincipalID, "%s %s: principalId", principalType, label)
			bindings := mmrBindingsFor(t, f.store, principalType, id, f.projectID)
			require.Len(t, bindings, 1, "%s %s: one binding under the canonical ID", principalType, label)
			assert.Equal(t, f.memberRD.ID, bindings[0].RoleDefinitionID)
		}

		rec := putMemberRoles(t, f.srv, f.owner, f.projectID, principalType, upper, []string{f.memberRD.ID}, nil)
		assertCanonical(rec.Code, rec.Body.Bytes(), http.StatusCreated, "PUT upper")
		for _, v := range variants {
			// The same principal already holds member, so each further
			// spelling is an idempotent no-op on that binding, not a create.
			rec = putMemberRoles(t, f.srv, f.owner, f.projectID, principalType, v, []string{f.memberRD.ID}, nil)
			assertCanonical(rec.Code, rec.Body.Bytes(), http.StatusOK, "PUT "+v)
		}
		for _, raw := range append([]string{upper}, variants...) {
			assert.Empty(t, mmrBindingsFor(t, f.store, principalType, raw, f.projectID), "%s: no binding stored under %q", principalType, raw)
		}

		rec = deleteMemberRoles(t, f.srv, f.owner, f.projectID, principalType, upper)
		require.Equal(t, http.StatusNoContent, rec.Code, "%s DELETE upper: %s", principalType, rec.Body.String())
		assert.Empty(t, mmrBindingsFor(t, f.store, principalType, id, f.projectID), "%s: DELETE by the upper-case ID removes the binding", principalType)
	}
}
