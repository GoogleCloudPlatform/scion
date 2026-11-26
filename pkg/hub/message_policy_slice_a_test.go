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
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func sliceAEnableAllExperiments(t *testing.T, srv *Server) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		switch e.Name {
		case experiments.MessagePolicies,
			experiments.Decisions,
			experiments.AgentSecrets,
			experiments.CommandRequests,
			experiments.SystemOne:
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
}

// TestL1_PromptTextInBody (Slice A acceptance test A9) verifies that normalized
// prompt text from decisions, secret requests, and command requests reaches
// both CEL and System One policies via message.body, while excluded identifiers,
// targets, URLs, and credentials are absent. A companion subtest asserts that
// no log, audit, or decision-row output contains a sentinel string planted in
// the prompt text.
func TestL1_PromptTextInBody(t *testing.T) {
	t.Run("decisions_reach_cel_and_system_one", func(t *testing.T) {
		ctx := context.Background()
		f := newMpgFixture(t, true)

		var mu sync.Mutex
		var providerPayloads []string
		mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-a9-dec", func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			providerPayloads = append(providerPayloads, string(raw))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"model": "jev-1.14.0",
				"answers": {"flagged": {"type": "noul", "noul": 0.95}},
				"usage": {"input_tokens": 20, "output_tokens": 1}
			}`))
		})
		sliceAEnableAllExperiments(t, f.srv)

		// Enforce CEL policy that matches on decision question, choice label/description, and other option.
		celPol := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "a9-cel-decisions",
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope:     store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule: `message.body.contains("DECISION_Q_TOKEN") && ` +
				`message.body.contains("CHOICE_LABEL_TOKEN") && ` +
				`message.body.contains("CHOICE_DESC_TOKEN") && ` +
				`message.body.contains("OTHER_LABEL_TOKEN") && ` +
				`message.body.contains("OTHER_DESC_TOKEN") && ` +
				`message.body.contains("OTHER_PLACEHOLDER_TOKEN") && ` +
				`!message.body.contains("EXCL_DEC_KEY") && ` +
				`!message.body.contains("EXCL_CHOICE_ID")`,
			OnFailed:     store.MessagePolicyOutcomeAdmit,
			RejectReason: "Matched decision prompt text via CEL.",
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, celPol))

		// System One policy selecting message.body so we can inspect the exact provider request payload.
		s1Pol := &store.MessagePolicy{
			ProjectID:     f.project.ID,
			Name:          "a9-s1-decisions",
			Enabled:       true,
			Mode:          store.MessagePolicyModeEnforce,
			Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule: `false`,
			SystemOneRule: `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"binding":{"instance":"ts-a9-dec"},"questions":[{"id":"flagged","type":"binary","instructions":"Check prompt."}]}}`,
			SystemOneRefs: []string{"ts-a9-dec"},
			OnUncertain:   store.MessagePolicyOutcomeAdmit,
			OnFailed:      store.MessagePolicyOutcomeAdmit,
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, s1Pol))

		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "Please review the question below.",
			Decisions: []messages.DecisionInput{
				{
					ID:       "EXCL_DEC_KEY",
					Question: "Which option? DECISION_Q_TOKEN",
					Default:  "EXCL_CHOICE_ID",
					Choices: []messages.ChoiceInput{
						{
							ID:          "EXCL_CHOICE_ID",
							Label:       "CHOICE_LABEL_TOKEN",
							Description: "CHOICE_DESC_TOKEN",
						},
					},
					AllowOther: &messages.OtherInput{
						Enabled:     true,
						Label:       "OTHER_LABEL_TOKEN",
						Description: "OTHER_DESC_TOKEN",
						Placeholder: "OTHER_PLACEHOLDER_TOKEN",
					},
				},
			},
		})
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
		assert.Contains(t, rr.Body.String(), "Matched decision prompt text via CEL.")

		mu.Lock()
		require.Len(t, providerPayloads, 1)
		payload := providerPayloads[0]
		mu.Unlock()
		for _, want := range []string{
			"DECISION_Q_TOKEN",
			"CHOICE_LABEL_TOKEN",
			"CHOICE_DESC_TOKEN",
			"OTHER_LABEL_TOKEN",
			"OTHER_DESC_TOKEN",
			"OTHER_PLACEHOLDER_TOKEN",
		} {
			assert.Contains(t, payload, want, "System One payload missing %q", want)
		}
		for _, excluded := range []string{"EXCL_DEC_KEY", "EXCL_CHOICE_ID"} {
			assert.NotContains(t, payload, excluded, "System One payload leaked %q", excluded)
		}
	})

	t.Run("secret_requests_reach_cel_and_system_one", func(t *testing.T) {
		ctx := context.Background()
		f := newMpgFixture(t, true)

		var mu sync.Mutex
		var providerPayloads []string
		mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-a9-sec", func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			providerPayloads = append(providerPayloads, string(raw))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"model": "jev-1.14.0",
				"answers": {"flagged": {"type": "noul", "noul": 0.95}},
				"usage": {"input_tokens": 20, "output_tokens": 1}
			}`))
		})
		sliceAEnableAllExperiments(t, f.srv)

		celPol := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "a9-cel-secrets",
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope:     store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule: `message.body.contains("SECRET_VAR_KEY_TOKEN") && ` +
				`message.body.contains("SECRET_DESC_TOKEN") && ` +
				`!message.body.contains("EXCLUDED_TARGET_PATH") && ` +
				`!message.body.contains("http://") && !message.body.contains("https://")`,
			OnFailed:     store.MessagePolicyOutcomeAdmit,
			RejectReason: "Matched secret request prompt text via CEL.",
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, celPol))

		s1Pol := &store.MessagePolicy{
			ProjectID:     f.project.ID,
			Name:          "a9-s1-secrets",
			Enabled:       true,
			Mode:          store.MessagePolicyModeEnforce,
			Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule: `false`,
			SystemOneRule: `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"binding":{"instance":"ts-a9-sec"},"questions":[{"id":"flagged","type":"binary","instructions":"Check prompt."}]}}`,
			SystemOneRefs: []string{"ts-a9-sec"},
			OnUncertain:   store.MessagePolicyOutcomeAdmit,
			OnFailed:      store.MessagePolicyOutcomeAdmit,
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, s1Pol))

		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "Need credentials.",
			SecretRequests: []messages.SecretRequestInput{
				{
					Key:         "SECRET_VAR_KEY_TOKEN",
					Description: "SECRET_DESC_TOKEN for staging",
					Type:        messages.SecretRequestTypeFile,
					Target:      "~/.config/EXCLUDED_TARGET_PATH",
				},
			},
		})
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
		assert.Contains(t, rr.Body.String(), "Matched secret request prompt text via CEL.")

		mu.Lock()
		require.Len(t, providerPayloads, 1)
		payload := providerPayloads[0]
		mu.Unlock()
		assert.Contains(t, payload, "SECRET_VAR_KEY_TOKEN")
		assert.Contains(t, payload, "SECRET_DESC_TOKEN")
		assert.NotContains(t, payload, "EXCLUDED_TARGET_PATH")
		assert.NotContains(t, payload, "sk-mp-s1-secret")
	})

	t.Run("command_request_reaches_cel_and_system_one", func(t *testing.T) {
		ctx := context.Background()
		f := newMpgFixture(t, true)

		var mu sync.Mutex
		var providerPayloads []string
		mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-a9-cmd", func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			providerPayloads = append(providerPayloads, string(raw))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"model": "jev-1.14.0",
				"answers": {"flagged": {"type": "noul", "noul": 0.95}},
				"usage": {"input_tokens": 20, "output_tokens": 1}
			}`))
		})
		sliceAEnableAllExperiments(t, f.srv)

		celPol := &store.MessagePolicy{
			ProjectID: f.project.ID,
			Name:      "a9-cel-cmd",
			Enabled:   true,
			Mode:      store.MessagePolicyModeEnforce,
			Scope:     store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule: `message.body.contains("CMD_RATIONALE_TOKEN") && ` +
				`message.body.contains("CMD_SCRIPT_TOKEN") && ` +
				`message.body.contains("/workspace/cmd-dir") && ` +
				`!message.body.contains("EXCLUDED_CMD_REQ_KEY") && ` +
				`!message.body.contains("777")`,
			OnFailed:     store.MessagePolicyOutcomeAdmit,
			RejectReason: "Matched command request prompt text via CEL.",
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, celPol))

		s1Pol := &store.MessagePolicy{
			ProjectID:     f.project.ID,
			Name:          "a9-s1-cmd",
			Enabled:       true,
			Mode:          store.MessagePolicyModeEnforce,
			Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule: `false`,
			SystemOneRule: `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"binding":{"instance":"ts-a9-cmd"},"questions":[{"id":"flagged","type":"binary","instructions":"Check command."}]}}`,
			SystemOneRefs: []string{"ts-a9-cmd"},
			OnUncertain:   store.MessagePolicyOutcomeAdmit,
			OnFailed:      store.MessagePolicyOutcomeAdmit,
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, s1Pol))

		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "Please approve this command.",
			CommandRequest: &messages.CommandRequestInput{
				Key:                  "EXCLUDED_CMD_REQ_KEY",
				Description:          "CMD_RATIONALE_TOKEN",
				Workdir:              "/workspace/cmd-dir",
				Script:               "echo CMD_SCRIPT_TOKEN",
				TimeoutS:             777,
				ShareOutputRequested: true,
			},
		})
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
		assert.Contains(t, rr.Body.String(), "Matched command request prompt text via CEL.")

		mu.Lock()
		require.Len(t, providerPayloads, 1)
		payload := providerPayloads[0]
		mu.Unlock()
		assert.Contains(t, payload, "CMD_RATIONALE_TOKEN")
		assert.Contains(t, payload, "CMD_SCRIPT_TOKEN")
		assert.Contains(t, payload, "/workspace/cmd-dir")
		assert.NotContains(t, payload, "EXCLUDED_CMD_REQ_KEY")
		assert.NotContains(t, payload, "777")
	})

	t.Run("no_content_disclosure_in_logs_and_decision_rows", func(t *testing.T) {
		ctx := context.Background()
		f := newMpgFixture(t, true)

		const sentinel = "PROMPT_SENTINEL_NEVER_LOG_987654321"
		var logBuf bytes.Buffer
		var logMu sync.Mutex
		captureLogger := slog.New(slog.NewJSONHandler(&sliceALockedWriter{mu: &logMu, w: &logBuf}, &slog.HandlerOptions{Level: slog.LevelDebug}))
		f.srv.messageLog = captureLogger
		prevDefault := slog.Default()
		slog.SetDefault(captureLogger)
		t.Cleanup(func() { slog.SetDefault(prevDefault) })

		mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-a9-nolog", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"model": "jev-1.14.0",
				"answers": {"flagged": {"type": "noul", "noul": 0.95}},
				"usage": {"input_tokens": 20, "output_tokens": 1}
			}`))
		})
		// System One runtime defaults its Logger to slog.Default(), which is
		// already captured above before mpgEnableSystemOne constructs the runtime.
		sliceAEnableAllExperiments(t, f.srv)

		enforcePol := &store.MessagePolicy{
			ProjectID:     f.project.ID,
			Name:          "a9-nolog-enforce",
			Enabled:       true,
			Mode:          store.MessagePolicyModeEnforce,
			Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule: `judgment_flags.flagged`,
			SystemOneRule: `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"binding":{"instance":"ts-a9-nolog"},"questions":[{"id":"flagged","type":"binary","instructions":"Check."}]}}`,
			SystemOneRefs: []string{"ts-a9-nolog"},
			OnUncertain:   store.MessagePolicyOutcomeReject,
			OnFailed:      store.MessagePolicyOutcomeReject,
			RejectReason:  "Policy {policy} blocked message.",
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, enforcePol))

		shadowPol := &store.MessagePolicy{
			ProjectID:     f.project.ID,
			Name:          "a9-nolog-shadow",
			Enabled:       true,
			Mode:          store.MessagePolicyModeShadow,
			Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
			ViolationRule: `message.body.contains("` + sentinel + `")`,
			OnFailed:      store.MessagePolicyOutcomeAdmit,
		}
		require.NoError(t, f.s.CreateMessagePolicy(ctx, shadowPol))

		rr := f.sendOutbound(t, OutboundMessageRequest{
			Recipient: "user:" + f.alice.Email,
			Msg:       "Intro line.",
			CommandRequest: &messages.CommandRequestInput{
				Key:         "nolog-cmd",
				Description: "Rationale with " + sentinel,
				Script:      "echo " + sentinel,
			},
		})
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
		assert.NotContains(t, rr.Body.String(), sentinel, "422 response must not leak prompt text")

		decisions := mpgWaitForDecisionCount(t, f.s, f.project.ID, 2)
		decJSON, err := json.Marshal(decisions)
		require.NoError(t, err)
		assert.NotContains(t, string(decJSON), sentinel, "decision rows must never contain prompt text")

		logMu.Lock()
		logs := logBuf.String()
		logMu.Unlock()
		assert.NotEmpty(t, logs, "expected debug/info logs to be captured")
		assert.NotContains(t, logs, sentinel, "logs must never contain prompt text or raw script")
	})
}

type sliceALockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *sliceALockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestOversize_Composite (Slice A acceptance test A10) verifies oversize
// behaviour when intro + prompt text exceeds MaxBodyBytes (16 KiB):
//   - System One on_oversize="truncate" truncates on a UTF-8 boundary with
//     accurate truncation metadata;
//   - System One on_oversize="reject" yields input_too_large;
//   - a System One rule not selecting message.body is unaffected;
//   - a CEL-only policy yields body_truncated.
func TestOversize_Composite(t *testing.T) {
	ctx := context.Background()
	f := newMpgFixture(t, true)

	var providerCalls atomic.Int32
	mpgEnableSystemOne(t, f.srv, f.s, f.project.ID, "ts-a10", func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "jev-1.14.0",
			"answers": {"ok": {"type": "noul", "noul": 0.1}},
			"usage": {"input_tokens": 20, "output_tokens": 1}
		}`))
	})
	sliceAEnableAllExperiments(t, f.srv)

	// 1. System One on_oversize="truncate"
	truncPol := &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "a10-s1-truncate",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: `judgment_flags.ok`,
		SystemOneRule: `{"dialect":"system_one","inputs":["message.body"],"on_oversize":"truncate","spec":{"version":1,"binding":{"instance":"ts-a10"},"questions":[{"id":"ok","type":"binary","instructions":"Check."}]}}`,
		SystemOneRefs: []string{"ts-a10"},
		OnUncertain:   store.MessagePolicyOutcomeAdmit,
		OnFailed:      store.MessagePolicyOutcomeAdmit,
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, truncPol))

	// 2. System One on_oversize="reject"
	rejectPol := &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "a10-s1-reject",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: `judgment_flags.ok`,
		SystemOneRule: `{"dialect":"system_one","inputs":["message.body"],"on_oversize":"reject","spec":{"version":1,"binding":{"instance":"ts-a10"},"questions":[{"id":"ok","type":"binary","instructions":"Check."}]}}`,
		SystemOneRefs: []string{"ts-a10"},
		OnUncertain:   store.MessagePolicyOutcomeAdmit,
		OnFailed:      store.MessagePolicyOutcomeAdmit,
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, rejectPol))

	// 3. System One rule NOT selecting message.body (selects message.type only)
	noBodyPol := &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "a10-s1-nobody",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: `judgment_flags.ok`,
		SystemOneRule: `{"dialect":"system_one","inputs":["message.type"],"on_oversize":"reject","spec":{"version":1,"binding":{"instance":"ts-a10"},"questions":[{"id":"ok","type":"binary","instructions":"Check."}]}}`,
		SystemOneRefs: []string{"ts-a10"},
		OnUncertain:   store.MessagePolicyOutcomeAdmit,
		OnFailed:      store.MessagePolicyOutcomeAdmit,
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, noBodyPol))

	// 4. CEL-only policy (admit on failure so we observe all 4 decision rows)
	celPol := &store.MessagePolicy{
		ProjectID:     f.project.ID,
		Name:          "a10-cel-only",
		Enabled:       true,
		Mode:          store.MessagePolicyModeEnforce,
		Scope:         store.MessagePolicyScope{Surfaces: []string{store.MessagePolicySurfaceAgentToUser}},
		ViolationRule: `message.body.contains("nope")`,
		OnFailed:      store.MessagePolicyOutcomeAdmit,
	}
	require.NoError(t, f.s.CreateMessagePolicy(ctx, celPol))

	// Build a decision prompt with a short intro (< 100 bytes) whose composite
	// message.body exceeds MaxBodyBytes (16 KiB) while staying under
	// MaxDecisionsPayloadBytes (32 KiB). Include multi-byte UTF-8 runes near
	// the boundary to verify clean UTF-8 truncation.
	decisions := make([]messages.DecisionInput, 10)
	longQ := strings.Repeat("é", 900) // 900 runes = 1800 bytes each -> 10 * 1800 = 18,000+ bytes > 16 KiB
	for i := range decisions {
		decisions[i] = messages.DecisionInput{
			Question: longQ,
			Choices:  []messages.ChoiceInput{{Label: "Yes"}},
		}
	}

	rr := f.sendOutbound(t, OutboundMessageRequest{
		Recipient: "user:" + f.alice.Email,
		Msg:       "Short intro.",
		Decisions: decisions,
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	rows := mpgWaitForDecisionCount(t, f.s, f.project.ID, 4)
	byName := make(map[string]store.MessagePolicyDecision, len(rows))
	for _, row := range rows {
		byName[row.PolicyName] = row
	}

	// 1. System One truncate: succeeds with Truncated=true metadata.
	truncRow := byName["a10-s1-truncate"]
	assert.Equal(t, store.MessagePolicyClassPass, truncRow.Class)
	var truncProj messagepolicy.EvaluationProjection
	require.NoError(t, json.Unmarshal(truncRow.Evaluation, &truncProj))
	assert.True(t, truncProj.Truncated, "truncate rule must record Truncated=true")
	require.Len(t, truncProj.InputFields, 1)
	assert.Equal(t, messagepolicy.FieldMessageBody, truncProj.InputFields[0].Name)
	assert.True(t, truncProj.InputFields[0].Truncated)

	// 2. System One reject: fails with input_too_large.
	rejectRow := byName["a10-s1-reject"]
	assert.Equal(t, store.MessagePolicyClassFailed, rejectRow.Class)
	assert.Equal(t, "input_too_large", rejectRow.Reason)

	// 3. System One rule not selecting message.body: unaffected, passes with Truncated=false.
	noBodyRow := byName["a10-s1-nobody"]
	assert.Equal(t, store.MessagePolicyClassPass, noBodyRow.Class)
	var noBodyProj messagepolicy.EvaluationProjection
	require.NoError(t, json.Unmarshal(noBodyRow.Evaluation, &noBodyProj))
	assert.False(t, noBodyProj.Truncated, "rule not selecting message.body must not be truncated")

	// 4. CEL-only policy: fails with body_truncated.
	celRow := byName["a10-cel-only"]
	assert.Equal(t, store.MessagePolicyClassFailed, celRow.Class)
	assert.Equal(t, messagepolicy.ReasonBodyTruncated, celRow.Reason)

	// Provider should have been called twice (truncate + nobody), not for reject or CEL.
	assert.Equal(t, int32(2), providerCalls.Load())
}
