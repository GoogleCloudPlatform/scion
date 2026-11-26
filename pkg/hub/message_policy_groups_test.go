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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
)

func mpgGroupBinaryRule(instanceName, instructions string) string {
	return fmt.Sprintf(
		`{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"binding":{"instance":%q},"questions":[{"id":"asks_choice","type":"binary","instructions":%q}]}}`,
		instanceName, instructions,
	)
}

func TestHubGroups_EnforceOneExecutionPerGroupAndProvenance(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	var providerCalls atomic.Int32
	var failMode atomic.Bool
	mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-grp", func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		if failMode.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"upstream_unavailable"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "jev-1.14.0",
			"answers": {"asks_choice": {"type": "noul", "noul": 0.92}},
			"usage": {"input_tokens": 25, "output_tokens": 2}
		}`))
	})

	rule := mpgGroupBinaryRule("ts-grp", "Does the message ask the recipient to choose?")

	// Create 3 enforce policies in non-sorted name order with distinct CEL
	// violation_rules and distinct on_failed_reasons maps.
	pols := []*store.MessagePolicy{
		{
			ProjectID:       f.project.ID,
			Name:            "z-enforce",
			Enabled:         true,
			Mode:            store.MessagePolicyModeEnforce,
			Scope:           store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule:   `!judgment_flags.asks_choice`, // passes when asks_choice=true
			SystemOneRule:   rule,
			SystemOneRefs:   []string{"ts-grp"},
			OnUncertain:     store.MessagePolicyOutcomeReject,
			OnFailed:        store.MessagePolicyOutcomeReject,
			OnFailedReasons: map[string]string{string(provider.FailUnavailable): store.MessagePolicyOutcomeAdmit},
		},
		{
			ProjectID:       f.project.ID,
			Name:            "a-enforce",
			Enabled:         true,
			Mode:            store.MessagePolicyModeEnforce,
			Scope:           store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule:   `judgment_flags.asks_choice`, // violates when asks_choice=true
			SystemOneRule:   rule,
			SystemOneRefs:   []string{"ts-grp"},
			OnUncertain:     store.MessagePolicyOutcomeReject,
			OnFailed:        store.MessagePolicyOutcomeAdmit,
			OnFailedReasons: map[string]string{string(provider.FailUnavailable): store.MessagePolicyOutcomeReject},
			RejectReason:    "a-enforce rejected",
		},
		{
			ProjectID:       f.project.ID,
			Name:            "m-enforce",
			Enabled:         true,
			Mode:            store.MessagePolicyModeEnforce,
			Scope:           store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule:   `!judgment_flags.asks_choice`, // passes when asks_choice=true
			SystemOneRule:   rule,
			SystemOneRefs:   []string{"ts-grp"},
			OnUncertain:     store.MessagePolicyOutcomeReject,
			OnFailed:        store.MessagePolicyOutcomeReject,
			OnFailedReasons: map[string]string{string(provider.FailUnavailable): store.MessagePolicyOutcomeAdmit},
		},
	}
	for _, p := range pols {
		require.NoError(t, f.s.CreateMessagePolicy(ctx, p))
	}

	// Send 1: provider succeeds with asks_choice=true. Expect 1 provider call,
	// 3 decision rows (a-enforce rejected, m-enforce & z-enforce admitted),
	// a-enforce is primary (lowest name), m-enforce & z-enforce carry
	// cache_hit=true and source_eval_id == primary eval_id.
	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Would you like option A or B?",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Equal(t, int32(1), providerCalls.Load(), "3 identical enforce policies must execute once")

	decs := mpgWaitForDecisionCount(t, f.s, f.project.ID, 3)
	require.Len(t, decs, 3)

	byName := make(map[string]store.MessagePolicyDecision, len(decs))
	projByName := make(map[string]messagepolicy.EvaluationProjection, len(decs))
	for _, d := range decs {
		byName[d.PolicyName] = d
		var proj messagepolicy.EvaluationProjection
		require.NoError(t, json.Unmarshal(d.Evaluation, &proj))
		projByName[d.PolicyName] = proj
	}

	assert.Equal(t, store.MessagePolicyClassViolation, byName["a-enforce"].Class)
	assert.Equal(t, store.MessagePolicyActionRejected, byName["a-enforce"].Action)
	assert.Equal(t, store.MessagePolicyClassPass, byName["m-enforce"].Class)
	assert.Equal(t, store.MessagePolicyActionAdmitted, byName["m-enforce"].Action)
	assert.Equal(t, store.MessagePolicyClassPass, byName["z-enforce"].Class)
	assert.Equal(t, store.MessagePolicyActionAdmitted, byName["z-enforce"].Action)

	primProj := projByName["a-enforce"]
	require.NotEmpty(t, primProj.EvalID)
	assert.False(t, primProj.CacheHit)
	assert.Empty(t, primProj.SourceEvalID)

	for _, secName := range []string{"m-enforce", "z-enforce"} {
		secProj := projByName[secName]
		assert.True(t, secProj.CacheHit, "%s must have cache_hit=true", secName)
		assert.Equal(t, primProj.EvalID, secProj.SourceEvalID, "%s must link source_eval_id to primary", secName)
		assert.NotEmpty(t, secProj.EvalID)
		assert.NotEqual(t, primProj.EvalID, secProj.EvalID, "%s must have unique eval_id", secName)
		assert.Equal(t, byName[secName].PolicyID, secProj.Rule.RuleID)
	}
	assert.NotEqual(t, projByName["m-enforce"].EvalID, projByName["z-enforce"].EvalID)

	// Send 2: provider fails with 503 unavailable. Failure is shared in 1 call;
	// each member applies its own on_failed_reasons map (a-enforce rejects,
	// m-enforce and z-enforce admit).
	providerCalls.Store(0)
	failMode.Store(true)
	rrFail := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Trigger unavailable failure",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rrFail.Code, rrFail.Body.String())
	assert.Equal(t, int32(1), providerCalls.Load(), "shared failure must execute once")

	allDecs := mpgWaitForDecisionCount(t, f.s, f.project.ID, 6)
	require.Len(t, allDecs, 6)
	failedByName := make(map[string]store.MessagePolicyDecision, 3)
	for _, d := range allDecs {
		if d.Class == store.MessagePolicyClassFailed {
			failedByName[d.PolicyName] = d
		}
	}
	require.Len(t, failedByName, 3)
	assert.Equal(t, store.MessagePolicyActionRejected, failedByName["a-enforce"].Action)
	assert.Equal(t, store.MessagePolicyActionAdmitted, failedByName["m-enforce"].Action)
	assert.Equal(t, store.MessagePolicyActionAdmitted, failedByName["z-enforce"].Action)

	var failPrimProj messagepolicy.EvaluationProjection
	require.NoError(t, json.Unmarshal(failedByName["a-enforce"].Evaluation, &failPrimProj))
	for _, secName := range []string{"m-enforce", "z-enforce"} {
		var sp messagepolicy.EvaluationProjection
		require.NoError(t, json.Unmarshal(failedByName[secName].Evaluation, &sp))
		assert.True(t, sp.CacheHit)
		assert.Equal(t, failPrimProj.EvalID, sp.SourceEvalID)
	}
}

func TestShadow_GroupsOwnPoliciesNoEnforceReuse(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	var providerCalls atomic.Int32
	var failAfterFirst atomic.Bool
	mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-shadow-grp", func(w http.ResponseWriter, _ *http.Request) {
		n := providerCalls.Add(1)
		if failAfterFirst.Load() && n >= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"shadow_unavailable"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "jev-1.14.0",
			"answers": {"asks_choice": {"type": "noul", "noul": 0.10}},
			"usage": {"input_tokens": 14, "output_tokens": 1}
		}`))
	})

	rule := mpgGroupBinaryRule("ts-shadow-grp", "Does the message ask the recipient to choose?")

	// 1. N=3 identical shadow policies alone -> exactly 1 shadow provider call.
	for _, name := range []string{"s-zeta", "s-alpha", "s-mid"} {
		require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
			ProjectID:       f.project.ID,
			Name:            name,
			Enabled:         true,
			Mode:            store.MessagePolicyModeShadow,
			Scope:           store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule:   `judgment_flags.asks_choice`,
			SystemOneRule:   rule,
			SystemOneRefs:   []string{"ts-shadow-grp"},
			OnUncertain:     store.MessagePolicyOutcomeAdmit,
			OnFailed:        store.MessagePolicyOutcomeReject,
			OnFailedReasons: map[string]string{string(provider.FailUnavailable): store.MessagePolicyOutcomeAdmit},
		}))
	}

	rr1 := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Shadow-only grouped message",
	})
	require.Equal(t, http.StatusOK, rr1.Code, rr1.Body.String())

	shadowDecs := mpgWaitForDecisionCount(t, f.s, f.project.ID, 3)
	require.Len(t, shadowDecs, 3)
	assert.Equal(t, int32(1), providerCalls.Load(), "3 identical shadow policies must execute once")

	shadowProj := make(map[string]messagepolicy.EvaluationProjection, 3)
	for _, d := range shadowDecs {
		assert.Equal(t, store.MessagePolicyActionShadow, d.Action)
		var proj messagepolicy.EvaluationProjection
		require.NoError(t, json.Unmarshal(d.Evaluation, &proj))
		shadowProj[d.PolicyName] = proj
	}
	assert.False(t, shadowProj["s-alpha"].CacheHit)
	assert.Empty(t, shadowProj["s-alpha"].SourceEvalID)
	for _, sec := range []string{"s-mid", "s-zeta"} {
		assert.True(t, shadowProj[sec].CacheHit)
		assert.Equal(t, shadowProj["s-alpha"].EvalID, shadowProj[sec].SourceEvalID)
		assert.NotEqual(t, shadowProj["s-alpha"].EvalID, shadowProj[sec].EvalID)
	}

	// 2. Add an enforce policy with the IDENTICAL System One rule, and fail the
	// 2nd call (shadow's own execution). Expect:
	//   - 2 provider calls total (1 enforce + 1 shadow; NO enforce->shadow reuse);
	//   - enforce succeeds and admits the send (HTTP 200);
	//   - shadow group fails together in 1 shadow call, sharing the failure
	//     among shadow members only with shadow provenance links.
	require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "e-identical",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: `judgment_flags.asks_choice`,
		SystemOneRule: rule,
		SystemOneRefs: []string{"ts-shadow-grp"},
		OnUncertain:   store.MessagePolicyOutcomeReject,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}))

	providerCalls.Store(0)
	failAfterFirst.Store(true)

	rr2 := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Enforce and shadow identical rule",
	})
	require.Equal(t, http.StatusOK, rr2.Code, rr2.Body.String())

	// Wait for 3 old + 1 new enforce + 3 new shadow = 7 decisions.
	allDecs := mpgWaitForDecisionCount(t, f.s, f.project.ID, 7)
	require.Len(t, allDecs, 7)
	assert.Equal(t, int32(2), providerCalls.Load(), "identical enforce and shadow keys must execute once per context (2 total, no enforce->shadow reuse)")

	var newEnforce *store.MessagePolicyDecision
	newShadowFailed := make(map[string]store.MessagePolicyDecision)
	for i := range allDecs {
		d := allDecs[i]
		if d.PolicyName == "e-identical" {
			newEnforce = &allDecs[i]
		}
		if d.Mode == store.MessagePolicyModeShadow && d.Class == store.MessagePolicyClassFailed {
			newShadowFailed[d.PolicyName] = d
		}
	}
	require.NotNil(t, newEnforce)
	assert.Equal(t, store.MessagePolicyClassPass, newEnforce.Class)
	assert.Equal(t, store.MessagePolicyActionAdmitted, newEnforce.Action)

	require.Len(t, newShadowFailed, 3, "all 3 shadow members must share the shadow group's failure")
	var shPrimProj messagepolicy.EvaluationProjection
	require.NoError(t, json.Unmarshal(newShadowFailed["s-alpha"].Evaluation, &shPrimProj))
	assert.False(t, shPrimProj.CacheHit)
	assert.Empty(t, shPrimProj.SourceEvalID)
	for _, sec := range []string{"s-mid", "s-zeta"} {
		var sp messagepolicy.EvaluationProjection
		require.NoError(t, json.Unmarshal(newShadowFailed[sec].Evaluation, &sp))
		assert.True(t, sp.CacheHit)
		assert.Equal(t, shPrimProj.EvalID, sp.SourceEvalID)
	}
}

