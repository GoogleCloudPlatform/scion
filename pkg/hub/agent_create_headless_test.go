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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/labels"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// headlessGatherBrokerClient records CreateAgentWithGather calls (including the
// second-pass finalize replay) and DeleteAgent calls for headless-create tests.
type headlessGatherBrokerClient struct {
	mockRuntimeBrokerClient

	mu              sync.Mutex
	calls           []*RemoteCreateAgentRequest
	firstPassReqs   *RemoteEnvRequirementsResponse
	secondPassReqs  *RemoteEnvRequirementsResponse
	deleteAgentIDs  []string
	createdAgentIDs []string
}

func (c *headlessGatherBrokerClient) CreateAgentWithGather(_ context.Context, brokerID, endpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, req)
	c.createdAgentIDs = append(c.createdAgentIDs, req.ID)
	c.lastBrokerID = brokerID
	c.lastEndpoint = endpoint
	c.lastCreateReq = req

	if c.returnErr != nil {
		return nil, nil, c.returnErr
	}
	if len(c.calls) == 1 && c.firstPassReqs != nil {
		return nil, c.firstPassReqs, nil
	}
	if len(c.calls) > 1 && c.secondPassReqs != nil {
		return nil, c.secondPassReqs, nil
	}
	return &RemoteAgentResponse{
		Agent: &RemoteAgentInfo{
			ID:     req.ID,
			Slug:   req.Slug,
			Name:   req.Name,
			Phase:  "running",
			Status: "running",
		},
		Created: true,
	}, nil, nil
}

func (c *headlessGatherBrokerClient) DeleteAgent(_ context.Context, _, _, agentID, _ string, _ DeleteAgentOptions) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleteAgentIDs = append(c.deleteAgentIDs, agentID)
	c.deleteCalled = true
	return nil
}

func installHeadlessGatherDispatcher(t *testing.T, f *schedFire) *headlessGatherBrokerClient {
	t.Helper()
	client := &headlessGatherBrokerClient{}
	backend := secret.NewLocalBackend(f.store, "test-hub-id", "test-secret")
	f.srv.SetSecretBackend(backend)
	disp := NewHTTPAgentDispatcherWithClient(f.store, client, false, slog.Default())
	disp.SetTokenGenerator(f.srv)
	disp.SetSecretBackend(backend)
	disp.SetHubID("test-hub-id")
	disp.SetAuthzService(f.srv.authzService)
	f.srv.SetDispatcher(disp)
	f.srv.registerFireActions()
	return client
}

func fireHeadlessDispatch(srv *Server, evt store.ScheduledEvent, fireEvt *hubfire.FireEvent) hubfire.FireOutcome {
	defKind := hubfire.DefinitionKindScheduledEvent
	defID := evt.ID
	if evt.ScheduleID != "" {
		defKind = hubfire.DefinitionKindSchedule
		defID = evt.ScheduleID
	}
	return srv.fireAction(context.Background(), hubfire.FireRequest{
		ProjectID: evt.ProjectID,
		Authority: hubfire.RecordedAuthority{
			Attribution:    evt.InitiatorAttribution,
			Ceiling:        evt.AuthorityCeiling,
			DefinitionKind: defKind,
			DefinitionID:   defID,
			FireID:         evt.ID,
		},
		ActionType: "dispatch_agent",
		Payload:    evt.Payload,
		FireAt:     evt.FireAt,
		Event:      fireEvt,
		Executor:   hubfire.ExecutorContext{Kind: "scheduler", ID: evt.ID},
	})
}

func resolvedSecretMap(secrets []ResolvedSecret) map[string]ResolvedSecret {
	out := make(map[string]ResolvedSecret, len(secrets))
	for _, rs := range secrets {
		out[rs.Name] = rs
	}
	return out
}

// TestEnvGather_HeadlessDefaultGathers covers Stage 1 §5.7 constraint 6 test 3
// and headless-identity-note.md §2–§4:
//   - default headless create dispatches through DispatchAgentCreateWithGather
//     (GatherEnv == true, NoAuth == false) and resolves as_needed keys on the
//     second pass;
//   - remaining unsatisfied Needs fail the fire with content-free
//     "missing required environment variables (N)", delete runtime files,
//     revoke minted credentials, compensate the agent row + edge, and record
//     sorted key names (never values) in audit and slog;
//   - explicit payload.noAuth: true sets req.NoAuth and withholds secrets.
func TestEnvGather_HeadlessDefaultGathers(t *testing.T) {
	// Not t.Parallel: subtest captures process-default slog logger.
	ctx := context.Background()

	t.Run("default_gathers_and_resolves_as_needed_secret", func(t *testing.T) {
		f := newSchedFire(t, "hg-twopass")
		client := installHeadlessGatherDispatcher(t, f)

		require.NoError(t, f.store.CreateSecret(ctx, &store.Secret{
			ID:             api.NewUUID(),
			Key:            "AS_NEEDED_TOKEN",
			EncryptedValue: "as-needed-secret-val",
			SecretType:     store.SecretTypeEnvironment,
			Target:         "AS_NEEDED_TOKEN",
			Scope:          store.ScopeUser,
			ScopeID:        f.creator.ID,
			InjectionMode:  store.InjectionModeAsNeeded,
		}))
		client.firstPassReqs = &RemoteEnvRequirementsResponse{
			AgentID:  "pending",
			Required: []string{"AS_NEEDED_TOKEN"},
			Needs:    []string{"AS_NEEDED_TOKEN"},
		}

		evt := withSessionRevision(f.event("gather-ok"), f.creator.ID)
		out := fireHeadlessDispatch(f.srv, evt, nil)
		require.Equal(t, hubfire.FireStatusFired, out.Status, "unexpected error: %s", out.Error)
		require.NotEmpty(t, out.ResultRef)

		child, _ := f.child(t, "gather-ok")
		assert.Equal(t, child.ID, out.ResultRef)
		assert.False(t, child.AppliedConfig.NoAuth)

		client.mu.Lock()
		require.Len(t, client.calls, 2, "expected initial gather pass + finalize replay")
		firstReq := client.calls[0]
		secondReq := client.calls[1]
		client.mu.Unlock()

		assert.True(t, firstReq.GatherEnv)
		assert.False(t, firstReq.NoAuth)
		assert.Contains(t, firstReq.AvailableAsNeededKeys, "AS_NEEDED_TOKEN")
		assert.Equal(t, "as-needed-secret-val", secondReq.ResolvedEnv["AS_NEEDED_TOKEN"])
	})

	t.Run("remaining_needs_compensates_revokes_and_redacts", func(t *testing.T) {
		var logBuf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(prev) })

		f := newSchedFire(t, "hg-missing")
		client := installHeadlessGatherDispatcher(t, f)

		const sentinelSecretValue = "SUPER_SECRET_SENTINEL_9921_DO_NOT_LEAK"
		require.NoError(t, f.store.CreateSecret(ctx, &store.Secret{
			ID:             api.NewUUID(),
			Key:            "PRESENT_API_KEY",
			EncryptedValue: sentinelSecretValue,
			SecretType:     store.SecretTypeEnvironment,
			Target:         "PRESENT_API_KEY",
			Scope:          store.ScopeUser,
			ScopeID:        f.creator.ID,
			InjectionMode:  store.InjectionModeAlways,
		}))
		// Return two unsorted missing keys so sorting and count formatting are verified.
		client.firstPassReqs = &RemoteEnvRequirementsResponse{
			AgentID:  "pending",
			Required: []string{"PRESENT_API_KEY", "MISSING_BETA", "MISSING_ALPHA"},
			Needs:    []string{"MISSING_BETA", "MISSING_ALPHA"},
		}

		evt := withSessionRevision(f.event("gather-missing"), f.creator.ID)
		out := fireHeadlessDispatch(f.srv, evt, nil)
		assert.Equal(t, hubfire.FireStatusFailed, out.Status)
		assert.Equal(t, "missing required environment variables (2)", out.Error)
		assert.Empty(t, out.ResultRef)
		assert.NotContains(t, out.Error, "MISSING_ALPHA", "fire outcome error must be content-free (count only)")
		assert.NotContains(t, out.Error, sentinelSecretValue)

		client.mu.Lock()
		require.Len(t, client.calls, 1)
		mintedToken := client.calls[0].AgentToken
		createdAgentID := client.calls[0].ID
		createdSlug := client.calls[0].Slug
		deletedSlugs := slices.Clone(client.deleteAgentIDs)
		client.mu.Unlock()

		require.NotEmpty(t, createdAgentID)
		require.NotEmpty(t, mintedToken)
		assert.Contains(t, deletedSlugs, createdSlug, "broker DeleteAgent must be called to remove runtime files")

		// Verify minted agent JWT was revoked on missing-env rollback.
		claims, err := f.srv.GetAgentTokenService().ValidateAgentToken(mintedToken)
		require.NoError(t, err)
		cred := getTestAgentCredential(t, f.store, claims.ID)
		require.NotNil(t, cred.RevokedAt, "minted agent credential must be revoked when create fails on missing env")

		// Verify agent row + delegation edge compensation and audit summary.
		sum := assertCompensated(t, f.store, createdAgentID)
		assert.Equal(t, createStageMissingEnv, sum.Stage)
		assert.Equal(t, "missing required environment variables: MISSING_ALPHA, MISSING_BETA", sum.Error)

		// Verify audit and slog never record the sentinel secret value.
		audits, _, err := f.store.ListMutationAudits(ctx, store.MutationAuditFilter{TargetID: createdAgentID})
		require.NoError(t, err)
		for _, rec := range audits {
			assert.NotContains(t, rec.AfterSummary, sentinelSecretValue)
			assert.NotContains(t, rec.BeforeSummary, sentinelSecretValue)
		}
		logs := logBuf.String()
		assert.Contains(t, logs, "MISSING_ALPHA")
		assert.Contains(t, logs, "MISSING_BETA")
		assert.NotContains(t, logs, sentinelSecretValue)
	})

	t.Run("explicit_noAuth_opt_out_sets_req_noAuth_and_withholds_secrets", func(t *testing.T) {
		f := newSchedFire(t, "hg-noauth")
		client := installHeadlessGatherDispatcher(t, f)

		require.NoError(t, f.store.CreateSecret(ctx, &store.Secret{
			ID:             api.NewUUID(),
			Key:            "USER_API_KEY",
			EncryptedValue: "top-secret-user-val",
			SecretType:     store.SecretTypeEnvironment,
			Target:         "USER_API_KEY",
			Scope:          store.ScopeUser,
			ScopeID:        f.creator.ID,
			InjectionMode:  store.InjectionModeAlways,
		}))

		evt := f.event("gather-noauth-child")
		evt.Payload = `{"agentName":"gather-noauth-child","noAuth":true}`
		evt = withSessionRevision(evt, f.creator.ID)
		out := fireHeadlessDispatch(f.srv, evt, nil)
		require.Equal(t, hubfire.FireStatusFired, out.Status, "unexpected error: %s", out.Error)

		client.mu.Lock()
		require.Len(t, client.calls, 1)
		req := client.calls[0]
		client.mu.Unlock()

		assert.True(t, req.NoAuth)
		assert.Empty(t, req.ResolvedSecrets)
		assert.NotContains(t, req.ResolvedEnv, "USER_API_KEY")
	})
}

