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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type mpChainP1Fixture struct {
	srv     *Server
	s       store.Store
	project *store.Project
	owner   *store.User
	alice   *store.User
	sender  *store.Agent
	peer    *store.Agent
	conv    *store.Conversation
}

func newMPChainP1Fixture(t *testing.T, policiesOn, chainsOn bool) *mpChainP1Fixture {
	t.Helper()
	srv, s := testServer(t)
	setServerExperimentDefault(t, srv, experiments.MessagePolicies, policiesOn)
	setServerExperimentDefault(t, srv, experiments.MessagePolicyChains, chainsOn)

	ctx := context.Background()
	db := openTestMemorySQLite(t, "sqlite")
	wcs := NewWebChatStore(db, "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	project := &store.Project{
		ID:   tid("mp-chain-p1-proj"),
		Name: "mp-chain-p1-proj",
		Slug: "mp-chain-p1-proj",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	owner := &store.User{
		ID:          tid("mp-chain-p1-owner"),
		Email:       "owner@example.com",
		DisplayName: "Owner",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now().UTC(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)
	bindProjectMember(t, s, project.ID, owner.ID)

	alice := &store.User{
		ID:          tid("mp-chain-p1-alice"),
		Email:       "alice@example.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now().UTC(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))
	ensureHubMembership(ctx, s, alice.ID)
	bindProjectMember(t, s, project.ID, alice.ID)

	sender := &store.Agent{
		ID:        tid("mp-chain-p1-sender"),
		Slug:      "sender-agent",
		Name:      "sender-agent",
		ProjectID: project.ID,
		OwnerID:   owner.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, sender))

	peer := &store.Agent{
		ID:        tid("mp-chain-p1-peer"),
		Slug:      "peer-agent",
		Name:      "peer-agent",
		ProjectID: project.ID,
		OwnerID:   owner.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, peer))

	conv := &store.Conversation{
		ID:          tid("mp-chain-p1-conv"),
		ProjectID:   &project.ID,
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:team-chat",
		DisplayName: "team-chat",
		DriftState:  "active",
	}
	require.NoError(t, s.CreateConversation(ctx, conv))
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID,
		PrincipalKind:  "agent",
		PrincipalID:    sender.ID,
		Role:           "member",
	}))
	require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID,
		PrincipalKind:  "user",
		PrincipalID:    alice.ID,
		Role:           "member",
	}))

	return &mpChainP1Fixture{
		srv:     srv,
		s:       s,
		project: project,
		owner:   owner,
		alice:   alice,
		sender:  sender,
		peer:    peer,
		conv:    conv,
	}
}

func (f *mpChainP1Fixture) ownerReq(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	ident := NewAuthenticatedUser(f.owner.ID, f.owner.Email, f.owner.DisplayName, f.owner.Role, "session")
	req = req.WithContext(contextWithIdentity(req.Context(), ident))
	rr := httptest.NewRecorder()
	prefix := "/api/v1/projects/" + f.project.ID + "/message-policies"
	rest := strings.TrimPrefix(path, prefix)
	f.srv.handleProjectMessagePolicies(rr, req, f.project.ID, rest)
	return rr
}

func (f *mpChainP1Fixture) sendOutbound(t *testing.T, req OutboundMessageRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(req)
	require.NoError(t, err)
	httpReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+f.project.ID+"/agents/"+f.sender.ID+"/outbound-message",
		bytes.NewReader(raw))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq = httpReq.WithContext(contextWithIdentity(httpReq.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: f.sender.ID},
		ProjectID: f.project.ID,
		Ancestry:  f.sender.Ancestry,
	}}))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, httpReq, f.sender.ID)
	return rr
}

