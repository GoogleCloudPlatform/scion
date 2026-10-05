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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
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
	// outerDeleteErr fails DeleteAgent outside a transaction only.
	outerDeleteErr error
}

func (s *createTxFaultStore) wrap(tx store.Store) *createTxFaultStore {
	c := *s
	c.Store = tx
	c.outerDeleteErr = nil
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

func (s *createTxFaultStore) DeleteAgent(ctx context.Context, id string) error {
	if s.outerDeleteErr != nil {
		return s.outerDeleteErr
	}
	return s.Store.DeleteAgent(ctx, id)
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
	Stage           string `json:"stage"`
	Error           string `json:"error"`
}

// An injected failure of the create's audit write rolls back the whole
// create: no agent row, no edge, no subscription.
func TestCreateAuditFailureRollsBack(t *testing.T) {
	f := newUATCreateFixture(t, "audit-rollback")
	real := f.store
	f.srv.store = &createTxFaultStore{Store: real, auditErrFor: mutationTypeAgentDelegation}

	rec := f.create(t, authUser(f.creator), CreateAgentRequest{Name: "audit-rollback", Notify: true})
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
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
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
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
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.True(t, f.client.deleteCalled, "broker delete called for the dispatched create")
	_, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "chain-dispatch-c")
	require.ErrorIs(t, err, store.ErrNotFound, "no agent row")
	assert.Empty(t, agentDelegatorEdges(t, f.store, parent.ID), "no active edge from the parent")

	require.NotNil(t, f.client.lastCreateReq)
	childID := f.client.lastCreateReq.ID
	require.NotEmpty(t, childID)
	sum := assertCompensated(t, f.store, childID)
	assert.Contains(t, sum.Error, "broker unavailable")
	assert.Equal(t, createStageDispatch, sum.Stage)

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
	sum := assertCompensated(t, f.store, client.lastCreateReq.ID)
	assert.Equal(t, createStageDispatch, sum.Stage)
	assert.Empty(t, activeEdgesFor(t, f.store, client.lastCreateReq.ID))
	subs, err := f.store.GetNotificationSubscriptionsByProject(context.Background(), f.proj.ID)
	require.NoError(t, err)
	assert.Empty(t, subs, "the compensation removes the notification subscription")
}

// compensationFailureLog is the ERROR record logCompensationFailure writes.
type compensationFailureLog struct {
	Msg           string `json:"msg"`
	AgentID       string `json:"agent_id"`
	CorrelationID string `json:"correlation_id"`
	OpID          string `json:"op_id"`
}

// createWithRequestID issues the fixture's create with request metadata
// carrying requestID, as the request-log middleware installs it, and
// returns the response and the compensation-failure records logged while
// it ran.
func (f *uatCreateFixture) createWithRequestID(t *testing.T, requestID string, req CreateAgentRequest) (*httptest.ResponseRecorder, []compensationFailureLog) {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, f.path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	user := authUser(f.creator)
	ctx := contextWithIdentity(r.Context(), user)
	ctx = context.WithValue(ctx, userContextKey{}, user)
	return serveWithRequestID(t, f.srv.mux, r.WithContext(ctx), requestID)
}

// serveWithRequestID serves r with request metadata carrying requestID, as
// the request-log middleware installs it, and returns the response and the
// compensation-failure records logged while it ran.
func serveWithRequestID(t *testing.T, h http.Handler, r *http.Request, requestID string) (*httptest.ResponseRecorder, []compensationFailureLog) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx := logging.ContextWithRequestMeta(r.Context(), &logging.RequestMeta{RequestID: requestID})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r.WithContext(ctx))
	slog.SetDefault(prev)

	var logs []compensationFailureLog
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		var l compensationFailureLog
		if json.Unmarshal(line, &l) == nil && l.Msg == "agent create compensation failed" {
			logs = append(logs, l)
		}
	}
	return rec, logs
}

// assertCompensationFailureResponse asserts a 500 whose correlation ID is
// requestID and returns the single ERROR record logged for agentID, which
// carries the same correlation ID.
func assertCompensationFailureResponse(t *testing.T, rec *httptest.ResponseRecorder, logs []compensationFailureLog, requestID, agentID string) compensationFailureLog {
	t.Helper()
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeInternalError, body.Error.Code)
	assert.Equal(t, requestID, body.Error.Details["correlation_id"], "the 500 body carries the request ID as its correlation ID")
	require.Len(t, logs, 1, "one ERROR record for the failed compensation")
	assert.Equal(t, agentID, logs[0].AgentID)
	assert.Equal(t, requestID, logs[0].CorrelationID, "the ERROR record carries the same correlation ID")
	require.NotEmpty(t, logs[0].OpID)
	return logs[0]
}

