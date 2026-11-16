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
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mpgOneDecision = `[{"id":"D234","question":"Ship the **feature** now?","choices":["yes","no"],"default":"yes"}]`

// mpgSetExperiment toggles hub.message_policies (and optionally web.decisions)
// on srv without touching other registered experiment defaults.
func mpgSetExperiment(t testing.TB, srv *Server, policiesOn, choicesOn bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		switch e.Name {
		case experiments.MessagePolicies:
			e.Default = policiesOn
		case experiments.Decisions:
			e.Default = choicesOn
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, policiesOn, srv.experimentEnabled(experiments.MessagePolicies))
}

// mpgFaultStore tracks and optionally fails MessagePolicyStore reads/writes so
// tests can prove the gate loads nothing when hub.message_policies is off.
type mpgFaultStore struct {
	store.Store
	fault         *storeFaultSwitch
	listCalls     atomic.Int64
	decisionCalls atomic.Int64
}

func (f *mpgFaultStore) ListMessagePolicies(ctx context.Context, filter store.MessagePolicyFilter, opts store.ListOptions) (*store.ListResult[store.MessagePolicy], error) {
	f.listCalls.Add(1)
	if f.fault.Active() {
		return nil, errors.New("unexpected ListMessagePolicies call while experiment is off")
	}
	return f.Store.ListMessagePolicies(ctx, filter, opts)
}

func (f *mpgFaultStore) CreateMessagePolicyDecision(ctx context.Context, d *store.MessagePolicyDecision) error {
	f.decisionCalls.Add(1)
	if f.fault.Active() {
		return errors.New("unexpected CreateMessagePolicyDecision call while experiment is off")
	}
	return f.Store.CreateMessagePolicyDecision(ctx, d)
}

// mpgRecordingChannel captures channel deliveries on the direct user path.
type mpgRecordingChannel struct {
	mu   sync.Mutex
	msgs []*messages.StructuredMessage
}

func (c *mpgRecordingChannel) Name() string    { return "mpg-recording" }
func (c *mpgRecordingChannel) Validate() error { return nil }
func (c *mpgRecordingChannel) Deliver(_ context.Context, msg *messages.StructuredMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, msg)
	return nil
}

func (c *mpgRecordingChannel) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}

func (c *mpgRecordingChannel) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = nil
}

// mpgFixture sets up a server, project, human member Alice, sender/peer/default
// agents, a group conversation with a default agent, a webChatStore + local-disk
// attachment store + scratchpad directory, a recording dispatcher, and a
// recording notification channel.
type mpgFixture struct {
	srv           *Server
	s             store.Store
	faultWrapper  *mpgFaultStore
	faultSwitch   *storeFaultSwitch
	project       *store.Project
	alice         *store.User
	sender        *store.Agent
	peerAgent     *store.Agent
	defaultAgent  *store.Agent
	groupConv     *store.Conversation
	wcs           WebChatStore
	attachDir     string
	sharedDir     string
	stagedPath    string
	dispatcher    *recordingDispatcher
	recordChannel *mpgRecordingChannel
}