// TestHeadlessCreate_UserAndProgenySecrets covers Stage 1 §5.7 constraint 6
// test 1 and headless-identity-note.md §2–§3:
//   - user-authored schedule delivers both AllowProgeny=false and AllowProgeny=true
//     user secrets plus project secrets;
//   - agent-authored schedule with AgentRoleFull and recorded chain delivers
//     AllowProgeny=true user secrets and project secrets, while withholding
//     AllowProgeny=false user secrets;
//   - agent-authored schedule capped to baseline (no ScopeProjectSecretRead)
//     delivers less without failing the create (omits ScopeProjectSecretRead
//     and withholds progeny user secrets gated by it);
//   - agent-authored schedule whose parent chain is unrecorded withholds
//     AllowProgeny=true user secrets even at AgentRoleFull.
func TestHeadlessCreate_UserAndProgenySecrets(t *testing.T) {
	ctx := context.Background()
	f := newSchedFire(t, "hc-secrets")
	// Disable test-server dev-auth full-role override so role caps (e.g.
	// AgentRoleBaseline) mint their real production scopes.
	f.srv.authzService.mintDevAuthOverride = false
	client := installHeadlessGatherDispatcher(t, f)

	ownerUser := hubMemberUser(t, f.store, "hc-secrets-owner")
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, f.proj.ID, ownerUser.ID))
	ownerSessionAgent := func(t *testing.T, slug string) *store.Agent {
		t.Helper()
		a, _ := f.createdAgent(t, f.create(t, authUser(ownerUser), CreateAgentRequest{
			Name:      slug,
			AgentRole: string(AgentRoleFull),
		}), slug)
		return a
	}

	require.NoError(t, f.store.CreateSecret(ctx, &store.Secret{
		ID:             api.NewUUID(),
		Key:            "USER_PRIVATE_SECRET",
		EncryptedValue: "private-user-val",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "USER_PRIVATE_SECRET",
		Scope:          store.ScopeUser,
		ScopeID:        ownerUser.ID,
		InjectionMode:  store.InjectionModeAlways,
		AllowProgeny:   false,
	}))
	require.NoError(t, f.store.CreateSecret(ctx, &store.Secret{
		ID:             api.NewUUID(),
		Key:            "USER_SHARED_SECRET",
		EncryptedValue: "shared-progeny-val",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "USER_SHARED_SECRET",
		Scope:          store.ScopeUser,
		ScopeID:        ownerUser.ID,
		InjectionMode:  store.InjectionModeAlways,
		AllowProgeny:   true,
		CreatedBy:      ownerUser.ID,
	}))
	require.NoError(t, f.store.CreateSecret(ctx, &store.Secret{
		ID:             api.NewUUID(),
		Key:            "PROJECT_SHARED_SECRET",
		EncryptedValue: "project-secret-val",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "PROJECT_SHARED_SECRET",
		Scope:          store.ScopeProject,
		ScopeID:        f.proj.ID,
		InjectionMode:  store.InjectionModeAlways,
	}))

	t.Run("user_principal_receives_owner_and_project_secrets", func(t *testing.T) {
		evt := withSessionRevision(f.event("user-owned-child"), ownerUser.ID)
		out := fireHeadlessDispatch(f.srv, evt, nil)
		require.Equal(t, hubfire.FireStatusFired, out.Status, out.Error)

		byName := resolvedSecretMap(client.lastCreateReq.ResolvedSecrets)
		assert.Equal(t, "private-user-val", byName["USER_PRIVATE_SECRET"].Value)
		assert.Equal(t, "shared-progeny-val", byName["USER_SHARED_SECRET"].Value)
		assert.Equal(t, "project-secret-val", byName["PROJECT_SHARED_SECRET"].Value)
	})

	t.Run("agent_principal_full_role_receives_progeny_and_project_secrets_not_private", func(t *testing.T) {
		parent := ownerSessionAgent(t, "parent-full-agent")
		evt := withAgentRevision(t, f.srv, f.event("agent-progeny-full"), parent.ID)
		out := fireHeadlessDispatch(f.srv, evt, nil)
		require.Equal(t, hubfire.FireStatusFired, out.Status, out.Error)

		child, _ := f.child(t, "agent-progeny-full")
		assert.Equal(t, parent.ID, child.OwnerID)
		assert.Equal(t, []string{ownerUser.ID, parent.ID}, child.Ancestry)
		assert.Equal(t, string(AgentRoleFull), child.AppliedConfig.AgentRole)

		byName := resolvedSecretMap(client.lastCreateReq.ResolvedSecrets)
		assert.NotContains(t, byName, "USER_PRIVATE_SECRET", "AllowProgeny=false user secret must be withheld from progeny")
		assert.Equal(t, "shared-progeny-val", byName["USER_SHARED_SECRET"].Value)
		assert.Equal(t, "project-secret-val", byName["PROJECT_SHARED_SECRET"].Value)
	})

	t.Run("agent_principal_capped_to_baseline_omits_project_secret_read_scope_without_error", func(t *testing.T) {
		parent := ownerSessionAgent(t, "parent-capped-agent")
		f.setProjectAnnotation(t, projectSettingMaxAgentRole, string(AgentRoleBaseline))
		t.Cleanup(func() {
			f.setProjectAnnotation(t, projectSettingMaxAgentRole, string(AgentRoleFull))
		})

		evt := withAgentRevision(t, f.srv, f.event("agent-progeny-baseline"), parent.ID)
		out := fireHeadlessDispatch(f.srv, evt, nil)
		require.Equal(t, hubfire.FireStatusFired, out.Status, out.Error)

		child, _ := f.child(t, "agent-progeny-baseline")
		assert.Equal(t, string(AgentRoleBaseline), child.AppliedConfig.AgentRole)

		claims, err := f.srv.GetAgentTokenService().ValidateAgentToken(client.lastCreateReq.AgentToken)
		require.NoError(t, err)
		assert.NotContains(t, claims.Scopes, ScopeProjectSecretRead, "baseline role must not carry ScopeProjectSecretRead")

		byName := resolvedSecretMap(client.lastCreateReq.ResolvedSecrets)
		assert.NotContains(t, byName, "USER_PRIVATE_SECRET")
		assert.NotContains(t, byName, "USER_SHARED_SECRET", "baseline lacks ScopeProjectSecretRead so progeny user secret is withheld without failing create")
		assert.Equal(t, "project-secret-val", byName["PROJECT_SHARED_SECRET"].Value, "launch-time secret.deliver remains admitted on baseline")
	})

	t.Run("agent_principal_with_unrecorded_parent_chain_withholds_progeny_user_secret", func(t *testing.T) {
		unrecordedParent := createFixtureAgent(t, f.bypassAgentsFixture, "parent-unrecorded", []string{ownerUser.ID}, AgentRoleFull)
		addProjectEdge(t, f.store, store.DelegationPrincipalUser, ownerUser.ID, unrecordedParent.ID, f.proj.ID)

		evt := withAgentRevision(t, f.srv, f.event("agent-progeny-unrecorded"), unrecordedParent.ID)
		out := fireHeadlessDispatch(f.srv, evt, nil)
		require.Equal(t, hubfire.FireStatusFired, out.Status, out.Error)

		byName := resolvedSecretMap(client.lastCreateReq.ResolvedSecrets)
		assert.NotContains(t, byName, "USER_PRIVATE_SECRET")
		assert.NotContains(t, byName, "USER_SHARED_SECRET", "unrecorded parent chain must not deliver owner progeny secrets")
	})
}

// TestHeadlessCreate_AuthorSuspendedOrUATRevokedRefused covers Stage 1 §5.7
// constraint 6 test 5: suspended user, held authoring agent, or revoked UAT
// returns FireStatusRefused, creates no agent row, and makes zero broker calls.
func TestHeadlessCreate_AuthorSuspendedOrUATRevokedRefused(t *testing.T) {
	ctx := context.Background()

	t.Run("suspended_user_refused_no_create", func(t *testing.T) {
		f := newSchedFire(t, "hc-susp-user")
		client := installHeadlessGatherDispatcher(t, f)

		u, err := f.store.GetUser(ctx, f.creator.ID)
		require.NoError(t, err)
		u.Status = store.UserStatusSuspended
		require.NoError(t, f.store.UpdateUser(ctx, u))

		evt := withSessionRevision(f.event("susp-user-child"), f.creator.ID)
		out := fireHeadlessDispatch(f.srv, evt, nil)
		assert.Equal(t, hubfire.FireStatusRefused, out.Status)
		f.assertNoChild(t, "susp-user-child")
		assert.Empty(t, client.calls)
	})

	t.Run("held_authoring_agent_refused_no_create", func(t *testing.T) {
		f := newSchedFire(t, "hc-held-agent")
		client := installHeadlessGatherDispatcher(t, f)

		parent := f.sessionAgent(t, "held-parent")
		client.mu.Lock()
		client.calls = nil
		client.mu.Unlock()

		_, err := f.store.CreateAgentHolds(ctx, []*store.AgentHold{{
			AgentID:           parent.ID,
			ProjectID:         f.proj.ID,
			Cause:             store.AgentHoldCauseOwnerAccessEnded,
			RootPrincipalType: store.AgentHoldRootUser,
			RootPrincipalID:   f.creator.ID,
			Trigger:           store.MembershipLossTriggerMemberRemove,
			ActorKind:         "system",
			ActorID:           "hub",
			CorrelationID:     "test-hold",
		}})
		require.NoError(t, err)

		evt := withAgentRevision(t, f.srv, f.event("held-agent-child"), parent.ID)
		out := fireHeadlessDispatch(f.srv, evt, nil)
		assert.Equal(t, hubfire.FireStatusRefused, out.Status)
		f.assertNoChild(t, "held-agent-child")
		assert.Empty(t, client.calls)
	})

	t.Run("revoked_uat_refused_no_create", func(t *testing.T) {
		f := newSchedFire(t, "hc-revoked-uat")
		client := installHeadlessGatherDispatcher(t, f)
		f.setProjectAnnotation(t, projectSettingDefaultAgentRole, string(AgentRoleBaseline))

		tok := f.storedUAT(t, minimalSelectors(t)...)
		require.NoError(t, f.store.RevokeUserAccessToken(ctx, tok.ID))

		evt := withUATRevision(f.event("revoked-uat-child"), tok)
		out := fireHeadlessDispatch(f.srv, evt, nil)
		assert.Equal(t, hubfire.FireStatusRefused, out.Status)
		f.assertNoChild(t, "revoked-uat-child")
		assert.Empty(t, client.calls)
	})
}

// TestHeadlessCreate_EventAttributesNeverSelectMaterial covers Stage 1 §5.7
// constraint 3 & constraint 6 test 4: event attributes and payload text naming
// an as_needed secret key have zero effect on material selection.
func TestHeadlessCreate_EventAttributesNeverSelectMaterial(t *testing.T) {
	ctx := context.Background()
	f := newSchedFire(t, "hc-attr-nomat")
	client := installHeadlessGatherDispatcher(t, f)

	require.NoError(t, f.store.CreateSecret(ctx, &store.Secret{
		ID:             api.NewUUID(),
		Key:            "UNREFERENCED_AS_NEEDED_SECRET",
		EncryptedValue: "never-selected-by-event",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "UNREFERENCED_AS_NEEDED_SECRET",
		Scope:          store.ScopeUser,
		ScopeID:        f.creator.ID,
		InjectionMode:  store.InjectionModeAsNeeded,
	}))
	require.NoError(t, f.store.CreateEnvVar(ctx, &store.EnvVar{
		ID:            api.NewUUID(),
		Key:           "UNREFERENCED_AS_NEEDED_ENV",
		Value:         "never-selected-env",
		Scope:         store.ScopeProject,
		ScopeID:       f.proj.ID,
		InjectionMode: store.InjectionModeAsNeeded,
	}))

	evt := f.event("attr-nomat-child")
	evt.Payload = `{"agentName":"attr-nomat-child","task":"UNREFERENCED_AS_NEEDED_SECRET UNREFERENCED_AS_NEEDED_ENV"}`
	evt = withSessionRevision(evt, f.creator.ID)

	fireEvt := &hubfire.FireEvent{
		ID:         "evt-webhook-1",
		ProjectID:  f.proj.ID,
		SourceKind: "webhook",
		Subject:    "UNREFERENCED_AS_NEEDED_SECRET",
		Trigger:    &hubfire.FireTriggerRef{ID: "trig-webhook-1", Name: "webhook-1"},
		Attributes: map[string]string{
			"UNREFERENCED_AS_NEEDED_SECRET": "UNREFERENCED_AS_NEEDED_SECRET",
			"secret":                        "UNREFERENCED_AS_NEEDED_SECRET",
			"env":                           "UNREFERENCED_AS_NEEDED_ENV",
		},
	}

	out := fireHeadlessDispatch(f.srv, evt, fireEvt)
	require.Equal(t, hubfire.FireStatusFired, out.Status, out.Error)

	req := client.lastCreateReq
	require.NotNil(t, req)
	assert.NotContains(t, req.ResolvedEnv, "UNREFERENCED_AS_NEEDED_SECRET")
	assert.NotContains(t, req.ResolvedEnv, "UNREFERENCED_AS_NEEDED_ENV")
	assert.NotContains(t, resolvedSecretMap(req.ResolvedSecrets), "UNREFERENCED_AS_NEEDED_SECRET")
}

