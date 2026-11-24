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
	"crypto/tls"
	"encoding/json"
	"errors"
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
	"golang.org/x/oauth2"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	systemoneprovider "github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
)

func mpgEnableSystemOne(t *testing.T, srv *Server, s store.Store, projectID, instanceName string, handler http.HandlerFunc) (*httptest.Server, *store.User) {
	t.Helper()
	ctx := context.Background()

	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		switch e.Name {
		case experiments.MessagePolicies, experiments.Decisions, experiments.SystemOne:
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg

	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret-signing-key")
	srv.SetSecretBackend(localBackend)
	srv.SetSystemOneADCForTesting(t, func(context.Context, []string) (*SystemOneADCCredentials, error) {
		return nil, egress.ErrAuth
	})

	tlsSrv := httptest.NewTLSServer(handler)
	t.Cleanup(tlsSrv.Close)

	srv.setSystemOneRuntimeOptions(
		systemoneruntime.WithEgressOptionsForTesting(
			egress.WithLoopbackServerForTesting(tlsSrv.URL, tlsSrv.Client()),
		),
	)

	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "mp-s1-admin-"+instanceName, store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	createRec := doRequestAsUser(t, srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    instanceName,
		"adapter": "typesafe",
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": "sk-mp-s1-secret-canary-9988"},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())

	instRow, err := s.GetSystemOneInstanceByName(ctx, instanceName)
	require.NoError(t, err)
	instRow.Endpoint = tlsSrv.URL
	require.NoError(t, s.UpdateSystemOneInstance(ctx, instRow))
	if projectID != "" {
		_, _, err = s.AddProjectSystemOneInstance(ctx, projectID, instanceName)
		require.NoError(t, err)
	}

	return tlsSrv, admin
}

func mpgWaitForDecisionCount(t *testing.T, s store.Store, projectID string, want int) []store.MessagePolicyDecision {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := s.ListMessagePolicyDecisions(context.Background(), store.MessagePolicyDecisionFilter{
			ProjectID: projectID,
		}, store.ListOptions{Limit: 50})
		require.NoError(t, err)
		if len(res.Items) >= want {
			return res.Items
		}
		time.Sleep(15 * time.Millisecond)
	}
	res, err := s.ListMessagePolicyDecisions(context.Background(), store.MessagePolicyDecisionFilter{
		ProjectID: projectID,
	}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(res.Items), want, "timed out waiting for %d decision rows", want)
	return res.Items
}

func TestMessagePolicySystemOne_AuthoringMatrixAndPolicyTest(t *testing.T) {
	f := newMPHFixture(t, true)
	var providerCalls atomic.Int32
	mpgEnableSystemOne(t, f.srv, f.s, f.project, "ts-authoring", func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "jev-1.14.0",
			"answers": {"asks_choice": {"type": "noul", "noul": 0.92}},
			"usage": {"input_tokens": 18, "output_tokens": 1}
		}`))
	})

	base := f.basePath()
	decodeErr := func(rec *httptest.ResponseRecorder) APIError {
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp.Error
	}

	validS1Rule := `{
		"dialect": "system_one",
		"inputs": ["message.body"],
		"on_oversize": "reject",
		"spec": {
			"version": 1,
			"binding": {"instance": "ts-authoring"},
			"questions": [{"id": "asks_choice", "type": "binary", "instructions": "Does the message ask the recipient to choose among alternatives?"}]
		}
	}`

	// 1. Valid System One policy accepted, system_one_refs derived, 0 provider calls at authoring.
	createBody := CreateMessagePolicyRequest{
		Name:              "prose-choices-s1",
		Mode:              store.MessagePolicyModeShadow,
		Scope:             store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule:     `recipient.kind != "agent" && !message.has_choices && judgment_flags.asks_choice`,
		SystemOneRule:     validS1Rule,
		OnUncertain:       store.MessagePolicyOutcomeAdmit,
		OnFailed:          store.MessagePolicyOutcomeReject,
		RejectReason:      "Please send structured choices instead of prose.",
		RejectRemediation: "Resend with --choices-file.",
	}
	rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, createBody)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created MessagePolicyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Equal(t, []string{"ts-authoring"}, created.SystemOneRefs)
	assert.Equal(t, store.MessagePolicyOutcomeAdmit, created.OnUncertain)
	assert.Zero(t, providerCalls.Load(), "authoring compile must not invoke provider")

	// 2. Missing on_uncertain returns 400 validation_error.
	noUnc := createBody
	noUnc.Name = "missing-uncertain"
	noUnc.OnUncertain = ""
	rec = mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, noUnc)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	apiErr := decodeErr(rec)
	assert.Equal(t, messagepolicy.ErrCodeValidation, apiErr.Code)
	assert.Equal(t, "on_uncertain", apiErr.Details["field"])

	// 3. Undeclared input field returns 400 invalid_rule (category=input).
	badInput := createBody
	badInput.Name = "bad-input"
	badInput.SystemOneRule = `{"dialect":"system_one","inputs":["secret.token"],"spec":{"version":1,"binding":{"instance":"ts-authoring"},"questions":[{"id":"q1","type":"binary","instructions":"Check."}]}}`
	rec = mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, badInput)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	apiErr = decodeErr(rec)
	assert.Equal(t, messagepolicy.ErrCodeInvalidRule, apiErr.Code)
	assert.Equal(t, "input", apiErr.Details["category"])

	// 4. Score question and threshold both return 400 not_yet_supported.
	scoreReq := createBody
	scoreReq.Name = "score-not-supported"
	scoreReq.SystemOneRule = `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"binding":{"instance":"ts-authoring"},"questions":[{"id":"sev","type":"score","instructions":"Rate.","levels":[{"id":"lo","description":"Low"},{"id":"hi","description":"High"}]}]}}`
	rec = mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, scoreReq)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, messagepolicy.ErrCodeNotYetSupported, decodeErr(rec).Code)

	threshReq := createBody
	threshReq.Name = "thresh-not-supported"
	threshReq.SystemOneRule = `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"binding":{"instance":"ts-authoring"},"questions":[{"id":"q1","type":"binary","instructions":"Check."}],"uncertain_if":[{"question":"q1","metric":"p_top","below":0.7}]}}`
	rec = mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, threshReq)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, messagepolicy.ErrCodeNotYetSupported, decodeErr(rec).Code)

	// 5. Over-16 KiB system_one_rule returns 400 invalid_rule (category=limit).
	oversizeReq := createBody
	oversizeReq.Name = "oversize-s1"
	oversizeReq.SystemOneRule = `{"spec":"` + strings.Repeat("a", messagepolicy.MaxSystemOneRuleBytes) + `"}`
	rec = mphSessionDo(t, f.srv, f.owner, http.MethodPost, base, oversizeReq)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	apiErr = decodeErr(rec)
	assert.Equal(t, messagepolicy.ErrCodeInvalidRule, apiErr.Code)
	assert.Equal(t, "limit", apiErr.Details["category"])

	// 6. Policy test endpoint executes System One rule and returns content-free evaluation projection.
	const sampleCanary = "DRY_RUN_SECRET_BODY_CANARY_4242"
	testRec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, base+"/prose-choices-s1/test", TestMessagePolicyRequest{
		Sample: &MessagePolicyTestSample{
			Surface: store.MessagePolicySurfaceAgentToUser,
			Body:    "Should we deploy option A or option B? " + sampleCanary,
		},
	})
	require.Equal(t, http.StatusOK, testRec.Code, testRec.Body.String())
	assert.NotContains(t, testRec.Body.String(), sampleCanary)
	var testResp TestMessagePolicyResponse
	require.NoError(t, json.Unmarshal(testRec.Body.Bytes(), &testResp))
	assert.Equal(t, store.MessagePolicyClassViolation, testResp.Class)
	assert.Equal(t, store.MessagePolicyActionShadow, testResp.Action)
	assert.Equal(t, int32(1), providerCalls.Load())
	require.NotEmpty(t, testResp.Evaluation)
	var proj messagepolicy.EvaluationProjection
	require.NoError(t, json.Unmarshal(testResp.Evaluation, &proj))
	assert.Equal(t, "success", proj.Class)
	assert.Equal(t, "ts-authoring", proj.Instance)

	// 6b (O4): PATCH clearing on_uncertain while system_one_rule is still set -> 400.
	emptyUnc := ""
	clearUncRec := mphSessionDo(t, f.srv, f.owner, http.MethodPatch, base+"/prose-choices-s1", PatchMessagePolicyRequest{
		OnUncertain: &emptyUnc,
	})
	require.Equal(t, http.StatusBadRequest, clearUncRec.Code, clearUncRec.Body.String())
	assert.Contains(t, clearUncRec.Body.String(), "on_uncertain")

	// 7. PATCH clearing system_one_rule clears system_one_refs and allows moving to enforce.
	emptyS1 := ""
	enforceMode := store.MessagePolicyModeEnforce
	celOnlyRule := `!message.has_choices && message.body.contains("Option A")`
	patchRec := mphSessionDo(t, f.srv, f.owner, http.MethodPatch, base+"/prose-choices-s1", PatchMessagePolicyRequest{
		Mode:          &enforceMode,
		ViolationRule: &celOnlyRule,
		SystemOneRule: &emptyS1,
	})
	require.Equal(t, http.StatusOK, patchRec.Code, patchRec.Body.String())
	var patched MessagePolicyResponse
	require.NoError(t, json.Unmarshal(patchRec.Body.Bytes(), &patched))
	assert.Empty(t, patched.SystemOneRule)
	assert.Empty(t, patched.SystemOneRefs)
	assert.Equal(t, store.MessagePolicyModeEnforce, patched.Mode)
	assert.Equal(t, int64(2), patched.Revision)
}

func TestMessagePolicySystemOne_ShadowDeliveryAndContentFreeProjection(t *testing.T) {
	f := newMpgFixture(t, true)
	const bodyCanary = "SHADOW_CONFIDENTIAL_BODY_CANARY_7711"

	var providerCalls atomic.Int32
	mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-shadow", func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "jev-1.14.0",
			"answers": {"asks_choice": {"type": "noul", "noul": 0.91}},
			"usage": {"input_tokens": 22, "output_tokens": 1}
		}`))
	})

	pol := &store.MessagePolicy{
		ProjectID:         f.project.ID,
		Name:              "shadow-prose-choices",
		Enabled:           true,
		Mode:              store.MessagePolicyModeShadow,
		Scope:             store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule:     `recipient.kind != "agent" && !message.has_choices && judgment_flags.asks_choice`,
		SystemOneRule:     `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"binding":{"instance":"ts-shadow"},"questions":[{"id":"asks_choice","type":"binary","instructions":"Does the message ask the recipient to choose among alternatives?"}]}}`,
		SystemOneRefs:     []string{"ts-shadow"},
		OnUncertain:       store.MessagePolicyOutcomeAdmit,
		OnFailed:          store.MessagePolicyOutcomeAdmit,
		RejectReason:      "Prose choice menu detected.",
		RejectRemediation: "Resend with --choices-file.",
	}
	require.NoError(t, f.s.CreateMessagePolicy(context.Background(), pol))

	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Would you like option A or option B? " + bodyCanary,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, 1, f.recordChannel.count(), "shadow policy must deliver message")

	decisions := mpgWaitForDecisionCount(t, f.s, f.project.ID, 1)
	require.Len(t, decisions, 1)
	dec := decisions[0]
	assert.Equal(t, store.MessagePolicyActionShadow, dec.Action)
	assert.Equal(t, store.MessagePolicyClassViolation, dec.Class)
	assert.NotEmpty(t, dec.MessageID)
	assert.Equal(t, int32(1), providerCalls.Load())

	require.NotEmpty(t, dec.Evaluation)
	evalJSON := string(dec.Evaluation)
	assert.NotContains(t, evalJSON, bodyCanary, "decision evaluation must never contain message body")
	assert.NotContains(t, evalJSON, `"sha256":`, "decision evaluation must omit input sha256")
	assert.NotContains(t, evalJSON, "sk-mp-s1-secret", "decision evaluation must never contain secrets")

	var proj messagepolicy.EvaluationProjection
	require.NoError(t, json.Unmarshal(dec.Evaluation, &proj))
	assert.Equal(t, "success", proj.Class)
	assert.Equal(t, "jev-1.14.0", proj.ConfiguredModel)
	assert.Equal(t, "ts-shadow", proj.Instance)
	assert.False(t, proj.Truncated)
	require.Contains(t, proj.Answers, "asks_choice")
	require.NotNil(t, proj.Answers["asks_choice"].Bool)
	assert.True(t, *proj.Answers["asks_choice"].Bool)
}