func newMpgFixture(t *testing.T, policiesOn bool) *mpgFixture {
	t.Helper()
	srv, s, faultWrapper, faultSwitch := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *mpgFaultStore {
		return &mpgFaultStore{Store: inner, fault: f}
	})
	mpgSetExperiment(t, srv, policiesOn, true)

	ctx := context.Background()

	db := openTestMemorySQLite(t, "sqlite")
	wcs := NewWebChatStore(db, "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	attachDir := t.TempDir()
	as, err := NewLocalDiskAttachmentStore(attachDir)
	require.NoError(t, err)
	srv.SetAttachmentStore(as)

	project := &store.Project{
		ID:         tid("mpg-project"),
		Name:       "mpg-project",
		Slug:       "mpg-project",
		SharedDirs: []api.SharedDir{{Name: attachmentSharedDirName}},
	}
	require.NoError(t, s.CreateProject(ctx, project))

	home := t.TempDir()
	t.Setenv("HOME", home)
	sharedDir := config.SharedDirHostPath(home, project.Slug, project.ID, attachmentSharedDirName)
	require.NoError(t, os.MkdirAll(sharedDir, 0o755))
	stagedPath := stageAgentFile(t, sharedDir, "report.md", "# confidential attachment\n")

	alice := &store.User{
		ID:          tid("mpg-alice"),
		Email:       "alice@example.com",
		DisplayName: "Alice Example",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now().UTC(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))
	ensureHubMembership(ctx, s, alice.ID)
	bindProjectMember(t, s, project.ID, alice.ID)

	brokerID := tid("mpg-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "mpg-broker",
		Slug:   "mpg-broker",
		Status: store.BrokerStatusOnline,
	}))

	mkAgent := func(slug string) *store.Agent {
		a := &store.Agent{
			ID:              tid("mpg-agent-" + slug),
			Name:            slug,
			Slug:            slug,
			ProjectID:       project.ID,
			Phase:           "running",
			OwnerID:         alice.ID,
			CreatedBy:       alice.ID,
			RuntimeBrokerID: brokerID,
			MessageMode:     store.MessageModeProject,
			Ancestry:        []string{alice.ID},
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	sender := mkAgent("sender-agent")
	peerAgent := mkAgent("peer-agent")
	defaultAgent := mkAgent("default-agent")

	projID := project.ID
	defaultID := defaultAgent.ID
	groupConv := &store.Conversation{
		ID:             tid("mpg-group-conv"),
		ProjectID:      &projID,
		Kind:           "group",
		Surface:        "native",
		DisplayName:    "mpg-team-room",
		DriftState:     "active",
		DefaultAgentID: &defaultID,
	}
	require.NoError(t, s.CreateConversation(ctx, groupConv))
	require.NoError(t, s.EnsureParticipant(ctx, &store.ConversationParticipant{
		ConversationID: groupConv.ID,
		PrincipalKind:  "user",
		PrincipalID:    alice.ID,
		Role:           "member",
	}))
	require.NoError(t, s.EnsureParticipant(ctx, &store.ConversationParticipant{
		ConversationID: groupConv.ID,
		PrincipalKind:  "agent",
		PrincipalID:    sender.ID,
		Role:           "member",
	}))
	require.NoError(t, s.EnsureParticipant(ctx, &store.ConversationParticipant{
		ConversationID: groupConv.ID,
		PrincipalKind:  "agent",
		PrincipalID:    defaultAgent.ID,
		Role:           "member",
	}))

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	recCh := &mpgRecordingChannel{}
	srv.SetChannelRegistry(&ChannelRegistry{
		entries: []channelEntry{{channel: recCh, config: ChannelConfig{Type: "mpg-recording"}}},
		log:     slog.Default(),
	})

	return &mpgFixture{
		srv:           srv,
		s:             s,
		faultWrapper:  faultWrapper,
		faultSwitch:   faultSwitch,
		project:       project,
		alice:         alice,
		sender:        sender,
		peerAgent:     peerAgent,
		defaultAgent:  defaultAgent,
		groupConv:     groupConv,
		wcs:           wcs,
		attachDir:     attachDir,
		sharedDir:     sharedDir,
		stagedPath:    stagedPath,
		dispatcher:    dispatcher,
		recordChannel: recCh,
	}
}

func (f *mpgFixture) sendOutbound(t testing.TB, req OutboundMessageRequest) *httptest.ResponseRecorder {
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

// TestMessagePolicyGate_FlagOff_LoadsNothingAndDelivers verifies Target 1 (gate
// half): when hub.message_policies is off, the H2 gate loads zero policies,
// records zero decisions, and keeps user DM, group conversation, and choice
// prompt sends byte-identical even if a policy exists in the database and the
// policy store is faulted.
func TestMessagePolicyGate_FlagOff_LoadsNothingAndDelivers(t *testing.T) {
	f := newMpgFixture(t, false)
	ctx := context.Background()

	// Seed an active enforce reject-all policy directly on the underlying store.
	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID: f.project.ID,
		Name:      "reject-everything",
		Enabled:   true,
		Mode:      store.MessagePolicyModeEnforce,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{store.MessagePolicySurfaceAgentToUser, store.MessagePolicySurfaceAgentToConversation},
			Sender:   store.MessagePolicySenderSelector{AllAgents: true},
		},
		ViolationRule: "true",
		OnFailed:      messagepolicy.OutcomeReject,
		RejectReason:  "should never run when flag is off",
	}))

	// Arm store fault on ListMessagePolicies / CreateMessagePolicyDecision.
	f.faultSwitch.Arm()

	// 1. User DM succeeds.
	rrDM := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Option A or Option B?",
	})
	require.Equal(t, http.StatusOK, rrDM.Code, "body: %s", rrDM.Body.String())

	// 2. Group conversation post succeeds.
	rrGroup := f.sendOutbound(t, OutboundMessageRequest{
		ConversationRef: "conv:" + f.groupConv.ID,
		Msg:             "Group post while flag is off",
	})
	require.Equal(t, http.StatusOK, rrGroup.Code, "body: %s", rrGroup.Body.String())

	// 3. Choice prompt succeeds.
	var choices []messages.DecisionInput
	require.NoError(t, json.Unmarshal([]byte(mpgOneDecision), &choices))
	rrChoice := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Choose one:",
		Choices:   choices,
	})
	require.Equal(t, http.StatusOK, rrChoice.Code, "body: %s", rrChoice.Body.String())

	assert.Zero(t, f.faultWrapper.listCalls.Load(), "flag off must not call ListMessagePolicies")
	assert.Zero(t, f.faultWrapper.decisionCalls.Load(), "flag off must not call CreateMessagePolicyDecision")

	decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: f.project.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, decs.Items)
}

