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
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
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
		return HeadlessCreate{
			ProjectID:      f.proj.ID,
			Authority:      auth,
			Identity:       identity,
			EdgeCeiling:    edgeCeiling,
			EdgeProvenance: edgeProv,
			AgentRole:      "",
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
			EventEnvKeyTriggerEventID: "tevt-123",
			EventEnvKeyTriggerID:      "trig-456",
		}
		agent, err := f.srv.createAgentHeadless(ctx, hc)
		require.NoError(t, err)

		stored, err := f.store.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, hc.Labels, stored.Labels)
		assert.Equal(t, "tevt-123", stored.AppliedConfig.Env[EventEnvKeyTriggerEventID])
		assert.Equal(t, "trig-456", stored.AppliedConfig.Env[EventEnvKeyTriggerID])

		req := client.lastCreateReq
		assert.Equal(t, "tevt-123", req.ResolvedEnv[EventEnvKeyTriggerEventID])
		assert.Equal(t, "trig-456", req.ResolvedEnv[EventEnvKeyTriggerID])
		assert.NotContains(t, resolvedSecretMap(req.ResolvedSecrets), EventEnvKeyTriggerEventID)
		assert.NotContains(t, resolvedSecretMap(req.ResolvedSecrets), EventEnvKeyTriggerID)
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

		// Enable HomeFiles capability on the Runtime Broker and verify transient delivery.
		broker, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
		require.NoError(t, err)
		broker.Capabilities = &store.BrokerCapabilities{HomeFiles: true}
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

	t.Run("non_empty_github_token_and_mismatched_edge_provenance_rejected", func(t *testing.T) {
		hcToken := baseHC("gh-token-unsupported")
		hcToken.GitHubToken = "installation"
		_, err := f.srv.createAgentHeadless(ctx, hcToken)
		require.ErrorIs(t, err, errHeadlessGitHubTokenUnsupported)
		f.assertNoChild(t, hcToken.Name)

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
}

func (s *getProjectFaultStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if !s.fault.Active() {
		return s.Store.GetProject(ctx, id)
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