func TestMessagePolicySystemOne_EnforceOutcomesAndFailClosedMatrix(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	// Handler mode:
	// 0 = asks_choice true (violation)
	// 1 = choice abstained (uncertain)
	// 2 = provider 503 unavailable (failed)
	// 3 = slow hang for timeout (failed: timeout)
	// 4 = choice label "out_of_scope" (violation with label template substitution)
	var handlerMode atomic.Int32
	var providerCalls atomic.Int32

	_, admin := mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-enforce", func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		switch handlerMode.Load() {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"model": "jev-1.14.0",
				"answers": {
					"topic": {
						"type": "choice",
						"choice": "unsure",
						"confidence": 0.7,
						"probabilities": {"in_scope": 0.1, "out_of_scope": 0.1, "unsure": 0.8}
					}
				},
				"usage": {"input_tokens": 12, "output_tokens": 1}
			}`))
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error": "SECRET_UPSTREAM_ERROR_TEXT_DO_NOT_ECHO"}`))
		case 3:
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			w.WriteHeader(http.StatusGatewayTimeout)
		case 4:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"model": "jev-1.14.0",
				"answers": {
					"topic": {
						"type": "choice",
						"choice": "out_of_scope",
						"confidence": 0.85,
						"probabilities": {"in_scope": 0.05, "out_of_scope": 0.90, "unsure": 0.05}
					}
				},
				"usage": {"input_tokens": 12, "output_tokens": 1}
			}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"model": "jev-1.14.0",
				"answers": {"asks_choice": {"type": "noul", "noul": 0.94}},
				"usage": {"input_tokens": 18, "output_tokens": 1}
			}`))
		}
	})

	proseRule := `{"dialect":"system_one","inputs":["message.body"],"on_oversize":"reject","spec":{"version":1,"binding":{"instance":"ts-enforce"},"questions":[{"id":"asks_choice","type":"binary","instructions":"Does the message ask the recipient to choose among alternatives?"}]}}`
	topicRule := `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"binding":{"instance":"ts-enforce"},"questions":[{"id":"topic","type":"choice","instructions":"Topic?","options":[{"id":"in_scope","description":"In scope"},{"id":"out_of_scope","description":"Out of scope"},{"id":"unsure","description":"Unsure","abstain":true}]}]}}`

	// 1. Prose-choices enforce violation -> 422 with policy reason/remediation; structured choices -> 200.
	prosePol := &store.MessagePolicy{
		ProjectID:         f.project.ID,
		Name:              "a-prose-choices",
		Enabled:           true,
		Mode:              store.MessagePolicyModeEnforce,
		Scope:             store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule:     `recipient.kind != "agent" && !message.has_choices && judgment_flags.asks_choice`,
		SystemOneRule:     proseRule,
		SystemOneRefs:     []string{"ts-enforce"},
		OnUncertain:       store.MessagePolicyOutcomeReject,
		OnFailed:          store.MessagePolicyOutcomeReject,
		RejectReason:      "Policy {policy}: prose choices are not allowed.",
		RejectRemediation: "Resend with --choices-file.",
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, prosePol))

	handlerMode.Store(0)
	f.recordChannel.reset()
	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Pick A or B please.",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Zero(t, f.recordChannel.count(), "rejected message must not be delivered")
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &errResp))
	assert.Equal(t, string(MessageDenialPolicyRejected), errResp.Error.Code)
	assert.Equal(t, "Policy a-prose-choices: prose choices are not allowed.", errResp.Error.Message)
	assert.Equal(t, "a-prose-choices", errResp.Error.Details["policy"])
	assert.Equal(t, "violation", errResp.Error.Details["class"])
	assert.Equal(t, "Resend with --choices-file.", errResp.Error.Details["remediation"])

	// Structured choices satisfies !message.has_choices == false -> admitted!
	var structuredChoices []messages.DecisionInput
	require.NoError(t, json.Unmarshal([]byte(mpgOneDecision), &structuredChoices))
	rrOK := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Please decide.",
		Choices:   structuredChoices,
	})
	require.Equal(t, http.StatusOK, rrOK.Code, rrOK.Body.String())
	assert.Equal(t, 1, f.recordChannel.count())

	// 2. Oversize reject -> Failed{input_too_large} with 0 provider calls -> 422.
	// Use multi-byte UTF-8 runes (9000 runes <= 16000 char ingress cap, 18000 bytes > 16384 byte field cap).
	callsBeforeOversize := providerCalls.Load()
	rrBig := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       strings.Repeat("é", 9000),
	})
	require.Equal(t, http.StatusUnprocessableEntity, rrBig.Code, rrBig.Body.String())
	assert.Equal(t, callsBeforeOversize, providerCalls.Load(), "oversize reject must make 0 provider calls")
	var bigErr ErrorResponse
	require.NoError(t, json.Unmarshal(rrBig.Body.Bytes(), &bigErr))
	assert.Equal(t, "failed", bigErr.Error.Details["class"])
	oversizeDecs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: f.project.ID,
		Class:     store.MessagePolicyClassFailed,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, oversizeDecs.Items)
	assert.Equal(t, string(systemoneprovider.FailInputTooLarge), oversizeDecs.Items[0].CELErrorReason)

	// 3. Provider error (503) -> Failed{unavailable} -> 422 without leaking upstream error text.
	handlerMode.Store(2)
	rrFail := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Normal message during outage.",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rrFail.Code, rrFail.Body.String())
	assert.NotContains(t, rrFail.Body.String(), "SECRET_UPSTREAM_ERROR_TEXT", "provider error text must never leak to sender")

	// Per-reason override: on_failed_reasons["unavailable"] = "admit" overrides on_failed="reject"!
	prosePol.OnFailedReasons = map[string]string{
		string(systemoneprovider.FailUnavailable): store.MessagePolicyOutcomeAdmit,
	}
	require.NoError(t, f.s.UpdateMessagePolicy(ctx, prosePol))
	rrOverride := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Normal message admitted by per-reason override.",
	})
	require.Equal(t, http.StatusOK, rrOverride.Code, rrOverride.Body.String())

	// 4. Timeout -> Failed{timeout} -> 422 (on_failed=reject, timeout not overridden).
	f.srv.messagePolicyGate.newSystemOneBudget = func() time.Duration { return 120 * time.Millisecond }
	handlerMode.Store(3)
	rrTimeout := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Slow provider call.",
	})
	f.srv.messagePolicyGate.newSystemOneBudget = nil
	require.Equal(t, http.StatusUnprocessableEntity, rrTimeout.Code, rrTimeout.Body.String())
	var timeoutErr ErrorResponse
	require.NoError(t, json.Unmarshal(rrTimeout.Body.Bytes(), &timeoutErr))
	assert.Equal(t, "failed", timeoutErr.Error.Details["class"])

	// 5. on_uncertain reject vs admit + choice label template substitution + mixed CEL + shadow.
	require.NoError(t, f.s.DeleteMessagePolicy(ctx, prosePol.ID))
	topicPol := &store.MessagePolicy{
		ProjectID:         f.project.ID,
		Name:              "b-topic-check",
		Enabled:           true,
		Mode:              store.MessagePolicyModeEnforce,
		Scope:             store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule:     `judgment_labels.topic == "out_of_scope"`,
		SystemOneRule:     topicRule,
		SystemOneRefs:     []string{"ts-enforce"},
		OnUncertain:       store.MessagePolicyOutcomeReject,
		OnFailed:          store.MessagePolicyOutcomeReject,
		RejectReason:      "Message classified as {judgment.topic} by {policy}.",
		RejectRemediation: "Keep messages on topic.",
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, topicPol))

	// Mixed CEL enforce policy (passes for normal body, rejects on "FORBIDDEN_CEL") + shadow policy (never blocks).
	celPol := &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "a-cel-check",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: `message.body.contains("FORBIDDEN_CEL")`,
		OnFailed:      store.MessagePolicyOutcomeReject,
		RejectReason:  "CEL policy rejected message.",
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, celPol))

	shadowPol := &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "z-shadow-always",
		Enabled:       true,
		Mode:          store.MessagePolicyModeShadow,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: `true`,
		OnFailed:      store.MessagePolicyOutcomeReject,
		RejectReason:  "Shadow only.",
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, shadowPol))

	// 5a. Uncertain with on_uncertain=reject -> 422 class=uncertain.
	handlerMode.Store(1)
	rrUncRej := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Ambiguous topic message.",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rrUncRej.Code, rrUncRej.Body.String())
	var uncErr ErrorResponse
	require.NoError(t, json.Unmarshal(rrUncRej.Body.Bytes(), &uncErr))
	assert.Equal(t, "uncertain", uncErr.Error.Details["class"])

	// 5b. Uncertain with on_uncertain=admit -> 200 (shadow policy still never blocks!).
	topicPol.OnUncertain = store.MessagePolicyOutcomeAdmit
	require.NoError(t, f.s.UpdateMessagePolicy(ctx, topicPol))
	rrUncAdmit := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Ambiguous topic message admitted.",
	})
	require.Equal(t, http.StatusOK, rrUncAdmit.Code, rrUncAdmit.Body.String())

	// 5c. Label template substitution with policy-authored choice ID ("out_of_scope").
	handlerMode.Store(4)
	rrLabel := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Off-topic chatter.",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rrLabel.Code, rrLabel.Body.String())
	var lblErr ErrorResponse
	require.NoError(t, json.Unmarshal(rrLabel.Body.Bytes(), &lblErr))
	assert.Equal(t, "Message classified as out_of_scope by b-topic-check.", lblErr.Error.Message)

	// 5d. Mixed CEL + System One: CEL policy "a-cel-check" comes first in name order and determines 422 body.
	rrMixed := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "FORBIDDEN_CEL and off-topic.",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rrMixed.Code, rrMixed.Body.String())
	var mixErr ErrorResponse
	require.NoError(t, json.Unmarshal(rrMixed.Body.Bytes(), &mixErr))
	assert.Equal(t, "a-cel-check", mixErr.Error.Details["policy"])

	// 5b (O3). Disabled allowlisted instance -> Failed{no_eligible_instance} with 0 provider calls -> 422.
	instRow, gErr := f.s.GetSystemOneInstanceByName(ctx, "ts-enforce")
	require.NoError(t, gErr)
	instRow.Enabled = false
	require.NoError(t, f.s.UpdateSystemOneInstance(ctx, instRow))

	callsBeforeDisabled := providerCalls.Load()
	rrDisabled := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Message while instance is disabled.",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rrDisabled.Code, rrDisabled.Body.String())
	assert.Equal(t, callsBeforeDisabled, providerCalls.Load(), "disabled instance must make 0 provider calls")
	var disErr ErrorResponse
	require.NoError(t, json.Unmarshal(rrDisabled.Body.Bytes(), &disErr))
	assert.Equal(t, "failed", disErr.Error.Details["class"])
	assert.Equal(t, "b-topic-check", disErr.Error.Details["policy"])
	disDecs, dErr := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: f.project.ID,
		PolicyID:  topicPol.ID,
	}, store.ListOptions{Limit: 1})
	require.NoError(t, dErr)
	require.NotEmpty(t, disDecs.Items)
	assert.Equal(t, string(systemoneprovider.FailNoEligibleInstance), disDecs.Items[0].CELErrorReason)

	// Re-enable before step 6 tests deletion.
	instRow.Enabled = true
	require.NoError(t, f.s.UpdateSystemOneInstance(ctx, instRow))

	// 6. Removed/disabled instance -> Failed{no_eligible_instance} with 0 provider calls -> 422.
	delRec := doRequestAsUser(t, f.srv, admin, http.MethodDelete, systemOneAdminPath+"/ts-enforce", nil)
	require.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())

	callsBeforeRemoved := providerCalls.Load()
	rrRemoved := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Message after instance removal.",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rrRemoved.Code, rrRemoved.Body.String())
	assert.Equal(t, callsBeforeRemoved, providerCalls.Load(), "removed instance must make 0 provider calls")
	var remErr ErrorResponse
	require.NoError(t, json.Unmarshal(rrRemoved.Body.Bytes(), &remErr))
	assert.Equal(t, "failed", remErr.Error.Details["class"])
	assert.Equal(t, "b-topic-check", remErr.Error.Details["policy"])
}

func TestMessagePolicySystemOne_I3AuthorizedProjectIDAndProjectScopedSecret(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		switch e.Name {
		case experiments.MessagePolicies, experiments.Decisions, experiments.SystemOne:
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	f.srv.experiments = reg

	localBackend := secret.NewLocalBackend(f.s, "test-hub-id", "test-secret-signing-key")
	f.srv.SetSecretBackend(localBackend)
	f.srv.SetSystemOneADCForTesting(t, func(context.Context, []string) (*SystemOneADCCredentials, error) {
		return nil, egress.ErrAuth
	})

	admin, err := f.s.GetUser(ctx, hubConfigTokenUser(t, f.s, "mp-i3-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)

	const projAPIKey = "AIza-mp-i3-project-secret-canary-4455"
	const projBAPIKey = "AIza-mp-i3-project-B-conflict-9999"
	_, _, err = f.srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name:       systemoneprovider.DefaultGeminiAPIKeySecretName,
		Value:      projAPIKey,
		SecretType: store.SecretTypeVariable,
		Scope:      store.ScopeProject,
		ScopeID:    f.project.ID,
	})
	require.NoError(t, err)

	// O1: Seed a second project B with a same-named project secret holding a different value.
	projB := &store.Project{
		ID:   "proj-i3-conflict-b",
		Name: "I3 Conflict Project B",
		Slug: "i3-conflict-project-b",
	}
	require.NoError(t, f.s.CreateProject(ctx, projB))
	_, _, err = f.srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name:       systemoneprovider.DefaultGeminiAPIKeySecretName,
		Value:      projBAPIKey,
		SecretType: store.SecretTypeVariable,
		Scope:      store.ScopeProject,
		ScopeID:    projB.ID,
	})
	require.NoError(t, err)

	var (
		providerCalls atomic.Int32
		seenKeyHeader string
	)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		seenKeyHeader = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"answers": {"asks_choice": {"type": "noul", "noul": 0.96, "confidence": 0.96}},
			"usage": {"total_input_tokens": 16, "total_output_tokens": 0, "total_tokens": 16}
		}`))
	}))
	t.Cleanup(tlsSrv.Close)

	f.srv.setSystemOneRuntimeOptions(
		systemoneruntime.WithEgressOptionsForTesting(
			egress.WithDialerForTesting(hubDialectPublicResolver{}, hubDialectTargetDialer{targetAddr: tlsSrv.Listener.Addr().String()}),
			egress.WithTLSConfigForTesting(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // offline test TLS server
		),
	)

	createRec := doRequestAsUser(t, f.srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "gemini-i3-mp",
		"adapter": "gemini_decisions",
		"model":   "gemini-2.5-flash",
		"auth": map[string]any{
			"kind": "api_key",
			"secretRef": map[string]any{
				"name":  systemoneprovider.DefaultGeminiAPIKeySecretName,
				"scope": systemoneprovider.SecretScopeProject,
			},
		},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	_, _, err = f.s.AddProjectSystemOneInstance(ctx, f.project.ID, "gemini-i3-mp")
	require.NoError(t, err)

	pol := &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "i3-proj-secret-policy",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: `!message.has_choices && judgment_flags.asks_choice`,
		SystemOneRule: `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"binding":{"instance":"gemini-i3-mp"},"questions":[{"id":"asks_choice","type":"binary","instructions":"Asks choice?"}]}}`,
		SystemOneRefs: []string{"gemini-i3-mp"},
		OnUncertain:   store.MessagePolicyOutcomeReject,
		OnFailed:      store.MessagePolicyOutcomeReject,
		RejectReason:  "Rejected via I3 project-scoped secret.",
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, pol))

	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Choose A or B?",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Equal(t, int32(1), providerCalls.Load())
	assert.Equal(t, projAPIKey, seenKeyHeader, "message_policy gate must forward the message's authorized ProjectID so project-scoped secrets resolve")

	decisions := mpgWaitForDecisionCount(t, f.s, f.project.ID, 1)
	require.Len(t, decisions, 1)
	assert.NotContains(t, string(decisions[0].Evaluation), projAPIKey, "decision evaluation must never contain project secret")
	var proj messagepolicy.EvaluationProjection
	require.NoError(t, json.Unmarshal(decisions[0].Evaluation, &proj))
	assert.Equal(t, "success", proj.Class)
	assert.Equal(t, "gemini-i3-mp", proj.Instance)

	// O1: Even when the caller's token claims a conflicting ProjectID (projB.ID),
	// the gate MUST derive ProjectID from the stored agent record (f.project.ID)
	// and resolve project A's secret, NEVER project B's same-named secret.
	_, _, err = f.s.AddProjectSystemOneInstance(ctx, projB.ID, "gemini-i3-mp")
	require.NoError(t, err)
	seenKeyHeader = ""
	rawBody, err := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Conflicting token claim test: choose A or B?",
	})
	require.NoError(t, err)
	conflictReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/"+f.project.ID+"/agents/"+f.sender.ID+"/outbound-message",
		bytes.NewReader(rawBody))
	conflictReq.Header.Set("Content-Type", "application/json")
	conflictReq = conflictReq.WithContext(contextWithIdentity(conflictReq.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: f.sender.ID},
		ProjectID: projB.ID, // conflicting claim!
		Ancestry:  f.sender.Ancestry,
	}}))
	conflictRec := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(conflictRec, conflictReq, f.sender.ID)
	require.Equal(t, http.StatusUnprocessableEntity, conflictRec.Code, conflictRec.Body.String())
	assert.Equal(t, int32(2), providerCalls.Load())
	assert.Equal(t, projAPIKey, seenKeyHeader, "stored agent ProjectID (A) must win over conflicting token claim (B)")
	assert.NotEqual(t, projBAPIKey, seenKeyHeader)
}

