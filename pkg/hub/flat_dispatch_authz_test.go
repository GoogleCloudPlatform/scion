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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Dispatch authorization for a Runtime Broker row that stores a runtime
// target descriptor: such a row is decided by canDispatchToBroker alone
// (broker.dispatch), never by the owner-consented provider arm of
// canUseBrokerForProject.

// flatDispatchVariant returns a copy of the fixture's flat row with the given
// descriptor and AutoProvide value (same ID, so the same provider link).
func flatDispatchVariant(f *flatHubFixture, desc *api.RuntimeTargetDescriptor, autoProvide bool) *store.RuntimeBroker {
	b := *f.flat
	b.RuntimeTarget = desc
	b.AutoProvide = autoProvide
	return &b
}

// A flat create that the owner-consented provider relationship would admit
// is refused with 403 when the caller lacks broker.dispatch.
func TestFlatDispatchAuthz_ConsentedProviderDoesNotAdmitFlatCreate(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	member := flatMemberUser(t, f.s, f.project.ID, "flat-consent-member")
	require.False(t, f.flat.AutoProvide, "precondition: the flat row is not AutoProvide")
	require.True(t, f.srv.brokerProviderHasOwnerConsent(context.Background(), f.flat, f.project.ID),
		"precondition: the flat row is an owner-consented provider of the project")

	// Passes every flat check (experiment on, matching target, no profile).
	body := map[string]interface{}{"name": "consent-flat", "runtimeBrokerId": f.flat.ID, "task": "t",
		"expectedRuntimeTargetId": f.flat.RuntimeTarget.ID}
	rec := doRequestAsUser(t, f.srv, member, http.MethodPost, "/api/v1/projects/"+f.project.ID+"/agents", body)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	f.noAgentWritten(t, "consent-flat")
}

// Any stored runtime target descriptor takes the strict path: a flat row, a
// row whose target type is unknown, and a descriptor without an ID. The same
// row without a descriptor (legacy) is still admitted by the consent arm.
func TestFlatDispatchAuthz_StrictPathForAnyRuntimeTargetDescriptor(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	member := flatMemberUser(t, f.s, f.project.ID, "flat-strict-member")
	ctx := contextWithIdentity(context.Background(), userIdentityFor(member))
	require.True(t, f.srv.brokerProviderHasOwnerConsent(ctx, f.flat, f.project.ID))

	cases := []struct {
		name string
		desc *api.RuntimeTargetDescriptor
	}{
		{"flat row", f.flat.RuntimeTarget},
		{"unknown target type", &api.RuntimeTargetDescriptor{ID: f.flat.RuntimeTarget.ID, Type: "unknown-kind"}},
		{"descriptor without an ID", &api.RuntimeTargetDescriptor{Type: "docker"}},
		{"empty descriptor", &api.RuntimeTargetDescriptor{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := flatDispatchVariant(f, c.desc, false)
			assert.False(t, f.srv.canUseBrokerForProject(ctx, b, f.project), "the consent arm does not admit")
			assert.False(t, f.srv.canDispatchToBroker(ctx, b), "no broker.dispatch, no admission")
		})
	}

	legacyView := flatDispatchVariant(f, nil, false)
	assert.True(t, f.srv.canUseBrokerForProject(ctx, legacyView, f.project),
		"a row without a descriptor keeps the consent arm")
}

