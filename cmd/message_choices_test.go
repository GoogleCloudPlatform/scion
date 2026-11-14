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

const choicesTestProject = "project-choices"

const choicesTwoDecisions = `[{"id":"D234","question":"Ship the **feature** now?","choices":["yes","no"]},{"question":"Which approach?","choices":["a","b"]}]`

// choicesState isolates the message flags and the ask_user fake for one
// test, in agent mode with sender as the agent. It returns the summaries
// the fake ask_user recorded.
func choicesState(t *testing.T, sender string) *[]string {
	t.Helper()
	orig := saveMessageTestState()
	restoreFlags := resetMessageFlags()
	origFormat, origAsk, origStdin := outputFormat, runChoicesAskUser, choicesStdin
	outputFormat = ""
	var asked []string
	runChoicesAskUser = func(summary string) { asked = append(asked, summary) }
	t.Cleanup(func() {
		restoreFlags()
		orig.restore()
		outputFormat, runChoicesAskUser, choicesStdin = origFormat, origAsk, origStdin
	})
	t.Setenv("SCION_AGENT_NAME", sender)
	t.Setenv("SCION_CLI_MODE", string(ModeAgent))
	return &asked
}

// newChoicesHub is a fake hub whose outbound-message endpoint records the
// request body and answers with status and body.
func newChoicesHub(t *testing.T, status int, body map[string]interface{}, got *map[string]interface{}) *HubContext {
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
	return &HubContext{Client: client, Endpoint: srv.URL, ProjectID: choicesTestProject}
}

func choicesSentBody() map[string]interface{} {
	return map[string]interface{}{
		"message_id": "msg-choices-1", "status": "sent",
		"recipient": "user:alice@example.com", "recipient_id": "uid-alice",
		"decisions": []map[string]interface{}{
			{"key": "D234", "version": 1, "ref": "agent-uuid/D234", "replaced": nil},
			{"key": "D1", "version": 2, "ref": "agent-uuid/D1", "replaced": map[string]interface{}{"version": 1, "status": "pending"}},
		},
	}
}