// TestMessagePolicyGate_H2EnforceViolation_ZeroSideEffectsAnd422WireShape
// verifies Target 2 and Target 8: an H2 enforce violation returns HTTP 422
// message_policy_rejected with rendered reason/remediation and typed details,
// writes NO message row, ingests NO attachments, triggers NO mention fan-out,
// wakes NO default agent, dispatches NO channel message, preserves the
// rate-limit charge, and writes a content-free action=rejected decision row.
func TestMessagePolicyGate_H2EnforceViolation_ZeroSideEffectsAnd422WireShape(t *testing.T) {
	f := newMpgFixture(t, true)
	ctx := context.Background()

	pol := &store.MessagePolicy{
		ProjectID: f.project.ID,
		Name:      "no-prose-choices",
		Enabled:   true,
		Mode:      store.MessagePolicyModeShadow,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{
				store.MessagePolicySurfaceAgentToUser,
				store.MessagePolicySurfaceAgentToConversation,
			},
			Sender: store.MessagePolicySenderSelector{AllAgents: true},
		},
		ViolationRule:     `recipient.kind != "agent" && !message.has_choices && message.body.matches("(?i)\\b(option a|option b)\\b")`,
		OnFailed:          messagepolicy.OutcomeReject,
		RejectReason:      "Policy {policy} blocked prose choices to {recipient}",
		RejectRemediation: "Resend with --choices-file (see scion-messaging)",
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, pol))
	// Promote to enforce so revision is 2.
	pol.Mode = store.MessagePolicyModeEnforce
	require.NoError(t, f.s.UpdateMessagePolicy(ctx, pol))
	require.Equal(t, int64(2), pol.Revision)

	const secretMarker = "TOP-SECRET-BODY-TOKEN-987654"
	violatingBody := fmt.Sprintf("%s: Do you prefer Option A or Option B? @%s @%s", secretMarker, f.peerAgent.Slug, f.alice.Email)

	// Case A: Agent -> user DM violating the policy with attachment + @agent mention.
	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient:   "user:" + f.alice.Email,
		Msg:         violatingBody,
		Attachments: []string{f.stagedPath},
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
	assert.Equal(t, string(MessageDenialPolicyRejected), errResp.Error.Code)
	assert.Equal(t, "Policy no-prose-choices blocked prose choices to alice@example.com", errResp.Error.Message)
	assert.NotContains(t, rr.Body.String(), secretMarker, "422 response must never echo message body")
	require.NotNil(t, errResp.Error.Details)
	assert.Equal(t, "no-prose-choices", errResp.Error.Details["policy"])
	assert.Equal(t, float64(2), errResp.Error.Details["policy_revision"])
	assert.Equal(t, messagepolicy.ClassViolation, errResp.Error.Details["class"])
	assert.Equal(t, "Resend with --choices-file (see scion-messaging)", errResp.Error.Details["remediation"])

	// Zero message rows persisted.
	msgs, err := f.s.ListMessages(ctx, store.MessageFilter{SenderID: f.sender.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "rejected user DM must persist zero message rows")

	// Zero attachment ingestion on disk.
	entries, err := os.ReadDir(f.attachDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "rejected user DM must ingest zero attachment files")

	// Zero mention fan-out dispatches to @peer-agent and zero channel dispatches.
	assert.Empty(t, f.dispatcher.getCalls(), "rejected user DM must trigger zero agent dispatches")
	assert.Zero(t, f.recordChannel.count(), "rejected user DM must trigger zero channel dispatches")

	// Zero webchat_dm participant registrations.
	dms, err := f.wcs.ListDMs(ctx, f.alice.ID)
	require.NoError(t, err)
	assert.Empty(t, dms, "rejected user DM must not register webchat_dm rows")

	// Case B: Agent -> group conversation post violating the policy (has defaultAgent + @peer-agent + @alice).
	rrGroup := f.sendOutbound(t, OutboundMessageRequest{
		ConversationRef: "conv:" + f.groupConv.ID,
		Msg:             violatingBody,
		Attachments:     []string{f.stagedPath},
	})
	require.Equal(t, http.StatusUnprocessableEntity, rrGroup.Code, "body: %s", rrGroup.Body.String())

	var groupErrResp ErrorResponse
	require.NoError(t, json.Unmarshal(rrGroup.Body.Bytes(), &groupErrResp))
	assert.Equal(t, string(MessageDenialPolicyRejected), groupErrResp.Error.Code)
	assert.Equal(t, "Policy no-prose-choices blocked prose choices to mpg-team-room", groupErrResp.Error.Message)

	msgs, err = f.s.ListMessages(ctx, store.MessageFilter{SenderID: f.sender.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "rejected group post must persist zero message rows")
	assert.Empty(t, f.dispatcher.getCalls(), "rejected group post must neither fan out mentions nor wake default agent")
	assert.Zero(t, f.recordChannel.count(), "rejected group post must trigger zero channel dispatches")

	// Verify both rejected decision rows are persisted and completely content-free.
	decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: f.project.ID,
		PolicyID:  pol.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, decs.Items, 2)

	// Newest first: index 0 is group post, index 1 is user DM.
	groupDec := decs.Items[0]
	assert.Equal(t, store.MessagePolicyActionRejected, groupDec.Action)
	assert.Equal(t, store.MessagePolicyClassViolation, groupDec.Class)
	assert.Equal(t, int64(2), groupDec.PolicyRevision)
	assert.Equal(t, store.MessagePolicySurfaceAgentToConversation, groupDec.Surface)
	assert.Equal(t, "agent", groupDec.SenderKind)
	assert.Equal(t, f.sender.ID, groupDec.SenderID)
	assert.Equal(t, "conversation", groupDec.RecipientKind)
	assert.Equal(t, f.groupConv.ID, groupDec.RecipientID)
	assert.Empty(t, groupDec.MessageID, "rejected decision must have empty message_id")

	dmDec := decs.Items[1]
	assert.Equal(t, store.MessagePolicyActionRejected, dmDec.Action)
	assert.Equal(t, store.MessagePolicyClassViolation, dmDec.Class)
	assert.Equal(t, store.MessagePolicySurfaceAgentToUser, dmDec.Surface)
	assert.Equal(t, "user", dmDec.RecipientKind)
	assert.Equal(t, f.alice.ID, dmDec.RecipientID)
	assert.Empty(t, dmDec.MessageID)

	rawDecs, err := json.Marshal(decs.Items)
	require.NoError(t, err)
	assert.NotContains(t, string(rawDecs), secretMarker, "decision rows must never contain message text")
	assert.NotContains(t, string(rawDecs), "Option A", "decision rows must never contain message text")

	// Case C: Resending with structured choices passes the policy and delivers.
	var choices []messages.DecisionInput
	require.NoError(t, json.Unmarshal([]byte(mpgOneDecision), &choices))
	rrAdmit := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Do you prefer Option A or Option B?",
		Choices:   choices,
	})
	require.Equal(t, http.StatusOK, rrAdmit.Code, "body: %s", rrAdmit.Body.String())
	var okResp struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rrAdmit.Body.Bytes(), &okResp))
	require.NotEmpty(t, okResp.MessageID)

	admittedDecs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: f.project.ID,
		Action:    store.MessagePolicyActionAdmitted,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, admittedDecs.Items, 1)
	assert.Equal(t, store.MessagePolicyClassPass, admittedDecs.Items[0].Class)
	assert.Equal(t, okResp.MessageID, admittedDecs.Items[0].MessageID)

	// Case D: Rate-limit charge stands even when H2 rejects.
	fixedNow := time.Now().UTC()
	f.srv.chatSendLimiter = newChatSendLimiterWithRates(map[chatSenderClass]float64{
		chatSenderHuman: 1,
		chatSenderAgent: 1,
	}, func() time.Time { return fixedNow })
	rrBurst := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Option A or Option B?",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rrBurst.Code)
	rrNext := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Clean follow-up immediately after rejection",
	})
	assert.Equal(t, http.StatusTooManyRequests, rrNext.Code,
		"rate-limit token charged before H2 must stand after a 422 policy rejection")
}

