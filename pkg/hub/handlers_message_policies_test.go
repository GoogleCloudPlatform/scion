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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mphFixture struct {
	srv          *Server
	s            store.Store
	project      string
	otherProject string
	owner        string
	projectAgent string
	otherAgent   string
}

func newMPHFixture(t *testing.T, policiesOn bool) *mphFixture {
	t.Helper()
	srv, s := testServer(t)
	mpgSetExperiment(t, srv, policiesOn, true)

	ctx := context.Background()
	project := projectOpsProject(t, s, "mph-proj")
	otherProject := projectOpsProject(t, s, "mph-other")
	owner := projectOpsUser(t, s, "mph-owner", false)
	projectOpsGrant(t, s, owner, project, store.ProjectRoleOwner)
	projectOpsGrant(t, s, owner, otherProject, store.ProjectRoleOwner)

	projectAgent := tid("mph-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:        projectAgent,
		Slug:      "mph-agent",
		Name:      "mph-agent",
		ProjectID: project,
		CreatedBy: owner,
		OwnerID:   owner,
	}))
	seedRecordedDelegationEdge(t, s, "user", owner, "agent", projectAgent, store.RoleScopeProject, project, string(AgentRoleBaseline))

	otherAgent := tid("mph-other-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:        otherAgent,
		Slug:      "mph-other-agent",
		Name:      "mph-other-agent",
		ProjectID: otherProject,
		CreatedBy: owner,
		OwnerID:   owner,
	}))
	seedRecordedDelegationEdge(t, s, "user", owner, "agent", otherAgent, store.RoleScopeProject, otherProject, string(AgentRoleBaseline))

	return &mphFixture{
		srv:          srv,
		s:            s,
		project:      project,
		otherProject: otherProject,
		owner:        owner,
		projectAgent: projectAgent,
		otherAgent:   otherAgent,
	}
}

func (f *mphFixture) basePath() string {
	return "/api/v1/projects/" + f.project + "/message-policies"
}

func mphSessionDo(t *testing.T, srv *Server, userID, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	identity := NewAuthenticatedUser(userID, userID+"@test.com", userID, "member", string(ClientTypeAPI))
	ctx := contextWithIdentity(context.Background(), identity)
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindInteractive, ID: "mph-sess"})
	req := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

func mphValidCreateBody(name string) CreateMessagePolicyRequest {
	return CreateMessagePolicyRequest{
		Name:        name,
		Description: "Require structured choices instead of prose menus",
		Scope: store.MessagePolicyScope{
			Surfaces: []string{
				store.MessagePolicySurfaceAgentToUser,
				store.MessagePolicySurfaceAgentToConversation,
			},
		},
		ViolationRule:     `!message.has_choices && message.body.matches("(?i)option a|option b")`,
		OnFailed:          store.MessagePolicyOutcomeReject,
		RejectReason:      "Message blocked by {policy} for {recipient}",
		RejectRemediation: "Resend with scion message --choices",
	}
}

// mphSpyDialect wraps a real CELDialect to record Invalidate calls while
// delegating compilation, caching, invalidation, and evaluation to the real CEL
// engine (target 5).
type mphSpyDialect struct {
	*ruleengine.CELDialect
	mu          sync.Mutex
	invalidated []mphInvalidation
}

type mphInvalidation struct {
	envName string
	scope   celengine.CacheScope
}

func (d *mphSpyDialect) Invalidate(envName string, scope celengine.CacheScope) {
	d.mu.Lock()
	d.invalidated = append(d.invalidated, mphInvalidation{envName: envName, scope: scope})
	d.mu.Unlock()
	d.CELDialect.Invalidate(envName, scope)
}

func (d *mphSpyDialect) snapshot() []mphInvalidation {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]mphInvalidation, len(d.invalidated))
	copy(out, d.invalidated)
	return out
}

// TestMessagePolicyHandlers_Target1_ExperimentOff404 verifies that every
// project message-policy route returns 404 when hub.message_policies is off.
func TestMessagePolicyHandlers_Target1_ExperimentOff404(t *testing.T) {
	f := newMPHFixture(t, false)
	base := f.basePath()

	routes := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, base, nil},
		{http.MethodPost, base, mphValidCreateBody("off-create")},
		{http.MethodGet, base + "/off-create", nil},
		{http.MethodPatch, base + "/off-create", map[string]any{"mode": "enforce"}},
		{http.MethodDelete, base + "/off-create", nil},
		{http.MethodPost, base + "/test", map[string]any{
			"violation_rule": `message.body.contains("x")`,
			"on_failed":      "reject",
			"sample":         map[string]any{"body": "x"},
		}},
		{http.MethodPost, base + "/off-create/test", map[string]any{
			"sample": map[string]any{"body": "x"},
		}},
		{http.MethodGet, base + "/decisions", nil},
		{http.MethodGet, base + "/off-create/decisions", nil},
	}

	for _, rt := range routes {
		rec := mphSessionDo(t, f.srv, f.owner, rt.method, rt.path, rt.body)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s while experiment off: %s", rt.method, rt.path, rec.Body.String())
	}
}

