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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecisionsCLIModesAndWindowsGate(t *testing.T) {
	for _, path := range []string{"decisions", "decisions.list", "decisions.show", "decisions.record"} {
		assert.True(t, agentAllowed[path], "agentAllowed[%q]", path)
		assert.True(t, windowsAllowed[path], "windowsAllowed[%q]", path)
	}

	for _, sub := range []string{"list", "show", "record"} {
		_, _, err := rootCmd.Find([]string{"decisions", sub})
		assert.NoError(t, err, "decisions %s is registered on rootCmd", sub)
	}

	for _, mode := range []CLIMode{ModeHuman, ModeAgent} {
		root := &cobra.Command{Use: "scion"}
		dec := newDecisionsCmd()
		root.AddCommand(dec)
		applyModeRestrictions(root, mode)
		names := collectCommandNames(root)
		for _, path := range []string{"decisions", "decisions.list", "decisions.show", "decisions.record"} {
			assert.Contains(t, names, path, "%s in %s mode", path, mode)
		}
		assert.True(t, containsLine(dec.Long, "scion decisions list"))
		assert.True(t, containsLine(dec.Long, "scion decisions show <key>"))
		assert.True(t, containsLine(dec.Long, "scion decisions record"))
	}

	// Verify Windows platform gate leaves decisions and all subcommands runnable (not gated).
	winTree, ran := gateTestTree(t)
	applyPlatformRestrictions(winTree, "windows")
	for _, args := range [][]string{
		{"decisions", "list"},
		{"decisions", "show", "D234"},
		{"decisions", "record", "D234", "1", "--from", "msg-1"},
	} {
		*ran = nil
		winTree.SetArgs(args)
		winTree.SetOut(&bytes.Buffer{})
		winTree.SetErr(&bytes.Buffer{})
		require.NoError(t, winTree.Execute(), "args=%v", args)
		require.Len(t, *ran, 1)
	}
}

func TestParseDecisionsRecordArgs(t *testing.T) {
	t.Run("positional form", func(t *testing.T) {
		got, err := parseDecisionsRecordArgs([]string{"D234", "1"})
		require.NoError(t, err)
		assert.Equal(t, []hubclient.InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}}, got)
	})

	t.Run("key=choice form single and batch", func(t *testing.T) {
		got, err := parseDecisionsRecordArgs([]string{"D234=1", "17=a", "ship-now=yes"})
		require.NoError(t, err)
		assert.Equal(t, []hubclient.InterpretedAnswerInput{
			{Key: "D234", ChoiceID: "1"},
			{Key: "17", ChoiceID: "a"},
			{Key: "ship-now", ChoiceID: "yes"},
		}, got)
	})

	t.Run("rejects mixed forms and invalid shapes", func(t *testing.T) {
		badCases := [][]string{
			{},
			{"D234"},
			{"D234", "1", "extra"},
			{"D234", "1", "17=a"},
			{"D234=1", "17", "a"},
			{"=a"},
			{"D234="},
			{"="},
			{"k1=1", "k2=1", "k3=1", "k4=1", "k5=1", "k6=1", "k7=1", "k8=1", "k9=1", "k10=1", "k11=1"},
		}
		for _, tc := range badCases {
			_, err := parseDecisionsRecordArgs(tc)
			assert.Error(t, err, "args=%v", tc)
		}
	})
}

func withMockDecisionsServer(t *testing.T, handler http.Handler) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	origFactory := decisionsClientFactory
	decisionsClientFactory = func() (hubclient.Client, error) {
		return hubclient.New(srv.URL, hubclient.WithAgentToken("test-agent-token"))
	}
	t.Cleanup(func() {
		decisionsClientFactory = origFactory
	})
}

func runDecisionsTestCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	prevFormat := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = prevFormat })

	root := &cobra.Command{Use: "scion", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newDecisionsCmd())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestDecisionsList_TablePaginationAndJSON(t *testing.T) {
	fixedNow := time.Date(2026, 11, 13, 5, 0, 0, 0, time.UTC)
	restoreNow := clitime.SetNow(func() time.Time { return fixedNow })
	defer restoreNow()
	clitime.SetZone(time.UTC)
	defer clitime.SetZone(nil)

	var seenStates []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/decisions", func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		cursor := r.URL.Query().Get("cursor")
		seenStates = append(seenStates, state+":"+cursor)
		w.Header().Set("Content-Type", "application/json")

		if cursor == "" {
			_ = json.NewEncoder(w).Encode(hubclient.AgentDecisionListResponse{
				Items: []hubclient.AgentDecisionListItem{
					{
						RowID:          "row-1",
						Key:            "D234",
						Ref:            "ag/D234",
						Version:        2,
						Status:         "pending",
						Question:       "Should we deploy the extraordinarily long release candidate to production right now?",
						ConversationID: "conv-alpha",
						CreatedAt:      fixedNow.Add(-5 * time.Minute),
					},
					{
						RowID:          "row-2",
						Key:            "17",
						Ref:            "ag/17",
						Version:        1,
						Status:         "decided",
						Question:       "Which strategy?",
						ConversationID: "conv-alpha",
						CreatedAt:      fixedNow.Add(-2 * time.Hour),
						Answer: &hubclient.DecisionAnswerView{
							ChoiceID:       "a",
							Label:          "Extend",
							AnsweredByName: "Alice",
							Provenance:     "agent_interpreted",
							Outcome:        "answer",
						},
					},
				},
				NextCursor: "page-2",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(hubclient.AgentDecisionListResponse{
			Items: []hubclient.AgentDecisionListItem{
				{
					RowID:          "row-3",
					Key:            "D100",
					Ref:            "ag/D100",
					Version:        1,
					Status:         "decided",
					Question:       "Confirm backup?",
					ConversationID: "conv-beta",
					CreatedAt:      fixedNow.Add(-24 * time.Hour),
					Answer: &hubclient.DecisionAnswerView{
						ChoiceID:       "1",
						Label:          "yes",
						AnsweredByName: "Bob",
						Provenance:     "direct",
						Outcome:        "confirm",
					},
				},
				{
					RowID:          "row-4",
					Key:            "D101",
					Ref:            "ag/D101",
					Version:        1,
					Status:         "decided",
					Question:       "Override region?",
					ConversationID: "conv-beta",
					CreatedAt:      fixedNow.Add(-48 * time.Hour),
					Answer: &hubclient.DecisionAnswerView{
						ChoiceID:       "2",
						Label:          "eu",
						AnsweredByName: "Bob",
						Provenance:     "direct",
						Outcome:        "override",
					},
				},
			},
		})
	})
	withMockDecisionsServer(t, mux)

	// Mutually exclusive state flags -> usage error.
	_, err := runDecisionsTestCmd(t, "decisions", "list", "--pending", "--all")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at most one")

	// Table output with pagination and provenance labels.
	out, err := runDecisionsTestCmd(t, "decisions", "list", "--all")
	require.NoError(t, err)
	assert.Equal(t, []string{"all:", "all:page-2"}, seenStates)
	assert.Contains(t, out, "KEY")
	assert.Contains(t, out, "PROVENANCE")
	assert.Contains(t, out, "D234")
	assert.Contains(t, out, "v2")
	assert.Contains(t, out, "...")
	assert.NotContains(t, out, "right now?") // truncated at 50 runes
	assert.Contains(t, out, "interpreted")
	assert.Contains(t, out, "confirmed")
	assert.Contains(t, out, "overridden")
	assert.Contains(t, out, "5m ago")

	// JSON output collects all pages into items.
	jsonOut, err := runDecisionsTestCmd(t, "decisions", "list", "--decided", "--json")
	require.NoError(t, err)
	var parsed hubclient.AgentDecisionListResponse
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &parsed))
	assert.Len(t, parsed.Items, 4)
}

func TestDecisionsShow_AllVersionsAndAllAnswerRows(t *testing.T) {
	clitime.SetZone(time.UTC)
	defer clitime.SetZone(nil)

	t0 := time.Date(2026, 11, 13, 3, 10, 0, 0, time.UTC)
	t1 := t0.Add(2 * time.Minute)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/decisions/D234", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hubclient.AgentDecisionShowResponse{
			Key: "D234",
			Ref: "agent-1/D234",
			Versions: []hubclient.AgentDecisionVersionView{
				{
					RowID:           "row-v1",
					Key:             "D234",
					Ref:             "agent-1/D234",
					Version:         1,
					Status:          "replaced",
					Question:        "Ship v1?",
					ConversationID:  "conv-1",
					PromptMessageID: "prompt-v1",
					Choices: []messages.Choice{
						{ID: "1", Label: "yes"},
						{ID: "2", Label: "no"},
					},
					DefaultChoiceID: "1",
					ReplacedBy:      &hubclient.DecisionReplacedByView{RowID: "row-v2", Version: 2, At: t1},
					CreatedAt:       t0,
					Answers:         []hubclient.DecisionAnswerView{},
				},
				{
					RowID:           "row-v2",
					Key:             "D234",
					Ref:             "agent-1/D234",
					Version:         2,
					Status:          "decided",
					Question:        "Ship v2?",
					ConversationID:  "conv-1",
					PromptMessageID: "prompt-v2",
					Choices: []messages.Choice{
						{ID: "1", Label: "yes"},
						{ID: "2", Label: "no"},
					},
					DefaultChoiceID: "1",
					CreatedAt:       t0,
					DecidedAt:       &t1,
					Answers: []hubclient.DecisionAnswerView{
						{
							ChoiceID:        "1",
							Label:           "yes",
							AnsweredByName:  "Alice",
							Provenance:      "agent_interpreted",
							Outcome:         "answer",
							SourceMessageID: "reply-msg-99",
							Surface:         "native",
							At:              t0,
						},
						{
							ChoiceID:       "2",
							Label:          "no",
							AnsweredByName: "Alice",
							Provenance:     "direct",
							Outcome:        "override",
							Surface:        "native",
							At:             t1,
						},
					},
				},
			},
		})
	})
	withMockDecisionsServer(t, mux)

	out, err := runDecisionsTestCmd(t, "decisions", "show", "D234")
	require.NoError(t, err)
	assert.Contains(t, out, "v1 [replaced] (replaced by v2)")
	assert.Contains(t, out, "- 1: yes (default)")
	assert.Contains(t, out, "v2 [decided]")
	assert.Contains(t, out, "yes (1) [interpreted] by Alice via native")
	assert.Contains(t, out, "(source reply-msg-99)")
	assert.Contains(t, out, "no (2) [overridden] by Alice via native")

	jsonOut, err := runDecisionsTestCmd(t, "decisions", "show", "D234", "--json")
	require.NoError(t, err)
	var parsed hubclient.AgentDecisionShowResponse
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &parsed))
	require.Len(t, parsed.Versions, 2)
	assert.Len(t, parsed.Versions[1].Answers, 2)
}