type mpCountingSecretBackend struct {
	secret.SecretBackend
	getCalls atomic.Int32
}

func (c *mpCountingSecretBackend) Get(ctx context.Context, key, scope, scopeID string) (*secret.SecretWithValue, error) {
	c.getCalls.Add(1)
	return c.SecretBackend.Get(ctx, key, scope, scopeID)
}

func TestMessagePolicySystemOne_TestEndpointAuthorityAndCredentialMatrix(t *testing.T) {
	ctx := context.Background()
	f := newMPHFixture(t, true)

	// Enable hub.message_policies, web.decisions, and hub.system_one.
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		switch e.Name {
		case experiments.MessagePolicies, experiments.Decisions, experiments.SystemOne:
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	f.srv.experiments = reg

	innerBackend := secret.NewLocalBackend(f.s, "test-hub-id", "test-secret-signing-key")
	countingSecrets := &mpCountingSecretBackend{SecretBackend: innerBackend}
	f.srv.SetSecretBackend(countingSecrets)

	var (
		adcMints      atomic.Int32
		providerCalls atomic.Int32
	)
	f.srv.SetSystemOneADCForTesting(t, func(context.Context, []string) (*SystemOneADCCredentials, error) {
		adcMints.Add(1)
		return &SystemOneADCCredentials{
			TokenSource: oauth2.StaticTokenSource(&oauth2.Token{
				AccessToken: "ya29.test-adc-token",
				TokenType:   "Bearer",
			}),
			CredentialType:      "service_account",
			ServiceAccountEmail: "test-sa@example.iam.gserviceaccount.com",
		}, nil
	})

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("x-goog-api-key") != "" || strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ya29.") {
			_, _ = w.Write([]byte(`{
				"answers": {"asks_choice": {"type": "noul", "noul": 0.93, "confidence": 0.93}},
				"usage": {"total_input_tokens": 12, "total_output_tokens": 0, "total_tokens": 12}
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"model": "jev-1.14.0",
			"answers": {"asks_choice": {"type": "noul", "noul": 0.93}},
			"usage": {"input_tokens": 12, "output_tokens": 1}
		}`))
	}))
	t.Cleanup(tlsSrv.Close)

	f.srv.setSystemOneRuntimeOptions(
		systemoneruntime.WithEgressOptionsForTesting(
			egress.WithDialerForTesting(hubDialectPublicResolver{}, hubDialectTargetDialer{targetAddr: tlsSrv.Listener.Addr().String()}),
			egress.WithTLSConfigForTesting(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // offline test TLS server
		),
	)

	// Seed hub-scoped and project-scoped GEMINI_API_KEY variable secrets.
	_, _, err = innerBackend.Set(ctx, &secret.SetSecretInput{
		Name:       systemoneprovider.DefaultGeminiAPIKeySecretName,
		Value:      "AIza-hub-scoped-matrix-key",
		SecretType: store.SecretTypeVariable,
		Scope:      store.ScopeHub,
		ScopeID:    "test-hub-id",
	})
	require.NoError(t, err)
	_, _, err = innerBackend.Set(ctx, &secret.SetSecretInput{
		Name:       systemoneprovider.DefaultGeminiAPIKeySecretName,
		Value:      "AIza-project-scoped-matrix-key",
		SecretType: store.SecretTypeVariable,
		Scope:      store.ScopeProject,
		ScopeID:    f.project,
	})
	require.NoError(t, err)

	// Create hub super-admin (for instance setup), hub-admin (has project.read +
	// project.manage_message_policies, NO project.secret_read), project admin,
	// and project member callers.
	superAdminID := hubConfigTokenUser(t, f.s, "mp-mat-super", store.SystemRoleSuperAdmin)
	superAdmin, err := f.s.GetUser(ctx, superAdminID)
	require.NoError(t, err)
	hubAdminID := hubConfigTokenUser(t, f.s, "mp-mat-hubadmin", store.SystemRoleHubAdmin)
	hubAdmin, err := f.s.GetUser(ctx, hubAdminID)
	require.NoError(t, err)

	projAdmin := projectOpsUser(t, f.s, "mp-mat-admin", false)
	projectOpsGrant(t, f.s, projAdmin, f.project, store.ProjectRoleAdmin)
	projMember := projectOpsUser(t, f.s, "mp-mat-member", false)
	projectOpsGrant(t, f.s, projMember, f.project, store.ProjectRoleMember)

	// Create 5 System One instances covering every credential mode.
	createInst := func(body map[string]any) {
		rec := doRequestAsUser(t, f.srv, superAdmin, http.MethodPost, systemOneAdminPath, body)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		if name, ok := body["name"].(string); ok && name != "" {
			_, _, aErr := f.s.AddProjectSystemOneInstance(ctx, f.project, name)
			require.NoError(t, aErr)
		}
	}
	createInst(map[string]any{
		"name":    "inst-owned",
		"adapter": "typesafe",
		"model":   "jev-1.14.0",
		"auth":    map[string]any{"kind": "api_key", "apiKey": "sk-owned-matrix-key"},
	})
	ownedRow, err := f.s.GetSystemOneInstanceByName(ctx, "inst-owned")
	require.NoError(t, err)
	ownedRow.Endpoint = "https://typesafe.example.test"
	require.NoError(t, f.s.UpdateSystemOneInstance(ctx, ownedRow))

	createInst(map[string]any{
		"name":    "inst-adc",
		"adapter": "gemini_decisions",
		"model":   "gemini-2.5-flash",
		"auth":    map[string]any{"kind": "google_access_token", "scopes": []string{"https://www.googleapis.com/auth/cloud-platform"}},
	})
	createInst(map[string]any{
		"name":    "inst-ref-hub",
		"adapter": "gemini_decisions",
		"model":   "gemini-2.5-flash",
		"auth": map[string]any{
			"kind":      "api_key",
			"secretRef": map[string]any{"name": systemoneprovider.DefaultGeminiAPIKeySecretName, "scope": systemoneprovider.SecretScopeHub},
		},
	})
	createInst(map[string]any{
		"name":    "inst-ref-project",
		"adapter": "gemini_decisions",
		"model":   "gemini-2.5-flash",
		"auth": map[string]any{
			"kind":      "api_key",
			"secretRef": map[string]any{"name": systemoneprovider.DefaultGeminiAPIKeySecretName, "scope": systemoneprovider.SecretScopeProject},
		},
	})
	createInst(map[string]any{
		"name":    "inst-ref-proj-or-hub",
		"adapter": "gemini_decisions",
		"model":   "gemini-2.5-flash",
		"auth": map[string]any{
			"kind":      "api_key",
			"secretRef": map[string]any{"name": systemoneprovider.DefaultGeminiAPIKeySecretName, "scope": systemoneprovider.SecretScopeProjectOrHub},
		},
	})

	// Mint UATs for the project owner with varying scope sets. Note that
	// project.secret_read is intentionally NOT a UAT-mintable scope, so no UAT
	// can satisfy the G4/L1 boundary for project / project_or_hub named refs.
	uatManageOnly := projectOpsMint(t, f.srv, f.owner, projectBoundary(f.project), "project:manage_message_policies")
	uatManageAndRead := projectOpsMint(t, f.srv, f.owner, projectBoundary(f.project), "project:manage_message_policies", "project:read")

	bearerDo := func(tok, method, path string, body any) *httptest.ResponseRecorder {
		raw, mErr := json.Marshal(body)
		require.NoError(t, mErr)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	agentDo := func(method, path string, body any) *httptest.ResponseRecorder {
		raw, mErr := json.Marshal(body)
		require.NoError(t, mErr)
		ident := &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: f.projectAgent},
			ProjectID: f.project,
		}}
		reqCtx := contextWithIdentity(context.Background(), ident)
		reqCtx = contextWithCredentialContext(reqCtx, CredentialContext{Kind: CredentialKindAgentJWT, ID: "ag-jwt"})
		req := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(reqCtx)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.srv.mux.ServeHTTP(rec, req)
		return rec
	}

	makeInlineReq := func(instName string) TestMessagePolicyRequest {
		return TestMessagePolicyRequest{
			ViolationRule: `!message.has_choices && judgment_flags.asks_choice`,
			SystemOneRule: `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"binding":{"instance":"` + instName + `"},"questions":[{"id":"asks_choice","type":"binary","instructions":"Asks choice?"}]}}`,
			OnUncertain:   store.MessagePolicyOutcomeReject,
			OnFailed:      store.MessagePolicyOutcomeReject,
			Sample: &MessagePolicyTestSample{
				Surface: store.MessagePolicySurfaceAgentToUser,
				Body:    "Pick option A or B?",
			},
		}
	}

	testURL := f.basePath() + "/test"

	// 1. Full caller × credential-mode matrix.
	type matrixCase struct {
		name       string
		inst       string
		call       func(req TestMessagePolicyRequest) *httptest.ResponseRecorder
		wantStatus int
		wantZeroIO bool
	}
	cases := []matrixCase{
		// Owned API key: manage-only UAT allowed; member/agent denied.
		{"owned/owner", "inst-owned", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return mphSessionDo(t, f.srv, f.owner, http.MethodPost, testURL, r)
		}, http.StatusOK, false},
		{"owned/admin", "inst-owned", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return mphSessionDo(t, f.srv, projAdmin, http.MethodPost, testURL, r)
		}, http.StatusOK, false},
		{"owned/hub_admin", "inst-owned", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return doRequestAsUser(t, f.srv, hubAdmin, http.MethodPost, testURL, r)
		}, http.StatusOK, false},
		{"owned/uat_manage_only", "inst-owned", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return bearerDo(uatManageOnly, http.MethodPost, testURL, r)
		}, http.StatusOK, false},
		{"owned/member_denied", "inst-owned", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return mphSessionDo(t, f.srv, projMember, http.MethodPost, testURL, r)
		}, http.StatusForbidden, true},
		{"owned/agent_denied", "inst-owned", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return agentDo(http.MethodPost, testURL, r)
		}, http.StatusForbidden, true},

		// ADC: owner, admin, hub-admin, and manage-only UAT allowed; member/agent denied.
		{"adc/owner", "inst-adc", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return mphSessionDo(t, f.srv, f.owner, http.MethodPost, testURL, r)
		}, http.StatusOK, false},
		{"adc/hub_admin", "inst-adc", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return doRequestAsUser(t, f.srv, hubAdmin, http.MethodPost, testURL, r)
		}, http.StatusOK, false},
		{"adc/uat_manage_only", "inst-adc", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return bearerDo(uatManageOnly, http.MethodPost, testURL, r)
		}, http.StatusOK, false},
		{"adc/member_denied", "inst-adc", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return mphSessionDo(t, f.srv, projMember, http.MethodPost, testURL, r)
		}, http.StatusForbidden, true},

		// Hub-scoped named ref: never reads project secret -> hub-admin & manage-only UAT allowed.
		{"ref_hub/hub_admin", "inst-ref-hub", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return doRequestAsUser(t, f.srv, hubAdmin, http.MethodPost, testURL, r)
		}, http.StatusOK, false},
		{"ref_hub/uat_manage_only", "inst-ref-hub", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return bearerDo(uatManageOnly, http.MethodPost, testURL, r)
		}, http.StatusOK, false},

		// Project-scoped named ref: G4/L1 boundary requires project.read + project.secret_read!
		// Owner and project-admin hold both; hub-admin and UATs do NOT hold project.secret_read.
		{"ref_project/owner", "inst-ref-project", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return mphSessionDo(t, f.srv, f.owner, http.MethodPost, testURL, r)
		}, http.StatusOK, false},
		{"ref_project/admin", "inst-ref-project", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return mphSessionDo(t, f.srv, projAdmin, http.MethodPost, testURL, r)
		}, http.StatusOK, false},
		{"ref_project/hub_admin_denied", "inst-ref-project", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return doRequestAsUser(t, f.srv, hubAdmin, http.MethodPost, testURL, r)
		}, http.StatusForbidden, true},
		{"ref_project/uat_manage_only_denied", "inst-ref-project", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return bearerDo(uatManageOnly, http.MethodPost, testURL, r)
		}, http.StatusForbidden, true},
		{"ref_project/uat_manage_and_read_denied", "inst-ref-project", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return bearerDo(uatManageAndRead, http.MethodPost, testURL, r)
		}, http.StatusForbidden, true},
		{"ref_project/member_denied", "inst-ref-project", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return mphSessionDo(t, f.srv, projMember, http.MethodPost, testURL, r)
		}, http.StatusForbidden, true},
		{"ref_project/agent_denied", "inst-ref-project", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return agentDo(http.MethodPost, testURL, r)
		}, http.StatusForbidden, true},

		// project_or_hub-scoped named ref: also enforces G4/L1 boundary!
		{"ref_proj_or_hub/owner", "inst-ref-proj-or-hub", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return mphSessionDo(t, f.srv, f.owner, http.MethodPost, testURL, r)
		}, http.StatusOK, false},
		{"ref_proj_or_hub/admin", "inst-ref-proj-or-hub", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return mphSessionDo(t, f.srv, projAdmin, http.MethodPost, testURL, r)
		}, http.StatusOK, false},
		{"ref_proj_or_hub/hub_admin_denied", "inst-ref-proj-or-hub", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return doRequestAsUser(t, f.srv, hubAdmin, http.MethodPost, testURL, r)
		}, http.StatusForbidden, true},
		{"ref_proj_or_hub/uat_manage_only_denied", "inst-ref-proj-or-hub", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return bearerDo(uatManageOnly, http.MethodPost, testURL, r)
		}, http.StatusForbidden, true},
		{"ref_proj_or_hub/uat_manage_and_read_denied", "inst-ref-proj-or-hub", func(r TestMessagePolicyRequest) *httptest.ResponseRecorder {
			return bearerDo(uatManageAndRead, http.MethodPost, testURL, r)
		}, http.StatusForbidden, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sBefore := countingSecrets.getCalls.Load()
			mBefore := adcMints.Load()
			pBefore := providerCalls.Load()

			rec := tc.call(makeInlineReq(tc.inst))
			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			if tc.wantZeroIO {
				assert.Equal(t, sBefore, countingSecrets.getCalls.Load(), "denied request must make 0 secret lookups")
				assert.Equal(t, mBefore, adcMints.Load(), "denied request must make 0 ADC token mints")
				assert.Equal(t, pBefore, providerCalls.Load(), "denied request must make 0 provider calls")
			}
		})
	}

	// 2. Synthetic judgments: ZERO provider requests, ZERO ADC mints, ZERO secret lookups
	// (even with manage-only UAT on a project-scoped named ref, because no secret is accessed).
	t.Run("synthetic_judgments_zero_provider_mint_and_secret_calls", func(t *testing.T) {
		sBefore := countingSecrets.getCalls.Load()
		mBefore := adcMints.Load()
		pBefore := providerCalls.Load()

		req := makeInlineReq("inst-ref-project")
		req.Sample.JudgmentFlags = map[string]bool{"asks_choice": true}
		rec := bearerDo(uatManageOnly, http.MethodPost, testURL, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp TestMessagePolicyResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, store.MessagePolicyClassViolation, resp.Class)
		assert.Empty(t, resp.Evaluation)

		assert.Equal(t, sBefore, countingSecrets.getCalls.Load(), "synthetic judgments must make 0 secret lookups")
		assert.Equal(t, mBefore, adcMints.Load(), "synthetic judgments must make 0 ADC mints")
		assert.Equal(t, pBefore, providerCalls.Load(), "synthetic judgments must make 0 provider calls")
	})

	// 3. Error paths and scope non-match without synthetic values must never trigger live calls.
	t.Run("error_and_nonmatch_paths_make_zero_live_calls", func(t *testing.T) {
		sBefore := countingSecrets.getCalls.Load()
		mBefore := adcMints.Load()
		pBefore := providerCalls.Load()

		// 3a. Invalid System One rule -> 400.
		badS1 := makeInlineReq("inst-ref-project")
		badS1.SystemOneRule = `{"dialect":"system_one","inputs":["secret.nope"],"spec":{"version":1,"questions":[{"id":"q1","type":"binary","instructions":"Check."}]}}`
		rec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, testURL, badS1)
		require.Equal(t, http.StatusBadRequest, rec.Code)

		// 3b. Uncovered sample surface -> 400.
		badSurf := makeInlineReq("inst-ref-project")
		badSurf.Sample.Surface = store.MessagePolicySurfaceAgentDM
		rec = mphSessionDo(t, f.srv, f.owner, http.MethodPost, testURL, badSurf)
		require.Equal(t, http.StatusBadRequest, rec.Code)

		// 3c. Scope mismatch -> 200 scope_matched=false.
		mismatch := makeInlineReq("inst-ref-project")
		mismatch.Scope = &store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToConversation}}
		mismatch.Sample.Surface = store.MessagePolicySurfaceAgentToUser
		rec = mphSessionDo(t, f.srv, f.owner, http.MethodPost, testURL, mismatch)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp TestMessagePolicyResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.False(t, resp.ScopeMatched)

		assert.Equal(t, sBefore, countingSecrets.getCalls.Load(), "error/non-match paths must make 0 secret lookups")
		assert.Equal(t, mBefore, adcMints.Load(), "error/non-match paths must make 0 ADC mints")
		assert.Equal(t, pBefore, providerCalls.Load(), "error/non-match paths must make 0 provider calls")
	})
}

