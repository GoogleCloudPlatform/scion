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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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

// mpgFaultStore tracks and optionally fails MessagePolicyStore and fact/message
// store calls so tests can exercise flag-off isolation, facts_unavailable
// fail-closed behaviour, and unstored-message decision suppression without ever
// reassigning srv.store.
type mpgFaultStore struct {
	store.Store
	fault                  *storeFaultSwitch
	listCalls              atomic.Int64
	decisionCalls          atomic.Int64
	onAfterListPolicies    func()
	failListAgents         atomic.Bool
	failListProjectMembers atomic.Bool
	failListParticipants   atomic.Bool
	failCreateMessage      atomic.Bool
}

func (f *mpgFaultStore) ListMessagePolicies(ctx context.Context, filter store.MessagePolicyFilter, opts store.ListOptions) (*store.ListResult[store.MessagePolicy], error) {
	f.listCalls.Add(1)
	if f.fault.Active() {
		return nil, errors.New("unexpected ListMessagePolicies call while experiment is off")
	}
	res, err := f.Store.ListMessagePolicies(ctx, filter, opts)
	if fn := f.onAfterListPolicies; fn != nil {
		fn()
	}
	return res, err
}

func (f *mpgFaultStore) CreateMessagePolicyDecision(ctx context.Context, d *store.MessagePolicyDecision) error {
	f.decisionCalls.Add(1)
	if f.fault.Active() {
		return errors.New("unexpected CreateMessagePolicyDecision call while experiment is off")
	}
	return f.Store.CreateMessagePolicyDecision(ctx, d)
}

func (f *mpgFaultStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	if f.failListAgents.Load() {
		return nil, errors.New("injected ListAgents fault")
	}
	return f.Store.ListAgents(ctx, filter, opts)
}

func (f *mpgFaultStore) ListProjectMembers(ctx context.Context, projectID string) ([]*store.ProjectMembership, error) {
	if f.failListProjectMembers.Load() {
		return nil, errors.New("injected ListProjectMembers fault")
	}
	return f.Store.ListProjectMembers(ctx, projectID)
}

func (f *mpgFaultStore) ListParticipants(ctx context.Context, conversationID string) ([]store.ConversationParticipant, error) {
	if f.failListParticipants.Load() {
		return nil, errors.New("injected ListParticipants fault")
	}
	return f.Store.ListParticipants(ctx, conversationID)
}

func (f *mpgFaultStore) CreateMessage(ctx context.Context, msg *store.Message) error {
	if f.failCreateMessage.Load() {
		return errors.New("injected CreateMessage fault")
	}
	return f.Store.CreateMessage(ctx, msg)
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
// name-order evaluation across multiple enforce policies, fail-closed
// policy_cap_exceeded handling for matching enforce policies past the 10-policy
// per-send cap, and separate mode-split capping so earlier-named shadow
// policies never crowd out enforce policies (review R1).
func TestMessagePolicyGate_MultipleEnforceOrderingAndCap10(t *testing.T) {
	t.Run("first_violation_in_name_order_and_excess_enforce_recorded_as_policy_cap_exceeded", func(t *testing.T) {
		f := newMpgFixture(t, true)
		ctx := context.Background()

		// Seed 12 enabled matching enforce policies (p01..p12); p02 and p05
		// violate; p11 sets on_failed=reject, p12 sets on_failed=admit.
		for i := 1; i <= 12; i++ {
			name := fmt.Sprintf("p%02d", i)
			rule := "false"
			if i == 2 || i == 5 {
				rule = "true"
			}
			onFailed := messagepolicy.OutcomeReject
			if i == 12 {
				onFailed = messagepolicy.OutcomeAdmit
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
				OnFailed:      onFailed,
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
		assert.Equal(t, messagepolicy.ClassViolation, errResp.Error.Details["class"])
		assert.Equal(t, "rejected by p02", errResp.Error.Message)

		decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{ProjectID: f.project.ID}, store.ListOptions{Limit: 20})
		require.NoError(t, err)
		require.Len(t, decs.Items, 12, "all 12 matching enforce policies must produce decision rows (none silently skipped)")

		byName := make(map[string]store.MessagePolicyDecision, len(decs.Items))
		for _, d := range decs.Items {
			byName[d.PolicyName] = d
			assert.Empty(t, d.MessageID, "overall send was rejected so every decision row must have empty message_id")
		}
		assert.Equal(t, store.MessagePolicyActionAdmitted, byName["p01"].Action)
		assert.Equal(t, store.MessagePolicyClassPass, byName["p01"].Class)
		assert.Equal(t, store.MessagePolicyActionRejected, byName["p02"].Action)
		assert.Equal(t, store.MessagePolicyClassViolation, byName["p02"].Class)
		assert.Equal(t, store.MessagePolicyActionRejected, byName["p05"].Action)
		assert.Equal(t, store.MessagePolicyClassViolation, byName["p05"].Class)

		assert.Equal(t, store.MessagePolicyClassFailed, byName["p11"].Class)
		assert.Equal(t, messagepolicy.ReasonPolicyCapExceeded, byName["p11"].CELErrorReason)
		assert.Equal(t, store.MessagePolicyActionRejected, byName["p11"].Action)

		assert.Equal(t, store.MessagePolicyClassFailed, byName["p12"].Class)
		assert.Equal(t, messagepolicy.ReasonPolicyCapExceeded, byName["p12"].CELErrorReason)
		assert.Equal(t, store.MessagePolicyActionAdmitted, byName["p12"].Action)
	})

	t.Run("p11_only_beyond_cap_fails_closed_via_on_failed", func(t *testing.T) {
		f := newMpgFixture(t, true)
		ctx := context.Background()

		// p01..p10 pass; p11 is past the 10-policy cap with on_failed=reject.
		var p11 *store.MessagePolicy
		for i := 1; i <= 11; i++ {
			pol := &store.MessagePolicy{
				ProjectID: f.project.ID,
				Name:      fmt.Sprintf("p%02d", i),
				Enabled:   true,
				Mode:      store.MessagePolicyModeEnforce,
				Scope: store.MessagePolicyScope{
					Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
					Sender:   store.MessagePolicySenderSelector{AllAgents: true},
				},
				ViolationRule: "false",
				OnFailed:      messagepolicy.OutcomeReject,
				RejectReason:  "cap exceeded on {policy}",
			}
			if i == 11 {
				pol.ViolationRule = "true"
				p11 = pol
			}
			require.NoError(t, f.s.CreateMessagePolicy(ctx, pol))
		}

		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "must not admit when p11 exceeds cap",
		})
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())

		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
		assert.Equal(t, "p11", errResp.Error.Details["policy"])
		assert.Equal(t, messagepolicy.ClassFailed, errResp.Error.Details["class"])
		assert.Equal(t, "cap exceeded on p11", errResp.Error.Message)

		msgs, err := f.s.ListMessages(ctx, store.MessageFilter{SenderID: f.sender.ID}, store.ListOptions{Limit: 5})
		require.NoError(t, err)
		assert.Empty(t, msgs.Items, "cap-exceeded reject must persist zero message rows")

		// Override policy_cap_exceeded -> admit on p11; send now admits and still
		// records p11 as failed{policy_cap_exceeded}.
		p11.OnFailedReasons = map[string]string{
			messagepolicy.ReasonPolicyCapExceeded: messagepolicy.OutcomeAdmit,
		}
		require.NoError(t, f.s.UpdateMessagePolicy(ctx, p11))

		rrAdmit := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "admitted via explicit on_failed_reasons override",
		})
		require.Equal(t, http.StatusOK, rrAdmit.Code, "body: %s", rrAdmit.Body.String())

		p11Decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
			PolicyID:  p11.ID,
		}, store.ListOptions{Limit: 5})
		require.NoError(t, err)
		require.Len(t, p11Decs.Items, 2)
		assert.Equal(t, store.MessagePolicyClassFailed, p11Decs.Items[0].Class)
		assert.Equal(t, messagepolicy.ReasonPolicyCapExceeded, p11Decs.Items[0].CELErrorReason)
		assert.Equal(t, store.MessagePolicyActionAdmitted, p11Decs.Items[0].Action)
		assert.NotEmpty(t, p11Decs.Items[0].MessageID)
	})

	t.Run("ten_shadow_sorting_ahead_of_one_enforce_never_disables_enforce", func(t *testing.T) {
		f := newMpgFixture(t, true)
		ctx := context.Background()

		// Seed 12 shadow policies sorting ahead of z-enforce-01; shadow is capped
		// separately at 10 and must not consume enforce capacity.
		for i := 1; i <= 12; i++ {
			require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
				ProjectID: f.project.ID,
				Name:      fmt.Sprintf("a-trial-%02d", i),
				Enabled:   true,
				Mode:      store.MessagePolicyModeShadow,
				Scope: store.MessagePolicyScope{
					Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
					Sender:   store.MessagePolicySenderSelector{AllAgents: true},
				},
				ViolationRule: "false",
				OnFailed:      messagepolicy.OutcomeAdmit,
			}))
		}
		enforcePol := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "z-enforce-01",
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: "true",
			OnFailed:      messagepolicy.OutcomeReject,
			RejectReason:  "blocked by {policy}",
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, enforcePol))

		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "shadow policies must not crowd out z-enforce-01",
		})
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())

		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
		assert.Equal(t, "z-enforce-01", errResp.Error.Details["policy"])
		assert.Equal(t, messagepolicy.ClassViolation, errResp.Error.Details["class"])

		msgs, err := f.s.ListMessages(ctx, store.MessageFilter{SenderID: f.sender.ID}, store.ListOptions{Limit: 5})
		require.NoError(t, err)
		assert.Empty(t, msgs.Items)

		// Wait for the capped 10 shadow decisions + 1 enforce decision to persist.
		require.Eventually(t, func() bool {
			decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
				ProjectID: f.project.ID,
			}, store.ListOptions{Limit: 25})
			return err == nil && len(decs.Items) == 1+store.MaxActiveMatchingMessagePolicies
		}, 3*time.Second, 15*time.Millisecond)

		decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
		}, store.ListOptions{Limit: 25})
		require.NoError(t, err)
		byName := make(map[string]store.MessagePolicyDecision, len(decs.Items))
		for _, d := range decs.Items {
			byName[d.PolicyName] = d
			assert.Empty(t, d.MessageID)
		}
		assert.Equal(t, store.MessagePolicyActionRejected, byName["z-enforce-01"].Action)
		assert.Contains(t, byName, "a-trial-10")
		assert.NotContains(t, byName, "a-trial-11", "shadow policies are capped separately at 10")
		assert.NotContains(t, byName, "a-trial-12")
		assert.Equal(t, int64(2), f.srv.messagePolicyShadowDrops.Load(),
			"excess shadow policies beyond the separate 10-policy cap must be counted as dropped")
	})
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

// mpgLogCaptureHandler records slog records emitted during a test so we can
// verify structured slog.Error emissions and confirm no message body leaks.
type mpgLogCaptureHandler struct {
	mu      sync.Mutex
	records []mpgCapturedLog
}

type mpgCapturedLog struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

func (h *mpgLogCaptureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *mpgLogCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, mpgCapturedLog{
		level: r.Level,
		msg:   r.Message,
		attrs: attrs,
	})
	h.mu.Unlock()
	return nil
}

