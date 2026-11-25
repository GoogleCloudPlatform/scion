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

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const decisionsTestProject = "project-decisions"

const twoTestDecisions = `[{"id":"D234","question":"Ship the **feature** now?","choices":["yes","no"]},{"question":"Which approach?","choices":["a","b"]}]`

// decisionsState isolates the message flags and the ask_user fake for one
// test, in agent mode with sender as the agent. It returns the summaries
// the fake ask_user recorded.
func decisionsState(t *testing.T, sender string) *[]string {
	t.Helper()
	orig := saveMessageTestState()
	restoreFlags := resetMessageFlags()
	origFormat, origAsk, origStdin := outputFormat, runDecisionsAskUser, decisionsStdin
	outputFormat = ""
	var asked []string
	runDecisionsAskUser = func(summary string) { asked = append(asked, summary) }
	t.Cleanup(func() {
		restoreFlags()
		orig.restore()
		outputFormat, runDecisionsAskUser, decisionsStdin = origFormat, origAsk, origStdin
	})
	t.Setenv("SCION_AGENT_NAME", sender)
	t.Setenv("SCION_CLI_MODE", string(ModeAgent))
	return &asked
}

// newDecisionsHub is a fake hub whose outbound-message endpoint records the
// request body and answers with status and body.
func newDecisionsHub(t *testing.T, status int, body map[string]interface{}, got *map[string]interface{}) *HubContext {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/outbound-message"):
			if got != nil {
				_ = json.NewDecoder(r.Body).Decode(got)
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL, ProjectID: decisionsTestProject}
}

func decisionsSentBody() map[string]interface{} {
	return map[string]interface{}{
		"message_id": "msg-decisions-1", "status": "sent",
		"recipient": "user:alice@example.com", "recipient_id": "uid-alice",
		"decisions": []map[string]interface{}{
			{"key": "D234", "version": 1, "ref": "agent-uuid/D234", "replaced": nil},
			{"key": "D1", "version": 2, "ref": "agent-uuid/D1", "replaced": map[string]interface{}{"version": 1, "status": "pending"}},
		},
	}
}

func TestValidateDecisionFlags(t *testing.T) {
	cases := []struct {
		name      string
		setup     func()
		agent     bool
		group     bool
		noAgent   bool
		human     bool
		wantErr   string
		wantUsage bool
	}{
		{name: "no decisions", setup: func() {}},
		{name: "decisions to a user", setup: func() { msgDecisions = "[]" }},
		{name: "both decision flags", setup: func() { msgDecisions = "[]"; msgDecisionsFile = "q.json" }, wantErr: "mutually exclusive", wantUsage: true},
		{name: "update without decisions", setup: func() { msgDecisionsUpdate = true }, wantErr: "--update is valid only with --decisions, --decisions-file, --secret-request, or --secret-request-file", wantUsage: true},
		{name: "two readers of stdin", setup: func() { msgDecisionsFile = "-"; msgBodyFile = "-" }, wantErr: "both read stdin", wantUsage: true},
		{name: "agent recipient", setup: func() { msgDecisions = "[]" }, agent: true, wantErr: "not to an agent", wantUsage: false},
		{name: "group recipient", setup: func() { msgDecisions = "[]" }, group: true, wantErr: "not to an agent or a group", wantUsage: false},
		{name: "attachments", setup: func() { msgDecisions = "[]"; msgAttach = []string{"f"} }, wantErr: "--attach", wantUsage: true},
		{name: "artifact", setup: func() { msgDecisions = "[]"; msgArtifacts = []string{"scion://artifact/a"} }, wantErr: "--artifact", wantUsage: true},
		{name: "wake", setup: func() { msgDecisions = "[]"; msgWake = true }, wantErr: "--wake", wantUsage: true},
		{name: "not an agent", setup: func() { msgDecisions = "[]" }, noAgent: true, wantErr: "only by an agent", wantUsage: false},
		{name: "agent name in human mode", setup: func() { msgDecisions = "[]" }, human: true, wantErr: "only by an agent", wantUsage: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := "asker"
			if tc.noAgent {
				sender = ""
			}
			decisionsState(t, sender)
			if tc.human {
				t.Setenv("SCION_CLI_MODE", string(ModeHuman))
			}
			origBody := msgBodyFile
			t.Cleanup(func() { msgBodyFile = origBody })
			tc.setup()
			err := validateDecisionFlags(tc.agent, tc.group)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Equal(t, tc.wantUsage, isUsageError(err), "usage error mismatch for %q", err)
		})
	}
}