func TestMessagePolicySystemOne_FullBodyLimitsRunBeforeCredentialIO(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		switch e.Name {
		case experiments.MessagePolicies, experiments.Decisions, experiments.SystemOne:
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	f.srv.experiments = reg

	innerBackend := secret.NewLocalBackend(f.s, "test-hub-id", "test-secret-signing-key")
	countingSecrets := &mpCountingSecretBackend{SecretBackend: innerBackend}
	f.srv.SetSecretBackend(countingSecrets)

	var (
		adcMints      atomic.Int32
		providerCalls atomic.Int32
	)
	f.srv.SetSystemOneADCForTesting(t, func(context.Context, []string) (*SystemOneADCCredentials, error) {
		adcMints.Add(1)
		return &SystemOneADCCredentials{
			TokenSource: oauth2.StaticTokenSource(&oauth2.Token{
				AccessToken: "ya29.test-adc",
				TokenType:   "Bearer",
			}),
			CredentialType: "service_account",
		}, nil
	})

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"answers": {"asks_choice": {"type": "noul", "noul": 0.91, "confidence": 0.91}},
			"usage": {"total_input_tokens": 20, "total_output_tokens": 0, "total_tokens": 20}
		}`))
	}))
	t.Cleanup(tlsSrv.Close)

	f.srv.setSystemOneRuntimeOptions(
		systemoneruntime.WithEgressOptionsForTesting(
			egress.WithDialerForTesting(hubDialectPublicResolver{}, hubDialectTargetDialer{targetAddr: tlsSrv.Listener.Addr().String()}),
			egress.WithTLSConfigForTesting(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // offline test TLS server
		),
	)

	adminID := hubConfigTokenUser(t, f.s, "mp-oversize-admin", store.SystemRoleSuperAdmin)
	admin, err := f.s.GetUser(ctx, adminID)
	require.NoError(t, err)

	_, _, err = innerBackend.Set(ctx, &secret.SetSecretInput{
		Name:       systemoneprovider.DefaultGeminiAPIKeySecretName,
		Value:      "AIza-oversize-project-key",
		SecretType: store.SecretTypeVariable,
		Scope:      store.ScopeProject,
		ScopeID:    f.project.ID,
	})
	require.NoError(t, err)

	createRec := doRequestAsUser(t, f.srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "gemini-oversize",
		"adapter": "gemini_decisions",
		"model":   "gemini-2.5-flash",
		"auth": map[string]any{
			"kind":      "api_key",
			"secretRef": map[string]any{"name": systemoneprovider.DefaultGeminiAPIKeySecretName, "scope": systemoneprovider.SecretScopeProject},
		},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	_, _, err = f.s.AddProjectSystemOneInstance(ctx, f.project.ID, "gemini-oversize")
	require.NoError(t, err)

	// Oversize body: 9000 'é' runes (<= 16000 rune ingress limit, 18000 bytes > 16384 byte field limit).
	oversizeBody := strings.Repeat("é", 9000)

	// 1. on_oversize: reject -> input_too_large BEFORE any secret lookup, ADC mint, or provider call.
	rejectPol := &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "a-s1-oversize-reject",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: `judgment_flags.asks_choice`,
		SystemOneRule: `{"dialect":"system_one","inputs":["message.body"],"on_oversize":"reject","spec":{"version":1,"binding":{"instance":"gemini-oversize"},"questions":[{"id":"asks_choice","type":"binary","instructions":"Check."}]}}`,
		SystemOneRefs: []string{"gemini-oversize"},
		OnUncertain:   store.MessagePolicyOutcomeReject,
		OnFailed:      store.MessagePolicyOutcomeReject,
		RejectReason:  "Oversize rejected.",
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, rejectPol))

	// CEL-only policy on the same send keeps exact M1 body_truncated behavior.
	celPol := &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "b-cel-only",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: `false`,
		OnFailed:      store.MessagePolicyOutcomeAdmit,
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, celPol))

	sBefore := countingSecrets.getCalls.Load()
	mBefore := adcMints.Load()
	pBefore := providerCalls.Load()

	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       oversizeBody,
	})
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Equal(t, sBefore, countingSecrets.getCalls.Load(), "on_oversize: reject must fail BEFORE secret lookup")
	assert.Equal(t, mBefore, adcMints.Load(), "on_oversize: reject must fail BEFORE ADC token mint")
	assert.Equal(t, pBefore, providerCalls.Load(), "on_oversize: reject must fail BEFORE provider call")

	decs, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: f.project.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, decs.Items, 2)
	reasonsByPolicy := map[string]string{}
	for _, d := range decs.Items {
		reasonsByPolicy[d.PolicyName] = d.CELErrorReason
	}
	assert.Equal(t, string(systemoneprovider.FailInputTooLarge), reasonsByPolicy["a-s1-oversize-reject"])
	assert.Equal(t, messagepolicy.ReasonBodyTruncated, reasonsByPolicy["b-cel-only"])

	// 2. on_oversize: truncate -> truncates UTF-8 cleanly and records Truncated: true in provenance.
	rejectPol.SystemOneRule = `{"dialect":"system_one","inputs":["message.body"],"on_oversize":"truncate","spec":{"version":1,"binding":{"instance":"gemini-oversize"},"questions":[{"id":"asks_choice","type":"binary","instructions":"Check."}]}}`
	require.NoError(t, f.s.UpdateMessagePolicy(ctx, rejectPol))

	rrTrunc := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       oversizeBody,
	})
	require.Equal(t, http.StatusUnprocessableEntity, rrTrunc.Code, rrTrunc.Body.String())
	assert.Equal(t, pBefore+1, providerCalls.Load())

	decs2, err := f.s.ListMessagePolicyDecisions(ctx, store.MessagePolicyDecisionFilter{
		ProjectID: f.project.ID,
		PolicyID:  rejectPol.ID,
	}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.NotEmpty(t, decs2.Items)
	var proj messagepolicy.EvaluationProjection
	require.NoError(t, json.Unmarshal(decs2.Items[0].Evaluation, &proj))
	assert.True(t, proj.Truncated, "oversize truncate must record truncated=true in EvaluationProjection")
	require.Len(t, proj.InputFields, 1)
	assert.True(t, proj.InputFields[0].Truncated)
}

func TestMessagePolicySystemOne_P2bAllowlistAndCapacityEnforcement(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		switch e.Name {
		case experiments.MessagePolicies, experiments.Decisions, experiments.SystemOne:
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	f.srv.experiments = reg

	innerBackend := secret.NewLocalBackend(f.s, "test-hub-id", "test-secret-signing-key")
	countingSecrets := &mpCountingSecretBackend{SecretBackend: innerBackend}
	f.srv.SetSecretBackend(countingSecrets)
	f.srv.SetSystemOneADCForTesting(t, func(context.Context, []string) (*SystemOneADCCredentials, error) {
		return nil, egress.ErrAuth
	})

	var providerCalls atomic.Int32
	var holdNext atomic.Bool
	inFlightEntered := make(chan struct{}, 4)
	releaseInFlight := make(chan struct{})

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		if holdNext.CompareAndSwap(true, false) {
			inFlightEntered <- struct{}{}
			select {
			case <-releaseInFlight:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"answers": {"asks_choice": {"type": "noul", "noul": 0.04, "confidence": 0.96}},
			"usage": {"total_input_tokens": 14, "total_output_tokens": 6, "total_tokens": 20}
		}`))
	}))
	t.Cleanup(tlsSrv.Close)

	// Short admission wait so capacity rejection is fast and deterministic.
	f.srv.config.SystemOneAdmissionWait = 25 * time.Millisecond
	f.srv.setSystemOneRuntimeOptions(
		systemoneruntime.WithEgressOptionsForTesting(
			egress.WithDialerForTesting(hubDialectPublicResolver{}, hubDialectTargetDialer{targetAddr: tlsSrv.Listener.Addr().String()}),
			egress.WithTLSConfigForTesting(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // offline test TLS server
		),
	)

	adminID := hubConfigTokenUser(t, f.s, "mp-p2b-admin", store.SystemRoleSuperAdmin)
	admin, err := f.s.GetUser(ctx, adminID)
	require.NoError(t, err)

	owner := projectOpsUser(t, f.s, "mp-p2b-owner", false)
	projectOpsGrant(t, f.s, owner, f.project.ID, store.ProjectRoleOwner)

	_, _, err = innerBackend.Set(ctx, &secret.SetSecretInput{
		Name:       systemoneprovider.DefaultGeminiAPIKeySecretName,
		Value:      "AIza-p2b-project-key",
		SecretType: store.SecretTypeVariable,
		Scope:      store.ScopeProject,
		ScopeID:    f.project.ID,
	})
	require.NoError(t, err)

	// Create a System One instance with maxInFlight=1 and a project-scoped named secret ref.
	// Initially it is NOT on f.project's allowlist (empty by default).
	createInstRec := doRequestAsUser(t, f.srv, admin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "gemini-p2b",
		"adapter": "gemini_decisions",
		"model":   "gemini-2.5-flash",
		"limits":  map[string]any{"maxInFlight": 1},
		"auth": map[string]any{
			"kind":      "api_key",
			"secretRef": map[string]any{"name": systemoneprovider.DefaultGeminiAPIKeySecretName, "scope": systemoneprovider.SecretScopeProject},
		},
	})
	require.Equal(t, http.StatusCreated, createInstRec.Code, createInstRec.Body.String())

	mpBase := "/api/v1/projects/" + f.project.ID + "/message-policies"
	allowlistPath := "/api/v1/projects/" + f.project.ID + "/system-one/instances/gemini-p2b"
	s1Rule := `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"binding":{"instance":"gemini-p2b"},"questions":[{"id":"asks_choice","type":"binary","instructions":"Asks choice?"}]}}`

	// 1. Authoring refusal while instance is NOT on the project allowlist.
	sBefore := countingSecrets.getCalls.Load()
	pBefore := providerCalls.Load()
	authorDeniedRec := mphSessionDo(t, f.srv, owner, http.MethodPost, mpBase, CreateMessagePolicyRequest{
		Name:          "p2b-unlisted",
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: `judgment_flags.asks_choice`,
		SystemOneRule: s1Rule,
		OnUncertain:   store.MessagePolicyOutcomeReject,
		OnFailed:      store.MessagePolicyOutcomeReject,
	})
	require.Equal(t, http.StatusBadRequest, authorDeniedRec.Code, authorDeniedRec.Body.String())
	assert.Contains(t, authorDeniedRec.Body.String(), messagepolicy.ErrCodeInvalidRule)
	assert.Equal(t, sBefore, countingSecrets.getCalls.Load(), "unlisted authoring must make 0 secret reads")
	assert.Equal(t, pBefore, providerCalls.Load(), "unlisted authoring must make 0 provider requests")

	// 2. Allowlist gemini-p2b on f.project, then create shadow and enforce policies.
	allowRec := mphSessionDo(t, f.srv, owner, http.MethodPut, allowlistPath, nil)
	require.Equal(t, http.StatusOK, allowRec.Code, allowRec.Body.String())

	shadowEnabled := true
	shadowRec := mphSessionDo(t, f.srv, owner, http.MethodPost, mpBase, CreateMessagePolicyRequest{
		Name:          "a-p2b-shadow",
		Enabled:       &shadowEnabled,
		Mode:          store.MessagePolicyModeShadow,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: `judgment_flags.asks_choice`,
		SystemOneRule: s1Rule,
		OnUncertain:   store.MessagePolicyOutcomeReject,
		OnFailed:      store.MessagePolicyOutcomeReject,
	})
	require.Equal(t, http.StatusCreated, shadowRec.Code, shadowRec.Body.String())
	var shadowCreated MessagePolicyResponse
	require.NoError(t, json.Unmarshal(shadowRec.Body.Bytes(), &shadowCreated))

	enforceEnabled := false
	enforceRec := mphSessionDo(t, f.srv, owner, http.MethodPost, mpBase, CreateMessagePolicyRequest{
		Name:          "b-p2b-enforce",
		Enabled:       &enforceEnabled,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: `judgment_flags.asks_choice`,
		SystemOneRule: s1Rule,
		OnUncertain:   store.MessagePolicyOutcomeReject,
		OnFailed:      store.MessagePolicyOutcomeReject,
		RejectReason:  "Policy failed.",
	})
	require.Equal(t, http.StatusCreated, enforceRec.Code, enforceRec.Body.String())
	var enforceCreated MessagePolicyResponse
	require.NoError(t, json.Unmarshal(enforceRec.Body.Bytes(), &enforceCreated))

	setEnabled := func(policyID string, on bool) {
		p, gErr := f.s.GetMessagePolicy(ctx, policyID)
		require.NoError(t, gErr)
		p.Enabled = on
		require.NoError(t, f.s.UpdateMessagePolicy(ctx, p))
	}

	// 3. Remove gemini-p2b from the project allowlist -> immediate no_eligible_instance
	//    with 0 provider requests AND 0 secret reads across shadow, enforce, and /test.
	rmRec := mphSessionDo(t, f.srv, owner, http.MethodDelete, allowlistPath, nil)
	require.Equal(t, http.StatusOK, rmRec.Code, rmRec.Body.String())

	t.Run("allowlist_refusal_shadow", func(t *testing.T) {
		s0 := countingSecrets.getCalls.Load()
		p0 := providerCalls.Load()

		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "Shadow send while unlisted.",
		})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		decs := mpgWaitForDecisionCount(t, f.s, f.project.ID, 1)
		assert.Equal(t, store.MessagePolicyModeShadow, decs[0].Mode)
		assert.Equal(t, store.MessagePolicyClassFailed, decs[0].Class)
		assert.Equal(t, string(systemoneprovider.FailNoEligibleInstance), decs[0].CELErrorReason)
		assert.Equal(t, s0, countingSecrets.getCalls.Load(), "shadow allowlist refusal must make 0 secret reads")
		assert.Equal(t, p0, providerCalls.Load(), "shadow allowlist refusal must make 0 provider requests")
	})

	t.Run("allowlist_refusal_enforce", func(t *testing.T) {
		setEnabled(shadowCreated.ID, false)
		setEnabled(enforceCreated.ID, true)

		s0 := countingSecrets.getCalls.Load()
		p0 := providerCalls.Load()

		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "Enforce send while unlisted.",
		})
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
		decs := mpgWaitForDecisionCount(t, f.s, f.project.ID, 2)
		var enfDec *store.MessagePolicyDecision
		for i := range decs {
			if decs[i].PolicyID == enforceCreated.ID {
				enfDec = &decs[i]
				break
			}
		}
		require.NotNil(t, enfDec)
		assert.Equal(t, store.MessagePolicyClassFailed, enfDec.Class)
		assert.Equal(t, string(systemoneprovider.FailNoEligibleInstance), enfDec.CELErrorReason)
		assert.Equal(t, store.MessagePolicyActionRejected, enfDec.Action)
		assert.Equal(t, s0, countingSecrets.getCalls.Load(), "enforce allowlist refusal must make 0 secret reads")
		assert.Equal(t, p0, providerCalls.Load(), "enforce allowlist refusal must make 0 provider requests")
	})

	t.Run("allowlist_refusal_test_endpoint_saved_and_inline", func(t *testing.T) {
		s0 := countingSecrets.getCalls.Load()
		p0 := providerCalls.Load()

		// Saved policy /test.
		savedRec := mphSessionDo(t, f.srv, owner, http.MethodPost, mpBase+"/"+enforceCreated.ID+"/test", TestMessagePolicyRequest{
			Sample: &MessagePolicyTestSample{
				Surface: store.MessagePolicySurfaceAgentToUser,
				Body:    "Saved policy test while unlisted.",
			},
		})
		require.Equal(t, http.StatusOK, savedRec.Code, savedRec.Body.String())
		var savedResp TestMessagePolicyResponse
		require.NoError(t, json.Unmarshal(savedRec.Body.Bytes(), &savedResp))
		assert.Equal(t, store.MessagePolicyClassFailed, savedResp.Class)
		assert.Equal(t, string(systemoneprovider.FailNoEligibleInstance), savedResp.ReasonCode)
		assert.Equal(t, store.MessagePolicyOutcomeReject, savedResp.Outcome)

		// Inline /test.
		inlineRec := mphSessionDo(t, f.srv, owner, http.MethodPost, mpBase+"/test", TestMessagePolicyRequest{
			ViolationRule: `judgment_flags.asks_choice`,
			SystemOneRule: s1Rule,
			OnUncertain:   store.MessagePolicyOutcomeReject,
			OnFailed:      store.MessagePolicyOutcomeReject,
			Sample: &MessagePolicyTestSample{
				Surface: store.MessagePolicySurfaceAgentToUser,
				Body:    "Inline test while unlisted.",
			},
		})
		require.Equal(t, http.StatusOK, inlineRec.Code, inlineRec.Body.String())
		var inlineResp TestMessagePolicyResponse
		require.NoError(t, json.Unmarshal(inlineRec.Body.Bytes(), &inlineResp))
		assert.Equal(t, store.MessagePolicyClassFailed, inlineResp.Class)
		assert.Equal(t, string(systemoneprovider.FailNoEligibleInstance), inlineResp.ReasonCode)
		assert.Equal(t, store.MessagePolicyOutcomeReject, inlineResp.Outcome)

		assert.Equal(t, s0, countingSecrets.getCalls.Load(), "/test allowlist refusal must make 0 secret reads")
		assert.Equal(t, p0, providerCalls.Load(), "/test allowlist refusal must make 0 provider requests")
	})

	// 4. Re-allowlist gemini-p2b and saturate its maxInFlight=1 slot -> capacity
	//    rejection across shadow, enforce, and /test with 0 additional provider requests.
	reAllowRec := mphSessionDo(t, f.srv, owner, http.MethodPut, allowlistPath, nil)
	require.Equal(t, http.StatusOK, reAllowRec.Code, reAllowRec.Body.String())

	holdNext.Store(true)
	holderDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		holderDone <- mphSessionDo(t, f.srv, owner, http.MethodPost, mpBase+"/"+enforceCreated.ID+"/test", TestMessagePolicyRequest{
			Sample: &MessagePolicyTestSample{
				Surface: store.MessagePolicySurfaceAgentToUser,
				Body:    "Occupies maxInFlight=1 slot.",
			},
		})
	}()
	select {
	case <-inFlightEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for holder request to enter mock provider")
	}

	pWhileFull := providerCalls.Load()

	t.Run("capacity_enforce", func(t *testing.T) {
		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "Enforce send at capacity.",
		})
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
		decs := mpgWaitForDecisionCount(t, f.s, f.project.ID, 3)
		assert.Equal(t, store.MessagePolicyClassFailed, decs[0].Class)
		assert.Equal(t, string(systemoneprovider.FailCapacity), decs[0].CELErrorReason)
		assert.Equal(t, store.MessagePolicyActionRejected, decs[0].Action)
		assert.Equal(t, pWhileFull, providerCalls.Load(), "capacity enforce refusal must make 0 additional provider requests")
	})

	t.Run("capacity_shadow", func(t *testing.T) {
		setEnabled(enforceCreated.ID, false)
		setEnabled(shadowCreated.ID, true)

		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "Shadow send at capacity.",
		})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		decs := mpgWaitForDecisionCount(t, f.s, f.project.ID, 4)
		assert.Equal(t, store.MessagePolicyModeShadow, decs[0].Mode)
		assert.Equal(t, store.MessagePolicyClassFailed, decs[0].Class)
		assert.Equal(t, string(systemoneprovider.FailCapacity), decs[0].CELErrorReason)
		assert.Equal(t, pWhileFull, providerCalls.Load(), "capacity shadow refusal must make 0 additional provider requests")
	})

	t.Run("capacity_test_endpoint", func(t *testing.T) {
		capRec := mphSessionDo(t, f.srv, owner, http.MethodPost, mpBase+"/"+enforceCreated.ID+"/test", TestMessagePolicyRequest{
			Sample: &MessagePolicyTestSample{
				Surface: store.MessagePolicySurfaceAgentToUser,
				Body:    "Policy test at capacity.",
			},
		})
		require.Equal(t, http.StatusOK, capRec.Code, capRec.Body.String())
		var capResp TestMessagePolicyResponse
		require.NoError(t, json.Unmarshal(capRec.Body.Bytes(), &capResp))
		assert.Equal(t, store.MessagePolicyClassFailed, capResp.Class)
		assert.Equal(t, string(systemoneprovider.FailCapacity), capResp.ReasonCode)
		assert.Equal(t, store.MessagePolicyOutcomeReject, capResp.Outcome)
		assert.Equal(t, pWhileFull, providerCalls.Load(), "capacity /test refusal must make 0 additional provider requests")
	})

	// Release the held slot and verify subsequent evaluation succeeds.
	close(releaseInFlight)
	holderRec := <-holderDone
	require.Equal(t, http.StatusOK, holderRec.Code, holderRec.Body.String())

	afterRec := mphSessionDo(t, f.srv, owner, http.MethodPost, mpBase+"/"+enforceCreated.ID+"/test", TestMessagePolicyRequest{
		Sample: &MessagePolicyTestSample{
			Surface: store.MessagePolicySurfaceAgentToUser,
			Body:    "Post-capacity test succeeds.",
		},
	})
	require.Equal(t, http.StatusOK, afterRec.Code, afterRec.Body.String())
	var afterResp TestMessagePolicyResponse
	require.NoError(t, json.Unmarshal(afterRec.Body.Bytes(), &afterResp))
	assert.Equal(t, store.MessagePolicyClassPass, afterResp.Class)
}