// TestMessagePolicyGate_DefaultReasonAndOmitEmptyRemediation verifies that an
// enforce policy without custom reject_reason / reject_remediation uses the
// default reason and omits remediation from 422 error details.
func TestMessagePolicyGate_DefaultReasonAndOmitEmptyRemediation(t *testing.T) {
	f := newMpgFixture(t, true)
	ctx := context.Background()

	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID: f.project.ID,
		Name:      "bare-policy",
		Enabled:   true,
		Mode:      store.MessagePolicyModeEnforce,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
			Sender:   store.MessagePolicySenderSelector{AllAgents: true},
		},
		ViolationRule: "true",
		OnFailed:      messagepolicy.OutcomeReject,
	}))

	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "hello",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code)

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
	assert.Equal(t, messagepolicy.DefaultRejectReason("bare-policy"), errResp.Error.Message)
	_, hasRemediation := errResp.Error.Details["remediation"]
	assert.False(t, hasRemediation, "empty remediation must be omitted from details")
}

// TestMessagePolicyGate_MultipleEnforceOrderingAndCap10 verifies deterministic
// name-order evaluation across multiple enforce policies, first-violation
// selection for the 422 response, recording of all evaluated policies, and the
// per-send cap of 10 active matching policies.
func TestMessagePolicyGate_MultipleEnforceOrderingAndCap10(t *testing.T) {
	f := newMpgFixture(t, true)
	ctx := context.Background()

	// Seed 12 enabled matching policies (p01..p12); p02 and p05 violate.
	for i := 1; i <= 12; i++ {
		name := fmt.Sprintf("p%02d", i)
		rule := "false"
		if i == 2 || i == 5 {
			rule = "true"
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      name,
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: rule,
			OnFailed:      messagepolicy.OutcomeReject,
			RejectReason:  "rejected by {policy}",
		}))
	}

	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "test multi-policy ordering",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code)

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
	assert.Equal(t, "p02", errResp.Error.Details["policy"], "first violating policy in name ASC order wins the 422 response")
	assert.Equal(t, "rejected by p02", errResp.Error.Message)

	decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: f.project.ID}, store.ListOptions{Limit: 20})
	require.NoError(t, err)
	require.Len(t, decs.Items, store.MaxActiveMatchingMessagePolicies, "at most 10 active matching policies are evaluated per send")

	byName := make(map[string]store.MessagePolicyDecision, len(decs.Items))
	for _, d := range decs.Items {
		byName[d.PolicyName] = d
		assert.Empty(t, d.MessageID, "overall send was rejected so every decision row must have empty message_id")
	}
	assert.NotContains(t, byName, "p11")
	assert.NotContains(t, byName, "p12")
	assert.Equal(t, store.MessagePolicyActionAdmitted, byName["p01"].Action)
	assert.Equal(t, store.MessagePolicyActionRejected, byName["p02"].Action)
	assert.Equal(t, store.MessagePolicyActionRejected, byName["p05"].Action)
}