func (h *mpgLogCaptureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *mpgLogCaptureHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *mpgLogCaptureHandler) snapshot() []mpgCapturedLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]mpgCapturedLog, len(h.records))
	copy(out, h.records)
	return out
}

func mpgSumInt64Metric(t *testing.T, rm metricdata.ResourceMetrics, name string) int64 {
	t.Helper()
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "expected int64 Sum for %s, got %T", name, m.Data)
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
		}
	}
	return total
}

// TestMessagePolicyGate_AdmissionFailureAndWrappedErrors verifies Slice 2
// robustness requirements (Targets 4c & 9a):
//  1. Evaluation-time UnsupportedDialectError records class="failed" with the
//     admission reason, emits slog.Error + OTel admission-error counter, never
//     admits by default, and never logs message text.
//  2. Wrapped ruleengine errors (fmt.Errorf("%w: %w", ErrBudgetExhausted, ...)
//     and errors.Join(*FailedError)) unpack accurately via errors.Is/errors.As
//     through the H2 gate.
func TestMessagePolicyGate_AdmissionFailureAndWrappedErrors(t *testing.T) {
	t.Run("eval_time_unsupported_dialect_logs_metrics_and_never_admits_by_default", func(t *testing.T) {
		f := newMpgFixture(t, true)
		ctx := context.Background()

		reader := sdkmetric.NewManualReader()
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
		otelMetrics, err := NewOTelMessagePolicyMetrics(mp)
		require.NoError(t, err)
		f.srv.SetMessagePolicyMetrics(otelMetrics)

		pol := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "admission-guard-policy",
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: "false",
			OnFailed:      messagepolicy.OutcomeReject,
			RejectReason:  "admission failure failed closed",
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, pol))

		// Simulate an evaluation-time consumer admission misconfiguration by
		// replacing message_policy's grants with an unrelated dialect so CEL is
		// denied with ReasonDialectNotAllowed.
		require.NoError(t, f.srv.RuleEngine().AllowConsumerDialects(
			messagepolicy.ConsumerName,
			ruleengine.Grant("other_dialect"),
		))
		t.Cleanup(func() {
			_ = f.srv.RuleEngine().AllowConsumerDialects(
				messagepolicy.ConsumerName,
				ruleengine.Grant(ruleengine.DialectCEL),
			)
		})

		logCap := &mpgLogCaptureHandler{}
		prevLogger := slog.Default()
		slog.SetDefault(slog.New(logCap))
		t.Cleanup(func() { slog.SetDefault(prevLogger) })

		const secretText = "SENSITIVE-ADMISSION-ERROR-BODY-778899"
		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       secretText,
		})
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())

		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		assert.Equal(t, string(MessageDenialPolicyRejected), resp.Error.Code)
		assert.Equal(t, store.MessagePolicyClassFailed, resp.Error.Details["class"])

		// Verify atomic counter + OTel counter incremented.
		assert.Equal(t, int64(1), f.srv.messagePolicyAdmissionErrors.Load())
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(ctx, &rm))
		assert.Equal(t, int64(1), mpgSumInt64Metric(t, rm, "scion.message_policy.admission.errors"))
		assert.Equal(t, int64(1), mpgSumInt64Metric(t, rm, "scion.message_policy.evaluations"))

		// Verify slog.Error emitted with structured fields and zero message body text.
		var foundErrLog bool
		for _, rec := range logCap.snapshot() {
			if rec.level >= slog.LevelError && strings.Contains(rec.msg, "message policy evaluation encountered ruleengine consumer admission failure") {
				foundErrLog = true
				assert.Equal(t, f.project.ID, rec.attrs["project_id"])
				assert.Equal(t, pol.ID, rec.attrs["policy_id"])
				assert.Equal(t, pol.Name, rec.attrs["policy"])
				assert.Equal(t, ruleengine.ReasonDialectNotAllowed, rec.attrs["reason"])
				for k, v := range rec.attrs {
					assert.NotContains(t, v, secretText, "slog.Error attr %s leaked message body", k)
				}
			}
		}
		assert.True(t, foundErrLog, "expected slog.Error for evaluation-time admission failure")

		// Verify persisted decision row has class=failed, reason=dialect_not_allowed, action=rejected.
		decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
			PolicyID:  pol.ID,
		}, store.ListOptions{Limit: 5})
		require.NoError(t, err)
		require.Len(t, decs.Items, 1)
		assert.Equal(t, store.MessagePolicyClassFailed, decs.Items[0].Class)
		assert.Equal(t, ruleengine.ReasonDialectNotAllowed, decs.Items[0].CELErrorReason)
		assert.Equal(t, store.MessagePolicyActionRejected, decs.Items[0].Action)
		assert.Empty(t, decs.Items[0].MessageID)

		// Also verify ResolveOutcome never admits by default when onFailed is empty.
		assert.Equal(t, messagepolicy.OutcomeReject, messagepolicy.ResolveOutcome(
			messagepolicy.ClassFailed, ruleengine.ReasonDialectNotAllowed, "", "", nil,
		))
	})

	t.Run("wrapped_ruleengine_errors_through_gate", func(t *testing.T) {
		f := newMpgFixture(t, true)
		ctx := context.Background()

		pol1 := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "01-wrapped-budget",
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
			Name:      "02-joined-failed",
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: "false",
			OnFailed:      messagepolicy.OutcomeAdmit,
		}
		pol3 := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "03-wrapped-admission",
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: "false",
			OnFailed:      messagepolicy.OutcomeReject,
			RejectReason:  "wrapped admission rejected",
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, pol1))
		require.NoError(t, f.s.CreateMessagePolicy(ctx, pol2))
		require.NoError(t, f.s.CreateMessagePolicy(ctx, pol3))

		f.srv.messagePolicyGate.evalOverride = func(_ context.Context, p *store.MessagePolicy, _ messagepolicy.Activation, _ *ruleengine.BudgetTracker) (messagepolicy.Classification, bool) {
			switch p.Name {
			case "01-wrapped-budget":
				wrapped := fmt.Errorf("outer gate: %w",
					fmt.Errorf("%w: %w", ruleengine.ErrBudgetExhausted, &ruleengine.FailedError{Reason: celengine.ReasonTimeout}),
				)
				return messagepolicy.ClassifyDecision(false, celengine.EvalStats{}, wrapped), true
			case "02-joined-failed":
				joined := errors.Join(
					errors.New("wrapper"),
					&ruleengine.FailedError{
						Reason: celengine.ReasonCostLimit,
						Cause:  errors.Join(errors.New("inner"), &celengine.EvalError{Reason: celengine.ReasonCostLimit}),
					},
				)
				return messagepolicy.ClassifyDecision(false, celengine.EvalStats{}, joined), true
			default:
				wrappedUnsup := fmt.Errorf("wrapped admission: %w", &ruleengine.UnsupportedDialectError{
					Dialect:  ruleengine.DialectCEL,
					Consumer: messagepolicy.ConsumerName,
					Reason:   ruleengine.ReasonConsumerNotRegistered,
				})
				return messagepolicy.ClassifyDecision(false, celengine.EvalStats{}, wrappedUnsup), true
			}
		}

		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "exercise wrapped ruleengine errors",
		})
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code)
		assert.Equal(t, int64(1), f.srv.messagePolicyAdmissionErrors.Load())

		decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
		}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, decs.Items, 3)

		byPolicy := make(map[string]store.MessagePolicyDecision, 3)
		for _, d := range decs.Items {
			byPolicy[d.PolicyName] = d
		}
		assert.Equal(t, messagepolicy.ReasonBudgetExhausted, byPolicy["01-wrapped-budget"].CELErrorReason)
		assert.Equal(t, store.MessagePolicyActionAdmitted, byPolicy["01-wrapped-budget"].Action)
		assert.Equal(t, celengine.ReasonCostLimit, byPolicy["02-joined-failed"].CELErrorReason)
		assert.Equal(t, store.MessagePolicyActionAdmitted, byPolicy["02-joined-failed"].Action)
		assert.Equal(t, ruleengine.ReasonConsumerNotRegistered, byPolicy["03-wrapped-admission"].CELErrorReason)
		assert.Equal(t, store.MessagePolicyActionRejected, byPolicy["03-wrapped-admission"].Action)
	})
}

// TestMessagePolicyDecisionTrimJob verifies the periodic bounded decision trim
// job (Target 8): aged decisions (> 7d) and excess per-project rows are trimmed
// while recent rows survive, and the background loop starts/stops cleanly.
func TestMessagePolicyDecisionTrimJob(t *testing.T) {
	t.Run("manual_pass_trims_aged_and_overflow_decisions", func(t *testing.T) {
		f := newMpgFixture(t, true)
		ctx := context.Background()

		now := time.Now().UTC().Truncate(time.Second)
		f.srv.messagePolicyTrim.nowFunc = func() time.Time { return now }
		f.srv.messagePolicyTrim.maxRowsPerProject = 2

		policyID := api.NewUUID()
		seedDecision := func(createdAt time.Time) string {
			d := &store.MessagePolicyDecision{
				ProjectID:      f.project.ID,
				PolicyID:       policyID,
				PolicyName:     "trim-policy",
				PolicyRevision: 1,
				Mode:           store.MessagePolicyModeShadow,
				Surface:        store.MessagePolicySurfaceAgentToUser,
				SenderID:       f.sender.ID,
				RecipientKind:  "user",
				RecipientID:    f.alice.ID,
				Class:          store.MessagePolicyClassPass,
				Action:         store.MessagePolicyActionShadow,
				CreatedAt:      createdAt,
			}
			require.NoError(t, f.s.CreateMessagePolicyDecision(ctx, d))
			return d.ID
		}

		// 2 aged decisions (> DefaultMessagePolicyDecisionMaxAge = 7 days).
		_ = seedDecision(now.Add(-8 * 24 * time.Hour))
		_ = seedDecision(now.Add(-9 * 24 * time.Hour))

		// 4 fresh decisions (cap = 2 -> 2 overflow deleted, 2 newest kept).
		_ = seedDecision(now.Add(-4 * time.Hour))
		_ = seedDecision(now.Add(-3 * time.Hour))
		keepOldest := seedDecision(now.Add(-2 * time.Hour))
		keepNewest := seedDecision(now.Add(-1 * time.Hour))

		trimRes, ran, err := f.srv.messagePolicyDecisionTrimPass(ctx)
		require.NoError(t, err)
		assert.True(t, ran)
		assert.Equal(t, 2, trimRes.AgedDeleted)
		assert.Equal(t, 2, trimRes.OverflowDeleted)
		assert.False(t, trimRes.More)

		rem, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
		}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, rem.Items, 2)
		assert.Equal(t, keepNewest, rem.Items[0].ID)
		assert.Equal(t, keepOldest, rem.Items[1].ID)
	})

	t.Run("periodic_loop_ticks_and_stops_idempotently", func(t *testing.T) {
		f := newMpgFixture(t, false) // trim runs even when experiment is toggled off
		ctx := context.Background()

		now := time.Now().UTC()
		policyID := api.NewUUID()
		aged := &store.MessagePolicyDecision{
			ProjectID:      f.project.ID,
			PolicyID:       policyID,
			PolicyName:     "aged-shadow",
			PolicyRevision: 1,
			Mode:           store.MessagePolicyModeShadow,
			Surface:        store.MessagePolicySurfaceAgentToUser,
			SenderID:       f.sender.ID,
			RecipientKind:  "user",
			RecipientID:    f.alice.ID,
			Class:          store.MessagePolicyClassViolation,
			Action:         store.MessagePolicyActionShadow,
			CreatedAt:      now.Add(-10 * 24 * time.Hour),
		}
		fresh := &store.MessagePolicyDecision{
			ProjectID:      f.project.ID,
			PolicyID:       policyID,
			PolicyName:     "fresh-shadow",
			PolicyRevision: 1,
			Mode:           store.MessagePolicyModeShadow,
			Surface:        store.MessagePolicySurfaceAgentToUser,
			SenderID:       f.sender.ID,
			RecipientKind:  "user",
			RecipientID:    f.alice.ID,
			Class:          store.MessagePolicyClassPass,
			Action:         store.MessagePolicyActionShadow,
			CreatedAt:      now.Add(-30 * time.Minute),
		}
		require.NoError(t, f.s.CreateMessagePolicyDecision(ctx, aged))
		require.NoError(t, f.s.CreateMessagePolicyDecision(ctx, fresh))

		f.srv.messagePolicyTrim.tickInterval = 15 * time.Millisecond
		loopCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		f.srv.startMessagePolicyDecisionTrim(loopCtx)
		// Second start call must be a safe no-op.
		f.srv.startMessagePolicyDecisionTrim(loopCtx)

		require.Eventually(t, func() bool {
			rem, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
				ProjectID: f.project.ID,
			}, store.ListOptions{Limit: 10})
			return err == nil && len(rem.Items) == 1 && rem.Items[0].ID == fresh.ID
		}, 2*time.Second, 15*time.Millisecond, "periodic trim loop did not delete aged decision")

		// Idempotent stop.
		f.srv.stopMessagePolicyDecisionTrim()
		f.srv.stopMessagePolicyDecisionTrim()
	})
}

