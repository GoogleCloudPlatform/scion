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
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Project members may have agents dispatched to a broker that is associated
// with their project with its owner's consent. A provider row carries
// consent when the broker has no owner, or LinkedBy is the broker's owner,
// "auto-provide", or an active user who is a current super-admin.

// newUsageBroker creates an online, non-auto-provide broker owned by ownerID
// and, when linkedBy is not nil, links it to projectID with that LinkedBy.
func newUsageBroker(t *testing.T, s store.Store, ownerID, projectID string, linkedBy *string) *store.RuntimeBroker {
	t.Helper()
	ctx := context.Background()
	b := &store.RuntimeBroker{
		ID:        uuid.New().String(),
		Name:      "usage-" + uuid.New().String()[:8],
		Slug:      "usage-" + uuid.New().String()[:8],
		Status:    store.BrokerStatusOnline,
		CreatedBy: ownerID,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, b))
	if linkedBy != nil {
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID:  projectID,
			BrokerID:   b.ID,
			BrokerName: b.Name,
			Status:     store.BrokerStatusOnline,
			LinkedBy:   *linkedBy,
		}))
	}
	return b
}

func decodeCreatedAgentBroker(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp CreateAgentResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotNil(t, resp.Agent)
	return resp.Agent.RuntimeBrokerID
}

func TestBrokerUsage_MemberUsesOwnerLinkedBrokerExplicitly(t *testing.T) {
	f := brokerLinkAuthzSetup(t)
	b := newUsageBroker(t, f.store, f.owner.ID, f.proj.ID, strPtr(f.owner.ID))

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
		CreateAgentRequest{Name: "usage-explicit", RuntimeBrokerID: b.ID})

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, b.ID, decodeCreatedAgentBroker(t, rec))
}

func TestBrokerUsage_MemberUsesOwnerLinkedDefaultBroker(t *testing.T) {
	f := brokerLinkAuthzSetup(t)
	b := newUsageBroker(t, f.store, f.owner.ID, f.proj.ID, strPtr(f.owner.ID))
	f.proj.DefaultRuntimeBrokerID = b.ID
	require.NoError(t, f.store.UpdateProject(context.Background(), f.proj))

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
		CreateAgentRequest{Name: "usage-default"})

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, b.ID, decodeCreatedAgentBroker(t, rec))
}

func TestBrokerUsage_ProviderWithoutConsentEvidenceStaysOwnerOnly(t *testing.T) {
	for _, linkedBy := range []string{"", "agent-create"} {
		t.Run("linkedBy="+linkedBy, func(t *testing.T) {
			f := brokerLinkAuthzSetup(t)
			b := newUsageBroker(t, f.store, f.owner.ID, f.proj.ID, strPtr(linkedBy))

			rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
				CreateAgentRequest{Name: "usage-noconsent-explicit", RuntimeBrokerID: b.ID})
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

			f.proj.DefaultRuntimeBrokerID = b.ID
			require.NoError(t, f.store.UpdateProject(context.Background(), f.proj))
			rec = doRequestAsUser(t, f.srv, f.member, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
				CreateAgentRequest{Name: "usage-noconsent-default"})
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())

			for _, name := range []string{"usage-noconsent-explicit", "usage-noconsent-default"} {
				_, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, name)
				assert.True(t, errors.Is(err, store.ErrNotFound), "no agent %q expected, got %v", name, err)
			}

			// The owner holds broker.dispatch and may use it.
			rec = createAgentAsOwner(t, f.bypassAgentsFixture, CreateAgentRequest{
				Name: "usage-noconsent-owner", RuntimeBrokerID: b.ID,
			})
			assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		})
	}
}

func TestBrokerUsage_MemberCannotUseBrokerThatIsNotAProvider(t *testing.T) {
	f := brokerLinkAuthzSetup(t)
	b := newUsageBroker(t, f.store, f.owner.ID, f.other.ID, strPtr(f.owner.ID))

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
		CreateAgentRequest{Name: "usage-not-provider", RuntimeBrokerID: b.ID})

	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assertNoProvider(t, f.store, f.proj.ID, b.ID)
}