// TestMessagePolicyGate_ShadowMode_DeliversAndRecordsOffHotPath verifies Target 3:
// shadow policies never block delivery, record action=shadow asynchronously with
// the persisted message_id and no message text, and drop gracefully with a
// metric/log rather than spawning unbounded goroutines when saturated.
func TestMessagePolicyGate_ShadowMode_DeliversAndRecordsOffHotPath(t *testing.T) {
	f := newMpgFixture(t, true)
	ctx := context.Background()

	pol := &store.MessagePolicy{
		ProjectID: f.project.ID,
		Name:      "shadow-prose-choices",
		Enabled:   true,
		Mode:      store.MessagePolicyModeShadow,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
			Sender:   store.MessagePolicySenderSelector{AllAgents: true},
		},
		ViolationRule: `message.body.matches("(?i)option a")`,
		OnFailed:      messagepolicy.OutcomeReject,
		RejectReason:  "shadow hit on {policy}",
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, pol))

	const secretText = "SHADOW-SECRET-PAYLOAD-445566"
	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       secretText + " Option A please",
	})
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var okResp struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &okResp))
	require.NotEmpty(t, okResp.MessageID)

	// Wait for the bounded shadow worker pool to persist the decision row.
	var got store.MessagePolicyDecision
	require.Eventually(t, func() bool {
		decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
			PolicyID:  pol.ID,
		}, store.ListOptions{Limit: 5})
		if err != nil || len(decs.Items) == 0 {
			return false
		}
		got = decs.Items[0]
		return true
	}, 3*time.Second, 15*time.Millisecond)

	assert.Equal(t, store.MessagePolicyModeShadow, got.Mode)
	assert.Equal(t, store.MessagePolicyClassViolation, got.Class)
	assert.Equal(t, store.MessagePolicyActionShadow, got.Action)
	assert.Equal(t, okResp.MessageID, got.MessageID)
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), secretText)
}