// TestMessagePolicyGate_FactsUnavailableFailsClosed verifies review O1: when a
// fact lookup (ListParticipants, ListAgents, or ListProjectMembers) fails, H2
// never evaluates enforce policies against false empty lists; instead each
// policy classifies as failed{facts_unavailable} and maps through on_failed /
// on_failed_reasons.
func TestMessagePolicyGate_FactsUnavailableFailsClosed(t *testing.T) {
	t.Run("participant_lookup_fault_fails_closed", func(t *testing.T) {
		f := newMpgFixture(t, true)
		ctx := context.Background()

		// A rule that would admit if conversation.agent_participant_ids were
		// evaluated as [] (violation_rule == false on empty list).
		pol := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "require-mentioned-participants",
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToConversation},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: `size(conversation.agent_participant_ids) > 0 && size(mentioned_agent_ids) == 0`,
			OnFailed:      messagepolicy.OutcomeReject,
			RejectReason:  "facts unavailable on {policy}",
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, pol))

		// Arm ListParticipants fault right after ListMessagePolicies runs (so
		// upstream conversation routing succeeds and the fault hits H2 facts).
		f.faultWrapper.onAfterListPolicies = func() {
			f.faultWrapper.failListParticipants.Store(true)
		}
		t.Cleanup(func() {
			f.faultWrapper.onAfterListPolicies = nil
			f.faultWrapper.failListParticipants.Store(false)
		})

		rr := f.sendOutbound(t, OutboundMessageRequest{
			ConversationRef: "conv:" + f.groupConv.ID,
			Msg:             fmt.Sprintf("Hello @%s", f.defaultAgent.Slug),
		})
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())

		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
		assert.Equal(t, string(MessageDenialPolicyRejected), errResp.Error.Code)
		assert.Equal(t, messagepolicy.ClassFailed, errResp.Error.Details["class"])
		assert.Equal(t, "facts unavailable on require-mentioned-participants", errResp.Error.Message)

		f.faultWrapper.onAfterListPolicies = nil
		f.faultWrapper.failListParticipants.Store(false)

		msgs, err := f.s.ListMessages(ctx, store.MessageFilter{SenderID: f.sender.ID}, store.ListOptions{Limit: 5})
		require.NoError(t, err)
		assert.Empty(t, msgs.Items, "facts_unavailable reject must persist zero message rows")

		decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
			PolicyID:  pol.ID,
		}, store.ListOptions{Limit: 5})
		require.NoError(t, err)
		require.Len(t, decs.Items, 1)
		assert.Equal(t, store.MessagePolicyClassFailed, decs.Items[0].Class)
		assert.Equal(t, messagepolicy.ReasonFactsUnavailable, decs.Items[0].CELErrorReason)
		assert.Equal(t, store.MessagePolicyActionRejected, decs.Items[0].Action)
		assert.Empty(t, decs.Items[0].MessageID)
	})

	t.Run("agent_and_member_mention_faults_and_on_failed_reasons_override", func(t *testing.T) {
		f := newMpgFixture(t, true)
		ctx := context.Background()

		pol := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "mention-facts-guard",
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: "false",
			OnFailed:      messagepolicy.OutcomeReject,
			RejectReason:  "mention facts unavailable",
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, pol))

		// 1. ListAgents fault on @peer-agent mention -> 422 failed{facts_unavailable}.
		f.faultWrapper.onAfterListPolicies = func() {
			f.faultWrapper.failListAgents.Store(true)
		}
		rrAgent := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "cc @" + f.peerAgent.Slug,
		})
		f.faultWrapper.onAfterListPolicies = nil
		f.faultWrapper.failListAgents.Store(false)
		require.Equal(t, http.StatusUnprocessableEntity, rrAgent.Code)

		// 2. ListProjectMembers fault on @alice@example.com mention -> 422 failed{facts_unavailable}.
		f.faultWrapper.onAfterListPolicies = func() {
			f.faultWrapper.failListProjectMembers.Store(true)
		}
		rrHuman := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "cc @" + f.alice.Email,
		})
		f.faultWrapper.onAfterListPolicies = nil
		f.faultWrapper.failListProjectMembers.Store(false)
		require.Equal(t, http.StatusUnprocessableEntity, rrHuman.Code)

		// 3. Explicit on_failed_reasons["facts_unavailable"] = "admit" admits and records failed{facts_unavailable}.
		pol.OnFailedReasons = map[string]string{
			messagepolicy.ReasonFactsUnavailable: messagepolicy.OutcomeAdmit,
		}
		require.NoError(t, f.s.UpdateMessagePolicy(ctx, pol))

		f.faultWrapper.onAfterListPolicies = func() {
			f.faultWrapper.failListProjectMembers.Store(true)
		}
		rrAdmit := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "cc @" + f.alice.Email,
		})
		f.faultWrapper.onAfterListPolicies = nil
		f.faultWrapper.failListProjectMembers.Store(false)
		require.Equal(t, http.StatusOK, rrAdmit.Code, "body: %s", rrAdmit.Body.String())

		decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
			PolicyID:  pol.ID,
		}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, decs.Items, 3)
		assert.Equal(t, store.MessagePolicyClassFailed, decs.Items[0].Class)
		assert.Equal(t, messagepolicy.ReasonFactsUnavailable, decs.Items[0].CELErrorReason)
		assert.Equal(t, store.MessagePolicyActionAdmitted, decs.Items[0].Action)
		assert.NotEmpty(t, decs.Items[0].MessageID)
	})
}

// TestMessagePolicyGate_ShutdownDrainAndCancel verifies review O3:
//  1. stopMessagePolicyGate drains queued shadow jobs cleanly when they finish
//     within the shutdown deadline, and refuses new submissions afterward.
//  2. When the shutdown deadline expires and workerCtx is canceled mid-job,
//     stopMessagePolicyGate returns promptly and workers stop without recording
//     spurious failed/canceled shadow decision rows.
func TestMessagePolicyGate_ShutdownDrainAndCancel(t *testing.T) {
	t.Run("drains_queued_shadow_jobs_within_deadline", func(t *testing.T) {
		f := newMpgFixture(t, true)
		ctx := context.Background()

		pol := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "shadow-drain-ok",
			Enabled:   true,
			Mode:      store.MessagePolicyModeShadow,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: "true",
			OnFailed:      messagepolicy.OutcomeReject,
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, pol))

		workerEntered := make(chan struct{}, 1)
		releaseWorker := make(chan struct{})
		f.srv.messagePolicyGate.testShadowBeforeEval = func() {
			select {
			case workerEntered <- struct{}{}:
			default:
			}
			<-releaseWorker
		}

		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "queued before shutdown drain",
		})
		require.Equal(t, http.StatusOK, rr.Code)
		<-workerEntered

		// Release worker shortly after stopMessagePolicyGate begins draining.
		go func() {
			time.Sleep(15 * time.Millisecond)
			close(releaseWorker)
		}()

		stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		f.srv.stopMessagePolicyGate(stopCtx)

		decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
			PolicyID:  pol.ID,
		}, store.ListOptions{Limit: 5})
		require.NoError(t, err)
		require.Len(t, decs.Items, 1, "drained shadow job must persist its decision row")
		assert.Equal(t, store.MessagePolicyClassViolation, decs.Items[0].Class)
		assert.Equal(t, store.MessagePolicyActionShadow, decs.Items[0].Action)
		assert.NotEmpty(t, decs.Items[0].MessageID)

		// Enqueue after stop is a safe no-op.
		f.srv.enqueueMessagePolicyShadow(messagePolicyShadowJob{
			policies:  []store.MessagePolicy{*pol},
			snapshot:  h2FactSnapshot{projectID: f.project.ID, surface: messagepolicy.SurfaceAgentToUser, senderID: f.sender.ID, recipientKind: "user", recipientID: f.alice.ID},
			messageID: api.NewUUID(),
		})
		decsAfter, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
			PolicyID:  pol.ID,
		}, store.ListOptions{Limit: 5})
		require.NoError(t, err)
		assert.Len(t, decsAfter.Items, 1)
	})

	t.Run("drain_timeout_cancels_without_recording_spurious_canceled_rows", func(t *testing.T) {
		f := newMpgFixture(t, true)
		ctx := context.Background()

		// Three shadow policies in one job: s01 completes before cancel; s02
		// blocks until workerCtx is canceled (returning failed/canceled from
		// ClassifyError); s03 trails in the same job, plus a second queued job.
		for i := 1; i <= 3; i++ {
			require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
				ProjectID: f.project.ID,
				Name:      fmt.Sprintf("s%02d", i),
				Enabled:   true,
				Mode:      store.MessagePolicyModeShadow,
				Scope: store.MessagePolicyScope{
					Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
					Sender:   store.MessagePolicySenderSelector{AllAgents: true},
				},
				ViolationRule: "false",
				OnFailed:      messagepolicy.OutcomeReject,
			}))
		}

		s02Blocked := make(chan struct{}, 1)
		f.srv.messagePolicyGate.evalOverride = func(evalCtx context.Context, p *store.MessagePolicy, _ messagepolicy.Activation, _ *ruleengine.BudgetTracker) (messagepolicy.Classification, bool) {
			if p.Name == "s01" {
				return messagepolicy.Classification{Class: messagepolicy.ClassPass}, true
			}
			select {
			case s02Blocked <- struct{}{}:
			default:
			}
			<-evalCtx.Done()
			return messagepolicy.ClassifyError(evalCtx.Err()), true
		}

		// Queue multiple sends so workers are mid-job and backlog sits in shadowCh.
		f.srv.chatSendLimiter = nil
		for i := 0; i < 4; i++ {
			rr := f.sendOutbound(t, OutboundMessageRequest{
				Recipient: "user:" + f.alice.Email,
				Msg:       fmt.Sprintf("in-flight shadow %d", i),
			})
			require.Equal(t, http.StatusOK, rr.Code)
		}
		<-s02Blocked

		shortCtx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
		defer cancel()
		start := time.Now()
		f.srv.stopMessagePolicyGate(shortCtx)
		elapsed := time.Since(start)
		assert.Less(t, elapsed, time.Second, "stopMessagePolicyGate must return promptly once ctx expires")

		// Idempotent second call.
		f.srv.stopMessagePolicyGate(ctx)

		decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
		}, store.ListOptions{Limit: 25})
		require.NoError(t, err)
		for _, d := range decs.Items {
			assert.NotEqual(t, store.MessagePolicyClassFailed, d.Class,
				"shutdown cancellation must never persist spurious failed/canceled shadow rows (got %+v)", d)
			assert.NotEqual(t, celengine.ReasonCanceled, d.CELErrorReason)
			assert.Equal(t, "s01", d.PolicyName)
		}
	})
}