func TestBrokerUsage_AutoProvideBrokerOpenToMembers(t *testing.T) {
	f := brokerLinkAuthzSetup(t)
	// An owned broker whose provider row carries no consent evidence, so
	// only its auto-provide setting opens it to members.
	b := newUsageBroker(t, f.store, f.owner.ID, f.proj.ID, strPtr("agent-create"))
	require.False(t, f.srv.brokerProviderHasOwnerConsent(context.Background(), b, f.proj.ID))
	b.AutoProvide = true
	require.NoError(t, f.store.UpdateRuntimeBroker(context.Background(), b))

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
		CreateAgentRequest{Name: "usage-auto-provide", RuntimeBrokerID: b.ID})

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, b.ID, decodeCreatedAgentBroker(t, rec))
}

func TestBrokerUsage_ProjectTokenUsesOwnerLinkedBroker(t *testing.T) {
	f := brokerLinkAuthzSetup(t)
	b := newUsageBroker(t, f.store, f.owner.ID, f.proj.ID, strPtr(f.owner.ID))
	uatCtx := contextWithIdentity(context.Background(),
		NewScopedUserIdentity(authUser(f.member), f.proj.ID, []string{"agent:create"}))

	rec := httptest.NewRecorder()
	assert.True(t, f.srv.checkBrokerDispatchAccess(uatCtx, rec, b.ID, f.proj), rec.Body.String())

	rec = httptest.NewRecorder()
	assert.False(t, f.srv.checkBrokerDispatchAccess(uatCtx, rec, b.ID, f.other))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestBrokerUsage_ImageOperationsStillNeedBrokerDispatch(t *testing.T) {
	f := brokerLinkAuthzSetup(t)
	b := newUsageBroker(t, f.store, f.owner.ID, f.proj.ID, strPtr(f.owner.ID))
	memberCtx := contextWithIdentity(context.Background(), authUser(f.member))

	assert.True(t, f.srv.canUseBrokerForProject(memberCtx, b, f.proj))
	assert.False(t, f.srv.canDispatchToBroker(memberCtx, b),
		"operations without a project keep the broker.dispatch rule")
}

func TestBrokerProviderHasOwnerConsent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("consent-project")
	ownerID := tid("consent-owner")
	createRS1Project(t, s, projectID, ownerID)
	admin := newSuperAdminUser(t, s, "consent-admin")
	member := newHubMemberUser(t, s, "consent-member")
	suspended := newSuperAdminUser(t, s, "consent-suspended-admin")
	suspended.Status = store.UserStatusSuspended
	require.NoError(t, s.UpdateUser(ctx, suspended))

	cases := []struct {
		name     string
		owner    string
		linkedBy *string
		want     bool
	}{
		{"linked by the owner", ownerID, strPtr(ownerID), true},
		{"linked by auto-provide", ownerID, strPtr(autoProvideLinkedBy), true},
		{"linked by a super-admin", ownerID, strPtr(admin.ID), true},
		{"linked by a suspended super-admin", ownerID, strPtr(suspended.ID), false},
		{"linked by an unknown user", ownerID, strPtr(tid("consent-unknown-user")), false},
		{"ownerless broker, empty linkedBy", "", strPtr(""), true},
		{"ownerless broker, placeholder linkedBy", "", strPtr("agent-create"), true},
		{"empty linkedBy", ownerID, strPtr(""), false},
		{"placeholder linkedBy", ownerID, strPtr("agent-create"), false},
		{"linked by another member", ownerID, strPtr(member.ID), false},
		{"not a provider", ownerID, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newUsageBroker(t, s, tc.owner, projectID, tc.linkedBy)
			assert.Equal(t, tc.want, srv.brokerProviderHasOwnerConsent(ctx, b, projectID))
		})
	}

	t.Run("linker whose super-admin binding is removed", func(t *testing.T) {
		former := newSuperAdminUser(t, s, "consent-removed-admin")
		b := newUsageBroker(t, s, ownerID, projectID, strPtr(former.ID))
		require.True(t, srv.brokerProviderHasOwnerConsent(ctx, b, projectID))

		removed, err := s.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, former.ID)
		require.NoError(t, err)
		require.NotZero(t, removed)
		assert.False(t, srv.brokerProviderHasOwnerConsent(ctx, b, projectID),
			"a super-admin linker counts only while the linker is a super-admin")
	})
}
