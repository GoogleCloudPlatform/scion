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

// Parity test matrix for the request-local authorization input memo. Row
// IDs in test/comment names (P1, A7, X7, ...) are stable identifiers for
// the parity matrix this suite implements; they do not reference a
// specific external document.
//
// Harness rules (H1-H5) implemented below:
//   - H1: faults are selected independently per run, by an n-th-call number
//     or a ctx predicate. A candidate run NEVER reuses a call index computed
//     from the reference run — the memo shifts indices.
//   - H2: every fault/cancel row asserts the fault actually fired where
//     expected, both to prove the row isn't vacuous and to prove the
//     candidate skipped a call the row says it should skip.
//   - H3: any row comparing store-call counts builds a fresh *AuthzService
//     per run (backfillDone latches for the service's lifetime).
//   - H4: two cancellation store variants — (i) wraps ctx.Err(), (ii)
//     ignores cancellation.
//   - H5: a test-local store-call counting wrapper (memoTestStore) stands in
//     for per-request store-call counters, which do not exist yet.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Harness: fault-injecting, call-counting store wrapper (H1, H3, H5)
// =============================================================================

// storeCallRecord captures one intercepted call for X6/X7-style assertions
// that a call did or did not observe an input/edges memo, or run under the
// per-decision delegation ceiling cache.
type storeCallRecord struct {
	method        string
	n             int // 1-based call number for this method on this wrapper
	hasInputMemo  bool
	hasEdgesMemo  bool
	ceilingActive bool
	// stack holds the fully qualified function names on the calling
	// goroutine's stack at the time of the call, innermost first. It is
	// captured only when recordCalls is set.
	stack []string
}

// memoTestStore wraps a real store.Store and intercepts the methods the
// memo serves or that its tests need to fault: the five memoized
// loaders (GetEffectiveGroups[ForAgent], ListRoleBindingsForPrincipals,
// GetRoleDefinitionsByIDs, ListAccessConstraints, GetDelegationEdgesForDelegate)
// plus the ceiling's own direct calls (GetRoleDefinition, GetUser, GetAgent,
// GetHubSetting), which the memo never serves (H1).
//
// fault, when non-nil, is consulted after the call is counted and (if
// recordCalls) recorded, and before delegating to the real store. Returning
// a non-nil error short-circuits the real call, exactly modeling a store
// fault; the wrapper never mutates or fabricates a success.
type memoTestStore struct {
	store.Store

	mu          sync.Mutex
	counts      map[string]int
	records     []storeCallRecord
	recordCalls bool
	fault       func(method string, n int, ctx context.Context) error

	// ignoreCancel implements H4 cancellation store variant (ii): the real
	// delegate call is made on a ctx with cancellation stripped, so the real
	// store answers as if the ctx were live even after the caller's ctx was
	// cancelled. Variant (i) needs no special handling here — a fault func
	// that checks ctx.Err() and returns a wrapped error is sufficient.
	ignoreCancel bool
}

func newMemoTestStore(s store.Store) *memoTestStore {
	return &memoTestStore{Store: s, counts: make(map[string]int)}
}

// delegateCtx returns the ctx to use for the real store call: unchanged for
// H4 variant (i) (and for every non-cancellation row, where ignoreCancel is
// false), or with cancellation stripped for variant (ii).
func (m *memoTestStore) delegateCtx(ctx context.Context) context.Context {
	if m.ignoreCancel {
		return context.WithoutCancel(ctx)
	}
	return ctx
}

// cancelHonouringFault is H4 store variant (i): once the ctx is done, every
// intercepted call returns a wrapped ctx.Err() instead of reaching the real
// store.
func cancelHonouringFault(_ string, _ int, ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("store call after cancel: %w", err)
	}
	return nil
}

func (m *memoTestStore) call(ctx context.Context, method string) error {
	m.mu.Lock()
	m.counts[method]++
	n := m.counts[method]
	if m.recordCalls {
		m.records = append(m.records, storeCallRecord{
			method:        method,
			n:             n,
			hasInputMemo:  authzInputMemoFromContext(ctx) != nil,
			hasEdgesMemo:  delegationEdgesMemoFromContext(ctx) != nil,
			ceilingActive: getDelegationCeilingCache(ctx) != nil,
			stack:         callerFuncNames(),
		})
	}
	fault := m.fault
	m.mu.Unlock()
	if fault != nil {
		return fault(method, n, ctx)
	}
	return nil
}

func (m *memoTestStore) countOf(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[method]
}

// snapshotCounts returns a point-in-time copy of every method's call count,
// for callers that need to diff counts across two points in a test (e.g.
// E6's post-cancellation per-method call-count comparison).
func (m *memoTestStore) snapshotCounts() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.counts))
	for k, v := range m.counts {
		out[k] = v
	}
	return out
}

func (m *memoTestStore) recordedCalls() []storeCallRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]storeCallRecord, len(m.records))
	copy(out, m.records)
	return out
}

func (m *memoTestStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	if err := m.call(ctx, "GetEffectiveGroups"); err != nil {
		return nil, err
	}
	return m.Store.GetEffectiveGroups(m.delegateCtx(ctx), userID)
}

func (m *memoTestStore) GetEffectiveGroupsForAgent(ctx context.Context, agentID string) ([]string, error) {
	if err := m.call(ctx, "GetEffectiveGroupsForAgent"); err != nil {
		return nil, err
	}
	return m.Store.GetEffectiveGroupsForAgent(m.delegateCtx(ctx), agentID)
}

func (m *memoTestStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes, scopeIDs []string) ([]*store.RoleBinding, error) {
	if err := m.call(ctx, "ListRoleBindingsForPrincipals"); err != nil {
		return nil, err
	}
	return m.Store.ListRoleBindingsForPrincipals(m.delegateCtx(ctx), principals, scopeTypes, scopeIDs)
}

func (m *memoTestStore) GetRoleDefinitionsByIDs(ctx context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	if err := m.call(ctx, "GetRoleDefinitionsByIDs"); err != nil {
		return nil, err
	}
	return m.Store.GetRoleDefinitionsByIDs(m.delegateCtx(ctx), ids)
}

func (m *memoTestStore) GetRoleDefinition(ctx context.Context, id string) (*store.RoleDefinition, error) {
	if err := m.call(ctx, "GetRoleDefinition"); err != nil {
		return nil, err
	}
	return m.Store.GetRoleDefinition(m.delegateCtx(ctx), id)
}

func (m *memoTestStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	if err := m.call(ctx, "ListAccessConstraints"); err != nil {
		return nil, err
	}
	return m.Store.ListAccessConstraints(m.delegateCtx(ctx), limit, offset)
}

func (m *memoTestStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	if err := m.call(ctx, "GetDelegationEdgesForDelegate"); err != nil {
		return nil, err
	}
	return m.Store.GetDelegationEdgesForDelegate(m.delegateCtx(ctx), delegateType, delegateID)
}

func (m *memoTestStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if err := m.call(ctx, "GetUser"); err != nil {
		return nil, err
	}
	return m.Store.GetUser(m.delegateCtx(ctx), id)
}

func (m *memoTestStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if err := m.call(ctx, "GetAgent"); err != nil {
		return nil, err
	}
	return m.Store.GetAgent(m.delegateCtx(ctx), id)
}

func (m *memoTestStore) GetHubSetting(ctx context.Context, section string) (*store.HubSetting, error) {
	if err := m.call(ctx, "GetHubSetting"); err != nil {
		return nil, err
	}
	return m.Store.GetHubSetting(m.delegateCtx(ctx), section)
}

// errInjected is the sentinel fault error every row injects. Its identity is
// never asserted on directly — callers compare Error() strings or
// errors.Is(context.Canceled) — but tests use errors.Is against this
// sentinel to confirm the deny actually came from the injected fault and not
// some unrelated error.
var errInjected = errors.New("authz_request_inputs_parity_test: injected store fault")

// callNFault returns a fault func that fails a method's n-th call only.
func callNFault(method string, n int) func(string, int, context.Context) error {
	return func(m string, callN int, _ context.Context) error {
		if m == method && callN == n {
			return fmt.Errorf("injected fault on %s call #%d: %w", m, callN, errInjected)
		}
		return nil
	}
}

// alwaysFault returns a fault func that fails every call to method.
func alwaysFault(method string) func(string, int, context.Context) error {
	return func(m string, _ int, _ context.Context) error {
		if m == method {
			return fmt.Errorf("injected fault on every %s call: %w", m, errInjected)
		}
		return nil
	}
}

// =============================================================================
// Harness: synchronous decision-audit recorder
// =============================================================================

type parityRecordingAuditEmitter struct {
	mu      sync.Mutex
	records []*store.DecisionAuditRecord
}

func (r *parityRecordingAuditEmitter) EmitDecisionAudit(_ context.Context, record *store.DecisionAuditRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *record
	r.records = append(r.records, &cp)
}

func (r *parityRecordingAuditEmitter) snapshot() []*store.DecisionAuditRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*store.DecisionAuditRecord, len(r.records))
	copy(out, r.records)
	return out
}

// newRecordingAuthz builds a fresh AuthzService (H3) over s (typically a
// *memoTestStore) with a synchronous recording emitter and
// DecisionAuditSampleRate = 1.0, so every decision is audited and captured
// in call order.
func newRecordingAuthz(s store.Store) (*AuthzService, *parityRecordingAuditEmitter) {
	authz := NewAuthzService(s, slog.Default())
	emitter := &parityRecordingAuditEmitter{}
	authz.SetDecisionAuditEmitter(emitter)
	authz.DecisionAuditSampleRate = 1.0
	return authz, emitter
}

// assertAuditSequenceEqual compares two audit-record sequences field by
// field apart from Timestamp.
// extraMsg, when provided (a single, ALREADY-formatted string — callers
// that need their own format+args must fmt.Sprintf it themselves before
// calling), is appended to each per-record failure message.
//
// An earlier version took msgAndArgs ...interface{}, type-asserted the
// first element as a format string, and re-Sprintf'd it internally. That
// pattern passed an already-formatted string as testify's format string,
// with the caller's own msgAndArgs (itself a format string plus args) as
// the %-substitution values for it — producing garbled output like
// "audit record 3 mismatch%!(EXTRA string=...)" whenever a caller passed
// its own msgAndArgs. It also made go vet's printf analysis infer this
// function as a Printf-style wrapper over an unverifiable format string,
// flagging every call site that passed its own format+args. A plain
// extraMsg ...string parameter sidesteps both problems.
func assertAuditSequenceEqual(t *testing.T, ref, cand []*store.DecisionAuditRecord, extraMsg ...string) {
	t.Helper()
	if !assert.Equal(t, len(ref), len(cand), "audit record count mismatch") {
		return
	}
	var suffix string
	if len(extraMsg) > 0 && extraMsg[0] != "" {
		suffix = ": " + extraMsg[0]
	}
	for i := range ref {
		r, c := *ref[i], *cand[i]
		r.Timestamp, c.Timestamp = time.Time{}, time.Time{}
		assert.Equal(t, r, c, fmt.Sprintf("audit record %d mismatch%s", i, suffix))
	}
}

// assertDecisionsEqual is reflect.DeepEqual on the whole Decision,
// with a readable failure message.
func assertDecisionsEqual(t *testing.T, ref, cand Decision, msgAndArgs ...interface{}) {
	t.Helper()
	if !reflect.DeepEqual(ref, cand) {
		assert.Fail(t, fmt.Sprintf("decisions differ:\n  reference: %+v\n  candidate: %+v", ref, cand), msgAndArgs...)
	}
}

// =============================================================================
// Fixtures
// =============================================================================

// a1Fixture builds the A1 row's fixture and is reused, unmodified, by every
// A-row and by X7: an agent principal in its own project, with lifecycle
// scopes (AgentRoleFull), and a single-level delegation edge to a
// **non-admin** user delegator who holds an **in-scope active project
// binding** (ProjectRoleAdmin, not a system role) — required so that
// IsSystemAdmin does not short-circuit (match by name, not line number,
// in authz_delegation_ceiling.go) and getEffectivePermissions loads
// constraints, which A7/A7'/X7 all
// depend on.
type a1Fixture struct {
	store       store.Store
	projectID   string
	otherProjID string
	delegatorID string
	agentID     string
	targetID    string // an agent resource in the agent's own project
	agent       AgentIdentity
	resource    Resource
}

// newA1Fixture creates the fixture on s using name as a per-test ID salt, so
// distinct tests sharing one store (rare in this file; most build their own
// via authzTestSetup) do not collide.
func newA1Fixture(t *testing.T, s store.Store, name string) *a1Fixture {
	t.Helper()
	projectID := tid(name + "-project")
	otherProjID := tid(name + "-other-project")
	delegatorID := tid(name + "-delegator")
	agentID := tid(name + "-agent")
	targetID := tid(name + "-target")

	createDCProject(t, s, projectID, name+"-proj")
	createDCProject(t, s, otherProjID, name+"-other-proj")
	// A non-admin delegator with an in-scope ACTIVE project binding.
	createDCUser(t, s, delegatorID, name+"-delegator@test.com", projectID, store.ProjectRoleAdmin)
	createDCAgent(t, s, agentID, projectID, delegatorID, AgentRoleFull)
	createDCAgent(t, s, targetID, projectID, delegatorID, AgentRoleFull)
	createDCEdge(t, s, store.DelegationPrincipalUser, delegatorID, store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	return &a1Fixture{
		store:       s,
		projectID:   projectID,
		otherProjID: otherProjID,
		delegatorID: delegatorID,
		agentID:     agentID,
		targetID:    targetID,
		agent:       dcAgentIdentity(agentID, projectID, AgentRoleFull),
		resource:    Resource{Type: "agent", ID: targetID, ParentType: "project", ParentID: projectID, OwnerID: delegatorID},
	}
}

// p1Fixture is an ordinary hub member reached only through a group binding
// (P1): a "members" group holding a project role binding, with the user a
// member of that group and nothing else. Also serves E1-E5b, whose rows
// pin the principal to "P1, a user, so there is no delegation ceiling."
type p1Fixture struct {
	store     store.Store
	projectID string
	userID    string
	groupID   string
	user      UserIdentity
	agentRes  Resource // an agent resource in the member's project
}

func newP1Fixture(t *testing.T, s store.Store, name string) *p1Fixture {
	t.Helper()
	ctx := context.Background()
	projectID := tid(name + "-project")
	userID := tid(name + "-user")
	ownerID := tid(name + "-owner")
	agentID := tid(name + "-agent")

	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Slug: name + "-proj", Name: name, OwnerID: ownerID}))
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: name + "@test.com", DisplayName: name, Role: "member", Status: "active"}))

	group := &store.Group{ID: tid(name + "-group"), Name: name + " members", Slug: "project:" + projectID + ":members", GroupType: store.GroupTypeExplicit, ProjectID: projectID}
	require.NoError(t, s.CreateGroup(ctx, group))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: group.ID, MemberID: userID, MemberType: store.GroupMemberTypeUser, Role: store.GroupMemberRoleMember}))

	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    "group",
		PrincipalID:      group.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: agentID, Slug: name + "-agent", Name: name + "-agent", ProjectID: projectID, Phase: "running", OwnerID: ownerID, Ancestry: []string{ownerID}}))

	return &p1Fixture{
		store:     s,
		projectID: projectID,
		userID:    userID,
		groupID:   group.ID,
		user:      NewAuthenticatedUser(userID, name+"@test.com", name, "member", "api"),
		agentRes:  Resource{Type: "agent", ID: agentID, ParentType: "project", ParentID: projectID, OwnerID: ownerID},
	}
}

// addToHubMembersGroup adds userID to the seeded "hub-members" group, which
// carries a system-scoped hub-member role binding (template/harness_config
// read+list, among others). This is the ONLY way a plain project-scoped
// principal reaches the global/core catalog the hub-wide filter guards —
// a project-scoped binding alone grants nothing at system scope.
func addToHubMembersGroup(t *testing.T, s store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	hubGroup, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: hubGroup.ID, MemberID: userID, MemberType: store.GroupMemberTypeUser, Role: store.GroupMemberRoleMember}))
}

// projectPrincipalFixture is a project-scoped principal reached through a
// direct (non-group) role binding, for P2 (member), P4 (admin) and similar
// rows that don't need the group-closure shape P1 exercises.
type projectPrincipalFixture struct {
	store     store.Store
	projectID string
	userID    string
	user      UserIdentity
	agentRes  Resource
}

func newProjectPrincipalFixture(t *testing.T, s store.Store, name, roleName string) *projectPrincipalFixture {
	t.Helper()
	ctx := context.Background()
	projectID := tid(name + "-project")
	userID := tid(name + "-user")
	ownerID := tid(name + "-owner")
	agentID := tid(name + "-agent")

	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Slug: name + "-proj", Name: name, OwnerID: ownerID}))
	createDCUser(t, s, userID, name+"@test.com", projectID, roleName)
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: agentID, Slug: name + "-agent", Name: name + "-agent", ProjectID: projectID, Phase: "running", OwnerID: ownerID, Ancestry: []string{ownerID}}))

	return &projectPrincipalFixture{
		store:     s,
		projectID: projectID,
		userID:    userID,
		user:      NewAuthenticatedUser(userID, name+"@test.com", name, "member", "api"),
		agentRes:  Resource{Type: "agent", ID: agentID, ParentType: "project", ParentID: projectID, OwnerID: ownerID},
	}
}

// =============================================================================
// Reference/candidate raw-loop runner
// =============================================================================

// rawTuple is one (resource, action) pair evaluated through CheckAccess.
type rawTuple struct {
	resource Resource
	action   Action
}

// agentResourceTuples builds one tuple per ResourceActions["agent"] against
// res, in registry order.
func agentResourceTuples(res Resource) []rawTuple {
	out := make([]rawTuple, 0, len(ResourceActions["agent"]))
	for _, a := range ResourceActions["agent"] {
		out = append(out, rawTuple{res, a})
	}
	return out
}

// repeatedActionTuples builds n identical (res, action) tuples. Used where a
// row needs every decision in the sequence to ask the SAME allow-or-deny
// question — isolating a pure store-call-count effect (e.g. "the method's
// k-th call fails") from any permission difference between actions, which
// would otherwise confound whether a later decision's result changed
// because of the fault or because that action was never grantable to
// begin with.
func repeatedActionTuples(res Resource, action Action, n int) []rawTuple {
	out := make([]rawTuple, n)
	for i := range out {
		out[i] = rawTuple{res, action}
	}
	return out
}

// runParity is the full parity harness for fault-free rows: it runs the
// reference once (CheckAccess, plain ctx, fresh service), then the
// candidate THREE times on three fresh memo ctxs/services — forward,
// reverse, and shuffled-with-a-logged-seed order — comparing every run
// against the reference keyed by tuple identity (not position), since the
// reference is itself order-independent (it never memoizes). It also runs
// a ComputeCapabilitiesBatch leg whenever the tuples form one or more
// complete "one resource x all ResourceActions[type]" blocks
// (agentResourceTuples' shape), and asserts the audit invariant (exactly one audit
// record per Decide call) on every leg, INCLUDING a full audit-record
// comparison against the reference on every leg — not just the forward
// run. The reverse/shuffled runs' emitted records are in CALL order,
// which differs by construction from the reference's canonical tuple
// order, so they are reordered back to canonical tuple-index order before
// the comparison; see the reorder step inside the loop below.
//
// The returned candDecisions/candStore are the FORWARD run's, for
// callers that need a representative candidate store for count assertions
// (e.g. "groups loaded once") — those counts are identical across all three
// orderings by construction, since the memo does not care about call order,
// only about which input is requested first.
//
// Rows that need fault injection build refStore/candStore themselves (via
// memoTestStore.fault, which is inherently order-sensitive — see H1) and
// call runParityWithStores instead, which runs forward order only.
func runParity(t *testing.T, s store.Store, identity Identity, tuples []rawTuple) (refDecisions, candDecisions []Decision, refStore, candStore *memoTestStore) {
	t.Helper()
	ctx := context.Background()

	refStore = newMemoTestStore(s)
	refAuthz, refEmit := newRecordingAuthz(refStore)
	refDecisions = make([]Decision, len(tuples))
	for i, tp := range tuples {
		refDecisions[i] = refAuthz.CheckAccess(ctx, identity, tp.resource, tp.action)
	}
	require.Len(t, refEmit.snapshot(), len(tuples), "audit invariant: exactly one audit record per decision (reference)")

	n := len(tuples)
	forwardOrder := make([]int, n)
	reverseOrder := make([]int, n)
	for i := 0; i < n; i++ {
		forwardOrder[i] = i
		reverseOrder[i] = n - 1 - i
	}
	seed := time.Now().UnixNano()
	shuffledOrder := append([]int(nil), forwardOrder...)
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(n, func(i, j int) { shuffledOrder[i], shuffledOrder[j] = shuffledOrder[j], shuffledOrder[i] })
	t.Logf("runParity: shuffle seed %d, order %v", seed, shuffledOrder)

	for _, run := range []struct {
		name  string
		order []int
	}{
		{"forward", forwardOrder},
		{"reverse", reverseOrder},
		{"shuffled", shuffledOrder},
	} {
		cStore := newMemoTestStore(s)
		cAuthz, cEmit := newRecordingAuthz(cStore)
		mctx := withAuthzInputMemo(context.Background())
		got := make([]Decision, n)
		for _, idx := range run.order {
			got[idx] = cAuthz.CheckAccess(mctx, identity, tuples[idx].resource, tuples[idx].action)
		}
		require.Len(t, cEmit.snapshot(), n, "audit invariant: exactly one audit record per decision (candidate, %s order)", run.name)
		for i := range refDecisions {
			assertDecisionsEqual(t, refDecisions[i], got[i], "%s order, tuple %d: %s %s", run.name, i, tuples[i].resource.Type, tuples[i].action)
		}
		// Compare the audit records on EVERY run, not just forward. The
		// records themselves are emitted in CALL order
		// (run.order), which legitimately differs across the three legs —
		// that's the whole point of exercising reverse/shuffled order — so
		// a raw positional comparison against the reference (which always
		// runs in canonical tuple order) would be comparing unrelated
		// decisions to each other. Reorder the candidate's emitted records
		// back to canonical tuple-index order first (run.order[pos] is the
		// tuple index the pos'th call was FOR), then compare keyed by tuple
		// identity exactly like the decisions above.
		emitted := cEmit.snapshot()
		byTupleIndex := make([]*store.DecisionAuditRecord, n)
		for pos, idx := range run.order {
			byTupleIndex[idx] = emitted[pos]
		}
		assertAuditSequenceEqual(t, refEmit.snapshot(), byTupleIndex, fmt.Sprintf("%s order, reordered to tuple identity", run.name))
		if run.name == "forward" {
			candDecisions = got
			candStore = cStore
		}
	}

	runBatchLegIfApplicable(t, s, identity, tuples, refDecisions)

	return refDecisions, candDecisions, refStore, candStore
}

// runBatchLegIfApplicable is runParity's batch leg: when tuples form one
// or more complete "one resource x all ResourceActions[resource.Type]"
// blocks, it runs ComputeCapabilitiesBatch over those resources on both a
// plain ctx (reference) and a memo ctx (candidate), asserts the audit invariant
// (exactly one audit record per decision) plus a full audit comparison,
// and asserts the candidate's capability list equals the reference's, and
// that the reference batch result agrees with the raw-loop reference
// decisions already proven above. It is a silent no-op for any other
// tuple shape (scope-action tuples, mixed resource types, a single
// explicit-permission tuple, etc.) — those rows are exercised by the raw
// loop only, which rows needing per-decision attribution use instead.
func runBatchLegIfApplicable(t *testing.T, s store.Store, identity Identity, tuples []rawTuple, refDecisions []Decision) {
	t.Helper()
	if len(tuples) == 0 {
		return
	}
	resourceType := tuples[0].resource.Type
	actions, ok := ResourceActions[resourceType]
	if !ok {
		return
	}
	full := make(map[Action]bool, len(actions))
	for _, a := range actions {
		full[a] = true
	}

	type block struct {
		resource Resource
		allowed  map[Action]bool
		seen     map[Action]bool
	}
	var order []string
	blocks := map[string]*block{}
	for i, tp := range tuples {
		if tp.resource.Type != resourceType || !full[tp.action] {
			return // not a uniform "resource x ResourceActions[type]" shape
		}
		key := tp.resource.Type + ":" + tp.resource.ID + ":" + tp.resource.ParentID + ":" + tp.resource.OwnerID
		b, ok := blocks[key]
		if !ok {
			b = &block{resource: tp.resource, allowed: map[Action]bool{}, seen: map[Action]bool{}}
			blocks[key] = b
			order = append(order, key)
		}
		if b.seen[tp.action] {
			return // a repeated (resource, action) pair: not this shape
		}
		b.seen[tp.action] = true
		b.allowed[tp.action] = refDecisions[i].Allowed
	}
	for _, key := range order {
		if len(blocks[key].seen) != len(actions) {
			return // an incomplete block: not this shape
		}
	}

	resources := make([]Resource, len(order))
	for i, key := range order {
		resources[i] = blocks[key].resource
	}

	ctx := context.Background()
	refStore := newMemoTestStore(s)
	refAuthz, refEmit := newRecordingAuthz(refStore)
	// The batch installs the memo itself, so the reference masks it to
	// keep every decision on its own store loads.
	refCaps := refAuthz.ComputeCapabilitiesBatch(maskAllAuthzMemo(ctx), identity, resources, resourceType)

	candStore := newMemoTestStore(s)
	candAuthz, candEmit := newRecordingAuthz(candStore)
	mctx := withAuthzInputMemo(ctx)
	candCaps := candAuthz.ComputeCapabilitiesBatch(mctx, identity, resources, resourceType)

	// ComputeCapabilitiesBatch evaluates one decision per (resource,
	// action) pair internally, so the expected count is len(resources) *
	// len(actions) (no raw-loop "+1" here, unlike runBatchParity, since
	// this leg does not also call AuthorizeReadBatch).
	wantDecisions := len(resources) * len(actions)
	require.Len(t, refEmit.snapshot(), wantDecisions, "batch leg audit invariant: exactly one audit record per decision (reference)")
	require.Len(t, candEmit.snapshot(), wantDecisions, "batch leg audit invariant: exactly one audit record per decision (candidate)")
	assertAuditSequenceEqual(t, refEmit.snapshot(), candEmit.snapshot(), "batch leg audit records")

	require.Len(t, refCaps, len(resources), "batch leg: ComputeCapabilitiesBatch result length")
	require.Len(t, candCaps, len(resources), "batch leg: ComputeCapabilitiesBatch result length")
	for i, key := range order {
		b := blocks[key]
		var want []string
		for _, a := range actions {
			if b.allowed[a] {
				want = append(want, string(a))
			}
		}
		assert.ElementsMatch(t, want, refCaps[i].Actions, "batch leg: reference ComputeCapabilitiesBatch must agree with the raw-loop reference for resource %s", b.resource.ID)
		assert.Equal(t, refCaps[i].Actions, candCaps[i].Actions, "batch leg: candidate ComputeCapabilitiesBatch for resource %s", b.resource.ID)
	}
}