func TestLoadDecisionsPayload(t *testing.T) {
	decisionsState(t, "asker")

	msgDecisions = "not json"
	_, err := loadDecisionsPayload()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JSON array")
	assert.False(t, isUsageError(err))

	msgDecisions = "[]"
	_, err = loadDecisionsPayload()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one decision")
	assert.False(t, isUsageError(err))

	// Only the JSON shape is checked: an invalid decision is the hub's to
	// refuse.
	msgDecisions = `[{"question":"q","choices":["only one"]}]`
	got, err := loadDecisionsPayload()
	require.NoError(t, err)
	assert.JSONEq(t, msgDecisions, string(got))

	msgDecisions = ""
	path := filepath.Join(t.TempDir(), "q.json")
	require.NoError(t, os.WriteFile(path, []byte("  "+twoTestDecisions+"\n"), 0o600))
	msgDecisionsFile = path
	got, err = loadDecisionsPayload()
	require.NoError(t, err)
	assert.JSONEq(t, twoTestDecisions, string(got))

	msgDecisionsFile = "-"
	decisionsStdin = strings.NewReader(twoTestDecisions)
	got, err = loadDecisionsPayload()
	require.NoError(t, err)
	assert.JSONEq(t, twoTestDecisions, string(got))
}

func TestDecisionsSummary(t *testing.T) {
	assert.Equal(t, "Decision needed: Ship the feature now? (+1 more)", decisionsSummary(json.RawMessage(twoTestDecisions)))
	assert.Equal(t, "Decision needed: Read the doc now",
		decisionsSummary(json.RawMessage(`[{"question":"Read [the doc](https://x.example)\n`+"`now`"+`","choices":["a","b"]}]`)))
	long := strings.Repeat("x", 300)
	s := decisionsSummary(json.RawMessage(`[{"question":"` + long + `","choices":["a","b"]}]`))
	assert.Equal(t, 120, len([]rune(strings.TrimPrefix(s, "Decision needed: "))))
	assert.True(t, strings.HasSuffix(s, "…"))

	// Markers go only where they mark: inside a word, or not followed by a
	// space at a line start, they are text.
	for q, want := range map[string]string{
		"Deploy ship_now to PR #12?":          "Deploy ship_now to PR #12?",
		"# Release\nShip **v2** _today_?":     "Release Ship v2 today?",
		"> quoted `code` and ~~struck~~ text": "quoted code and struck text",
		"Use snake_case_name (or **bold**).":  "Use snake_case_name (or bold).",
	} {
		raw, err := json.Marshal([]map[string]interface{}{{"question": q, "choices": []string{"a", "b"}}})
		require.NoError(t, err)
		assert.Equal(t, "Decision needed: "+want, decisionsSummary(raw), q)
	}
}

func TestDecisionSendLine(t *testing.T) {
	assert.Equal(t, "D234 v1 (new)", decisionSendLine(hubclient.DecisionSendResult{Key: "D234", Version: 1}))
	assert.Equal(t, "D1 v2 (new, replaces v1 which was pending)", decisionSendLine(hubclient.DecisionSendResult{
		Key: "D1", Version: 2, Replaced: &hubclient.DecisionReplaced{Version: 1, Status: "pending"}}))
	assert.Equal(t, "D1 v3 (new, replaces v2 which was decided: yes)", decisionSendLine(hubclient.DecisionSendResult{
		Key: "D1", Version: 3, Replaced: &hubclient.DecisionReplaced{Version: 2, Status: "decided", Answer: "yes"}}))
	assert.Equal(t, "D1 v3 (new, replaces v2 which was decided by Pat: yes)", decisionSendLine(hubclient.DecisionSendResult{
		Key: "D1", Version: 3, Replaced: &hubclient.DecisionReplaced{Version: 2, Status: "decided", Answer: "yes", AnsweredBy: "Pat"}}))
}