// TestMessagePolicyHandlers_CRUDCoverageAndCacheInvalidation verifies create
// (default shadow mode + revision=1), list, get (by ID and name, including
// coverage.enforced_surfaces and coverage.uncovered_agent_paths), patch
// (revision bump + Invalidate with Consumer "message_policy" taking effect),
// and delete (including cache invalidation and cross-project isolation).
func TestMessagePolicyHandlers_CRUDCoverageAndCacheInvalidation(t *testing.T) {
	f := newMPHFixture(t, true)
	base := f.basePath()

	// Install a real CEL CachedDialect wrapped in mphSpyDialect so we can
	// assert both the exact Invalidate(Consumer: "message_policy") call and
	// that cached program eviction takes effect (target 5).
	celEng, err := messagepolicy.NewCELEngine()
	require.NoError(t, err)
	baseCEL, err := ruleengine.NewCELDialect(celEng)
	require.NoError(t, err)
	spy := &mphSpyDialect{CELDialect: baseCEL}
	reEng := ruleengine.NewEngine()
	require.NoError(t, reEng.Register(spy))
	require.NoError(t, reEng.AllowConsumerDialects(messagepolicy.ConsumerName, ruleengine.Grant(ruleengine.DialectCEL)))
	ev, err := messagepolicy.New(reEng)
	require.NoError(t, err)
	f.srv.messagePolicyGate.evaluator = ev

	// 1. Create defaults to mode="shadow", enabled=true, revision=1.
	createReq := mphValidCreateBody("no-prose-choices")
	createReq.Scope.Sender.AgentIDs = []string{f.projectAgent}
	rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, createReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var created MessagePolicyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.NotEmpty(t, created.ID)
	assert.Equal(t, f.project, created.ProjectID)
	assert.Equal(t, "no-prose-choices", created.Name)
	assert.True(t, created.Enabled)
	assert.Equal(t, store.MessagePolicyModeShadow, created.Mode)
	assert.Equal(t, int64(1), created.Revision)
	assert.Equal(t, []string{
		store.MessagePolicySurfaceAgentToUser,
		store.MessagePolicySurfaceAgentToConversation,
	}, created.Coverage.EnforcedSurfaces)
	assert.Equal(t, []string{
		store.MessagePolicySurfaceAgentDM,
		store.MessagePolicySurfaceGroupPost,
		store.MessagePolicySurfaceGroupSet,
		store.MessagePolicySurfaceMentionFanout,
		store.MessagePolicySurfaceMentionDelivery,
		"broadcast",
	}, created.Coverage.UncoveredAgentPaths)

	// Duplicate name in the same project -> 409 Conflict.
	dupRec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, createReq)
	assert.Equal(t, http.StatusConflict, dupRec.Code, dupRec.Body.String())

	// 2. GET by ID and GET by slug name both return the policy + coverage (target 6).
	for _, ref := range []string{created.ID, created.Name} {
		getRec := mphSessionDo(t, f.srv, f.owner, http.MethodGet, base+"/"+ref, nil)
		require.Equal(t, http.StatusOK, getRec.Code, "GET %s: %s", ref, getRec.Body.String())
		var got MessagePolicyResponse
		require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &got))
		assert.Equal(t, created.ID, got.ID)
		assert.Equal(t, created.Coverage.UncoveredAgentPaths, got.Coverage.UncoveredAgentPaths)
	}

	// Cross-project GET by UUID returns 404.
	otherBase := "/api/v1/projects/" + f.otherProject + "/message-policies"
	crossGet := mphSessionDo(t, f.srv, f.owner, http.MethodGet, otherBase+"/"+created.ID, nil)
	assert.Equal(t, http.StatusNotFound, crossGet.Code, crossGet.Body.String())

	// 3. List policies returns the created policy + top-level coverage.
	listRec := mphSessionDo(t, f.srv, f.owner, http.MethodGet, base+"?mode=shadow&enabled=true", nil)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	var listed ListMessagePoliciesResponse
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &listed))
	require.Len(t, listed.Policies, 1)
	assert.Equal(t, created.ID, listed.Policies[0].ID)
	assert.Equal(t, 1, listed.TotalCount)
	assert.Contains(t, listed.Coverage.UncoveredAgentPaths, "agent_dm")

	// Prime the compiled program cache at revision 1 before editing.
	act := messagepolicy.NewActivation(messagepolicy.Facts{
		Surface:   messagepolicy.SurfaceAgentToUser,
		Sender:    messagepolicy.SenderFacts{Kind: "agent", ID: f.projectAgent, Slug: "mph-agent", ProjectID: f.project},
		Recipient: messagepolicy.RecipientFacts{Kind: "user", ID: f.owner, Slug: "mph-owner"},
		Message:   messagepolicy.MessageFacts{Type: "message", Body: "Please pick Option A or Option B."},
		Conversation: messagepolicy.ConversationFacts{
			Kind: "direct",
		},
	})
	clsBefore := ev.EvaluateViolation(context.Background(), created.ID, 1, created.ViolationRule, act)
	assert.Equal(t, messagepolicy.ClassViolation, clsBefore.Class)
	assert.Equal(t, 1, celEng.CacheStats().Entries, "revision 1 compiled rule should be cached")

	// 4. PATCH bumps revision to 2, updates mode/rule, and calls Invalidate with
	// Consumer: "message_policy" (target 5).
	newMode := store.MessagePolicyModeEnforce
	newRule := `message.body.contains("FORBIDDEN_TOKEN")`
	patchRec := mphSessionDo(t, f.srv, f.owner, http.MethodPatch, base+"/"+created.Name, PatchMessagePolicyRequest{
		Mode:             &newMode,
		ViolationRule:    &newRule,
		ExpectedRevision: 1,
	})
	require.Equal(t, http.StatusOK, patchRec.Code, patchRec.Body.String())

	var patched MessagePolicyResponse
	require.NoError(t, json.Unmarshal(patchRec.Body.Bytes(), &patched))
	assert.Equal(t, int64(2), patched.Revision)
	assert.Equal(t, store.MessagePolicyModeEnforce, patched.Mode)
	assert.Equal(t, newRule, patched.ViolationRule)

	invals := spy.snapshot()
	require.Len(t, invals, 1, "PATCH must call Invalidate once")
	assert.Equal(t, messagepolicy.EnvironmentName, invals[0].envName)
	assert.Equal(t, celengine.CacheScope{
		Consumer: messagepolicy.ConsumerName, // "message_policy"
		ID:       created.ID,
		Revision: 1,
	}, invals[0].scope)
	assert.Equal(t, 0, celEng.CacheStats().Entries, "Invalidate must evict the cached revision 1 program")

	// Evaluating with the updated policy takes effect immediately: the old
	// "Option A" phrase now passes, while "FORBIDDEN_TOKEN" violates.
	clsAfterOldSample := ev.EvaluateViolation(context.Background(), patched.ID, patched.Revision, patched.ViolationRule, act)
	assert.Equal(t, messagepolicy.ClassPass, clsAfterOldSample.Class)

	// Stale ExpectedRevision -> 409 revision_conflict.
	staleRec := mphSessionDo(t, f.srv, f.owner, http.MethodPatch, base+"/"+created.ID, PatchMessagePolicyRequest{
		Mode:             &newMode,
		ExpectedRevision: 1,
	})
	assert.Equal(t, http.StatusConflict, staleRec.Code, staleRec.Body.String())

	// Prime cache at revision 2, then DELETE -> 204 and cache invalidated again.
	_ = ev.EvaluateViolation(context.Background(), patched.ID, patched.Revision, patched.ViolationRule, act)
	assert.Equal(t, 1, celEng.CacheStats().Entries)

	delRec := mphSessionDo(t, f.srv, f.owner, http.MethodDelete, base+"/"+created.ID, nil)
	require.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())

	invals = spy.snapshot()
	require.Len(t, invals, 2, "DELETE must also call Invalidate")
	assert.Equal(t, celengine.CacheScope{
		Consumer: messagepolicy.ConsumerName,
		ID:       created.ID,
		Revision: 2,
	}, invals[1].scope)
	assert.Equal(t, 0, celEng.CacheStats().Entries, "DELETE Invalidate must evict the cached revision 2 program")

	getAfterDel := mphSessionDo(t, f.srv, f.owner, http.MethodGet, base+"/"+created.ID, nil)
	assert.Equal(t, http.StatusNotFound, getAfterDel.Code)

	// Verify delete mutation audit BeforeSummary matches AuditObligation.BeforeFields.
	var delAudits []*store.MutationAuditRecord
	require.Eventually(t, func() bool {
		var auditErr error
		delAudits, _, auditErr = f.s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
			MutationType: mutationTypeMessagePolicyDelete,
			TargetID:     f.project,
			Limit:        10,
		})
		return auditErr == nil && len(delAudits) == 1
	}, 2*time.Second, 10*time.Millisecond, "delete mutation audit should be recorded")
	var beforeFields map[string]any
	require.NoError(t, json.Unmarshal([]byte(delAudits[0].BeforeSummary), &beforeFields))
	assert.Equal(t, map[string]any{
		"policy_id":   created.ID,
		"policy_name": created.Name,
		"mode":        store.MessagePolicyModeEnforce,
		"revision":    float64(2),
	}, beforeFields)
}