// runParityWithStores is runParity's single-order primitive: the caller
// supplies (possibly fault-injecting) stores, already wrapping the same
// underlying data, and both runs evaluate tuples forward only, since a
// call-count-based fault (H1) is inherently order-sensitive — reordering
// tuples would change which decision's load lands on the faulted call
// number, corrupting the row's intended semantics. Fault-free callers
// should use runParity instead, which also runs reverse and shuffled
// orders and a batch leg. The two AuthzServices built here are always
// fresh (H3).
func runParityWithStores(t *testing.T, refStore, candStore *memoTestStore, identity Identity, tuples []rawTuple) (refDecisions, candDecisions []Decision, retRefStore, retCandStore *memoTestStore) {
	t.Helper()
	ctx := context.Background()

	refAuthz, refEmit := newRecordingAuthz(refStore)
	for _, tp := range tuples {
		refDecisions = append(refDecisions, refAuthz.CheckAccess(ctx, identity, tp.resource, tp.action))
	}
	require.Len(t, refEmit.snapshot(), len(tuples), "audit invariant: exactly one audit record per decision (reference)")

	candAuthz, candEmit := newRecordingAuthz(candStore)
	mctx := withAuthzInputMemo(ctx)
	for _, tp := range tuples {
		candDecisions = append(candDecisions, candAuthz.CheckAccess(mctx, identity, tp.resource, tp.action))
	}
	require.Len(t, candEmit.snapshot(), len(tuples), "audit invariant: exactly one audit record per decision (candidate)")

	require.Equal(t, len(refDecisions), len(candDecisions))
	for i := range refDecisions {
		assertDecisionsEqual(t, refDecisions[i], candDecisions[i], "tuple %d: %s %s", i, tuples[i].resource.Type, tuples[i].action)
	}
	assertAuditSequenceEqual(t, refEmit.snapshot(), candEmit.snapshot())
	return refDecisions, candDecisions, refStore, candStore
}

// TestParity_Gate6_ExactlyOneAuditRecordPerDecision pins the audit invariant directly: N
// decisions on ONE memo ctx produce exactly N audit records, in the same
// order as the decisions, each naming the (ResourceID, Permission) pair it
// was for — proving the memo does not
// collapse, skip, or duplicate any audit emission even when every memoized
// slot is served from a shared entry across many decisions.
func TestParity_Gate6_ExactlyOneAuditRecordPerDecision(t *testing.T) {
	t.Run("user_principal", func(t *testing.T) {
		// P1 exercises closure, bindings, role defs and constraints (the
		// group binding resolves to a real role definition).
		_, s := authzTestSetup(t)
		f := newP1Fixture(t, s, "gate6-user")
		tuples := agentResourceTuples(f.agentRes)

		mstore := newMemoTestStore(s)
		authz, emit := newRecordingAuthz(mstore)
		mctx := withAuthzInputMemo(context.Background())
		for _, tp := range tuples {
			authz.CheckAccess(mctx, f.user, tp.resource, tp.action)
		}

		records := emit.snapshot()
		require.Len(t, records, len(tuples), "audit invariant: exactly one audit record per decision")
		for i, tp := range tuples {
			assert.Equal(t, tp.resource.ID, records[i].ResourceID, "record %d resource ID (in-order)", i)
			assert.Equal(t, string(tp.action), records[i].Permission, "record %d action (in-order)", i)
		}
		assert.GreaterOrEqual(t, mstore.countOf("GetEffectiveGroups"), 1, "H2: closure slot exercised")
		assert.GreaterOrEqual(t, mstore.countOf("ListRoleBindingsForPrincipals"), 1, "H2: bindings slot exercised")
		assert.GreaterOrEqual(t, mstore.countOf("GetRoleDefinitionsByIDs"), 1, "H2: role-defs slot exercised")
		assert.GreaterOrEqual(t, mstore.countOf("ListAccessConstraints"), 1, "H2: constraints slot exercised")
	})

	t.Run("agent_principal_with_edge", func(t *testing.T) {
		// A1 additionally exercises the edges slot (the fifth memoized
		// input), which no user-principal fixture ever reaches.
		_, s := authzTestSetup(t)
		f := newA1Fixture(t, s, "gate6-agent")
		tuples := agentGrantableTuples(f)

		mstore := newMemoTestStore(s)
		authz, emit := newRecordingAuthz(mstore)
		mctx := withAuthzInputMemo(context.Background())
		for _, tp := range tuples {
			authz.CheckAccess(mctx, f.agent, tp.resource, tp.action)
		}

		records := emit.snapshot()
		require.Len(t, records, len(tuples), "audit invariant: exactly one audit record per decision")
		for i, tp := range tuples {
			assert.Equal(t, tp.resource.ID, records[i].ResourceID, "record %d resource ID (in-order)", i)
			assert.Equal(t, string(tp.action), records[i].Permission, "record %d action (in-order)", i)
		}
		assert.GreaterOrEqual(t, mstore.countOf("GetEffectiveGroupsForAgent"), 1, "H2: closure slot exercised")
		assert.GreaterOrEqual(t, mstore.countOf("ListAccessConstraints"), 1, "H2: constraints slot exercised")
		assert.GreaterOrEqual(t, mstore.countOf("GetDelegationEdgesForDelegate"), 1, "H2: edges slot exercised")
	})
}

// =============================================================================
// Store-call count tests
// =============================================================================

// TestParity_StoreCallCounts_ComputeCapabilitiesBatch pins the memo's
// headline claim: under the memo, a batch of N agents for one
// user principal costs 1 GetEffectiveGroups, 1 ListRoleBindingsForPrincipals,
// <=1 GetRoleDefinitionsByIDs and P ListAccessConstraints pages, versus 8N
// (8N*P for constraints) without it. K = len(ResourceActions["agent"]) = 8.
func TestParity_StoreCallCounts_ComputeCapabilitiesBatch(t *testing.T) {
	for _, n := range []int{1, 25, 100} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			_, s := authzTestSetup(t)
			name := fmt.Sprintf("cnt-%d", n)
			f := newP1Fixture(t, s, name)
			ctx := context.Background()

			resources := make([]Resource, n)
			for i := 0; i < n; i++ {
				agentID := tid(fmt.Sprintf("%s-agent-%d", name, i))
				require.NoError(t, s.CreateAgent(ctx, &store.Agent{
					ID: agentID, Slug: fmt.Sprintf("%s-agent-%d", name, i), Name: fmt.Sprintf("%s-agent-%d", name, i),
					ProjectID: f.projectID, Phase: "running", OwnerID: f.userID, Ancestry: []string{f.userID},
				}))
				resources[i] = Resource{Type: "agent", ID: agentID, ParentType: "project", ParentID: f.projectID, OwnerID: f.userID}
			}

			// H3: a fresh service for each of the two runs.
			memoStore := newMemoTestStore(s)
			memoAuthz, _ := newRecordingAuthz(memoStore)
			memoAuthz.ComputeCapabilitiesBatch(withAuthzInputMemo(ctx), f.user, resources, "agent")
			assert.Equal(t, 1, memoStore.countOf("GetEffectiveGroups"), "GetEffectiveGroups under the memo")
			assert.Equal(t, 1, memoStore.countOf("ListRoleBindingsForPrincipals"), "ListRoleBindingsForPrincipals under the memo")
			assert.Equal(t, 1, memoStore.countOf("GetRoleDefinitionsByIDs"), "GetRoleDefinitionsByIDs under the memo: the fixture has bindings, so this must be loaded exactly once, not skipped")
			pages := memoStore.countOf("ListAccessConstraints")
			// The fixture has far fewer than 500 constraints (the
			// ListAccessConstraints page size), so there is exactly one
			// page, not merely "at least one" (C3 separately pins pages=2
			// for a 501-row fixture).
			assert.Equal(t, 1, pages, "exactly one constraint page for a fixture with far fewer than 500 rows")

			// With no outer memo the batch installs its own, at the same cost.
			selfStore := newMemoTestStore(s)
			selfAuthz, _ := newRecordingAuthz(selfStore)
			selfAuthz.ComputeCapabilitiesBatch(ctx, f.user, resources, "agent")
			for _, m := range []string{"GetEffectiveGroups", "ListRoleBindingsForPrincipals", "GetRoleDefinitionsByIDs", "ListAccessConstraints"} {
				assert.Equal(t, memoStore.countOf(m), selfStore.countOf(m), "%s with the batch's own memo", m)
			}

			// A masked memo gives the per-decision cost of evaluating with no memo.
			plainStore := newMemoTestStore(s)
			plainAuthz, _ := newRecordingAuthz(plainStore)
			plainAuthz.ComputeCapabilitiesBatch(maskAllAuthzMemo(ctx), f.user, resources, "agent")
			k := len(ResourceActions["agent"])
			require.Equal(t, 8, k, "test assumes 8 agent resource actions; ResourceActions[\"agent\"] changed")
			// Without the memo, each action the kernel denies also reaches the
			// owner/ancestor candidates, whose project-access check
			// (ptone/scion#2141) loads the closure and bindings once more per
			// decision. The project-member role grants one of the eight agent
			// actions, so seven decisions per agent make that check. Under
			// the memo (asserted above) the check reads the memo and adds
			// nothing.
			const relationshipChecks = 7
			assert.Equal(t, (k+relationshipChecks)*n, plainStore.countOf("GetEffectiveGroups"), "GetEffectiveGroups without the memo")
			assert.Equal(t, (k+relationshipChecks)*n, plainStore.countOf("ListRoleBindingsForPrincipals"), "ListRoleBindingsForPrincipals without the memo")
			assert.Equal(t, k*n, plainStore.countOf("GetRoleDefinitionsByIDs"), "GetRoleDefinitionsByIDs without the memo")
			assert.Equal(t, k*n*pages, plainStore.countOf("ListAccessConstraints"), "ListAccessConstraints without the memo")
		})
	}
}

// =============================================================================
// P rows: same-decision parity across principal classes
// =============================================================================

// TestParity_P1_OrdinaryMemberViaGroup is row P1: deep-equal;
// GetEffectiveGroups loaded once under the memo.
func TestParity_P1_OrdinaryMemberViaGroup(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "p1")
	_, _, _, candStore := runParity(t, s, f.user, agentResourceTuples(f.agentRes))
	assert.Equal(t, 1, candStore.countOf("GetEffectiveGroups"), "groups loaded once under the memo")
}

// TestParity_P4_ProjectAdmin is row P4: deep-equal; every action is
// decided. "Decided" means every tuple reaches a real decision (not a
// zero-load short-circuit like D2) — not that a project admin is allowed on
// every one: some agent actions (e.g. grant_hub_mode) have additional
// non-role-binding gates that a project-admin role binding alone does not
// satisfy. H2 non-vacuity: at least one tuple must be allowed and at least
// one denied, so the row isn't degenerate in either direction.
func TestParity_P4_ProjectAdmin(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newProjectPrincipalFixture(t, s, "p4", store.ProjectRoleAdmin)
	// P4 injects no fault, so it should use runParity (which also
	// exercises the reverse, shuffled and batch legs), not
	// runParityWithStores (forward-order only, reserved for fault-injecting
	// rows where H1 order-sensitivity matters).
	refDecisions, _, refStore, _ := runParity(t, s, f.user, agentResourceTuples(f.agentRes))
	var sawAllow, sawDeny bool
	for _, d := range refDecisions {
		if d.Allowed {
			sawAllow = true
		} else {
			sawDeny = true
		}
	}
	assert.True(t, sawAllow, "H2: at least one action must be allowed for a project admin")
	assert.True(t, sawDeny, "H2: at least one action must still be gated by something beyond the role binding")
	assert.Equal(t, len(ResourceActions["agent"]), refStore.countOf("GetEffectiveGroups"), "sanity: every tuple reached step 2 (no zero-load short-circuit)")
}

// TestParity_U1_ScopedUATAgentReadOnly is row U1: a UAT scoped to
// project X with only the agent:read selector, built via
// NewScopedUserIdentityWithCeiling / BuildCeilingFromSelectors (a zero
// ceiling denies everything, so U1-U3 must carry a real one). Must show
// deep-equal gate reasons and the 7a ceiling restriction.
func TestParity_U1_ScopedUATAgentReadOnly(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "u1")
	ceiling, ok := permissions.BuildCeilingFromSelectors([]string{"agent:read"})
	require.True(t, ok, "agent:read must resolve to a valid ceiling selector")
	// Step 1 (enforceUATConstraints) checks the identity's legacy scopes
	// list independently of the ceiling step 7a checks, so both must name
	// agent:read for the request to reach the kernel at all.
	scoped := NewScopedUserIdentityWithCeiling(f.user, f.projectID, []string{"agent:read"}, "u1-uat", ceiling)

	refDecisions, _, _, _ := runParity(t, s, scoped, agentResourceTuples(f.agentRes))
	// Non-vacuity: agent.read must be allowed and every other action denied
	// by the ceiling restriction, so the row actually exercises both 7a
	// branches, not just one.
	var sawAllow, sawCeilingDeny bool
	for i, d := range refDecisions {
		if ResourceActions["agent"][i] == ActionRead {
			assert.True(t, d.Allowed, "agent:read must be allowed under its own ceiling selector: reason=%q", d.Reason)
			sawAllow = true
		} else if !d.Allowed {
			sawCeilingDeny = true
		}
	}
	assert.True(t, sawAllow, "fixture must reach an allow (H2 non-vacuity)")
	assert.True(t, sawCeilingDeny, "fixture must reach a ceiling-restricted deny (H2 non-vacuity)")
}

// =============================================================================
// D rows: zero-load and pre-load denies
// =============================================================================

// TestParity_D2_UnresolvablePermission is row D2: a bogus action that
// resolves to no registered permission must deny with zero input loads,
// before step 0/1, in both paths.
func TestParity_D2_UnresolvablePermission(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "d2")
	tuples := []rawTuple{{f.agentRes, Action("bogus-action-not-registered")}}
	_, _, refStore, candStore := runParity(t, s, f.user, tuples)
	for _, method := range []string{"GetEffectiveGroups", "ListRoleBindingsForPrincipals", "GetRoleDefinitionsByIDs", "ListAccessConstraints"} {
		assert.Equal(t, 0, refStore.countOf(method), "reference: zero %s loads for an unresolvable permission", method)
		assert.Equal(t, 0, candStore.countOf(method), "candidate: zero %s loads for an unresolvable permission", method)
	}
}

// TestParity_D3_DeliveryGateZeroLoads is row D3: a deliver request is
// denied at step 0 before any input load, for every credential kind,
// because deliveryCredentialKinds is currently empty.
func TestParity_D3_DeliveryGateZeroLoads(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "d3")
	tuples := []rawTuple{{Resource{Type: "secret", ID: tid("d3-secret")}, ActionDeliver}}
	refDecisions, candDecisions, refStore, candStore := runParity(t, s, f.user, tuples)
	for _, d := range append(refDecisions, candDecisions...) {
		assert.False(t, d.Allowed)
		assert.Equal(t, "deliver permissions require a delivery credential", d.Reason)
	}
	for _, method := range []string{"GetEffectiveGroups", "ListRoleBindingsForPrincipals", "GetRoleDefinitionsByIDs", "ListAccessConstraints"} {
		assert.Equal(t, 0, refStore.countOf(method), "reference: zero %s loads for a deliver request", method)
		assert.Equal(t, 0, candStore.countOf(method), "candidate: zero %s loads for a deliver request", method)
	}
}

// =============================================================================
// E rows: deterministic and first-attempt-transient faults (P1 principal)
// =============================================================================

// TestParity_E1_GetEffectiveGroupsAlwaysFails is row E1: deep-equal
// denies; the call count equals the number of decisions reaching step 2, in
// both runs (H3; every decision reaches step 2 here).
func TestParity_E1_GetEffectiveGroupsAlwaysFails(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "e1")
	tuples := agentResourceTuples(f.agentRes)

	refStore := newMemoTestStore(s)
	refStore.fault = alwaysFault("GetEffectiveGroups")
	candStore := newMemoTestStore(s)
	candStore.fault = alwaysFault("GetEffectiveGroups")

	refDecisions, _, _, _ := runParityWithStores(t, refStore, candStore, f.user, tuples)
	for i, d := range refDecisions {
		assert.False(t, d.Allowed, "tuple %d must deny", i)
		assert.Equal(t, "principal resolution error (fail-closed)", d.Reason)
	}
	assert.Equal(t, len(tuples), refStore.countOf("GetEffectiveGroups"), "reference: one call per decision, never memoized")
	assert.Equal(t, len(tuples), candStore.countOf("GetEffectiveGroups"), "candidate: an always-failing load is never stored, so every decision retries")
}

// TestParity_E2_ListRoleBindingsAlwaysFails is row E2: same shape as
// E1, one level deeper (principal resolution succeeds; bindings never do).
func TestParity_E2_ListRoleBindingsAlwaysFails(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "e2")
	tuples := agentResourceTuples(f.agentRes)

	refStore := newMemoTestStore(s)
	refStore.fault = alwaysFault("ListRoleBindingsForPrincipals")
	candStore := newMemoTestStore(s)
	candStore.fault = alwaysFault("ListRoleBindingsForPrincipals")

	refDecisions, _, _, _ := runParityWithStores(t, refStore, candStore, f.user, tuples)
	for i, d := range refDecisions {
		assert.False(t, d.Allowed, "tuple %d must deny", i)
		assert.Equal(t, "binding resolution error (fail-closed)", d.Reason)
	}
	assert.Equal(t, len(tuples), refStore.countOf("ListRoleBindingsForPrincipals"))
	assert.Equal(t, len(tuples), candStore.countOf("ListRoleBindingsForPrincipals"))
}

// TestParity_E3_GetRoleDefinitionsAlwaysFails is row E3: P1 has
// bindings (so this loader is actually reached), and it always fails.
func TestParity_E3_GetRoleDefinitionsAlwaysFails(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "e3")
	tuples := agentResourceTuples(f.agentRes)

	refStore := newMemoTestStore(s)
	refStore.fault = alwaysFault("GetRoleDefinitionsByIDs")
	candStore := newMemoTestStore(s)
	candStore.fault = alwaysFault("GetRoleDefinitionsByIDs")

	refDecisions, _, _, _ := runParityWithStores(t, refStore, candStore, f.user, tuples)
	for i, d := range refDecisions {
		assert.False(t, d.Allowed, "tuple %d must deny", i)
		assert.Equal(t, "role resolution error (fail-closed)", d.Reason)
	}
	assert.Equal(t, len(tuples), refStore.countOf("GetRoleDefinitionsByIDs"))
	assert.Equal(t, len(tuples), candStore.countOf("GetRoleDefinitionsByIDs"))
}

// c3Fixture builds 501 unrelated, inapplicable access-constraint rows so
// that loadAllAccessConstraints must page twice (limit 500): offset 0 then
// offset 500. The rows target a principal/scope that never matches the P1
// fixture's closure, so they never change a decision's outcome — only the
// page count.
func c3Fixture(t *testing.T, s store.Store, name string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 501; i++ {
		_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
			Name:               fmt.Sprintf("%s-constraint-%d", name, i),
			SubjectKind:        store.ConstraintSubjectAllPrincipals,
			ScopeType:          ScopeTypeSystem,
			MaximumPermissions: []string{"agent.read", "agent.list", "agent.create", "agent.delete", "agent.attach", "agent.lifecycle", "agent.update", "agent.port_access", "agent.set_message_mode", "agent.stop_all", "agent.message", "project.read"},
			CreatedBy:          "test",
		})
		require.NoError(t, err)
	}
}

// TestParity_C3_PagedConstraints is row C3: P = 2 pages, loaded once
// under the memo; deep-equal.
func TestParity_C3_PagedConstraints(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "c3")
	c3Fixture(t, s, "c3")
	tuples := agentResourceTuples(f.agentRes)
	_, _, refStore, candStore := runParity(t, s, f.user, tuples)
	assert.Equal(t, 2*len(tuples), refStore.countOf("ListAccessConstraints"), "reference: 2 pages per decision, unmemoized")
	assert.Equal(t, 2, candStore.countOf("ListAccessConstraints"), "candidate: 2 pages, loaded once under the memo")
}

// offsetFaultStore wraps memoTestStore to fail ListAccessConstraints by its
// offset argument rather than by call number, because the generic
// call-number fault (H1's first form) cannot see method arguments.
type offsetFaultStore struct {
	*memoTestStore
	failOffset int
	fired      bool
	mu         sync.Mutex
}

func newOffsetFaultStore(s store.Store, failOffset int) *offsetFaultStore {
	return &offsetFaultStore{memoTestStore: newMemoTestStore(s), failOffset: failOffset}
}

func (o *offsetFaultStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	o.memoTestStore.mu.Lock()
	o.counts["ListAccessConstraints"]++
	o.memoTestStore.mu.Unlock()
	if offset == o.failOffset {
		o.mu.Lock()
		o.fired = true
		o.mu.Unlock()
		return nil, fmt.Errorf("injected offset-%d fault: %w", offset, errInjected)
	}
	return o.Store.ListAccessConstraints(ctx, limit, offset)
}

func (o *offsetFaultStore) hasFired() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.fired
}

// TestParity_E4_ListAccessConstraintsFailsByOffset is row E4: P1 with
// the C3 fixture; ListAccessConstraints fails (i) whenever offset == 0, (ii)
// whenever offset == 500. Both variants must deep-equal deny-all.
func TestParity_E4_ListAccessConstraintsFailsByOffset(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset int
	}{
		{"offset0", 0},
		{"offset500", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s := authzTestSetup(t)
			f := newP1Fixture(t, s, "e4-"+tc.name)
			c3Fixture(t, s, "e4-"+tc.name)
			tuples := agentResourceTuples(f.agentRes)

			refStore := newOffsetFaultStore(s, tc.offset)
			candStore := newOffsetFaultStore(s, tc.offset)

			ctx := context.Background()
			refAuthz, refEmit := newRecordingAuthz(refStore)
			var refDecisions []Decision
			for _, tp := range tuples {
				refDecisions = append(refDecisions, refAuthz.CheckAccess(ctx, f.user, tp.resource, tp.action))
			}

			candAuthz, candEmit := newRecordingAuthz(candStore)
			mctx := withAuthzInputMemo(ctx)
			var candDecisions []Decision
			for _, tp := range tuples {
				candDecisions = append(candDecisions, candAuthz.CheckAccess(mctx, f.user, tp.resource, tp.action))
			}

			for i := range refDecisions {
				assertDecisionsEqual(t, refDecisions[i], candDecisions[i], "tuple %d", i)
				assert.False(t, refDecisions[i].Allowed, "tuple %d must deny-all", i)
			}
			assertAuditSequenceEqual(t, refEmit.snapshot(), candEmit.snapshot())
			assert.True(t, refStore.hasFired(), "H2: the offset fault must actually fire in the reference")
			assert.True(t, candStore.hasFired(), "H2: the offset fault must actually fire in the candidate too (deny-all every decision, so no memoization saves it)")
		})
	}
}

