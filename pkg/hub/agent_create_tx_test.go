// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTxFaultStore injects failures into the writes of the agent-create
// transaction and its compensation, including inside WithTx.
type createTxFaultStore struct {
	store.Store
	// auditErrFor fails CreateMutationAudit for records of this mutation
	// type.
	auditErrFor string
	subErr      error
	deactErr    error
}

func (s *createTxFaultStore) wrap(tx store.Store) *createTxFaultStore {
	c := *s
	c.Store = tx
	return &c
}

func (s *createTxFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error { return fn(s.wrap(tx)) })
}

func (s *createTxFaultStore) CreateMutationAudit(ctx context.Context, r *store.MutationAuditRecord) error {
	if s.auditErrFor != "" && r.MutationType == s.auditErrFor {
		return errors.New("injected mutation audit write fault")
	}
	return s.Store.CreateMutationAudit(ctx, r)
}

func (s *createTxFaultStore) CreateNotificationSubscription(ctx context.Context, sub *store.NotificationSubscription) error {
	if s.subErr != nil {
		return s.subErr
	}
	return s.Store.CreateNotificationSubscription(ctx, sub)
}

func (s *createTxFaultStore) DeactivateDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string, d store.Deactivation) (int, error) {
	if s.deactErr != nil {
		return 0, s.deactErr
	}
	return s.Store.DeactivateDelegationEdgesForDelegate(ctx, delegateType, delegateID, d)
}

// agentAudits returns the mutation audit records of type for agentID.
func agentAudits(t *testing.T, s store.Store, mutationType, agentID string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{TargetType: "agent", MutationType: mutationType})
	require.NoError(t, err)
	var out []*store.MutationAuditRecord
	for _, r := range recs {
		if r.TargetID == agentID {
			out = append(out, r)
		}
	}
	return out
}

// compensationSummary is the AfterSummary of an agent_create_dispatch_failed
// record.
type compensationSummary struct {
	OriginalAuditID string `json:"original_audit_id"`
	OpID            string `json:"op_id"`
	Error           string `json:"error"`
}

// An injected failure of the create's audit write rolls back the whole
// create: no agent row, no edge, no subscription.
func TestCreateAuditFailureRollsBack(t *testing.T) {
	f := newUATCreateFixture(t, "audit-rollback")
	real := f.store
	f.srv.store = &createTxFaultStore{Store: real, auditErrFor: mutationTypeAgentDelegation}

	rec := f.create(t, authUser(f.creator), CreateAgentRequest{Name: "audit-rollback", Notify: true})
	assert.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
	assertCreateWroteNothing(t, real, f.proj.ID, "audit-rollback", f.creator.ID)

	// Control: the same create without the fault writes the audit record
	// in the transaction, attributed to the creator.
	f.srv.store = real
	agent, _ := f.createdAgent(t, f.create(t, authUser(f.creator), CreateAgentRequest{Name: "audit-rollback"}), "audit-rollback")
	audits := agentAudits(t, real, mutationTypeAgentDelegation, agent.ID)
	require.Len(t, audits, 1)
	assert.Equal(t, "allow", audits[0].CanDelegateResult)
	assert.Equal(t, f.creator.ID, audits[0].ActorPrincipalID)
}

// An injected failure of the notification subscription write rolls back
// the whole create: no agent row, no edge, no audit record.
func TestCreateSubscriptionFailureRollsBack(t *testing.T) {
	f := newUATCreateFixture(t, "sub-rollback")
	real := f.store
	f.srv.store = &createTxFaultStore{Store: real, subErr: errors.New("injected subscription write fault")}

	rec := f.create(t, authUser(f.creator), CreateAgentRequest{Name: "sub-rollback", Notify: true})
	assert.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
	assertCreateWroteNothing(t, real, f.proj.ID, "sub-rollback", f.creator.ID)

	// Control: without the fault the subscription commits with the agent.
	f.srv.store = real
	agent, _ := f.createdAgent(t, f.create(t, authUser(f.creator), CreateAgentRequest{Name: "sub-rollback", Notify: true}), "sub-rollback")
	subs, err := real.GetNotificationSubscriptionsByProject(context.Background(), f.proj.ID)
	require.NoError(t, err)
	require.Len(t, subs, 1)
	assert.Equal(t, agent.ID, subs[0].AgentID)
	assert.Equal(t, f.creator.ID, subs[0].SubscriberID)
	assert.Len(t, agentAudits(t, real, mutationTypeAgentDelegation, agent.ID), 1)
}