func TestHubGroups_IndependentConcurrentAndCompileFailureUngrouped(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	var arrived atomic.Int32
	bothArrived := make(chan struct{})
	mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-conc", func(w http.ResponseWriter, r *http.Request) {
		if arrived.Add(1) == 2 {
			close(bothArrived)
		}
		select {
		case <-bothArrived:
		case <-r.Context().Done():
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "jev-1.14.0",
			"answers": {"asks_choice": {"type": "noul", "noul": 0.1}},
			"usage": {"input_tokens": 10, "output_tokens": 1}
		}`))
	})

	ruleA := mpgGroupBinaryRule("ts-conc", "Group A question")
	ruleB := mpgGroupBinaryRule("ts-conc", "Group B question")

	for _, p := range []*store.MessagePolicy{
		{
			ProjectID:     f.project.ID,
			Name:          "grp-a-1",
			Enabled:       true,
			Mode:          store.MessagePolicyModeEnforce,
			Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule: `judgment_flags.asks_choice`,
			SystemOneRule: ruleA,
			SystemOneRefs: []string{"ts-conc"},
			OnFailed:      store.MessagePolicyOutcomeReject,
		},
		{
			ProjectID:     f.project.ID,
			Name:          "grp-a-2",
			Enabled:       true,
			Mode:          store.MessagePolicyModeEnforce,
			Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule: `judgment_flags.asks_choice`,
			SystemOneRule: ruleA,
			SystemOneRefs: []string{"ts-conc"},
			OnFailed:      store.MessagePolicyOutcomeReject,
		},
		{
			ProjectID:     f.project.ID,
			Name:          "grp-b-1",
			Enabled:       true,
			Mode:          store.MessagePolicyModeEnforce,
			Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule: `judgment_flags.asks_choice`,
			SystemOneRule: ruleB,
			SystemOneRefs: []string{"ts-conc"},
			OnFailed:      store.MessagePolicyOutcomeReject,
		},
		{
			// Invalid rule: must stay ungrouped with compile_error while valid
			// groups still execute concurrently and succeed.
			ProjectID:       f.project.ID,
			Name:            "grp-bad-compile",
			Enabled:         true,
			Mode:            store.MessagePolicyModeEnforce,
			Scope:           store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule:   `judgment_flags.asks_choice`,
			SystemOneRule:   `{"dialect":"system_one","spec":{"version":999}}`,
			OnFailed:        store.MessagePolicyOutcomeReject,
			OnFailedReasons: map[string]string{messagepolicy.ReasonCompileError: store.MessagePolicyOutcomeAdmit},
		},
	} {
		require.NoError(t, f.s.CreateMessagePolicy(ctx, p))
	}

	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Concurrent groups with ungrouped compile failure",
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, int32(2), arrived.Load(), "two distinct valid groups must execute concurrently once each")

	decs := mpgWaitForDecisionCount(t, f.s, f.project.ID, 4)
	require.Len(t, decs, 4)
	byName := make(map[string]store.MessagePolicyDecision, 4)
	for _, d := range decs {
		byName[d.PolicyName] = d
	}
	assert.Equal(t, store.MessagePolicyClassPass, byName["grp-a-1"].Class)
	assert.Equal(t, store.MessagePolicyClassPass, byName["grp-a-2"].Class)
	assert.Equal(t, store.MessagePolicyClassPass, byName["grp-b-1"].Class)
	assert.Equal(t, store.MessagePolicyClassFailed, byName["grp-bad-compile"].Class)
	assert.Equal(t, messagepolicy.ReasonCompileError, byName["grp-bad-compile"].CELErrorReason)
}

func TestHubGroups_SoCtxCancelOnRequestDisconnect(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-cancel", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
		w.WriteHeader(http.StatusRequestTimeout)
	})

	rule := mpgGroupBinaryRule("ts-cancel", "Slow question for cancel test")
	for _, name := range []string{"c-pol-1", "c-pol-2"} {
		require.NoError(t, f.s.CreateMessagePolicy(ctx, &store.MessagePolicy{
			ProjectID:     f.project.ID,
			Name:          name,
			Enabled:       true,
			Mode:          store.MessagePolicyModeEnforce,
			Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule: `judgment_flags.asks_choice`,
			SystemOneRule: rule,
			SystemOneRefs: []string{"ts-cancel"},
			OnFailed:      store.MessagePolicyOutcomeReject,
		}))
	}

	reqCtx, cancelReq := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelReq()

	body := fmt.Sprintf(`{"recipient":"user:%s","msg":"cancel test"}`, f.alice.Email)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+f.project.ID+"/agents/"+f.sender.ID+"/outbound-message", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(reqCtx, &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: f.sender.ID},
		ProjectID: f.project.ID,
		Ancestry:  f.sender.Ancestry,
	}}))
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.srv.handleAgentOutboundMessage(rec, req, f.sender.ID)
	}()

	<-entered
	cancelReq()
	<-done

	assert.Equal(t, 499, rec.Code)
	assert.Equal(t, int32(1), calls.Load(), "canceled group must have executed only once")

	// Directly exercise the production hub gate helper (evalPlannedSystemOnePolicies)
	// under a canceled soCtx and verify EVERY member's canceled classification,
	// real on_failed_reasons outcome, member-to-slot mapping, and provenance.
	calls.Store(0)
	entered2 := make(chan struct{})
	release2 := make(chan struct{})
	defer close(release2)
	mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-cancel-direct", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered2)
		}
		select {
		case <-r.Context().Done():
		case <-release2:
		}
		w.WriteHeader(http.StatusRequestTimeout)
	})
	ruleDirect := mpgGroupBinaryRule("ts-cancel-direct", "Slow question for per-member cancel check")
	pols := []store.MessagePolicy{
		{
			ID:              "pol-cancel-z",
			ProjectID:       f.project.ID,
			Name:            "c-pol-reject",
			Revision:        3,
			Enabled:         true,
			Mode:            store.MessagePolicyModeEnforce,
			ViolationRule:   `judgment_flags.asks_choice`,
			SystemOneRule:   ruleDirect,
			OnFailed:        store.MessagePolicyOutcomeAdmit,
			OnFailedReasons: map[string]string{string(provider.FailCanceled): store.MessagePolicyOutcomeReject},
		},
		{
			ID:              "pol-cancel-a",
			ProjectID:       f.project.ID,
			Name:            "c-pol-admit",
			Revision:        1,
			Enabled:         true,
			Mode:            store.MessagePolicyModeEnforce,
			ViolationRule:   `judgment_flags.asks_choice`,
			SystemOneRule:   ruleDirect,
			OnFailed:        store.MessagePolicyOutcomeReject,
			OnFailedReasons: map[string]string{string(provider.FailCanceled): store.MessagePolicyOutcomeAdmit},
		},
		{
			ID:            "pol-cancel-m",
			ProjectID:     f.project.ID,
			Name:          "c-pol-default",
			Revision:      2,
			Enabled:       true,
			Mode:          store.MessagePolicyModeEnforce,
			ViolationRule: `judgment_flags.asks_choice`,
			SystemOneRule: ruleDirect,
			OnFailed:      store.MessagePolicyOutcomeReject,
		},
	}

	facts := messagepolicy.Facts{
		Surface:   messagepolicy.SurfaceAgentToUser,
		Sender:    messagepolicy.SenderFacts{Kind: "agent", ID: f.sender.ID, Slug: f.sender.Slug, ProjectID: f.project.ID},
		Recipient: messagepolicy.RecipientFacts{Kind: "user", ID: f.alice.ID, Slug: f.alice.Email},
		Message:   messagepolicy.MessageFacts{Type: "message", Body: "cancel per-member test"},
	}
	act := messagepolicy.NewActivation(facts)
	budget := f.srv.messagePolicyGate.makeSendBudget()
	soCtx, soCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer soCancel()

	results := make([]policyEvalResult, len(pols))
	directDone := make(chan struct{})
	go func() {
		defer close(directDone)
		f.srv.evalPlannedSystemOnePolicies(soCtx, pols, store.MaxActiveMatchingMessagePolicies, f.project.ID, facts, act, budget, 3*time.Second, false, results)
	}()

	<-entered2
	soCancel()
	<-directDone

	assert.Equal(t, int32(1), calls.Load(), "production hub helper must execute canceled group once")
	wantOutcomes := []string{
		messagepolicy.OutcomeReject, // c-pol-reject: on_failed_reasons["canceled"] = reject
		messagepolicy.OutcomeAdmit,  // c-pol-admit:  on_failed_reasons["canceled"] = admit
		messagepolicy.OutcomeReject, // c-pol-default: on_failed = reject
	}
	primaryIdx := 1 // c-pol-admit has lowest PolicyName
	require.NotNil(t, results[primaryIdx].classification.Evaluation)
	primaryEvalID := results[primaryIdx].classification.Evaluation.EvalID
	require.NotEmpty(t, primaryEvalID)

	seenEvalIDs := make(map[string]bool, len(pols))
	for i := range pols {
		r := results[i]
		require.NotNil(t, r.policy, "member %d must be populated", i)
		assert.Equal(t, pols[i].ID, r.policy.ID, "result slot %d must map to policy %s", i, pols[i].ID)
		assert.Equal(t, messagepolicy.ClassFailed, r.classification.Class, "member %d must be failed", i)
		assert.Equal(t, string(provider.FailCanceled), r.classification.Reason, "member %d must receive runtime canceled classification", i)

		outcome := messagepolicy.ResolveOutcome(
			r.classification.Class,
			r.classification.Reason,
			pols[i].OnUncertain,
			pols[i].OnFailed,
			pols[i].OnFailedReasons,
		)
		assert.Equal(t, wantOutcomes[i], outcome, "member %d (%s) outcome mismatch", i, pols[i].Name)

		require.NotNil(t, r.classification.Evaluation, "member %d must retain canceled provenance", i)
		assert.Equal(t, pols[i].ID, r.classification.Evaluation.Rule.RuleID, "member %d rule_id mismatch", i)
		assert.Equal(t, pols[i].Revision, r.classification.Evaluation.Rule.Revision, "member %d revision mismatch", i)
		assert.False(t, seenEvalIDs[r.classification.Evaluation.EvalID], "member %d must have unique eval_id", i)
		seenEvalIDs[r.classification.Evaluation.EvalID] = true
		if i == primaryIdx {
			assert.False(t, r.classification.Evaluation.CacheHit)
			assert.Empty(t, r.classification.SourceEvalID)
		} else {
			assert.True(t, r.classification.Evaluation.CacheHit)
			assert.Nil(t, r.classification.Evaluation.Usage)
			assert.Equal(t, primaryEvalID, r.classification.SourceEvalID)
		}
	}
}