// TestHeadlessCreate_InternalSecretsNeverDelivered covers Stage 1 §5.7
// constraint 2 & constraint 6 test 2: SecretTypeInternal rows at hub or project
// scope are never delivered on pass 1 and never satisfy missing env needs on
// pass 2.
func TestHeadlessCreate_InternalSecretsNeverDelivered(t *testing.T) {
	ctx := context.Background()
	f := newSchedFire(t, "hc-internal")
	client := installHeadlessGatherDispatcher(t, f)

	for _, sc := range []*store.Secret{
		{
			ID:             api.NewUUID(),
			Key:            "TRIGGER_HMAC_SECRET_HUB",
			EncryptedValue: "internal-hub-hmac",
			SecretType:     store.SecretTypeInternal,
			Target:         "TRIGGER_HMAC_SECRET_HUB",
			Scope:          store.ScopeHub,
			ScopeID:        "test-hub-id",
			InjectionMode:  store.InjectionModeAlways,
		},
		{
			ID:             api.NewUUID(),
			Key:            "TRIGGER_HMAC_SECRET_PROJ",
			EncryptedValue: "internal-project-hmac",
			SecretType:     store.SecretTypeInternal,
			Target:         "TRIGGER_HMAC_SECRET_PROJ",
			Scope:          store.ScopeProject,
			ScopeID:        f.proj.ID,
			InjectionMode:  store.InjectionModeAsNeeded,
		},
	} {
		require.NoError(t, f.store.CreateSecret(ctx, sc))
	}

	t.Run("always_internal_secret_never_delivered", func(t *testing.T) {
		evt := withSessionRevision(f.event("internal-pass1"), f.creator.ID)
		out := fireHeadlessDispatch(f.srv, evt, nil)
		require.Equal(t, hubfire.FireStatusFired, out.Status, out.Error)

		req := client.lastCreateReq
		assert.NotContains(t, req.ResolvedEnv, "TRIGGER_HMAC_SECRET_HUB")
		assert.NotContains(t, req.ResolvedEnv, "TRIGGER_HMAC_SECRET_PROJ")
		byName := resolvedSecretMap(req.ResolvedSecrets)
		assert.NotContains(t, byName, "TRIGGER_HMAC_SECRET_HUB")
		assert.NotContains(t, byName, "TRIGGER_HMAC_SECRET_PROJ")
		assert.NotContains(t, req.AvailableAsNeededKeys, "TRIGGER_HMAC_SECRET_PROJ")
	})

	t.Run("as_needed_internal_secret_never_satisfies_missing_need", func(t *testing.T) {
		client.mu.Lock()
		client.calls = nil
		client.firstPassReqs = &RemoteEnvRequirementsResponse{
			AgentID:  "pending",
			Required: []string{"TRIGGER_HMAC_SECRET_PROJ"},
			Needs:    []string{"TRIGGER_HMAC_SECRET_PROJ"},
		}
		client.mu.Unlock()

		evt := withSessionRevision(f.event("internal-pass2"), f.creator.ID)
		out := fireHeadlessDispatch(f.srv, evt, nil)
		assert.Equal(t, hubfire.FireStatusFailed, out.Status)
		assert.Equal(t, "missing required environment variables (1)", out.Error)
		f.assertNoChild(t, "internal-pass2")
	})
}

// TestHeadlessCreate_LabelsAndDataChannels covers Stage 1 §5.7 constraints 3–5
// and rev 8.28 (HeadlessCreate.Labels, EventEnv allow-list, EventFile cap &
// pre-restack error, and reserved GitHubToken zero value).
func TestHeadlessCreate_LabelsAndDataChannels(t *testing.T) {
	ctx := context.Background()
	f := newSchedFire(t, "hc-channels")
	client := installHeadlessGatherDispatcher(t, f)

	evt := withSessionRevision(f.event("channels-base"), f.creator.ID)
	auth, identity, err := f.srv.resolveRecordedAuthority(ctx, scheduledRecordedAuthority(evt), f.proj.ID, "agent.create")
	require.NoError(t, err)
	edgeCeiling, edgeProv, err := f.srv.fireEffectCeiling(ctx, auth)
	require.NoError(t, err)

	baseHC := func(name string) HeadlessCreate {
		rr := headlessResolvedRole{preCapRole: AgentRoleFull}
		return HeadlessCreate{
			ProjectID:      f.proj.ID,
			Authority:      auth,
			Identity:       identity,
			EdgeCeiling:    edgeCeiling,
			EdgeProvenance: edgeProv,
			AgentRole:      "",
			resolvedRole:   &rr,
			Name:           name,
			GitHubToken:    "",
			Detached:       true,
		}
	}

	t.Run("valid_labels_and_allowlisted_event_env_persisted_as_plain_data", func(t *testing.T) {
		hc := baseHC("channels-valid")
		hc.Labels = map[string]string{
			"scion.dev/trigger": "pr-opened",
			"env":               "staging",
		}
		hc.EventEnv = map[string]string{
			EventEnvKeyTriggerEventID:   "tevt-123",
			EventEnvKeyTriggerID:        "trig-456",
			EventEnvKeyTriggerEventFile: HeadlessEventFileContainerPath,
		}
		agent, err := f.srv.createAgentHeadless(ctx, hc)
		require.NoError(t, err)

		stored, err := f.store.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, hc.Labels, stored.Labels)
		assert.Equal(t, "tevt-123", stored.AppliedConfig.Env[EventEnvKeyTriggerEventID])
		assert.Equal(t, "trig-456", stored.AppliedConfig.Env[EventEnvKeyTriggerID])
		assert.Equal(t, HeadlessEventFileContainerPath, stored.AppliedConfig.Env[EventEnvKeyTriggerEventFile])

		req := client.lastCreateReq
		assert.Equal(t, "tevt-123", req.ResolvedEnv[EventEnvKeyTriggerEventID])
		assert.Equal(t, "trig-456", req.ResolvedEnv[EventEnvKeyTriggerID])
		assert.Equal(t, HeadlessEventFileContainerPath, req.ResolvedEnv[EventEnvKeyTriggerEventFile])
		assert.NotContains(t, resolvedSecretMap(req.ResolvedSecrets), EventEnvKeyTriggerEventID)
		assert.NotContains(t, resolvedSecretMap(req.ResolvedSecrets), EventEnvKeyTriggerID)
		assert.NotContains(t, resolvedSecretMap(req.ResolvedSecrets), EventEnvKeyTriggerEventFile)
	})

	t.Run("nil_resolved_role_fails_closed", func(t *testing.T) {
		hc := baseHC("nil-resolved-role")
		hc.resolvedRole = nil
		agent, err := f.srv.createAgentHeadless(ctx, hc)
		require.ErrorIs(t, err, errHeadlessMissingResolvedRole)
		assert.Nil(t, agent)
		f.assertNoChild(t, hc.Name)
	})

	t.Run("no_dispatcher_with_event_file_fails_and_rolls_back", func(t *testing.T) {
		broker, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
		require.NoError(t, err)
		prevCaps := broker.Capabilities
		broker.Capabilities = &store.BrokerCapabilities{HomeFiles: true}
		require.NoError(t, f.store.UpdateRuntimeBroker(ctx, broker))
		t.Cleanup(func() {
			broker.Capabilities = prevCaps
			_ = f.store.UpdateRuntimeBroker(ctx, broker)
		})

		prevDispatcher := f.srv.GetDispatcher()
		f.srv.SetDispatcher(nil)
		t.Cleanup(func() { f.srv.SetDispatcher(prevDispatcher) })

		hc := baseHC("no-disp-homefiles")
		hc.EventFile = []byte(`{"event_id":"evt-nodisp"}`)
		agent, err := f.srv.createAgentHeadless(ctx, hc)
		require.ErrorIs(t, err, errHeadlessNoDispatcherForHomeFiles)
		assert.Nil(t, agent)
		f.assertNoChild(t, hc.Name)
	})

	t.Run("invalid_labels_rejected_before_commit", func(t *testing.T) {
		tooMany := make(map[string]string, labels.MaxLabels+1)
		for i := range labels.MaxLabels + 1 {
			tooMany[strings.Repeat("k", 1)+string(rune('a'+(i%26)))+strings.Repeat("x", i/26)] = "v"
		}
		for _, tc := range []struct {
			name   string
			labels map[string]string
		}{
			{"exceeds_max_count", tooMany},
			{"value_too_long", map[string]string{"k": strings.Repeat("a", labels.MaxValueLen+1)}},
			{"non_printable_ascii", map[string]string{"k": "bad\x00val"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				hc := baseHC("lbl-bad-" + strings.ReplaceAll(tc.name, "_", "-"))
				hc.Labels = tc.labels
				_, err := f.srv.createAgentHeadless(ctx, hc)
				require.ErrorContains(t, err, "invalid labels")
				f.assertNoChild(t, hc.Name)
			})
		}
	})

	t.Run("disallowed_event_env_and_nul_byte_rejected", func(t *testing.T) {
		hcBadKey := baseHC("env-bad-key")
		hcBadKey.EventEnv = map[string]string{"ANTHROPIC_API_KEY": "injected"}
		_, err := f.srv.createAgentHeadless(ctx, hcBadKey)
		require.ErrorIs(t, err, errDisallowedHeadlessEventEnvKey)
		f.assertNoChild(t, hcBadKey.Name)

		hcNul := baseHC("env-nul-val")
		hcNul.EventEnv = map[string]string{EventEnvKeyTriggerID: "trig\x00bad"}
		_, err = f.srv.createAgentHeadless(ctx, hcNul)
		require.ErrorIs(t, err, errInvalidHeadlessEventEnvValue)
		f.assertNoChild(t, hcNul.Name)
	})

	t.Run("event_file_cap_capability_gate_and_home_files_delivery", func(t *testing.T) {
		hcTooLarge := baseHC("file-too-large")
		hcTooLarge.EventFile = bytes.Repeat([]byte("x"), maxHeadlessEventFileBytes+1)
		_, err := f.srv.createAgentHeadless(ctx, hcTooLarge)
		require.ErrorIs(t, err, errHeadlessEventFileTooLarge)
		f.assertNoChild(t, hcTooLarge.Name)

		// Empty EventFile succeeds even when the Runtime Broker does not advertise HomeFiles.
		hcEmpty := baseHC("file-empty-ok")
		hcEmpty.EventFile = []byte{}
		_, err = f.srv.createAgentHeadless(ctx, hcEmpty)
		require.NoError(t, err)
		assert.Empty(t, client.lastCreateReq.HomeFiles)

		// Non-empty EventFile is refused before agent creation when broker lacks Capabilities.HomeFiles.
		hcNoCap := baseHC("file-no-broker-cap")
		hcNoCap.EventFile = []byte(`{"id":"evt-1"}`)
		_, err = f.srv.createAgentHeadless(ctx, hcNoCap)
		require.ErrorIs(t, err, ErrBrokerLacksHomeFiles)
		f.assertNoChild(t, hcNoCap.Name)

		// Enable HomeFiles and GitHubTokenInstallationOnly capabilities on the Runtime Broker.
		broker, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
		require.NoError(t, err)
		broker.Capabilities = &store.BrokerCapabilities{HomeFiles: true, GitHubTokenInstallationOnly: true}
		require.NoError(t, f.store.UpdateRuntimeBroker(ctx, broker))

		payload := []byte(`{"id":"evt-1","secret":"USER_PRIVATE_SECRET"}`)
		hcDelivered := baseHC("file-delivered")
		hcDelivered.EventFile = payload
		created, err := f.srv.createAgentHeadless(ctx, hcDelivered)
		require.NoError(t, err)
		require.NotNil(t, created)
		require.Equal(t, []api.HomeFile{{
			Path:    HeadlessEventFilePath,
			Mode:    0o600,
			Content: payload,
		}}, client.lastCreateReq.HomeFiles)

		// Missing env alongside non-empty EventFile still compensates and returns content-free HeadlessMissingEnvError.
		client.mu.Lock()
		client.calls = nil
		client.firstPassReqs = &RemoteEnvRequirementsResponse{
			AgentID:  "pending",
			Required: []string{"MISSING_WITH_FILE"},
			Needs:    []string{"MISSING_WITH_FILE"},
		}
		client.mu.Unlock()
		hcMissingEnv := baseHC("file-missing-env")
		hcMissingEnv.EventFile = payload
		_, err = f.srv.createAgentHeadless(ctx, hcMissingEnv)
		var missingErr *HeadlessMissingEnvError
		require.ErrorAs(t, err, &missingErr)
		assert.Equal(t, 1, missingErr.Count)
		assert.Equal(t, "missing required environment variables (1)", err.Error())
		f.assertNoChild(t, hcMissingEnv.Name)
	})

	t.Run("github_token_preference_and_mismatched_edge_provenance", func(t *testing.T) {
		hcBadToken := baseHC("gh-token-invalid")
		hcBadToken.GitHubToken = "magic"
		_, err := f.srv.createAgentHeadless(ctx, hcBadToken)
		require.ErrorIs(t, err, errHeadlessInvalidGitHubToken)
		f.assertNoChild(t, hcBadToken.Name)

		hcInst := baseHC("gh-token-installation")
		hcInst.GitHubToken = DispatchAgentGitHubTokenInstallation
		agInst, err := f.srv.createAgentHeadless(ctx, hcInst)
		require.NoError(t, err)
		require.NotNil(t, agInst.AppliedConfig)
		assert.Equal(t, DispatchAgentGitHubTokenInstallation, agInst.AppliedConfig.GitHubTokenPreference)
		require.NotNil(t, agInst.AppliedConfig.CreateInputs)
		assert.Equal(t, DispatchAgentGitHubTokenInstallation, agInst.AppliedConfig.CreateInputs.GitHubTokenPreference)
		audits := agentAudits(t, f.store, mutationTypeAgentDelegation, agInst.ID)
		require.Len(t, audits, 1)
		assert.Contains(t, audits[0].AfterSummary, `"github_token_preference":"installation"`)

		hcBadID := baseHC("prov-bad-id")
		hcBadID.EdgeProvenance.SourcePrincipalID = api.NewUUID()
		_, err = f.srv.createAgentHeadless(ctx, hcBadID)
		require.ErrorIs(t, err, errHeadlessProvenancePrincipalMismatch)
		f.assertNoChild(t, hcBadID.Name)

		hcBadKind := baseHC("prov-bad-kind")
		hcBadKind.EdgeProvenance.SourcePrincipalKind = store.DelegationPrincipalAgent
		_, err = f.srv.createAgentHeadless(ctx, hcBadKind)
		require.ErrorIs(t, err, errHeadlessProvenancePrincipalMismatch)
		f.assertNoChild(t, hcBadKind.Name)

		hcBadCred := baseHC("prov-bad-cred")
		hcBadCred.EdgeProvenance.SourceCredentialKind = store.SourceCredentialUAT
		_, err = f.srv.createAgentHeadless(ctx, hcBadCred)
		require.ErrorIs(t, err, errHeadlessInvalidEdgeProvenance)
		f.assertNoChild(t, hcBadCred.Name)

		hcNoEvent := baseHC("prov-no-event")
		hcNoEvent.EdgeProvenance.SourceEventID = ""
		_, err = f.srv.createAgentHeadless(ctx, hcNoEvent)
		require.ErrorIs(t, err, errHeadlessInvalidEdgeProvenance)
		f.assertNoChild(t, hcNoEvent.Name)

		hcZeroRev := baseHC("prov-zero-rev")
		hcZeroRev.EdgeProvenance.SourceAuthorizationRevision = 0
		_, err = f.srv.createAgentHeadless(ctx, hcZeroRev)
		require.ErrorIs(t, err, errHeadlessInvalidEdgeProvenance)
		f.assertNoChild(t, hcZeroRev.Name)

		hcNoSched := baseHC("prov-no-sched")
		hcNoSched.EdgeProvenance.SourceScheduleID = ""
		_, err = f.srv.createAgentHeadless(ctx, hcNoSched)
		require.ErrorIs(t, err, errHeadlessInvalidEdgeProvenance)
		f.assertNoChild(t, hcNoSched.Name)
	})
}