func TestMessagePolicySystemOne_H2DialectDisabledShadowEnforceMixed(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	var providerCalls atomic.Int32
	mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-dis", func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-1.14.0","answers":{"asks_choice":{"type":"noul","noul":0.1}},"usage":{"input_tokens":10,"output_tokens":1}}`))
	})

	s1Rule := `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"binding":{"instance":"ts-dis"},"questions":[{"id":"asks_choice","type":"binary","instructions":"Asks choice?"}]}}`

	// Author policies while hub.system_one is ON: on_failed=admit, but
	// on_failed_reasons[dialect_disabled]=reject.
	shadowPol := &store.MessagePolicy{
		ProjectID:       f.project.ID,
		Name:            "a-s1-shadow-dis",
		Enabled:         true,
		Mode:            store.MessagePolicyModeShadow,
		Scope:           store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule:   `judgment_flags.asks_choice`,
		SystemOneRule:   s1Rule,
		SystemOneRefs:   []string{"ts-dis"},
		OnUncertain:     store.MessagePolicyOutcomeAdmit,
		OnFailed:        store.MessagePolicyOutcomeAdmit,
		OnFailedReasons: map[string]string{systemone.ReasonDialectDisabled: store.MessagePolicyOutcomeReject},
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, shadowPol))

	enforcePol := &store.MessagePolicy{
		ProjectID:       f.project.ID,
		Name:            "b-s1-enforce-dis",
		Enabled:         false,
		Mode:            store.MessagePolicyModeEnforce,
		Scope:           store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule:   `judgment_flags.asks_choice`,
		SystemOneRule:   s1Rule,
		SystemOneRefs:   []string{"ts-dis"},
		OnUncertain:     store.MessagePolicyOutcomeAdmit,
		OnFailed:        store.MessagePolicyOutcomeAdmit,
		OnFailedReasons: map[string]string{systemone.ReasonDialectDisabled: store.MessagePolicyOutcomeReject},
		RejectReason:    "System One dialect is disabled.",
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, enforcePol))

	celPol := &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "c-cel-pass",
		Enabled:       false,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: `false`,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, celPol))

	// Flip hub.system_one OFF (keep hub.message_policies ON).
	var exps []experiments.Experiment
	for _, e := range experiments.Default().All() {
		switch e.Name {
		case experiments.MessagePolicies, experiments.Decisions:
			e.Default = true
		case experiments.SystemOne:
			e.Default = false
		}
		exps = append(exps, e)
	}
	regOff, err := experiments.NewRegistry(exps, nil)
	require.NoError(t, err)
	f.srv.experiments = regOff

	admErrsBefore := f.srv.messagePolicyAdmissionErrors.Load()

	// 1. Shadow mode: delivers 200, records class=failed, reason=dialect_disabled, 0 provider calls, 0 admission errors.
	rrShadow := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Shadow send while hub.system_one is disabled.",
	})
	require.Equal(t, http.StatusOK, rrShadow.Code, rrShadow.Body.String())
	decs := mpgWaitForDecisionCount(t, f.s, f.project.ID, 1)
	assert.Equal(t, store.MessagePolicyModeShadow, decs[0].Mode)
	assert.Equal(t, store.MessagePolicyClassFailed, decs[0].Class)
	assert.Equal(t, systemone.ReasonDialectDisabled, decs[0].CELErrorReason)
	assert.Zero(t, providerCalls.Load())
	assert.Equal(t, admErrsBefore, f.srv.messagePolicyAdmissionErrors.Load(), "dialect_disabled must not increment admission error counter")

	// 2. Enforce + mixed (CEL pass + System One disabled): on_failed_reasons[dialect_disabled]=reject overrides on_failed=admit -> 422.
	shadowPol.Enabled = false
	require.NoError(t, f.s.UpdateMessagePolicy(ctx, shadowPol))
	enforcePol.Enabled = true
	require.NoError(t, f.s.UpdateMessagePolicy(ctx, enforcePol))
	celPol.Enabled = true
	require.NoError(t, f.s.UpdateMessagePolicy(ctx, celPol))

	rrEnf := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Mixed enforce send while hub.system_one is disabled.",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rrEnf.Code, rrEnf.Body.String())
	var enfErr ErrorResponse
	require.NoError(t, json.Unmarshal(rrEnf.Body.Bytes(), &enfErr))
	assert.Equal(t, "failed", enfErr.Error.Details["class"])
	assert.Equal(t, "b-s1-enforce-dis", enfErr.Error.Details["policy"])
	assert.Zero(t, providerCalls.Load())
	assert.Equal(t, admErrsBefore, f.srv.messagePolicyAdmissionErrors.Load(), "dialect_disabled must not increment admission error counter")

	decsAll := mpgWaitForDecisionCount(t, f.s, f.project.ID, 3)
	foundEnf := false
	for _, d := range decsAll {
		if d.PolicyID == enforcePol.ID {
			foundEnf = true
			assert.Equal(t, store.MessagePolicyClassFailed, d.Class)
			assert.Equal(t, systemone.ReasonDialectDisabled, d.CELErrorReason)
			assert.Equal(t, store.MessagePolicyActionRejected, d.Action)
		}
	}
	assert.True(t, foundEnf, "expected enforce decision row for b-s1-enforce-dis")
}

type mpListAgentsFaultStore struct {
	store.Store
	sw    *storeFaultSwitch
	calls atomic.Int32
}

func (w *mpListAgentsFaultStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	if w.sw.isArmed() {
		w.calls.Add(1)
		return nil, errors.New("injected ListAgents fault")
	}
	return w.Store.ListAgents(ctx, filter, opts)
}

func TestMessagePolicySystemOne_R2GroupSendCELOnlySkipsAgentListing(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	// Create a second agent participant in the group conversation.
	peerAgent := &store.Agent{
		ID:        "ag-r2-peer",
		Slug:      "peer-agent",
		Name:      "Peer Agent",
		ProjectID: f.project.ID,
		Phase:     "running",
	}
	require.NoError(t, f.s.CreateAgent(ctx, peerAgent))

	conv := &store.Conversation{
		ID:        "conv-r2-group",
		ProjectID: f.project.ID,
		Kind:      "group",
		Name:      "r2-team-thread",
		Surface:   "native",
	}
	require.NoError(t, f.s.CreateConversation(ctx, conv))
	require.NoError(t, f.s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID,
		PrincipalKind:  "agent",
		PrincipalID:    f.sender.ID,
	}))
	require.NoError(t, f.s.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: conv.ID,
		PrincipalKind:  "agent",
		PrincipalID:    peerAgent.ID,
	}))

	// Create a CEL-only enforce policy with on_failed=reject.
	celPol := &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "r2-cel-only-group",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToConversation}},
		ViolationRule: `message.body.contains("FORBIDDEN")`,
		OnFailed:      store.MessagePolicyOutcomeReject,
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, celPol))

	// Arm a ListAgents fault: CEL-only policy must NOT list project agents on a
	// group send without prose @-mentions, so the send admits with 0 ListAgents calls.
	hook, fault := installStoreFault(t, f.srv, func(inner store.Store, sw *storeFaultSwitch) *mpListAgentsFaultStore {
		return &mpListAgentsFaultStore{Store: inner, sw: sw}
	})
	fault.arm()
	defer fault.disarm()

	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "conv:" + conv.ID,
		Msg:       "Normal group update with no mentions.",
	})
	require.Equal(t, http.StatusOK, rr.Code, "CEL-only group send must admit without listing project agents: %s", rr.Body.String())
	assert.Zero(t, hook.calls.Load(), "CEL-only group send must make 0 ListAgents calls")
}

func TestMessagePolicySystemOne_O2UnboundRuleBeyond200InstancesL1Precheck(t *testing.T) {
	ctx := context.Background()
	f := newMPHFixture(t, true)

	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		switch e.Name {
		case experiments.MessagePolicies, experiments.Decisions, experiments.SystemOne:
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	f.srv.experiments = reg

	innerBackend := secret.NewLocalBackend(f.s, "test-hub-id", "test-secret-signing-key")
	countingSecrets := &mpCountingSecretBackend{SecretBackend: innerBackend}
	f.srv.SetSecretBackend(countingSecrets)
	f.srv.SetSystemOneADCForTesting(t, func(context.Context, []string) (*SystemOneADCCredentials, error) {
		return nil, egress.ErrAuth
	})

	var providerCalls atomic.Int32
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"answers": {"asks_choice": {"type": "noul", "noul": 0.91, "confidence": 0.91}},
			"usage": {"total_input_tokens": 12, "total_output_tokens": 0, "total_tokens": 12}
		}`))
	}))
	t.Cleanup(tlsSrv.Close)

	f.srv.setSystemOneRuntimeOptions(
		systemoneruntime.WithEgressOptionsForTesting(
			egress.WithDialerForTesting(hubDialectPublicResolver{}, hubDialectTargetDialer{targetAddr: tlsSrv.Listener.Addr().String()}),
			egress.WithTLSConfigForTesting(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // offline test TLS server
		),
	)

	_, _, err = innerBackend.Set(ctx, &secret.SetSecretInput{
		Name:       systemoneprovider.DefaultGeminiAPIKeySecretName,
		Value:      "AIza-o2-beyond-200-key",
		SecretType: store.SecretTypeVariable,
		Scope:      store.ScopeProject,
		ScopeID:    f.project,
	})
	require.NoError(t, err)

	// Seed 205 enabled owned-key dummy instances so the project-ref instance
	// sits strictly beyond page 1 (200) of ListSystemOneInstances.
	for i := 0; i < 205; i++ {
		name := fmt.Sprintf("a-dummy-%03d", i)
		require.NoError(t, f.s.CreateSystemOneInstance(ctx, &store.SystemOneInstance{
			ID:       fmt.Sprintf("inst-o2-dummy-%03d", i),
			Name:     name,
			Adapter:  "typesafe",
			Model:    "jev-1.14.0",
			Endpoint: "https://typesafe.example.test",
			AuthKind: "api_key",
			Enabled:  true,
			Revision: 1,
		}))
	}

	// Create the project-ref instance sorting alphabetically AFTER all 205 dummies
	// and allowlist ONLY it on f.project.
	superAdminID := hubConfigTokenUser(t, f.s, "mp-o2-super", store.SystemRoleSuperAdmin)
	superAdmin, err := f.s.GetUser(ctx, superAdminID)
	require.NoError(t, err)
	createRec := doRequestAsUser(t, f.srv, superAdmin, http.MethodPost, systemOneAdminPath, map[string]any{
		"name":    "z-beyond-200-proj-ref",
		"adapter": "gemini_decisions",
		"model":   "gemini-2.5-flash",
		"auth": map[string]any{
			"kind":      "api_key",
			"secretRef": map[string]any{"name": systemoneprovider.DefaultGeminiAPIKeySecretName, "scope": systemoneprovider.SecretScopeProject},
		},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	_, _, err = f.s.AddProjectSystemOneInstance(ctx, f.project, "z-beyond-200-proj-ref")
	require.NoError(t, err)

	hubAdminID := hubConfigTokenUser(t, f.s, "mp-o2-hubadmin", store.SystemRoleHubAdmin)
	hubAdmin, err := f.s.GetUser(ctx, hubAdminID)
	require.NoError(t, err)
	uatManageOnly := projectOpsMint(t, f.srv, f.owner, projectBoundary(f.project), "project:manage_message_policies")

	// Unbound System One rule (no spec.binding.instance) -> resolves the single allowlisted
	// project-ref instance beyond page 1.
	unboundReq := TestMessagePolicyRequest{
		ViolationRule: `judgment_flags.asks_choice`,
		SystemOneRule: `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"questions":[{"id":"asks_choice","type":"binary","instructions":"Asks choice?"}]}}`,
		OnUncertain:   store.MessagePolicyOutcomeReject,
		OnFailed:      store.MessagePolicyOutcomeReject,
		Sample: &MessagePolicyTestSample{
			Surface: store.MessagePolicySurfaceAgentToUser,
			Body:    "Pick A or B?",
		},
	}
	testURL := f.basePath() + "/test"

	// hub-admin (lacks project.secret_read) -> 403 with 0 secret reads and 0 provider calls.
	sBefore := countingSecrets.getCalls.Load()
	pBefore := providerCalls.Load()
	haRec := doRequestAsUser(t, f.srv, hubAdmin, http.MethodPost, testURL, unboundReq)
	require.Equal(t, http.StatusForbidden, haRec.Code, haRec.Body.String())
	assert.Equal(t, sBefore, countingSecrets.getCalls.Load(), "hub-admin must make 0 secret reads")
	assert.Equal(t, pBefore, providerCalls.Load(), "hub-admin must make 0 provider calls")

	// manage-only UAT (lacks project.secret_read) -> 403 with 0 secret reads and 0 provider calls.
	raw, err := json.Marshal(unboundReq)
	require.NoError(t, err)
	uatReq := httptest.NewRequest(http.MethodPost, testURL, bytes.NewReader(raw))
	uatReq.Header.Set("Content-Type", "application/json")
	uatReq.Header.Set("Authorization", "Bearer "+uatManageOnly)
	uatRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(uatRec, uatReq)
	require.Equal(t, http.StatusForbidden, uatRec.Code, uatRec.Body.String())
	assert.Equal(t, sBefore, countingSecrets.getCalls.Load(), "UAT must make 0 secret reads")
	assert.Equal(t, pBefore, providerCalls.Load(), "UAT must make 0 provider calls")

	// project owner (holds project.read + project.secret_read) -> 200 OK,
	// and execution resolves the exact same beyond-200 instance ("z-beyond-200-proj-ref").
	ownerRec := mphSessionDo(t, f.srv, f.owner, http.MethodPost, testURL, unboundReq)
	require.Equal(t, http.StatusOK, ownerRec.Code, ownerRec.Body.String())
	assert.Equal(t, pBefore+1, providerCalls.Load())
	var ownerResp TestMessagePolicyResponse
	require.NoError(t, json.Unmarshal(ownerRec.Body.Bytes(), &ownerResp))
	var ownerProj messagepolicy.EvaluationProjection
	require.NoError(t, json.Unmarshal(ownerResp.Evaluation, &ownerProj))
	assert.Equal(t, "z-beyond-200-proj-ref", ownerProj.Instance, "execution must resolve the exact same beyond-200 instance checked by L1 pre-check")
}