// TestMessagePolicyGate_UnstoredSendDoesNotRecordDanglingMessageID verifies
// review O4 and N1:
//  1. When an H2-admitted send later fails before persistence (e.g. reserved
//     channel 400 or CreateMessage store 500), neither shadow nor admitted
//     enforce decision rows are recorded referencing an unstored message_id.
//  2. When nil evaluator is encountered defensively, evalOneMessagePolicy
//     returns failed{consumer_not_registered} directly (N1).
func TestMessagePolicyGate_UnstoredSendDoesNotRecordDanglingMessageID(t *testing.T) {
	f := newMpgFixture(t, true)
	ctx := context.Background()

	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID: f.project.ID,
		Name:      "enforce-pass",
		Enabled:   true,
		Mode:      store.MessagePolicyModeEnforce,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
			Sender:   store.MessagePolicySenderSelector{AllAgents: true},
		},
		ViolationRule: "false",
		OnFailed:      messagepolicy.OutcomeReject,
	}))
	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID: f.project.ID,
		Name:      "shadow-watch",
		Enabled:   true,
		Mode:      store.MessagePolicyModeShadow,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
			Sender:   store.MessagePolicySenderSelector{AllAgents: true},
		},
		ViolationRule: "true",
		OnFailed:      messagepolicy.OutcomeAdmit,
	}))

	// 1. Direct path store failure (500): message is never stored, so zero
	// decision rows may reference it.
	f.srv.chatSendLimiter = nil
	f.faultWrapper.failCreateMessage.Store(true)
	rrStoreFail := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "store will fail with 500",
	})
	f.faultWrapper.failCreateMessage.Store(false)
	require.Equal(t, http.StatusInternalServerError, rrStoreFail.Code)

	// 2. Shadow-only fast path store failure (500): disable enforce-pass, fault
	// CreateMessage again, verify shadow fast path also records nothing when unstored.
	enfPol, err := f.s.GetMessagePolicyByName(ctx, f.project.ID, "enforce-pass")
	require.NoError(t, err)
	enfPol.Enabled = false
	require.NoError(t, f.s.UpdateMessagePolicy(ctx, enfPol))

	f.faultWrapper.failCreateMessage.Store(true)
	rrShadowFail := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "shadow-only store will fail with 500",
	})
	f.faultWrapper.failCreateMessage.Store(false)
	require.Equal(t, http.StatusInternalServerError, rrShadowFail.Code)

	// Re-enable enforce-pass and also test reserved channel 400 via choices/D2 broker path.
	enfPol.Enabled = true
	require.NoError(t, f.s.UpdateMessagePolicy(ctx, enfPol))

	bus := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = bus.Close() })
	bp := NewMessageBrokerProxy(bus, f.srv.store, f.srv.events, f.srv.GetDispatcher, slog.Default())
	t.Cleanup(bp.Stop)
	f.srv.SetMessageBrokerProxy(bp)

	rrReserved := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Channel:   eventbus.InProcessBusName,
		Msg:       "reserved channel must fail with 400",
	})
	require.Equal(t, http.StatusBadRequest, rrReserved.Code)
	f.srv.SetMessageBrokerProxy(nil)

	// Give background workers a moment to ensure nothing was queued.
	time.Sleep(40 * time.Millisecond)
	decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: f.project.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, decs.Items, "failed sends must not persist admitted or shadow decisions pointing at unstored messages")

	// 3. Subsequent healthy send persists message and records both enforce-admitted and shadow with message_id.
	rrOK := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "stored cleanly",
	})
	require.Equal(t, http.StatusOK, rrOK.Code, "body: %s", rrOK.Body.String())
	var okBody struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rrOK.Body.Bytes(), &okBody))
	require.NotEmpty(t, okBody.MessageID)

	require.Eventually(t, func() bool {
		got, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
		}, store.ListOptions{Limit: 10})
		return err == nil && len(got.Items) == 2
	}, 3*time.Second, 15*time.Millisecond)

	decsOK, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: f.project.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	for _, d := range decsOK.Items {
		assert.Equal(t, okBody.MessageID, d.MessageID)
	}

	// 4. N1: nil evaluator on a bare Server fails closed with consumer_not_registered.
	bareSrv := &Server{}
	c := bareSrv.evalOneMessagePolicy(ctx, enfPol, messagepolicy.Activation{}, nil)
	assert.Equal(t, messagepolicy.ClassFailed, c.Class)
	assert.Equal(t, ruleengine.ReasonConsumerNotRegistered, c.Reason)
	assert.True(t, c.AdmissionError)
}

// mpgRecordingSpokeBus captures messages published to an external channel spoke
// on the MessageBrokerProxy path.
type mpgRecordingSpokeBus struct {
	mu   sync.Mutex
	msgs []*messages.StructuredMessage
}

func (b *mpgRecordingSpokeBus) Publish(_ context.Context, _ string, msg *messages.StructuredMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, msg)
	return nil
}

func (b *mpgRecordingSpokeBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nullSub{}, nil
}

func (b *mpgRecordingSpokeBus) Close() error { return nil }

func (b *mpgRecordingSpokeBus) snapshot() []*messages.StructuredMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*messages.StructuredMessage, len(b.msgs))
	copy(out, b.msgs)
	return out
}

// TestMessagePolicyGate_DeriveH2FactSnapshot_Classification verifies R1: when
// routing resolved a conversation (ConvResult != nil), H2 takes the surface and
// conversation/recipient kind strictly from ConvResult.Kind; fallback heuristics
// apply only when ConvResult is nil (write-deny-off fallback).
func TestMessagePolicyGate_DeriveH2FactSnapshot_Classification(t *testing.T) {
	agent := &store.Agent{
		ID:        "a1111111-1111-4111-8111-111111111111",
		Slug:      "sender-agent",
		ProjectID: "33333333-3333-4333-8333-333333333333",
	}
	userID := "22222222-2222-4222-8222-222222222222"
	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", userID)
	require.NoError(t, err)

	t.Run("conv_result_direct_with_non_dm_thread_id_classifies_as_agent_to_user", func(t *testing.T) {
		snap := deriveH2FactSnapshot(&h2PolicyInput{
			agent: agent,
			req: &OutboundMessageRequest{
				Channel:  "slack",
				ThreadID: "1700000000.123",
				Msg:      "hello",
				Type:     "input-needed",
			},
			routing: &routingResult{
				Recipient:      "user:alice@example.com",
				RecipientID:    userID,
				UserResolved:   true,
				ConversationID: "c1",
				Channel:        "slack",
				ThreadID:       "1700000000.123",
				ConvResult: &messaging.ConversationResult{
					ConversationID: "c1",
					Kind:           "direct",
					ExternalRef:    dmKey,
				},
			},
		})
		assert.Equal(t, messagepolicy.SurfaceAgentToUser, snap.surface)
		assert.Equal(t, "user", snap.recipientKind)
		assert.Equal(t, "direct", snap.convKind)
		assert.Equal(t, userID, snap.recipientID)
		assert.Equal(t, "alice@example.com", snap.recipientSlug)
		assert.Equal(t, "c1", snap.convID)

		// Even with a thread:-shaped Recipient, a user-resolved RecipientID on a
		// direct ConvResult classifies strictly from ConvResult.Kind as agent_to_user.
		spoofedPrefixSnap := deriveH2FactSnapshot(&h2PolicyInput{
			agent: agent,
			req: &OutboundMessageRequest{
				Channel:  "slack",
				ThreadID: "1700000000.123",
				Msg:      "hello",
			},
			routing: &routingResult{
				Recipient:    "thread:spoofed",
				RecipientID:  userID,
				UserResolved: true,
				ConvResult: &messaging.ConversationResult{
					ConversationID: "c1",
					Kind:           "direct",
					ExternalRef:    dmKey,
				},
			},
		})
		assert.Equal(t, messagepolicy.SurfaceAgentToUser, spoofedPrefixSnap.surface)
		assert.Equal(t, "user", spoofedPrefixSnap.recipientKind)
		assert.Equal(t, "direct", spoofedPrefixSnap.convKind)
		assert.Equal(t, userID, spoofedPrefixSnap.recipientID)
	})

	t.Run("two_agent_direct_conv_result_is_uncovered_agent_dm", func(t *testing.T) {
		for _, peerID := range []string{
			"00000000-0000-4000-8000-000000000001", // sorts before agent.ID
			"f9999999-9999-4999-8999-999999999999", // sorts after agent.ID
		} {
			agentDMKey, keyErr := messages.DMConversationKey("agent", agent.ID, "agent", peerID)
			require.NoError(t, keyErr)

			for _, recipientID := range []string{"", peerID} {
				snap := deriveH2FactSnapshot(&h2PolicyInput{
					agent: agent,
					req:   &OutboundMessageRequest{Msg: "agent dm"},
					routing: &routingResult{
						Recipient:      "agent:peer-bot",
						RecipientID:    recipientID,
						ConversationID: "adm-1",
						ConvResult: &messaging.ConversationResult{
							ConversationID: "adm-1",
							Kind:           "direct",
							ExternalRef:    agentDMKey,
						},
					},
				})
				assert.Equal(t, messagepolicy.SurfaceAgentDM, snap.uncoveredPath)
				assert.Empty(t, snap.surface)
				assert.NotEqual(t, "user", snap.recipientKind)
				assert.Equal(t, "agent", snap.recipientKind)
				assert.Equal(t, peerID, snap.recipientID)
				assert.Equal(t, "direct", snap.convKind)
			}
		}
	})

	t.Run("unresolved_recipient_id_never_classifies_as_user", func(t *testing.T) {
		// Empty RecipientID on a user DM key (no user resolution ran) must not
		// synthesize a user recipient from ParseDMKey.
		emptyRecipSnap := deriveH2FactSnapshot(&h2PolicyInput{
			agent: agent,
			req:   &OutboundMessageRequest{Msg: "no user resolution"},
			routing: &routingResult{
				ConvResult: &messaging.ConversationResult{
					ConversationID: "c1",
					Kind:           "direct",
					ExternalRef:    dmKey,
				},
			},
		})
		assert.NotEqual(t, "user", emptyRecipSnap.recipientKind)
		assert.Empty(t, emptyRecipSnap.surface)

		// Unresolved RecipientID (UserResolved == false) on either a two-agent DM
		// key (mismatched ID) or nil ConvResult must never classify as "user".
		peerID := "b2222222-2222-4222-8222-222222222222"
		agentDMKey, keyErr := messages.DMConversationKey("agent", agent.ID, "agent", peerID)
		require.NoError(t, keyErr)

		mismatchedAgentDMSnap := deriveH2FactSnapshot(&h2PolicyInput{
			agent: agent,
			req:   &OutboundMessageRequest{Msg: "mismatched id"},
			routing: &routingResult{
				Recipient:   "user:spoofed@example.com",
				RecipientID: agent.ID,
				ConvResult: &messaging.ConversationResult{
					ConversationID: "adm-2",
					Kind:           "direct",
					ExternalRef:    agentDMKey,
				},
			},
		})
		assert.NotEqual(t, "user", mismatchedAgentDMSnap.recipientKind)
		assert.Empty(t, mismatchedAgentDMSnap.surface)

		nilConvUnresolvedSnap := deriveH2FactSnapshot(&h2PolicyInput{
			agent: agent,
			req:   &OutboundMessageRequest{Msg: "unresolved nil conv"},
			routing: &routingResult{
				Recipient:   "agent:peer-bot",
				RecipientID: peerID,
			},
		})
		assert.NotEqual(t, "user", nilConvUnresolvedSnap.recipientKind)
		assert.Empty(t, nilConvUnresolvedSnap.surface)
	})

	t.Run("conv_result_group_classifies_strictly_from_conv_result_kind", func(t *testing.T) {
		snap := deriveH2FactSnapshot(&h2PolicyInput{
			agent: agent,
			req: &OutboundMessageRequest{
				ThreadID: dmKey,
				Msg:      "hello group",
			},
			routing: &routingResult{
				Recipient:      "user:alice@example.com",
				RecipientID:    userID,
				ConversationID: "g1",
				ConvResult: &messaging.ConversationResult{
					ConversationID: "g1",
					Kind:           "group",
					DisplayName:    "eng-room",
				},
			},
		})
		assert.Equal(t, messagepolicy.SurfaceAgentToConversation, snap.surface)
		assert.Equal(t, "conversation", snap.recipientKind)
		assert.Equal(t, "group", snap.convKind)
		assert.Equal(t, "g1", snap.recipientID)
		assert.Equal(t, "eng-room", snap.recipientSlug)
	})

	t.Run("conv_result_nil_falls_back_to_addressing_heuristics", func(t *testing.T) {
		groupByThread := deriveH2FactSnapshot(&h2PolicyInput{
			agent: agent,
			req: &OutboundMessageRequest{
				ThreadID: "1700000000.123",
				Msg:      "thread fallback",
			},
			routing: &routingResult{
				Recipient:   "user:alice@example.com",
				RecipientID: "t-conv",
			},
		})
		assert.Equal(t, messagepolicy.SurfaceAgentToConversation, groupByThread.surface)
		assert.Equal(t, "conversation", groupByThread.recipientKind)
		assert.Equal(t, "group", groupByThread.convKind)

		directFallback := deriveH2FactSnapshot(&h2PolicyInput{
			agent: agent,
			req: &OutboundMessageRequest{
				ThreadID: dmKey,
				Msg:      "dm fallback",
			},
			routing: &routingResult{
				Recipient:    "user:alice@example.com",
				RecipientID:  userID,
				UserResolved: true,
			},
		})
		assert.Equal(t, messagepolicy.SurfaceAgentToUser, directFallback.surface)
		assert.Equal(t, "user", directFallback.recipientKind)
		assert.Equal(t, "direct", directFallback.convKind)
		assert.Equal(t, userID, directFallback.recipientID)
		assert.Equal(t, "alice@example.com", directFallback.recipientSlug)
	})
}

