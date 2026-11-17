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

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
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
	}))
	otherAgent := tid("mph-other-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:        otherAgent,
		Slug:      "mph-other-agent",
		Name:      "mph-other-agent",
		ProjectID: otherProject,
	}))

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
		nonBoolBody.ViolationRule = `message.body`
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

	t.Run("non-empty system_one_rule returns 400 not_yet_supported", func(t *testing.T) {
		body := mphValidCreateBody("m2-rule")
		body.SystemOneRule = `judge whether the message asks a choice`
		rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, body)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		apiErr := decodeErr(t, rec)
		assert.Equal(t, messagepolicy.ErrCodeNotYetSupported, apiErr.Code)
		assert.Contains(t, apiErr.Message, "not yet supported")
	})

	t.Run("uncovered surface or agent recipient selector returns 400 surface_not_covered (target 6)", func(t *testing.T) {
		for _, surf := range []string{"agent_dm", "group_post", "group_set", "mention_fanout", "mention_delivery", "broadcast"} {
			body := mphValidCreateBody("uncovered-surf")
			body.Scope.Surfaces = []string{surf}
			rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, body)
			require.Equal(t, http.StatusBadRequest, rec.Code, "surface %s: %s", surf, rec.Body.String())
			apiErr := decodeErr(t, rec)
			assert.Equal(t, messagepolicy.ErrCodeSurfaceNotCovered, apiErr.Code)
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

	t.Run("reserved names and cross-project sender agent_ids return 400", func(t *testing.T) {
		for _, reserved := range []string{"test", "decisions"} {
			body := mphValidCreateBody(reserved)
			rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, body)
			require.Equal(t, http.StatusBadRequest, rec.Code, "reserved name %s: %s", reserved, rec.Body.String())
			assert.Equal(t, ErrCodeValidationError, decodeErr(t, rec).Code)
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
		// Engine has CEL dialect, but "message_policy" consumer is NOT registered.
		unregEngine := ruleengine.NewEngine()
		require.NoError(t, unregEngine.Register(celDialect))
		ev, err := messagepolicy.New(unregEngine)
		require.NoError(t, err)
		fUnreg.srv.messagePolicyGate.evaluator = ev

		rec := mphSessionDo(t, fUnreg.srv, fUnreg.owner, http.MethodPost, fUnreg.basePath(), mphValidCreateBody("unregistered-consumer"))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		apiErr := decodeErr(t, rec)
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
	prefix := strings.Repeat("Routine engineering progress report line with normal prose.\n", 260)
	tail := "\nWould you prefer Option A or Option B? (" + sentinel + ")"
	require.Less(t, len(tail), messagepolicy.MaxBodyBytes)
	full16KiB := prefix[:messagepolicy.MaxBodyBytes-len(tail)] + tail
	require.Equal(t, messagepolicy.MaxBodyBytes, len(full16KiB))

	testReq := TestMessagePolicyRequest{
		Mode:          store.MessagePolicyModeEnforce,
		ViolationRule: `!message.has_choices && message.body.matches("(?i)(option a|option b|choose one|pick one|1\\)|2\\))")`,
		OnFailed:      store.MessagePolicyOutcomeReject,
		RejectReason:  "Blocked by {policy} for {recipient}",
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
		RecipientID:    "conv-123",
		MessageID:      "msg-456",
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
}