// assertCompensated asserts that agentID was rolled back by compensation:
// no agent row, no active edge, a create_compensation deactivation under
// the op ID the agent_create_dispatch_failed record names, and that record
// referencing the create's own audit record.
func assertCompensated(t *testing.T, s store.Store, agentID string) compensationSummary {
	t.Helper()
	ctx := context.Background()
	_, err := s.GetAgent(ctx, agentID)
	require.ErrorIs(t, err, store.ErrNotFound, "agent row deleted")
	assert.Empty(t, activeEdgesFor(t, s, agentID), "no active edge")

	created := agentAudits(t, s, mutationTypeAgentDelegation, agentID)
	require.Len(t, created, 1, "the create's audit record is kept")
	failed := agentAudits(t, s, mutationTypeAgentCreateDispatchFailed, agentID)
	require.Len(t, failed, 1)
	var sum compensationSummary
	require.NoError(t, json.Unmarshal([]byte(failed[0].AfterSummary), &sum))
	assert.Equal(t, created[0].ID, sum.OriginalAuditID)
	require.NotEmpty(t, sum.OpID)
	assert.NotEmpty(t, failed[0].ActorPrincipalKind)

	// The edge was deactivated with cause create_compensation under that op
	// ID: reactivating exactly that (cause, op ID) finds one edge. Undo it
	// afterwards so the caller sees the compensated state.
	n, err := s.ReactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agentID,
		store.EdgeDeactivationCreateCompensation, sum.OpID)
	require.NoError(t, err)
	require.Equal(t, 1, n, "one edge deactivated with cause create_compensation")
	_, err = s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agentID,
		store.Deactivation{Cause: store.EdgeDeactivationCreateCompensation, OpID: sum.OpID})
	require.NoError(t, err)
	return sum
}

// A dispatch failure after the create committed calls the broker delete,
// deletes the agent, deactivates its edge with cause create_compensation
// and writes agent_create_dispatch_failed referencing the create's audit
// record. The child's minted token yields no further authority.
func TestDispatchFailureCompensates(t *testing.T) {
	f := newChainFixture(t, "chain-dispatch")
	parent, _ := f.sessionParent(t, "chain-dispatch-p")
	f.client.returnErr = errors.New("broker unavailable")
	f.client.deleteCalled = false

	rec := f.createAsParent(t, f.agentToken(t, parent.ID), CreateAgentRequest{Name: "chain-dispatch-c"})
	require.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
	assert.True(t, f.client.deleteCalled, "broker delete called for the dispatched create")
	_, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "chain-dispatch-c")
	require.ErrorIs(t, err, store.ErrNotFound, "no agent row")
	assert.Empty(t, agentDelegatorEdges(t, f.store, parent.ID), "no active edge from the parent")

	require.NotNil(t, f.client.lastCreateReq)
	childID := f.client.lastCreateReq.ID
	require.NotEmpty(t, childID)
	sum := assertCompensated(t, f.store, childID)
	assert.Contains(t, sum.Error, "broker unavailable")

	// The token minted for the failed create gives nothing.
	childToken := f.client.lastCreateReq.AgentToken
	require.NotEmpty(t, childToken)
	f.client.returnErr = nil
	rec = f.createAsParent(t, childToken, CreateAgentRequest{Name: "chain-dispatch-gc"})
	assert.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	_, err = f.store.GetAgentBySlug(context.Background(), f.proj.ID, "chain-dispatch-gc")
	assert.ErrorIs(t, err, store.ErrNotFound, "no grandchild row")

	claims := f.mint().tokenClaims(t, childToken)
	refresh := httptest.NewRecorder()
	f.srv.handleAgentTokenRefresh(refresh, buildAgentRefreshRequest(childID, claims, "", false), childID)
	assert.NotEqual(t, http.StatusOK, refresh.Code, refresh.Body.String())
	_, _, err = f.srv.authzService.sourceEffectCeiling(context.Background(), &agentIdentityWrapper{AgentTokenClaims: claims})
	assert.ErrorIs(t, err, ErrProvenanceChain)
}