func TestDecisionsRecord_BothFormsAndErrorRendering(t *testing.T) {
	var captured []hubclient.RecordInterpretedRequest
	mode := "ok"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/decisions/interpreted", func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "exp_off":
			http.NotFound(w, r)
			return
		case "conflict":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"already_answered","message":"D234: already answered (interpreted by you from ptone's reply, yes)","details":{"errors":[{"key":"D234","code":"already_answered","message":"D234: already answered (interpreted by you from ptone's reply, yes)"}]}}}`))
			return
		}
		var req hubclient.RecordInterpretedRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		captured = append(captured, req)

		decs := make([]hubclient.DecisionView, len(req.Answers))
		for i, a := range req.Answers {
			decs[i] = hubclient.DecisionView{
				RowID:   "row-" + a.Key,
				Key:     a.Key,
				Version: 1,
				Status:  "decided",
				Decision: &hubclient.DecisionAnswerView{
					ChoiceID:   a.ChoiceID,
					Label:      "label-" + a.ChoiceID,
					Provenance: "agent_interpreted",
				},
			}
		}
		batchID := ""
		if len(decs) >= 2 {
			batchID = "batch-xyz"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hubclient.RecordInterpretedResponse{
			ResponseMessageID: "resp-123",
			BatchID:           batchID,
			Decisions:         decs,
		})
	})
	mux.HandleFunc("/api/v1/agent/decisions/MISSING", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"decision_not_found","message":"decision \"MISSING\" was not found"}}`))
	})
	withMockDecisionsServer(t, mux)

	// Missing --from -> usage error.
	_, err := runDecisionsTestCmd(t, "decisions", "record", "D234", "1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--from")

	// Mixed positional and key=choice -> usage error.
	_, err = runDecisionsTestCmd(t, "decisions", "record", "--from", "reply-1", "D234", "1", "17=a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot mix")

	// Form 1: positional single decision.
	out1, err := runDecisionsTestCmd(t, "decisions", "record", "D234", "1", "--from", "reply-1")
	require.NoError(t, err)
	require.Len(t, captured, 1)
	_, uuidErr := uuid.Parse(captured[0].IdempotencyKey)
	assert.NoError(t, uuidErr, "CLI generates a UUID idempotency key")
	assert.Equal(t, "reply-1", captured[0].SourceMessageID)
	assert.Equal(t, []hubclient.InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}}, captured[0].Answers)
	assert.Contains(t, out1, "Recorded 1 interpreted answer from reply-1 (response resp-123):")
	assert.Contains(t, out1, "D234 v1 -> label-1 (1)")

	// Form 2: multi-pair batch with --json.
	out2, err := runDecisionsTestCmd(t, "decisions", "record", "--from", "reply-2", "D234=1", "17=a", "--json")
	require.NoError(t, err)
	require.Len(t, captured, 2)
	assert.NotEqual(t, captured[0].IdempotencyKey, captured[1].IdempotencyKey)
	assert.Equal(t, []hubclient.InterpretedAnswerInput{
		{Key: "D234", ChoiceID: "1"},
		{Key: "17", ChoiceID: "a"},
	}, captured[1].Answers)
	var recResp hubclient.RecordInterpretedResponse
	require.NoError(t, json.Unmarshal([]byte(out2), &recResp))
	assert.Equal(t, "batch-xyz", recResp.BatchID)
	assert.Len(t, recResp.Decisions, 2)

	// 409 conflict renders per-key reason verbatim.
	mode = "conflict"
	_, err = runDecisionsTestCmd(t, "decisions", "record", "--from", "reply-2", "D234=1")
	require.Error(t, err)
	assert.Equal(t, "D234: already answered (interpreted by you from ptone's reply, yes)", strings.TrimSpace(err.Error()))

	// 404 decision_not_found is NOT misreported as experiment disabled.
	_, err = runDecisionsTestCmd(t, "decisions", "show", "MISSING")
	require.Error(t, err)
	assert.Equal(t, `decision "MISSING" was not found`, strings.TrimSpace(err.Error()))

	// Experiment off 404 -> friendly message naming web.chat_choices.
	mode = "exp_off"
	_, err = runDecisionsTestCmd(t, "decisions", "record", "--from", "reply-2", "D234=1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decisions need the web.chat_choices experiment")
}