// TestParity_E5_EachLoaderFailsOnFirstCallOnly is row E5: each served
// loader fails on its call #1 only, in each run independently. Decision 1
// denies in both paths; the error is never memoized, so decision 2 retries
// and the rest match.
func TestParity_E5_EachLoaderFailsOnFirstCallOnly(t *testing.T) {
	for _, method := range []string{"GetEffectiveGroups", "ListRoleBindingsForPrincipals", "GetRoleDefinitionsByIDs", "ListAccessConstraints"} {
		t.Run(method, func(t *testing.T) {
			_, s := authzTestSetup(t)
			f := newP1Fixture(t, s, "e5-"+method)
			tuples := agentResourceTuples(f.agentRes)
			require.GreaterOrEqual(t, len(tuples), 2, "need at least 2 decisions to see the retry")

			refStore := newMemoTestStore(s)
			refStore.fault = callNFault(method, 1)
			candStore := newMemoTestStore(s)
			candStore.fault = callNFault(method, 1)

			refDecisions, _, refS, candS := runParityWithStores(t, refStore, candStore, f.user, tuples)
			assert.False(t, refDecisions[0].Allowed, "decision 1 must deny in both runs")
			assert.GreaterOrEqual(t, refS.countOf(method), 1, "H2: the fault must fire in the reference")
			assert.GreaterOrEqual(t, candS.countOf(method), 1, "H2: the fault must fire in the candidate too (it's decision 1, nothing is memoized yet)")

			// Pin the exact "fails on first call only, not memoized" shape:
			// call #1 fails, call #2 (decision 2's retry) succeeds and is
			// memoized, so the method is never called again for decisions
			// 3-8 — exactly 2 calls total in the candidate.
			assert.Equal(t, 2, candS.countOf(method), "the candidate must call %s exactly twice: the failed call #1 and the successful, memoized call #2", method)
			if reason, ok := map[string]string{
				"GetEffectiveGroups":            "principal resolution error (fail-closed)",
				"ListRoleBindingsForPrincipals": "binding resolution error (fail-closed)",
				"GetRoleDefinitionsByIDs":       "role resolution error (fail-closed)",
			}[method]; ok {
				assert.Equal(t, reason, refDecisions[0].Reason, "decision 1's exact deny reason")
			}
		})
	}
}

// e5bExpectedFaultReason maps each E5b-served loader to the EXACT Reason its
// fail-closed deny produces (authz.go steps 2/3/4's resolution-error denies,
// and step 7c's access-constraint-load deny-all restriction, whose Kind
// "access_constraint_error" has a nil Check and so always gets the generic
// "restriction has no check function (fail closed)" detail from the kernel —
// see TestParity_A7Prime_FirstAttemptTransientPlusDeterministicFault's
// assertion for the same exact string). Pinning this (rather than just
// asserting !Allowed) is what proves the REFERENCE decision actually took
// the faulted method's error path, not some unrelated deny.
var e5bExpectedFaultReason = map[string]string{
	"GetEffectiveGroups":            "principal resolution error (fail-closed)",
	"ListRoleBindingsForPrincipals": "binding resolution error (fail-closed)",
	"GetRoleDefinitionsByIDs":       "role resolution error (fail-closed)",
	"ListAccessConstraints":         "restriction removed permission: access_constraint_error - restriction has no check function (fail closed)",
}

// TestParity_E5b_DocumentedDivergenceOnLaterTransient is row E5b: for
// each served loader and k in {2, 5}, a fault on that method's call #k
// denies decision k in the reference but is never observed by the candidate
// (the memo already holds a success after decision 1). The candidate's
// every decision equals a no-fault baseline; the reference equals the
// baseline everywhere except decision k.
//
// The tuples are n identical ActionRead decisions against the SAME
// resource, not agentResourceTuples' mixed read/update/delete/attach/
// lifecycle set. P1's project-member role only grants agent.read — update,
// delete, attach and lifecycle are ALREADY denied with no fault at all, so
// a fault landing on decision index 1 (k=2) or 4 (k=5) of the mixed set
// changes nothing observable: the reference was already a deny for an
// unrelated reason (no active binding grants permission), and
// ListAccessConstraints's deny-all restriction can't be distinguished from
// that pre-existing deny by Reason OR by Allowed. Repeating the one action
// that IS allowed at baseline (read) isolates the fault's effect: decision
// k-1 flips from a genuine allow to a genuine, specifically-reasoned
// fail-closed deny, which is the only way this row can prove the fault was
// observed (and not simply irrelevant) in the reference.
//
// The production install sites (the capability functions,
// AuthorizeReadBatch and the agent list handlers) put every decision of a
// call or list request under one memo, so this divergence is reachable in
// production, for read decisions as well as writes: a load that fails after
// an earlier success in the same request no longer denies the later
// decision. That is accepted. The memo reuses only a value the same request
// already loaded successfully, a failure before the first success is never
// cached and still fails closed, a done ctx bypasses the memo, and lookups
// made for other principals (relationship candidates, the delegation
// ceiling's delegators) never observe it.
func TestParity_E5b_DocumentedDivergenceOnLaterTransient(t *testing.T) {
	for _, method := range []string{"GetEffectiveGroups", "ListRoleBindingsForPrincipals", "GetRoleDefinitionsByIDs", "ListAccessConstraints"} {
		for _, k := range []int{2, 5} {
			t.Run(fmt.Sprintf("%s/k=%d", method, k), func(t *testing.T) {
				_, s := authzTestSetup(t)
				f := newP1Fixture(t, s, fmt.Sprintf("e5b-%s-%d", method, k))
				tuples := repeatedActionTuples(f.agentRes, ActionRead, 6)
				require.GreaterOrEqual(t, len(tuples), k)
				ctx := context.Background()

				baselineStore := newMemoTestStore(s)
				baselineAuthz, _ := newRecordingAuthz(baselineStore)
				mctxBase := withAuthzInputMemo(ctx)
				var baseline []Decision
				for _, tp := range tuples {
					baseline = append(baseline, baselineAuthz.CheckAccess(mctxBase, f.user, tp.resource, tp.action))
				}
				// H2 precondition: decision k-1 must be a genuine allow
				// with no fault at all, or a fault landing on it cannot be
				// shown to have CAUSED a deny — it could just as easily be a
				// pre-existing, unrelated deny that the fault never touches.
				require.True(t, baseline[k-1].Allowed,
					"H2 precondition: baseline decision %d (the one call #%d's fault lands on) must be an allow", k, k)

				refStore := newMemoTestStore(s)
				refStore.fault = callNFault(method, k)
				refAuthz, _ := newRecordingAuthz(refStore)
				var ref []Decision
				for _, tp := range tuples {
					ref = append(ref, refAuthz.CheckAccess(ctx, f.user, tp.resource, tp.action))
				}

				candStore := newMemoTestStore(s)
				candStore.fault = callNFault(method, k)
				candAuthz, _ := newRecordingAuthz(candStore)
				mctx := withAuthzInputMemo(ctx)
				var cand []Decision
				for _, tp := range tuples {
					cand = append(cand, candAuthz.CheckAccess(mctx, f.user, tp.resource, tp.action))
				}

				for i := range cand {
					assertDecisionsEqual(t, baseline[i], cand[i], "candidate tuple %d vs no-fault baseline", i)
				}
				assert.Equal(t, 1, candStore.countOf(method), "candidate: %s loaded once; call #%d never happens under the memo", method, k)

				for i := range ref {
					if i == k-1 {
						assert.False(t, ref[i].Allowed, "reference decision %d (call #%d) must deny", i+1, k)
						// Pin the EXACT Reason, not just !Allowed — this is
						// what proves the fault (not some unrelated cause)
						// produced the deny, since baseline[k-1] was just
						// proven to be an allow above.
						assert.Equal(t, e5bExpectedFaultReason[method], ref[i].Reason,
							"reference decision %d's exact fail-closed reason for the %s fault", i+1, method)
						continue
					}
					assertDecisionsEqual(t, baseline[i], ref[i], "reference tuple %d vs no-fault baseline", i)
				}
				assert.GreaterOrEqual(t, refStore.countOf(method), k, "H2: call #%d of %s must actually occur in the reference", k, method)
			})
		}
	}
}

// =============================================================================
// E6/E6b: cancellation (H4)
// =============================================================================

// TestParity_E6_CancellationBothVariants is row E6: P1 and A1 (raw
// loop), the ctx is cancelled between tuple j and j+1, under both H4 store
// variants. Every decision after cancellation must deep-equal the reference,
// including Reason, because the memo bypasses entirely on a done ctx and
// issues today's store call. AuthorizeReadBatch, called with an
// already-cancelled ctx, must return ctx.Err() in both runs.
func TestParity_E6_CancellationBothVariants(t *testing.T) {
	for _, honour := range []bool{true, false} {
		variant := "ignores-cancellation"
		if honour {
			variant = "honours-cancellation"
		}
		t.Run(variant, func(t *testing.T) {
			_, s := authzTestSetup(t)
			f := newP1Fixture(t, s, "e6-"+variant)
			a1 := newA1Fixture(t, s, "e6-a1-"+variant)

			makeStore := func() *memoTestStore {
				st := newMemoTestStore(s)
				st.ignoreCancel = !honour
				if honour {
					st.fault = cancelHonouringFault
				}
				return st
			}

			// P1 and A1 are run as two independent cancellation sequences,
			// each with its own cancellation point partway through (since
			// identity is fixed per CheckAccess call, one combined sequence
			// would not let both principals reach step 10's masked ctx).
			for _, ident := range []struct {
				name     string
				identity Identity
				tuples   []rawTuple
			}{
				{"P1", f.user, agentResourceTuples(f.agentRes)},
				{"A1", a1.agent, agentResourceTuples(a1.resource)},
			} {
				t.Run(ident.name, func(t *testing.T) {
					jj := len(ident.tuples) / 2
					require.Greater(t, jj, 0)

					refSt := makeStore()
					refAuthz, _ := newRecordingAuthz(refSt)
					refCtx, refCancel := context.WithCancel(context.Background())
					defer refCancel()
					var refDecisions []Decision
					var refCountsAtCancel map[string]int
					for i, tp := range ident.tuples {
						if i == jj {
							refCancel()
							refCountsAtCancel = refSt.snapshotCounts()
						}
						refDecisions = append(refDecisions, refAuthz.CheckAccess(refCtx, ident.identity, tp.resource, tp.action))
					}

					candSt := makeStore()
					candAuthz, _ := newRecordingAuthz(candSt)
					candCtx, candCancel := context.WithCancel(withAuthzInputMemo(context.Background()))
					defer candCancel()
					var candDecisions []Decision
					var candCountsAtCancel map[string]int
					for i, tp := range ident.tuples {
						if i == jj {
							candCancel()
							candCountsAtCancel = candSt.snapshotCounts()
						}
						candDecisions = append(candDecisions, candAuthz.CheckAccess(candCtx, ident.identity, tp.resource, tp.action))
					}

					for i := jj; i < len(ident.tuples); i++ {
						assertDecisionsEqual(t, refDecisions[i], candDecisions[i], "post-cancel tuple %d", i)
					}

					// The decision-equality assertion above cannot by itself
					// catch a memo that silently served post-cancel
					// decisions from a stale cache instead of bypassing to
					// the store, because a mutant that drops the done-ctx
					// bypass can still produce the same decisions in this
					// fixture's shape. Assert directly that the candidate
					// issues the SAME per-method store calls after
					// cancellation as the reference: a working done-ctx
					// bypass makes every post-cancel decision reach the
					// store exactly like the reference does, so the
					// post-cancel call-count DELTA must be equal per method.
					refCountsAfter := refSt.snapshotCounts()
					candCountsAfter := candSt.snapshotCounts()
					for _, method := range []string{"GetEffectiveGroups", "GetEffectiveGroupsForAgent", "ListRoleBindingsForPrincipals", "GetRoleDefinitionsByIDs", "ListAccessConstraints", "GetDelegationEdgesForDelegate"} {
						refDelta := refCountsAfter[method] - refCountsAtCancel[method]
						candDelta := candCountsAfter[method] - candCountsAtCancel[method]
						assert.Equal(t, refDelta, candDelta, "post-cancellation %s call count must match the reference (done-ctx bypass)", method)
					}
					// Non-vacuity: the fixture must actually have decisions
					// left to run after cancellation, so the delta above is
					// not trivially 0 == 0.
					require.Greater(t, len(ident.tuples)-jj, 0, "H2: there must be post-cancel decisions")

					// AuthorizeReadBatch with an ALREADY-cancelled ctx
					// returns ctx.Err() at its own entry check in authz.go
					// before any decision or memo access is
					// even attempted — by construction, this sub-assertion
					// cannot discriminate a working done-ctx bypass from a
					// memo that leaks post-cancel reads, because no store
					// call happens in either run. It exists only to pin
					// AuthorizeReadBatch's own early-return contract, not as
					// evidence about the memo; the memo's done-ctx bypass is
					// what the per-method count-delta assertion above
					// tests, via the raw CheckAccess loop that cancels
					// MID-sequence.
					doneCtx, doneCancel := context.WithCancel(context.Background())
					doneCancel()
					_, refErr := refAuthz.AuthorizeReadBatch(doneCtx, ident.identity, []Resource{ident.tuples[0].resource})
					require.Error(t, refErr)
					assert.True(t, errors.Is(refErr, context.Canceled))
					mDoneCtx := withAuthzInputMemo(doneCtx)
					_, candErr := candAuthz.AuthorizeReadBatch(mDoneCtx, ident.identity, []Resource{ident.tuples[0].resource})
					require.Error(t, candErr)
					assert.True(t, errors.Is(candErr, context.Canceled))
				})
			}
		})
	}
}

// =============================================================================
// A rows: agent principal / delegation ceiling (all reuse a1Fixture)
// =============================================================================

// agentScopeTuples builds one tuple per ScopeActions["agent"], against a
// Resource shaped the way ComputeScopeCapabilities builds one: no ID, just
// the parent project.
func agentScopeTuples(projectID string) []rawTuple {
	scopeRes := Resource{Type: "agent", ParentType: "project", ParentID: projectID}
	out := make([]rawTuple, 0, len(ScopeActions["agent"]))
	for _, a := range ScopeActions["agent"] {
		out = append(out, rawTuple{scopeRes, a})
	}
	return out
}

// agentAllTuples is agentResourceTuples plus agentScopeTuples: all 8
// ResourceActions["agent"] and all 4 ScopeActions["agent"], as rows A1-A8
// require.
func agentAllTuples(res Resource, projectID string) []rawTuple {
	return append(agentResourceTuples(res), agentScopeTuples(projectID)...)
}

// agentGrantableTuples returns a small, deliberately-chosen tuple set that a
// full-lifecycle agent (AgentRoleFull) actually passes the AK1 kernel for,
// so every decision reaches step 10 and the ceiling result is what
// determines Allowed. This matters because agent.read/agent.list/
// agent.update/agent.port_access/agent.stop_all/agent.message/
// agent.grant_hub_mode have no AgentScopes entry in the permissions
// registry at all (agents cannot pass the kernel for them via JWT scope,
// exactly as TestDelegationCeiling_UserAgentChain's comment documents for
// agent.read), and this fixture's target is owned by the delegator, not the
// agent, so no owner/ancestor relationship grant covers them either.
// project.read (AgentScopes: ["project:read"]) is the read-only permission
// AgentRoleFull actually holds; agent.delete/attach/lifecycle (AgentScopes:
// ["project:agent:lifecycle"]) are the non-read-only ones.
func agentGrantableTuples(f *a1Fixture) []rawTuple {
	// Deliberately unowned (no OwnerID) on every tuple's resource, both
	// here and below. A relationship-grant fallback for a user delegator
	// (userRelationshipAuthority) means an "owner" relationship on a
	// resource the delegator itself owns grants the permission
	// independent of any role-binding/role-definition resolution. Every
	// caller of this helper (A6, A7, A7', A8) targets the delegator's
	// ROLE-BASED permission check specifically (faulting GetRoleDefinition,
	// ListAccessConstraints or the edges lookup); an owned resource would
	// let the owner-relationship fallback silently paper over those
	// faults, making the row pass without the fault having been reached.
	// No caller relies on ownership, so every resource here is unowned —
	// f.resource itself carries OwnerID: f.delegatorID (newA1Fixture), so
	// tuples 1-3 use an explicit unowned copy, not f.resource directly.
	//
	// The fixture's non-admin ProjectRoleAdmin delegator holds project.read
	// and agent.lifecycle through its role binding, but NOT discrete
	// agent.delete/agent.attach permissions — those two are only reachable
	// here through the owner-relationship fallback, which the unowned
	// resource disables. So delete/attach deny via
	// DenyCauseCeilingDelegatorLacksPermission even with no fault at all;
	// read/lifecycle allow with no fault and only deny that way once the
	// fault is injected. See TestParity_A6_DelegatorRoleDefinitionFails for
	// why both shapes still belong in the same row.
	projectRes := Resource{Type: "project", ID: f.projectID}
	unownedResource := f.resource
	unownedResource.OwnerID = ""
	return []rawTuple{
		{projectRes, ActionRead},
		{unownedResource, ActionDelete},
		{unownedResource, ActionAttach},
		{unownedResource, ActionLifecycle},
	}
}

// TestParity_A1_AgentOwnProjectSingleLevelEdge is row A1: deep-equal;
// GetDelegationEdgesForDelegate loaded once per phase (the edges memo);
// the delegator's own loads stay per decision (the ceiling ctx is masked).
func TestParity_A1_AgentOwnProjectSingleLevelEdge(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newA1Fixture(t, s, "a1")
	tuples := agentAllTuples(f.resource, f.projectID)

	refDecisions, _, refStore, candStore := runParity(t, s, f.agent, tuples)
	var sawAllow bool
	for _, d := range refDecisions {
		if d.Allowed {
			sawAllow = true
		}
	}
	assert.True(t, sawAllow, "H2: the fixture must reach at least one allowed decision so step 10 actually runs")
	assert.Equal(t, 1, candStore.countOf("GetDelegationEdgesForDelegate"), "edges loaded once per phase under the memo")
	assert.GreaterOrEqual(t, refStore.countOf("GetDelegationEdgesForDelegate"), 1, "reference issues at least one edge load")
	// The delegator's own inputs are never memoized (masked ctx): GetUser
	// is on the delegator path only and is not memo-served, so its count
	// must be identical in both runs.
	assert.Equal(t, refStore.countOf("GetUser"), candStore.countOf("GetUser"), "delegator GetUser calls stay per decision in both runs")
}

// TestParity_A2_AgentNoScopes is row A2: an agent with no JWT scopes
// hits the 7b deny-all restriction for everything; deep-equal.
func TestParity_A2_AgentNoScopes(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newA1Fixture(t, s, "a2")
	noScopeAgent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: f.agentID},
		ProjectID: f.projectID,
	}}
	tuples := agentAllTuples(f.resource, f.projectID)
	refDecisions, _, _, _ := runParity(t, s, noScopeAgent, tuples)
	for i, d := range refDecisions {
		assert.False(t, d.Allowed, "tuple %d must deny under 7b deny-all", i)
	}
}

// TestParity_A3_AgentCrossProject is row A3: the target resource is
// in a different project than the agent's own; the ceiling scope switches
// to the resource's project (in authz_delegation_ceiling.go), where the
// agent has no edge. Deep-equal.
func TestParity_A3_AgentCrossProject(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newA1Fixture(t, s, "a3")
	crossRes := Resource{Type: "agent", ID: tid("a3-cross-target"), ParentType: "project", ParentID: f.otherProjID, OwnerID: f.delegatorID}
	tuples := agentAllTuples(crossRes, f.otherProjID)
	refDecisions, _, _, _ := runParity(t, s, f.agent, tuples)
	// H2: at least the JWT-scope-eligible actions must reach the kernel with
	// a cross-project denial (rather than short-circuiting earlier), i.e.
	// some decision must actually deny for a reason distinct from 7b.
	var sawNonScopeDeny bool
	for _, d := range refDecisions {
		if !d.Allowed && d.Reason != "" {
			sawNonScopeDeny = true
		}
	}
	assert.True(t, sawNonScopeDeny, "H2: cross-project fixture must reach a real denial")

	// Pin the specific cross-project deny mechanism: the kernel's
	// buildDenyReasons produces "no active binding grants permission
	// \"<id>\"" because the synthetic binding exists (so len(rejected)>0)
	// but its scope (the agent's OWN project) does not match the
	// cross-project resource, so it grants nothing. The permission ID
	// itself is per-action by construction (the kernel interpolates it
	// into the string), so the assertion pins the stable PREFIX, not full
	// equality across tuples — a hardcoded full literal would be wrong
	// here, not just brittle.
	for i, d := range refDecisions {
		if !d.Allowed && d.Reason != "" {
			assert.Contains(t, d.Reason, "no active binding grants permission", "tuple %d: the cross-project denial must be the kernel's scope-mismatch deny, not some other mechanism", i)
		}
	}
}

// TestParity_A4_DelegatorLostPermission is row A4: the delegator no
// longer holds any permission (no role binding at all), so the ceiling
// denies with DenyCauseCeilingDelegatorLacksPermission. Deep-equal.
func TestParity_A4_DelegatorLostPermission(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	projectID := tid("a4-project")
	delegatorID := tid("a4-delegator")
	agentID := tid("a4-agent")
	targetID := tid("a4-target")

	createDCProject(t, s, projectID, "a4-proj")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: delegatorID, Email: "a4-delegator@test.com", DisplayName: "a4", Role: "member", Status: "active"}))
	createDCAgent(t, s, agentID, projectID, delegatorID, AgentRoleFull)
	createDCAgent(t, s, targetID, projectID, delegatorID, AgentRoleFull)
	createDCEdge(t, s, store.DelegationPrincipalUser, delegatorID, store.DelegationPrincipalAgent, agentID, store.RoleScopeProject, projectID, string(AgentRoleFull))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	res := Resource{Type: "agent", ID: targetID, ParentType: "project", ParentID: projectID, OwnerID: delegatorID}
	tuples := agentAllTuples(res, projectID)

	refDecisions, _, _, _ := runParity(t, s, agent, tuples)
	var sawCeilingDeny bool
	for _, d := range refDecisions {
		if d.DenyCause == DenyCauseCeilingDelegatorLacksPermission {
			sawCeilingDeny = true
		}
	}
	assert.True(t, sawCeilingDeny, "H2: fixture must reach DenyCauseCeilingDelegatorLacksPermission")
}

// TestParity_A5_AgentInAncestryOfTarget is row A5: an agent principal
// appearing in the target's Ancestry chain, granted through the
// ancestor relationship. None of the 8 ResourceActions["agent"] that the
// ancestor relationship admits for an agent principal (agent.delete,
// agent.attach, agent.lifecycle, agent.set_message_mode — see
// relationshipCharacterizedAllowlist{"ancestor","agent","agent"}) is
// exclusive to the relationship: AgentRoleFull's own JWT scope already
// grants all four in the agent's own project, which would make the row
// deep-equal without the ancestor grant ever mattering. To isolate the
// relationship, the target is in a DIFFERENT project than the agent's own,
// so the JWT-scope synthetic binding does not apply (a scope mismatch, the
// same kernel-deny mechanism A3 exercises) and only the ancestor relationship
// can grant it.
func TestParity_A5_AgentInAncestryOfTarget(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	f := newA1Fixture(t, s, "a5")
	otherTargetID := tid("a5-other-target")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: otherTargetID, Slug: "a5-other-target", Name: "a5-other-target",
		ProjectID: f.otherProjID, Phase: "running", OwnerID: f.delegatorID,
		Ancestry: []string{f.delegatorID, f.agentID},
	}))
	res := Resource{Type: "agent", ID: otherTargetID, ParentType: "project", ParentID: f.otherProjID, OwnerID: f.delegatorID, Ancestry: []string{f.delegatorID, f.agentID}}

	refDecisions, _, _, _ := runParity(t, s, f.agent, []rawTuple{{res, ActionDelete}})
	require.Len(t, refDecisions, 1)
	assert.True(t, refDecisions[0].Allowed, "H2: the agent must be granted access through the ancestor relationship: reason=%q", refDecisions[0].Reason)
	assert.Equal(t, "relationship grant: ancestor access", refDecisions[0].Reason)
}