func TestMessagePolicyChainsP1_AuthoringFlagOffAndValidatorMatrix(t *testing.T) {
	f := newMPChainP1Fixture(t, true, false)
	base := "/api/v1/projects/" + f.project.ID + "/message-policies"

	validCreate := CreateMessagePolicyRequest{
		Name: "chain-p1-alpha",
		Mode: store.MessagePolicyModeEnforce,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{store.MessagePolicySurfaceAgentToUser, store.MessagePolicySurfaceAgentToConversation},
		},
		Steps: []store.MessagePolicyStep{
			{
				ID:      "only_user",
				Role:    store.MessagePolicyStepRoleGuard,
				Dialect: store.MessagePolicyStepDialectCEL,
				Expr:    `surface == "agent_to_user"`,
			},
			{
				ID:      "no_secret",
				Role:    store.MessagePolicyStepRoleViolation,
				Dialect: store.MessagePolicyStepDialectCEL,
				Expr:    `message.body.contains("SECRET")`,
				Reason:  "Step blocked secret for {recipient}.",
			},
		},
		OnFailed:     store.MessagePolicyOutcomeReject,
		RejectReason: "Chain default reject.",
	}

	// 1. Flag OFF -> POST with steps returns 400 experiment_disabled.
	rr := f.ownerReq(t, http.MethodPost, base, validCreate)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), ErrCodeExperimentDisabled)

	// 2. Flag ON -> POST valid CEL-only chain succeeds 201 and stores fail-closed sentinel.
	setServerExperimentDefault(t, f.srv, experiments.MessagePolicyChains, true)
	rr = f.ownerReq(t, http.MethodPost, base, validCreate)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	var created MessagePolicyResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &created))
	assert.Equal(t, store.MessagePolicyChainSourceAuthored, created.ChainSource)
	require.Len(t, created.Steps, 2)
	assert.Equal(t, "Chain default reject.", created.RejectReason)

	// Verify DB row holds fail-closed legacy sentinel (D6).
	stored, err := f.s.GetMessagePolicy(context.Background(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, messagepolicy.ChainLegacySentinelViolationRule, stored.ViolationRule)
	assert.Equal(t, messagepolicy.ChainLegacySentinelRejectReason("chain-p1-alpha"), stored.RejectReason)
	assert.Empty(t, stored.SystemOneRule)
	require.NotNil(t, stored.Chain)

	// 3. PATCH mode->shadow and scope->agent_dm rejected 400 not_yet_supported (phase acceptance invariant).
	shadow := store.MessagePolicyModeShadow
	rr = f.ownerReq(t, http.MethodPatch, base+"/"+created.ID, PatchMessagePolicyRequest{Mode: &shadow})
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), messagepolicy.ErrCodeNotYetSupported)

	dmScope := store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentDM}}
	rr = f.ownerReq(t, http.MethodPatch, base+"/"+created.ID, PatchMessagePolicyRequest{Scope: &dmScope})
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), messagepolicy.ErrCodeNotYetSupported)

	// 4. POST /test on chain policy returns 400 not_yet_supported (never evaluates sentinel).
	rr = f.ownerReq(t, http.MethodPost, base+"/"+created.ID+"/test", TestMessagePolicyRequest{
		Surface: store.MessagePolicySurfaceAgentToUser,
		Body:    "clean",
	})
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), messagepolicy.ErrCodeNotYetSupported)

	// 5. Legacy policy GET returns read-time legacy_mapped projection, and PATCH conversion is 400 in P1.
	legacyReq := CreateMessagePolicyRequest{
		Name:          "legacy-cel",
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: `message.body.contains("LEGACY_BAD")`,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}
	rr = f.ownerReq(t, http.MethodPost, base, legacyReq)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var legacyResp MessagePolicyResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &legacyResp))
	assert.Equal(t, store.MessagePolicyChainSourceLegacyMapped, legacyResp.ChainSource)
	require.Len(t, legacyResp.Steps, 1)
	assert.Equal(t, "violation", legacyResp.Steps[0].ID)

	convertSteps := validCreate.Steps
	rr = f.ownerReq(t, http.MethodPatch, base+"/"+legacyResp.ID, PatchMessagePolicyRequest{Steps: &convertSteps})
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), messagepolicy.ErrCodeNotYetSupported)

	// 6. Flag OFF again: editing chain policy fails 400 experiment_disabled,
	// but disable-only PATCH and DELETE remain allowed (D3).
	setServerExperimentDefault(t, f.srv, experiments.MessagePolicyChains, false)
	desc := "blocked edit"
	rr = f.ownerReq(t, http.MethodPatch, base+"/"+created.ID, PatchMessagePolicyRequest{Description: &desc})
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), ErrCodeExperimentDisabled)

	disabled := false
	rr = f.ownerReq(t, http.MethodPatch, base+"/"+created.ID, PatchMessagePolicyRequest{Enabled: &disabled})
	assert.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	rr = f.ownerReq(t, http.MethodDelete, base+"/"+created.ID, nil)
	assert.Equal(t, http.StatusNoContent, rr.Code)
}