// TestHeadlessCreate_ProjectDefaultsParityTable covers headless-identity-note.md
// §2 acceptance: runs interactive HTTP create and headless dispatch_agent fire
// over identical scion.io/* project annotations and asserts identical
// AppliedConfig, enforcing 100% coverage of projectSettingKeys.
func TestHeadlessCreate_ProjectDefaultsParityTable(t *testing.T) {
	ctx := context.Background()
	covered := make(map[string]bool, len(projectSettingKeys))

	seedCatalogAndSA := func(t *testing.T, f *schedFire) (*store.GCPServiceAccount, *store.Template) {
		t.Helper()
		f.withDispatcher(t)
		f.srv.registerFireActions()

		tmpl := &store.Template{
			ID:          api.NewUUID(),
			Name:        "parity-tmpl",
			Slug:        "parity-tmpl",
			Scope:       store.TemplateScopeProject,
			ScopeID:     f.proj.ID,
			Harness:     "claude",
			ContentHash: "tmpl-hash-1",
			Status:      store.TemplateStatusActive,
		}
		require.NoError(t, f.store.CreateTemplate(ctx, tmpl))

		hc := &store.HarnessConfig{
			ID:          api.NewUUID(),
			Name:        "parity-hc",
			Slug:        "parity-hc",
			Scope:       store.HarnessConfigScopeProject,
			ScopeID:     f.proj.ID,
			Harness:     "claude",
			ContentHash: "hc-hash-1",
			Status:      store.HarnessConfigStatusActive,
		}
		require.NoError(t, f.store.CreateHarnessConfig(ctx, hc))

		sa := bypassAgentsCreateSA(t, f.bypassAgentsFixture, f.proj.ID, true)
		return sa, tmpl
	}

	assertParity := func(t *testing.T, f *schedFire, slugPrefix string, anns map[string]string) {
		t.Helper()
		for k, v := range anns {
			covered[k] = true
			f.setProjectAnnotation(t, k, v)
		}

		interactiveSlug := slugPrefix + "-interactive"
		headlessSlug := slugPrefix + "-headless"

		rec := f.create(t, authUser(f.creator), CreateAgentRequest{
			Name:   interactiveSlug,
			Task:   "parity task",
			Branch: "feature/parity",
		})
		ia, _ := f.createdAgent(t, rec, interactiveSlug)

		evt := f.event(headlessSlug)
		evt.Payload = `{"agentName":"` + headlessSlug + `","task":"parity task","branch":"feature/parity"}`
		evt = withSessionRevision(evt, f.creator.ID)
		out := fireHeadlessDispatch(f.srv, evt, nil)
		require.Equal(t, hubfire.FireStatusFired, out.Status, out.Error)
		ha, _ := f.child(t, headlessSlug)

		assert.Equal(t, ia.Template, ha.Template)
		assert.Equal(t, ia.OwnerID, ha.OwnerID)
		assert.Equal(t, ia.Ancestry, ha.Ancestry)
		assert.Equal(t, ia.MessageMode, ha.MessageMode)
		assert.Equal(t, ia.AppliedConfig, ha.AppliedConfig)
	}

	t.Run("flat_broker_all_core_settings_and_broker_gcp_sa", func(t *testing.T) {
		f := newSchedFire(t, "parity-flat")
		setExperimentOverrides(t, f.srv, map[string]bool{experiments.FlatRuntimeBrokers: true})
		sa, tmpl := seedCatalogAndSA(t, f)

		flatBroker := &store.RuntimeBroker{
			ID:              tid("parity-flat-broker"),
			Name:            "parity-flat-broker",
			Slug:            "parity-flat-broker",
			Status:          store.BrokerStatusOnline,
			ConnectionState: "connected",
			Endpoint:        "http://localhost:9800",
			AutoProvide:     true,
			RuntimeTarget: &api.RuntimeTargetDescriptor{
				ID:          tid("rt-parity-flat"),
				Type:        "docker",
				DisplayName: "Docker",
			},
		}
		require.NoError(t, f.store.CreateRuntimeBroker(ctx, flatBroker))
		require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID:  f.proj.ID,
			BrokerID:   flatBroker.ID,
			BrokerName: flatBroker.Name,
			Status:     store.BrokerStatusOnline,
			LocalPath:  "/tmp/parity-flat",
		}))
		proj, err := f.store.GetProject(ctx, f.proj.ID)
		require.NoError(t, err)
		proj.DefaultRuntimeBrokerID = flatBroker.ID
		require.NoError(t, f.store.UpdateProject(ctx, proj))

		byBrokerJSON, err := json.Marshal(map[string]string{flatBroker.ID: sa.ID})
		require.NoError(t, err)

		assertParity(t, f, "flat", map[string]string{
			projectSettingDefaultTemplate:                tmpl.Slug,
			projectSettingDefaultHarnessConfig:           "parity-hc",
			projectSettingDefaultHarnessAuth:             "api-key",
			projectSettingDefaultModel:                   "claude-sonnet-4-5",
			projectSettingDefaultThinkingLevel:           "medium",
			projectSettingTelemetryEnabled:               "true",
			projectSettingAutoExposePortsEnabled:         "true",
			projectSettingActiveProfile:                  "k8s-standard",
			projectSettingDefaultMaxTurns:                "42",
			projectSettingDefaultMaxModelCalls:           "84",
			projectSettingDefaultMaxDuration:             "45m",
			projectSettingDefaultResourcesCPUReq:         "500m",
			projectSettingDefaultResourcesMemReq:         "1Gi",
			projectSettingDefaultResourcesCPULim:         "2",
			projectSettingDefaultResourcesMemLim:         "4Gi",
			projectSettingDefaultResourcesDisk:           "20Gi",
			projectSettingMaxAgentRole:                   string(AgentRoleFull),
			projectSettingDefaultAgentRole:               string(AgentRoleBaseline),
			projectSettingDefaultGCPIdentitySAIDByBroker: string(byBrokerJSON),
		})
	})

	t.Run("project_wide_gcp_identity_assign_mode", func(t *testing.T) {
		f := newSchedFire(t, "parity-gcp-proj")
		sa, _ := seedCatalogAndSA(t, f)

		assertParity(t, f, "gcp-proj", map[string]string{
			projectSettingDefaultGCPIdentityMode: store.GCPMetadataModeAssign,
			projectSettingDefaultGCPIdentitySAID: sa.ID,
		})
	})

	t.Run("hosted_profile_broker_active_profile_and_profile_gcp_sa", func(t *testing.T) {
		f := newSchedFire(t, "parity-profile")
		sa, _ := seedCatalogAndSA(t, f)

		broker, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
		require.NoError(t, err)
		broker.Profiles = []store.BrokerProfile{{
			Name:      "k8s-gpu",
			Type:      "kubernetes",
			Available: true,
		}}
		require.NoError(t, f.store.UpdateRuntimeBroker(ctx, broker))

		byProfileJSON, err := json.Marshal(map[string]string{"k8s-gpu": sa.ID})
		require.NoError(t, err)

		assertParity(t, f, "prof", map[string]string{
			projectSettingActiveProfile:                   "k8s-gpu",
			projectSettingDefaultGCPIdentitySAIDByProfile: string(byProfileJSON),
		})
	})

	assert.ElementsMatch(t, projectSettingKeys, slices.Collect(maps.Keys(covered)),
		"parity table test must exercise every scion.io/* key in projectSettingKeys")
}