// TestParity_D1_FilteredBindings is row D1 ("expired binding,
// NotBefore in the future, missing role def | deep-equal"), as three
// sub-cases: each binding is filtered out or degrades to no
// permissions downstream of the memo, which stores bindings and roleDefs
// unfiltered — expiry, NotBefore and missing-role-def handling run fresh on
// every decision against KernelRequest.Now, exactly where a stale cached
// filter result could diverge if the memo ever started caching post-filter
// state instead of raw inputs.
func TestParity_D1_FilteredBindings(t *testing.T) {
	t.Run("expired_binding", func(t *testing.T) {
		_, s := authzTestSetup(t)
		ctx := context.Background()
		f := newProjectPrincipalFixture(t, s, "d1-expired", store.ProjectRoleMember)
		// Replace the fresh binding with an expired one for the same role.
		bindings, err := s.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{{Type: "user", ID: f.userID}}, nil, nil)
		require.NoError(t, err)
		require.NotEmpty(t, bindings)
		for _, b := range bindings {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
		}
		rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
		require.NoError(t, err)
		expired := time.Now().Add(-time.Hour)
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.userID,
			ScopeType: store.RoleScopeProject, ScopeID: f.projectID, ExpiresAt: &expired, CreatedBy: "test",
		})
		require.NoError(t, err)

		refDecisions, _, _, _ := runParity(t, s, f.user, agentResourceTuples(f.agentRes))
		for i, d := range refDecisions {
			assert.False(t, d.Allowed, "H2: an expired binding must grant nothing: tuple %d", i)
		}
	})

	t.Run("not_yet_active_binding", func(t *testing.T) {
		_, s := authzTestSetup(t)
		ctx := context.Background()
		f := newProjectPrincipalFixture(t, s, "d1-notbefore", store.ProjectRoleMember)
		bindings, err := s.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{{Type: "user", ID: f.userID}}, nil, nil)
		require.NoError(t, err)
		require.NotEmpty(t, bindings)
		for _, b := range bindings {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
		}
		rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
		require.NoError(t, err)
		future := time.Now().Add(time.Hour)
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.userID,
			ScopeType: store.RoleScopeProject, ScopeID: f.projectID, NotBefore: &future, CreatedBy: "test",
		})
		require.NoError(t, err)

		refDecisions, _, _, _ := runParity(t, s, f.user, agentResourceTuples(f.agentRes))
		for i, d := range refDecisions {
			assert.False(t, d.Allowed, "H2: a not-yet-active binding must grant nothing: tuple %d", i)
		}
	})

	t.Run("missing_role_definition", func(t *testing.T) {
		_, baseStore := authzTestSetup(t)
		ctx := context.Background()
		f := newProjectPrincipalFixture(t, baseStore, "d1-norole", store.ProjectRoleMember)
		bindings, err := baseStore.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{{Type: "user", ID: f.userID}}, nil, nil)
		require.NoError(t, err)
		require.NotEmpty(t, bindings)
		roleDefID := bindings[0].RoleDefinitionID

		// Both CreateRoleBinding (creation-time "binding guard") and
		// DeleteRoleDefinition (refuses deletion while bindings reference
		// it, "role has N active binding(s)") prevent orphaning a binding
		// through the store API. Simulate "missing" the way
		// loadRoleDefinitions actually observes it instead: a store wrapper
		// that filters the real role definition out of
		// GetRoleDefinitionsByIDs' result, so the binding resolves and is
		// active, but contributes no permissions — exactly D1's scenario.
		s := &missingRoleDefStore{Store: baseStore, missingID: roleDefID}

		refDecisions, _, _, _ := runParity(t, s, f.user, agentResourceTuples(f.agentRes))
		for i, d := range refDecisions {
			assert.False(t, d.Allowed, "H2: a binding with a missing role definition must grant nothing: tuple %d", i)
		}
	})
}

// missingRoleDefStore wraps a store.Store and removes missingID from every
// GetRoleDefinitionsByIDs result, simulating a role definition that no
// longer resolves even though a binding still references it — a state the
// store API itself refuses to create (DeleteRoleDefinition rejects deletion
// while any binding references the role).
type missingRoleDefStore struct {
	store.Store
	missingID string
}

func (m *missingRoleDefStore) GetRoleDefinitionsByIDs(ctx context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	defs, err := m.Store.GetRoleDefinitionsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	delete(defs, m.missingID)
	return defs, nil
}

// TestParity_A6_DelegatorRoleDefinitionFails is row A6: the delegator's
// GetRoleDefinition (not memo-served) fails (a) always, (b) on call #1
// only. Deep-equal for every decision in both variants. tuple 0
// (project.read) proves the read action fails closed exactly like the
// writes — there is no read-only exception in the ceiling's error path.
// Two of the four tuples (agent.delete, agent.attach) deny via
// DenyCauseCeilingDelegatorLacksPermission with no fault at all, because
// the delegator's role binding never grants them independent of
// ownership; the other two (project.read, agent.lifecycle) are normally
// allowed and only flip to that same deny once the fault lands on their
// own GetRoleDefinition call — every call ("always") or just the first
// decision's ("call1," since each decision issues exactly one such call
// in order). Pinning the exact DenyCause (or exact allow, for a decision
// the fault did not reach) on every one of the four means the row cannot
// pass on a decision the fault never actually reached, and still catches
// a mutant that fails open on the GetRoleDefinition error, or that lets
// one decision's fault leak into another's result.
func TestParity_A6_DelegatorRoleDefinitionFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int // 0 = always
	}{
		{"always", 0},
		{"call1", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s := authzTestSetup(t)
			f := newA1Fixture(t, s, "a6-"+tc.name)
			// agentGrantableTuples, not agentAllTuples — a read action
			// (project.read) must actually reach the ceiling, which
			// agent.read/agent.list on an agent-typed resource never do
			// (no AgentScopes entry at all — see agentGrantableTuples).
			// Every resource here is unowned (see agentGrantableTuples'
			// comment), so the delegator's relationship-grant fallback
			// cannot grant any of these four independent of the
			// role-definition lookup this row faults.
			tuples := agentGrantableTuples(f)

			// An in-test no-fault control, run on all four tuples. Indices
			// 0 (project.read) and 3 (agent.lifecycle) are permissions the
			// delegator's ProjectRoleAdmin binding genuinely grants, so the
			// control must show them allowed absent the fault — the fault
			// then flips each to a ceiling deny below, which is the
			// interesting case. Indices 1 (agent.delete) and 2
			// (agent.attach) are NOT covered by that role binding at all
			// (only the owner-relationship fallback reaches them, and it is
			// disabled here by the unowned resource), so the control
			// already denies for those two, fault or not, via the same
			// DenyCauseCeilingDelegatorLacksPermission. Asserting that
			// exact cause here, rather than skipping the control for these
			// two, still catches a mutant that fails open on the
			// GetRoleDefinition error: such a mutant would flip indices 1
			// and 2 to Allowed as well, which this loop would catch.
			controlStore := newMemoTestStore(s)
			controlAuthz, _ := newRecordingAuthz(controlStore)
			roleGrantedNoFault := map[int]bool{0: true, 3: true}
			for i, tp := range tuples {
				controlDecision := controlAuthz.CheckAccess(context.Background(), f.agent, tp.resource, tp.action)
				if roleGrantedNoFault[i] {
					require.True(t, controlDecision.Allowed, "H2: tuple %d must be allowed with no fault, or its DenyCause pin below proves nothing", i)
				} else {
					require.False(t, controlDecision.Allowed, "H2: tuple %d has no role-based grant and no ownership fallback, so it must already deny with no fault", i)
					require.Equal(t, DenyCauseCeilingDelegatorLacksPermission, controlDecision.DenyCause,
						"H2: tuple %d's no-fault deny must be the delegator lacking the permission, not some other cause", i)
				}
			}

			var fault func(string, int, context.Context) error
			if tc.n == 0 {
				fault = alwaysFault("GetRoleDefinition")
			} else {
				fault = callNFault("GetRoleDefinition", tc.n)
			}
			refStore := newMemoTestStore(s)
			refStore.fault = fault
			candStore := newMemoTestStore(s)
			candStore.fault = fault

			refDecisions, candDecisions, _, _ := runParityWithStores(t, refStore, candStore, f.agent, tuples)
			assert.GreaterOrEqual(t, refStore.countOf("GetRoleDefinition"), 1, "H2: the fault must actually be reachable in the reference")

			// Confirm a read tuple (project.read, index 0 per
			// agentGrantableTuples) is actually present, so "the read
			// action fails closed like the writes" is a claim about a
			// tuple that really exists in this row, not a vacuous one.
			require.True(t, isReadOnlyOperation(tuples[0].action), "H2: tuple 0 must be the read action")

			// GetRoleDefinition is an UNMEMOIZED, direct call on the
			// delegator's own permission resolution path
			// (getEffectivePermissions), issued fresh on every decide call
			// regardless of the request-local memo — so decision 0's call
			// is always call #1, decision 1's is call #2, and so on, for
			// both variants ("always" simply fails every one of them).
			// Pin the exact DenyCause for every tuple, not just tuple 0:
			// a decision the delegator's role binding would grant with no
			// fault (0, 3 — see roleGrantedNoFault above) only becomes
			// this specific ceiling deny once ITS OWN call is the one that
			// fails, which for "call1" is decision tc.n-1 alone; a
			// decision the role binding never grants (1, 2) shows this
			// same deny regardless, since it has nothing to do with the
			// fault. A decision whose own call is unaffected must still
			// show its normal allow, not the deny — asserting that is
			// what catches a candidate that wrongly lets one decision's
			// fault leak into another's result.
			for i := range refDecisions {
				wantCeilingDeny := !roleGrantedNoFault[i] || tc.n == 0 || i == tc.n-1
				if wantCeilingDeny {
					assert.Equal(t, DenyCauseCeilingDelegatorLacksPermission, refDecisions[i].DenyCause,
						"reference decision %d must deny via the delegator's role-definition resolution failing, not some other cause", i)
					assert.Equal(t, DenyCauseCeilingDelegatorLacksPermission, candDecisions[i].DenyCause,
						"candidate decision %d must show the identical ceiling deny — the memo must not mask the delegator-side fault", i)
				} else {
					assert.True(t, refDecisions[i].Allowed,
						"reference decision %d is not the call the fault landed on, so it must still allow normally", i)
					assert.True(t, candDecisions[i].Allowed,
						"candidate decision %d is not the call the fault landed on, so it must still allow normally", i)
				}
			}
		})
	}
}

// TestParity_A7_ExactParityDelegatorConstraintFailure is row A7: the
// delegator is the A1 fixture's non-admin, in-scope-bound user.
// ListAccessConstraints fails iff getDelegationCeilingCache(ctx) != nil —
// true only inside step 10 — which selects exactly the delegator's calls
// in both runs, regardless of how the memo shifts decide's own call
// indices. Exact parity is required: every decision denies with
// DenyCauseCeilingDelegatorLacksPermission.
func TestParity_A7_ExactParityDelegatorConstraintFailure(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newA1Fixture(t, s, "a7")
	tuples := agentGrantableTuples(f) // a read tuple must also reach the ceiling

	predicateFault := func(method string, _ int, ctx context.Context) error {
		if method != "ListAccessConstraints" {
			return nil
		}
		if getDelegationCeilingCache(ctx) != nil {
			return fmt.Errorf("injected delegator-side constraint fault: %w", errInjected)
		}
		return nil
	}
	refStore := newMemoTestStore(s)
	refStore.fault = predicateFault
	refStore.recordCalls = true
	candStore := newMemoTestStore(s)
	candStore.fault = predicateFault
	candStore.recordCalls = true

	refDecisions, _, refStoreOut, candStoreOut := runParityWithStores(t, refStore, candStore, f.agent, tuples)
	var sawCeilingDeny bool
	for i, d := range refDecisions {
		assert.False(t, d.Allowed, "tuple %d must deny (delegator constraint load fails, deny-all filters permissions to empty)", i)
		if d.DenyCause == DenyCauseCeilingDelegatorLacksPermission {
			sawCeilingDeny = true
		}
	}
	assert.True(t, sawCeilingDeny, "H2: at least one tuple must reach DenyCauseCeilingDelegatorLacksPermission, proving the kernel/relationship stage allowed before the ceiling denied")

	// The actual count clause, not just NotZero. decide's own
	// (predicate-false) 7c calls: exactly one per decision in the
	// reference (every decision reaches 7c unconditionally), versus
	// exactly 1 in the candidate (memoized after the first). The
	// delegator's (predicate-true) calls: equal between the two runs,
	// since the delegator path is never memoized in either.
	countPredicate := func(records []storeCallRecord, wantTrue bool) int {
		n := 0
		for _, r := range records {
			if r.method == "ListAccessConstraints" && r.ceilingActive == wantTrue {
				n++
			}
		}
		return n
	}
	refRecords := refStoreOut.recordedCalls()
	candRecords := candStoreOut.recordedCalls()
	refPredicateFalse := countPredicate(refRecords, false)
	candPredicateFalse := countPredicate(candRecords, false)
	refPredicateTrue := countPredicate(refRecords, true)
	candPredicateTrue := countPredicate(candRecords, true)

	assert.Equal(t, len(tuples), refPredicateFalse, "the reference's own decide-side constraint calls, one per decision")
	assert.Equal(t, 1, candPredicateFalse, "the candidate's decide-side constraint calls, memoized to exactly 1")
	assert.Equal(t, refPredicateTrue, candPredicateTrue, "the delegator's constraint calls must be equal between runs — a leak here is exactly what this row exists to catch")
	assert.Greater(t, refPredicateTrue, 0, "H2: the delegator's predicate-true calls must actually occur")
}

// TestParity_A7Prime_FirstAttemptTransientPlusDeterministicDelegatorFault is
// row A7': same fixture as A7. decide's own (predicate-false) 7c call
// fails on its 1st call only (decision 1, deny-all, nothing memoized, the
// ceiling is not reached). Every delegator (predicate-true) call fails
// always. Decision 2+ : 7c succeeds and is memoized in the candidate, then a
// ceiling deny with DelegatorLacksPermission in both runs.
func TestParity_A7Prime_FirstAttemptTransientPlusDeterministicFault(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newA1Fixture(t, s, "a7prime")
	tuples := agentGrantableTuples(f)
	require.GreaterOrEqual(t, len(tuples), 2)

	predicateFalseCallCount := 0
	var mu sync.Mutex
	fault := func(method string, _ int, ctx context.Context) error {
		if method != "ListAccessConstraints" {
			return nil
		}
		if getDelegationCeilingCache(ctx) != nil {
			return fmt.Errorf("injected delegator-side constraint fault: %w", errInjected)
		}
		mu.Lock()
		predicateFalseCallCount++
		n := predicateFalseCallCount
		mu.Unlock()
		if n == 1 {
			return fmt.Errorf("injected decide-side first-attempt fault: %w", errInjected)
		}
		return nil
	}
	refStore := newMemoTestStore(s)
	refStore.fault = fault
	// The candidate needs its own independent predicate-false counter.
	predicateFalseCallCount2 := 0
	var mu2 sync.Mutex
	candFault := func(method string, _ int, ctx context.Context) error {
		if method != "ListAccessConstraints" {
			return nil
		}
		if getDelegationCeilingCache(ctx) != nil {
			return fmt.Errorf("injected delegator-side constraint fault: %w", errInjected)
		}
		mu2.Lock()
		predicateFalseCallCount2++
		n := predicateFalseCallCount2
		mu2.Unlock()
		if n == 1 {
			return fmt.Errorf("injected decide-side first-attempt fault: %w", errInjected)
		}
		return nil
	}
	candStore := newMemoTestStore(s)
	candStore.fault = candFault

	refDecisions, candDecisions, _, _ := runParityWithStores(t, refStore, candStore, f.agent, tuples)
	assert.False(t, refDecisions[0].Allowed, "decision 1 must deny at 7c (deny-all)")
	// Pin the exact 7c deny-all reason. The first tuple (project.read) is
	// granted by the agent's JWT scope before restrictions, so the
	// nil-Check access_constraint_error restriction removes a permission
	// that WAS granted (authz_kernel.go's "restriction has no check
	// function (fail closed)" detail), not "never granted".
	assert.Equal(t, "restriction removed permission: access_constraint_error - restriction has no check function (fail closed)", refDecisions[0].Reason, "decision 1's exact 7c deny-all reason")
	assertDecisionsEqual(t, refDecisions[0], candDecisions[0], "decision 1")
	for i := 1; i < len(refDecisions); i++ {
		assert.False(t, refDecisions[i].Allowed, "decision %d must be a ceiling deny", i+1)
		assert.Equal(t, DenyCauseCeilingDelegatorLacksPermission, refDecisions[i].DenyCause, "decision %d", i+1)
	}
}

// TestParity_A8_DelegationEdgesFail is row A8: A1's single-delegate
// fixture, so each decision that reaches step 10 makes at most one edge
// lookup THROUGH THE CEILING (the call2_divergence_use subtest is the one
// exception: a progeny-granted secret.use also triggers upstream #2206's
// execution-project admission stage, which makes its own, always-uncached
// edge lookup before the ceiling's — see that subtest's comment for the
// resulting 3-call shape). (a) always fails: deep-equal by construction
// (every decision hits the same fault). (b) call #1 only: deep-equal (both
// runs fail independently on their own first attempt, exactly like E5). (c)
// the edge lookup THE CEILING ITSELF makes on the reference's second
// step-10 decision: the documented later-transient divergence — it fires there, never
// on the candidate's (memo hit). The candidate's corresponding decision
// deep-equals the same decision in a no-fault run.
//
// Because an edge-lookup error fails closed for every action, (c) applies
// to read decisions exactly as to writes: a memo hit on the requester's
// edges masks a later transient edge error for a read too. With the memo
// installed at the capability functions, AuthorizeReadBatch and the agent
// list handlers, that holds for every listing request; the install-site
// form is pinned by TestMemoInstall_LaterEdgeFaultServedFromMemoAtInstallSites.
// Edge lookups made while walking a relationship candidate's source agent
// chain never share the memo (TestMemoInstall_SourceAgentEdgeFaultNotServedFromMemo),
// so the divergence is limited to the requester's own ceiling lookups.
func TestParity_A8_DelegationEdgesFail(t *testing.T) {
	t.Run("always", func(t *testing.T) {
		_, s := authzTestSetup(t)
		f := newA1Fixture(t, s, "a8-always")
		tuples := agentGrantableTuples(f)
		refStore := newMemoTestStore(s)
		refStore.fault = alwaysFault("GetDelegationEdgesForDelegate")
		candStore := newMemoTestStore(s)
		candStore.fault = alwaysFault("GetDelegationEdgesForDelegate")
		refDecisions, _, _, _ := runParityWithStores(t, refStore, candStore, f.agent, tuples)
		// Upstream #2206 removed the read-only fail-open skip from the
		// edge-lookup error path entirely: every action, read included, now
		// fails closed uniformly with DenyCauseCeilingError on an edge
		// error. H2 (non-vacuity): both a read and a write tuple must
		// actually reach this path and deny with that exact cause — proving
		// the fault fired for both action classes, not just one.
		var sawReadCeilingError, sawWriteCeilingError bool
		for i, d := range refDecisions {
			action := tuples[i].action
			if d.DenyCause != DenyCauseCeilingError {
				continue
			}
			if isReadOnlyOperation(action) {
				sawReadCeilingError = true
			} else {
				sawWriteCeilingError = true
			}
		}
		assert.True(t, sawReadCeilingError, "H2: a read action must fail closed with DenyCauseCeilingError (no read-only skip exists post-#2206)")
		assert.True(t, sawWriteCeilingError, "H2: a write action must fail closed with DenyCauseCeilingError")
	})

	t.Run("call1", func(t *testing.T) {
		_, s := authzTestSetup(t)
		f := newA1Fixture(t, s, "a8-call1")
		tuples := agentGrantableTuples(f)
		refStore := newMemoTestStore(s)
		refStore.fault = callNFault("GetDelegationEdgesForDelegate", 1)
		candStore := newMemoTestStore(s)
		candStore.fault = callNFault("GetDelegationEdgesForDelegate", 1)
		runParityWithStores(t, refStore, candStore, f.agent, tuples)
		assert.GreaterOrEqual(t, refStore.countOf("GetDelegationEdgesForDelegate"), 1)
	})

	// call2Divergence runs tp0 (always allowed, primes the edges memo with a
	// successful call #1) then tp1 (whose edge lookup is call #2, the one
	// that fails in the reference), against a FRESH fixture each time, and
	// asserts: (1) H2 non-vacuity — the fault actually fired on call #2 in
	// both the reference and candidate runs' first attempt (the candidate's
	// own would-be call #2 never happens, which IS the divergence); (2) the
	// reference's decision 2 matches wantRefAllowed/wantRefDenyCause
	// exactly, proving the denial (or skip-allow) is caused by the EDGE
	// fault specifically, not a pre-existing delegator-lacks-permission
	// deny (the old version's write case picked agent.delete, which the
	// project-admin delegator never held, so it denied regardless of any
	// fault); (3) the candidate's decision 2 deep-equals a no-fault
	// baseline's decision 2 (the documented later-transient divergence).
	// actionResource returns the correct resource for action under f: the
	// shared project resource for ActionRead (project.read, since
	// agent.read has no AgentScopes entry at all — see
	// agentGrantableTuples), or f's agent resource for any other action.
	actionResource := func(f *a1Fixture, action Action) Resource {
		if action == ActionRead {
			// Deliberately NOT OwnerID: f.delegatorID — see
			// agentGrantableTuples's comment (upstream #2206's
			// userRelationshipAuthority owner-relationship fallback would
			// otherwise mask the edge fault this row exists to exercise).
			return Resource{Type: "project", ID: f.projectID}
		}
		return f.resource
	}

	call2Divergence := func(t *testing.T, name string, action0, action1 Action, wantRefAllowed bool, wantRefDenyCause DenyCause) {
		t.Helper()
		_, s := authzTestSetup(t)
		f := newA1Fixture(t, s, "a8-call2-"+name)
		tuples := []rawTuple{
			{actionResource(f, action0), action0},
			{actionResource(f, action1), action1},
		}
		ctx := context.Background()

		baselineStore := newMemoTestStore(s)
		baselineAuthz, _ := newRecordingAuthz(baselineStore)
		var baseline []Decision
		for _, tp := range tuples {
			baseline = append(baseline, baselineAuthz.CheckAccess(ctx, f.agent, tp.resource, tp.action))
		}
		require.Equal(t, 2, baselineStore.countOf("GetDelegationEdgesForDelegate"), "H2: both decisions must reach step 10 unmemoized (fixture sanity)")

		edgeFaultOnCall2 := func(reached *int) func(string, int, context.Context) error {
			return func(method string, _ int, _ context.Context) error {
				if method != "GetDelegationEdgesForDelegate" {
					return nil
				}
				*reached++
				if *reached == 2 {
					return fmt.Errorf("injected edge fault on call #2: %w", errInjected)
				}
				return nil
			}
		}

		refReached := 0
		refStore := newMemoTestStore(s)
		refStore.fault = edgeFaultOnCall2(&refReached)
		refAuthz, _ := newRecordingAuthz(refStore)
		var ref []Decision
		for _, tp := range tuples {
			ref = append(ref, refAuthz.CheckAccess(ctx, f.agent, tp.resource, tp.action))
		}
		require.Equal(t, 2, refReached, "H2: the fault predicate must see exactly 2 edge calls in the reference")
		assert.Equal(t, wantRefAllowed, ref[1].Allowed, "reference decision 2 (%s): the edge fault's own outcome", name)
		assert.Equal(t, wantRefDenyCause, ref[1].DenyCause, "reference decision 2 (%s): DenyCause must show the edge-failure path, not a pre-existing delegator-lacks deny", name)

		candReached := 0
		candStore := newMemoTestStore(s)
		candStore.fault = edgeFaultOnCall2(&candReached)
		candAuthz, _ := newRecordingAuthz(candStore)
		mctx := withAuthzInputMemo(ctx)
		var cand []Decision
		for _, tp := range tuples {
			cand = append(cand, candAuthz.CheckAccess(mctx, f.agent, tp.resource, tp.action))
		}
		assert.Equal(t, 1, candReached, "H2: the candidate's edges call happens once; call #2 never occurs (memo hit)")
		assert.Equal(t, 1, candStore.countOf("GetDelegationEdgesForDelegate"))
		assertDecisionsEqual(t, baseline[1], cand[1], "candidate decision 2 (%s) vs no-fault baseline", name)
	}

	t.Run("call2_divergence_read", func(t *testing.T) {
		// project.read: upstream #2206 removed the read-only fail-open skip
		// from the edge-lookup error path entirely, so an edge fault now
		// fails closed uniformly regardless of action — the delegator
		// holding project.read no longer matters once the edge lookup
		// itself errors. agent.lifecycle primes call #1 (the delegator
		// holds it too, so decision 1 is an ordinary allow).
		call2Divergence(t, "read", ActionLifecycle, ActionRead, false, DenyCauseCeilingError)
	})

	t.Run("call2_divergence_write", func(t *testing.T) {
		// agent.lifecycle: the delegator DOES hold it (unlike agent.delete
		// in the old version), so with no fault the ceiling allows it
		// normally. Under the reference's edge fault it fails closed
		// (DenyCauseCeilingError); the candidate, serving the memoized
		// edges from decision 1, evaluates the ceiling normally and
		// allows — the real later-transient divergence, write direction.
		call2Divergence(t, "write", ActionRead, ActionLifecycle, false, DenyCauseCeilingError)
	})

	// A use-action (T8 fixture) variant is also covered for A8, since use
	// is not read-only, so an edge error on it fails closed exactly like a
	// write. secret.use has no CapabilityKind/AgentScopes entry, so it is
	// reached only through the progeny relationship grant
	// (not the JWT-scope kernel path), which requires a dedicated agent
	// whose Ancestry includes the secret's owner. That agent also needs its
	// OWN delegation edge (to the same admin delegator) for the ceiling to
	// have anything to look up at all.
	t.Run("call2_divergence_use", func(t *testing.T) {
		_, s := authzTestSetup(t)
		f := newA1Fixture(t, s, "a8-call2-use")
		secretID := tid("a8-call2-use-secret")
		require.NoError(t, s.CreateSecret(context.Background(), &store.Secret{
			ID: secretID, Key: "a8-use-secret", Scope: "user", ScopeID: f.delegatorID, AllowProgeny: true, CreatedBy: f.delegatorID,
		}))
		secretRes := Resource{Type: "secret", ID: secretID}
		usePerm := permissions.Permission{ID: "secret.use", Action: string(ActionUse)}

		// Distinct from newA1Fixture's own agentID (tid(name+"-agent")) —
		// using the same name suffix here previously collided with it.
		useAgentID := tid("a8-call2-use-use-agent")
		createDCAgent(t, s, useAgentID, f.projectID, f.delegatorID, AgentRoleFull)
		createDCEdge(t, s, store.DelegationPrincipalUser, f.delegatorID, store.DelegationPrincipalAgent, useAgentID, store.RoleScopeProject, f.projectID, string(AgentRoleFull))
		useAgent := &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: useAgentID},
			ProjectID: f.projectID,
			Ancestry:  []string{f.delegatorID},
			Scopes:    allRegisteredAgentScopes(),
		}}

		ctx := context.Background()
		decide := func(authz *AuthzService, c context.Context) Decision {
			return authz.Decide(c, AuthzRequest{
				Principal:  principalContextForIdentity(useAgent),
				Credential: credentialContextForIdentity(useAgent),
				Resource:   secretRes,
				Action:     ActionUse,
				Permission: usePerm.ID,
			})
		}
		// agent.lifecycle on the agent's own resource primes edge call #1.
		// secret.use is reached only via the progeny relationship grant
		// (kernel denies it directly), so decide's step 9 now also runs
		// upstream #2206's new execution-project admission stage
		// (authz_execution_project.go), whose source resolver makes its
		// OWN, always-uncached GetDelegationEdgesForDelegate call (edge
		// call #2) — bypassing both the per-decision ceiling cache and our
		// edges memo entirely, since it calls the store directly rather
		// than through getCachedDelegationEdges. Only AFTER that does
		// secret.use reach step 10's ceiling, whose own edge lookup is edge
		// call #3 — the one this row's fault, and the memo, actually target.
		priming := rawTuple{f.resource, ActionLifecycle}

		baselineStore := newMemoTestStore(s)
		baselineAuthz, _ := newRecordingAuthz(baselineStore)
		baseline0 := baselineAuthz.CheckAccess(ctx, useAgent, priming.resource, priming.action)
		baseline1 := decide(baselineAuthz, ctx)
		require.True(t, baseline0.Allowed, "H2 sanity: priming decision must be allowed")
		require.Equal(t, 3, baselineStore.countOf("GetDelegationEdgesForDelegate"), "1 priming + 1 execution-project resolver (uncached) + 1 ceiling lookup")

		refReached := 0
		refStore := newMemoTestStore(s)
		refStore.fault = func(method string, _ int, _ context.Context) error {
			if method != "GetDelegationEdgesForDelegate" {
				return nil
			}
			refReached++
			if refReached == 3 {
				return fmt.Errorf("injected edge fault on call #3: %w", errInjected)
			}
			return nil
		}
		refAuthz, _ := newRecordingAuthz(refStore)
		refAuthz.CheckAccess(ctx, useAgent, priming.resource, priming.action)
		refUse := decide(refAuthz, ctx)
		require.Equal(t, 3, refReached, "H2: the fault predicate must see exactly 3 edge calls")
		assert.False(t, refUse.Allowed, "reference secret.use must fail closed on the edge error (use is not read-only)")
		assert.Equal(t, DenyCauseCeilingError, refUse.DenyCause)

		candReached := 0
		candStore := newMemoTestStore(s)
		candStore.fault = func(method string, _ int, _ context.Context) error {
			if method != "GetDelegationEdgesForDelegate" {
				return nil
			}
			candReached++
			if candReached == 3 {
				return fmt.Errorf("injected edge fault on call #3: %w", errInjected)
			}
			return nil
		}
		candAuthz, _ := newRecordingAuthz(candStore)
		mctx := withAuthzInputMemo(ctx)
		candAuthz.CheckAccess(mctx, useAgent, priming.resource, priming.action)
		candUse := decide(candAuthz, mctx)
		// The candidate still reaches call #2 (the execution-project
		// resolver's uncached lookup always hits the store, memo or not),
		// but never call #3: the ceiling's own lookup is served from the
		// edges memo populated by the priming decision (same delegate key).
		assert.Equal(t, 2, candReached, "H2: the candidate reaches the uncached resolver call, but call #3 (the ceiling's own lookup) never occurs (memo hit)")
		assertDecisionsEqual(t, baseline1, candUse, "candidate secret.use decision vs no-fault baseline")
	})
}