// TestMessagePolicyHandlers_AuthoringValidation400s verifies structured 400
// rejections across CEL compile errors (line/column/category, target 5), regex
// categories (regex_literal_required and size, target 4a), non-empty
// system_one_rule, uncovered surfaces and agent recipient selectors
// (surface_not_covered, target 6), missing on_failed (target 4), reserved
// route names, cross-project sender agent_ids, and unsupported dialect with
// reason.
func TestMessagePolicyHandlers_AuthoringValidation400s(t *testing.T) {
	f := newMPHFixture(t, true)
	base := f.basePath()

	decodeErr := func(t *testing.T, rec *httptest.ResponseRecorder) APIError {
		t.Helper()
		var env ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), "body: %s", rec.Body.String())
		return env.Error
	}

	t.Run("cel syntax error returns invalid_rule with line column category", func(t *testing.T) {
		body := mphValidCreateBody("bad-syntax")
		body.ViolationRule = `message.body.contains(`
		rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, body)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		apiErr := decodeErr(t, rec)
		assert.Equal(t, messagepolicy.ErrCodeInvalidRule, apiErr.Code)
		assert.Equal(t, "syntax", apiErr.Details["category"])
		assert.GreaterOrEqual(t, int(apiErr.Details["line"].(float64)), 1)
		assert.GreaterOrEqual(t, int(apiErr.Details["column"].(float64)), 1)
	})

	t.Run("cel unknown variable and non-bool return invalid_rule with category", func(t *testing.T) {
		unknownBody := mphValidCreateBody("bad-unknown")
		unknownBody.ViolationRule = `nonexistent_ident == true`
		rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, unknownBody)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		apiErr := decodeErr(t, rec)
		assert.Equal(t, messagepolicy.ErrCodeInvalidRule, apiErr.Code)
		assert.Equal(t, "unknown_variable", apiErr.Details["category"])

		nonBoolBody := mphValidCreateBody("bad-result-type")
		nonBoolBody.ViolationRule = `sender.slug`
		rec = mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, nonBoolBody)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		apiErr = decodeErr(t, rec)
		assert.Equal(t, messagepolicy.ErrCodeInvalidRule, apiErr.Code)
		assert.Equal(t, "result_type", apiErr.Details["category"])
	})

	t.Run("non-literal and oversized regex return invalid_rule with category (target 4a)", func(t *testing.T) {
		dynRegex := mphValidCreateBody("bad-dyn-regex")
		dynRegex.ViolationRule = `message.body.matches(sender.slug)`
		rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, dynRegex)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		apiErr := decodeErr(t, rec)
		assert.Equal(t, messagepolicy.ErrCodeInvalidRule, apiErr.Code)
		assert.Equal(t, "regex_literal_required", apiErr.Details["category"])

		bigRegex := mphValidCreateBody("bad-big-regex")
		bigRegex.ViolationRule = `message.body.matches("` + strings.Repeat("a", 600) + `")`
		rec = mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, bigRegex)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		apiErr = decodeErr(t, rec)
		assert.Equal(t, messagepolicy.ErrCodeInvalidRule, apiErr.Code)
		assert.Equal(t, "size", apiErr.Details["category"])
	})

	t.Run("system_one_rule requires on_uncertain and refuses invalid or unsupported specs", func(t *testing.T) {
		missingUnc := mphValidCreateBody("m2-missing-uncertain")
		missingUnc.SystemOneRule = `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"questions":[{"id":"asks_choice","type":"binary","instructions":"Ask choice?"}]}}`
		missingUnc.OnUncertain = ""
		rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, missingUnc)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		apiErr := decodeErr(t, rec)
		assert.Equal(t, messagepolicy.ErrCodeValidation, apiErr.Code)
		assert.Equal(t, "on_uncertain", apiErr.Details["field"])

		// With hub.system_one experiment OFF, compiling system_one_rule returns 400 unsupported_dialect.
		withUnc := mphValidCreateBody("m2-disabled-dialect")
		withUnc.SystemOneRule = missingUnc.SystemOneRule
		withUnc.OnUncertain = messagepolicy.OutcomeReject
		rec = mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, withUnc)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		apiErr = decodeErr(t, rec)
		assert.Equal(t, messagepolicy.ErrCodeUnsupportedDialect, apiErr.Code)
	})

	t.Run("uncovered surface or agent recipient selector returns 400 surface_not_covered (target 6)", func(t *testing.T) {
		for _, surf := range []string{"agent_dm", "group_post", "group_set", "mention_fanout", "mention_delivery", "broadcast"} {
			body := mphValidCreateBody("uncovered-surf")
			body.Scope.Surfaces = []string{surf}
			rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, body)
			require.Equal(t, http.StatusBadRequest, rec.Code, "surface %s: %s", surf, rec.Body.String())
			apiErr := decodeErr(t, rec)
			assert.Equal(t, messagepolicy.ErrCodeSurfaceNotCovered, apiErr.Code)
			assert.ElementsMatch(t, []any{"agent_to_user", "agent_to_conversation"}, apiErr.Details["enforced_surfaces"])
		}

		agentKind := mphValidCreateBody("uncovered-recip-kind")
		agentKind.Scope.Recipient.Kinds = []string{"agent"}
		rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, agentKind)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Equal(t, messagepolicy.ErrCodeSurfaceNotCovered, decodeErr(t, rec).Code)

		agentIDs := mphValidCreateBody("uncovered-recip-ids")
		agentIDs.Scope.Recipient.AgentIDs = []string{f.projectAgent}
		rec = mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, agentIDs)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Equal(t, messagepolicy.ErrCodeSurfaceNotCovered, decodeErr(t, rec).Code)
	})

	t.Run("missing or invalid on_failed returns 400 (target 4)", func(t *testing.T) {
		missing := mphValidCreateBody("missing-on-failed")
		missing.OnFailed = ""
		rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, missing)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		apiErr := decodeErr(t, rec)
		assert.Equal(t, ErrCodeValidationError, apiErr.Code)
		assert.Equal(t, "on_failed", apiErr.Details["field"])
	})

	t.Run("reserved names, uuid-shaped names, and cross-project sender agent_ids return 400", func(t *testing.T) {
		for _, reserved := range []string{"test", "decisions"} {
			body := mphValidCreateBody(reserved)
			rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, body)
			require.Equal(t, http.StatusBadRequest, rec.Code, "reserved name %s: %s", reserved, rec.Body.String())
			apiErr := decodeErr(t, rec)
			assert.Equal(t, ErrCodeValidationError, apiErr.Code)
			assert.Equal(t, "name", apiErr.Details["field"])
		}

		uuidNames := []string{
			"550e8400-e29b-41d4-a716-446655440000",
			"550e8400e29b41d4a716446655440000",
		}
		for _, uuidName := range uuidNames {
			body := mphValidCreateBody(uuidName)
			rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, body)
			require.Equal(t, http.StatusBadRequest, rec.Code, "uuid name %s: %s", uuidName, rec.Body.String())
			apiErr := decodeErr(t, rec)
			assert.Equal(t, ErrCodeValidationError, apiErr.Code)
			assert.Equal(t, "name", apiErr.Details["field"])
		}

		// PATCH cannot rename an existing policy to a reserved or UUID-shaped name.
		seedRec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, mphValidCreateBody("rename-guard-seed"))
		require.Equal(t, http.StatusCreated, seedRec.Code, seedRec.Body.String())
		var seeded MessagePolicyResponse
		require.NoError(t, json.Unmarshal(seedRec.Body.Bytes(), &seeded))
		for _, badName := range append([]string{"test", "decisions"}, uuidNames...) {
			nameCopy := badName
			patchRec := mphSessionDo(t, f.srv, f.owner, http.MethodPatch, base+"/"+seeded.ID, PatchMessagePolicyRequest{
				Name: &nameCopy,
			})
			require.Equal(t, http.StatusBadRequest, patchRec.Code, "PATCH bad name %s: %s", badName, patchRec.Body.String())
			apiErr := decodeErr(t, patchRec)
			assert.Equal(t, ErrCodeValidationError, apiErr.Code)
			assert.Equal(t, "name", apiErr.Details["field"])
		}

		crossSender := mphValidCreateBody("cross-sender")
		crossSender.Scope.Sender.AgentIDs = []string{f.otherAgent}
		rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, crossSender)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		apiErr := decodeErr(t, rec)
		assert.Equal(t, ErrCodeValidationError, apiErr.Code)
		assert.Equal(t, "scope.sender.agent_ids", apiErr.Details["field"])
	})

	t.Run("unsupported dialect returns 400 unsupported_dialect with reason", func(t *testing.T) {
		fUnreg := newMPHFixture(t, true)
		celEng, err := messagepolicy.NewCELEngine()
		require.NoError(t, err)
		celDialect, err := ruleengine.NewCELDialect(celEng)
		require.NoError(t, err)
		reEng := ruleengine.NewEngine()
		require.NoError(t, reEng.Register(celDialect))
		require.NoError(t, reEng.AllowConsumerDialects(messagepolicy.ConsumerName, ruleengine.Grant(ruleengine.DialectCEL)))
		ev, err := messagepolicy.New(reEng)
		require.NoError(t, err)
		// Revoke CEL grant after evaluator construction so authoring hits
		// dialect_not_allowed and surfaces the structured reason.
		require.NoError(t, reEng.AllowConsumerDialects(messagepolicy.ConsumerName))
		fUnreg.srv.messagePolicyGate.evaluator = ev

		rec := mphSessionDo(t, fUnreg.srv, fUnreg.owner, http.MethodPost, fUnreg.basePath(), mphValidCreateBody("dialect-revoked"))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		apiErr := decodeErr(t, rec)
		assert.Equal(t, messagepolicy.ErrCodeUnsupportedDialect, apiErr.Code)
		assert.Equal(t, string(ruleengine.ReasonDialectNotAllowed), apiErr.Details["reason"])

		// Missing evaluator surfaces consumer_not_registered.
		fUnreg.srv.messagePolicyGate.evaluator = nil
		rec = mphSessionDo(t, fUnreg.srv, fUnreg.owner, http.MethodPost, fUnreg.basePath(), mphValidCreateBody("unregistered-consumer"))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		apiErr = decodeErr(t, rec)
		assert.Equal(t, messagepolicy.ErrCodeUnsupportedDialect, apiErr.Code)
		assert.Equal(t, string(ruleengine.ReasonConsumerNotRegistered), apiErr.Details["reason"])
	})
}