// TestMessagePolicyGate_DirectConvNonDMThreadID_EnforceRejectsEndToEnd verifies
// R1 end to end through handleAgentOutboundMessage with a MessageBrokerProxy and
// external channel spoke: an agent_to_user-only enforce policy cannot be bypassed
// by addressing a direct user DM with channel="slack" and a non-dm: thread_id.
func TestMessagePolicyGate_DirectConvNonDMThreadID_EnforceRejectsEndToEnd(t *testing.T) {
	f := newMpgFixture(t, true)
	ctx := context.Background()

	slackSpoke := &mpgRecordingSpokeBus{}
	inprocBus := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = inprocBus.Close() })
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inprocBus},
		{Name: "web", Bus: nullSpokeEventBus{}},
		{Name: "slack", Bus: slackSpoke},
	}, slog.Default())
	proxy := NewMessageBrokerProxy(fanout, f.srv.store, f.srv.events, f.srv.GetDispatcher, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	f.srv.SetMessageBrokerProxy(proxy)
	f.srv.mu.RLock()
	proxy.webChatStore = f.srv.webChatStore
	f.srv.mu.RUnlock()
	proxy.subscribeProjectUserMessages(f.project.ID)

	dmKey, err := messages.DMConversationKey("agent", f.sender.ID, "user", f.alice.ID)
	require.NoError(t, err)
	dmConv, err := f.s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	pol := &store.MessagePolicy{
		ProjectID: f.project.ID,
		Name:      "dm-only-guard",
		Enabled:   true,
		Mode:      store.MessagePolicyModeEnforce,
		Scope: store.MessagePolicyScope{
			Surfaces:  []string{store.MessagePolicySurfaceAgentToUser},
			Sender:    store.MessagePolicySenderSelector{AllAgents: true},
			Recipient: store.MessagePolicyRecipientSelector{Kinds: []string{"user"}},
		},
		ViolationRule: `surface == "agent_to_user" && recipient.kind == "user" && conversation.kind == "direct" && message.body.matches("(?i)option a")`,
		OnFailed:      messagepolicy.OutcomeReject,
		RejectReason:  "DM blocked by {policy} to {recipient}",
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, pol))

	// 1. Bypass attempt via conversation_ref + external channel + non-dm: thread_id -> 422.
	rrRef := f.sendOutbound(t, OutboundMessageRequest{
		ConversationRef: "conv:" + dmConv.ID,
		Channel:         "slack",
		ThreadID:        "1700000000.123",
		Msg:             "Do you want Option A?",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rrRef.Code, "body: %s", rrRef.Body.String())

	var refErr ErrorResponse
	require.NoError(t, json.Unmarshal(rrRef.Body.Bytes(), &refErr))
	assert.Equal(t, string(MessageDenialPolicyRejected), refErr.Error.Code)
	assert.Equal(t, "DM blocked by dm-only-guard to alice@example.com", refErr.Error.Message)

	// 2. Bypass attempt via raw conversation_id + explicit recipient + non-dm: thread_id -> 422.
	rrID := f.sendOutbound(t, OutboundMessageRequest{
		ConversationID: dmConv.ID,
		Recipient:      "user:" + f.alice.Email,
		Channel:        "slack",
		ThreadID:       "1700000000.456",
		Msg:            "Still asking Option A?",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rrID.Code, "body: %s", rrID.Body.String())

	assert.Empty(t, slackSpoke.snapshot(), "rejected DM sends must never publish to the external channel spoke")
	msgs, err := f.s.ListMessages(ctx, store.MessageFilter{SenderID: f.sender.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "rejected DM sends must persist zero message rows")

	rejDecs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: f.project.ID,
		PolicyID:  pol.ID,
		Action:    store.MessagePolicyActionRejected,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, rejDecs.Items, 2)
	for _, d := range rejDecs.Items {
		assert.Equal(t, store.MessagePolicySurfaceAgentToUser, d.Surface)
		assert.Equal(t, "user", d.RecipientKind)
		assert.Equal(t, f.alice.ID, d.RecipientID)
		assert.Equal(t, store.MessagePolicyClassViolation, d.Class)
		assert.Empty(t, d.MessageID)
	}

	// 3. Compliant send with the exact same conversation_ref + external channel +
	// non-dm: thread_id succeeds and delivers to the slack spoke, proving the 422s
	// above came strictly from H2 agent_to_user policy evaluation.
	rrOK := f.sendOutbound(t, OutboundMessageRequest{
		ConversationRef: "conv:" + dmConv.ID,
		Channel:         "slack",
		ThreadID:        "1700000000.789",
		Msg:             "Deployment finished cleanly.",
	})
	require.Equal(t, http.StatusOK, rrOK.Code, "body: %s", rrOK.Body.String())
	delivered := slackSpoke.snapshot()
	require.Len(t, delivered, 1)
	assert.Equal(t, "1700000000.789", delivered[0].ThreadID)
	assert.Equal(t, "user:"+f.alice.Email, delivered[0].Recipient)

	admDecs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: f.project.ID,
		PolicyID:  pol.ID,
		Action:    store.MessagePolicyActionAdmitted,
	}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, admDecs.Items, 1)
	assert.Equal(t, store.MessagePolicySurfaceAgentToUser, admDecs.Items[0].Surface)
	assert.Equal(t, "user", admDecs.Items[0].RecipientKind)
	assert.Equal(t, f.alice.ID, admDecs.Items[0].RecipientID)
	assert.Equal(t, store.MessagePolicyClassPass, admDecs.Items[0].Class)
}

// TestMessagePolicyGate_ShadowDropMetrics_ConsistentUnitAndReasons verifies N2:
// scion.message_policy.shadow.drops uses unit "{evaluation}" and distinguishes
// "queue_full" and "policy_cap_exceeded" via the "reason" attribute, counting
// dropped policy evaluations consistently across both paths.
func TestMessagePolicyGate_ShadowDropMetrics_ConsistentUnitAndReasons(t *testing.T) {
	f := newMpgFixture(t, true)
	ctx := context.Background()
	f.srv.chatSendLimiter = nil

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	otelMetrics, err := NewOTelMessagePolicyMetrics(mp)
	require.NoError(t, err)
	f.srv.SetMessagePolicyMetrics(otelMetrics)

	// 1. Seed 12 shadow policies -> 1 send drops 2 excess policy evaluations with reason=policy_cap_exceeded.
	var created []*store.MessagePolicy
	for i := 1; i <= 12; i++ {
		p := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      fmt.Sprintf("s-metric-%02d", i),
			Enabled:   true,
			Mode:      store.MessagePolicyModeShadow,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: "false",
			OnFailed:      messagepolicy.OutcomeAdmit,
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, p))
		created = append(created, p)
	}

	rrCap := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "trigger shadow cap drop",
	})
	require.Equal(t, http.StatusOK, rrCap.Code)

	// Wait for that 1 shadow job to drain before blocking workers for queue_full.
	require.Eventually(t, func() bool {
		decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
		}, store.ListOptions{Limit: 20})
		return err == nil && len(decs.Items) == store.MaxActiveMatchingMessagePolicies
	}, 3*time.Second, 15*time.Millisecond)

	// 2. Keep exactly 2 shadow policies enabled and saturate the shadow queue with 5 overflow sends ->
	// 5 dropped batches * 2 policies = 10 dropped evaluations with reason=queue_full.
	for i := 2; i < len(created); i++ {
		created[i].Enabled = false
		require.NoError(t, f.s.UpdateMessagePolicy(ctx, created[i]))
	}

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

	workersBlocked := messagePolicyShadowWorkers
	for i := 0; i < workersBlocked; i++ {
		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       fmt.Sprintf("block worker %d", i),
		})
		require.Equal(t, http.StatusOK, rr.Code)
	}
	// Give the 2 workers a brief moment to dequeue and block on unblock.
	require.Eventually(t, func() bool {
		f.srv.messagePolicyGate.mu.Lock()
		defer f.srv.messagePolicyGate.mu.Unlock()
		return len(f.srv.messagePolicyGate.shadowCh) == 0
	}, time.Second, 5*time.Millisecond)

	overflowBatches := 5
	for i := 0; i < messagePolicyShadowQueueCap+overflowBatches; i++ {
		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       fmt.Sprintf("fill and overflow %d", i),
		})
		require.Equal(t, http.StatusOK, rr.Code)
	}

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))

	var foundMetric bool
	byReason := make(map[string]int64)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "scion.message_policy.shadow.drops" {
				continue
			}
			foundMetric = true
			assert.Equal(t, "{evaluation}", m.Unit)
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, dp := range sum.DataPoints {
				rVal, hasReason := dp.Attributes.Value("reason")
				assert.True(t, hasReason, "every shadow.drops data point must carry a reason attribute")
				byReason[rVal.AsString()] += dp.Value
			}
		}
	}
	require.True(t, foundMetric, "expected scion.message_policy.shadow.drops metric")
	assert.Equal(t, int64(2), byReason[messagepolicy.ReasonPolicyCapExceeded])
	assert.Equal(t, int64(overflowBatches*2), byReason[shadowDropReasonQueueFull])
	assert.Equal(t, int64(2+overflowBatches*2), f.srv.messagePolicyShadowDrops.Load())

	close(unblock)
}