// TestHeadlessCreate_ScheduleNoAuthPayloadAndPatchRevision covers
// headless-identity-note.md §2.3 & §3:
//   - convenience noAuth field on POST /schedules and POST /scheduled-events;
//   - 400 when noAuth is set on non-dispatch_agent event types;
//   - PATCH /schedules/{id} payload replacement toggling noAuth bumps
//     authorization_revision, while resending identical payload does not.
func TestHeadlessCreate_ScheduleNoAuthPayloadAndPatchRevision(t *testing.T) {
	ctx := context.Background()
	f := newSchedFire(t, "hc-sched-api")
	sched := NewScheduler(f.store, slog.Default())
	sched.srv = f.srv
	f.srv.scheduler = sched
	admin := hubMemberUser(t, f.store, "hc-sched-admin")
	grantFixtureRole(t, f.bypassAgentsFixture, admin.ID, store.ProjectRoleAdmin)

	schedURL := "/api/v1/projects/" + f.proj.ID + "/schedules"
	eventsURL := "/api/v1/projects/" + f.proj.ID + "/scheduled-events"
	caller := authUser(admin)

	t.Run("create_schedule_and_one_shot_with_noAuth_convenience_field", func(t *testing.T) {
		rec := requestAsIdentity(t, f.srv, caller, http.MethodPost, schedURL, CreateScheduleRequest{
			Name:      "nightly-noauth",
			CronExpr:  "0 3 * * *",
			EventType: "dispatch_agent",
			AgentName: "nightly-worker",
			NoAuth:    true,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var created store.Schedule
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		var p DispatchAgentEventPayload
		require.NoError(t, json.Unmarshal([]byte(created.Payload), &p))
		assert.Equal(t, "nightly-worker", p.AgentName)
		assert.True(t, p.NoAuth)

		storedSched, err := f.store.GetSchedule(ctx, created.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, storedSched.AuthorizationRevision)

		oneShotRec := requestAsIdentity(t, f.srv, caller, http.MethodPost, eventsURL, CreateScheduledEventRequest{
			EventType: "dispatch_agent",
			FireIn:    "10m",
			AgentName: "oneshot-worker",
			NoAuth:    true,
		})
		require.Equal(t, http.StatusCreated, oneShotRec.Code, oneShotRec.Body.String())
		var createdEvt store.ScheduledEvent
		require.NoError(t, json.Unmarshal(oneShotRec.Body.Bytes(), &createdEvt))
		var ep DispatchAgentEventPayload
		require.NoError(t, json.Unmarshal([]byte(createdEvt.Payload), &ep))
		assert.Equal(t, "oneshot-worker", ep.AgentName)
		assert.True(t, ep.NoAuth)
	})

	t.Run("noAuth_rejected_on_non_dispatch_agent_event_types", func(t *testing.T) {
		rec := requestAsIdentity(t, f.srv, caller, http.MethodPost, schedURL, CreateScheduleRequest{
			Name:      "bad-msg-noauth",
			CronExpr:  "0 3 * * *",
			EventType: "message",
			AgentName: "worker",
			Message:   "hi",
			NoAuth:    true,
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

		evtRec := requestAsIdentity(t, f.srv, caller, http.MethodPost, eventsURL, CreateScheduledEventRequest{
			EventType: "message",
			FireIn:    "10m",
			AgentName: "worker",
			Message:   "hi",
			NoAuth:    true,
		})
		assert.Equal(t, http.StatusBadRequest, evtRec.Code, evtRec.Body.String())

		execSchedRec := requestAsIdentity(t, f.srv, caller, http.MethodPost, schedURL, CreateScheduleRequest{
			Name:      "bad-exec-noauth",
			CronExpr:  "0 3 * * *",
			EventType: "exec",
			AgentName: "worker",
			Command:   []string{"/bin/true"},
			NoAuth:    true,
		})
		assert.Equal(t, http.StatusBadRequest, execSchedRec.Code, execSchedRec.Body.String())

		execEvtRec := requestAsIdentity(t, f.srv, caller, http.MethodPost, eventsURL, CreateScheduledEventRequest{
			EventType: "exec",
			FireIn:    "10m",
			AgentName: "worker",
			Command:   []string{"/bin/true"},
			NoAuth:    true,
		})
		assert.Equal(t, http.StatusBadRequest, execEvtRec.Code, execEvtRec.Body.String())
	})

	t.Run("noAuth_together_with_raw_payload_rejected_400", func(t *testing.T) {
		rec := requestAsIdentity(t, f.srv, caller, http.MethodPost, schedURL, CreateScheduleRequest{
			Name:      "ambiguous-sched-noauth",
			CronExpr:  "0 5 * * *",
			EventType: "dispatch_agent",
			Payload:   `{"agentName":"ambiguous-worker"}`,
			NoAuth:    true,
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

		evtRec := requestAsIdentity(t, f.srv, caller, http.MethodPost, eventsURL, CreateScheduledEventRequest{
			EventType: "dispatch_agent",
			FireIn:    "15m",
			Payload:   `{"agentName":"ambiguous-oneshot"}`,
			NoAuth:    true,
		})
		assert.Equal(t, http.StatusBadRequest, evtRec.Code, evtRec.Body.String())
	})

	t.Run("patch_schedule_payload_noAuth_bumps_authorization_revision", func(t *testing.T) {
		createRec := requestAsIdentity(t, f.srv, caller, http.MethodPost, schedURL, CreateScheduleRequest{
			Name:      "toggle-noauth",
			CronExpr:  "0 4 * * *",
			EventType: "dispatch_agent",
			AgentName: "toggle-worker",
		})
		require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
		var created store.Schedule
		require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))

		before, err := f.store.GetSchedule(ctx, created.ID)
		require.NoError(t, err)
		require.Equal(t, 1, before.AuthorizationRevision)

		patchRec := requestAsIdentity(t, f.srv, caller, http.MethodPatch, schedURL+"/"+created.ID, map[string]any{
			"payload": `{"agentName":"toggle-worker","noAuth":true}`,
		})
		require.Equal(t, http.StatusOK, patchRec.Code, patchRec.Body.String())

		afterToggle, err := f.store.GetSchedule(ctx, created.ID)
		require.NoError(t, err)
		assert.Equal(t, 2, afterToggle.AuthorizationRevision, "toggling noAuth in payload must bump authorization_revision")
		var p DispatchAgentEventPayload
		require.NoError(t, json.Unmarshal([]byte(afterToggle.Payload), &p))
		assert.True(t, p.NoAuth)

		// Re-sending identical payload must not bump revision again.
		noopRec := requestAsIdentity(t, f.srv, caller, http.MethodPatch, schedURL+"/"+created.ID, map[string]any{
			"payload": afterToggle.Payload,
		})
		require.Equal(t, http.StatusOK, noopRec.Code, noopRec.Body.String())
		afterNoop, err := f.store.GetSchedule(ctx, created.ID)
		require.NoError(t, err)
		assert.Equal(t, 2, afterNoop.AuthorizationRevision)
	})
}

// TestHeadlessCreate_ReincarnatePreservesPosture covers headless-identity-note.md
// §3 & §5:
//   - default headless agent reincarnates with NoAuth == false;
//   - explicit payload.noAuth: true headless agent keeps NoAuth == true;
//   - legacy pre-2b scheduled row (AgentRole == none, CreateInputs.NoAuth == true)
//     keeps NoAuth == true across reincarnation.
func TestHeadlessCreate_ReincarnatePreservesPosture(t *testing.T) {
	ctx := context.Background()
	f := newSchedFire(t, "hc-reinc")
	installHeadlessGatherDispatcher(t, f)
	f.setProjectAnnotation(t, projectSettingDefaultAgentRole, string(AgentRoleBaseline))

	noopReg := func(string) string { return "" }
	noopTag := func(string) string { return "" }

	t.Run("default_headless_agent_reincarnates_with_credentials", func(t *testing.T) {
		evt := withSessionRevision(f.event("reinc-default"), f.creator.ID)
		out := fireHeadlessDispatch(f.srv, evt, nil)
		require.Equal(t, hubfire.FireStatusFired, out.Status, out.Error)

		child, _ := f.child(t, "reinc-default")
		require.False(t, child.AppliedConfig.NoAuth)
		require.False(t, child.AppliedConfig.CreateInputs.NoAuth)

		fresh, _, err := f.srv.buildPatchedAppliedConfig(ctx, child, f.proj, noopReg, noopTag, nil)
		require.NoError(t, err)
		assert.False(t, fresh.NoAuth)
		assert.Equal(t, string(AgentRoleBaseline), fresh.AgentRole)
	})

	t.Run("explicit_noAuth_headless_agent_stays_noAuth_across_reincarnation", func(t *testing.T) {
		evt := f.event("reinc-optout")
		evt.Payload = `{"agentName":"reinc-optout","noAuth":true}`
		evt = withSessionRevision(evt, f.creator.ID)
		out := fireHeadlessDispatch(f.srv, evt, nil)
		require.Equal(t, hubfire.FireStatusFired, out.Status, out.Error)

		child, edge := f.child(t, "reinc-optout")
		require.Equal(t, string(AgentRoleNone), child.AppliedConfig.AgentRole)
		require.Equal(t, string(AgentRoleNone), edge.Role)
		require.True(t, child.AppliedConfig.NoAuth)
		require.True(t, child.AppliedConfig.CreateInputs.NoAuth)

		fresh, _, err := f.srv.buildPatchedAppliedConfig(ctx, child, f.proj, noopReg, noopTag, nil)
		require.NoError(t, err)
		assert.True(t, fresh.NoAuth)
		assert.Equal(t, string(AgentRoleNone), fresh.AgentRole)
	})

	t.Run("legacy_pre_2b_scheduled_agent_stays_noAuth_across_reincarnation", func(t *testing.T) {
		legacy := &store.Agent{
			ID:        api.NewUUID(),
			Slug:      "reinc-legacy",
			Name:      "reinc-legacy",
			ProjectID: f.proj.ID,
			Phase:     "stopped",
			Detached:  true,
			CreatedBy: f.creator.ID,
			AppliedConfig: &store.AgentAppliedConfig{
				AgentRole: string(AgentRoleNone),
				NoAuth:    true,
				CreateInputs: &store.AgentCreateInputs{
					NoAuth: true,
				},
			},
		}
		require.NoError(t, f.store.CreateAgent(ctx, legacy))

		fresh, _, err := f.srv.buildPatchedAppliedConfig(ctx, legacy, f.proj, noopReg, noopTag, nil)
		require.NoError(t, err)
		assert.True(t, fresh.NoAuth)
		assert.Equal(t, string(AgentRoleNone), fresh.AgentRole)
	})
}

// TestHeadlessCreate_NarrowCeilingRequiresNoAuthOrReauthorize covers R3
// (headless-identity-note.md §2 & §3, Stage 1 design rev 8.30):
//   - a UAT or agent-principal revision whose frozen ceiling admits no role in
//     {full, baseline, readonly} fails the fire with the exact remedy text and
//     creates nothing;
//   - the same narrow-ceiling fixtures with payload.noAuth: true explicitly
//     select role none and succeed with NoAuth=true under the narrow ceiling.
func TestHeadlessCreate_NarrowCeilingRequiresNoAuthOrReauthorize(t *testing.T) {
	f := newSchedFire(t, "hc-narrow")
	client := installHeadlessGatherDispatcher(t, f)

	const wantErrText = "no usable role for the recorded authority; set noAuth: true or re-authorize the schedule"

	t.Run("narrow_uat_without_readonly_selectors", func(t *testing.T) {
		// UAT holds agent:create and project:read only (no readonly role selectors).
		tok := f.storedUAT(t, "agent:create", "project:read")

		evtFail := withUATRevision(f.event("narrow-uat-fail"), tok)
		outFail := fireHeadlessDispatch(f.srv, evtFail, nil)
		assert.Equal(t, hubfire.FireStatusFailed, outFail.Status)
		assert.Equal(t, wantErrText, outFail.Error)
		f.assertNoChild(t, "narrow-uat-fail")

		evtOptOut := f.event("narrow-uat-noauth")
		evtOptOut.Payload = `{"agentName":"narrow-uat-noauth","noAuth":true}`
		evtOptOut = withUATRevision(evtOptOut, tok)
		outOptOut := fireHeadlessDispatch(f.srv, evtOptOut, nil)
		require.Equal(t, hubfire.FireStatusFired, outOptOut.Status, outOptOut.Error)

		child, edge := f.child(t, "narrow-uat-noauth")
		assert.Equal(t, f.creator.ID, child.OwnerID)
		assert.Equal(t, []string{f.creator.ID}, child.Ancestry)
		assert.Equal(t, string(AgentRoleNone), child.AppliedConfig.AgentRole)
		assert.Equal(t, string(AgentRoleNone), edge.Role)
		assert.True(t, child.AppliedConfig.NoAuth)
		assert.True(t, child.AppliedConfig.CreateInputs.NoAuth)
		assert.True(t, client.lastCreateReq.NoAuth)
	})

	t.Run("narrow_agent_revision_without_readonly_coverage", func(t *testing.T) {
		parent := f.sessionAgent(t, "narrow-agent-parent")

		evtFail := withAgentRevision(t, f.srv, f.event("narrow-agent-fail"), parent.ID)
		evtFail.AuthorityCeiling.PermissionIDs = []string{"agent.create", "project.read"}
		outFail := fireHeadlessDispatch(f.srv, evtFail, nil)
		assert.Equal(t, hubfire.FireStatusFailed, outFail.Status)
		assert.Equal(t, wantErrText, outFail.Error)
		f.assertNoChild(t, "narrow-agent-fail")

		evtOptOut := f.event("narrow-agent-noauth")
		evtOptOut.Payload = `{"agentName":"narrow-agent-noauth","noAuth":true}`
		evtOptOut = withAgentRevision(t, f.srv, evtOptOut, parent.ID)
		evtOptOut.AuthorityCeiling.PermissionIDs = []string{"agent.create", "project.read"}
		outOptOut := fireHeadlessDispatch(f.srv, evtOptOut, nil)
		require.Equal(t, hubfire.FireStatusFired, outOptOut.Status, outOptOut.Error)

		child, edge := f.child(t, "narrow-agent-noauth")
		assert.Equal(t, parent.ID, child.OwnerID)
		assert.Equal(t, []string{f.creator.ID, parent.ID}, child.Ancestry)
		assert.Equal(t, string(AgentRoleNone), child.AppliedConfig.AgentRole)
		assert.Equal(t, string(AgentRoleNone), edge.Role)
		assert.Equal(t, []string{"agent.create", "project.read"}, edge.PermissionIDs)
		assert.True(t, child.AppliedConfig.NoAuth)
		assert.True(t, child.AppliedConfig.CreateInputs.NoAuth)
		assert.True(t, client.lastCreateReq.NoAuth)
	})
}

var errInjectedGetProjectFault = errors.New("injected get-project store fault")

type getProjectFaultStore struct {
	store.Store
	fault *storeFaultSwitch
	err   error
}

func (s *getProjectFaultStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if !s.fault.Active() {
		return s.Store.GetProject(ctx, id)
	}
	if s.err != nil {
		return nil, s.err
	}
	return nil, errInjectedGetProjectFault
}

// TestDispatchAgentAction_ProjectLookupFaultAndRoleSnapshot covers pass-2 O1:
//   - a non-ErrNotFound GetProject error in authorizeDispatchAgentFire fails
//     the fire (FireStatusFailed) rather than proceeding with project == nil;
//   - mutating scion.io/default-agent-role between Authorize and Fire does not
//     alter the role written by createAgentHeadless from what CanDelegate
//     approved.
func TestDispatchAgentAction_ProjectLookupFaultAndRoleSnapshot(t *testing.T) {
	ctx := context.Background()

	t.Run("get_project_error_fails_fire", func(t *testing.T) {
		srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *getProjectFaultStore {
			return &getProjectFaultStore{Store: inner, fault: f}
		})
		srv.registerFireActions()

		user := &store.User{
			ID:          tid("user-proj-fault"),
			Email:       "projfault@example.com",
			DisplayName: "Proj Fault Tester",
			Role:        store.UserRoleMember,
			Status:      "active",
			Created:     time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, user))
		ensureHubMembership(ctx, s, user.ID)

		project := &store.Project{
			ID:        tid("project-proj-fault"),
			Name:      "proj-fault-project",
			Slug:      "proj-fault-project",
			OwnerID:   user.ID,
			CreatedBy: user.ID,
			Created:   time.Now(),
			Updated:   time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, project))
		srv.seedProjectCreatorMembership(ctx, project)
		require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, user.ID))
		linkScheduledDispatchBroker(t, s, project.ID)

		// Seed a full-role parent agent while the fault switch is disarmed.
		seedParentEvt := withSessionRevision(store.ScheduledEvent{
			ID:         api.NewUUID(),
			ProjectID:  project.ID,
			EventType:  "dispatch_agent",
			Payload:    `{"agentName":"proj-fault-parent"}`,
			CreatedBy:  user.ID,
			ScheduleID: "sched-proj-fault-parent",
			FireAt:     time.Now(),
		}, user.ID)
		seedOut := fireHeadlessDispatch(srv, seedParentEvt, nil)
		require.Equal(t, hubfire.FireStatusFired, seedOut.Status, seedOut.Error)
		parent, err := s.GetAgentBySlug(ctx, project.ID, "proj-fault-parent")
		require.NoError(t, err)

		evtAgent := withAgentRevision(t, srv, store.ScheduledEvent{
			ID:         api.NewUUID(),
			ProjectID:  project.ID,
			EventType:  "dispatch_agent",
			Payload:    `{"agentName":"proj-fault-child"}`,
			CreatedBy:  parent.ID,
			ScheduleID: "sched-proj-fault-child",
			FireAt:     time.Now(),
		}, parent.ID)

		evtUser := withSessionRevision(store.ScheduledEvent{
			ID:         api.NewUUID(),
			ProjectID:  project.ID,
			EventType:  "dispatch_agent",
			Payload:    `{"agentName":"proj-fault-user-child"}`,
			CreatedBy:  user.ID,
			ScheduleID: "sched-proj-fault-user-child",
			FireAt:     time.Now(),
		}, user.ID)
		rec := scheduledRecordedAuthority(evtUser)
		auth, identity, err := srv.resolveRecordedAuthority(ctx, rec, project.ID, dispatchAgentPermission)
		require.NoError(t, err)

		// Arm the GetProject store fault.
		fault.Arm()

		// Full fireAction pipeline with an agent principal (resolveFireAgent
		// reads only GetAgent, so authorizeDispatchAgentFire hits GetProject).
		out := fireHeadlessDispatch(srv, evtAgent, nil)
		assert.Equal(t, hubfire.FireStatusFailed, out.Status)
		assert.Contains(t, out.Error, errInjectedGetProjectFault.Error())
		_, err = s.GetAgentBySlug(ctx, project.ID, "proj-fault-child")
		assert.ErrorIs(t, err, store.ErrNotFound)

		// Direct dispatchAgentAction.Authorize with a user principal returns an
		// operational error (not a FireRefusal) on a non-NotFound GetProject fault.
		action := &dispatchAgentAction{srv: srv}
		authorized, refusal, err := action.Authorize(ctx, hubfire.FireRequest{
			ProjectID:  project.ID,
			Authority:  rec,
			ActionType: "dispatch_agent",
			Payload:    evtUser.Payload,
		}, auth, identity)
		require.ErrorIs(t, err, errDispatchAgentProjectLookup)
		assert.ErrorIs(t, err, errInjectedGetProjectFault)
		assert.Nil(t, authorized)
		assert.Nil(t, refusal)
		_, err = s.GetAgentBySlug(ctx, project.ID, "proj-fault-user-child")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("get_project_not_found_refuses_schedule_and_trigger_with_base_text", func(t *testing.T) {
		srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *getProjectFaultStore {
			return &getProjectFaultStore{Store: inner, fault: f, err: store.ErrNotFound}
		})
		srv.registerFireActions()

		user := &store.User{
			ID:          tid("user-proj-nf"),
			Email:       "projnf@example.com",
			DisplayName: "Proj NotFound Tester",
			Role:        store.UserRoleMember,
			Status:      "active",
			Created:     time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, user))
		ensureHubMembership(ctx, s, user.ID)

		project := &store.Project{
			ID:        tid("project-proj-nf"),
			Name:      "proj-nf-project",
			Slug:      "proj-nf-project",
			OwnerID:   user.ID,
			CreatedBy: user.ID,
			Created:   time.Now(),
			Updated:   time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, project))
		srv.seedProjectCreatorMembership(ctx, project)
		require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, user.ID))
		linkScheduledDispatchBroker(t, s, project.ID)

		evtBase := withSessionRevision(store.ScheduledEvent{
			ID:        api.NewUUID(),
			ProjectID: project.ID,
			EventType: "dispatch_agent",
			Payload:   `{"agentName":"proj-nf-child"}`,
			CreatedBy: user.ID,
			FireAt:    time.Now(),
		}, user.ID)

		fault.Arm()
		wantErr := fmt.Sprintf("project %q no longer exists", project.ID)
		for _, defKind := range []string{
			hubfire.DefinitionKindScheduledEvent,
			hubfire.DefinitionKindSchedule,
			hubfire.DefinitionKindTrigger,
		} {
			out := srv.fireAction(ctx, hubfire.FireRequest{
				ProjectID: project.ID,
				Authority: hubfire.RecordedAuthority{
					Attribution:    evtBase.InitiatorAttribution,
					Ceiling:        evtBase.AuthorityCeiling,
					DefinitionKind: defKind,
					DefinitionID:   "def-" + defKind,
					FireID:         "fire-" + defKind,
				},
				ActionType: "dispatch_agent",
				Payload:    evtBase.Payload,
			})
			assert.Equal(t, hubfire.FireStatusRefused, out.Status, "defKind=%s", defKind)
			assert.Equal(t, wantErr, out.Error, "defKind=%s", defKind)
		}

		_, err := s.GetAgentBySlug(ctx, project.ID, "proj-nf-child")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("default_agent_role_change_between_authorize_and_fire_preserves_approved_role", func(t *testing.T) {
		f := newSchedFire(t, "hc-role-toctou")
		installHeadlessGatherDispatcher(t, f)
		action := &dispatchAgentAction{srv: f.srv}

		for _, tc := range []struct {
			name        string
			childSlug   string
			mutatedRole AgentRole
		}{
			{name: "escalated_to_full_after_authorize", childSlug: "toctou-escalated", mutatedRole: AgentRoleFull},
			{name: "narrowed_to_readonly_after_authorize", childSlug: "toctou-narrowed", mutatedRole: AgentRoleReadOnly},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f.setProjectAnnotation(t, projectSettingDefaultAgentRole, string(AgentRoleBaseline))

				evt := withSessionRevision(f.event(tc.childSlug), f.creator.ID)
				rec := scheduledRecordedAuthority(evt)
				req := hubfire.FireRequest{
					ProjectID:  f.proj.ID,
					Authority:  rec,
					ActionType: "dispatch_agent",
					Payload:    evt.Payload,
				}
				auth, identity, err := f.srv.resolveRecordedAuthority(ctx, rec, f.proj.ID, dispatchAgentPermission)
				require.NoError(t, err)

				authorized, refusal, err := action.Authorize(ctx, req, auth, identity)
				require.NoError(t, err)
				require.Nil(t, refusal)

				// Mutate project default-agent-role between Authorize and Fire.
				f.setProjectAnnotation(t, projectSettingDefaultAgentRole, string(tc.mutatedRole))

				out, err := action.Fire(ctx, req, authorized)
				require.NoError(t, err)
				assert.Equal(t, hubfire.FireStatusFired, out.Status)

				child, edge := f.child(t, tc.childSlug)
				assert.Equal(t, string(AgentRoleBaseline), child.AppliedConfig.AgentRole)
				assert.Equal(t, string(AgentRoleBaseline), edge.Role)
			})
		}
	})
}