// TestMessagePolicyHandlers_TestEndpoint_16KiBAndContentFree verifies the
// dry-run test endpoint (both inline /test and stored /{policyId}/test):
//   - accepts a full 16 KiB UTF-8 sample body with a realistic case-insensitive
//     keyword alternation regex without tripping cost_limit (target 4b);
//   - classifies missing_input / failed evaluations through on_failed and
//     on_failed_reasons (target 4);
//   - records zero rows in message_policy_decisions and never echoes the
//     sample message body in the HTTP response (target 8).
func TestMessagePolicyHandlers_TestEndpoint_16KiBAndContentFree(t *testing.T) {
	f := newMPHFixture(t, true)
	base := f.basePath()

	const sentinel = "SUPER_SECRET_INLINE_SAMPLE_SENTINEL_99182"
	prefix := strings.Repeat("Routine engineering progress report line with normal prose.\n", 300)
	tail := "\nWould you prefer Option A or Option B? (" + sentinel + ")"
	require.Less(t, len(tail), messagepolicy.MaxBodyBytes)
	full16KiB := prefix[:messagepolicy.MaxBodyBytes-len(tail)] + tail
	require.Equal(t, messagepolicy.MaxBodyBytes, len(full16KiB))

	testReq := TestMessagePolicyRequest{
		Mode:              store.MessagePolicyModeEnforce,
		ViolationRule:     `!message.has_choices && message.body.matches("(?i)(option a|option b|choose one|pick one|1\\)|2\\))")`,
		OnFailed:          store.MessagePolicyOutcomeReject,
		RejectReason:      "Blocked by {policy} for {recipient}",
		RejectRemediation: "Use scion message --choices",
		Sample: &MessagePolicyTestSample{
			Surface:   store.MessagePolicySurfaceAgentToUser,
			Body:      full16KiB,
			Recipient: &messagepolicy.RecipientFacts{Kind: "user", ID: f.owner, Slug: "alice"},
		},
	}
	rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base+"/test", testReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), sentinel, "test response must never echo sample body")

	var testResp TestMessagePolicyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &testResp))
	assert.True(t, testResp.ScopeMatched)
	assert.Equal(t, messagepolicy.ClassViolation, testResp.Class)
	assert.Equal(t, messagepolicy.OutcomeReject, testResp.Outcome)
	assert.Equal(t, store.MessagePolicyActionRejected, testResp.Action)
	assert.Equal(t, "Blocked by inline-test for alice", testResp.Reason)
	assert.Equal(t, "Use scion message --choices", testResp.Remediation)
	assert.Equal(t, messagepolicy.MaxBodyBytes, testResp.BodyBytes)
	assert.False(t, testResp.BodyTruncated)

	// Missing input (judgment_flags.asks_choice absent in M1) resolves via
	// on_failed_reasons["missing_input"] without echoing body text (target 4).
	missingInputReq := TestMessagePolicyRequest{
		Mode:            store.MessagePolicyModeEnforce,
		ViolationRule:   `judgment_flags.asks_choice`,
		OnFailed:        store.MessagePolicyOutcomeReject,
		OnFailedReasons: map[string]string{"missing_input": store.MessagePolicyOutcomeAdmit},
		Sample: &MessagePolicyTestSample{
			Surface: store.MessagePolicySurfaceAgentToUser,
			Body:    "hello " + sentinel,
		},
	}
	rec = mphSessionDo(t, f.srv, f.owner, http.MethodPost, base+"/test", missingInputReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), sentinel)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &testResp))
	assert.Equal(t, messagepolicy.ClassFailed, testResp.Class)
	assert.Equal(t, "missing_input", testResp.ReasonCode)
	assert.Equal(t, messagepolicy.OutcomeAdmit, testResp.Outcome)
	assert.Equal(t, store.MessagePolicyActionAdmitted, testResp.Action)

	// Stored /{policyId}/test also works and writes zero decision rows.
	createRec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, mphValidCreateBody("stored-dry-run"))
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	storedTestRec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base+"/stored-dry-run/test", map[string]any{
		"sample": map[string]any{
			"body": " Benign update with no choices menu (" + sentinel + ").",
		},
	})
	require.Equal(t, http.StatusOK, storedTestRec.Code, storedTestRec.Body.String())
	assert.NotContains(t, storedTestRec.Body.String(), sentinel)
	require.NoError(t, json.Unmarshal(storedTestRec.Body.Bytes(), &testResp))
	assert.Equal(t, "stored-dry-run", testResp.PolicyName)
	assert.Equal(t, messagepolicy.ClassPass, testResp.Class)
	assert.Equal(t, store.MessagePolicyActionShadow, testResp.Action)

	decisions, err := f.s.ListMessagePolicyDecisions(context.Background(),
		store.MessagePolicyDecisionFilter{ProjectID: f.project}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	assert.Empty(t, decisions.Items, "dry-run test endpoint must never persist decision rows")
}

// TestMessagePolicyHandlers_DecisionsContentFreeAndFiltered verifies project
// and per-policy decision listing, filtering, pagination, and the strict
// content-free privacy guarantee (target 8).
func TestMessagePolicyHandlers_DecisionsContentFreeAndFiltered(t *testing.T) {
	f := newMPHFixture(t, true)
	base := f.basePath()
	ctx := context.Background()

	polA := mphValidCreateBody("policy-alpha")
	polA.Mode = store.MessagePolicyModeEnforce
	recA := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, polA)
	require.Equal(t, http.StatusCreated, recA.Code, recA.Body.String())
	var createdA MessagePolicyResponse
	require.NoError(t, json.Unmarshal(recA.Body.Bytes(), &createdA))

	polB := mphValidCreateBody("policy-beta")
	recB := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, polB)
	require.Equal(t, http.StatusCreated, recB.Code, recB.Body.String())
	var createdB MessagePolicyResponse
	require.NoError(t, json.Unmarshal(recB.Body.Bytes(), &createdB))

	require.NoError(t, f.s.CreateMessagePolicyDecision(ctx, &store.MessagePolicyDecision{
		ProjectID:      f.project,
		PolicyID:       createdA.ID,
		PolicyName:     createdA.Name,
		PolicyRevision: 1,
		Mode:           store.MessagePolicyModeEnforce,
		Surface:        store.MessagePolicySurfaceAgentToUser,
		SenderKind:     "agent",
		SenderID:       f.projectAgent,
		RecipientKind:  "user",
		RecipientID:    f.owner,
		Class:          store.MessagePolicyClassViolation,
		Action:         store.MessagePolicyActionRejected,
		Reason:         "Message blocked by policy-alpha for mph-owner",
		ElapsedMs:      2,
	}))
	require.NoError(t, f.s.CreateMessagePolicyDecision(ctx, &store.MessagePolicyDecision{
		ProjectID:      f.project,
		PolicyID:       createdB.ID,
		PolicyName:     createdB.Name,
		PolicyRevision: 1,
		Mode:           store.MessagePolicyModeShadow,
		Surface:        store.MessagePolicySurfaceAgentToConversation,
		SenderKind:     "agent",
		SenderID:       f.projectAgent,
		RecipientKind:  "conversation",
		RecipientID:    tid("mph-conv-123"),
		MessageID:      tid("mph-msg-456"),
		Class:          store.MessagePolicyClassPass,
		Action:         store.MessagePolicyActionShadow,
		ElapsedMs:      1,
	}))

	// Project-wide decisions list with pagination (limit=1).
	page1Rec := mphSessionDo(t, f.srv, f.owner, http.MethodGet, base+"/decisions?limit=1", nil)
	require.Equal(t, http.StatusOK, page1Rec.Code, page1Rec.Body.String())
	var page1 ListMessagePolicyDecisionsResponse
	require.NoError(t, json.Unmarshal(page1Rec.Body.Bytes(), &page1))
	require.Len(t, page1.Decisions, 1)
	assert.Equal(t, 2, page1.TotalCount)
	require.NotEmpty(t, page1.NextCursor)

	page2Rec := mphSessionDo(t, f.srv, f.owner, http.MethodGet, base+"/decisions?limit=1&cursor="+page1.NextCursor, nil)
	require.Equal(t, http.StatusOK, page2Rec.Code, page2Rec.Body.String())
	var page2 ListMessagePolicyDecisionsResponse
	require.NoError(t, json.Unmarshal(page2Rec.Body.Bytes(), &page2))
	require.Len(t, page2.Decisions, 1)
	assert.NotEqual(t, page1.Decisions[0].ID, page2.Decisions[0].ID)

	// Per-policy decisions route (by policy name) + filter by action/class.
	alphaRec := mphSessionDo(t, f.srv, f.owner, http.MethodGet,
		base+"/policy-alpha/decisions?mode=enforce&class=violation&action=rejected", nil)
	require.Equal(t, http.StatusOK, alphaRec.Code, alphaRec.Body.String())
	var alphaDecisions ListMessagePolicyDecisionsResponse
	require.NoError(t, json.Unmarshal(alphaRec.Body.Bytes(), &alphaDecisions))
	require.Len(t, alphaDecisions.Decisions, 1)
	assert.Equal(t, createdA.ID, alphaDecisions.Decisions[0].PolicyID)

	// Verify content-free JSON contract: decision objects carry no "body" or
	// "msg" key anywhere in the response payload.
	var rawPayload map[string]any
	require.NoError(t, json.Unmarshal(alphaRec.Body.Bytes(), &rawPayload))
	rows, ok := rawPayload["decisions"].([]any)
	require.True(t, ok)
	for _, row := range rows {
		m := row.(map[string]any)
		assert.NotContains(t, m, "body")
		assert.NotContains(t, m, "msg")
		assert.NotContains(t, m, "message_body")
	}

	// Malformed cursors on both list endpoints return 400 invalid_cursor.
	for _, badCursorPath := range []string{
		base + "?cursor=not-base64-!!!",
		base + "/decisions?cursor=not-base64-!!!",
	} {
		curRec := mphSessionDo(t, f.srv, f.owner, http.MethodGet, badCursorPath, nil)
		require.Equal(t, http.StatusBadRequest, curRec.Code, "path %s: %s", badCursorPath, curRec.Body.String())
		var curErr ErrorResponse
		require.NoError(t, json.Unmarshal(curRec.Body.Bytes(), &curErr))
		assert.Equal(t, ErrCodeInvalidCursor, curErr.Error.Code)
	}

	// Non-UUID unknown ?policy_id= returns 404 not_found (never invalid_cursor).
	unknownRefRec := mphSessionDo(t, f.srv, f.owner, http.MethodGet, base+"/decisions?policy_id=typo-name", nil)
	require.Equal(t, http.StatusNotFound, unknownRefRec.Code, unknownRefRec.Body.String())
	var unknownErr ErrorResponse
	require.NoError(t, json.Unmarshal(unknownRefRec.Body.Bytes(), &unknownErr))
	assert.Equal(t, ErrCodeNotFound, unknownErr.Error.Code)

	// Deleting a policy still allows querying historical decisions by its UUID,
	// while its former slug name returns 404 not_found.
	delAlphaRec := mphSessionDo(t, f.srv, f.owner, http.MethodDelete, base+"/"+createdA.ID, nil)
	require.Equal(t, http.StatusNoContent, delAlphaRec.Code, delAlphaRec.Body.String())

	histByUUIDRec := mphSessionDo(t, f.srv, f.owner, http.MethodGet, base+"/decisions?policy_id="+createdA.ID, nil)
	require.Equal(t, http.StatusOK, histByUUIDRec.Code, histByUUIDRec.Body.String())
	var histDecisions ListMessagePolicyDecisionsResponse
	require.NoError(t, json.Unmarshal(histByUUIDRec.Body.Bytes(), &histDecisions))
	require.Len(t, histDecisions.Decisions, 1)
	assert.Equal(t, createdA.ID, histDecisions.Decisions[0].PolicyID)

	deletedNameRec := mphSessionDo(t, f.srv, f.owner, http.MethodGet, base+"/decisions?policy_id=policy-alpha", nil)
	require.Equal(t, http.StatusNotFound, deletedNameRec.Code, deletedNameRec.Body.String())
}