// TestMessagePolicyGate_TwoAgentDirectConvUncovered_HumanDMStillRejected verifies
// the agent-to-agent DM over-match fix:
//  1. An enforce agent_to_user policy (plus a shadow policy) with a constructed
//     two-agent direct ConvResult at H2 (RecipientID empty or equal to the
//     non-sender agent) is treated as UNCOVERED: admitted with zero policy
//     evaluations, zero decision rows, and log + metric uncovered_path=agent_dm.
//  2. A human DM under the exact same policy is still rejected (422) both at H2
//     and end-to-end through handleAgentOutboundMessage.
func TestMessagePolicyGate_TwoAgentDirectConvUncovered_HumanDMStillRejected(t *testing.T) {
	f := newMpgFixture(t, true)
	ctx := context.Background()

	var logBuf bytes.Buffer
	var logMu sync.Mutex
	f.srv.messageLog = slog.New(slog.NewJSONHandler(&mpgLockedWriter{mu: &logMu, w: &logBuf}, &slog.HandlerOptions{Level: slog.LevelInfo}))

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	otelMetrics, err := NewOTelMessagePolicyMetrics(mp)
	require.NoError(t, err)
	f.srv.SetMessagePolicyMetrics(otelMetrics)

	// Enforce policy omits scope.recipient.kinds so any surface="agent_to_user"
	// classification would match and reject via recipient.kind != "agent" or body regex.
	enfPol := &store.MessagePolicy{
		ProjectID: f.project.ID,
		Name:      "enforce-agent-to-user",
		Enabled:   true,
		Mode:      store.MessagePolicyModeEnforce,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
			Sender:   store.MessagePolicySenderSelector{AllAgents: true},
		},
		ViolationRule: `recipient.kind != "agent" && !message.has_choices && message.body.matches("(?i)option a")`,
		OnFailed:      messagepolicy.OutcomeReject,
		RejectReason:  "Blocked by {policy} to {recipient}",
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, enfPol))

	shadowPol := &store.MessagePolicy{
		ProjectID: f.project.ID,
		Name:      "shadow-agent-to-user",
		Enabled:   true,
		Mode:      store.MessagePolicyModeShadow,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
			Sender:   store.MessagePolicySenderSelector{AllAgents: true},
		},
		ViolationRule: `true`,
		OnFailed:      messagepolicy.OutcomeAdmit,
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, shadowPol))

	// Exercise both lexical UUID orders in dm:agent:<id1>:agent:<id2> and both
	// RecipientID == "" and RecipientID == peerID.
	const secretBody = "CONFIDENTIAL-AGENT-DM: Should we pick Option A?"
	var expectedUncovered int64
	for _, peerID := range []string{
		f.peerAgent.ID,
		"00000000-0000-4000-8000-000000000001",
		"f9999999-9999-4999-8999-999999999999",
	} {
		agentDMKey, keyErr := messages.DMConversationKey("agent", f.sender.ID, "agent", peerID)
		require.NoError(t, keyErr)

		for _, tc := range []struct {
			name        string
			recipient   string
			recipientID string
		}{
			{name: "empty_recipient_id", recipient: "", recipientID: ""},
			{name: "peer_agent_recipient_id", recipient: "agent:" + f.peerAgent.Slug, recipientID: peerID},
			{name: "kindless_peer_agent_recipient_id", recipient: "", recipientID: peerID},
		} {
			expectedUncovered++
			rr := httptest.NewRecorder()
			out, ok := f.srv.evaluateH2MessagePolicies(ctx, rr, &h2PolicyInput{
				agent: f.sender,
				req: &OutboundMessageRequest{
					Recipient:   tc.recipient,
					RecipientID: tc.recipientID,
					Msg:         secretBody,
					Type:        "instruction",
				},
				routing: &routingResult{
					Recipient:      tc.recipient,
					RecipientID:    tc.recipientID,
					ConversationID: "conv-agent-dm",
					ConvResult: &messaging.ConversationResult{
						ConversationID: "conv-agent-dm",
						Kind:           "direct",
						ExternalRef:    agentDMKey,
					},
				},
			})
			require.True(t, ok, "case %s (peer=%s) must be admitted as uncovered; body=%s", tc.name, peerID, rr.Body.String())
			assert.Equal(t, http.StatusOK, rr.Code)
			assert.Empty(t, rr.Body.String())
			assert.Empty(t, out.admittedDecisions)
			assert.Nil(t, out.shadowJob)
			f.srv.recordAdmittedH2PolicyDecisions(ctx, &out, api.NewUUID())
		}
	}

	assert.Zero(t, f.faultWrapper.listCalls.Load(), "uncovered agent_dm at H2 must perform zero policy loads/evaluations")
	assert.Zero(t, f.faultWrapper.decisionCalls.Load(), "uncovered agent_dm at H2 must persist zero decision rows")
	allDecs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: f.project.ID,
	}, store.ListOptions{Limit: 20})
	require.NoError(t, err)
	assert.Empty(t, allDecs.Items, "uncovered agent_dm must produce zero decision rows")

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	assert.Equal(t, int64(0), mpgSumInt64Metric(t, rm, "scion.message_policy.evaluations"))
	assert.Equal(t, expectedUncovered, f.srv.messagePolicyUncovered.Load())

	var uncoveredByPath = make(map[string]int64)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "scion.message_policy.uncovered" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, dp := range sum.DataPoints {
				v, hasAttr := dp.Attributes.Value("uncovered_path")
				assert.True(t, hasAttr, "uncovered metric must carry uncovered_path attribute")
				uncoveredByPath[v.AsString()] += dp.Value
			}
		}
	}
	assert.Equal(t, expectedUncovered, uncoveredByPath[messagepolicy.SurfaceAgentDM])

	logMu.Lock()
	logs := logBuf.String()
	logMu.Unlock()
	assert.Contains(t, logs, `"uncovered_path":"agent_dm"`)
	assert.NotContains(t, logs, "CONFIDENTIAL-AGENT-DM", "uncovered log must stay content-free")

	// (2) Human DM under the exact same policy is still rejected (both at H2 and end-to-end).
	userDMKey, err := messages.DMConversationKey("agent", f.sender.ID, "user", f.alice.ID)
	require.NoError(t, err)
	userDMConv, err := f.s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: userDMKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	rrH2Human := httptest.NewRecorder()
	_, humanOK := f.srv.evaluateH2MessagePolicies(ctx, rrH2Human, &h2PolicyInput{
		agent: f.sender,
		req: &OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "Do you want Option A?",
			Type:      "input-needed",
		},
		routing: &routingResult{
			Recipient:      "user:" + f.alice.Email,
			RecipientID:    f.alice.ID,
			UserResolved:   true,
			ConversationID: userDMConv.ID,
			ConvResult: &messaging.ConversationResult{
				ConversationID: userDMConv.ID,
				Kind:           "direct",
				ExternalRef:    userDMKey,
			},
		},
	})
	require.False(t, humanOK, "human DM at H2 must still be rejected")
	require.Equal(t, http.StatusUnprocessableEntity, rrH2Human.Code)

	// End-to-end human DMs (explicit user email, conversation_ref with no explicit
	// recipient, and asserted conversation_id + kindless recipient_id) all reject 422.
	for _, req := range []OutboundMessageRequest{
		{
			Recipient: "user:" + f.alice.Email,
			Msg:       "Do you want Option A?",
		},
		{
			ConversationRef: "conv:" + userDMConv.ID,
			Msg:             "Do you want Option A via conv ref?",
		},
		{
			ConversationID: userDMConv.ID,
			RecipientID:    f.alice.ID,
			Msg:            "Do you want Option A via conv_id + recipient_id?",
		},
	} {
		rrHuman := f.sendOutbound(t, req)
		require.Equal(t, http.StatusUnprocessableEntity, rrHuman.Code, "body: %s", rrHuman.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rrHuman.Body.Bytes(), &errResp))
		assert.Equal(t, string(MessageDenialPolicyRejected), errResp.Error.Code)
	}

	assert.Zero(t, f.recordChannel.count(), "rejected human DMs must never deliver to user channels")
	rejDecs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: f.project.ID,
		PolicyID:  enfPol.ID,
		Action:    store.MessagePolicyActionRejected,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, rejDecs.Items, 4)
	for _, d := range rejDecs.Items {
		assert.Equal(t, store.MessagePolicySurfaceAgentToUser, d.Surface)
		assert.Equal(t, "user", d.RecipientKind)
		assert.Equal(t, f.alice.ID, d.RecipientID)
		assert.Equal(t, store.MessagePolicyClassViolation, d.Class)
	}
}