// TestMessagePolicyGate_ShadowPoolBoundedDrop verifies that when all shadow
// workers are blocked and the bounded queue is full, sends return immediately
// and count drops rather than blocking or launching unbounded goroutines.
func TestMessagePolicyGate_ShadowPoolBoundedDrop(t *testing.T) {
	f := newMpgFixture(t, true)
	ctx := context.Background()

	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID: f.project.ID,
		Name:      "shadow-drop-probe",
		Enabled:   true,
		Mode:      store.MessagePolicyModeShadow,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
			Sender:   store.MessagePolicySenderSelector{AllAgents: true},
		},
		ViolationRule: "true",
		OnFailed:      messagepolicy.OutcomeReject,
	}))

	// Disable chat send rate limit for this queue-capacity test so rapid sends
	// reach the shadow pool rather than getting 429'd before the gate.
	f.srv.chatSendLimiter = nil

	unblock := make(chan struct{})
	f.srv.messagePolicyGate.testShadowBeforeEval = func() {
		<-unblock
	}
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	}()

	totalSends := messagePolicyShadowWorkers + messagePolicyShadowQueueCap + 16
	for i := 0; i < totalSends; i++ {
		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       fmt.Sprintf("burst message %d", i),
		})
		require.Equal(t, http.StatusOK, rr.Code)
	}

	assert.GreaterOrEqual(t, f.srv.messagePolicyShadowDrops.Load(), int64(1),
		"overflow past bounded shadow queue must drop and increment drop counter")

	close(unblock)
}

