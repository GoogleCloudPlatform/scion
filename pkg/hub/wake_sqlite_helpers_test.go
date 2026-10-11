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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// createWakeDMFixtures creates a project, broker, project-provider, sender
// agent (always running), and target agent (with the given phase) for wake
// DM tests. Both agents are in the same project with project-level message
// mode so authorization succeeds.
func createWakeDMFixtures(t *testing.T, targetPhase string) (
	srv *Server, s store.Store,
	senderAgent, targetAgent *store.Agent,
) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("project-wake-dm-" + targetPhase),
		Name: "Wake DM Test Project",
		Slug: "wake-dm-test-project-" + targetPhase,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     tid("broker-wake-dm-" + targetPhase),
		Name:   "Wake DM Test Broker",
		Slug:   "wake-dm-test-broker-" + targetPhase,
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	// Create an owner user for ancestry.
	owner := &store.User{
		ID:      tid("owner-wake-dm-" + targetPhase),
		Email:   "owner-wake-dm-" + targetPhase + "@test.example",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	// The owner is a project member, so the fixture agents are in good
	// standing (ptone/scion#3433).
	ensureStandingRoot(t, s, project.ID, owner.ID)

	senderAgent = &store.Agent{
		ID:              tid("sender-wake-dm-" + targetPhase),
		Slug:            "sender-wake-dm-" + targetPhase,
		Name:            "Sender Agent",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, senderAgent))

	targetAgent = &store.Agent{
		ID:              tid("target-wake-dm-" + targetPhase),
		Slug:            "target-wake-dm-" + targetPhase,
		Name:            "Target Agent",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           targetPhase,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, targetAgent))

	return srv, s, senderAgent, targetAgent
}

// wakeTrackingDispatcher records DispatchAgentStart and DispatchAgentMessage
// calls to verify wake invocations.
type wakeTrackingDispatcher struct {
	mu2            sync.Mutex
	messageCalls   []wakeDispatchMsg
	startCalls     []wakeDispatchStart
	startReturnErr error
	msgReturnErr   error
}

// wakeDMTestIdentity implements the AgentIdentity interface for test agent senders.
type wakeDMTestIdentity struct {
	id        string
	projectID string
	ancestry  []string
}

func newWakeQuotaFixture(t *testing.T, name string, limit int64) *wakeQuotaFixture {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	setBrokerAgentCeiling(t, s, limit)
	grantDevUserRuntimeBrokerAccess(t, s)
	broker, project := newQuotaTestBrokerAndProject(t, s, name)
	target := newQuotaTestAgent(t, s, broker, project, name+"-target", state.PhaseSuspended)
	return &wakeQuotaFixture{srv, s, broker, project, target}
}

func (d *wakeTrackingDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, prompt string, cont bool) error {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	d.startCalls = append(d.startCalls, wakeDispatchStart{AgentID: agent.ID, Prompt: prompt, Continue: cont})
	return d.startReturnErr
}

func (d *wakeTrackingDispatcher) DispatchAgentMessage(_ context.Context, agent *store.Agent, message string, interrupt bool, _ *messages.StructuredMessage) error {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	d.messageCalls = append(d.messageCalls, wakeDispatchMsg{AgentID: agent.ID, Message: message, Interrupt: interrupt})
	return d.msgReturnErr
}

func (d *wakeTrackingDispatcher) getStartCalls() []wakeDispatchStart {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	out := make([]wakeDispatchStart, len(d.startCalls))
	copy(out, d.startCalls)
	return out
}

func (d *wakeTrackingDispatcher) getMessageCalls() []wakeDispatchMsg {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	out := make([]wakeDispatchMsg, len(d.messageCalls))
	copy(out, d.messageCalls)
	return out
}

// Implement remaining AgentDispatcher methods as no-ops.
func (d *wakeTrackingDispatcher) DispatchAgentCreate(_ context.Context, _ *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *wakeTrackingDispatcher) DispatchAgentProvision(_ context.Context, _ *store.Agent) error {
	return nil
}

func (d *wakeTrackingDispatcher) DispatchAgentReprovision(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *wakeTrackingDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *wakeTrackingDispatcher) DispatchAgentRestart(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *wakeTrackingDispatcher) DispatchAgentResetAuth(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *wakeTrackingDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	return nil
}
func (d *wakeTrackingDispatcher) DispatchCheckAgentPrompt(_ context.Context, _ *store.Agent) (bool, error) {
	return false, nil
}
func (d *wakeTrackingDispatcher) DispatchAgentCreateWithGather(_ context.Context, _ *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *wakeTrackingDispatcher) DispatchAgentLogs(_ context.Context, _ *store.Agent, _ int) (string, error) {
	return "", nil
}
func (d *wakeTrackingDispatcher) DispatchAgentExec(_ context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	return "", 0, nil
}
func (d *wakeTrackingDispatcher) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) (*CreateDispatchResult, error) {
	return nil, nil
}

type wakeDispatchMsg struct {
	AgentID   string
	Message   string
	Interrupt bool
}

type wakeDispatchStart struct {
	AgentID  string
	Prompt   string
	Continue bool
}

func (i *wakeDMTestIdentity) ID() string                      { return i.id }
func (i *wakeDMTestIdentity) Type() string                    { return "agent" }
func (i *wakeDMTestIdentity) ProjectID() string               { return i.projectID }
func (i *wakeDMTestIdentity) Scopes() []AgentTokenScope       { return nil }
func (i *wakeDMTestIdentity) HasScope(_ AgentTokenScope) bool { return true }
func (i *wakeDMTestIdentity) Ancestry() []string              { return i.ancestry }
func (i *wakeDMTestIdentity) OriginUserID() string {
	if len(i.ancestry) > 0 {
		return i.ancestry[0]
	}
	return ""
}

// localAncestryProvenance opts this fake into AncestryIsHubAttested: the
// marker is not inherited from Type() == "agent", so test fakes must opt in
// explicitly.
func (i *wakeDMTestIdentity) localAncestryProvenance() ancestryProvenance {
	return ancestryProvenanceAgentJWT
}
func (i *wakeDMTestIdentity) TokenID() string { return "test-token-id" }

// wakeQuotaFixture is a minimal broker+project+target fixture for wake/quota
// interaction tests, built on the shared quota-test helpers rather than
// createWakeDMFixtures so the caller controls the broker ceiling directly.
type wakeQuotaFixture struct {
	srv     *Server
	s       store.Store
	broker  *store.RuntimeBroker
	project *store.Project
	target  *store.Agent
}

func (u *wakeQuotaFixture) count(t *testing.T) int64 {
	return int64(brokerReservationCount(t, u.s, u.broker.ID))
}