func mphIdentityDo(t *testing.T, srv *Server, identity Identity, cred CredentialContext, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	ctx := context.Background()
	if identity != nil {
		ctx = contextWithIdentity(ctx, identity)
	}
	if cred.Kind != "" {
		ctx = contextWithCredentialContext(ctx, cred)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

// TestMessagePolicyAuthz_Target7_CredentialMatrix verifies the full M1 §4
// authorization and credential matrix across Write (POST/PATCH/DELETE), Read
// (GET list and GET by ID), and Decisions/test (GET decisions, GET
// {policyId}/decisions, POST test, POST {policyId}/test), plus 401/405 method
// controls and UAT scope listing (`hub token scopes`).
func TestMessagePolicyAuthz_Target7_CredentialMatrix(t *testing.T) {
	f := newMPHFixture(t, true)
	base := f.basePath()
	ctx := context.Background()

	// Seed users across all §4 roles.
	admin := projectOpsUser(t, f.s, "mph-admin", false)
	projectOpsGrant(t, f.s, admin, f.project, store.ProjectRoleAdmin)

	member := projectOpsUser(t, f.s, "mph-member", false)
	projectOpsGrant(t, f.s, member, f.project, store.ProjectRoleMember)

	viewer := projectOpsUser(t, f.s, "mph-viewer", false)
	projectOpsGrant(t, f.s, viewer, f.project, store.ProjectRoleViewer)

	nonMember := projectOpsUser(t, f.s, "mph-outsider", false)

	localSuperAdmin := tid("mph-superadmin")
	createTestUserWithRole(t, f.s, localSuperAdmin, localSuperAdmin+"@test.com", store.UserRoleAdmin, store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, f.s, localSuperAdmin)

	localHubAdmin := tid("mph-hubadmin")
	createTestUserWithRole(t, f.s, localHubAdmin, localHubAdmin+"@test.com", "member", store.SystemRoleHubAdmin)
	ensureHubMembership(ctx, f.s, localHubAdmin)

	// Seed a baseline policy so Read/PATCH/DELETE/{policyId}/test/{policyId}/decisions
	// always have a live target.
	seedRec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, mphValidCreateBody("baseline-policy"))
	require.Equal(t, http.StatusCreated, seedRec.Code, seedRec.Body.String())
	var baseline MessagePolicyResponse
	require.NoError(t, json.Unmarshal(seedRec.Body.Bytes(), &baseline))

	testPayload := map[string]any{
		"violation_rule": `message.body.contains("option a")`,
		"on_failed":      "reject",
		"sample":         map[string]any{"body": "pick option a"},
	}
	storedTestPayload := map[string]any{
		"sample": map[string]any{"body": "benign status"},
	}

	type callFn func(method, path string, body any) *httptest.ResponseRecorder
	sessionCaller := func(userID, userRole string) callFn {
		return func(method, path string, body any) *httptest.ResponseRecorder {
			ident := NewAuthenticatedUser(userID, userID+"@test.com", userID, userRole, string(ClientTypeAPI))
			return mphIdentityDo(t, f.srv, ident, CredentialContext{Kind: CredentialKindInteractive, ID: "sess-" + userID}, method, path, body)
		}
	}
	devCaller := func(userID, userRole string) callFn {
		return func(method, path string, body any) *httptest.ResponseRecorder {
			ident := NewAuthenticatedUser(userID, userID+"@test.com", userID, userRole, string(ClientTypeCLI))
			return mphIdentityDo(t, f.srv, ident, CredentialContext{Kind: CredentialKindDev, ID: "dev-" + userID}, method, path, body)
		}
	}
	tokenCaller := func(token string) callFn {
		return func(method, path string, body any) *httptest.ResponseRecorder {
			return doRequestWithToken(t, f.srv, token, method, path, body)
		}
	}

	seq := 0
	assertMatrix := func(t *testing.T, label string, call callFn, wantWriteOK, wantReadOK, wantManageOpsOK bool) {
		t.Helper()
		seq++
		polName := "matrix-pol-" + strings.ReplaceAll(strings.ToLower(label), " ", "-") + "-" + string(rune('a'+(seq%20)))
		polName = strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				return r
			}
			return '-'
		}, polName)
		for strings.Contains(polName, "--") {
			polName = strings.ReplaceAll(polName, "--", "-")
		}
		polName = strings.Trim(polName, "-")
		if len(polName) > 48 {
			polName = strings.Trim(polName[:48], "-")
		}

		// --- Read column (list + get) ---
		listRec := call(http.MethodGet, base, nil)
		getRec := call(http.MethodGet, base+"/"+baseline.ID, nil)
		if wantReadOK {
			assert.Equal(t, http.StatusOK, listRec.Code, "%s GET list: %s", label, listRec.Body.String())
			assert.Equal(t, http.StatusOK, getRec.Code, "%s GET by ID: %s", label, getRec.Body.String())
		} else {
			assert.Equal(t, http.StatusForbidden, listRec.Code, "%s GET list must be 403: %s", label, listRec.Body.String())
			assert.Equal(t, http.StatusForbidden, getRec.Code, "%s GET by ID must be 403: %s", label, getRec.Body.String())
		}

		// --- Decisions / test column ---
		decRec := call(http.MethodGet, base+"/decisions", nil)
		polDecRec := call(http.MethodGet, base+"/"+baseline.ID+"/decisions", nil)
		inlineTestRec := call(http.MethodPost, base+"/test", testPayload)
		storedTestRec := call(http.MethodPost, base+"/"+baseline.ID+"/test", storedTestPayload)
		if wantManageOpsOK {
			assert.Equal(t, http.StatusOK, decRec.Code, "%s GET decisions: %s", label, decRec.Body.String())
			assert.Equal(t, http.StatusOK, polDecRec.Code, "%s GET policy decisions: %s", label, polDecRec.Body.String())
			assert.Equal(t, http.StatusOK, inlineTestRec.Code, "%s POST test: %s", label, inlineTestRec.Body.String())
			assert.Equal(t, http.StatusOK, storedTestRec.Code, "%s POST policy test: %s", label, storedTestRec.Body.String())
		} else {
			assert.Equal(t, http.StatusForbidden, decRec.Code, "%s GET decisions must be 403: %s", label, decRec.Body.String())
			assert.Equal(t, http.StatusForbidden, polDecRec.Code, "%s GET policy decisions must be 403: %s", label, polDecRec.Body.String())
			assert.Equal(t, http.StatusForbidden, inlineTestRec.Code, "%s POST test must be 403: %s", label, inlineTestRec.Body.String())
			assert.Equal(t, http.StatusForbidden, storedTestRec.Code, "%s POST policy test must be 403: %s", label, storedTestRec.Body.String())
		}

		// --- Write column (create, patch, delete) ---
		if wantWriteOK {
			cRec := call(http.MethodPost, base, mphValidCreateBody(polName))
			require.Equal(t, http.StatusCreated, cRec.Code, "%s POST create (%s): %s", label, polName, cRec.Body.String())
			var created MessagePolicyResponse
			require.NoError(t, json.Unmarshal(cRec.Body.Bytes(), &created))

			desc := "updated by " + label
			pRec := call(http.MethodPatch, base+"/"+created.ID, PatchMessagePolicyRequest{Description: &desc})
			assert.Equal(t, http.StatusOK, pRec.Code, "%s PATCH: %s", label, pRec.Body.String())

			dRec := call(http.MethodDelete, base+"/"+created.ID, nil)
			assert.Equal(t, http.StatusNoContent, dRec.Code, "%s DELETE: %s", label, dRec.Body.String())
		} else {
			cRec := call(http.MethodPost, base, mphValidCreateBody(polName))
			assert.Equal(t, http.StatusForbidden, cRec.Code, "%s POST create must be 403: %s", label, cRec.Body.String())

			desc := "denied update"
			pRec := call(http.MethodPatch, base+"/"+baseline.ID, PatchMessagePolicyRequest{Description: &desc})
			assert.Equal(t, http.StatusForbidden, pRec.Code, "%s PATCH must be 403: %s", label, pRec.Body.String())

			dRec := call(http.MethodDelete, base+"/"+baseline.ID, nil)
			assert.Equal(t, http.StatusForbidden, dRec.Code, "%s DELETE must be 403: %s", label, dRec.Body.String())
		}
	}

	// Row 1: Owner or admin, interactive session -> allow / allow / allow.
	t.Run("owner and admin interactive sessions", func(t *testing.T) {
		assertMatrix(t, "owner-session", sessionCaller(f.owner, "member"), true, true, true)
		assertMatrix(t, "admin-session", sessionCaller(admin, "member"), true, true, true)
	})

	// Row 2: Owner or admin, dev credential (workstation hub) -> allow / allow / allow.
	t.Run("owner and admin dev credentials", func(t *testing.T) {
		assertMatrix(t, "owner-dev", devCaller(f.owner, "member"), true, true, true)
		assertMatrix(t, "admin-dev", devCaller(admin, "member"), true, true, true)

		// Also verify the workstation DevUser struct itself.
		projectOpsGrant(t, f.s, DevUserID, f.project, store.ProjectRoleOwner)
		devUser := NewDevUser(DevUserConfig{Username: "workstation-dev", Email: "dev@localhost"})
		assertMatrix(t, "workstation-devuser", func(method, path string, body any) *httptest.ResponseRecorder {
			return mphIdentityDo(t, f.srv, devUser, CredentialContext{Kind: CredentialKindDev, ID: "dev-tok"}, method, path, body)
		}, true, true, true)
	})

	// Row 3: Owner or admin UAT with project:manage_message_policies (project or hub boundary)
	// -> allow / allow (manage satisfies read!) / allow; and with project:read_message_policies
	// only -> 403 / allow / 403.
	t.Run("owner and admin scoped UATs with manage or read selectors", func(t *testing.T) {
		for _, boundary := range []TokenBoundary{projectBoundary(f.project), hubBoundary()} {
			ownerManageTok := projectOpsMint(t, f.srv, f.owner, boundary, "project:manage_message_policies")
			assertMatrix(t, "owner-uat-manage-"+string(boundary.Kind), tokenCaller(ownerManageTok), true, true, true)

			adminManageTok := projectOpsMint(t, f.srv, admin, boundary, "project:manage_message_policies")
			assertMatrix(t, "admin-uat-manage-"+string(boundary.Kind), tokenCaller(adminManageTok), true, true, true)

			ownerReadTok := projectOpsMint(t, f.srv, f.owner, boundary, "project:read_message_policies")
			assertMatrix(t, "owner-uat-read-"+string(boundary.Kind), tokenCaller(ownerReadTok), false, true, false)
		}
	})

	// Row 4: Owner or admin UAT without the selector, or bounded to another project -> 403.
	t.Run("owner UAT without selector or with other-project boundary", func(t *testing.T) {
		noSelectorTok := projectOpsMint(t, f.srv, f.owner, projectBoundary(f.project), "project:read", "project:manage")
		assertMatrix(t, "owner-uat-no-selector", tokenCaller(noSelectorTok), false, false, false)

		otherProjManageTok := projectOpsMint(t, f.srv, f.owner, projectBoundary(f.otherProject),
			"project:manage_message_policies", "project:read_message_policies")
		assertMatrix(t, "owner-uat-other-project", tokenCaller(otherProjManageTok), false, false, false)
	})

	// Row 5 & Row 6: Project member (any user credential) and viewer -> 403 / allow / 403;
	// non-member -> 403 / 403 / 403.
	t.Run("member viewer and non-member", func(t *testing.T) {
		assertMatrix(t, "member-session", sessionCaller(member, "member"), false, true, false)
		assertMatrix(t, "member-dev", devCaller(member, "member"), false, true, false)

		memberReadTok := projectOpsMint(t, f.srv, member, projectBoundary(f.project), "project:read_message_policies")
		assertMatrix(t, "member-uat-read", tokenCaller(memberReadTok), false, true, false)

		memberNoSelTok := projectOpsMint(t, f.srv, member, projectBoundary(f.project), "project:read")
		assertMatrix(t, "member-uat-no-selector", tokenCaller(memberNoSelTok), false, false, false)

		// Member cannot mint project:manage_message_policies.
		_, _, mintErr := f.srv.uatService.CreateTokenWithParams(rs4MintContext(member), CreateTokenParams{
			UserID:   member,
			Name:     "member-illegal-manage",
			Boundary: projectBoundary(f.project),
			Scopes:   []string{"project:manage_message_policies"},
		})
		assert.Error(t, mintErr, "member must not be able to mint project:manage_message_policies")

		// Defense-in-depth: even if a non-owner/non-admin holds a custom role
		// granting project.manage_message_policies, messagePolicyOwnerOrAdminRule
		// still denies Write and Decisions/test with 403.
		customRoleID := tid("mph-custom-role")
		_, err := f.s.CreateRoleDefinition(ctx, &store.RoleDefinition{
			ID:          customRoleID,
			Name:        "custom-mp-mgr",
			Description: "Custom MP Mgr",
			ScopeType:   store.RoleScopeProject,
			Permissions: []string{"project.manage_message_policies", "project.read_message_policies"},
		})
		require.NoError(t, err)
		customHolder := projectOpsUser(t, f.s, "mph-custom-holder", false)
		_, err = f.s.CreateRoleBinding(ctx, &store.RoleBinding{
			ID:               tid("mph-custom-rb"),
			RoleDefinitionID: customRoleID,
			PrincipalType:    "user",
			PrincipalID:      customHolder,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          f.project,
		})
		require.NoError(t, err)
		assertMatrix(t, "custom-role-not-owner-admin", sessionCaller(customHolder, "member"), false, true, false)

		assertMatrix(t, "viewer-session", sessionCaller(viewer, "member"), false, true, false)
		viewerReadTok := projectOpsMint(t, f.srv, viewer, projectBoundary(f.project), "project:read_message_policies")
		assertMatrix(t, "viewer-uat-read", tokenCaller(viewerReadTok), false, true, false)

		assertMatrix(t, "outsider-session", sessionCaller(nonMember, "member"), false, false, false)
	})

	// Row 7: Agent of the project (own credential) -> 403 / allow / 403;
	// cross-project agent and scope-less agent -> 403 / 403 / 403.
	t.Run("project agent own credential vs cross-project or missing scope", func(t *testing.T) {
		projAgentIdent := dcAgentIdentity(f.projectAgent, f.project, AgentRoleBaseline)
		assertMatrix(t, "project-agent-own-jwt", func(method, path string, body any) *httptest.ResponseRecorder {
			return mphIdentityDo(t, f.srv, projAgentIdent, CredentialContext{Kind: CredentialKindAgentJWT, ID: "ajwt-1"}, method, path, body)
		}, false, true, false)

		noScopeClaims := &AgentTokenClaims{
			ProjectID: f.project,
			Scopes:    []AgentTokenScope{ScopeAgentStatusUpdate},
		}
		noScopeClaims.Subject = f.projectAgent
		noScopeAgentIdent := &agentIdentityWrapper{noScopeClaims}
		assertMatrix(t, "project-agent-without-project-read", func(method, path string, body any) *httptest.ResponseRecorder {
			return mphIdentityDo(t, f.srv, noScopeAgentIdent, CredentialContext{Kind: CredentialKindAgentJWT, ID: "ajwt-noscope"}, method, path, body)
		}, false, false, false)

		crossAgentIdent := dcAgentIdentity(f.otherAgent, f.otherProject, AgentRoleBaseline)
		assertMatrix(t, "other-project-agent", func(method, path string, body any) *httptest.ResponseRecorder {
			return mphIdentityDo(t, f.srv, crossAgentIdent, CredentialContext{Kind: CredentialKindAgentJWT, ID: "ajwt-other"}, method, path, body)
		}, false, false, false)
	})

	// Row 8: Agent-delegated credential, broker / broker on-behalf-of,
	// hub-delivery, federation, and visitor -> 403 across ALL columns.
	t.Run("disallowed credentials and principals get 403 across all columns", func(t *testing.T) {
		delegated := &DelegatedAgentIdentity{
			agentID:           f.projectAgent,
			agentProjectID:    f.project,
			authorizingUserID: f.owner,
			grantID:           "grant-1",
			credentialID:      "dcred-1",
			ceiling: permissions.FrozenPermissionCeiling{
				Version:       permissions.CeilingVersionV1,
				PermissionIDs: []string{"project.read_message_policies", "project.manage_message_policies"},
			},
		}
		assertMatrix(t, "agent-delegated", func(method, path string, body any) *httptest.ResponseRecorder {
			return mphIdentityDo(t, f.srv, delegated, CredentialContext{Kind: CredentialKindDelegatedAgent, ID: "dcred-1"}, method, path, body)
		}, false, false, false)

		brokerIdent := NewBrokerIdentity(tid("mph-broker"))
		assertMatrix(t, "broker-direct", func(method, path string, body any) *httptest.ResponseRecorder {
			return mphIdentityDo(t, f.srv, brokerIdent, CredentialContext{Kind: CredentialKindBroker, ID: "brk-1"}, method, path, body)
		}, false, false, false)

		ownerUser := NewAuthenticatedUser(f.owner, f.owner+"@test.com", f.owner, "member", string(ClientTypeAPI))
		assertMatrix(t, "broker-on-behalf-of-owner", func(method, path string, body any) *httptest.ResponseRecorder {
			return mphIdentityDo(t, f.srv, ownerUser, CredentialContext{Kind: CredentialKindBroker, ID: "brk-obo"}, method, path, body)
		}, false, false, false)

		assertMatrix(t, "hub-delivery", func(method, path string, body any) *httptest.ResponseRecorder {
			return mphIdentityDo(t, f.srv, ownerUser, CredentialContext{Kind: CredentialKindHubDelivery, ID: "hdel-1"}, method, path, body)
		}, false, false, false)

		fedUser := NewFederatedUserIdentity("https://issuer.example", "fed-sub", f.owner+"@test.com", "Fed Owner", "admin", nil)
		assertMatrix(t, "federated-user", func(method, path string, body any) *httptest.ResponseRecorder {
			return mphIdentityDo(t, f.srv, fedUser, CredentialContext{Kind: CredentialKindFederation, ID: "fed-1"}, method, path, body)
		}, false, false, false)

		fedAgent := NewFederatedAgentIdentity("https://issuer.example", f.projectAgent, f.project, "mph-agent", f.owner, nil, nil)
		assertMatrix(t, "federated-agent", func(method, path string, body any) *httptest.ResponseRecorder {
			return mphIdentityDo(t, f.srv, fedAgent, CredentialContext{Kind: CredentialKindFederation, ID: "fed-ag"}, method, path, body)
		}, false, false, false)

		issuer := "https://issuer.example"
		visEmail := "visitor@example.com"
		visUser := &store.User{
			DisplayName: "Visitor", Kind: store.UserKindFederated, Status: store.UserStatusActive,
			HomeIssuer: &issuer, FederatedEmail: &visEmail,
		}
		require.NoError(t, f.s.CreateFederatedUser(ctx, visUser))
		_, err := f.s.CreateVisitorRoleBinding(ctx, &store.RoleBinding{
			PrincipalType: store.RoleBindingPrincipalUser,
			PrincipalID:   visUser.ID,
			ScopeType:     store.RoleScopeProject,
			ScopeID:       f.project,
			CreatedBy:     f.owner,
		})
		require.NoError(t, err)
		visitorIdent := NewVisitorIdentity(visUser.ID, issuer, "vis-sub", visEmail, "Visitor")
		assertMatrix(t, "visitor", func(method, path string, body any) *httptest.ResponseRecorder {
			return mphIdentityDo(t, f.srv, visitorIdent, CredentialContext{Kind: CredentialKindFederation, ID: "vis-1"}, method, path, body)
		}, false, false, false)
	})

	// Row 9: Unscoped local Hub admin (both super-admin and hub-admin) -> allow / allow / allow.
	t.Run("unscoped local hub admins", func(t *testing.T) {
		assertMatrix(t, "local-super-admin", sessionCaller(localSuperAdmin, store.UserRoleAdmin), true, true, true)
		assertMatrix(t, "local-hub-admin", sessionCaller(localHubAdmin, "member"), true, true, true)
	})

	// Unauthenticated (nil identity) -> 401; unsupported HTTP methods with
	// experiment ON -> 405; and UAT scope listing (`hub token scopes`) exposes
	// both selectors with accurate per-caller eligibility.
	t.Run("unauthenticated 401 unsupported method 405 and token scopes listing", func(t *testing.T) {
		unauthRec := mphIdentityDo(t, f.srv, nil, CredentialContext{}, http.MethodGet, base, nil)
		assert.Equal(t, http.StatusUnauthorized, unauthRec.Code)

		methodChecks := []struct {
			method string
			path   string
		}{
			{http.MethodPut, base},
			{http.MethodDelete, base},
			{http.MethodGet, base + "/test"},
			{http.MethodPost, base + "/decisions"},
			{http.MethodPut, base + "/" + baseline.ID},
			{http.MethodPost, base + "/" + baseline.ID},
			{http.MethodGet, base + "/" + baseline.ID + "/test"},
			{http.MethodPost, base + "/" + baseline.ID + "/decisions"},
		}
		for _, mc := range methodChecks {
			rec := mphSessionDo(t, f.srv, f.owner, mc.method, mc.path, nil)
			assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "%s %s should return 405: %s", mc.method, mc.path, rec.Body.String())
		}

		// Verify permissions.UATScopeOptions(false) and GET /api/v1/auth/scopes.
		pickerScopes := make([]string, 0)
		for _, opt := range permissions.UATScopeOptions(false) {
			pickerScopes = append(pickerScopes, opt.UATScope)
		}
		assert.Contains(t, pickerScopes, "project:manage_message_policies")
		assert.Contains(t, pickerScopes, "project:read_message_policies")

		ownerScopesRec := mphSessionDo(t, f.srv, f.owner, http.MethodGet,
			"/api/v1/auth/scopes?boundary=project&projectId="+f.project, nil)
		require.Equal(t, http.StatusOK, ownerScopesRec.Code, ownerScopesRec.Body.String())
		assert.Contains(t, ownerScopesRec.Body.String(), `"project:manage_message_policies"`)
		assert.Contains(t, ownerScopesRec.Body.String(), `"project:read_message_policies"`)
	})
}