// =============================================================================
// E6b(a): REQUIRED non-waivable security gate
// =============================================================================

// TestParity_E6b_DoneCtxEdgesBypass_RequiredGate is row E6b (a), a required
// unit-level test backing the non-waivable acceptance gate: no fail-open
// skip exists in authz_delegation_ceiling.go for any action, including
// reads (grep-verified: isReadOnlyOperation has exactly one remaining call
// site, inside ceilingReadAllowance, which governs an allow path, not an
// error skip), and a done ctx bypasses the memo.
//
// It calls checkDelegationCeiling directly (package-internal), exactly as
// decide's step 10 does: first on a live memo ctx to populate the edges
// slot, then on a pre-cancelled child of that SAME memo ctx (run 1), and
// compares the outcome to the identical call with no memo at all on an
// equally cancelled ctx (run 2). A fresh per-decision delegation-ceiling
// cache on every call ensures only the memo could have served the edges.
//
// The load-bearing assertion is non-vacuity, not the outcome tuple: run 1
// must still issue GetDelegationEdgesForDelegate on the done ctx. A broken
// memo that incorrectly served edges from a done ctx would still produce
// an identical (allowed, reason, err) tuple to run 2 for both read and
// write (upstream #2206 removed the old read-only fail-open skip entirely
// — a done-ctx store error now fails closed uniformly, regardless of
// action) — only the call count catches that regression.
//
// The read arm matters as much as the write arm here: with the memo
// installed at the capability functions, AuthorizeReadBatch and the agent
// list handlers, a memo hit can mask a later transient edge error for a
// read (see the delegation-edge fault test above). This test is what keeps
// that accepted divergence from extending to a cancelled request: on a done
// ctx the edges are always looked up again, so read and write both fail
// closed.
func TestParity_E6b_DoneCtxEdgesBypass_RequiredGate(t *testing.T) {
	for _, variant := range []string{"honours", "ignores"} {
		for _, ac := range []struct {
			name   string
			action Action
		}{{"read", ActionRead}, {"write", ActionUpdate}} {
			t.Run(variant+"/"+ac.name, func(t *testing.T) {
				_, s := authzTestSetup(t)
				f := newA1Fixture(t, s, "e6b-"+variant+"-"+ac.name)

				req := AuthzRequest{
					Principal:  PrincipalContext{Identity: f.agent},
					Credential: credentialContextForIdentity(f.agent),
					Resource:   f.resource,
					Action:     ac.action,
				}
				// checkDelegationCeiling now takes permissionID explicitly
				// (upstream #2206); decide() resolves it the same way before
				// step 10, so mirror that here rather than hardcoding a
				// literal permission string.
				permissionID, permErr := resolveResourcePermission(f.resource.Type, ac.action)
				require.NoError(t, permErr)

				makeStore := func() *memoTestStore {
					st := newMemoTestStore(s)
					if variant == "honours" {
						st.fault = cancelHonouringFault
					} else {
						st.ignoreCancel = true
					}
					return st
				}

				// --- Run 1: memo populated on a live ctx, then a
				// pre-cancelled child of the SAME memo ctx. ---
				run1Store := makeStore()
				authz1 := NewAuthzService(run1Store, slog.Default())
				bg := context.Background()
				mctx := withAuthzInputMemo(bg)

				var primeCause DenyCause
				liveCtx := maskAuthzInputs(contextWithDelegationCeilingCache(mctx))
				_, _, primeErr := authz1.checkDelegationCeiling(liveCtx, req, permissionID, f.agentID, nil, &primeCause)
				require.NoError(t, primeErr, "priming call on a live ctx must succeed")
				require.Equal(t, 1, run1Store.countOf("GetDelegationEdgesForDelegate"), "priming call must populate the edges memo")

				cctx, cancel := context.WithCancel(mctx)
				cancel()
				var cause1 DenyCause
				doneCtx := maskAuthzInputs(contextWithDelegationCeilingCache(cctx))
				allowed1, reason1, err1 := authz1.checkDelegationCeiling(doneCtx, req, permissionID, f.agentID, nil, &cause1)

				// Non-vacuity, load-bearing: the second call must still
				// reach the store on the done ctx.
				require.Equal(t, 2, run1Store.countOf("GetDelegationEdgesForDelegate"),
					"the done-ctx call must bypass the memo and reach the store, not be served from it")

				// --- Run 2: the same call, no memo at all, on an
				// independently cancelled ctx with a fresh cache. ---
				run2Store := makeStore()
				authz2 := NewAuthzService(run2Store, slog.Default())
				cctx2, cancel2 := context.WithCancel(context.Background())
				cancel2()
				var cause2 DenyCause
				allowed2, reason2, err2 := authz2.checkDelegationCeiling(maskAuthzInputs(contextWithDelegationCeilingCache(cctx2)), req, permissionID, f.agentID, nil, &cause2)

				assert.Equal(t, allowed1, allowed2, "Allowed must match")
				assert.Equal(t, reason1, reason2, "Reason must match")
				assert.Equal(t, cause1, cause2, "DenyCause must match")
				switch {
				case err1 == nil && err2 == nil:
					// both nil, fine
				case err1 != nil && err2 != nil:
					assert.Equal(t, err1.Error(), err2.Error(), "error text must match")
					if variant == "honours" {
						assert.True(t, errors.Is(err1, context.Canceled), "run 1 error must be context.Canceled")
						assert.True(t, errors.Is(err2, context.Canceled), "run 2 error must be context.Canceled")
					}
				default:
					t.Fatalf("error-ness mismatch: err1=%v err2=%v", err1, err2)
				}

				if variant == "honours" {
					// Upstream #2206 removed the read-only fail-open skip
					// from the edge-lookup error path entirely: a store
					// error now fails closed uniformly for every action,
					// read included. Both branches assert the same shape;
					// kept separate (rather than collapsed) so a future
					// read-only exception, if one is ever reintroduced,
					// fails exactly one of these two assertions and not the
					// other.
					if ac.name == "read" {
						assert.False(t, allowed1, "read must fail closed on the store error (no read-only skip exists post-#2206)")
						assert.Contains(t, reason1, "delegation ceiling check failed (fail-closed):")
						assert.Error(t, err1)
					} else {
						assert.False(t, allowed1, "write must fail closed on the store error")
						assert.Contains(t, reason1, "delegation ceiling check failed (fail-closed):")
						assert.Error(t, err1)
					}
				}
			})
		}
	}
}

// =============================================================================
// X7: REQUIRED non-waivable security gate
// =============================================================================

// x7Fixture builds the three delegator-visibility branches X7's fixture
// requires:
//  1. a1: the A7/A1 delegator — a non-admin user with an in-scope active
//     project binding, so getEffectivePermissions actually loads constraints;
//  2. chainAgent: a depth-1 agent-to-agent chain (chainAgent -> midAgent ->
//     the same user), covering checkAgentHoldsPermission (GetAgent);
//  3. noEdgeAgent: an agent with no delegation edge at all, covering
//     backfillCompleted (GetHubSetting). authzTestSetup's testServer removes
//     the backfill marker, so this is the pre-backfill temporary-allow path.
type x7Fixture struct {
	a1         *a1Fixture
	chainAgent AgentIdentity
	chainRes   Resource
	noEdgeAg   AgentIdentity
	noEdgeRes  Resource
}

func newX7Fixture(t *testing.T, s store.Store, name string) *x7Fixture {
	t.Helper()
	a1 := newA1Fixture(t, s, name+"-a1")

	midAgentID := tid(name + "-mid-agent")
	chainAgentID := tid(name + "-chain-agent")
	chainTargetID := tid(name + "-chain-target")
	createDCAgent(t, s, midAgentID, a1.projectID, a1.delegatorID, AgentRoleFull)
	createDCAgent(t, s, chainAgentID, a1.projectID, a1.delegatorID, AgentRoleFull)
	createDCAgent(t, s, chainTargetID, a1.projectID, a1.delegatorID, AgentRoleFull)
	createDCEdge(t, s, store.DelegationPrincipalUser, a1.delegatorID, store.DelegationPrincipalAgent, midAgentID,
		store.RoleScopeProject, a1.projectID, string(AgentRoleFull))
	createDCEdge(t, s, store.DelegationPrincipalAgent, midAgentID, store.DelegationPrincipalAgent, chainAgentID,
		store.RoleScopeProject, a1.projectID, string(AgentRoleFull))

	noEdgeAgentID := tid(name + "-no-edge-agent")
	noEdgeTargetID := tid(name + "-no-edge-target")
	createDCAgent(t, s, noEdgeAgentID, a1.projectID, a1.delegatorID, AgentRoleFull)
	createDCAgent(t, s, noEdgeTargetID, a1.projectID, a1.delegatorID, AgentRoleFull)

	return &x7Fixture{
		a1:         a1,
		chainAgent: dcAgentIdentity(chainAgentID, a1.projectID, AgentRoleFull),
		chainRes:   Resource{Type: "agent", ID: chainTargetID, ParentType: "project", ParentID: a1.projectID, OwnerID: a1.delegatorID},
		noEdgeAg:   dcAgentIdentity(noEdgeAgentID, a1.projectID, AgentRoleFull),
		noEdgeRes:  Resource{Type: "agent", ID: noEdgeTargetID, ParentType: "project", ParentID: a1.projectID, OwnerID: a1.delegatorID},
	}
}

// x7DecisionTuple pairs a tuple with the identity that must evaluate it, so
// the three fixture branches can be walked in one ordered sequence.
type x7DecisionTuple struct {
	identity Identity
	resource Resource
	action   Action
}

// tuples returns one read tuple and one write tuple per delegate. The read
// tuple targets the shared project (project.read, AgentScopes:
// ["project:read"]) rather than the agent resource, because agent.read has
// no AgentScopes entry at all — an agent can never pass the AK1 kernel for
// it via JWT scope, and none of these targets is owned by or ancestor-linked
// to its agent, so no relationship grant covers it either (see
// agentGrantableTuples). The write tuple uses agent.delete (AgentScopes:
// ["project:agent:lifecycle"]), which AgentRoleFull does hold.
func (f *x7Fixture) tuples() []x7DecisionTuple {
	projectRes := Resource{Type: "project", ID: f.a1.projectID, OwnerID: f.a1.delegatorID}
	var out []x7DecisionTuple
	for _, d := range []struct {
		identity Identity
		writeRes Resource
	}{
		{f.a1.agent, f.a1.resource},
		{f.chainAgent, f.chainRes},
		{f.noEdgeAg, f.noEdgeRes},
	} {
		out = append(out, x7DecisionTuple{d.identity, projectRes, ActionRead})
		out = append(out, x7DecisionTuple{d.identity, d.writeRes, ActionDelete})
	}
	return out
}

// x7NonEdgeServedMethods are the delegator-side methods X7's count clause
// covers: every predicate-true (ceiling-active) call except the edges load,
// which is checked separately by (c).
var x7NonEdgeServedMethods = map[string]bool{
	"GetEffectiveGroups":            true,
	"GetEffectiveGroupsForAgent":    true,
	"ListRoleBindingsForPrincipals": true,
	"GetRoleDefinition":             true,
	"ListAccessConstraints":         true,
	"GetUser":                       true,
	"GetAgent":                      true,
	"GetHubSetting":                 true,
}

// perDecisionCeilingCounts runs identity/resource/action tuples one at a
// time through CheckAccess on a fresh recording AuthzService, and returns,
// for each decision, the count of predicate-true (ceiling-active) calls to
// each x7NonEdgeServedMethods method, plus the total GetDelegationEdgesForDelegate
// count for the whole phase.
func perDecisionCeilingCounts(t *testing.T, st *memoTestStore, ctx context.Context, tuples []x7DecisionTuple) (perDecision []map[string]int, edgesTotal int, allNilMemoUnderCeiling bool) {
	t.Helper()
	st.recordCalls = true
	authz, _ := newRecordingAuthz(st)
	prevLen := 0
	allNilMemoUnderCeiling = true
	for _, tp := range tuples {
		authz.CheckAccess(ctx, tp.identity, tp.resource, tp.action)
		all := st.recordedCalls()
		decisionCalls := all[prevLen:]
		prevLen = len(all)

		counts := map[string]int{}
		for _, rec := range decisionCalls {
			if !rec.ceilingActive {
				continue
			}
			// Only the INPUT memo (inputsKey) is masked under the ceiling;
			// the edges key is deliberately left visible to every ceiling
			// call (maskAuthzInputs only masks inputsKey), so
			// hasEdgesMemo == true is expected everywhere under the ceiling
			// and is not itself evidence of a leak. Only GetDelegationEdgesForDelegate
			// actually reads it; every other method never consults it even
			// though it is technically visible in the ctx.
			if rec.hasInputMemo {
				allNilMemoUnderCeiling = false
			}
			if rec.method == "GetDelegationEdgesForDelegate" {
				continue
			}
			if x7NonEdgeServedMethods[rec.method] {
				counts[rec.method]++
			}
		}
		perDecision = append(perDecision, counts)
	}
	edgesTotal = st.countOf("GetDelegationEdgesForDelegate")
	return
}

// TestParity_X7_CeilingSeesNoMemoExceptEdges is row X7, a required
// non-waivable security gate: the delegation ceiling's own loads (excluding
// edges) must never see the principal/constraint memo, and their
// per-decision call counts must be identical to the reference's — the
// proof that loadAllAccessConstraints changing does not leak into the
// delegator path, because the ceiling ctx is masked.
//
// The fixture's user-delegator branch (a1) exercises the delegator-side
// callees (resolveUserDelegatorAuthority, evaluateUserDelegatorAuthority,
// userRelationshipAuthority on its relationship-grant fallback)
// generically — x7NonEdgeServedMethods tracks every store method they
// call (GetUser, GetEffectiveGroups, ListRoleBindingsForPrincipals,
// GetRoleDefinition, ListAccessConstraints), regardless of which internal
// function makes the call, so this count clause does not need to name
// them individually. What this test CANNOT cover, by construction, is
// authz_relationship_rules.go's relationshipSourceDelegationHolds: that is
// a SEPARATE, UNMASKED caller of the chain-walk, reached from decide's
// step 9 (before step 10's mask even applies), not from
// checkDelegationCeiling — see maskAuthzInputs's doc comment for the full
// analysis. It is dormant today (no production install site exists for
// this memo at all), not something X7's masked-ceiling-focused count
// clause can exercise.
func TestParity_X7_CeilingSeesNoMemoExceptEdges(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newX7Fixture(t, s, "x7")
	tuples := f.tuples()
	ctx := context.Background()

	refStore := newMemoTestStore(s)
	refCounts, _, refNilOK := perDecisionCeilingCounts(t, refStore, ctx, tuples)

	candStore := newMemoTestStore(s)
	candCounts, candEdgesTotal, candNilOK := perDecisionCeilingCounts(t, candStore, withAuthzInputMemo(ctx), tuples)

	assert.True(t, refNilOK, "sanity: the reference (no memo present at all) trivially sees no memo under the ceiling")
	assert.True(t, candNilOK, "(b) every ceiling call except edges must observe a nil input memo, and the edges call must not observe the input memo either")

	require.Equal(t, len(refCounts), len(candCounts))
	for i := range refCounts {
		assert.Equal(t, refCounts[i], candCounts[i], "(a) decision %d: predicate-true call counts (excluding edges) must match the reference", i)
	}

	// (c) GetDelegationEdgesForDelegate issued once per delegate per phase:
	// four distinct delegates are ever looked up — a1's agent, chainAgent,
	// the depth-1 midAgent the chain recurses into, and noEdgeAgent — each
	// evaluated across 2 actions, so a working memo issues exactly 4 calls
	// total, not 8. The no-edge agent's empty result is still memoized
	// (presence is tracked separately from value), so it contributes
	// exactly one lookup, not one per action.
	assert.Equal(t, 4, candEdgesTotal, "edges loaded once per delegate per phase: 4 distinct delegates (a1 agent, chain agent, mid agent, no-edge agent)")

	// H2: each fixture branch must actually be reached in the reference —
	// GetUser (a1's direct user delegator), GetAgent (the agent-to-agent
	// chain) and GetHubSetting (the no-edge, pre-backfill path) must all
	// appear somewhere in the reference's per-decision counts.
	var sawGetUser, sawGetAgent, sawGetHubSetting bool
	for _, c := range refCounts {
		if c["GetUser"] > 0 {
			sawGetUser = true
		}
		if c["GetAgent"] > 0 {
			sawGetAgent = true
		}
		if c["GetHubSetting"] > 0 {
			sawGetHubSetting = true
		}
	}
	assert.True(t, sawGetUser, "H2: the direct user-delegator branch must be reached")
	assert.True(t, sawGetAgent, "H2: the agent-to-agent chain branch must be reached")
	assert.True(t, sawGetHubSetting, "H2: the no-edge/backfill branch must be reached")

	// Also pin non-vacuity for the memo-SERVED loads themselves
	// (GetEffectiveGroups[ForAgent], ListRoleBindingsForPrincipals,
	// ListAccessConstraints) — count-equality between ref and cand for
	// these is clause (a)'s whole point, and it is vacuous if the reference
	// never issues them in the first place.
	var sawPredicateTrueConstraints, sawClosureLoad bool
	for _, c := range refCounts {
		if c["ListAccessConstraints"] > 0 {
			sawPredicateTrueConstraints = true
		}
		if c["GetEffectiveGroups"] > 0 || c["GetEffectiveGroupsForAgent"] > 0 {
			sawClosureLoad = true
		}
	}
	assert.True(t, sawPredicateTrueConstraints, "H2: clause (a)'s ListAccessConstraints comparison must not be vacuous")
	assert.True(t, sawClosureLoad, "H2: clause (a)'s closure-load comparison must not be vacuous")
}

// =============================================================================
// X rows: memo mechanics (mask stickiness, isolation, immutability, freshness)
// =============================================================================

// TestParity_X6_MaskStickinessUnit is row X6(1): a nested
// withAuthzInputMemo under a mask stays masked; both accessors return nil.
func TestParity_X6_MaskStickinessUnit(t *testing.T) {
	bg := context.Background()
	ctx := maskAllAuthzMemo(withAuthzInputMemo(bg))
	ctx = withAuthzInputMemo(ctx) // must be a no-op: the mask is sticky
	assert.Nil(t, authzInputMemoFromContext(ctx), "inputsKey must stay masked")
	assert.Nil(t, delegationEdgesMemoFromContext(ctx), "edgesKey must stay masked")
}

// TestParity_X6_CanMintSelectorSeesNoOuterMemo is row X6(2):
// CanMintSelector results deep-equal with and without an outer memo, and
// every store call made inside it observes a nil input memo and a nil edges
// memo, with at least one call actually recorded (H2 non-vacuity).
func TestParity_X6_CanMintSelectorSeesNoOuterMemo(t *testing.T) {
	_, s := authzTestSetup(t)
	userID := tid("x6-user")
	projectID := tid("x6-proj")
	createDelegateTestProject(t, s, projectID, "x6-proj", "test")
	createTestUserWithProjectRole(t, s, userID, "x6@test.com", projectID, store.ProjectRoleMember)
	selectors := []string{"project:read", "agent:read"}
	boundary := TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}

	plainStore := newMemoTestStore(s)
	plainAuthz := NewAuthzService(plainStore, slog.Default())
	plainResults, plainErr := plainAuthz.CanMintSelector(context.Background(), activeUserPrincipal(userID), boundary, selectors)
	require.NoError(t, plainErr)

	outerStore := newMemoTestStore(s)
	outerStore.recordCalls = true
	outerAuthz := NewAuthzService(outerStore, slog.Default())
	outerCtx := withAuthzInputMemo(context.Background())
	outerResults, outerErr := outerAuthz.CanMintSelector(outerCtx, activeUserPrincipal(userID), boundary, selectors)
	require.NoError(t, outerErr)

	assert.Equal(t, plainResults, outerResults, "CanMintSelector results must deep-equal with and without an outer memo")

	recorded := outerStore.recordedCalls()
	require.NotEmpty(t, recorded, "H2: at least one store call must be recorded inside CanMintSelector")
	for _, rec := range recorded {
		assert.False(t, rec.hasInputMemo, "call %s must not observe the outer input memo", rec.method)
		assert.False(t, rec.hasEdgesMemo, "call %s must not observe the outer edges memo", rec.method)
	}
}