func TestMessagePolicyChainsP1_E1E2Gate_SkipRejectDisabledAndUnsupported(t *testing.T) {
	f := newMPChainP1Fixture(t, true, true)
	ctx := context.Background()
	base := "/api/v1/projects/" + f.project.ID + "/message-policies"

	// Create a P1 CEL-only chain policy on agent_to_user + agent_to_conversation.
	createReq := CreateMessagePolicyRequest{
		Name: "e1e2-chain",
		Mode: store.MessagePolicyModeEnforce,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{store.MessagePolicySurfaceAgentToUser, store.MessagePolicySurfaceAgentToConversation},
		},
		Steps: []store.MessagePolicyStep{
			{
				ID:      "only_user",
				Role:    store.MessagePolicyStepRoleGuard,
				Dialect: store.MessagePolicyStepDialectCEL,
				Expr:    `surface == "agent_to_user"`,
			},
			{
				ID:      "no_secret",
				Role:    store.MessagePolicyStepRoleViolation,
				Dialect: store.MessagePolicyStepDialectCEL,
				Expr:    `message.body.contains("SECRET_WORD")`,
				Reason:  "Step custom reason: no secrets to {recipient}",
			},
		},
		OnFailed: store.MessagePolicyOutcomeAdmit,
		OnFailedReasons: map[string]string{
			messagepolicy.ReasonChainDisabled: store.MessagePolicyOutcomeReject,
		},
		RejectReason: "Chain default reason",
	}
	rr := f.ownerReq(t, http.MethodPost, base, createReq)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var polResp MessagePolicyResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &polResp))

	// 1. E2 conversation send: guard false -> skip_policy, send admitted (200),
	// decision row carries content-free chain provenance.
	rr = f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "conv:" + f.conv.ID,
		Msg:       "contains SECRET_WORD in group chat",
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: f.project.ID,
		PolicyID:  polResp.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, decs.Items, 1)
	assert.Equal(t, store.MessagePolicyClassPass, decs.Items[0].Class)
	assert.Equal(t, store.MessagePolicyActionAdmitted, decs.Items[0].Action)
	require.NotEmpty(t, decs.Items[0].Chain)
	assert.NotContains(t, string(decs.Items[0].Chain), "SECRET_WORD")
	assert.NotContains(t, string(decs.Items[0].Chain), "Step custom reason")
	var prov messagepolicy.ChainDecisionProvenance
	require.NoError(t, json.Unmarshal(decs.Items[0].Chain, &prov))
	assert.Equal(t, 1, prov.Through)
	assert.Equal(t, "fail", prov.EngineClass)
	assert.Equal(t, messagepolicy.HandlingSkipPolicy, prov.ConsumerHandling)
	assert.Equal(t, messagepolicy.ChainVerdictSkipPolicy, prov.Verdict)
	assert.Equal(t, []int{1}, prov.Unexecuted)

	// 2. E1 user send: guard true + violation true -> 422 with step reason (not sentinel).
	rr = f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.ID,
		Msg:       "leaking SECRET_WORD to user",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "Step custom reason: no secrets to")
	assert.NotContains(t, rr.Body.String(), "requires a newer Hub")

	decs, err = f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: f.project.ID,
		PolicyID:  polResp.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, decs.Items, 2)
	latest := decs.Items[0]
	assert.Equal(t, store.MessagePolicyClassViolation, latest.Class)
	assert.Equal(t, store.MessagePolicyActionRejected, latest.Action)
	require.NoError(t, json.Unmarshal(latest.Chain, &prov))
	assert.Equal(t, 2, prov.Through)
	require.NotNil(t, prov.Deciding)
	assert.Equal(t, "no_secret", prov.Deciding.ID)
	assert.Equal(t, "pass", prov.EngineClass)
	assert.Equal(t, messagepolicy.HandlingRejectViolation, prov.ConsumerHandling)
	assert.Equal(t, messagepolicy.ChainVerdictReject, prov.Verdict)
	assert.NotContains(t, string(latest.Chain), "SECRET_WORD")
	assert.NotContains(t, string(latest.Chain), "Step custom reason")

	// 3. Flag OFF -> failed/chain_disabled resolved via on_failed_reasons["chain_disabled"]=reject (D3).
	setServerExperimentDefault(t, f.srv, experiments.MessagePolicyChains, false)
	rr = f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.ID,
		Msg:       "clean message while flag off",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	decs, err = f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: f.project.ID,
		PolicyID:  polResp.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, store.MessagePolicyClassFailed, decs.Items[0].Class)
	assert.Equal(t, messagepolicy.ReasonChainDisabled, decs.Items[0].CELErrorReason)
	assert.Equal(t, store.MessagePolicyActionRejected, decs.Items[0].Action)

	// Re-enable flag and disable e1e2-chain so we can test non-P1 unsupported paths cleanly.
	setServerExperimentDefault(t, f.srv, experiments.MessagePolicyChains, true)
	disabled := false
	rr = f.ownerReq(t, http.MethodPatch, base+"/"+polResp.ID, PatchMessagePolicyRequest{Enabled: &disabled})
	require.Equal(t, http.StatusOK, rr.Code)

	// 4. Persisted chain policy on non-P1 paths (shadow, agent_dm, mention_delivery)
	// resolves as failed/chain_unsupported via on_failed=admit with NO sentinel evaluation (D4).
	seedUnsupported := &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "seeded-unsupported-chain",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentDM, store.MessagePolicySurfaceMentionDelivery}},
		ViolationRule: messagepolicy.ChainLegacySentinelViolationRule,
		RejectReason:  messagepolicy.ChainLegacySentinelRejectReason("seeded-unsupported-chain"),
		Chain: &store.MessagePolicyChain{
			Version: store.MessagePolicyChainVersion,
			Steps: []store.MessagePolicyStep{
				{ID: "v1", Role: store.MessagePolicyStepRoleViolation, Dialect: store.MessagePolicyStepDialectCEL, Expr: "false"},
			},
		},
		OnFailed: store.MessagePolicyOutcomeAdmit,
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, seedUnsupported))

	// Evaluate on agent_dm origin unit directly via evaluateOriginMessagePolicies:
	// must NOT evaluate the sentinel ("true" -> violation/reject); must classify
	// failed/chain_unsupported and admit via on_failed=admit.
	out, rej := f.srv.evaluateOriginMessagePolicies(ctx, originPolicyEvalInput{
		sender: f.sender,
		originSnap: h2FactSnapshot{
			surface:       store.MessagePolicySurfaceAgentDM,
			projectID:     f.project.ID,
			senderID:      f.sender.ID,
			senderSlug:    f.sender.Slug,
			recipientKind: "agent",
			recipientID:   f.peer.ID,
			recipientSlug: f.peer.Slug,
			msgType:       "message",
			msgBody:       "dm hello",
			introBody:     "dm hello",
		},
	})
	require.Nil(t, rej, "chain_unsupported with on_failed=admit must admit, not execute sentinel")
	require.Len(t, out.admittedDecisions, 1)
	assert.Equal(t, store.MessagePolicyClassFailed, out.admittedDecisions[0].Class)
	assert.Equal(t, messagepolicy.ReasonChainUnsupported, out.admittedDecisions[0].CELErrorReason)

	// Also verify shadow mode on a seeded chain policy -> failed/chain_unsupported, never sentinel.
	seedShadow := &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "seeded-shadow-chain",
		Enabled:       true,
		Mode:          store.MessagePolicyModeShadow,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: messagepolicy.ChainLegacySentinelViolationRule,
		RejectReason:  messagepolicy.ChainLegacySentinelRejectReason("seeded-shadow-chain"),
		Chain: &store.MessagePolicyChain{
			Version: store.MessagePolicyChainVersion,
			Steps: []store.MessagePolicyStep{
				{ID: "v1", Role: store.MessagePolicyStepRoleViolation, Dialect: store.MessagePolicyStepDialectCEL, Expr: "false"},
			},
		},
		OnFailed: store.MessagePolicyOutcomeAdmit,
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, seedShadow))

	rows := f.srv.evaluateMatchedPoliciesAcrossUnits(
		ctx,
		[]matchedPolicyUnits{{policy: *seedShadow, unitIdxs: []int{0}}},
		[]policyEvalUnit{{
			isOrigin: true,
			hasFacts: true,
			snap: h2FactSnapshot{
				surface:   store.MessagePolicySurfaceAgentToUser,
				projectID: f.project.ID,
				msgBody:   "hello",
			},
		}},
		f.project.ID,
		messagepolicy.NewSendBudget(),
		messagepolicy.DefaultSystemOneSendBudget,
		store.MessagePolicyModeShadow,
	)
	require.Len(t, rows, 1)
	assert.Equal(t, messagepolicy.ClassFailed, rows[0].classification.Class)
	assert.Equal(t, messagepolicy.ReasonChainUnsupported, rows[0].classification.Reason)
	assert.Equal(t, messagepolicy.OutcomeAdmit, rows[0].outcome)

	// 5. Legacy policy continues evaluating on today's path unchanged.
	legacyPol := &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "legacy-still-works",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: `message.body.contains("LEGACY_TRIGGER")`,
		OnFailed:      store.MessagePolicyOutcomeReject,
		RejectReason:  "Legacy blocked it",
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, legacyPol))

	rr = f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.ID,
		Msg:       "contains LEGACY_TRIGGER",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "Legacy blocked it")
}