// TestMessagePolicyHandlers_CanonicalizeSenderAgentIDsAndEnforceMatches verifies R1:
// non-canonical sender agent_ids (leading/trailing whitespace, uppercase hex,
// unhyphenated 32-hex, braced/URN UUIDs, and duplicate representations) are
// persisted in canonical lowercase UUID form across create, patch, and dry-run
// test, and that an enforce policy authored or patched with non-canonical
// sender agent_ids matches at send time and rejects with HTTP 422.
func TestMessagePolicyHandlers_CanonicalizeSenderAgentIDsAndEnforceMatches(t *testing.T) {
	f := newMpgFixture(t, true)
	ctx := context.Background()
	owner := projectOpsUser(t, f.s, "mph-r1-owner", false)
	projectOpsGrant(t, f.s, owner, f.project.ID, store.ProjectRoleOwner)
	base := "/api/v1/projects/" + f.project.ID + "/message-policies"

	upperPaddedSender := "  " + strings.ToUpper(f.sender.ID) + "\t"
	unhyphenatedPeer := strings.ReplaceAll(f.peerAgent.ID, "-", "")
	bracedDuplicateSender := "{" + strings.ToUpper(f.sender.ID) + "}"
	require.NotEqual(t, f.sender.ID, upperPaddedSender)
	require.NotEqual(t, f.peerAgent.ID, unhyphenatedPeer)

	// 1. Create an enforce policy with non-canonical sender agent_ids (plus
	// whitespace-padded on_failed to verify trim-once consistency).
	createBody := CreateMessagePolicyRequest{
		Name: "canonical-sender-enforce",
		Mode: store.MessagePolicyModeEnforce,
		Scope: store.MessagePolicyScope{
			Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
			Sender: store.MessagePolicySenderSelector{
				AgentIDs: []string{upperPaddedSender, unhyphenatedPeer, bracedDuplicateSender},
			},
		},
		ViolationRule:     `message.body.contains("BLOCK_NONCANON")`,
		OnFailed:          "  reject  ",
		RejectReason:      "Blocked by {policy} for {recipient}",
		RejectRemediation: "Remove forbidden token",
	}
	createRec := mphSessionDo(t, f.srv, owner, http.MethodPost, base, createBody)
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())

	var created MessagePolicyResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	assert.Equal(t, []string{f.sender.ID, f.peerAgent.ID}, created.Scope.Sender.AgentIDs,
		"create response must return canonical deduplicated sender agent IDs")
	assert.Equal(t, store.MessagePolicyOutcomeReject, created.OnFailed)

	stored, err := f.s.GetMessagePolicy(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{f.sender.ID, f.peerAgent.ID}, stored.Scope.Sender.AgentIDs,
		"persisted scope.sender.agent_ids must be canonical lowercase UUIDs")

	// 2. Live H2 gate send from f.sender matches the canonicalized enforce policy -> 422.
	sendRec := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Please review BLOCK_NONCANON payload",
	})
	require.Equal(t, http.StatusUnprocessableEntity, sendRec.Code, "enforce policy with non-canonical authored sender ID must match and reject: %s", sendRec.Body.String())
	var sendErr ErrorResponse
	require.NoError(t, json.Unmarshal(sendRec.Body.Bytes(), &sendErr))
	assert.Equal(t, string(MessageDenialPolicyRejected), sendErr.Error.Code)
	assert.Equal(t, "canonical-sender-enforce", sendErr.Error.Details["policy"])
	assert.Equal(t, 0, f.recordChannel.count(), "rejected send must not dispatch to channel")

	decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: f.project.ID,
		PolicyID:  created.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, decs.Items, 1)
	assert.Equal(t, store.MessagePolicyActionRejected, decs.Items[0].Action)
	assert.Equal(t, f.sender.ID, decs.Items[0].SenderID)

	// 3. PATCH with unhyphenated uppercase sender ID also persists canonical ID and matches at send time.
	unhyphenatedUpperSender := " " + strings.ToUpper(strings.ReplaceAll(f.sender.ID, "-", "")) + " "
	patchScope := store.MessagePolicyScope{
		Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
		Sender: store.MessagePolicySenderSelector{
			AgentIDs: []string{unhyphenatedUpperSender},
		},
	}
	patchRec := mphSessionDo(t, f.srv, owner, http.MethodPatch, base+"/"+created.ID, PatchMessagePolicyRequest{
		Scope: &patchScope,
	})
	require.Equal(t, http.StatusOK, patchRec.Code, patchRec.Body.String())

	var patched MessagePolicyResponse
	require.NoError(t, json.Unmarshal(patchRec.Body.Bytes(), &patched))
	assert.Equal(t, []string{f.sender.ID}, patched.Scope.Sender.AgentIDs)

	storedAfterPatch, err := f.s.GetMessagePolicy(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{f.sender.ID}, storedAfterPatch.Scope.Sender.AgentIDs)

	sendRec2 := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Second BLOCK_NONCANON attempt after patch",
	})
	require.Equal(t, http.StatusUnprocessableEntity, sendRec2.Code, "patched enforce policy must still match and reject: %s", sendRec2.Body.String())

	// 4. Dry-run /test with non-canonical inline scope.sender.agent_ids and
	// non-canonical sample.sender.id canonicalizes both and matches scope.
	dryRunRec := mphSessionDo(t, f.srv, owner, http.MethodPost, base+"/test", TestMessagePolicyRequest{
		Mode: store.MessagePolicyModeEnforce,
		Scope: &store.MessagePolicyScope{
			Surfaces: []string{store.MessagePolicySurfaceAgentToUser},
			Sender: store.MessagePolicySenderSelector{
				AgentIDs: []string{unhyphenatedUpperSender},
			},
		},
		ViolationRule: `message.body.contains("BLOCK_NONCANON")`,
		OnFailed:      " reject ",
		Sample: &MessagePolicyTestSample{
			Surface: store.MessagePolicySurfaceAgentToUser,
			Body:    "BLOCK_NONCANON dry run",
			Sender:  &messagepolicy.SenderFacts{ID: "  " + strings.ToUpper(f.sender.ID) + "  "},
		},
	})
	require.Equal(t, http.StatusOK, dryRunRec.Code, dryRunRec.Body.String())
	var dryRunResp TestMessagePolicyResponse
	require.NoError(t, json.Unmarshal(dryRunRec.Body.Bytes(), &dryRunResp))
	assert.True(t, dryRunResp.ScopeMatched, "dry-run must match after canonicalizing sender IDs")
	assert.Equal(t, messagepolicy.ClassViolation, dryRunResp.Class)
	assert.Equal(t, store.MessagePolicyActionRejected, dryRunResp.Action)
}