// TestMessagePolicyGate_FailuresAndBudgetExhaustion verifies Target 4 and 4a:
// missing judgment inputs, CEL runtime errors, cost_limit errors, and 250ms
// per-send BudgetTracker exhaustion classify as failed with their exact CEL
// reason and map strictly via on_failed / on_failed_reasons.
func TestMessagePolicyGate_FailuresAndBudgetExhaustion(t *testing.T) {
	t.Run("missing_input_and_on_failed_reasons_override", func(t *testing.T) {
		f := newMpgFixture(t, true)
		ctx := context.Background()

		// Missing judgment key -> missing_input. Policy sets on_failed=reject
		// initially, then overrides missing_input=admit.
		pol := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "missing-judgment",
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: `judgment_flags.asks_choice`,
			OnFailed:      messagepolicy.OutcomeReject,
			RejectReason:  "policy failed closed",
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, pol))

		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "hello",
		})
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code)
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
		assert.Equal(t, messagepolicy.ClassFailed, errResp.Error.Details["class"])

		// Override missing_input -> admit while keeping on_failed -> reject.
		pol.OnFailedReasons = map[string]string{
			"missing_input": messagepolicy.OutcomeAdmit,
		}
		require.NoError(t, f.s.UpdateMessagePolicy(ctx, pol))

		rrAdmit := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "hello again",
		})
		require.Equal(t, http.StatusOK, rrAdmit.Code, "body: %s", rrAdmit.Body.String())

		decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
			PolicyID:  pol.ID,
		}, store.ListOptions{Limit: 5})
		require.NoError(t, err)
		require.Len(t, decs.Items, 2)
		assert.Equal(t, store.MessagePolicyClassFailed, decs.Items[0].Class)
		assert.Equal(t, "missing_input", decs.Items[0].CELErrorReason)
		assert.Equal(t, store.MessagePolicyActionAdmitted, decs.Items[0].Action)
		assert.Equal(t, store.MessagePolicyActionRejected, decs.Items[1].Action)
	})

	t.Run("cost_limit_and_runtime_error", func(t *testing.T) {
		f := newMpgFixture(t, true)
		ctx := context.Background()

		// Compact comprehension that passes static cost checks (< 4096 bytes) and
		// exceeds the 250k CEL runtime cost limit on a 16 KiB body.
		costExpr := `[1,2,3,4,5,6,7,8,9,10].all(a, [1,2,3,4,5,6,7,8,9,10].all(b, (string(message.body) + string(message.body)).size() >= a + b - 20))`

		polCost := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "01-cost-limit",
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: costExpr,
			OnFailed:      messagepolicy.OutcomeAdmit,
			OnFailedReasons: map[string]string{
				"cost_limit": messagepolicy.OutcomeReject,
			},
			RejectReason: "cost limit rejected",
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, polCost))

		fullBody := strings.Repeat("word ", 3000) // 15,000 chars (<= messages.MaxMessageLength=16,000)
		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       fullBody,
		})
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code)

		decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
			PolicyID:  polCost.ID,
		}, store.ListOptions{Limit: 5})
		require.NoError(t, err)
		require.Len(t, decs.Items, 1)
		assert.Equal(t, store.MessagePolicyClassFailed, decs.Items[0].Class)
		assert.Equal(t, "cost_limit", decs.Items[0].CELErrorReason)
		assert.Equal(t, store.MessagePolicyActionRejected, decs.Items[0].Action)

		// Also exercise runtime division-by-zero -> eval_error.
		polDiv := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "00-div-zero",
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: "1 / (message.has_choices ? 1 : 0) == 1",
			OnFailed:      messagepolicy.OutcomeReject,
			RejectReason:  "runtime eval error rejected",
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, polDiv))

		rrDiv := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "divide by zero",
		})
		require.Equal(t, http.StatusUnprocessableEntity, rrDiv.Code)

		divDecs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
			PolicyID:  polDiv.ID,
		}, store.ListOptions{Limit: 5})
		require.NoError(t, err)
		require.Len(t, divDecs.Items, 1)
		assert.Equal(t, store.MessagePolicyClassFailed, divDecs.Items[0].Class)
		assert.Equal(t, celengine.ReasonRuntime, divDecs.Items[0].CELErrorReason)
		assert.Equal(t, store.MessagePolicyActionRejected, divDecs.Items[0].Action)
	})

	t.Run("per_send_budget_exhausted_remaining_policies", func(t *testing.T) {
		f := newMpgFixture(t, true)
		ctx := context.Background()

		pol1 := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "01-charges-budget",
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: "false",
			OnFailed:      messagepolicy.OutcomeAdmit,
		}
		pol2 := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "02-after-exhaustion",
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: "false",
			OnFailed:      messagepolicy.OutcomeReject,
			RejectReason:  "budget exhausted on {policy}",
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, pol1))
		require.NoError(t, f.s.CreateMessagePolicy(ctx, pol2))

		// Inject a tiny budget tracker via test seam that pol1 exhausts so pol2
		// hits budget_exhausted.
		f.srv.messagePolicyGate.newSendBudget = func() *ruleengine.BudgetTracker {
			bt := ruleengine.NewBudgetTracker(10 * time.Millisecond)
			bt.Charge(10 * time.Millisecond)
			return bt
		}

		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "check budget exhaustion",
		})
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code)

		decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: f.project.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, decs.Items, 2)
		for _, d := range decs.Items {
			assert.Equal(t, store.MessagePolicyClassFailed, d.Class)
			assert.Equal(t, messagepolicy.ReasonBudgetExhausted, d.CELErrorReason)
		}
	})
}