func TestValidateChoiceFlags(t *testing.T) {
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
		{name: "no choices", setup: func() {}},
		{name: "choices to a user", setup: func() { msgChoices = "[]" }},
		{name: "both choice flags", setup: func() { msgChoices = "[]"; msgChoicesFile = "q.json" }, wantErr: "mutually exclusive", wantUsage: true},
		{name: "update without choices", setup: func() { msgChoicesUpdate = true }, wantErr: "--update is valid only", wantUsage: true},
		{name: "two readers of stdin", setup: func() { msgChoicesFile = "-"; msgBodyFile = "-" }, wantErr: "both read stdin", wantUsage: true},
		{name: "agent recipient", setup: func() { msgChoices = "[]" }, agent: true, wantErr: "not to an agent", wantUsage: false},
		{name: "group recipient", setup: func() { msgChoices = "[]" }, group: true, wantErr: "not to an agent or a group", wantUsage: false},
		{name: "scheduled", setup: func() { msgChoices = "[]"; msgIn = "5m" }, wantErr: "--in or --at", wantUsage: true},
		{name: "attachments", setup: func() { msgChoices = "[]"; msgAttach = []string{"f"} }, wantErr: "--attach", wantUsage: true},
		{name: "artifact", setup: func() { msgChoices = "[]"; msgArtifacts = []string{"scion://artifact/a"} }, wantErr: "--artifact", wantUsage: true},
		{name: "wake", setup: func() { msgChoices = "[]"; msgWake = true }, wantErr: "--wake", wantUsage: true},
		{name: "cc", setup: func() { msgChoices = "[]"; msgCC = []string{"agent:x"} }, wantErr: "--cc", wantUsage: true},
		{name: "channel", setup: func() { msgChoices = "[]"; msgChannel = "web" }, wantErr: "--channel", wantUsage: true},
		{name: "thread id", setup: func() { msgChoices = "[]"; msgThreadID = "t1" }, wantErr: "--thread-id", wantUsage: true},
		{name: "not an agent", setup: func() { msgChoices = "[]" }, noAgent: true, wantErr: "only by an agent", wantUsage: false},
		{name: "agent name in human mode", setup: func() { msgChoices = "[]" }, human: true, wantErr: "only by an agent", wantUsage: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := "asker"
			if tc.noAgent {
				sender = ""
			}
			choicesState(t, sender)
			if tc.human {
				t.Setenv("SCION_CLI_MODE", string(ModeHuman))
			}
			origBody := msgBodyFile
			t.Cleanup(func() { msgBodyFile = origBody })
			tc.setup()
			err := validateChoiceFlags(tc.agent, tc.group)
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

func TestLoadChoicesPayload(t *testing.T) {
	choicesState(t, "asker")

	msgChoices = "not json"
	_, err := loadChoicesPayload()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JSON array")
	assert.False(t, isUsageError(err))

	msgChoices = "[]"
	_, err = loadChoicesPayload()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one decision")
	assert.False(t, isUsageError(err))

	// Only the JSON shape is checked: an invalid decision is the hub's to
	// refuse.
	msgChoices = `[{"question":"q","choices":["only one"]}]`
	got, err := loadChoicesPayload()
	require.NoError(t, err)
	assert.JSONEq(t, msgChoices, string(got))

	msgChoices = ""
	path := filepath.Join(t.TempDir(), "q.json")
	require.NoError(t, os.WriteFile(path, []byte("  "+choicesTwoDecisions+"\n"), 0o600))
	msgChoicesFile = path
	got, err = loadChoicesPayload()
	require.NoError(t, err)
	assert.JSONEq(t, choicesTwoDecisions, string(got))

	msgChoicesFile = "-"
	choicesStdin = strings.NewReader(choicesTwoDecisions)
	got, err = loadChoicesPayload()
	require.NoError(t, err)
	assert.JSONEq(t, choicesTwoDecisions, string(got))
}

func TestChoicesSummary(t *testing.T) {
	assert.Equal(t, "Decision needed: Ship the feature now? (+1 more)", choicesSummary(json.RawMessage(choicesTwoDecisions)))
	assert.Equal(t, "Decision needed: Read the doc now",
		choicesSummary(json.RawMessage(`[{"question":"Read [the doc](https://x.example)\n`+"`now`"+`","choices":["a","b"]}]`)))
	long := strings.Repeat("x", 300)
	s := choicesSummary(json.RawMessage(`[{"question":"` + long + `","choices":["a","b"]}]`))
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
		assert.Equal(t, "Decision needed: "+want, choicesSummary(raw), q)
	}
}

func TestChoiceDecisionLine(t *testing.T) {
	assert.Equal(t, "D234 v1 (new)", choiceDecisionLine(hubclient.ChoiceDecisionResult{Key: "D234", Version: 1}))
	assert.Equal(t, "D1 v2 (new, replaces v1 which was pending)", choiceDecisionLine(hubclient.ChoiceDecisionResult{
		Key: "D1", Version: 2, Replaced: &hubclient.ChoiceReplacedDecision{Version: 1, Status: "pending"}}))
	assert.Equal(t, "D1 v3 (new, replaces v2 which was decided: yes)", choiceDecisionLine(hubclient.ChoiceDecisionResult{
		Key: "D1", Version: 3, Replaced: &hubclient.ChoiceReplacedDecision{Version: 2, Status: "decided", Answer: "yes"}}))
	assert.Equal(t, "D1 v3 (new, replaces v2 which was decided by Pat: yes)", choiceDecisionLine(hubclient.ChoiceDecisionResult{
		Key: "D1", Version: 3, Replaced: &hubclient.ChoiceReplacedDecision{Version: 2, Status: "decided", Answer: "yes", AnsweredBy: "Pat"}}))
}

// A choice prompt to a user sends the decisions and --update, prints the
// message id and one line per decision, and then marks the agent waiting
// for input with the summary.
func TestSendOutboundChoices_UserRecipient(t *testing.T) {
	asked := choicesState(t, "asker")
	msgChoicesPayload = json.RawMessage(choicesTwoDecisions)
	msgChoicesUpdate = true
	var got map[string]interface{}
	hubCtx := newChoicesHub(t, http.StatusOK, choicesSentBody(), &got)

	var err error
	out := captureStdout(t, func() { err = sendOutboundMessageViaHub(hubCtx, "user:alice@example.com", "", false) })
	require.NoError(t, err)

	assert.Equal(t, true, got["choices_update"])
	assert.NotContains(t, got, "type", "the hub sets a choice prompt's type")
	raw, _ := json.Marshal(got["choices"])
	assert.JSONEq(t, choicesTwoDecisions, string(raw))

	assert.Contains(t, out, "Message sent to user:alice@example.com via Hub (message msg-choices-1).")
	assert.Contains(t, out, "D234 v1 (new)\n")
	assert.Equal(t, "D1 v2 (new, replaces v1 which was pending)", lastLine(out))
	assert.Equal(t, []string{"Decision needed: Ship the feature now? (+1 more)"}, *asked)
}

// A conv: send carries the decisions too; --json prints the hub's result
// (message id and decisions) and still runs the side effect.
func TestSendOutboundChoices_ConversationJSON(t *testing.T) {
	asked := choicesState(t, "asker")
	outputFormat = "json"
	msgChoicesPayload = json.RawMessage(choicesTwoDecisions)
	var got map[string]interface{}
	hubCtx := newChoicesHub(t, http.StatusOK, choicesSentBody(), &got)

	ref := &messaging.Reference{Kind: messaging.RefConversation, Value: "11111111-1111-1111-1111-111111111111", Raw: "conv:11111111-1111-1111-1111-111111111111"}
	var err error
	out := captureStdout(t, func() { err = sendMessageViaConversation(hubCtx, ref, "Two calls", false, false, nil) })
	require.NoError(t, err)
	assert.Equal(t, "Two calls", got["msg"])
	assert.Contains(t, got, "choices")

	var res hubclient.OutboundMessageResult
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "msg-choices-1", res.MessageID)
	require.Len(t, res.Decisions, 2)
	assert.Equal(t, "D234", res.Decisions[0].Key)
	assert.Len(t, *asked, 1)
}