// TestHeadlessCreate_NoAuthOwnerGitTokenFallback covers security-pass L-1:
// with payload.noAuth: true, OwnerID is recorded from the revision principal,
// so when no project-scope GITHUB_TOKEN exists the git token falls back to the
// owner's user-scope GITHUB_TOKEN (pickNoAuthGitToken) while all other secrets
// remain withheld — matching an interactive --no-auth create by the same user.
func TestHeadlessCreate_NoAuthOwnerGitTokenFallback(t *testing.T) {
	ctx := context.Background()
	f := newSchedFire(t, "hc-noauth-git")
	client := installHeadlessGatherDispatcher(t, f)

	const (
		ownerGitToken      = "ghp-owner-user-scope-token"
		ownerLLMSecret     = "sk-ant-owner-secret"
		projectOtherSecret = "sk-proj-other-secret"
	)

	for _, in := range []*secret.SetSecretInput{
		{
			Name: noAuthGitTokenKey, Value: ownerGitToken,
			SecretType: store.SecretTypeEnvironment, Target: noAuthGitTokenKey,
			Scope: store.ScopeUser, ScopeID: f.creator.ID,
			InjectionMode: store.InjectionModeAlways,
			CreatedBy:     f.creator.ID, UpdatedBy: f.creator.ID,
		},
		{
			Name: "ANTHROPIC_API_KEY", Value: ownerLLMSecret,
			SecretType: store.SecretTypeEnvironment, Target: "ANTHROPIC_API_KEY",
			Scope: store.ScopeUser, ScopeID: f.creator.ID,
			InjectionMode: store.InjectionModeAlways, AllowProgeny: true,
			CreatedBy: f.creator.ID, UpdatedBy: f.creator.ID,
		},
		{
			Name: "OPENAI_API_KEY", Value: projectOtherSecret,
			SecretType: store.SecretTypeEnvironment, Target: "OPENAI_API_KEY",
			Scope: store.ScopeProject, ScopeID: f.proj.ID,
			InjectionMode: store.InjectionModeAlways,
			CreatedBy:     f.creator.ID, UpdatedBy: f.creator.ID,
		},
	} {
		_, _, err := f.srv.secretBackend.Set(ctx, in)
		require.NoError(t, err)
	}

	evt := f.event("noauth-owner-git")
	evt.Payload = `{"agentName":"noauth-owner-git","noAuth":true}`
	evt = withSessionRevision(evt, f.creator.ID)
	out := fireHeadlessDispatch(f.srv, evt, nil)
	require.Equal(t, hubfire.FireStatusFired, out.Status, out.Error)

	child, edge := f.child(t, "noauth-owner-git")
	assert.Equal(t, f.creator.ID, child.OwnerID)
	assert.Equal(t, []string{f.creator.ID}, child.Ancestry)
	assert.Equal(t, string(AgentRoleNone), child.AppliedConfig.AgentRole)
	assert.Equal(t, string(AgentRoleNone), edge.Role)
	assert.True(t, child.AppliedConfig.NoAuth)

	req := client.lastCreateReq
	require.NotNil(t, req)
	assert.True(t, req.NoAuth)
	assert.Empty(t, req.ResolvedSecrets)
	assert.Equal(t, ownerGitToken, req.ResolvedEnv[noAuthGitTokenKey])
	assert.Equal(t, api.EnvKindSecretFetchable, req.EnvClassifications[noAuthGitTokenKey])
	assert.NotContains(t, req.ResolvedEnv, "ANTHROPIC_API_KEY")
	assert.NotContains(t, req.ResolvedEnv, "OPENAI_API_KEY")
}