// TestMessagePolicyHandlers_UnreachableRecipientKinds400 verifies O3:
// authoring or patching a policy whose scope.recipient.kinds cannot be reached
// from any of its scope.surfaces returns HTTP 400 validation_error.
func TestMessagePolicyHandlers_UnreachableRecipientKinds400(t *testing.T) {
	f := newMPHFixture(t, true)
	base := f.basePath()

	for _, tc := range []struct {
		name       string
		policyName string
		surfaces   []string
		kinds      []string
	}{
		{
			name:       "agent_to_user cannot reach conversation kind",
			policyName: "unreach-kind-user",
			surfaces:   []string{store.MessagePolicySurfaceAgentToUser},
			kinds:      []string{"conversation"},
		},
		{
			name:       "agent_to_conversation cannot reach user kind",
			policyName: "unreach-kind-conv",
			surfaces:   []string{store.MessagePolicySurfaceAgentToConversation},
			kinds:      []string{"user"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := mphValidCreateBody(tc.policyName)
			body.Scope.Surfaces = tc.surfaces
			body.Scope.Recipient.Kinds = tc.kinds
			rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			var env ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
			assert.Equal(t, ErrCodeValidationError, env.Error.Code)
			assert.Equal(t, "scope.recipient.kinds", env.Error.Details["field"])
		})
	}

	// PATCH narrowing surfaces so stored recipient.kinds becomes unreachable also returns 400.
	seedBody := mphValidCreateBody("patch-unreach-seed")
	seedBody.Scope.Recipient.Kinds = []string{"conversation"}
	seedRec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, seedBody)
	require.Equal(t, http.StatusCreated, seedRec.Code, seedRec.Body.String())

	patchScope := store.MessagePolicyScope{
		Surfaces:  []string{store.MessagePolicySurfaceAgentToUser},
		Recipient: store.MessagePolicyRecipientSelector{Kinds: []string{"conversation"}},
	}
	patchRec := mphSessionDo(t, f.srv, f.owner, http.MethodPatch, base+"/patch-unreach-seed", PatchMessagePolicyRequest{
		Scope: &patchScope,
	})
	require.Equal(t, http.StatusBadRequest, patchRec.Code, patchRec.Body.String())
	var patchErr ErrorResponse
	require.NoError(t, json.Unmarshal(patchRec.Body.Bytes(), &patchErr))
	assert.Equal(t, ErrCodeValidationError, patchErr.Error.Code)
	assert.Equal(t, "scope.recipient.kinds", patchErr.Error.Details["field"])
}