// TestMessagePolicyGate_TwoAgentDirectConvNoExplicitRecipient_RoutesToExecuteAgentDMNeverReachesH2
// pins outbound routing (test 3): a two-agent direct conversation addressed with
// no explicit recipient goes to ExecuteAgentDM and never reaches H2.
func TestMessagePolicyGate_TwoAgentDirectConvNoExplicitRecipient_RoutesToExecuteAgentDMNeverReachesH2(t *testing.T) {
	f := newMpgFixture(t, true)
	ctx := context.Background()

	agentDMKey, err := messages.DMConversationKey("agent", f.sender.ID, "agent", f.peerAgent.ID)
	require.NoError(t, err)
	agentDMConv, err := f.s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: agentDMKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	// Seed a catch-all enforce policy AND arm the store fault switch on
	// ListMessagePolicies / CreateMessagePolicyDecision so reaching H2 (either
	// covered or uncovered) would be immediately observable.
	pol := &store.MessagePolicy{
		ProjectID: f.project.ID,
		Name:      "reject-everything-at-h2",
		Enabled:   true,
		Mode:      store.MessagePolicyModeEnforce,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{
				store.MessagePolicySurfaceAgentToUser,
				store.MessagePolicySurfaceAgentToConversation,
			},
			Sender: store.MessagePolicySenderSelector{AllAgents: true},
		},
		ViolationRule: `true`,
		OnFailed:      messagepolicy.OutcomeReject,
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, pol))
	f.faultSwitch.Arm()

	rr := f.sendOutbound(t, OutboundMessageRequest{
		ConversationRef: "conv:" + agentDMConv.ID,
		Msg:             "Do you want Option A?",
		Type:            "instruction",
	})
	require.Equal(t, http.StatusOK, rr.Code, "two-agent direct conversation must succeed via ExecuteAgentDM; body: %s", rr.Body.String())

	calls := f.dispatcher.getCalls()
	require.Len(t, calls, 1, "expected 1 ExecuteAgentDM dispatch call")
	assert.Equal(t, f.peerAgent.ID, calls[0].Agent.ID)
	assert.Zero(t, f.recordChannel.count(), "agent DM must not deliver through user channels")

	// H2 was never reached at all (neither policy load nor H2 uncovered counter).
	assert.Zero(t, f.faultWrapper.listCalls.Load(), "ExecuteAgentDM path must never reach H2 ListMessagePolicies")
	assert.Zero(t, f.faultWrapper.decisionCalls.Load(), "ExecuteAgentDM path must never persist H2 policy decisions")
	assert.Zero(t, f.srv.messagePolicyUncovered.Load(), "ExecuteAgentDM path returns before H2 and must not hit H2 uncovered fallback")

	msgs, err := f.s.ListMessages(ctx, store.MessageFilter{SenderID: f.sender.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, msgs.Items, 1)
	assert.Equal(t, f.peerAgent.ID, msgs.Items[0].RecipientID)
	assert.Equal(t, "agent:"+f.peerAgent.Slug, msgs.Items[0].Recipient)
	assert.Equal(t, agentDMConv.ID, msgs.Items[0].ConversationID)
}

type mpgLockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (lw *mpgLockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}

// TestMessagePolicyGate_BodyExceedsMaxBodyBytes_FailsBodyTruncated verifies
// security finding M-1 (baseline): when the outbound message body exceeds
// messagepolicy.MaxBodyBytes (16 KiB), every matching policy classifies as
// failed/body_truncated and resolves through on_failed / on_failed_reasons:
//  1. Multibyte-padded body (>16 KiB, <16,000 runes) with a contains() enforce
//     rule gives failed/body_truncated, and on_failed=reject returns HTTP 422.
//  2. Guarded has(message.body) rules (both && and !has(...) ||) still classify
//     as failed/body_truncated (never pass).
//  3. Body-independent rules on a >16 KiB body also classify as
//     failed/body_truncated (documented baseline), and on_failed_reasons accepts
//     body_truncated.
func TestMessagePolicyGate_BodyExceedsMaxBodyBytes_FailsBodyTruncated(t *testing.T) {
	// 5,462 3-byte runes ("界") = 16,386 bytes (> MaxBodyBytes 16,384) while
	// staying well under messages.MaxMessageLength (16,000 runes).
	paddedForbidden := strings.Repeat("界", 5462) + " FORBIDDEN_TOKEN_991"
	require.Greater(t, len(paddedForbidden), messagepolicy.MaxBodyBytes)
	require.Less(t, len([]rune(paddedForbidden)), messages.MaxMessageLength)

	paddedBenign := strings.Repeat("界", 5462) + " routine status report"
	require.Greater(t, len(paddedBenign), messagepolicy.MaxBodyBytes)
	require.Less(t, len([]rune(paddedBenign)), messages.MaxMessageLength)

	t.Run("contains_rule_and_guarded_has_body_rules_fail_body_truncated_422", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			rule string
			body string
		}{
			{
				name: "contains_rule",
				rule: `message.body.contains("FORBIDDEN_TOKEN_991")`,
				body: paddedForbidden,
			},
			{
				name: "guarded_has_body_and",
				rule: `has(message.body) && message.body.contains("FORBIDDEN_TOKEN_991")`,
				body: paddedBenign,
			},
			{
				name: "guarded_not_has_body_or",
				rule: `!has(message.body) || message.body.contains("FORBIDDEN_TOKEN_991")`,
				body: paddedForbidden,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newMpgFixture(t, true)
				ctx := context.Background()

				pol := &store.MessagePolicy{
					ProjectID: f.project.ID,
					Name:      "m1-" + strings.ReplaceAll(tc.name, "_", "-"),
					Enabled:   true,
					Mode:      store.MessagePolicyModeEnforce,
					Scope: store.MessagePolicyScope{
						Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
						Sender:   store.MessagePolicySenderSelector{AllAgents: true},
					},
					ViolationRule: tc.rule,
					OnFailed:      messagepolicy.OutcomeReject,
					RejectReason:  "Oversize message rejected by {policy}",
				}
				require.NoError(t, f.s.CreateMessagePolicy(ctx, pol))

				rr := f.sendOutbound(t, OutboundMessageRequest{
					Recipient: "user:" + f.alice.Email,
					Msg:       tc.body,
				})
				require.Equal(t, http.StatusUnprocessableEntity, rr.Code, "body: %s", rr.Body.String())
				assert.NotContains(t, rr.Body.String(), "FORBIDDEN_TOKEN_991")

				var errResp ErrorResponse
				require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
				assert.Equal(t, string(MessageDenialPolicyRejected), errResp.Error.Code)
				assert.Equal(t, messagepolicy.ClassFailed, errResp.Error.Details["class"])

				assert.Zero(t, f.recordChannel.count(), "rejected padded send must not deliver")
				msgs, err := f.s.ListMessages(ctx, store.MessageFilter{SenderID: f.sender.ID}, store.ListOptions{Limit: 10})
				require.NoError(t, err)
				assert.Empty(t, msgs.Items)

				decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
					ProjectID: f.project.ID,
					PolicyID:  pol.ID,
				}, store.ListOptions{Limit: 10})
				require.NoError(t, err)
				require.Len(t, decs.Items, 1)
				assert.Equal(t, store.MessagePolicyClassFailed, decs.Items[0].Class)
				assert.Equal(t, messagepolicy.ReasonBodyTruncated, decs.Items[0].CELErrorReason)
				assert.Equal(t, store.MessagePolicyActionRejected, decs.Items[0].Action)
			})
		}
	})

	t.Run("body_independent_rule_also_fails_body_truncated_and_respects_on_failed_reasons", func(t *testing.T) {
		f := newMpgFixture(t, true)
		ctx := context.Background()

		// Verify a short non-urgent message passes this body-independent rule.
		pol := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "body-independent-urgent-guard",
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: `message.urgent && recipient.kind == "user"`,
			OnFailed:      messagepolicy.OutcomeReject,
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, pol))

		shadowPol := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "shadow-body-independent",
			Enabled:   true,
			Mode:      store.MessagePolicyModeShadow,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: `message.has_choices`,
			OnFailed:      messagepolicy.OutcomeReject,
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, shadowPol))

		rrShort := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "short non-urgent update",
			Urgent:    false,
		})
		require.Equal(t, http.StatusOK, rrShort.Code)

		// Long body (>16 KiB) with Urgent=false STILL classifies as
		// failed/body_truncated (documented baseline) and rejects under on_failed=reject.
		rrLongReject := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       paddedBenign,
			Urgent:    false,
		})
		require.Equal(t, http.StatusUnprocessableEntity, rrLongReject.Code, "body: %s", rrLongReject.Body.String())

		// Validate authoring accepts on_failed_reasons["body_truncated"] = "admit",
		// then confirm both enforce and shadow record failed/body_truncated.
		_, valErr := f.srv.MessagePolicyEvaluator().ValidateAuthoring(ctx, messagepolicy.AuthoringSpec{
			Name:            pol.Name,
			Mode:            pol.Mode,
			Scope:           messagepolicy.Scope{Surfaces: []string{messagepolicy.SurfaceAgentToUser}},
			ViolationRule:   pol.ViolationRule,
			OnFailed:        messagepolicy.OutcomeReject,
			OnFailedReasons: map[string]string{messagepolicy.ReasonBodyTruncated: messagepolicy.OutcomeAdmit},
		})
		require.NoError(t, valErr)

		pol.OnFailedReasons = map[string]string{messagepolicy.ReasonBodyTruncated: messagepolicy.OutcomeAdmit}
		require.NoError(t, f.s.UpdateMessagePolicy(ctx, pol))

		rrLongAdmit := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       paddedBenign,
			Urgent:    false,
		})
		require.Equal(t, http.StatusOK, rrLongAdmit.Code, "body: %s", rrLongAdmit.Body.String())

		require.Eventually(t, func() bool {
			shDecs, listErr := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
				ProjectID: f.project.ID,
				PolicyID:  shadowPol.ID,
				Class:     store.MessagePolicyClassFailed,
			}, store.ListOptions{Limit: 10})
			return listErr == nil && len(shDecs.Items) >= 2
		}, 3*time.Second, 15*time.Millisecond)

		enfDecs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
			PolicyID:  pol.ID,
			Class:     store.MessagePolicyClassFailed,
		}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, enfDecs.Items, 2)
		actions := []string{enfDecs.Items[0].Action, enfDecs.Items[1].Action}
		assert.ElementsMatch(t, []string{store.MessagePolicyActionRejected, store.MessagePolicyActionAdmitted}, actions)
		for _, d := range enfDecs.Items {
			assert.Equal(t, messagepolicy.ReasonBodyTruncated, d.CELErrorReason)
		}

		shDecs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
			ProjectID: f.project.ID,
			PolicyID:  shadowPol.ID,
			Class:     store.MessagePolicyClassFailed,
		}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		for _, d := range shDecs.Items {
			assert.Equal(t, store.MessagePolicyActionShadow, d.Action)
			assert.Equal(t, messagepolicy.ReasonBodyTruncated, d.CELErrorReason)
		}
	})
}