// The resolver's selection uses the same rule: with a legacy and a flat row
// both linked as owner-consented providers, no project default and no
// runtimeBrokerId, a member's create lands on the legacy row; the flat row
// is not selected through the consent arm.
//
// These expectations follow the current flat dispatch rule; revisit them if
// a later decision changes which callers may use a flat Runtime Broker.
func TestFlatDispatchAuthz_SelectionSkipsConsentedFlatRow(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	ctx := context.Background()
	f.project.DefaultRuntimeBrokerID = ""
	require.NoError(t, f.s.UpdateProject(ctx, f.project))
	require.True(t, f.srv.brokerProviderHasOwnerConsent(ctx, f.flat, f.project.ID))
	require.True(t, f.srv.brokerProviderHasOwnerConsent(ctx, f.legacy, f.project.ID))
	require.False(t, f.flat.AutoProvide, "precondition: the flat row is not AutoProvide")
	member := flatMemberUser(t, f.s, f.project.ID, "flat-select-member")

	body := map[string]interface{}{"name": "select-legacy", "task": "t"}
	rec := doRequestAsUser(t, f.srv, member, http.MethodPost, "/api/v1/projects/"+f.project.ID+"/agents", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	a := f.agentBySlug(t, "select-legacy")
	require.NotNil(t, a)
	assert.Equal(t, f.legacy.ID, a.RuntimeBrokerID, "the legacy row is selected")
	assert.False(t, a.IsPinned())
}

// A project whose only provider and default is a flat row the member cannot
// dispatch to: the create finds no usable Runtime Broker (422
// no_runtime_broker), not 403, and the error's list of available Runtime
// Brokers does not name the flat row.
//
// These expectations follow the current flat dispatch rule; revisit them if
// a later decision changes which callers may use a flat Runtime Broker.
func TestFlatDispatchAuthz_FlatOnlyDefaultNotUsableByMember(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
	ctx := context.Background()
	require.False(t, f.flat.AutoProvide, "precondition: the flat row is not AutoProvide")
	p := f.flatOnlyProject(t)
	p.DefaultRuntimeBrokerID = f.flat.ID
	require.NoError(t, f.s.UpdateProject(ctx, p))
	member := flatMemberUser(t, f.s, p.ID, "flat-only-member")

	body := map[string]interface{}{"name": "flat-only-default", "task": "t"}
	rec := doRequestAsUser(t, f.srv, member, http.MethodPost, "/api/v1/projects/"+p.ID+"/agents", body)
	requireAPIError(t, rec, http.StatusUnprocessableEntity, ErrCodeNoRuntimeBroker)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	raw, ok := resp.Error.Details["availableBrokers"].([]interface{})
	require.True(t, ok, "availableBrokers must be a list, got %T", resp.Error.Details["availableBrokers"])
	for _, entry := range raw {
		m, _ := entry.(map[string]interface{})
		assert.NotEqual(t, f.flat.ID, m["id"], "the error list excludes the flat row the member cannot use")
	}
	_, err := f.s.GetAgentBySlug(ctx, p.ID, "flat-only-default")
	assert.ErrorIs(t, err, store.ErrNotFound, "no agent row is written")
}

// No new grant: the set of callers admitted by checkBrokerDispatchAccess is
// the same as before the flat routing in canUseBrokerForProject was added.
// Expectations, derived by reading the code:
//   - flat rows (and a row with an unknown target type): the bc98ac2a rule,
//     canDispatchToBroker: an unauthenticated caller is denied; AutoProvide
//     admits every authenticated caller; otherwise the owner or a
//     super-admin (broker.dispatch), or an agent with agent create in a
//     project the row serves;
//   - legacy rows: upstream's canUseBrokerForProject (merged at 2d516db3):
//     the same arms plus the owner-consented provider arm (so a member is
//     admitted on a consented legacy row, which bc98ac2a's
//     canDispatchToBroker did not do; that change is upstream's).
//
// These expectations follow the current flat dispatch rule; revisit them if
// a later decision changes which callers may use a flat Runtime Broker.
func TestFlatDispatchAuthz_NoNewGrant(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("nng-project"), Name: "NNG", Slug: "nng-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	mkUser := func(name, role string) *store.User {
		u := &store.User{ID: tid("nng-" + name), Email: "nng-" + name + "@example.com", DisplayName: name, Role: role, Status: "active"}
		require.NoError(t, s.CreateUser(ctx, u))
		return u
	}
	owner := mkUser("owner", store.UserRoleMember)
	admin := mkUser("admin", store.UserRoleAdmin)
	grantSuperAdmin(t, s, admin.ID) // a system super-admin role binding (broker.dispatch everywhere)
	member := mkUser("member", store.UserRoleMember)
	createTestUserWithProjectRole(t, s, member.ID, member.Email, project.ID, store.ProjectRoleMember)
	outsider := mkUser("outsider", store.UserRoleMember)

	type brokerCase struct {
		key         string
		target      *api.RuntimeTargetDescriptor
		autoProvide bool
		linkedBy    string // "" with link=true: a link without the owner's consent
		link        bool
	}
	brokers := []brokerCase{
		{key: "legacy consented", link: true, linkedBy: owner.ID},
		{key: "legacy not consented", link: true},
		{key: "flat linked", target: &api.RuntimeTargetDescriptor{Type: "docker"}, link: true, linkedBy: owner.ID},
		{key: "flat linked AutoProvide", target: &api.RuntimeTargetDescriptor{Type: "docker"}, link: true, linkedBy: owner.ID, autoProvide: true},
		{key: "flat unlinked", target: &api.RuntimeTargetDescriptor{Type: "docker"}},
		{key: "flat unknown type", target: &api.RuntimeTargetDescriptor{Type: "unknown-kind"}, link: true, linkedBy: owner.ID},
	}
	ids := map[string]string{}
	for i, b := range brokers {
		row := &store.RuntimeBroker{ID: tid("nng-broker-" + b.key), Name: "nng-broker-" + string(rune('a'+i)), Slug: "nng-broker-" + string(rune('a'+i)),
			Status: store.BrokerStatusOnline, CreatedBy: owner.ID, AutoProvide: b.autoProvide}
		if b.target != nil {
			tgt := *b.target
			tgt.ID = tid("nng-target-" + b.key)
			row.RuntimeTarget = &tgt
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, row))
		if b.link {
			require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: project.ID, BrokerID: row.ID, BrokerName: row.Name, Status: row.Status, LinkedBy: b.linkedBy}))
		}
		stored, err := s.GetRuntimeBroker(ctx, row.ID)
		require.NoError(t, err)
		require.Equal(t, b.target != nil, stored.IsFlat(), "%s: stored flat-ness", b.key)
		require.Equal(t, b.autoProvide, stored.AutoProvide, "%s: stored AutoProvide", b.key)
		ids[b.key] = row.ID
	}

	// want lists the broker keys each caller is admitted on.
	callers := []struct {
		name string
		id   Identity
		want []string
	}{
		{"broker owner", userIdentityFor(owner), []string{"legacy consented", "legacy not consented", "flat linked", "flat linked AutoProvide", "flat unlinked", "flat unknown type"}},
		{"super-admin", userIdentityFor(admin), []string{"legacy consented", "legacy not consented", "flat linked", "flat linked AutoProvide", "flat unlinked", "flat unknown type"}},
		{"project member", userIdentityFor(member), []string{"legacy consented", "flat linked AutoProvide"}},
		{"member of no project", userIdentityFor(outsider), []string{"legacy consented", "flat linked AutoProvide"}},
		{"agent in the project with agent create", agentIdentityFor("nng-agent-in", project.ID, ScopeAgentCreate), []string{"legacy consented", "legacy not consented", "flat linked", "flat linked AutoProvide", "flat unknown type"}},
		{"agent in the project without agent create", agentIdentityFor("nng-agent-noscope", project.ID), []string{"flat linked AutoProvide"}},
		{"agent of another project", agentIdentityFor("nng-agent-out", tid("nng-other-project"), ScopeAgentCreate), []string{"flat linked AutoProvide"}},
		{"a Runtime Broker identity", NewBrokerIdentity(ids["flat linked"]), []string{"flat linked AutoProvide"}},
	}
	for _, c := range callers {
		want := map[string]bool{}
		for _, k := range c.want {
			want[k] = true
		}
		for _, b := range brokers {
			t.Run(c.name+"/"+b.key, func(t *testing.T) {
				rec := httptest.NewRecorder()
				got := srv.checkBrokerDispatchAccess(contextWithIdentity(ctx, c.id), rec, ids[b.key], project)
				assert.Equal(t, want[b.key], got, "admission")
				if !got {
					assert.Equal(t, http.StatusForbidden, rec.Code)
				}
			})
		}
	}
	// An unauthenticated caller is admitted on none of them.
	for _, b := range brokers {
		assert.False(t, srv.checkBrokerDispatchAccess(ctx, httptest.NewRecorder(), ids[b.key], project), "unauthenticated: %s", b.key)
	}
}