// A session create whose dispatch fails is compensated the same way, and
// the response keeps the dispatch failure's own status.
func TestSessionDispatchFailureCompensates(t *testing.T) {
	f := newUATCreateFixture(t, "sess-dispatch")
	client := f.withDispatcher(t)
	client.returnErr = errors.New("broker unavailable")

	rec := f.create(t, authUser(f.creator), CreateAgentRequest{Name: "sess-dispatch", Notify: true})
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	require.NotNil(t, client.lastCreateReq)
	assertCompensated(t, f.store, client.lastCreateReq.ID)
	assert.Empty(t, activeEdgesFor(t, f.store, client.lastCreateReq.ID))
}

// When the compensation transaction fails, the response is a 500 carrying a
// correlation ID, and the agent row is removed on its own as a fallback.
func TestDispatchCompensationFailureReportsCorrelationID(t *testing.T) {
	f := newUATCreateFixture(t, "comp-fail")
	client := f.withDispatcher(t)
	client.returnErr = errors.New("broker unavailable")
	real := f.store
	f.srv.store = &createTxFaultStore{Store: real, deactErr: errors.New("injected deactivation fault")}

	rec := f.create(t, authUser(f.creator), CreateAgentRequest{Name: "comp-fail"})
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeInternalError, body.Error.Code)
	assert.NotEmpty(t, body.Error.Details["correlation_id"], "the 500 body carries a correlation ID")

	require.NotNil(t, client.lastCreateReq)
	agentID := client.lastCreateReq.ID
	_, err := real.GetAgent(context.Background(), agentID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the fallback removes the agent row")
	assert.Empty(t, agentAudits(t, real, mutationTypeAgentCreateDispatchFailed, agentID),
		"the failed compensation wrote no record")
}

// With no dispatcher the agent stays created, with its edge and its audit
// record.
func TestNoDispatcherLeavesCreatedAgent(t *testing.T) {
	f := newUATCreateFixture(t, "no-disp")
	f.srv.SetDispatcher(nil)

	agent, edge := f.createdAgent(t, f.create(t, authUser(f.creator), CreateAgentRequest{Name: "no-disp"}), "no-disp")
	assert.Equal(t, string(state.PhaseCreated), agent.Phase)
	assert.True(t, edge.Active)
	assert.Len(t, agentAudits(t, f.store, mutationTypeAgentDelegation, agent.ID), 1)
	assert.Empty(t, agentAudits(t, f.store, mutationTypeAgentCreateDispatchFailed, agent.ID))
}

// commitAgentCreate refuses an incomplete write before touching the store.
func TestCommitAgentCreateRejectsIncompleteWrite(t *testing.T) {
	f := newUATCreateFixture(t, "incomplete")
	ctx := context.Background()
	prov := store.AuthorityProvenance{ProvenanceVersion: 1}
	full := func() agentCreateWrite {
		return agentCreateWrite{
			Provenance: prov,
			Agent:      &store.Agent{Slug: "incomplete", Name: "incomplete", ProjectID: f.proj.ID},
			Slug:       "incomplete",
			Edge:       &store.DelegationEdge{},
			Audit:      &store.MutationAuditRecord{MutationType: mutationTypeAgentDelegation},
		}
	}
	for name, mutate := range map[string]func(*agentCreateWrite){
		"no agent":        func(w *agentCreateWrite) { w.Agent = nil },
		"no edge":         func(w *agentCreateWrite) { w.Edge = nil },
		"no audit":        func(w *agentCreateWrite) { w.Audit = nil },
		"zero provenance": func(w *agentCreateWrite) { w.Provenance = store.AuthorityProvenance{} },
	} {
		t.Run(name, func(t *testing.T) {
			w := full()
			mutate(&w)
			assert.ErrorIs(t, f.srv.commitAgentCreate(ctx, w), errAgentCreateWriteInvalid)
		})
	}
	_, err := f.store.GetAgentBySlug(ctx, f.proj.ID, "incomplete")
	assert.ErrorIs(t, err, store.ErrNotFound)
}