// TestParity_X1_TwoPrincipalsInterleaved is row X1: two principals
// interleaved in one memo ctx must not leak entries between them.
func TestParity_X1_TwoPrincipalsInterleaved(t *testing.T) {
	_, s := authzTestSetup(t)
	p1 := newP1Fixture(t, s, "x1-p1")
	p2 := newP1Fixture(t, s, "x1-p2")
	ctx := withAuthzInputMemo(context.Background())

	authz, _ := newRecordingAuthz(newMemoTestStore(s))
	// Interleave: p1 action, p2 action, p1 action, p2 action...
	var p1Decisions, p2Decisions []Decision
	for _, a := range ResourceActions["agent"] {
		p1Decisions = append(p1Decisions, authz.CheckAccess(ctx, p1.user, p1.agentRes, a))
		p2Decisions = append(p2Decisions, authz.CheckAccess(ctx, p2.user, p2.agentRes, a))
	}

	// No leakage: re-run each principal alone on a fresh memo and compare.
	freshCtx1 := withAuthzInputMemo(context.Background())
	authzFresh1, _ := newRecordingAuthz(newMemoTestStore(s))
	for i, a := range ResourceActions["agent"] {
		d := authzFresh1.CheckAccess(freshCtx1, p1.user, p1.agentRes, a)
		assertDecisionsEqual(t, d, p1Decisions[i], "p1 tuple %d must be unaffected by interleaving with p2", i)
	}
	freshCtx2 := withAuthzInputMemo(context.Background())
	authzFresh2, _ := newRecordingAuthz(newMemoTestStore(s))
	for i, a := range ResourceActions["agent"] {
		d := authzFresh2.CheckAccess(freshCtx2, p2.user, p2.agentRes, a)
		assertDecisionsEqual(t, d, p2Decisions[i], "p2 tuple %d must be unaffected by interleaving with p1", i)
	}
}

// TestParity_C1_ConstraintDenyOnProject is row C1: an access
// constraint denying the member group's permission on the project excludes
// it in both paths, including on relationship grants; deep-equal.
func TestParity_C1_ConstraintDenyOnProject(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	f := newP1Fixture(t, s, "c1")

	_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:               "c1-deny-agent-read",
		SubjectKind:        store.ConstraintSubjectGroupClosure,
		SubjectGroupID:     &f.groupID,
		ScopeType:          ScopeTypeProject,
		ScopeID:            f.projectID,
		MaximumPermissions: []string{}, // empty allowlist excludes every permission
		CreatedBy:          "test",
	})
	require.NoError(t, err)

	refDecisions, _, _, _ := runParity(t, s, f.user, agentResourceTuples(f.agentRes))
	for i, d := range refDecisions {
		assert.False(t, d.Allowed, "tuple %d must be denied by the access constraint", i)
	}
}

// TestParity_R1_OwnerOnlyNoBinding is row R1: a user with no role
// binding at all, granted only through the resource-owner relationship.
// Deep-equal.
func TestParity_R1_OwnerOnlyNoBinding(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	ownerID := tid("r1-owner")
	agentID := tid("r1-agent")
	projectID := tid("r1-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Slug: "r1-proj", Name: "r1"}))
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: ownerID, Email: "r1@test.com", DisplayName: "r1", Role: "member", Status: "active"}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: agentID, Slug: "r1-agent", Name: "r1-agent", ProjectID: projectID, Phase: "running", OwnerID: ownerID}))
	// No permission-granting binding: an access-only project binding gives
	// the owner relationship the active project access it requires
	// (ptone/scion#2141) without granting any permission itself.
	grantProjectAccessOnly(t, s, ownerID, projectID)
	owner := NewAuthenticatedUser(ownerID, "r1@test.com", "r1", "member", "api")
	res := Resource{Type: "agent", ID: agentID, ParentType: "project", ParentID: projectID, OwnerID: ownerID}

	refDecisions, _, _, _ := runParity(t, s, owner, agentResourceTuples(res))
	var sawOwnerGrant bool
	for _, d := range refDecisions {
		if d.Allowed && d.Reason == "relationship grant: resource owner" {
			sawOwnerGrant = true
		}
	}
	assert.True(t, sawOwnerGrant, "H2: at least one action must be granted through the owner relationship")
}

// TestParity_X3_Immutability is row X3: pinned to the A1 agent
// principal over its own agent resource (step 5b synthetic role) AND a
// global skill resource (step 5b2 synthetic catalog role). The stored
// roleDefs must lack synthetic roles; the map handed back to decide is
// never the stored map, checked on BOTH a miss handle (decision 1) and a
// hit handle (decision 2); the H2 precondition confirms a synthetic grant
// specifically (not just a non-empty grant list); and the stored refs,
// bindings, edges and constraint rows are snapshotted and reconfirmed
// unchanged after running more decisions.
func TestParity_X3_Immutability(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newA1Fixture(t, s, "x3")
	skillRes := Resource{Type: "skill", ID: tid("x3-skill"), ScopeKind: store.SkillScopeGlobal}
	ctx := withAuthzInputMemo(context.Background())
	authz, _ := newRecordingAuthz(newMemoTestStore(s))

	// Give the agent a REAL, non-synthetic role binding so entry.roleDefs
	// is genuinely non-empty, independent of the step 5b/5b2 synthetic
	// roles decide adds to its OWN local copy below. Without this, every
	// mutation-detection check on an EMPTY map is vacuous: before==after==0
	// whether or not RoleDefs() actually clones — an empty map can hide a
	// dropped maps.Clone (both RoleDefs() return paths) completely.
	x3RD := createTestRoleDefinition(t, s, "x3-real-role", store.RoleScopeProject, []string{"project.read"})
	_, err := s.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: x3RD.ID,
		PrincipalType:    store.RoleBindingPrincipalAgent,
		PrincipalID:      f.agentID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// An INERT access constraint — its MaximumPermissions allowlist
	// covers every permission this test ever decides on, for
	// every principal it matters to (the agent's own evaluation AND the
	// delegator's getEffectivePermissions resolution inside the ceiling,
	// which applies the SAME ConstraintSubjectAllPrincipals restriction).
	// This exists purely to make loadAllAccessConstraints's result
	// non-empty, so the constraint-immutability check below has something
	// to snapshot; a constraint missing a used permission ID would turn
	// this agent's own-project lifecycle grant, or the delegator's held
	// lifecycle permission, into an unintended deny.
	_, err = s.CreateAccessConstraint(context.Background(), &store.AccessConstraint{
		Name:               "x3-inert-constraint",
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          ScopeTypeSystem,
		MaximumPermissions: []string{"agent.read", "agent.list", "agent.create", "agent.delete", "agent.attach", "agent.lifecycle", "agent.update", "agent.port_access", "agent.set_message_mode", "agent.stop_all", "agent.message", "project.read", "skill.read", "skill.list"},
		CreatedBy:          "test",
	})
	require.NoError(t, err)

	// H2 precondition: the agent's own-project resource must show the
	// SPECIFIC step-5b synthetic-role grant (MatchedGrant ==
	// "agent-jwt-scope", that step's RoleName in authz.go), and the global
	// skill resource must show the SPECIFIC step-5b2 synthetic catalog-role
	// grant (MatchedGrant == "agent-skill-catalog",
	// authz_skill_scope.go's agentSkillCatalogRoleName) — not merely a
	// non-empty grant list, which could pass via an unrelated grant path.
	explain := func(res Resource, action Action) Decision {
		return authz.Decide(context.Background(), AuthzRequest{
			Principal:  PrincipalContext{Identity: f.agent},
			Credential: credentialContextForIdentity(f.agent),
			Resource:   res,
			Action:     action,
			Explain:    true,
		})
	}
	// agent.read has no AgentScopes entry at all (an agent can never pass
	// the kernel for it via JWT scope — see agentGrantableTuples), so
	// ActionLifecycle (AgentScopes: ["project:agent:lifecycle"], which
	// AgentRoleFull holds) is used to actually exercise step 5b.
	agentExplain := explain(f.resource, ActionLifecycle)
	require.True(t, agentExplain.Allowed, "reason=%q", agentExplain.Reason)
	assert.Equal(t, "agent-jwt-scope", agentExplain.MatchedGrant, "H2: step 5b's SPECIFIC synthetic role must be the matched grant")

	skillExplain := explain(skillRes, ActionRead)
	require.True(t, skillExplain.Allowed, "H2: the global skill catalog read must be allowed: reason=%q", skillExplain.Reason)
	assert.Equal(t, "agent-skill-catalog", skillExplain.MatchedGrant, "H2: step 5b2's SPECIFIC synthetic catalog role must be the matched grant")

	// Decision 1 (agent resource): a miss handle. Capture ITS OWN RoleDefs()
	// return value too (the miss path was previously unchecked).
	in0 := authz.inputsFor(ctx, f.agent)
	_, err = in0.Principals()
	require.NoError(t, err)
	_, err = in0.Bindings()
	require.NoError(t, err)
	rd0, err := in0.RoleDefs() // MISS: this stores the entry's roleDefs
	require.NoError(t, err)

	memo := authzInputMemoFromContext(ctx)
	require.NotNil(t, memo)
	key := principalKey{normType: "agent", id: f.agentID}
	entry := memo.entryFor(key)
	require.True(t, entry.roleDefsOK)
	require.NotEmpty(t, entry.roleDefs, "the fixture must produce a non-empty stored roleDefs map (via x3-real-role above), or the mutation checks below are vacuous on an empty map")

	// Map-IDENTITY check. Unlike "delete a key, compare lengths," this
	// works even on an EMPTY map, because it compares the map header's
	// pointer value directly: if the clone were dropped from BOTH
	// RoleDefs() return paths, rd0 would BE entry.roleDefs — the exact
	// same backing map, same pointer — regardless of how many keys it has.
	assert.NotEqual(t, reflect.ValueOf(entry.roleDefs).Pointer(), reflect.ValueOf(rd0).Pointer(),
		"the MISS path's RoleDefs() must return a DIFFERENT map (a clone), not the stored map itself")

	before0 := len(entry.roleDefs)
	for id := range rd0 {
		delete(rd0, id)
		break
	}
	assert.Equal(t, before0, len(entry.roleDefs), "the MISS path's RoleDefs() must also return a clone; mutating it must not affect the stored map")

	// Decision 2 (conceptually): a hit handle reads the same entry.
	in1 := authz.inputsFor(ctx, f.agent)
	_, err = in1.Principals()
	require.NoError(t, err)
	_, err = in1.Bindings()
	require.NoError(t, err)
	rd1, err := in1.RoleDefs() // HIT
	require.NoError(t, err)

	assert.NotEqual(t, reflect.ValueOf(entry.roleDefs).Pointer(), reflect.ValueOf(rd1).Pointer(),
		"the HIT path's RoleDefs() must return a DIFFERENT map (a clone), not the stored map itself")
	assert.NotEqual(t, reflect.ValueOf(rd0).Pointer(), reflect.ValueOf(rd1).Pointer(),
		"two separate RoleDefs() calls must return two SEPARATE clones, not the same map handed out twice")

	before1 := len(entry.roleDefs)
	for id := range rd1 {
		delete(rd1, id)
		break
	}
	assert.Equal(t, before1, len(entry.roleDefs), "the HIT path's RoleDefs() must also return a clone; mutating it must not affect the stored map")

	// Deep-copy the *RolePermissions POINTEE and its Permissions map, not
	// just the pointer — the same class of hazard as the bindings/edges/
	// constraints snapshots below. A shallow `roleDefsBeforeDecide[id] = rp`
	// snapshot shares the exact same *RolePermissions object (and the
	// exact same Permissions map inside it) with entry.roleDefs; an
	// in-place write to either would silently "mutate" the snapshot too,
	// and the deep-equal below would never catch it. Snapshot BEFORE any
	// decide call runs — the "no synthetic role leaked in" check must run
	// AFTER the decide() calls below, since checking before them could
	// never observe a leak. It is asserted after those calls instead,
	// alongside a full deep-equal against this snapshot.
	deepCopyRoleDefs := func(in map[string]*RolePermissions) map[string]*RolePermissions {
		out := make(map[string]*RolePermissions, len(in))
		for id, rp := range in {
			if rp == nil {
				out[id] = nil
				continue
			}
			cp := *rp
			if rp.Permissions != nil {
				cp.Permissions = make(map[string]struct{}, len(rp.Permissions))
				for p := range rp.Permissions {
					cp.Permissions[p] = struct{}{}
				}
			}
			out[id] = &cp
		}
		return out
	}
	roleDefsBeforeDecide := deepCopyRoleDefs(entry.roleDefs)

	// append(nil, emptySlice...) collapses a non-nil-but-empty slice back to
	// nil (nothing to append), which would make an "unchanged" comparison
	// pass or fail on nilness alone rather than content — copy explicitly
	// so nil-vs-non-nil-empty is preserved faithfully either way.
	//
	// Copying the POINTER SLICE is not enough for bindings, edges and
	// constraints — store.PrincipalRef has no pointer fields, so
	// a shallow element copy of refs IS a deep copy, but *store.RoleBinding,
	// *store.DelegationEdge and *store.AccessConstraint are pointers to
	// mutable structs. A shallow copy means entry.bindings[i] and
	// bindingsSnapshot[i] are the SAME object; an in-place field mutation
	// on the stored binding (e.g. someone writes rb.ScopeID = x in place)
	// would silently also "mutate" the snapshot, and assert.Equal below
	// would never be able to catch it. Dereference and copy the POINTEE so
	// the snapshot is a true, independent copy.
	copyRefs := func(in []store.PrincipalRef) []store.PrincipalRef {
		if in == nil {
			return nil
		}
		out := make([]store.PrincipalRef, len(in))
		copy(out, in)
		return out
	}
	deepCopyBindings := func(in []*store.RoleBinding) []*store.RoleBinding {
		if in == nil {
			return nil
		}
		out := make([]*store.RoleBinding, len(in))
		for i, b := range in {
			if b != nil {
				cp := *b
				out[i] = &cp
			}
		}
		return out
	}
	deepCopyEdges := func(in []*store.DelegationEdge) []*store.DelegationEdge {
		if in == nil {
			return nil
		}
		out := make([]*store.DelegationEdge, len(in))
		for i, e := range in {
			if e != nil {
				cp := *e
				out[i] = &cp
			}
		}
		return out
	}
	deepCopyConstraints := func(in []*store.AccessConstraint) []*store.AccessConstraint {
		if in == nil {
			return nil
		}
		out := make([]*store.AccessConstraint, len(in))
		for i, c := range in {
			if c != nil {
				cp := *c
				out[i] = &cp
			}
		}
		return out
	}

	// Run ONE decision on the memo ctx BEFORE taking the
	// refs/bindings/edges/constraint snapshots, so that edges (step 10,
	// via getCachedDelegationEdges) and the constraint slot (step 7c, via
	// loadAllAccessConstraints) are actually populated when the snapshot is
	// taken. A snapshot taken before any memo-ctx decision ever ran would
	// find memo.edges empty and memo.constraints nil, making the "stored
	// edges/constraints unchanged" checks below vacuous — they would
	// iterate zero times or be skipped entirely, and a mutant that appends
	// to the stored constraint slot on a hit, or mutates a stored
	// *DelegationEdge in place, would survive undetected.
	popDecision := authz.CheckAccess(ctx, f.agent, f.resource, ActionLifecycle)
	require.True(t, popDecision.Allowed, "H2: the populating decision must be allowed via the 5b synthetic grant (and the inert constraint must not have removed it) — reason=%q", popDecision.Reason)
	// Assert Allowed/MatchedGrant on this memo-ctx decision itself, not
	// only on the context.Background() explain calls above — proving the
	// leak checks below follow a decision that actually ran through THIS
	// memo and wrote a synthetic role somewhere reachable from it, not an
	// unrelated one.
	assert.Equal(t, "agent-jwt-scope", popDecision.MatchedGrant, "the populating memo-ctx decision's MatchedGrant must be the 5b synthetic grant, exactly like the context.Background() explain call above")

	refsSnapshot := copyRefs(entry.refs)
	bindingsSnapshot := deepCopyBindings(entry.bindings)
	memo.mu.Lock()
	edgesSnapshot := make(map[string][]*store.DelegationEdge, len(memo.edges))
	for k, v := range memo.edges {
		edgesSnapshot[k] = deepCopyEdges(v)
	}
	var constraintsSnapshot []*store.AccessConstraint
	constraintsWereLoaded := memo.constraints != nil
	if constraintsWereLoaded {
		constraintsSnapshot = deepCopyConstraints(*memo.constraints)
	}
	memo.mu.Unlock()

	// The snapshots above must be non-empty, or the "unchanged after
	// further decisions" checks at the end of this test are vacuous —
	// exactly the gap this test is designed to close. The x3-inert-constraint
	// fixture row (added above) guarantees constraintsWereLoaded; the A1
	// fixture's single delegation edge guarantees edgesSnapshot.
	require.NotEmpty(t, edgesSnapshot, "the edges snapshot must be non-empty after the populating decision, or the 'edges unchanged' check below is vacuous")
	require.True(t, constraintsWereLoaded, "the constraint slot must have been loaded by the populating decision")
	require.NotEmpty(t, constraintsSnapshot, "the constraint snapshot must be non-empty (the x3-inert-constraint fixture row), or the 'constraints unchanged' check below is vacuous")

	// Run several MORE decisions on the same memo (including the skill
	// resource, which exercises a different synthetic-role path), then
	// reconfirm every snapshot taken above is still deep-equal — nothing
	// downstream (kernel evaluation, hub-wide filters, the ceiling)
	// mutates a memoized input in place.
	//
	// The delete decision's outcome here is deterministic, not an "it may
	// or may not hold" shrug — assert it rather than discard it, so it
	// documents that this decision genuinely reaches step 10 on the memo
	// ctx, like popDecision and skillDecision. It is ALLOWED: f.resource's
	// OwnerID is f.delegatorID, and the delegator now holds agent.delete
	// through a relationship-grant fallback (userRelationshipAuthority) on
	// a resource they own, independent of their role bindings. Confirmed
	// deterministic by actually re-running this decision, not inferred.
	delDecision := authz.CheckAccess(ctx, f.agent, f.resource, ActionDelete)
	require.True(t, delDecision.Allowed, "H2: the A1 delegator now holds agent.delete via the owner-relationship fallback")
	assert.Empty(t, delDecision.DenyCause, "an allowed decision carries no DenyCause")
	lifecycleDecision2 := authz.CheckAccess(ctx, f.agent, f.resource, ActionLifecycle)
	skillDecision := authz.CheckAccess(ctx, f.agent, skillRes, ActionRead)

	// Same assertion as popDecision above, on two more memo-ctx decisions
	// — proves the final leak/immutability check below follows decisions
	// that genuinely exercised both the 5b (agent) and 5b2 (skill)
	// synthetic-role paths on this memo, not just the
	// context.Background() explain calls at the top of this test.
	require.True(t, lifecycleDecision2.Allowed, "H2: the second agent-resource memo-ctx decision must also be allowed via the 5b synthetic grant")
	assert.Equal(t, "agent-jwt-scope", lifecycleDecision2.MatchedGrant, "second agent-resource memo-ctx decision's MatchedGrant")
	require.True(t, skillDecision.Allowed, "H2: the skill-resource memo-ctx decision must be allowed via the 5b2 synthetic catalog grant — reason=%q", skillDecision.Reason)
	assert.Equal(t, "agent-skill-catalog", skillDecision.MatchedGrant, "skill-resource memo-ctx decision's MatchedGrant")

	// The synthetic roles decide writes into its OWN local clone (step
	// 5b/5b2) must NOT have leaked into the stored map. Checked AFTER every
	// decide() call above, not before — checking before could never have
	// observed a leak. The deep equal against the before-decide snapshot is
	// the stronger of the two checks (it also catches any OTHER stored-map
	// mutation, not just a "synthetic" substring), but both are kept: the
	// substring check gives a readable failure naming the exact leaked key.
	for id, rp := range entry.roleDefs {
		assert.NotContains(t, id, "synthetic", "stored roleDefs must not contain a synthetic role id %q: %+v", id, rp)
	}
	assert.Equal(t, roleDefsBeforeDecide, entry.roleDefs, "stored roleDefs must be unchanged after the memo-ctx decisions above (including one on the skill resource, which exercises the 5b2 synthetic-role path) — if this fails, decide's synthetic-role write escaped its own local clone into the memo's stored map")

	assert.Equal(t, refsSnapshot, entry.refs, "stored refs must be unchanged after further decisions")
	assert.Equal(t, bindingsSnapshot, entry.bindings, "stored bindings must be unchanged after further decisions")
	memo.mu.Lock()
	for k, v := range edgesSnapshot {
		assert.Equal(t, v, memo.edges[k], "stored edges for %q must be unchanged after further decisions", k)
	}
	require.NotNil(t, memo.constraints)
	assert.Equal(t, constraintsSnapshot, *memo.constraints, "stored constraint rows must be unchanged after further decisions")
	memo.mu.Unlock()
}

// =============================================================================
// T rows: resource-type coverage, a representative subset
// =============================================================================

// resourceTypeTuples builds one tuple per ResourceActions[res.Type].
func resourceTypeTuples(res Resource) []rawTuple {
	out := make([]rawTuple, 0, len(ResourceActions[res.Type]))
	for _, a := range ResourceActions[res.Type] {
		out = append(out, rawTuple{res, a})
	}
	return out
}

// TestParity_T1_Project is row T1: a parentless project resource,
// exercising the UAT gate's project-resource shape. Deep-equal.
func TestParity_T1_Project(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "t1")
	res := Resource{Type: "project", ID: f.projectID}
	runParity(t, s, f.user, resourceTypeTuples(res))
}

// TestParity_T4_SkillProgeny is row T4: an agent reads its origin
// user's personal (user-scoped) skill through the progeny relationship. The
// skill fact itself is pure; the store call is stage-4 relationshipSourceActive
// GetUser, which stays per decision even under the memo.
func TestParity_T4_SkillProgeny(t *testing.T) {
	gf := newGoldenFixture(t)
	// Upstream added an execution-project admission stage
	// (authz_execution_project.go) to every progeny-sourced
	// relationship grant for an agent principal. It requires a REAL stored
	// store.Agent row (not just an in-memory JWT identity) and a resolvable
	// delegation-edge chain to an active user holding live admission to the
	// agent's OWN project — independent of the progeny sharing-source
	// relationship itself (gf.projectOwnerID, below, via Ancestry). Without
	// this, the stage denies with "relationship grant restricted by
	// execution_project" before the progeny grant is even evaluated.
	agentID := tid("t4-skill-agent")
	betaMemberID := tid("t4-beta-member")
	createDCUser(t, gf.store, betaMemberID, "t4-beta-member@test.com", gf.projectBeta.ID, store.ProjectRoleAdmin)
	require.NoError(t, gf.store.CreateAgent(context.Background(), &store.Agent{
		ID: agentID, Slug: "t4-skill-agent", Name: "t4-skill-agent",
		ProjectID: gf.projectBeta.ID, Phase: "running",
		OwnerID: betaMemberID, Ancestry: []string{gf.projectOwnerID},
	}))
	createDCEdge(t, gf.store, store.DelegationPrincipalUser, betaMemberID, store.DelegationPrincipalAgent, agentID, store.RoleScopeProject, gf.projectBeta.ID, string(AgentRoleFull))
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: gf.projectBeta.ID,
		Ancestry:  []string{gf.projectOwnerID},
		Scopes:    allRegisteredAgentScopes(),
	}}
	skillRes := Resource{Type: "skill", ID: gf.skillInjectionID, ScopeKind: store.SkillScopeUser, ScopeUserID: gf.projectOwnerID}

	ctx := context.Background()
	refStore := newMemoTestStore(gf.store)
	refAuthz, refEmit := newRecordingAuthz(refStore)
	refDecision := refAuthz.CheckAccess(ctx, agent, skillRes, ActionRead)
	require.True(t, refDecision.Allowed, "reference progeny skill read must be allowed: %q", refDecision.Reason)

	candStore := newMemoTestStore(gf.store)
	candAuthz, candEmit := newRecordingAuthz(candStore)
	candDecision := candAuthz.CheckAccess(withAuthzInputMemo(ctx), agent, skillRes, ActionRead)

	assertDecisionsEqual(t, refDecision, candDecision)
	assertAuditSequenceEqual(t, refEmit.snapshot(), candEmit.snapshot())
}

