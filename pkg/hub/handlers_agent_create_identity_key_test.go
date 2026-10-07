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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func createAgentPath(f *projectAgentAuthzFixture) string {
	return "/api/v1/projects/" + f.project.ID + "/agents"
}

// attachAutoProvideBroker gives f.project an auto-provide runtime broker, the
// same way authz_bypass_agents_test.go's bypassAgentsSetup does, so create
// requests in this file resolve a broker instead of failing at broker
// selection before they ever reach identity-key validation.
func attachAutoProvideBroker(t *testing.T, f *projectAgentAuthzFixture) {
	t.Helper()
	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID:          uuid.New().String(),
		Name:        "identity-key-test-broker",
		Slug:        "identity-key-test-broker",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  f.project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	f.project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, f.store.UpdateProject(ctx, f.project))
}

// TestCreateAgentInProject_WritesSlugIdentityKey: a successful create writes
// an identity-key row for the new agent's Slug (Name defaults to Slug at
// create, so distinct({Slug, Slugify(Name)}) collapses to that one key).
func TestCreateAgentInProject_WritesSlugIdentityKey(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	attachAutoProvideBroker(t, f)
	ctx := context.Background()

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, createAgentPath(f),
		CreateAgentRequest{Name: "Fresh Recruit"})
	require.Equal(t, http.StatusCreated, rec.Code, "create body: %s", rec.Body.String())

	created, err := f.store.GetAgentBySlug(ctx, f.project.ID, "fresh-recruit")
	require.NoError(t, err)

	keys, err := f.store.ListAgentIdentityKeys(ctx, f.project.ID)
	require.NoError(t, err)
	require.NotNil(t, findIdentityKey(keys, created.ID, "fresh-recruit"),
		"expected an identity-key row for the new agent's slug")
}

// TestCreateAgentInProject_RejectsReservedSlug: a create whose slug is
// itself a reserved word is rejected the same way applyAgentUpdate rejects
// a PATCH to a reserved display name. Asserts the specific error code, not
// just the status, so the errInvalidDisplayName -> invalid_name mapping is
// pinned end-to-end at the HTTP layer too.
func TestCreateAgentInProject_RejectsReservedSlug(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	attachAutoProvideBroker(t, f)
	ctx := context.Background()

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, createAgentPath(f),
		CreateAgentRequest{Name: "Admin"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "create body: %s", rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	require.Equal(t, "invalid_name", errResp.Error.Code, "create body: %s", rec.Body.String())

	_, err := f.store.GetAgentBySlug(ctx, f.project.ID, "admin")
	require.ErrorIs(t, err, store.ErrNotFound, "no agent may have been created")
}

// TestCreateAgentInProject_OrderingCollisionIsRejected is the ordering
// regression: renaming an existing agent to a display name reserves
// that name's key, so a later create whose Slug happens to match that same
// key must be rejected too, not just the reverse (a later rename colliding
// with an existing agent's Slug, which TestApplyAgentUpdate_DisplayNameCollisionIsRejected
// already covers). The order these two agents' names were set in must not
// change the outcome.
func TestCreateAgentInProject_OrderingCollisionIsRejected(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	attachAutoProvideBroker(t, f)
	ctx := context.Background()

	renameRec := doRequestAsUser(t, f.srv, f.member, http.MethodPatch, f.targetPath(),
		map[string]interface{}{"name": "Widget Bot"})
	require.Equal(t, http.StatusOK, renameRec.Code, "PATCH body: %s", renameRec.Body.String())

	createRec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, createAgentPath(f),
		CreateAgentRequest{Name: "Widget Bot"})
	require.Equal(t, http.StatusConflict, createRec.Code, "create body: %s", createRec.Body.String())

	_, err := f.store.GetAgentBySlug(ctx, f.project.ID, "widget-bot")
	require.ErrorIs(t, err, store.ErrNotFound, "no second agent may have been created")
}