// TestMessagePolicyGate_RequestCanceledDuringEval_AbortsWithoutDelivery verifies
// security finding L-2(a): when the HTTP request context is canceled during H2
// evaluation, evaluateH2MessagePolicies aborts without delivery even when
// on_failed=admit (and on_failed_reasons["canceled"]="admit") and even when
// MessageBrokerProxy is active (whose subscriber persists on a detached context).
func TestMessagePolicyGate_RequestCanceledDuringEval_AbortsWithoutDelivery(t *testing.T) {
	f := newMpgFixture(t, true)
	ctx := context.Background()

	slackSpoke := &mpgRecordingSpokeBus{}
	inprocBus := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = inprocBus.Close() })
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inprocBus},
		{Name: "web", Bus: nullSpokeEventBus{}},
		{Name: "slack", Bus: slackSpoke},
	}, slog.Default())
	proxy := NewMessageBrokerProxy(fanout, f.srv.store, f.srv.events, f.srv.GetDispatcher, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	f.srv.SetMessageBrokerProxy(proxy)
	f.srv.mu.RLock()
	proxy.webChatStore = f.srv.webChatStore
	f.srv.mu.RUnlock()
	proxy.subscribeProjectUserMessages(f.project.ID)

	pol := &store.MessagePolicy{
		ProjectID: f.project.ID,
		Name:      "fail-open-on-cancel-test",
		Enabled:   true,
		Mode:      store.MessagePolicyModeEnforce,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
			Sender:   store.MessagePolicySenderSelector{AllAgents: true},
		},
		ViolationRule:   `true`,
		OnFailed:        messagepolicy.OutcomeAdmit,
		OnFailedReasons: map[string]string{celengine.ReasonCanceled: messagepolicy.OutcomeAdmit},
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, pol))

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()

	f.srv.messagePolicyGate.evalOverride = func(evalCtx context.Context, p *store.MessagePolicy, act messagepolicy.Activation, tracker *ruleengine.BudgetTracker) (messagepolicy.Classification, bool) {
		cancelReq()
		c := f.srv.MessagePolicyEvaluator().EvaluateViolation(evalCtx, p.ID, p.Revision, p.ViolationRule, act, messagepolicy.WithBudget(tracker))
		require.Equal(t, messagepolicy.ClassFailed, c.Class)
		require.Equal(t, celengine.ReasonCanceled, c.Reason)
		return c, true
	}
	t.Cleanup(func() { f.srv.messagePolicyGate.evalOverride = nil })

	raw, err := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Channel:   "slack",
		Msg:       "must never deliver on canceled request",
	})
	require.NoError(t, err)
	httpReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+f.project.ID+"/agents/"+f.sender.ID+"/outbound-message",
		bytes.NewReader(raw))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq = httpReq.WithContext(contextWithIdentity(reqCtx, &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: f.sender.ID},
		ProjectID: f.project.ID,
		Ancestry:  f.sender.Ancestry,
	}}))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, httpReq, f.sender.ID)

	assert.Equal(t, 499, rr.Code, "canceled request must abort without admitting; body: %s", rr.Body.String())
	assert.Empty(t, slackSpoke.snapshot(), "canceled request must never publish to channel spoke")
	assert.Zero(t, f.recordChannel.count(), "canceled request must never deliver")

	// Poll briefly to ensure no detached broker subscriber persisted the message.
	for deadline := time.Now().Add(150 * time.Millisecond); time.Now().Before(deadline); time.Sleep(15 * time.Millisecond) {
		msgs, listErr := f.s.ListMessages(ctx, store.MessageFilter{SenderID: f.sender.ID}, store.ListOptions{Limit: 10})
		require.NoError(t, listErr)
		require.Empty(t, msgs.Items, "canceled request must persist zero message rows")
	}
}

func mpgSeedShadowBenchmarkPolicies(tb testing.TB, ctx context.Context, s store.Store, projectID string) []*store.MessagePolicy {
	tb.Helper()
	specs := []struct {
		name string
		rule string
	}{
		{
			name: "shadow-01-no-prose-choices",
			rule: `recipient.kind != "agent" && !message.has_choices && message.body.matches("(?i)(\\bchoose\\s+(between|one\\s+of)\\b|\\boption\\s+[a-d1-4]\\b)")`,
		},
		{
			name: "shadow-02-urgent-attachment-guard",
			rule: `message.urgent && message.attachment_count > 3 && sender.slug != "release-bot"`,
		},
		{
			name: "shadow-03-mention-fanout-watch",
			rule: `size(mentioned_user_ids) > 2 && message.body.contains("@")`,
		},
	}
	out := make([]*store.MessagePolicy, 0, len(specs))
	for _, sp := range specs {
		pol := &store.MessagePolicy{
			ProjectID: projectID,
			Name:      sp.name,
			Enabled:   true,
			Mode:      store.MessagePolicyModeShadow,
			Scope: store.MessagePolicyScope{
				Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
				Sender:   store.MessagePolicySenderSelector{AllAgents: true},
			},
			ViolationRule: sp.rule,
			OnFailed:      messagepolicy.OutcomeAdmit,
		}
		require.NoError(tb, s.CreateMessagePolicy(ctx, pol))
		out = append(out, pol)
	}
	return out
}

func mpgPercentileDuration(samples []time.Duration, pct float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	idx := int(float64(len(sorted)-1) * pct)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// TestMessagePolicyGate_ShadowOverheadP99Under5ms verifies Target 3 during unit
// test runs: warm H2 gate overhead with active shadow policies stays under 5 ms
// at p99.
func TestMessagePolicyGate_ShadowOverheadP99Under5ms(t *testing.T) {
	f := newMpgFixture(t, true)
	ctx := context.Background()
	mpgSeedShadowBenchmarkPolicies(t, ctx, f.s, f.project.ID)

	req := &OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       strings.Repeat("Build and test suite completed cleanly across packages. ", 18) + "Option B looks ready.",
		Type:      "input-needed",
	}
	route := &routingResult{
		Recipient:    "user:" + f.alice.Email,
		RecipientID:  f.alice.ID,
		UserResolved: true,
	}
	evalInput := &h2PolicyInput{
		agent:   f.sender,
		req:     req,
		routing: route,
	}
	const msgID = "00000000-0000-4000-8000-000000000001"

	// Warm program cache and SQLite pages.
	for i := 0; i < 5; i++ {
		rr := httptest.NewRecorder()
		out, ok := f.srv.evaluateH2MessagePolicies(ctx, rr, evalInput)
		require.True(t, ok)
		f.srv.recordAdmittedH2PolicyDecisions(ctx, &out, msgID)
	}

	const N = 80
	durations := make([]time.Duration, N)
	for i := 0; i < N; i++ {
		rr := httptest.NewRecorder()
		start := time.Now()
		out, ok := f.srv.evaluateH2MessagePolicies(ctx, rr, evalInput)
		f.srv.recordAdmittedH2PolicyDecisions(ctx, &out, msgID)
		durations[i] = time.Since(start)
		require.True(t, ok)
	}
	p50 := mpgPercentileDuration(durations, 0.50)
	p99 := mpgPercentileDuration(durations, 0.99)
	t.Logf("H2 shadow gate warm latency (N=%d, 3 shadow policies): p50=%s p99=%s", N, p50, p99)
	assert.Less(t, p99, 5*time.Millisecond, "shadow-only H2 gate p99 must stay under 5 ms (got %s)", p99)
}

func newMpgBenchmarkEnv(b *testing.B) (*Server, store.Store, *store.Project, *store.Agent, *store.User, []*store.MessagePolicy) {
	b.Helper()
	ctx := context.Background()
	s, err := newTestStore(b, ":memory:")
	require.NoError(b, err)

	srv, err := New(ServerConfig{}, s)
	require.NoError(b, err)
	b.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.CleanupResources(shutCtx)
		_ = s.Close()
	})
	mpgSetExperiment(b, srv, true, true)

	proj := &store.Project{
		ID:   api.NewUUID(),
		Name: "Bench Project",
		Slug: "bench-project",
	}
	require.NoError(b, s.CreateProject(ctx, proj))

	alice := &store.User{
		ID:          api.NewUUID(),
		Email:       "alice-bench@example.com",
		DisplayName: "Alice Bench",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(b, s.CreateUser(ctx, alice))

	sender := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "bench-sender",
		Name:      "Bench Sender",
		ProjectID: proj.ID,
		Phase:     "running",
	}
	require.NoError(b, s.CreateAgent(ctx, sender))

	policies := mpgSeedShadowBenchmarkPolicies(b, ctx, s, proj.ID)
	return srv, s, proj, sender, alice, policies
}

// BenchmarkMessagePolicyGate_ShadowOnly measures hot-path H2 gate latency when
// only shadow policies are active (Target 3: p99 < 5 ms).
func BenchmarkMessagePolicyGate_ShadowOnly(b *testing.B) {
	srv, _, _, sender, alice, _ := newMpgBenchmarkEnv(b)
	ctx := context.Background()

	req := &OutboundMessageRequest{
		Recipient: "user:" + alice.Email,
		Msg:       strings.Repeat("Build and test suite completed cleanly across packages. ", 18) + "Option B looks ready.",
		Type:      "input-needed",
	}
	route := &routingResult{
		Recipient:    "user:" + alice.Email,
		RecipientID:  alice.ID,
		UserResolved: true,
	}
	evalInput := &h2PolicyInput{
		agent:   sender,
		req:     req,
		routing: route,
	}
	const msgID = "00000000-0000-4000-8000-000000000001"

	rr := httptest.NewRecorder()
	// Warm program cache.
	outWarm, _ := srv.evaluateH2MessagePolicies(ctx, rr, evalInput)
	srv.recordAdmittedH2PolicyDecisions(ctx, &outWarm, msgID)

	durations := make([]time.Duration, 0, b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t0 := time.Now()
		out, ok := srv.evaluateH2MessagePolicies(ctx, rr, evalInput)
		srv.recordAdmittedH2PolicyDecisions(ctx, &out, msgID)
		d := time.Since(t0)
		if !ok {
			b.Fatal("unexpected shadow gate rejection")
		}
		durations = append(durations, d)
	}
	b.StopTimer()

	p50 := mpgPercentileDuration(durations, 0.50)
	p99 := mpgPercentileDuration(durations, 0.99)
	b.ReportMetric(float64(p50.Nanoseconds())/1e6, "p50_ms")
	b.ReportMetric(float64(p99.Nanoseconds())/1e6, "p99_ms")
	if p99 >= 5*time.Millisecond {
		b.Fatalf("shadow gate p99 %s exceeded 5ms target", p99)
	}
}

// BenchmarkMessagePolicyGate_ShadowCacheMiss measures synchronous CEL recompile
// + evaluate latency when the scoped 1 MiB program cache misses on every evaluation
// (implementation brief §1, Target 3).
func BenchmarkMessagePolicyGate_ShadowCacheMiss(b *testing.B) {
	srv, _, proj, sender, alice, policies := newMpgBenchmarkEnv(b)
	ctx := context.Background()

	pol := policies[0]
	act := messagepolicy.NewActivation(messagepolicy.Facts{
		Surface: messagepolicy.SurfaceAgentToUser,
		Sender: messagepolicy.SenderFacts{
			ID:        sender.ID,
			Slug:      sender.Slug,
			ProjectID: proj.ID,
		},
		Recipient: messagepolicy.RecipientFacts{
			Kind: "user",
			ID:   alice.ID,
			Slug: alice.Email,
		},
		Message: messagepolicy.MessageFacts{
			Type: "input-needed",
			Body: strings.Repeat("Build and test suite completed cleanly across packages. ", 18) + "Option B looks ready.",
		},
		Conversation: messagepolicy.ConversationFacts{
			Kind: "direct",
		},
	})

	durations := make([]time.Duration, 0, b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		srv.RuleEngine().Invalidate(ruleengine.DialectCEL, messagepolicy.EnvironmentName, celengine.CacheScope{
			Consumer: messagepolicy.ConsumerName,
			ID:       pol.ID,
			Revision: pol.Revision,
		})
		budget := messagepolicy.NewSendBudget()
		b.StartTimer()

		t0 := time.Now()
		c := srv.evalOneMessagePolicy(ctx, pol, act, budget)
		d := time.Since(t0)
		if c.Class != messagepolicy.ClassViolation {
			b.Fatalf("expected ClassViolation on cache-miss eval, got %+v", c)
		}
		durations = append(durations, d)
	}
	b.StopTimer()

	p50 := mpgPercentileDuration(durations, 0.50)
	p99 := mpgPercentileDuration(durations, 0.99)
	b.ReportMetric(float64(p50.Nanoseconds())/1e6, "p50_ms")
	b.ReportMetric(float64(p99.Nanoseconds())/1e6, "p99_ms")
}