// The decisions are printed whatever the send status (here: deferred).
func TestSendOutboundChoices_NonSentStatusPrintsDecisions(t *testing.T) {
	choicesState(t, "asker")
	msgChoicesPayload = json.RawMessage(choicesTwoDecisions)
	body := choicesSentBody()
	body["status"] = "deferred"
	hubCtx := newChoicesHub(t, http.StatusOK, body, nil)

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
func TestSendOutboundChoices_ExperimentDisabledSkipsAskUser(t *testing.T) {
	asked := choicesState(t, "asker")
	msgChoicesPayload = json.RawMessage(choicesTwoDecisions)
	hubCtx := newChoicesHub(t, http.StatusBadRequest, map[string]interface{}{
		"error": map[string]interface{}{"code": "experiment_disabled", "message": "experiment_disabled: web.chat_choices is off"},
	}, nil)

	var err error
	_ = captureStdout(t, func() { err = sendOutboundMessageViaHub(hubCtx, "user:alice@example.com", "", false) })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "web.chat_choices")
	assert.Empty(t, *asked)
}

// Outside agent mode the side effect does not run.
func TestSendOutboundChoices_HumanModeNoAskUser(t *testing.T) {
	asked := choicesState(t, "asker")
	t.Setenv("SCION_CLI_MODE", string(ModeHuman))
	msgChoicesPayload = json.RawMessage(choicesTwoDecisions)
	hubCtx := newChoicesHub(t, http.StatusOK, choicesSentBody(), nil)
	var err error
	_ = captureStdout(t, func() { err = sendOutboundMessageViaHub(hubCtx, "user:alice@example.com", "", false) })
	require.NoError(t, err)
	assert.Empty(t, *asked)
}

// The command refuses an agent recipient before anything is sent, and a
// body is optional with choices.
func TestMessageCommand_ChoicesArgs(t *testing.T) {
	choicesState(t, "asker")
	require.NoError(t, messageCmd.Flags().Set("choices", choicesTwoDecisions))
	assert.NoError(t, messageCmd.Args(messageCmd, []string{"user:alice@example.com"}), "body is optional with choices")

	err := messageCmd.RunE(messageCmd, []string{"agent:builder"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not to an agent")
	assert.False(t, isUsageError(err))
}

func TestMessageCommand_UpdateHelpDocumentsFullSet(t *testing.T) {
	flag := messageCmd.Flags().Lookup("update")
	require.NotNil(t, flag)
	assert.Contains(t, flag.Usage, "still-pending keys")
	assert.Contains(t, messageCmd.Long, "still-pending keys")
}