// TestCreateAgentInProject_ResumesPreExistingReservedSlugAgent is the
// regression test for the reserved-word check's placement: the check must
// only apply to a value that is about to be written as a brand new agent's
// identity key. A pre-existing stopped agent whose Slug already happens to
// be a reserved word (allowed before this change, since ValidateAgentName
// alone never checked for one) must still be resumable by name through this
// same create-or-start endpoint.
func TestCreateAgentInProject_ResumesPreExistingReservedSlugAgent(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	attachAutoProvideBroker(t, f)
	ctx := context.Background()

	existing := &store.Agent{
		ID: uuid.New().String(), Slug: "hub", Name: "hub",
		ProjectID: f.project.ID, Phase: string(state.PhaseStopped),
		RuntimeBrokerID: f.project.DefaultRuntimeBrokerID,
		CreatedBy:       f.member.ID, OwnerID: f.member.ID,
		AppliedConfig: &store.AgentAppliedConfig{InlineConfig: &api.ScionConfig{}},
	}
	require.NoError(t, f.store.CreateAgent(ctx, existing))

	// The test environment has no dispatcher wired, so a resume that gets
	// past the existing-agent lookup still hits "no runtime broker
	// available" trying to dispatch -- a different 400 than the one this
	// test is pinning. The discriminator is the reserved-word rejection
	// itself, which must be absent regardless of how the resume attempt
	// otherwise concludes.
	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, createAgentPath(f),
		CreateAgentRequest{Name: "hub", Resume: true})
	require.False(t, strings.Contains(rec.Body.String(), "reserved word"),
		"resume must not be blocked by the reserved-word check: %d %s", rec.Code, rec.Body.String())
}

// TestCommitAgentCreate_FKViolationIsNotDisplayNameError guards the
// distinction commitAgentCreate draws between two different causes
// of store.ErrInvalidInput: a genuine display-name validation failure, and a
// foreign-key violation from CreateAgent itself (for example, the project
// row disappearing under a concurrent delete). Only the former may satisfy
// errors.Is(err, errInvalidDisplayName) -- the HTTP create path keys its
// invalid_name mapping on exactly that sentinel, specifically so that a
// store-layer error whose text was never meant for an API client (it can
// include raw constraint/SQL detail) cannot be surfaced as if it were one.
func TestCommitAgentCreate_FKViolationIsNotDisplayNameError(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID:            uuid.New().String(),
		Slug:          "fk-violation-agent",
		Name:          "fk-violation-agent",
		ProjectID:     uuid.New().String(), // no such project: CreateAgent's FK write fails
		Phase:         string(state.PhaseStopped),
		CreatedBy:     f.member.ID,
		OwnerID:       f.member.ID,
		AppliedConfig: &store.AgentAppliedConfig{InlineConfig: &api.ScionConfig{}},
	}

	err := f.srv.commitAgentCreate(ctx, agentCreateWrite{
		Ceiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		Provenance: store.AuthorityProvenance{
			ProvenanceVersion:    store.ProvenanceVersionV1,
			SourcePrincipalKind:  store.DelegationPrincipalUser,
			SourcePrincipalID:    f.member.ID,
			SourceCredentialKind: store.SourceCredentialSession,
		},
		Agent: agent,
		Slug:  agent.Slug,
		Edge: &store.DelegationEdge{
			DelegatorType: store.DelegationPrincipalUser, DelegatorID: f.member.ID,
			DelegateType: store.DelegationPrincipalAgent, ScopeType: store.RoleScopeProject,
			ScopeID: agent.ProjectID, Role: string(AgentRoleNone), Active: true,
		},
		Audit: &store.MutationAuditRecord{MutationType: mutationTypeAgentDelegation, CanDelegateResult: "allow"},
	})
	require.Error(t, err)
	require.False(t, errors.Is(err, errInvalidDisplayName),
		"a foreign-key violation must not be reported as a display-name validation failure: %v", err)
	require.True(t, errors.Is(err, store.ErrInvalidInput),
		"expected the store layer's existing foreign-key-violation mapping to still apply: %v", err)
}

// TestCreateAgentInProject_AcceptsDiacriticsDigitsAndSeparators confirms a
// name built from characters outside plain ASCII letters -- Latin letters
// with diacritics, digits, space, and '-_.' -- still succeeds at create.
// Unlike PATCH, a create's req.Name is only ever used to derive Slug (Name
// is set to the resulting Slug, not to the raw input -- see
// createAgentInProject), so create has no separate raw display value for
// the charset/length/control-character rules to apply to; this test exists
// to confirm that collapsing step itself doesn't reject legitimate
// non-ASCII input along the way.
func TestCreateAgentInProject_AcceptsDiacriticsDigitsAndSeparators(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	attachAutoProvideBroker(t, f)
	ctx := context.Background()

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, createAgentPath(f),
		CreateAgentRequest{Name: "Café Böt_1.2 Prime"})
	require.Equal(t, http.StatusCreated, rec.Code, "create body: %s", rec.Body.String())

	created, err := f.store.GetAgentBySlug(ctx, f.project.ID, "cafe-bot-1-2-prime")
	require.NoError(t, err)

	keys, err := f.store.ListAgentIdentityKeys(ctx, f.project.ID)
	require.NoError(t, err)
	require.NotNil(t, findIdentityKey(keys, created.ID, "cafe-bot-1-2-prime"))
}