// A decision prompt to a user sends the decisions and --update, prints the
// message id and one line per decision, and then marks the agent waiting
// for input with the summary.
func TestSendOutboundDecisions_UserRecipient(t *testing.T) {
	asked := decisionsState(t, "asker")
	msgDecisionsPayload = json.RawMessage(twoTestDecisions)
	msgDecisionsUpdate = true
	var got map[string]interface{}
	hubCtx := newDecisionsHub(t, http.StatusOK, decisionsSentBody(), &got)

	var err error
	out := captureStdout(t, func() { err = sendOutboundMessageViaHub(hubCtx, "user:alice@example.com", "", false) })
	require.NoError(t, err)

	assert.Equal(t, true, got["decisions_update"])
	assert.NotContains(t, got, "type", "the hub sets a decision prompt's type")
	raw, _ := json.Marshal(got["decisions"])
	assert.JSONEq(t, twoTestDecisions, string(raw))

	assert.Contains(t, out, "Message sent to user:alice@example.com via Hub (message msg-decisions-1).")
	assert.Contains(t, out, "D234 v1 (new)\n")
	assert.Equal(t, "D1 v2 (new, replaces v1 which was pending)", lastLine(out))
	assert.Equal(t, []string{"Decision needed: Ship the feature now? (+1 more)"}, *asked)
}

// A conv: send carries the decisions too; --json prints the hub's result
// (message id and decisions) and still runs the side effect.
func TestSendOutboundDecisions_ConversationJSON(t *testing.T) {
	asked := decisionsState(t, "asker")
	outputFormat = "json"
	msgDecisionsPayload = json.RawMessage(twoTestDecisions)
	var got map[string]interface{}
	hubCtx := newDecisionsHub(t, http.StatusOK, decisionsSentBody(), &got)

	ref := &messaging.Reference{Kind: messaging.RefConversation, Value: "11111111-1111-1111-1111-111111111111", Raw: "conv:11111111-1111-1111-1111-111111111111"}
	var err error
	out := captureStdout(t, func() { err = sendMessageViaConversation(hubCtx, ref, "Two calls", false, false, nil) })
	require.NoError(t, err)
	assert.Equal(t, "Two calls", got["msg"])
	assert.Contains(t, got, "decisions")

	var res hubclient.OutboundMessageResult
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "msg-decisions-1", res.MessageID)
	require.Len(t, res.Decisions, 2)
	assert.Equal(t, "D234", res.Decisions[0].Key)
	assert.Len(t, *asked, 1)
}

// The decisions are printed whatever the send status (here: deferred).
func TestSendOutboundDecisions_NonSentStatusPrintsDecisions(t *testing.T) {
	decisionsState(t, "asker")
	msgDecisionsPayload = json.RawMessage(twoTestDecisions)
	body := decisionsSentBody()
	body["status"] = "deferred"
	hubCtx := newDecisionsHub(t, http.StatusOK, body, nil)

	ref := &messaging.Reference{Kind: messaging.RefConversation, Value: "11111111-1111-1111-1111-111111111111", Raw: "conv:11111111-1111-1111-1111-111111111111"}
	var err error
	out := captureStdout(t, func() { err = sendMessageViaConversation(hubCtx, ref, "Two calls", false, false, nil) })
	require.NoError(t, err)
	assert.Contains(t, out, "saved to history")
	assert.Contains(t, out, "D234 v1 (new)\n")
	assert.Contains(t, out, "D1 v2 (new, replaces v1 which was pending)\n")
}

// A refused send (400 experiment_disabled) fails and leaves the agent's
// activity alone.
func TestSendOutboundDecisions_ExperimentDisabledSkipsAskUser(t *testing.T) {
	asked := decisionsState(t, "asker")
	msgDecisionsPayload = json.RawMessage(twoTestDecisions)
	hubCtx := newDecisionsHub(t, http.StatusBadRequest, map[string]interface{}{
		"error": map[string]interface{}{"code": "experiment_disabled", "message": "experiment_disabled: web.decisions is off"},
	}, nil)

	var err error
	_ = captureStdout(t, func() { err = sendOutboundMessageViaHub(hubCtx, "user:alice@example.com", "", false) })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "web.decisions")
	assert.Empty(t, *asked)
}

// Outside agent mode the side effect does not run.
func TestSendOutboundDecisions_HumanModeNoAskUser(t *testing.T) {
	asked := decisionsState(t, "asker")
	t.Setenv("SCION_CLI_MODE", string(ModeHuman))
	msgDecisionsPayload = json.RawMessage(twoTestDecisions)
	hubCtx := newDecisionsHub(t, http.StatusOK, decisionsSentBody(), nil)
	var err error
	_ = captureStdout(t, func() { err = sendOutboundMessageViaHub(hubCtx, "user:alice@example.com", "", false) })
	require.NoError(t, err)
	assert.Empty(t, *asked)
}

