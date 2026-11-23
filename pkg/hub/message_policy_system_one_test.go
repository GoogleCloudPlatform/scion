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
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	systemoneprovider "github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
)

func mpgEnableSystemOne(t *testing.T, srv *Server, s store.Store, instanceName string, handler http.HandlerFunc) (*httptest.Server, *store.User) {
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

func TestMessagePolicySystemOne_AuthoringMatrixAndDryRunTest(t *testing.T) {
	f := newMPHFixture(t, true)
	var providerCalls atomic.Int32
	mpgEnableSystemOne(t, f.srv, f.s, "ts-authoring", func(w http.ResponseWriter, _ *http.Request) {
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

	// 6. Dry-run test endpoint executes System One rule and returns content-free evaluation projection.
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
	mpgEnableSystemOne(t, f.srv, f.s, "ts-shadow", func(w http.ResponseWriter, _ *http.Request) {
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

	_, admin := mpgEnableSystemOne(t, f.srv, f.s, "ts-enforce", func(w http.ResponseWriter, r *http.Request) {
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
	_, _, err = f.srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name:       systemoneprovider.DefaultGeminiAPIKeySecretName,
		Value:      projAPIKey,
		SecretType: store.SecretTypeVariable,
		Scope:      store.ScopeProject,
		ScopeID:    f.project.ID,
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
}