// TestParity_T5_SecretProgenyRawLoop is row T5: raw Decide with an
// explicit Permission ("project.secret_read"), the shape httpdispatcher
// uses. secret has no capability action, so it is unreachable through the
// batch. Progeny facts make store calls and stay per decision.
func TestParity_T5_SecretProgenyRawLoop(t *testing.T) {
	gf := newGoldenFixture(t)
	// See TestParity_T4_SkillProgeny's comment: the execution-project
	// admission stage needs a real stored agent row plus a resolvable
	// delegation-edge chain to an active, admitted project-Beta user,
	// independent of the progeny sharing-source relationship
	// (gf.projectOwnerID, via Ancestry). project.secret_read requires
	// recorded provenance on every delegation hop, so the allow path seeds
	// a recorded edge; the sibling agent below carries an unrecorded edge
	// and is denied.
	betaMemberID := tid("t5-beta-member")
	createDCUser(t, gf.store, betaMemberID, "t5-beta-member@test.com", gf.projectBeta.ID, store.ProjectRoleAdmin)
	newAgent := func(slug string, seedEdge func(t *testing.T, s store.Store, delegatorType, delegatorID, delegateType, delegateID, scopeType, scopeID, role string)) AgentIdentity {
		agentID := tid(slug)
		require.NoError(t, gf.store.CreateAgent(context.Background(), &store.Agent{
			ID: agentID, Slug: slug, Name: slug,
			ProjectID: gf.projectBeta.ID, Phase: "running",
			OwnerID: betaMemberID, Ancestry: []string{gf.projectOwnerID},
		}))
		seedEdge(t, gf.store, store.DelegationPrincipalUser, betaMemberID, store.DelegationPrincipalAgent, agentID, store.RoleScopeProject, gf.projectBeta.ID, string(AgentRoleFull))
		return &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: agentID},
			ProjectID: gf.projectBeta.ID,
			Ancestry:  []string{gf.projectOwnerID},
			Scopes:    allRegisteredAgentScopes(),
		}}
	}
	secretRes := Resource{Type: "secret", ID: gf.secretID}
	p := permissions.Permission{ID: "project.secret_read", Action: string(ActionRead)}
	ctx := context.Background()
	decidePair := func(agent AgentIdentity) (Decision, Decision) {
		refStore := newMemoTestStore(gf.store)
		refAuthz, refEmit := newRecordingAuthz(refStore)
		refDecision := decideExplicit(t, refAuthz, agent, secretRes, p)

		candStore := newMemoTestStore(gf.store)
		candAuthz, candEmit := newRecordingAuthz(candStore)
		candDecision := candAuthz.Decide(withAuthzInputMemo(ctx), AuthzRequest{
			Principal:  principalContextForIdentity(agent),
			Credential: credentialContextForIdentity(agent),
			Resource:   secretRes,
			Action:     ActionRead,
			Permission: p.ID,
		})

		assertDecisionsEqual(t, refDecision, candDecision, "memo-ctx candidate must match the reference")
		assertAuditSequenceEqual(t, refEmit.snapshot(), candEmit.snapshot())
		return refDecision, candDecision
	}

	refDecision, _ := decidePair(newAgent("t5-secret-agent", seedRecordedDelegationEdge))
	require.True(t, refDecision.Allowed, "reference progeny secret read must be allowed: %q", refDecision.Reason)

	// The same row through an unrecorded edge is denied at the hop ceiling,
	// identically on both sides.
	unrecRef, unrecCand := decidePair(newAgent("t5-secret-agent-unrecorded", createDCEdge))
	assert.False(t, unrecRef.Allowed, "reference progeny secret read through an unrecorded edge must be denied")
	assert.Equal(t, DenyCauseCeilingUnrecorded, unrecRef.DenyCause, "reference deny cause: reason=%q", unrecRef.Reason)
	assert.Contains(t, unrecRef.Reason, "project.secret_read requires recorded provenance")
	assert.Equal(t, DenyCauseCeilingUnrecorded, unrecCand.DenyCause, "candidate deny cause: reason=%q", unrecCand.Reason)
}

// TestParity_T8_SecretUseRuntimeRow is row T8: raw Decide,
// secret.use (ActionUse), agent with scope project:secret:read. secret.use
// has no CapabilityKind, so it is unreachable through the batch. use is not
// a read-only action, so an edge error on this permission fails closed
// exactly like a write.
func TestParity_T8_SecretUseRuntimeRow(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newA1Fixture(t, s, "t8")
	secretID := tid("t8-secret")
	require.NoError(t, s.CreateSecret(context.Background(), &store.Secret{
		ID: secretID, Key: "t8-secret", Scope: "user", ScopeID: f.delegatorID, AllowProgeny: true, CreatedBy: f.delegatorID,
	}))
	secretRes := Resource{Type: "secret", ID: secretID}
	p := permissions.Permission{ID: "secret.use", Action: string(ActionUse)}

	// The agent's ancestry must include the secret owner for the progeny
	// grant to admit secret.use. The execution-project admission stage
	// also needs a real stored agent row and a resolvable delegation-edge
	// chain to an active, admitted user —
	// f.delegatorID (the A1 fixture's project-admin) already satisfies
	// "admitted," so the agent row and edge need adding here. That edge
	// ALSO makes this agent reachable at step 10's ceiling for the first
	// time (previously, with no edge at all, a pre-backfill temporary
	// allow masked the ceiling check entirely — authzTestSetup's
	// testServer removes the backfill marker). secret.use is an
	// agent-scoped permission (AgentScopes: ["project:secret:read"], no
	// UATScope), so no standard seeded user role grants it; f.delegatorID
	// needs an explicit role binding for it, or the ceiling denies "the
	// delegator does not hold secret.use" even though the progeny grant
	// itself succeeds. secret.use also requires recorded provenance on
	// every delegation hop, so the allow path seeds a recorded edge; the
	// sibling agent below carries an unrecorded edge and is denied.
	t8RD := createTestRoleDefinition(t, s, "t8-secret-use-role", store.RoleScopeProject, []string{"secret.use"})
	_, err := s.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: t8RD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.delegatorID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	newUseAgent := func(slug string, seedEdge func(t *testing.T, s store.Store, delegatorType, delegatorID, delegateType, delegateID, scopeType, scopeID, role string)) AgentIdentity {
		useAgentID := tid(slug)
		createDCAgent(t, s, useAgentID, f.projectID, f.delegatorID, AgentRoleFull)
		seedEdge(t, s, store.DelegationPrincipalUser, f.delegatorID, store.DelegationPrincipalAgent, useAgentID, store.RoleScopeProject, f.projectID, string(AgentRoleFull))
		return &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: useAgentID},
			ProjectID: f.projectID,
			Ancestry:  []string{f.delegatorID},
			Scopes:    allRegisteredAgentScopes(),
		}}
	}

	ctx := context.Background()
	decidePair := func(useAgent AgentIdentity) (Decision, Decision) {
		refStore := newMemoTestStore(s)
		refAuthz, refEmit := newRecordingAuthz(refStore)
		refDecision := decideExplicit(t, refAuthz, useAgent, secretRes, p)
		require.Len(t, refEmit.snapshot(), 1, "audit invariant: exactly one audit record per decision (reference)")

		candStore := newMemoTestStore(s)
		candAuthz, candEmit := newRecordingAuthz(candStore)
		mctx := withAuthzInputMemo(ctx)
		candDecision := candAuthz.Decide(mctx, AuthzRequest{
			Principal:  principalContextForIdentity(useAgent),
			Credential: credentialContextForIdentity(useAgent),
			Resource:   secretRes,
			Action:     ActionUse,
			Permission: p.ID,
		})
		require.Len(t, candEmit.snapshot(), 1, "audit invariant: exactly one audit record per decision (candidate)")

		// Compare the audit sequence, not just the Decision.
		assertAuditSequenceEqual(t, refEmit.snapshot(), candEmit.snapshot())
		assertDecisionsEqual(t, refDecision, candDecision)
		return refDecision, candDecision
	}

	refDecision, _ := decidePair(newUseAgent("t8-use-agent", seedRecordedDelegationEdge))
	// H2: the progeny grant must actually be what allowed secret.use,
	// not some other path (e.g. a vacuously-passing deny on both sides).
	require.True(t, refDecision.Allowed, "reference secret.use must be allowed via the progeny grant: reason=%q", refDecision.Reason)
	assert.Contains(t, refDecision.Reason, "progeny", "H2: the reference Reason must show the progeny grant")
	assert.Contains(t, refDecision.Reason, "relationship grant", "H2: the reference Reason must show a relationship grant, not a role binding")

	// The same row through an unrecorded edge is denied at the hop ceiling,
	// identically on both sides.
	unrecRef, unrecCand := decidePair(newUseAgent("t8-use-agent-unrecorded", createDCEdge))
	assert.False(t, unrecRef.Allowed, "reference secret.use through an unrecorded edge must be denied")
	assert.Equal(t, DenyCauseCeilingUnrecorded, unrecRef.DenyCause, "reference deny cause: reason=%q", unrecRef.Reason)
	assert.Contains(t, unrecRef.Reason, "secret.use requires recorded provenance")
	assert.Equal(t, DenyCauseCeilingUnrecorded, unrecCand.DenyCause, "candidate deny cause: reason=%q", unrecCand.Reason)
}

// =============================================================================
// X2/X2b/X2c: concurrency and the deterministic lost race (-race)
// =============================================================================

// TestParity_X2_ConcurrentRequestsIsolated is row X2: concurrent
// requests with separate contexts (and separate memos) never leak into each
// other. Run with -race.
func TestParity_X2_ConcurrentRequestsIsolated(t *testing.T) {
	_, s := authzTestSetup(t)
	f1 := newP1Fixture(t, s, "x2-1")
	f2 := newP1Fixture(t, s, "x2-2")
	authz, _ := newRecordingAuthz(newMemoTestStore(s))

	var wg sync.WaitGroup
	run := func(f *p1Fixture) {
		defer wg.Done()
		ctx := withAuthzInputMemo(context.Background())
		for i := 0; i < 20; i++ {
			for _, a := range ResourceActions["agent"] {
				authz.CheckAccess(ctx, f.user, f.agentRes, a)
			}
		}
	}
	wg.Add(2)
	go run(f1)
	go run(f2)
	wg.Wait()
}

// TestParity_X2b_ConcurrentGoroutinesOneMemo is row X2b: N goroutines
// share ONE memo ctx, calling CheckAccess for mixed principals against a
// stable store. No race, and every decision deep-equals the reference. Run
// with -race.
func TestParity_X2b_ConcurrentGoroutinesOneMemo(t *testing.T) {
	_, s := authzTestSetup(t)
	principals := []*p1Fixture{
		newP1Fixture(t, s, "x2b-1"),
		newP1Fixture(t, s, "x2b-2"),
		newP1Fixture(t, s, "x2b-3"),
	}

	// Reference decisions, computed sequentially with no memo.
	refAuthz, _ := newRecordingAuthz(newMemoTestStore(s))
	refCtx := context.Background()
	want := make(map[string]Decision)
	keyFor := func(fi int, action Action) string { return fmt.Sprintf("%d/%s", fi, action) }
	for fi, f := range principals {
		for _, a := range ResourceActions["agent"] {
			want[keyFor(fi, a)] = refAuthz.CheckAccess(refCtx, f.user, f.agentRes, a)
		}
	}

	candAuthz, _ := newRecordingAuthz(newMemoTestStore(s))
	mctx := withAuthzInputMemo(context.Background())
	var mu sync.Mutex
	got := make(map[string]Decision)
	var wg sync.WaitGroup
	for fi, f := range principals {
		fi, f := fi, f
		for _, a := range ResourceActions["agent"] {
			a := a
			wg.Add(1)
			go func() {
				defer wg.Done()
				d := candAuthz.CheckAccess(mctx, f.user, f.agentRes, a)
				mu.Lock()
				got[keyFor(fi, a)] = d
				mu.Unlock()
			}()
		}
	}
	wg.Wait()

	for k, w := range want {
		assertDecisionsEqual(t, w, got[k], "key %s", k)
	}
}

// blockOnceStore blocks the first GetEffectiveGroups call for a specific
// user until release is closed, then answers with overrideGroups instead of
// delegating to the real store — used by X2c to force a deterministic lost
// race between two handles for the same principal.
type blockOnceStore struct {
	*memoTestStore
	blockUserID    string
	release        chan struct{}
	overrideGroups []string
	blocked        chan struct{} // closed once the blocked call is parked, waiting on release
}

func (b *blockOnceStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	if userID == b.blockUserID && b.release != nil {
		release := b.release
		b.release = nil // only the first call blocks
		if b.blocked != nil {
			close(b.blocked)
		}
		<-release
		if err := b.call(ctx, "GetEffectiveGroups"); err != nil {
			return nil, err
		}
		return b.overrideGroups, nil
	}
	if err := b.call(ctx, "GetEffectiveGroups"); err != nil {
		return nil, err
	}
	return b.Store.GetEffectiveGroups(ctx, userID)
}

// TestParity_X2c_DeterministicLostRace is row X2c: two handles, hA
// and hB, for the same principal on one memo. hB's GetEffectiveGroups is
// blocked on a channel; hA loads closure A and stores it first; hB is then
// released with closure B != A. Both orderings (o1: hB first, o2: hA first)
// must show hB detached: it queries bindings/roleDefs with its OWN refs B
// and stores nothing, while the entry ends holding A's values throughout.
func TestParity_X2c_DeterministicLostRace(t *testing.T) {
	for _, order := range []string{"o1_detached_first", "o2_attached_first"} {
		t.Run(order, func(t *testing.T) {
			_, s := authzTestSetup(t)
			f := newP1Fixture(t, s, "x2c-"+order)

			// A second, disjoint group for the SAME user, so a closure built
			// from it ("closure B") differs from the real one ("closure A").
			groupB := &store.Group{ID: tid("x2c-" + order + "-groupB"), Name: "B", Slug: "x2c-" + order + "-groupB", GroupType: store.GroupTypeExplicit}
			require.NoError(t, s.CreateGroup(context.Background(), groupB))

			release := make(chan struct{})
			blocked := make(chan struct{})
			bs := &blockOnceStore{
				memoTestStore:  newMemoTestStore(s),
				blockUserID:    f.userID,
				release:        release,
				overrideGroups: []string{groupB.ID},
				blocked:        blocked,
			}
			authz := NewAuthzService(bs, slog.Default())
			mctx := withAuthzInputMemo(context.Background())

			hA := authz.inputsFor(mctx, f.user)
			hB := authz.inputsFor(mctx, f.user)

			var hBDone = make(chan struct{})
			go func() {
				defer close(hBDone)
				<-blocked // wait until hB's GetEffectiveGroups call has parked
				// hA proceeds and stores closure A while hB is still blocked.
				// require.* calls FailNow, which is unsupported from a
				// non-test goroutine (the testing package panics/corrupts
				// state) — use assert.* here and let the main goroutine's
				// own assertions below catch any resulting inconsistency.
				_, err := hA.Principals()
				assert.NoError(t, err)
				if order == "o2_attached_first" {
					_, err = hA.Bindings()
					assert.NoError(t, err)
					_, err = hA.RoleDefs()
					assert.NoError(t, err)
				}
				close(release) // release hB with closure B
			}()

			_, errB := hB.Principals()
			require.NoError(t, errB)
			<-hBDone

			// hB is detached: it must not consume the entry's bindings slot.
			bindingsB, err := hB.Bindings()
			require.NoError(t, err)
			_, err = hB.RoleDefs()
			require.NoError(t, err)

			memo := authzInputMemoFromContext(mctx)
			key := principalKey{normType: "user", id: f.userID}
			entry := memo.entryFor(key)
			require.True(t, entry.refsOK)
			// The entry must hold A's refs (the real group), not B's.
			var entryHasGroupB bool
			for _, r := range entry.refs {
				if r.ID == groupB.ID {
					entryHasGroupB = true
				}
			}
			assert.False(t, entryHasGroupB, "the entry must hold closure A, not the detached handle's closure B")

			// hB's own Bindings() call must have queried with refs B (user +
			// groupB), which has no role binding at all — unlike refs A's
			// real group, which does (via p1Fixture's setup). An empty
			// result is therefore proof hB queried with B, not a silent
			// reuse of the entry's bindings(A).
			assert.Empty(t, bindingsB, "hB must query with its own refs B (no binding), not consume the entry's bindings(A)")
		})
	}
}

// =============================================================================
// X4/X5: freshness across separately-installed memos
// =============================================================================

// TestParity_X4_CrossRequestFreshness is row X4: a binding removed
// between two separate memo ctxs (two independent "requests") must be
// reflected in the second one — the memo never outlives its ctx.
func TestParity_X4_CrossRequestFreshness(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newProjectPrincipalFixture(t, s, "x4", store.ProjectRoleOwner)
	authz, _ := newRecordingAuthz(newMemoTestStore(s))

	ctx1 := withAuthzInputMemo(context.Background())
	d1 := authz.CheckAccess(ctx1, f.user, f.agentRes, ActionDelete)
	require.True(t, d1.Allowed, "H2: project admin must initially be allowed to delete")

	bg := context.Background()
	bindings, err := s.ListRoleBindingsForPrincipals(bg, []store.PrincipalRef{{Type: "user", ID: f.userID}}, nil, nil)
	require.NoError(t, err)
	require.NotEmpty(t, bindings)
	for _, b := range bindings {
		require.NoError(t, s.DeleteRoleBinding(bg, b.ID))
	}

	ctx2 := withAuthzInputMemo(context.Background()) // a separate memo/request
	d2 := authz.CheckAccess(ctx2, f.user, f.agentRes, ActionDelete)
	assert.False(t, d2.Allowed, "request 2's own fresh memo must reflect the removed binding")
}

// TestParity_X5_BatchLevelInstallIsPerCall is row X5: each
// installation is scoped to one call; a binding added between two calls is
// visible to the second call's own fresh memo.
func TestParity_X5_BatchLevelInstallIsPerCall(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "x5")
	authz, _ := newRecordingAuthz(newMemoTestStore(s))

	caps1 := authz.ComputeCapabilities(withAuthzInputMemo(context.Background()), f.user, f.agentRes)
	require.NotContains(t, caps1.Actions, string(ActionDelete), "H2: an ordinary member must not start with delete")

	createDCUser(t, s, f.userID, "x5@test.com", f.projectID, store.ProjectRoleOwner)

	caps2 := authz.ComputeCapabilities(withAuthzInputMemo(context.Background()), f.user, f.agentRes)
	assert.Contains(t, caps2.Actions, string(ActionDelete), "the second call's own fresh memo must see the new admin binding")
}

// TestParity_CapabilitiesForActionsInstallIsPerCall checks that the
// per-action capability function, called with no outer memo, installs a
// memo scoped to that call: a binding added between two calls is visible
// to the second.
func TestParity_CapabilitiesForActionsInstallIsPerCall(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "per-call-actions")
	authz, _ := newRecordingAuthz(newMemoTestStore(s))
	actions := []Action{ActionRead, ActionDelete}
	resources := []Resource{f.agentRes, f.agentRes}

	caps1 := authz.ComputeCapabilitiesForActions(context.Background(), f.user, resources, actions)
	for i := range caps1 {
		require.Equal(t, []string{string(ActionRead)}, caps1[i].Actions, "an ordinary member starts with read only (resource %d)", i)
	}

	createDCUser(t, s, f.userID, "per-call-actions@test.com", f.projectID, store.ProjectRoleOwner)

	caps2 := authz.ComputeCapabilitiesForActions(context.Background(), f.user, resources, actions)
	for i := range caps2 {
		assert.Contains(t, caps2[i].Actions, string(ActionDelete), "the second call's own memo must see the new owner binding (resource %d)", i)
	}
	batch := authz.ComputeCapabilitiesBatch(context.Background(), f.user, []Resource{f.agentRes}, "agent")
	assert.Contains(t, batch[0].Actions, string(ActionDelete), "a later batch call installs its own memo too")
}

// =============================================================================
// Remaining P/U/C/R/T rows (completing the parity matrix for branch 1)
// =============================================================================

// TestParity_P2_DirectBindingMember is row P2 ("direct-binding
// member | deep-equal"): a direct (non-group) role binding, the shape
// newProjectPrincipalFixture already builds.
func TestParity_P2_DirectBindingMember(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newProjectPrincipalFixture(t, s, "p2", store.ProjectRoleMember)
	_, _, _, candStore := runParity(t, s, f.user, agentResourceTuples(f.agentRes))
	assert.Equal(t, 1, candStore.countOf("GetEffectiveGroups"), "groups loaded once under the memo")
}

// TestParity_P3_ProjectOwnerWithOwnedAgent is row P3 ("project owner
// (binding + OwnerID on some agents) | deep-equal, including the owner
// relationship candidate"): the owner has both a role binding AND is the
// OwnerID of one of the two agent resources tested, so both the
// binding-granted path and the owner-relationship path are exercised.
func TestParity_P3_ProjectOwnerWithOwnedAgent(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	projectID := tid("p3-project")
	ownerID := tid("p3-owner")
	otherOwnerID := tid("p3-other-owner")
	ownedAgentID := tid("p3-owned-agent")
	otherAgentID := tid("p3-other-agent")

	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Slug: "p3-proj", Name: "p3", OwnerID: ownerID}))
	createDCUser(t, s, ownerID, "p3-owner@test.com", projectID, store.ProjectRoleOwner)
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: ownedAgentID, Slug: "p3-owned", Name: "p3-owned", ProjectID: projectID, Phase: "running", OwnerID: ownerID, Ancestry: []string{ownerID}}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: otherAgentID, Slug: "p3-other", Name: "p3-other", ProjectID: projectID, Phase: "running", OwnerID: otherOwnerID, Ancestry: []string{otherOwnerID}}))

	owner := NewAuthenticatedUser(ownerID, "p3-owner@test.com", "p3", "member", "api")
	ownedRes := Resource{Type: "agent", ID: ownedAgentID, ParentType: "project", ParentID: projectID, OwnerID: ownerID}
	otherRes := Resource{Type: "agent", ID: otherAgentID, ParentType: "project", ParentID: projectID, OwnerID: otherOwnerID}

	tuples := append(agentResourceTuples(ownedRes), agentResourceTuples(otherRes)...)
	refDecisions, _, _, _ := runParity(t, s, owner, tuples)
	var sawOwnerGrant bool
	for _, d := range refDecisions {
		if d.Allowed && d.Reason == "relationship grant: resource owner" {
			sawOwnerGrant = true
		}
	}
	assert.True(t, sawOwnerGrant, "H2: the owner-relationship candidate must actually be exercised on the owned agent")
}

// TestParity_P5_HubAdmin is row P5 ("hub-admin | deep-equal").
func TestParity_P5_HubAdmin(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("p5-hubadmin")
	createTestUserWithRole(t, s, userID, "p5@test.com", "admin", store.SystemRoleHubAdmin)
	admin := NewAuthenticatedUser(userID, "p5@test.com", "p5", "admin", "api")

	projectID := tid("p5-project")
	ownerID := tid("p5-owner")
	agentID := tid("p5-agent")
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Slug: "p5-proj", Name: "p5", OwnerID: ownerID}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: agentID, Slug: "p5-agent", Name: "p5-agent", ProjectID: projectID, Phase: "running", OwnerID: ownerID, Ancestry: []string{ownerID}}))
	res := Resource{Type: "agent", ID: agentID, ParentType: "project", ParentID: projectID, OwnerID: ownerID}

	runParity(t, s, admin, agentResourceTuples(res))
}

// TestParity_P6_SuperAdmin is row P6 ("super-admin | deep-equal").
func TestParity_P6_SuperAdmin(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("p6-superadmin")
	createTestUserWithRole(t, s, userID, "p6@test.com", "admin", store.SystemRoleSuperAdmin)
	admin := NewAuthenticatedUser(userID, "p6@test.com", "p6", "admin", "api")

	projectID := tid("p6-project")
	ownerID := tid("p6-owner")
	agentID := tid("p6-agent")
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Slug: "p6-proj", Name: "p6", OwnerID: ownerID}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: agentID, Slug: "p6-agent", Name: "p6-agent", ProjectID: projectID, Phase: "running", OwnerID: ownerID, Ancestry: []string{ownerID}}))
	res := Resource{Type: "agent", ID: agentID, ParentType: "project", ParentID: projectID, OwnerID: ownerID}

	refDecisions, _, _, _ := runParity(t, s, admin, agentResourceTuples(res))
	for i, d := range refDecisions {
		assert.True(t, d.Allowed, "super-admin must be allowed on tuple %d", i)
	}
}

// TestParity_P7_NonMemberHubUser is row P7 ("non-member hub user |
// deep-equal denies"): a hub member with no binding on this project at all.
func TestParity_P7_NonMemberHubUser(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("p7-nonmember")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "p7@test.com", DisplayName: "p7", Role: "member", Status: "active"}))
	user := NewAuthenticatedUser(userID, "p7@test.com", "p7", "member", "api")

	projectID := tid("p7-project")
	ownerID := tid("p7-owner")
	agentID := tid("p7-agent")
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Slug: "p7-proj", Name: "p7", OwnerID: ownerID}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: agentID, Slug: "p7-agent", Name: "p7-agent", ProjectID: projectID, Phase: "running", OwnerID: ownerID, Ancestry: []string{ownerID}}))
	res := Resource{Type: "agent", ID: agentID, ParentType: "project", ParentID: projectID, OwnerID: ownerID}

	refDecisions, _, _, _ := runParity(t, s, user, agentResourceTuples(res))
	for i, d := range refDecisions {
		assert.False(t, d.Allowed, "non-member must be denied on tuple %d", i)
	}
}