// TestMessagePolicyGate_FactsActivationOnGroupConversation verifies that CEL
// rules receive accurate sender, recipient, message, mentioned_agent_ids,
// mentioned_user_ids, and group conversation participant facts on H2.
func TestMessagePolicyGate_FactsActivationOnGroupConversation(t *testing.T) {
	f := newMpgFixture(t, true)
	ctx := context.Background()

	expr := strings.Join([]string{
		`surface == "agent_to_conversation"`,
		fmt.Sprintf(`sender.id == %q`, f.sender.ID),
		`sender.slug == "sender-agent"`,
		fmt.Sprintf(`sender.project_id == %q`, f.project.ID),
		`recipient.kind == "conversation"`,
		fmt.Sprintf(`recipient.id == %q`, f.groupConv.ID),
		`recipient.slug == "mpg-team-room"`,
		`message.urgent == true`,
		`message.attachment_count == 1`,
		fmt.Sprintf(`%q in mentioned_agent_ids`, f.peerAgent.ID),
		fmt.Sprintf(`%q in mentioned_user_ids`, f.alice.ID),
		`conversation.kind == "group"`,
		fmt.Sprintf(`%q in conversation.agent_participant_ids`, f.defaultAgent.ID),
		fmt.Sprintf(`%q in conversation.user_participant_ids`, f.alice.ID),
	}, " && ")

	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID: f.project.ID,
		Name:      "assert-all-facts",
		Enabled:   true,
		Mode:      store.MessagePolicyModeEnforce,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{store.MessagePolicySurfaceAgentToConversation},
			Sender:   store.MessagePolicySenderSelector{AgentIDs: []string{f.sender.ID}},
		},
		ViolationRule: expr,
		OnFailed:      messagepolicy.OutcomeAdmit,
		RejectReason:  "all facts matched expected values",
	}))

	rr := f.sendOutbound(t, OutboundMessageRequest{
		ConversationRef: "conv:" + f.groupConv.ID,
		Msg:             fmt.Sprintf("Ping @%s and @%s please", f.peerAgent.Slug, f.alice.Email),
		Urgent:          true,
		Attachments:     []string{f.stagedPath},
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())
}