// When the compensation transaction and the fallback edge deactivation both
// fail, the response is a 500 carrying the request ID as its correlation
// ID, and the agent row is removed on its own. The edge stays active with a
// deleted delegate: the documented residual state of this path.
func TestDispatchCompensationFailureReportsCorrelationID(t *testing.T) {
	f := newUATCreateFixture(t, "comp-fail")
	client := f.withDispatcher(t)
	client.returnErr = errors.New("broker unavailable")
	real := f.store
	f.srv.store = &createTxFaultStore{Store: real, deactErr: errors.New("injected deactivation fault")}

	rec, logs := f.createWithRequestID(t, "req-comp-fail", CreateAgentRequest{Name: "comp-fail"})
	require.NotNil(t, client.lastCreateReq)
	agentID := client.lastCreateReq.ID
	assertCompensationFailureResponse(t, rec, logs, "req-comp-fail", agentID)

	_, err := real.GetAgent(context.Background(), agentID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the fallback removes the agent row")
	assert.Empty(t, agentAudits(t, real, mutationTypeAgentCreateDispatchFailed, agentID),
		"the failed compensation wrote no record")
	edges := activeEdgesFor(t, real, agentID)
	require.Len(t, edges, 1, "the edge stays active when its deactivation fails")
	assert.Equal(t, agentID, edges[0].DelegateID)
}

// When the compensation transaction fails on its audit insert, the fallback
// deactivates the edge on its own with cause create_compensation under the
// op ID the ERROR record names, and removes the agent row.
func TestDispatchCompensationAuditFailureDeactivatesEdge(t *testing.T) {
	f := newUATCreateFixture(t, "comp-audit-fail")
	client := f.withDispatcher(t)
	client.returnErr = errors.New("broker unavailable")
	real := f.store
	f.srv.store = &createTxFaultStore{Store: real, auditErrFor: mutationTypeAgentCreateDispatchFailed}

	rec, logs := f.createWithRequestID(t, "req-comp-audit-fail", CreateAgentRequest{Name: "comp-audit-fail"})
	require.NotNil(t, client.lastCreateReq)
	agentID := client.lastCreateReq.ID
	logged := assertCompensationFailureResponse(t, rec, logs, "req-comp-audit-fail", agentID)

	ctx := context.Background()
	_, err := real.GetAgent(ctx, agentID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the fallback removes the agent row")
	assert.Empty(t, agentAudits(t, real, mutationTypeAgentCreateDispatchFailed, agentID))
	assert.Empty(t, activeEdgesFor(t, real, agentID), "the fallback deactivates the edge")
	n, err := real.ReactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agentID,
		store.EdgeDeactivationCreateCompensation, logged.OpID)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "one edge deactivated with cause create_compensation under the logged op ID")
}

// When the compensation transaction and the fallback row delete both fail,
// the agent row keeps its active edge: the fallback deactivates the edge
// only after the row is gone.
func TestDispatchCompensationDeleteFailureKeepsRowAndEdge(t *testing.T) {
	f := newUATCreateFixture(t, "comp-delete-fail")
	client := f.withDispatcher(t)
	client.returnErr = errors.New("broker unavailable")
	real := f.store
	f.srv.store = &createTxFaultStore{
		Store:          real,
		auditErrFor:    mutationTypeAgentCreateDispatchFailed,
		outerDeleteErr: errors.New("injected agent delete fault"),
	}

	rec, logs := f.createWithRequestID(t, "req-comp-delete-fail", CreateAgentRequest{Name: "comp-delete-fail"})
	require.NotNil(t, client.lastCreateReq)
	agentID := client.lastCreateReq.ID
	assertCompensationFailureResponse(t, rec, logs, "req-comp-delete-fail", agentID)

	row, err := real.GetAgent(context.Background(), agentID)
	require.NoError(t, err, "the row survives the failed delete")
	assert.True(t, row.DeletedAt.IsZero())
	assert.Len(t, activeEdgesFor(t, real, agentID), 1, "the surviving row keeps its active edge")
}

// A second compensation of the same create changes nothing and writes no
// second audit record.
func TestCompensateAgentCreateIsIdempotent(t *testing.T) {
	f := newUATCreateFixture(t, "comp-twice")
	f.srv.SetDispatcher(nil)
	agent, _ := f.createdAgent(t, f.create(t, authUser(f.creator), CreateAgentRequest{Name: "comp-twice"}), "comp-twice")
	created := agentAudits(t, f.store, mutationTypeAgentDelegation, agent.ID)
	require.Len(t, created, 1)

	ctx := context.Background()
	c := createCompensation{Agent: agent, OriginalAuditID: created[0].ID, Stage: createStageDispatch, Cause: errors.New("broker unavailable")}
	require.NoError(t, f.srv.compensateAgentCreate(ctx, c))
	require.NoError(t, f.srv.compensateAgentCreate(ctx, c))

	sum := assertCompensated(t, f.store, agent.ID)
	assert.Equal(t, createStageDispatch, sum.Stage)
	assert.Len(t, agentAudits(t, f.store, mutationTypeAgentCreateDispatchFailed, agent.ID), 1,
		"exactly one compensation record")
}

// truncateAuditText never splits a multi-byte character.
func TestTruncateAuditTextKeepsRuneBoundary(t *testing.T) {
	s := "abé世" // a, b, 2-byte é, 3-byte 世: 7 bytes
	require.Len(t, s, 7)
	for max, want := range map[int]string{
		1: "a",
		2: "ab",
		3: "ab",
		4: "abé",
		5: "abé",
		6: "abé",
		7: s,
		9: s,
	} {
		got := truncateAuditText(s, max)
		assert.Equal(t, want, got, "max %d", max)
		assert.True(t, utf8.ValidString(got), "max %d", max)
		assert.LessOrEqual(t, len(got), max, "max %d", max)
	}
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