// TestParity_U2_ScopedAndUnscopedSameUserNoLeakage is row U2 ("scoped
// UAT and unscoped session for the same user in one ctx | no credential
// leakage either way"). A ScopedUserIdentity wrapping a UserIdentity has the
// SAME principalKey as the unscoped identity (Type()/ID() are promoted
// through the embedded field), so both share one memo entry by
// construction. This row proves that sharing is safe: each decision still
// applies its OWN credential restriction (7a) independently, so the
// broader unscoped grant never leaks into the scoped identity's decisions,
// and vice versa.
func TestParity_U2_ScopedAndUnscopedSameUserNoLeakage(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "u2")
	ceiling, ok := permissions.BuildCeilingFromSelectors([]string{"agent:read"})
	require.True(t, ok)
	scoped := NewScopedUserIdentityWithCeiling(f.user, f.projectID, []string{"agent:read"}, "u2-uat", ceiling)

	// References: each identity evaluated alone, no memo, no interleaving.
	refUnscoped, _, _, _ := runParity(t, s, f.user, agentResourceTuples(f.agentRes))
	refScoped, _, _, _ := runParity(t, s, scoped, agentResourceTuples(f.agentRes))

	// Candidate: BOTH identities interleaved in ONE memo ctx.
	candStore := newMemoTestStore(s)
	candAuthz, _ := newRecordingAuthz(candStore)
	mctx := withAuthzInputMemo(context.Background())
	actions := ResourceActions["agent"]
	var candUnscoped, candScoped []Decision
	for _, a := range actions {
		candUnscoped = append(candUnscoped, candAuthz.CheckAccess(mctx, f.user, f.agentRes, a))
		candScoped = append(candScoped, candAuthz.CheckAccess(mctx, scoped, f.agentRes, a))
	}

	for i := range actions {
		assertDecisionsEqual(t, refUnscoped[i], candUnscoped[i], "unscoped tuple %d: the scoped identity sharing the memo entry must not narrow it", i)
		assertDecisionsEqual(t, refScoped[i], candScoped[i], "scoped tuple %d: the unscoped identity sharing the memo entry must not widen it", i)
	}
	assert.Equal(t, 1, candStore.countOf("GetEffectiveGroups"), "both identities share one memo entry (same principalKey)")

	var sawScopedDeny, sawUnscopedAllow bool
	for i := range actions {
		if !candScoped[i].Allowed {
			sawScopedDeny = true
		}
		if candUnscoped[i].Allowed {
			sawUnscopedAllow = true
		}
	}
	assert.True(t, sawScopedDeny, "H2: the scoped identity must actually be restricted by its own ceiling")
	assert.True(t, sawUnscopedAllow, "H2: the unscoped identity must actually retain its full access")
}

// TestParity_U3_ScopedUATAnotherProject is row U3 ("scoped UAT for
// another project | deep-equal step-1 denies; zero closure loads"). The
// ceiling may be real (this row does not depend on it), but the resource's
// project differs from the UAT's scoped project, so
// enforceUATConstraints denies at step 1, before step 2 ever runs.
func TestParity_U3_ScopedUATAnotherProject(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "u3")
	otherProjectID := tid("u3-other-project")
	ctx := context.Background()
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: otherProjectID, Slug: "u3-other-proj", Name: "u3-other"}))
	otherAgentID := tid("u3-other-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: otherAgentID, Slug: "u3-other-agent", Name: "u3-other-agent", ProjectID: otherProjectID, Phase: "running"}))
	otherRes := Resource{Type: "agent", ID: otherAgentID, ParentType: "project", ParentID: otherProjectID}

	ceiling, ok := permissions.BuildCeilingFromSelectors([]string{"agent:read"})
	require.True(t, ok)
	scoped := NewScopedUserIdentityWithCeiling(f.user, f.projectID, []string{"agent:read"}, "u3-uat", ceiling)

	refDecisions, _, refStore, candStore := runParity(t, s, scoped, agentResourceTuples(otherRes))
	for i, d := range refDecisions {
		assert.False(t, d.Allowed, "tuple %d must deny at step 1 (wrong project)", i)
		assert.Equal(t, "token not scoped for this project", d.Reason, "tuple %d", i)
	}
	assert.Equal(t, 0, refStore.countOf("GetEffectiveGroups"), "reference: zero closure loads")
	assert.Equal(t, 0, candStore.countOf("GetEffectiveGroups"), "candidate: zero closure loads")
}

// TestParity_C2_ConstraintTimeWindow is row C2 ("constraint time
// window inactive / active | deep-equal").
func TestParity_C2_ConstraintTimeWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active bool
	}{
		{"inactive_not_yet", false},
		{"active", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s := authzTestSetup(t)
			ctx := context.Background()
			f := newP1Fixture(t, s, "c2-"+tc.name)

			notBefore := time.Now().Add(-time.Hour)
			if !tc.active {
				notBefore = time.Now().Add(time.Hour) // not active yet
			}
			_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
				Name:               "c2-" + tc.name,
				SubjectKind:        store.ConstraintSubjectGroupClosure,
				SubjectGroupID:     &f.groupID,
				ScopeType:          ScopeTypeProject,
				ScopeID:            f.projectID,
				MaximumPermissions: []string{},
				NotBefore:          &notBefore,
				CreatedBy:          "test",
			})
			require.NoError(t, err)

			refDecisions, _, _, _ := runParity(t, s, f.user, agentResourceTuples(f.agentRes))
			var sawAllow bool
			for _, d := range refDecisions {
				if d.Allowed {
					sawAllow = true
				}
			}
			if tc.active {
				assert.False(t, sawAllow, "H2: an active empty-allowlist constraint must deny everything")
			} else {
				assert.True(t, sawAllow, "H2: an inactive (not-yet) constraint must not restrict anything")
			}
		})
	}
}

// TestParity_C4_SystemVsProjectScopedConstraint is row C4
// ("system-scoped vs project-scoped constraint | deep-equal").
func TestParity_C4_SystemVsProjectScopedConstraint(t *testing.T) {
	for _, tc := range []struct {
		name      string
		scopeType string
	}{
		{"system", ScopeTypeSystem},
		{"project", ScopeTypeProject},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s := authzTestSetup(t)
			ctx := context.Background()
			f := newP1Fixture(t, s, "c4-"+tc.name)

			scopeID := ""
			if tc.scopeType == ScopeTypeProject {
				scopeID = f.projectID
			}
			_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
				Name:               "c4-" + tc.name,
				SubjectKind:        store.ConstraintSubjectGroupClosure,
				SubjectGroupID:     &f.groupID,
				ScopeType:          tc.scopeType,
				ScopeID:            scopeID,
				MaximumPermissions: []string{},
				CreatedBy:          "test",
			})
			require.NoError(t, err)

			refDecisions, _, _, _ := runParity(t, s, f.user, agentResourceTuples(f.agentRes))
			for i, d := range refDecisions {
				assert.False(t, d.Allowed, "tuple %d must be denied by the %s-scoped constraint", i, tc.scopeType)
			}
		})
	}
}

// TestParity_R2_AncestorOnly is row R2 ("ancestor only | deep-equal").
func TestParity_R2_AncestorOnly(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	ancestorID := tid("r2-ancestor")
	outsiderID := tid("r2-outsider")
	projectID := tid("r2-project")
	agentID := tid("r2-agent")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: ancestorID, Email: "r2-ancestor@test.com", DisplayName: "r2a", Role: "member", Status: "active"}))
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: outsiderID, Email: "r2-outsider@test.com", DisplayName: "r2o", Role: "member", Status: "active"}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Slug: "r2-proj", Name: "r2"}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: agentID, Slug: "r2-agent", Name: "r2-agent", ProjectID: projectID, Phase: "running", Ancestry: []string{ancestorID}}))
	res := Resource{Type: "agent", ID: agentID, ParentType: "project", ParentID: projectID, Ancestry: []string{ancestorID}}
	// Access-only project binding: the ancestor relationship requires active
	// project access (ptone/scion#2141); the binding grants no permission.
	grantProjectAccessOnly(t, s, ancestorID, projectID)

	ancestor := NewAuthenticatedUser(ancestorID, "r2-ancestor@test.com", "r2a", "member", "api")
	refDecisions, _, _, _ := runParity(t, s, ancestor, agentResourceTuples(res))
	var sawAncestorGrant bool
	for _, d := range refDecisions {
		if d.Allowed && d.Reason == "relationship grant: ancestor access" {
			sawAncestorGrant = true
		}
	}
	assert.True(t, sawAncestorGrant, "H2: the ancestor relationship must actually be exercised")

	outsider := NewAuthenticatedUser(outsiderID, "r2-outsider@test.com", "r2o", "member", "api")
	outsiderDecisions, _, _, _ := runParity(t, s, outsider, agentResourceTuples(res))
	for i, d := range outsiderDecisions {
		assert.False(t, d.Allowed, "outsider tuple %d must deny", i)
	}
}

// TestParity_R3_RelationshipGrantRestrictedByUATScope is row R3
// ("relationship grant restricted by UAT scope | deep-equal"). The UAT
// owner's ceiling does not cover agent.delete, so the owner relationship
// would grant it but the token's scope blocks it. Step 1
// (enforceUATConstraints) evaluates the token's permission ceiling — the
// same ceiling the 7a restriction uses — so for a UAT the scope denial
// lands at step 1, before any relationship candidate is considered. The
// 7a "relationship grant restricted by credential_scope" branch is still
// exercised for agent tokens (authz_relationship_rules_test.go).
func TestParity_R3_RelationshipGrantRestrictedByUATScope(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	ownerID := tid("r3-owner")
	projectID := tid("r3-project")
	agentID := tid("r3-agent")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: ownerID, Email: "r3-owner@test.com", DisplayName: "r3", Role: "member", Status: "active"}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Slug: "r3-proj", Name: "r3", OwnerID: ownerID}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: agentID, Slug: "r3-agent", Name: "r3-agent", ProjectID: projectID, Phase: "running", OwnerID: ownerID, Ancestry: []string{ownerID}}))
	res := Resource{Type: "agent", ID: agentID, ParentType: "project", ParentID: projectID, OwnerID: ownerID}

	owner := NewAuthenticatedUser(ownerID, "r3-owner@test.com", "r3", "member", "api")
	// The legacy scopes list names agent:delete while the ceiling omits it:
	// step 1 consults the ceiling, so the legacy list cannot widen the
	// token past it.
	ceiling, ok := permissions.BuildCeilingFromSelectors([]string{"agent:read"}) // deliberately NOT agent:delete
	require.True(t, ok)
	scoped := NewScopedUserIdentityWithCeiling(owner, projectID, []string{"agent:delete"}, "r3-uat", ceiling)

	// A second tuple (ActionRead, which the ceiling DOES cover) so the
	// candidate's decision 2 is a principals/bindings memo HIT, not just a
	// single-decision run where the memo-hit path is never exercised for
	// this row.
	refDecisions, _, _, _ := runParity(t, s, scoped, []rawTuple{{res, ActionDelete}, {res, ActionRead}})
	require.Len(t, refDecisions, 2)
	assert.False(t, refDecisions[0].Allowed)
	assert.Equal(t, "token does not have scope: agent:delete", refDecisions[0].Reason, "H2: the ceiling, not the legacy scopes list, must decide the step-1 scope check")
}

// =============================================================================
// Remaining T rows (resource-type coverage): batch + AuthorizeReadBatch
// =============================================================================

// runBatchParity proves deep-equal for one resource through both
// ComputeCapabilitiesBatch and AuthorizeReadBatch — the two paths
// specified for resource-type coverage — reference (no memo) vs candidate
// (one memo installed for both batch calls). It returns
// the reference capability actions and AuthorizeReadBatch result for
// row-specific H2 assertions.
func runBatchParity(t *testing.T, s store.Store, identity Identity, resourceType string, res Resource) (refActions []string, refAllowed []bool) {
	t.Helper()
	ctx := context.Background()
	wantDecisions := len(ResourceActions[resourceType]) + 1 // the batch, plus AuthorizeReadBatch's one read decision

	refStore := newMemoTestStore(s)
	refAuthz, refEmit := newRecordingAuthz(refStore)
	// Both functions install the memo themselves, so the reference masks
	// it to keep every decision on its own store loads.
	refCtx := maskAllAuthzMemo(ctx)
	refCaps := refAuthz.ComputeCapabilitiesBatch(refCtx, identity, []Resource{res}, resourceType)
	refAllowed, refErr := refAuthz.AuthorizeReadBatch(refCtx, identity, []Resource{res})
	require.NoError(t, refErr)
	require.Len(t, refEmit.snapshot(), wantDecisions, "audit invariant: exactly one audit record per decision (reference)")

	candStore := newMemoTestStore(s)
	candAuthz, candEmit := newRecordingAuthz(candStore)
	mctx := withAuthzInputMemo(ctx)
	candCaps := candAuthz.ComputeCapabilitiesBatch(mctx, identity, []Resource{res}, resourceType)
	candAllowed, candErr := candAuthz.AuthorizeReadBatch(mctx, identity, []Resource{res})
	require.NoError(t, candErr)
	require.Len(t, candEmit.snapshot(), wantDecisions, "audit invariant: exactly one audit record per decision (candidate)")

	require.Len(t, refCaps, 1)
	require.Len(t, candCaps, 1)
	assert.Equal(t, refCaps[0].Actions, candCaps[0].Actions, "ComputeCapabilitiesBatch actions for %s %s", resourceType, res.ID)
	assert.Equal(t, refAllowed, candAllowed, "AuthorizeReadBatch for %s %s", resourceType, res.ID)
	// The batch APIs above return only capability lists and bools, never
	// Reason/DenyCause/MatchedGrant — the audit records are the ONLY place
	// T2/T3/T6/T7 (this helper's callers) can observe parity on anything
	// beyond Allowed. Without this, a candidate that agreed on every
	// Actions list and every AuthorizeReadBatch bool, but reached
	// those results through a different Reason (e.g. a leaked grant versus
	// the intended one), would pass silently. Emission order is
	// deterministic and identical on both sides, since both call the same
	// ComputeCapabilitiesBatch then AuthorizeReadBatch in the same order.
	assertAuditSequenceEqual(t, refEmit.snapshot(), candEmit.snapshot(), fmt.Sprintf("runBatchParity audit records for %s %s", resourceType, res.ID))
	return refCaps[0].Actions, refAllowed
}

// TestParity_T2_TemplateGlobalAndProjectScoped is row T2 ("template,
// global + project-scoped | batch + AuthorizeReadBatch | hub-wide filter
// reads roleDefs (filterHubWideTemplateGrants)").
func TestParity_T2_TemplateGlobalAndProjectScoped(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "t2")
	ctx := context.Background()
	// The global catalog is reachable only through the curated hub-member
	// system-scoped role (the hub-wide filter's whole purpose); P1's plain
	// project-member binding alone grants nothing at system scope.
	addToHubMembersGroup(t, s, f.userID)
	globalTplID := tid("t2-global-tpl")
	projectTplID := tid("t2-project-tpl")
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{ID: globalTplID, Name: "t2g", Slug: "t2g", Harness: "claude", Image: "img", Scope: store.TemplateScopeGlobal}))
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{ID: projectTplID, Name: "t2p", Slug: "t2p", Harness: "claude", Image: "img", Scope: store.TemplateScopeProject, ScopeID: f.projectID}))

	t.Run("global", func(t *testing.T) {
		actions, _ := runBatchParity(t, s, f.user, "template", templateResource(&store.Template{ID: globalTplID, Scope: store.TemplateScopeGlobal}))
		assert.Contains(t, actions, "read", "H2: the hub-wide filter must actually grant read on the global template, not just produce an empty (vacuously passing) set")
	})
	t.Run("project", func(t *testing.T) {
		actions, _ := runBatchParity(t, s, f.user, "template", templateResource(&store.Template{ID: projectTplID, Scope: store.TemplateScopeProject, ScopeID: f.projectID}))
		assert.Contains(t, actions, "read", "H2: the project-scoped template must actually grant read, not just produce an empty (vacuously passing) set")
	})
}

// TestParity_T3_HarnessConfigGlobalAndProjectScoped is row T3
// ("harness_config, global + project-scoped | batch + AuthorizeReadBatch |
// hub-wide filter (filterHubWideHarnessConfigGrants)").
func TestParity_T3_HarnessConfigGlobalAndProjectScoped(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "t3")
	ctx := context.Background()
	addToHubMembersGroup(t, s, f.userID) // see T2: required for global-catalog access
	globalHCID := tid("t3-global-hc")
	projectHCID := tid("t3-project-hc")
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{ID: globalHCID, Name: "t3g", Slug: "t3g", Harness: "claude", Scope: store.HarnessConfigScopeGlobal}))
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{ID: projectHCID, Name: "t3p", Slug: "t3p", Harness: "claude", Scope: store.HarnessConfigScopeProject, ScopeID: f.projectID}))

	t.Run("global", func(t *testing.T) {
		actions, _ := runBatchParity(t, s, f.user, "harness_config", harnessConfigResource(&store.HarnessConfig{ID: globalHCID, Scope: store.HarnessConfigScopeGlobal}))
		assert.Contains(t, actions, "read", "H2: the hub-wide filter must actually grant read on the global harness config, not just produce an empty (vacuously passing) set")
	})
	t.Run("project", func(t *testing.T) {
		actions, _ := runBatchParity(t, s, f.user, "harness_config", harnessConfigResource(&store.HarnessConfig{ID: projectHCID, Scope: store.HarnessConfigScopeProject, ScopeID: f.projectID}))
		assert.Contains(t, actions, "read", "H2: the project-scoped harness config must actually grant read, not just produce an empty (vacuously passing) set")
	})
}

// TestParity_T6_GCPServiceAccountHubScopedAssign is row T6
// ("gcp_service_account, hub-scoped, assign | batch | hub-member fact
// (relationshipCandidates' hub_member_sa_assign candidate)"): a current
// hub member (group membership, not just a role
// binding) may assign a hub-scoped service account they did not create,
// through the hub_member_sa_assign relationship.
func TestParity_T6_GCPServiceAccountHubScopedAssign(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	memberID := tid("t6-member")
	creatorID := tid("t6-creator")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: memberID, Email: "t6@test.com", DisplayName: "t6", Role: "member", Status: "active"}))
	addToHubMembersGroup(t, s, memberID)
	member := NewAuthenticatedUser(memberID, "t6@test.com", "t6", "member", "api")

	hubSA := gcpServiceAccountResource(&store.GCPServiceAccount{ID: tid("t6-sa"), CreatedBy: creatorID, Scope: store.ScopeHub, ScopeID: "hub"})
	actions, _ := runBatchParity(t, s, member, "gcp_service_account", hubSA)
	assert.Contains(t, actions, "assign", "H2: the hub-member fact must actually grant assign on a hub-scoped SA the member did not create")
}

// TestParity_T7_ParentlessDenyBaselineForAgent is row T7 ("broker,
// group, user for the agent principal | batch + AuthorizeReadBatch |
// parentless deny baseline").
func TestParity_T7_ParentlessDenyBaselineForAgent(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newA1Fixture(t, s, "t7")
	ctx := context.Background()
	brokerID := tid("t7-broker")
	groupID := tid("t7-group")
	userID := tid("t7-user")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{ID: brokerID, Name: "t7-broker", Slug: "t7-broker"}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: groupID, Name: "t7-group", Slug: "t7-group", GroupType: store.GroupTypeExplicit}))
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "t7@test.com", DisplayName: "t7", Role: "member", Status: "active"}))

	for _, res := range []Resource{
		brokerResource(&store.RuntimeBroker{ID: brokerID}),
		groupResource(&store.Group{ID: groupID}),
		userResource(&store.User{ID: userID}),
	} {
		t.Run(res.Type, func(t *testing.T) {
			actions, allowed := runBatchParity(t, s, f.agent, res.Type, res)
			assert.Empty(t, actions, "H2: the agent must get an empty capability set (parentless deny baseline) for %s", res.Type)
			for _, a := range allowed {
				assert.False(t, a, "H2: AuthorizeReadBatch must deny %s", res.Type)
			}
		})
	}
}

// =============================================================================
// #2155 check: identity re-classification adds no new decide path or
// principal type the memo must handle
// =============================================================================

// TestParity_BrokerOnBehalfOf_SharesMemoEntryWithPlainUser confirms directly,
// rather than by analysis alone, that a prior upstream change to decide's
// early classification (principalContextForIdentity's type-assertion
// switch, plus a suppliedCredentialCompatible/brokerOnBehalfOfAuthorizes
// gate, entirely upstream of step 2's a.inputsFor) does not affect this
// memo: it does not add a new PrincipalKind (principalContextForIdentity's
// switch still produces only the kinds the const block already named) and
// does not touch NormalizePrincipalType or authorizationPrincipals, which
// principalKey is keyed from. A broker-on-behalf-of request for a local
// user is still, from step 2 onward, exactly a request from that
// *AuthenticatedUser — only Credential.Kind differs, and credential data
// is never a memoized input. This test proves it: the broker-on-behalf-of
// decision and a plain interactive decision for the SAME user, run in one
// memo phase, are deep-equal apart from the credential-identifying fields,
// and share exactly one memo entry (one GetEffectiveGroups call for both).
func TestParity_BrokerOnBehalfOf_SharesMemoEntryWithPlainUser(t *testing.T) {
	_, s := authzTestSetup(t)
	f := newP1Fixture(t, s, "obo")
	brokerID := tid("obo-broker")
	broker := NewBrokerIdentity(brokerID)

	// Build the ctx BrokerAuthMiddleware would install after HMAC
	// verification and a successful on-behalf-of resolution: the broker
	// identity marker, the dedicated BrokerOnBehalfOf marker naming this
	// principal, and the effective identity set to the local user.
	oboCtx := contextWithBrokerIdentity(context.Background(), broker)
	oboCtx = contextWithBrokerOnBehalfOf(oboCtx, BrokerOnBehalfOf{Broker: broker, BrokerID: brokerID})
	oboCtx = contextWithIdentity(oboCtx, f.user)

	oboRequest := func(ctx context.Context, action Action) AuthzRequest {
		return AuthzRequest{
			Principal:  PrincipalContext{Identity: f.user},
			Credential: CredentialContext{Kind: CredentialKindBroker, ID: brokerID, Type: "broker"},
			Resource:   f.agentRes,
			Action:     action,
		}
	}

	actions := ResourceActions["agent"]

	refStore := newMemoTestStore(s)
	refAuthz, _ := newRecordingAuthz(refStore)
	var refPlain, refObo []Decision
	for _, a := range actions {
		refPlain = append(refPlain, refAuthz.CheckAccess(context.Background(), f.user, f.agentRes, a))
		refObo = append(refObo, refAuthz.Decide(oboCtx, oboRequest(oboCtx, a)))
	}

	candStore := newMemoTestStore(s)
	candAuthz, _ := newRecordingAuthz(candStore)
	mctx := withAuthzInputMemo(context.Background())
	moboCtx := withAuthzInputMemo(oboCtx)
	var candPlain, candObo []Decision
	for _, a := range actions {
		candPlain = append(candPlain, candAuthz.CheckAccess(mctx, f.user, f.agentRes, a))
		candObo = append(candObo, candAuthz.Decide(moboCtx, oboRequest(moboCtx, a)))
	}

	for i := range actions {
		assertDecisionsEqual(t, refPlain[i], candPlain[i], "plain tuple %d", i)

		// The on-behalf-of decision differs from plain only in the
		// credential-identifying fields; zero those before comparing, since
		// they are expected to differ (broker credential vs interactive).
		refObo[i].CredentialKind, candObo[i].CredentialKind = "", ""
		refObo[i].CredentialID, candObo[i].CredentialID = "", ""
		refObo[i].CredentialType, candObo[i].CredentialType = "", ""
		assertDecisionsEqual(t, refObo[i], candObo[i], "on-behalf-of tuple %d", i)

		// And the on-behalf-of decision's AUTHORIZATION outcome (Allowed,
		// Reason, DenyCause — not the credential metadata) must match the
		// plain user's, since the principal and its memoized inputs are
		// identical; only the credential bookkeeping differs.
		assert.Equal(t, refPlain[i].Allowed, refObo[i].Allowed, "tuple %d: on-behalf-of must authorize exactly like the plain user", i)
		assert.Equal(t, refPlain[i].Reason, refObo[i].Reason, "tuple %d", i)
	}

	// mctx and moboCtx are two SEPARATE memo installs in this test (one per
	// withAuthzInputMemo call), which is deliberate: it proves plain vs
	// on-behalf-of parity independent of memo sharing. The count assertion
	// below uses one shared memo to prove the sharing claim itself.
	sharedStore := newMemoTestStore(s)
	sharedAuthz, _ := newRecordingAuthz(sharedStore)
	sharedCtx := withAuthzInputMemo(context.Background())
	sharedAuthz.CheckAccess(sharedCtx, f.user, f.agentRes, ActionRead)
	// Reuse the SAME installed memo ctx for the on-behalf-of call by
	// deriving it from sharedCtx (which already carries the memo), not from
	// a fresh background ctx, so both calls land in the same entry.
	sharedOboOnSameMemo := contextWithBrokerIdentity(sharedCtx, broker)
	sharedOboOnSameMemo = contextWithBrokerOnBehalfOf(sharedOboOnSameMemo, BrokerOnBehalfOf{Broker: broker, BrokerID: brokerID})
	sharedOboOnSameMemo = contextWithIdentity(sharedOboOnSameMemo, f.user)
	sharedAuthz.Decide(sharedOboOnSameMemo, oboRequest(sharedOboOnSameMemo, ActionRead))
	assert.Equal(t, 1, sharedStore.countOf("GetEffectiveGroups"), "plain and on-behalf-of decisions for the same user must share one memo entry (same principalKey, credential-independent)")
}