// The command refuses an agent recipient before anything is sent, and a
// body is optional with --decisions.
func TestMessageCommand_DecisionsArgs(t *testing.T) {
	decisionsState(t, "asker")
	require.NoError(t, messageCmd.Flags().Set("decisions", twoTestDecisions))
	assert.NoError(t, messageCmd.Args(messageCmd, []string{"user:alice@example.com"}), "body is optional with --decisions")

	err := messageCmd.RunE(messageCmd, []string{"agent:builder"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not to an agent")
	assert.False(t, isUsageError(err))
}

func TestMessageCommand_UpdateHelpDocumentsFullSet(t *testing.T) {
	flag := messageCmd.Flags().Lookup("update")
	require.NotNil(t, flag)
	assert.Contains(t, flag.Usage, "--decisions")
	assert.Contains(t, flag.Usage, "still-pending keys")
	assert.Contains(t, flag.Usage, "scion decisions withdraw")
	assert.Contains(t, messageCmd.Long, "Decision prompts (--decisions or --decisions-file")
	assert.Contains(t, messageCmd.Long, "still-pending keys")
	assert.Contains(t, messageCmd.Long, "To retire a decision without a replacement")
	assert.Contains(t, messageCmd.Long, "withdraw")
}

// TestMessageCommand_DecisionsFlagsAndRemovedChoicesAliases verifies that
// --decisions and --decisions-file are registered and visible, while the
// legacy --choices and --choices-file aliases have been removed (ptone/scion#8249).
func TestMessageCommand_DecisionsFlagsAndRemovedChoicesAliases(t *testing.T) {
	for _, removed := range []string{"choices", "choices-file"} {
		assert.Nil(t, messageCmd.Flags().Lookup(removed), "--%s legacy alias must be removed", removed)
		assert.NotContains(t, messageCmd.Flags().FlagUsages(), "--"+removed)
	}
	assert.Contains(t, messageCmd.Flags().FlagUsages(), "--decisions ")
	assert.Contains(t, messageCmd.Flags().FlagUsages(), "--decisions-file ")

	// End-to-end execution through rootCmd: --decisions-file and --format json (ptone/scion#8156).
	t.Run("execute_decisions_file_and_format_json", func(t *testing.T) {
		asked := decisionsState(t, "asker")
		var got map[string]interface{}
		hubCtx := newDecisionsHub(t, http.StatusOK, decisionsSentBody(), &got)

		t.Setenv("HOME", t.TempDir())
		t.Setenv("SCION_HUB_ENDPOINT", hubCtx.Endpoint)
		t.Setenv("SCION_HUB_TOKEN", "test-agent-token")
		t.Setenv("SCION_PROJECT_ID", decisionsTestProject)
		t.Cleanup(func() {
			rootCmd.SetArgs(nil)
			if f := rootCmd.PersistentFlags().Lookup("format"); f != nil {
				_ = f.Value.Set(f.DefValue)
				f.Changed = false
			}
		})

		path := filepath.Join(t.TempDir(), "decisions.json")
		require.NoError(t, os.WriteFile(path, []byte(twoTestDecisions), 0o600))

		var execErr error
		stdout, stderr := captureStdoutStderr(t, func() {
			rootCmd.SetArgs([]string{"message", "user:alice@example.com", "--decisions-file", path, "--format", "json"})
			execErr = rootCmd.Execute()
		})
		require.NoError(t, execErr)
		assert.Empty(t, stderr)

		var res hubclient.OutboundMessageResult
		require.NoError(t, json.Unmarshal([]byte(stdout), &res), "stdout must be valid JSON: %s", stdout)
		assert.Equal(t, "msg-decisions-1", res.MessageID)
		require.Len(t, res.Decisions, 2)
		assert.Equal(t, "D234", res.Decisions[0].Key)
		assert.Equal(t, []string{"Decision needed: Ship the feature now? (+1 more)"}, *asked)
	})

	t.Run("removed_choices_flags_are_rejected_as_unknown", func(t *testing.T) {
		for _, tc := range []struct {
			flag string
			val  string
		}{
			{"--choices", twoTestDecisions},
			{"--choices-file", "decisions.json"},
		} {
			t.Run(tc.flag, func(t *testing.T) {
				decisionsState(t, "asker")
				t.Cleanup(func() { rootCmd.SetArgs(nil) })
				var execErr error
				_, _ = captureStdoutStderr(t, func() {
					rootCmd.SetArgs([]string{"message", "user:alice@example.com", tc.flag, tc.val})
					execErr = rootCmd.Execute()
				})
				require.Error(t, execErr)
				assert.Contains(t, execErr.Error(), "unknown flag: "+tc.flag)
			})
		}
	})
}

func TestDecisionLengthWarnings(t *testing.T) {
	t.Run("at_or_below_soft_limits_produces_no_warnings", func(t *testing.T) {
		// Exact boundary using multi-byte runes (UTF-8 rune count, not bytes).
		qExact := strings.Repeat("é", decisionQuestionSoftMax)
		lExact := strings.Repeat("é", decisionChoiceLabelSoftMax)
		raw, err := json.Marshal([]map[string]interface{}{
			{
				"id":       "deploy-window",
				"question": "  " + qExact + "  ",
				"choices": []interface{}{
					lExact,
					map[string]string{"id": "hold", "label": "  " + lExact + "  ", "description": strings.Repeat("d", 200)},
				},
			},
		})
		require.NoError(t, err)
		assert.Empty(t, decisionLengthWarnings(raw))
	})

	t.Run("long_question_and_labels_warn_and_point_to_message_body", func(t *testing.T) {
		qLong := strings.Repeat("q", decisionQuestionSoftMax+1)
		lLong := strings.Repeat("l", decisionChoiceLabelSoftMax+1)
		raw, err := json.Marshal([]map[string]interface{}{
			{
				"id":       "deploy-window",
				"question": qLong,
				"choices":  []interface{}{lLong, "Hold"},
			},
			{
				"question": "Short question?",
				"choices": []interface{}{
					"Yes",
					map[string]string{"id": "no", "label": lLong},
				},
			},
		})
		require.NoError(t, err)

		got := decisionLengthWarnings(raw)
		require.Len(t, got, 3)
		assert.Contains(t, got[0], "Warning: decision deploy-window question is 201 characters (soft limit 200)")
		assert.Contains(t, got[0], "message body")
		assert.Contains(t, got[1], "Warning: decision deploy-window choice 1 label is 41 characters (soft limit 40)")
		assert.Contains(t, got[1], "message body")
		assert.Contains(t, got[2], "Warning: decision #2 choice 2 label is 41 characters (soft limit 40)")
		assert.Contains(t, got[2], "message body")
	})

	t.Run("malformed_payload_returns_no_warnings", func(t *testing.T) {
		assert.Empty(t, decisionLengthWarnings(json.RawMessage(`["not-an-object"]`)))
		assert.Empty(t, decisionLengthWarnings(nil))
	})
}

func TestMessageCommand_DecisionsSoftLengthWarningDoesNotBlockSend(t *testing.T) {
	asked := decisionsState(t, "asker")
	var got map[string]interface{}
	hubCtx := newDecisionsHub(t, http.StatusOK, decisionsSentBody(), &got)

	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_HUB_ENDPOINT", hubCtx.Endpoint)
	t.Setenv("SCION_HUB_TOKEN", "test-agent-token")
	t.Setenv("SCION_PROJECT_ID", decisionsTestProject)
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		if f := rootCmd.PersistentFlags().Lookup("format"); f != nil {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	})

	payload, err := json.Marshal([]map[string]interface{}{
		{
			"id":       "D234",
			"question": strings.Repeat("x", decisionQuestionSoftMax+5),
			"choices":  []string{strings.Repeat("y", decisionChoiceLabelSoftMax+3), "Hold"},
		},
	})
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "long-decisions.json")
	require.NoError(t, os.WriteFile(path, payload, 0o600))

	var execErr error
	stdout, stderr := captureStdoutStderr(t, func() {
		rootCmd.SetArgs([]string{"message", "user:alice@example.com", "--decisions-file", path, "--format", "json"})
		execErr = rootCmd.Execute()
	})
	require.NoError(t, execErr, "soft length warning must not block the send")
	assert.Contains(t, stderr, "Warning: decision D234 question is 205 characters (soft limit 200)")
	assert.Contains(t, stderr, "Warning: decision D234 choice 1 label is 43 characters (soft limit 40)")
	assert.Contains(t, stderr, "message body")

	var res hubclient.OutboundMessageResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &res), "stdout must stay pure JSON when soft length warnings go to stderr: %s", stdout)
	assert.Equal(t, "msg-decisions-1", res.MessageID)
	assert.Len(t, *asked, 1)
}

func TestMessageCommand_DecisionsHelpDocumentsSoftLimits(t *testing.T) {
	assert.Contains(t, messageCmd.Long, "200 characters")
	assert.Contains(t, messageCmd.Long, "40 characters")
	assert.Contains(t, messageCmd.Long, "message body")
}