var errInjectedGetAgentBySlugFault = errors.New("injected get-agent-by-slug store fault")

type getAgentBySlugFaultStore struct {
	store.Store
	fault    *storeFaultSwitch
	failSlug string
}

func (s *getAgentBySlugFaultStore) GetAgentBySlug(ctx context.Context, projectID, slug string) (*store.Agent, error) {
	if s.fault.Active() && slug == s.failSlug {
		return nil, errInjectedGetAgentBySlugFault
	}
	return s.Store.GetAgentBySlug(ctx, projectID, slug)
}

func TestHeadlessCreate_GetAgentBySlugErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	const targetSlug = "slug-lookup-fault-child"

	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *getAgentBySlugFaultStore {
		return &getAgentBySlugFaultStore{Store: inner, fault: f, failSlug: targetSlug}
	})
	srv.registerFireActions()

	client := &headlessGatherBrokerClient{}
	disp := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	srv.SetDispatcher(disp)

	user := &store.User{
		ID:          tid("user-slug-fault"),
		Email:       "slugfault@example.com",
		DisplayName: "Slug Fault Tester",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("project-slug-fault"),
		Name:      "slug-fault-project",
		Slug:      "slug-fault-project",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, user.ID))
	linkScheduledDispatchBroker(t, s, project.ID)

	evt := withSessionRevision(store.ScheduledEvent{
		ID:         api.NewUUID(),
		ProjectID:  project.ID,
		EventType:  "dispatch_agent",
		Payload:    `{"agentName":"` + targetSlug + `"}`,
		CreatedBy:  user.ID,
		ScheduleID: "sched-slug-fault",
		FireAt:     time.Now(),
	}, user.ID)
	rec := scheduledRecordedAuthority(evt)
	auth, identity, err := srv.resolveRecordedAuthority(ctx, rec, project.ID, dispatchAgentPermission)
	require.NoError(t, err)
	edgeCeiling, edgeProvenance, err := srv.fireEffectCeiling(ctx, auth)
	require.NoError(t, err)

	fault.Arm()

	// 1. Direct createAgentHeadless returns a wrapped error and creates nothing.
	rr := headlessResolvedRole{preCapRole: AgentRoleFull}
	agent, err := srv.createAgentHeadless(ctx, HeadlessCreate{
		ProjectID:      project.ID,
		Authority:      auth,
		Identity:       identity,
		CreatorName:    fireCreatorName(auth),
		EdgeCeiling:    edgeCeiling,
		EdgeProvenance: edgeProvenance,
		resolvedRole:   &rr,
		Name:           targetSlug,
		Detached:       true,
	})
	require.ErrorIs(t, err, errInjectedGetAgentBySlugFault)
	assert.Nil(t, agent)
	assert.Nil(t, client.lastCreateReq)

	// 2. Full fireAction pipeline fails closed without creating a row or dispatching.
	out := fireHeadlessDispatch(srv, evt, nil)
	assert.Equal(t, hubfire.FireStatusFailed, out.Status)
	assert.Contains(t, out.Error, errInjectedGetAgentBySlugFault.Error())
	assert.Nil(t, client.lastCreateReq)

	_, err = s.GetAgentBySlug(ctx, project.ID, targetSlug)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestHeadlessCreate_HubDefaultMessageMode verifies that createAgentHeadless
// applies server.hub.default_message_mode when neither template nor parent sets
// a message mode, while preserving template precedence and refusing "hub".
func TestHeadlessCreate_HubDefaultMessageMode(t *testing.T) {
	cases := []struct {
		name       string
		hubDefault string
		tmplMode   string
		wantMode   string
	}{
		{name: "unset hub default: project", wantMode: store.MessageModeProject},
		{name: "hub default applies", hubDefault: store.MessageModeNone, wantMode: store.MessageModeNone},
		{name: "template wins over hub default", hubDefault: store.MessageModeNone, tmplMode: store.MessageModeBranch, wantMode: store.MessageModeBranch},
		{name: "hub value never applies as a default", hubDefault: store.MessageModeHub, wantMode: store.MessageModeProject},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newSchedFire(t, fmt.Sprintf("hc-dmm-%d", i))
			_ = installHeadlessGatherDispatcher(t, f)
			f.srv.config.DefaultMessageMode = tc.hubDefault

			tmplSlug := fmt.Sprintf("hc-dmm-tmpl-%d", i)
			require.NoError(t, f.store.CreateTemplate(ctx, &store.Template{
				ID:          tid(tmplSlug),
				Name:        tmplSlug,
				Slug:        tmplSlug,
				Harness:     "claude",
				ContentHash: "d00dfeed",
				Scope:       store.TemplateScopeGlobal,
				Status:      store.TemplateStatusActive,
				AgentConfig: &api.ScionConfig{MessageMode: tc.tmplMode},
			}))

			evt := withSessionRevision(f.event(fmt.Sprintf("dmm-agent-%d", i)), f.creator.ID)
			auth, identity, err := f.srv.resolveRecordedAuthority(ctx, scheduledRecordedAuthority(evt), f.proj.ID, "agent.create")
			require.NoError(t, err)
			edgeCeiling, edgeProv, err := f.srv.fireEffectCeiling(ctx, auth)
			require.NoError(t, err)

			rr := headlessResolvedRole{preCapRole: AgentRoleFull}
			agent, err := f.srv.createAgentHeadless(ctx, HeadlessCreate{
				ProjectID:      f.proj.ID,
				Authority:      auth,
				Identity:       identity,
				EdgeCeiling:    edgeCeiling,
				EdgeProvenance: edgeProv,
				resolvedRole:   &rr,
				Name:           fmt.Sprintf("dmm-agent-%d", i),
				Template:       tmplSlug,
				Detached:       true,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.wantMode, agent.MessageMode)

			stored, err := f.store.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantMode, stored.MessageMode)
		})
	}
}

// TestHeadlessCreate_HomeFilesPendingBlocksConcurrentStart verifies the
// structural fix for the held start claim race (P1c-1 O-C / sec L2):
//   - While create-time HomeFiles have not yet been delivered, CreateInputs.HomeFilesPending
//     is true on the committed agent row, and both startAgentCore and DispatchAgentStart
//     refuse concurrent manual starts with a clear retryable 409 Conflict error.
//   - Once createAgentHeadless delivers the HomeFiles, HomeFilesPending is cleared
//     and subsequent starts proceed normally.
//   - A normal headless create without EventFile never sets HomeFilesPending and is
//     unaffected.
func TestHeadlessCreate_HomeFilesPendingBlocksConcurrentStart(t *testing.T) {
	ctx := context.Background()
	f := newSchedFire(t, "hf-race")
	client := installHeadlessGatherDispatcher(t, f)

	broker, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
	require.NoError(t, err)
	broker.Capabilities = &store.BrokerCapabilities{HomeFiles: true}
	require.NoError(t, f.store.UpdateRuntimeBroker(ctx, broker))

	evt := withSessionRevision(f.event("hf-race-agent"), f.creator.ID)
	auth, identity, err := f.srv.resolveRecordedAuthority(ctx, scheduledRecordedAuthority(evt), f.proj.ID, "agent.create")
	require.NoError(t, err)
	edgeCeiling, edgeProv, err := f.srv.fireEffectCeiling(ctx, auth)
	require.NoError(t, err)
	rr := headlessResolvedRole{preCapRole: AgentRoleFull}

	t.Run("concurrent_manual_start_refused_until_home_files_delivered", func(t *testing.T) {
		var (
			callbackRan     bool
			startCoreErr    error
			dispatcherErr   error
			restartErr      error
			httpStatus      int
			httpBody        string
			pendingAtCommit bool
		)
		created, err := f.srv.createAgentHeadless(ctx, HeadlessCreate{
			ProjectID:      f.proj.ID,
			Authority:      auth,
			Identity:       identity,
			EdgeCeiling:    edgeCeiling,
			EdgeProvenance: edgeProv,
			resolvedRole:   &rr,
			Name:           "hf-race-agent",
			Detached:       true,
			EventFile:      []byte(`{"event_id":"ev-race-1"}`),
			OnCommitted: func(agentID string) {
				callbackRan = true
				committed, gerr := f.store.GetAgent(ctx, agentID)
				if gerr != nil {
					t.Errorf("GetAgent in OnCommitted: %v", gerr)
					return
				}
				pendingAtCommit = agentHomeFilesPending(committed)

				// Simulate a concurrent manual start via startAgentCore during
				// the window between commitAgentCreate and createUnderClaim.
				startCoreErr = f.srv.startAgentCore(ctx, committed, StartOpts{
					Kind: store.StartClaimUser,
				})
				rec := httptest.NewRecorder()
				if f.srv.writeStartClaimError(ctx, rec, startCoreErr, agentID) {
					httpStatus = rec.Code
					httpBody = rec.Body.String()
				}

				// Direct DispatchAgentStart and DispatchAgentRestart backstops must also refuse.
				dispatcherErr = f.srv.GetDispatcher().DispatchAgentStart(ctx, committed, "", false)
				restartErr = f.srv.GetDispatcher().DispatchAgentRestart(ctx, committed)
			},
		})
		require.NoError(t, err)
		require.NotNil(t, created)
		require.True(t, callbackRan, "OnCommitted must run between commit and dispatch")
		assert.True(t, pendingAtCommit, "HomeFilesPending must be true immediately after commit")
		require.ErrorIs(t, startCoreErr, errHomeFilesNotDelivered)
		assert.Equal(t, http.StatusConflict, httpStatus)
		assert.Contains(t, httpBody, "home files")
		assert.Contains(t, httpBody, "if creation failed, delete and re-create the agent")
		require.ErrorIs(t, dispatcherErr, errHomeFilesNotDelivered)
		require.ErrorIs(t, restartErr, errHomeFilesNotDelivered)
		assert.Equal(t, []api.HomeFile{{
			Path:    HeadlessEventFilePath,
			Mode:    0o600,
			Content: []byte(`{"event_id":"ev-race-1"}`),
		}}, client.lastCreateReq.HomeFiles)

		// After create dispatch completes, HomeFilesPending is cleared in store.
		afterCreate, err := f.store.GetAgent(ctx, created.ID)
		require.NoError(t, err)
		assert.False(t, agentHomeFilesPending(afterCreate), "HomeFilesPending must be cleared after create dispatch delivers HomeFiles")

		// A subsequent manual start is no longer blocked by HomeFilesPending.
		startAfterErr := f.srv.startAgentCore(ctx, afterCreate, StartOpts{
			Kind: store.StartClaimUser,
		})
		assert.NotErrorIs(t, startAfterErr, errHomeFilesNotDelivered)
	})

	t.Run("start_agent_core_refuses_pending_without_dispatcher_backstop", func(t *testing.T) {
		// Replace the dispatcher with a stub that has no HomeFilesPending check,
		// proving startAgentCore itself blocks pending starts (kills mutant m1).
		origDisp := f.srv.GetDispatcher()
		stubDisp := &noopStartTrackingDispatcher{AgentDispatcher: origDisp}
		f.srv.SetDispatcher(stubDisp)
		t.Cleanup(func() { f.srv.SetDispatcher(origDisp) })

		pendingAgent := &store.Agent{
			ID:              api.NewUUID(),
			Name:            "hf-pending-core-only",
			Slug:            "hf-pending-core-only",
			ProjectID:       f.proj.ID,
			RuntimeBrokerID: f.broker.ID,
			AppliedConfig: &store.AgentAppliedConfig{
				CreateInputs: &store.AgentCreateInputs{HomeFilesPending: true},
			},
		}
		err := f.srv.startAgentCore(ctx, pendingAgent, StartOpts{Kind: store.StartClaimUser})
		require.ErrorIs(t, err, errHomeFilesNotDelivered)
		assert.Zero(t, stubDisp.startCalls, "startAgentCore must refuse before calling dispatcher")
	})

	t.Run("async_launch_report_clears_pending_and_persist_accepted_launch_preserves_clear", func(t *testing.T) {
		// Simulates the R1 race: broker accepts an async launch and sends its
		// succeeded launch report (through handleAgentLaunchReport) BEFORE
		// createAgentHeadless runs persistAcceptedLaunch.
		origDisp := f.srv.GetDispatcher()
		inner := origDisp.(*HTTPAgentDispatcher)
		earlyDisp := &earlyLaunchReportDispatcher{
			HTTPAgentDispatcher: inner,
			srv:                 f.srv,
			t:                   t,
			brokerID:            f.broker.ID,
		}
		f.srv.SetDispatcher(earlyDisp)
		t.Cleanup(func() { f.srv.SetDispatcher(origDisp) })

		created, err := f.srv.createAgentHeadless(ctx, HeadlessCreate{
			ProjectID:      f.proj.ID,
			Authority:      auth,
			Identity:       identity,
			EdgeCeiling:    edgeCeiling,
			EdgeProvenance: edgeProv,
			resolvedRole:   &rr,
			Name:           "hf-async-early-report",
			Detached:       true,
			EventFile:      []byte(`{"event_id":"ev-async-1"}`),
		})
		require.NoError(t, err)
		require.NotNil(t, created)
		require.True(t, earlyDisp.reportSent, "launch report must have been posted")

		// Stored row must remain cleared (persistAcceptedLaunch must NOT resurrect HomeFilesPending).
		row, err := f.store.GetAgent(ctx, created.ID)
		require.NoError(t, err)
		assert.False(t, agentHomeFilesPending(row), "persistAcceptedLaunch must preserve cleared HomeFilesPending")

		// mergeDispatchedAgent must also preserve cleared HomeFilesPending.
		staleSrc := &store.Agent{
			AppliedConfig: &store.AgentAppliedConfig{
				CreateInputs: &store.AgentCreateInputs{HomeFilesPending: true},
			},
		}
		mergeDispatchedAgent(row, staleSrc)
		assert.False(t, agentHomeFilesPending(row), "mergeDispatchedAgent must not resurrect HomeFilesPending")
		assert.True(t, agentHomeFilesPending(staleSrc), "mergeDispatchedAgent must not mutate src")

		// Agent is startable (not blocked by errHomeFilesNotDelivered).
		startErr := f.srv.startAgentCore(ctx, row, StartOpts{Kind: store.StartClaimUser})
		assert.NotErrorIs(t, startErr, errHomeFilesNotDelivered)
	})

	t.Run("claim_held_during_home_files_create_rolls_back_and_releases_subject_claim", func(t *testing.T) {
		trigID := api.NewUUID()
		evID := api.NewUUID()
		fireID := api.NewUUID()
		now := time.Now().UTC()
		claimed, existing, err := f.store.ClaimTriggerSubject(ctx, trigID, "pr-held-1", evID, fireID, now, now.Add(-5*time.Minute))
		require.NoError(t, err)
		require.Nil(t, existing)
		require.NotNil(t, claimed)

		var (
			createdID string
			heldRun   *startClaimRun
		)
		defer func() {
			if heldRun != nil {
				heldRun.finish(startReleased)
			}
		}()
		_, err = f.srv.createAgentHeadless(ctx, HeadlessCreate{
			ProjectID:      f.proj.ID,
			Authority:      auth,
			Identity:       identity,
			EdgeCeiling:    edgeCeiling,
			EdgeProvenance: edgeProv,
			resolvedRole:   &rr,
			Name:           "hf-held-claim-agent",
			Detached:       true,
			EventFile:      []byte(`{"event_id":"ev-held-1"}`),
			OnCommitTx: func(txCtx context.Context, tx store.Store, agentID string) error {
				bound, bErr := tx.BindTriggerSubjectClaim(txCtx, claimed.ID, fireID, agentID, time.Now().UTC())
				if bErr != nil {
					return bErr
				}
				if !bound {
					return errors.New("bind failed")
				}
				return nil
			},
			OnCommitted: func(agentID string) {
				createdID = agentID
				committed, gErr := f.store.GetAgent(ctx, agentID)
				require.NoError(t, gErr)
				// Acquire a competing start claim directly so createUnderClaim
				// encounters ClaimHeldError.
				var acqErr error
				heldRun, acqErr = f.srv.acquireStartClaim(ctx, committed, store.StartClaimUser)
				require.NoError(t, acqErr)
			},
		})
		require.Error(t, err)
		var held *store.ClaimHeldError
		require.ErrorAs(t, err, &held, "ClaimHeldError must be returned (kills mutant m5)")
		require.NotEmpty(t, createdID)

		// The pending orphan agent must have been rolled back.
		_, gErr := f.store.GetAgent(ctx, createdID)
		assert.ErrorIs(t, gErr, store.ErrNotFound, "pending orphan agent must be rolled back on ClaimHeldError")

		// Because the orphan agent row was rolled back, Live() sees the missing
		// agent and lazily releases the bound subject claim so later events are
		// not stuck busy (preserving P1c-2's post-commit claim invariant).
		claims := trigger.NewSubjectClaims(f.store, f.store, time.Now)
		live, liveErr := claims.Live(ctx, trigID, "pr-held-1")
		require.NoError(t, liveErr)
		assert.Nil(t, live, "Live must see rolled-back agent is gone and release the claim")
		_, claimErr := f.store.GetActiveTriggerSubjectClaim(ctx, trigID, "pr-held-1")
		assert.ErrorIs(t, claimErr, store.ErrNotFound, "bound subject claim must be lazily released after rollback")
	})

	t.Run("finalize_env_and_create_family_refused_while_home_files_pending", func(t *testing.T) {
		var (
			httpCode       int
			httpBody       string
			finalizeErr    error
			createErr      error
			gatherErr      error
			provisionErr   error
			callsDuringWin []*RemoteCreateAgentRequest
		)
		created, err := f.srv.createAgentHeadless(ctx, HeadlessCreate{
			ProjectID:      f.proj.ID,
			Authority:      auth,
			Identity:       identity,
			EdgeCeiling:    edgeCeiling,
			EdgeProvenance: edgeProv,
			resolvedRole:   &rr,
			Name:           "hf-finalize-env-race",
			Detached:       true,
			EventFile:      []byte(`{"event_id":"ev-finalize-race"}`),
			OnCommitted: func(agentID string) {
				committed, gErr := f.store.GetAgent(ctx, agentID)
				require.NoError(t, gErr)
				require.True(t, agentHomeFilesPending(committed))

				client.mu.Lock()
				beforeCalls := len(client.calls)
				client.mu.Unlock()

				// 1. Real HTTP POST .../env during the commit->claim window must be
				// refused with 409 Conflict and must not launch the agent without HomeFiles.
				rec := doRequest(t, f.srv, http.MethodPost,
					fmt.Sprintf("/api/v1/projects/%s/agents/%s/env", f.proj.ID, committed.Slug),
					map[string]interface{}{"env": map[string]string{"FOO": "bar"}})
				httpCode = rec.Code
				httpBody = rec.Body.String()

				// 2. Direct create-family dispatcher entry points without HomeFiles on ctx
				// must all refuse with errHomeFilesNotDelivered.
				disp := f.srv.GetDispatcher()
				_, finalizeErr = disp.DispatchFinalizeEnv(ctx, committed, map[string]string{"FOO": "bar"})
				_, createErr = disp.DispatchAgentCreate(ctx, committed)
				_, gatherErr = disp.DispatchAgentCreateWithGather(ctx, committed)
				provisionErr = disp.DispatchAgentProvision(ctx, committed)

				client.mu.Lock()
				callsDuringWin = append(callsDuringWin, client.calls[beforeCalls:]...)
				client.mu.Unlock()
			},
		})
		require.NoError(t, err, "trigger create must still succeed with its HomeFiles after env-submit refusal")
		require.NotNil(t, created)
		assert.Equal(t, http.StatusConflict, httpCode, "POST .../env body: %s", httpBody)
		assert.Contains(t, httpBody, "home files")
		assert.Contains(t, httpBody, "if creation failed, delete and re-create the agent")
		require.ErrorIs(t, finalizeErr, errHomeFilesNotDelivered)
		require.ErrorIs(t, createErr, errHomeFilesNotDelivered)
		require.ErrorIs(t, gatherErr, errHomeFilesNotDelivered)
		require.ErrorIs(t, provisionErr, errHomeFilesNotDelivered)
		assert.Empty(t, callsDuringWin, "no broker create/start request may be sent during the commit->claim window without HomeFiles")
		require.Equal(t, []api.HomeFile{{
			Path:    HeadlessEventFilePath,
			Mode:    0o600,
			Content: []byte(`{"event_id":"ev-finalize-race"}`),
		}}, client.lastCreateReq.HomeFiles)
	})

	t.Run("clear_agent_home_files_pending_retries_on_version_conflict", func(t *testing.T) {
		srv, rawStore, wrapper, fault := testServerWithStoreFault(t, func(inner store.Store, sw *storeFaultSwitch) *conflictUpdateAgentStore {
			return &conflictUpdateAgentStore{Store: inner, fault: sw, conflictsRemaining: 2}
		})
		proj := &store.Project{ID: api.NewUUID(), Name: "hf-retry-proj", Slug: "hf-retry-proj"}
		require.NoError(t, rawStore.CreateProject(ctx, proj))
		ag := &store.Agent{
			ID:        api.NewUUID(),
			Name:      "hf-clear-retry",
			Slug:      "hf-clear-retry",
			ProjectID: proj.ID,
			AppliedConfig: &store.AgentAppliedConfig{
				CreateInputs: &store.AgentCreateInputs{HomeFilesPending: true},
			},
		}
		require.NoError(t, rawStore.CreateAgent(ctx, ag))
		wrapper.targetID = ag.ID
		fault.Arm()

		srv.clearAgentHomeFilesPending(ctx, ag)
		assert.Equal(t, 3, wrapper.attempts, "expected 2 version conflicts followed by success on 3rd attempt")
		assert.False(t, agentHomeFilesPending(ag), "in-memory flag must be cleared")

		stored, gErr := rawStore.GetAgent(ctx, ag.ID)
		require.NoError(t, gErr)
		assert.False(t, agentHomeFilesPending(stored), "persisted flag must be cleared after version-conflict retry")
	})

	t.Run("normal_create_without_event_file_unaffected", func(t *testing.T) {
		var pendingAtCommit bool
		created, err := f.srv.createAgentHeadless(ctx, HeadlessCreate{
			ProjectID:      f.proj.ID,
			Authority:      auth,
			Identity:       identity,
			EdgeCeiling:    edgeCeiling,
			EdgeProvenance: edgeProv,
			resolvedRole:   &rr,
			Name:           "hf-normal-agent",
			Detached:       true,
			OnCommitted: func(agentID string) {
				committed, gerr := f.store.GetAgent(ctx, agentID)
				if gerr == nil {
					pendingAtCommit = agentHomeFilesPending(committed)
				}
			},
		})
		require.NoError(t, err)
		require.NotNil(t, created)
		assert.False(t, pendingAtCommit, "normal create without EventFile must not set HomeFilesPending")
		stored, err := f.store.GetAgent(ctx, created.ID)
		require.NoError(t, err)
		assert.False(t, agentHomeFilesPending(stored))
	})
}

type noopStartTrackingDispatcher struct {
	AgentDispatcher
	startCalls int
}

func (d *noopStartTrackingDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string, _ bool) error {
	d.startCalls++
	return nil
}

type earlyLaunchReportDispatcher struct {
	*HTTPAgentDispatcher
	srv        *Server
	t          *testing.T
	brokerID   string
	reportSent bool
}

func (d *earlyLaunchReportDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	if _, err := d.HTTPAgentDispatcher.DispatchAgentCreateWithGather(ctx, agent); err != nil {
		return nil, err
	}
	launchID, err := d.srv.store.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	// Drive the real HTTP launch-report handler (state=succeeded) BEFORE
	// createAgentHeadless reaches persistAcceptedLaunch.
	rec := postLaunchReport(d.t, d.srv, d.brokerID, agent.ID, d.brokerID, AgentLaunchReport{
		LaunchID:   launchID,
		InstanceID: "broker-inst-1",
		Seq:        1,
		State:      store.LaunchReportStateSucceeded,
		Phase:      "running",
	})
	require.Equal(d.t, http.StatusOK, rec.Code, "launch report body: %s", rec.Body.String())
	d.reportSent = true
	return &CreateDispatchResult{Launch: &LaunchAccepted{ID: launchID}}, nil
}

type conflictUpdateAgentStore struct {
	store.Store
	fault              *storeFaultSwitch
	targetID           string
	conflictsRemaining int
	attempts           int
}

func (s *conflictUpdateAgentStore) UpdateAgent(ctx context.Context, agent *store.Agent) error {
	if s.fault.Active() && agent.ID == s.targetID {
		s.attempts++
		if s.conflictsRemaining > 0 {
			s.conflictsRemaining--
			return store.ErrVersionConflict
		}
	}
	return s.Store.UpdateAgent(ctx, agent)
}
