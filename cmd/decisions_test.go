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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecisionsCLIModesAndWindowsGate(t *testing.T) {
	for _, path := range []string{"decisions", "decisions.list", "decisions.show", "decisions.record", "decisions.withdraw"} {
		assert.True(t, agentAllowed[path], "agentAllowed[%q]", path)
		assert.True(t, windowsAllowed[path], "windowsAllowed[%q]", path)
	}

	for _, sub := range []string{"list", "show", "record", "withdraw"} {
		_, _, err := rootCmd.Find([]string{"decisions", sub})
		assert.NoError(t, err, "decisions %s is registered on rootCmd", sub)
	}

	for _, mode := range []CLIMode{ModeHuman, ModeAgent} {
		root := &cobra.Command{Use: "scion"}
		dec := newDecisionsCmd()
		root.AddCommand(dec)
		applyModeRestrictions(root, mode)
		names := collectCommandNames(root)
		for _, path := range []string{"decisions", "decisions.list", "decisions.show", "decisions.record", "decisions.withdraw"} {
			assert.Contains(t, names, path, "%s in %s mode", path, mode)
		}
		assert.True(t, containsLine(dec.Long, "scion decisions list"))
		assert.True(t, containsLine(dec.Long, "scion decisions show <key>"))
		assert.True(t, containsLine(dec.Long, "scion decisions record"))
		assert.True(t, containsLine(dec.Long, "scion decisions withdraw"))
	}

	// Verify Windows platform gate leaves decisions and all subcommands runnable (not gated).
	winTree, ran := gateTestTree(t)
	applyPlatformRestrictions(winTree, "windows")
	for _, args := range [][]string{
		{"decisions", "list"},
		{"decisions", "show", "D234"},
		{"decisions", "record", "D234", "1", "--from", "msg-1"},
		{"decisions", "withdraw", "D234"},
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
		got, err := parseDecisionsRecordArgs([]string{"D234", "1"}, nil)
		require.NoError(t, err)
		assert.Equal(t, []hubclient.InterpretedAnswerInput{{Key: "D234", ChoiceID: "1"}}, got)
	})

	t.Run("key=choice form single and batch", func(t *testing.T) {
		got, err := parseDecisionsRecordArgs([]string{"D234=1", "17=a", "ship-now=yes"}, nil)
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
			_, err := parseDecisionsRecordArgs(tc, nil)
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
	prevProject := projectPath
	projectPath = ""
	t.Cleanup(func() {
		outputFormat = prevFormat
		projectPath = prevProject
	})

	root := &cobra.Command{Use: "scion", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVarP(&projectPath, "project", "g", "", "")
	root.AddCommand(newDecisionsCmd())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestDecisionsList_TablePaginationAndJSON(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	fixedNow := time.Date(2026, 11, 13, 5, 0, 0, 0, time.UTC)
	restoreNow := clitime.SetNow(func() time.Time { return fixedNow })
	defer restoreNow()
	clitime.SetZone(time.UTC)
	defer clitime.SetZone(nil)

	var seenStates []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/decisions", func(w http.ResponseWriter, r *http.Request) {
		assert.False(t, r.URL.Query().Has("q"), "without -q, agent list omits q")
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
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"Not found"}}`))
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

	// Experiment off 404 -> friendly message naming web.decisions.
	mode = "exp_off"
	_, err = runDecisionsTestCmd(t, "decisions", "record", "--from", "reply-2", "D234=1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decisions need the web.decisions experiment")
}

func TestDecisionsListAgent_SearchValidationPaginationAndErrors(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	fixedNow := time.Date(2026, 11, 13, 5, 0, 0, 0, time.UTC)
	restoreNow := clitime.SetNow(func() time.Time { return fixedNow })
	defer restoreNow()
	clitime.SetZone(time.UTC)
	defer clitime.SetZone(nil)

	var seenRawQueries []string
	serverMode := "ok"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/decisions", func(w http.ResponseWriter, r *http.Request) {
		seenRawQueries = append(seenRawQueries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if serverMode == "timeout" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"SEARCH_TIMEOUT","message":"Search took too long. Try a longer phrase or search one space."}}`))
			return
		}
		appliedQuery := strings.TrimSpace(r.URL.Query().Get("q"))
		if serverMode == "old_hub" {
			// Pre-P3 hub ignores q and omits the "query" marker.
			next := "p2"
			if r.URL.Query().Get("cursor") != "" {
				next = ""
			}
			_ = json.NewEncoder(w).Encode(hubclient.AgentDecisionListResponse{
				Items: []hubclient.AgentDecisionListItem{
					{
						RowID:          "row-unfiltered",
						Key:            "unrelated-1",
						Ref:            "ag/unrelated-1",
						Version:        1,
						Status:         "pending",
						Question:       "Unrelated decision?",
						ConversationID: "conv-1",
						CreatedAt:      fixedNow.Add(-5 * time.Minute),
					},
				},
				NextCursor: next,
			})
			return
		}
		if serverMode == "empty" {
			_ = json.NewEncoder(w).Encode(hubclient.AgentDecisionListResponse{
				Items: []hubclient.AgentDecisionListItem{},
				Query: appliedQuery,
			})
			return
		}
		if r.URL.Query().Get("cursor") == "" {
			_ = json.NewEncoder(w).Encode(hubclient.AgentDecisionListResponse{
				Items: []hubclient.AgentDecisionListItem{
					{
						RowID:          "row-1",
						Key:            "canary-1",
						Ref:            "ag/canary-1",
						Version:        1,
						Status:         "pending",
						Question:       "Roll out canary 50%?",
						ConversationID: "conv-1",
						CreatedAt:      fixedNow.Add(-10 * time.Minute),
					},
				},
				NextCursor: "p2",
				Query:      appliedQuery,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(hubclient.AgentDecisionListResponse{
			Items: []hubclient.AgentDecisionListItem{
				{
					RowID:          "row-2",
					Key:            "canary-2",
					Ref:            "ag/canary-2",
					Version:        1,
					Status:         "decided",
					Question:       "Keep canary?",
					ConversationID: "conv-1",
					CreatedAt:      fixedNow.Add(-20 * time.Minute),
					Answer: &hubclient.DecisionAnswerView{
						ChoiceID:       "1",
						Label:          "CanaryYes",
						AnsweredByName: "Alice",
						Provenance:     "direct",
					},
				},
			},
			Query: appliedQuery,
		})
	})
	withMockDecisionsServer(t, mux)

	// Client-side bounds validation sends no HTTP request.
	for _, args := range [][]string{
		{"decisions", "list", "-q", "a"},
		{"decisions", "list", "--search", "  x  "},
		{"decisions", "list", "-q", strings.Repeat("é", 257)},
	} {
		_, err := runDecisionsTestCmd(t, args...)
		require.Error(t, err, "%v", args)
		assert.True(t, isUsageError(err), "%v: %v", args, err)
		assert.Contains(t, err.Error(), "--search must be 2 to 256 characters", "%v", args)
	}
	assert.Empty(t, seenRawQueries, "invalid --search sends no HTTP request")

	// Whitespace-only -q behaves identically to omitting -q (no q query parameter).
	serverMode = "empty"
	outNoQ, err := runDecisionsTestCmd(t, "decisions", "list")
	require.NoError(t, err)
	outBlankQ, err := runDecisionsTestCmd(t, "decisions", "list", "-q", "   ")
	require.NoError(t, err)
	assert.Equal(t, "No pending decisions.\n", outNoQ)
	assert.Equal(t, outNoQ, outBlankQ, "whitespace -q matches omitted -q byte-for-byte")
	assert.Equal(t, []string{"limit=200&state=pending", "limit=200&state=pending"}, seenRawQueries)

	// Empty search results with -q name the query across pending, decided, and all.
	outEmptyPendingQ, err := runDecisionsTestCmd(t, "decisions", "list", "-q", "  release  ")
	require.NoError(t, err)
	assert.Equal(t, "No pending decisions match \"release\".\n", outEmptyPendingQ)

	outEmptyDecidedQ, err := runDecisionsTestCmd(t, "decisions", "list", "--decided", "-q", "release")
	require.NoError(t, err)
	assert.Equal(t, "No decided decisions match \"release\".\n", outEmptyDecidedQ)

	outEmptyAllQ, err := runDecisionsTestCmd(t, "decisions", "list", "--all", "-q", "release")
	require.NoError(t, err)
	assert.Equal(t, "No all decisions match \"release\".\n", outEmptyAllQ)

	// Pre-P3 hub that ignores q (omits "query" marker) fails instead of returning unfiltered rows.
	seenRawQueries = nil
	serverMode = "old_hub"
	_, err = runDecisionsTestCmd(t, "decisions", "list", "-q", "canary")
	require.Error(t, err)
	assert.Equal(t, "this hub does not support decision search", err.Error())
	assert.Len(t, seenRawQueries, 1, "old hub check stops on first page before following cursor")

	// Omitting -q against a pre-P3 hub continues to work.
	outOldNoQ, err := runDecisionsTestCmd(t, "decisions", "list")
	require.NoError(t, err)
	assert.Contains(t, outOldNoQ, "unrelated-1")

	// Search with cursor pagination across state flags and --json.
	seenRawQueries = nil
	serverMode = "ok"
	outTable, err := runDecisionsTestCmd(t, "decisions", "list", "--all", "-q", "  canary 50%  ")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"limit=200&q=canary+50%25&state=all",
		"cursor=p2&limit=200&q=canary+50%25&state=all",
	}, seenRawQueries)
	assert.Contains(t, outTable, "canary-1")
	assert.Contains(t, outTable, "canary-2")
	assert.Contains(t, outTable, "CanaryYes")

	seenRawQueries = nil
	outJSON, err := runDecisionsTestCmd(t, "decisions", "list", "--decided", "--search", strings.Repeat("é", 256), "--json")
	require.NoError(t, err)
	require.Len(t, seenRawQueries, 2)
	var parsed hubclient.AgentDecisionListResponse
	require.NoError(t, json.Unmarshal([]byte(outJSON), &parsed))
	assert.Len(t, parsed.Items, 2)
	assert.Equal(t, strings.Repeat("é", 256), parsed.Query)

	// 503 SEARCH_TIMEOUT surfaces the Hub's timeout message cleanly.
	serverMode = "timeout"
	_, err = runDecisionsTestCmd(t, "decisions", "list", "-q", "canary")
	require.Error(t, err)
	assert.Equal(t, "Search took too long. Try a longer phrase or search one space.", err.Error())

	// Help text in agent mode uses "Decisions" (C4) and shows -q/--search while hiding human-only flags.
	root := &cobra.Command{Use: "scion"}
	root.AddCommand(newDecisionsCmd())
	applyModeRestrictions(root, ModeAgent)
	var helpBuf bytes.Buffer
	root.SetOut(&helpBuf)
	root.SetErr(&helpBuf)
	root.SetArgs([]string{"decisions", "list", "--help"})
	require.NoError(t, root.Execute())
	helpText := helpBuf.String()
	assert.Contains(t, helpText, "Decisions")
	_, flagsSection, ok := strings.Cut(helpText, "\nFlags:\n")
	require.True(t, ok, helpText)
	assert.Contains(t, flagsSection, "-q, --search")
	assert.NotContains(t, flagsSection, "--conversation")
}

func TestDecisionsWithdraw_OutputIdempotencyErrorsAndModes(t *testing.T) {
	var captured []hubclient.WithdrawDecisionsRequest
	serverMode := "ok"
	alreadyDone := map[string]bool{}
	withdrawHits := 0

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/decisions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if serverMode == "exp_off" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"route not found"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(hubclient.AgentDecisionListResponse{
			Items: []hubclient.AgentDecisionListItem{},
		})
	})
	mux.HandleFunc("/api/v1/agent/decisions/withdraw", func(w http.ResponseWriter, r *http.Request) {
		withdrawHits++
		w.Header().Set("Content-Type", "application/json")
		switch serverMode {
		case "exp_off":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"route not found"}}`))
			return
		case "old_hub_404_json":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"route not found"}}`))
			return
		case "old_hub_404_plain":
			http.NotFound(w, r)
			return
		case "old_hub_405":
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"error":{"code":"method_not_allowed","message":"Method not allowed"}}`))
			return
		case "conflict_409":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"withdraw_failed","message":"17: already answered by alice (yes); NOPE: decision not found","details":{"failures":[{"key":"17","reason":"already_answered","decision":{"key":"17","version":1,"status":"decided","decision":{"choiceId":"1","label":"yes","answeredByName":"alice","provenance":"direct"}}},{"key":"NOPE","reason":"decision_not_found","message":"NOPE: decision not found"}]}}}`))
			return
		}

		var req hubclient.WithdrawDecisionsRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		captured = append(captured, req)

		var resp hubclient.WithdrawDecisionsResponse
		resp.Withdrawn = []hubclient.DecisionView{}
		resp.AlreadyWithdrawn = []hubclient.DecisionView{}
		for _, k := range req.Keys {
			norm := messages.DecisionKeyNorm(k)
			view := hubclient.DecisionView{
				RowID:   "row-" + norm,
				Key:     k,
				Version: 1,
				Status:  "withdrawn",
				Withdrawn: &hubclient.DecisionWithdrawnView{
					At:     time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC),
					Reason: req.Reason,
					ByName: "release-bot",
				},
			}
			if alreadyDone[norm] {
				resp.AlreadyWithdrawn = append(resp.AlreadyWithdrawn, view)
			} else {
				alreadyDone[norm] = true
				resp.Withdrawn = append(resp.Withdrawn, view)
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	withMockDecisionsServer(t, mux)

	// 1. Human mode refuses before sending any HTTP request.
	t.Setenv("SCION_CLI_MODE", "human")
	_, err := runDecisionsTestCmd(t, "decisions", "withdraw", "D234")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "this command is for agents")
	assert.Zero(t, withdrawHits)

	// 2. Agent mode: validation rejects missing keys, duplicate keys, >10 keys, and --reason > 500 runes.
	t.Setenv("SCION_CLI_MODE", "agent")
	for _, badArgs := range [][]string{
		{"decisions", "withdraw"},
		{"decisions", "withdraw", "   "},
		{"decisions", "withdraw", "D234", "d234"},
		{"decisions", "withdraw", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9", "k10", "k11"},
		{"decisions", "withdraw", "D234", "--reason", strings.Repeat("é", 501)},
	} {
		_, err := runDecisionsTestCmd(t, badArgs...)
		require.Error(t, err, "args=%v", badArgs)
		assert.True(t, isUsageError(err), "args=%v: %v", badArgs, err)
	}
	assert.Zero(t, withdrawHits, "client-side validation sends no HTTP request")

	// 3. First withdrawal prints "<key> v1 withdrawn"; repeat prints "<key> v1 already withdrawn".
	out1, err := runDecisionsTestCmd(t, "decisions", "withdraw", "D234", "--reason", "  CI found a regression  ")
	require.NoError(t, err)
	assert.Equal(t, "D234 v1 withdrawn\n", out1)
	require.Len(t, captured, 1)
	assert.Equal(t, []string{"D234"}, captured[0].Keys)
	assert.Equal(t, "CI found a regression", captured[0].Reason)

	out2, err := runDecisionsTestCmd(t, "decisions", "withdraw", "D234")
	require.NoError(t, err)
	assert.Equal(t, "D234 v1 already withdrawn\n", out2)

	// 4. --json outputs the WithdrawDecisionsResponse payload.
	jsonOut, err := runDecisionsTestCmd(t, "decisions", "withdraw", "17", "--json")
	require.NoError(t, err)
	var parsed hubclient.WithdrawDecisionsResponse
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &parsed))
	require.Len(t, parsed.Withdrawn, 1)
	assert.Equal(t, "17", parsed.Withdrawn[0].Key)

	// 5. 409 withdraw_failed prints one line per failed key and returns an error (exit 1).
	serverMode = "conflict_409"
	_, err = runDecisionsTestCmd(t, "decisions", "withdraw", "17", "NOPE")
	require.Error(t, err)
	assert.False(t, isUsageError(err))
	assert.Equal(t, "17: already answered by alice (yes)\nNOPE: decision not found", err.Error())

	// 6. Old hub (plain 404, JSON 404 while agent decisions list works, or 405 on POST /withdraw)
	// reports the section 4.4 message.
	for _, oldMode := range []string{"old_hub_404_plain", "old_hub_404_json", "old_hub_405"} {
		serverMode = oldMode
		_, err := runDecisionsTestCmd(t, "decisions", "withdraw", "D234")
		require.Error(t, err, oldMode)
		assert.Equal(t, "this hub does not support withdrawing decisions; update the hub", err.Error(), oldMode)
	}

	// 7. Experiment off (both /withdraw and /agent/decisions return 404 route not found)
	// reports web.decisions experiment required.
	serverMode = "exp_off"
	_, err = runDecisionsTestCmd(t, "decisions", "withdraw", "D234")
	require.Error(t, err)
	assert.Equal(t, "decisions need the web.decisions experiment", err.Error())
}

func TestDecisionsWithdraw_MultiKeyWholeSetAndListShowWithdrawn(t *testing.T) {
	fixedNow := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	restoreNow := clitime.SetNow(func() time.Time { return fixedNow })
	defer restoreNow()
	clitime.SetZone(time.UTC)
	defer clitime.SetZone(nil)

	tWithdrawn := time.Date(2026, 11, 18, 5, 45, 0, 0, time.UTC)
	var lastWithdrawReq hubclient.WithdrawDecisionsRequest
	listMode := "ok"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/decisions/withdraw", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&lastWithdrawReq))
		w.Header().Set("Content-Type", "application/json")
		if lastWithdrawReq.WholeSet {
			_ = json.NewEncoder(w).Encode(hubclient.WithdrawDecisionsResponse{
				Withdrawn: []hubclient.DecisionView{
					{Key: "D1", Version: 1, Status: "withdrawn"},
					{Key: "D3", Version: 1, Status: "withdrawn"},
				},
				AlreadyWithdrawn: []hubclient.DecisionView{},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(hubclient.WithdrawDecisionsResponse{
			Withdrawn: []hubclient.DecisionView{
				{Key: "D2", Version: 2, Status: "withdrawn"},
			},
			AlreadyWithdrawn: []hubclient.DecisionView{
				{Key: "D1", Version: 1, Status: "withdrawn"},
			},
		})
	})
	mux.HandleFunc("/api/v1/agent/decisions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if listMode == "old_hub_state" && r.URL.Query().Get("state") == "withdrawn" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"validation_error","message":"invalid state \"withdrawn\": must be pending, decided, or all"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(hubclient.AgentDecisionListResponse{
			Items: []hubclient.AgentDecisionListItem{
				{
					RowID:          "row-wd-1",
					Key:            "D234",
					Ref:            "ag/D234",
					Version:        1,
					Status:         "withdrawn",
					Question:       "Ship v0.9 today?",
					ConversationID: "conv-rel",
					CreatedAt:      fixedNow.Add(-30 * time.Minute),
					Withdrawn: &hubclient.DecisionWithdrawnView{
						At:     tWithdrawn,
						Reason: "CI found a regression",
						ByName: "release-bot",
					},
				},
			},
			Query: strings.TrimSpace(r.URL.Query().Get("q")),
		})
	})
	mux.HandleFunc("/api/v1/agent/decisions/D234", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hubclient.AgentDecisionShowResponse{
			Key: "D234",
			Ref: "ag/D234",
			Versions: []hubclient.AgentDecisionVersionView{
				{
					RowID:           "row-wd-1",
					Key:             "D234",
					Ref:             "ag/D234",
					Version:         1,
					Status:          "withdrawn",
					Question:        "Ship v0.9 today?",
					ConversationID:  "conv-rel",
					PromptMessageID: "prompt-1",
					Choices:         []messages.Choice{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}},
					ReplacedBy:      &hubclient.DecisionReplacedByView{RowID: "row-wd-2", Version: 2, At: fixedNow},
					Withdrawn: &hubclient.DecisionWithdrawnView{
						At:     tWithdrawn,
						Reason: "CI found a regression",
						ByName: "release-bot",
					},
					CreatedAt: fixedNow.Add(-30 * time.Minute),
					Answers:   []hubclient.DecisionAnswerView{},
				},
			},
		})
	})
	withMockDecisionsServer(t, mux)

	// 1. Multi-key output preserves requested key order and distinguishes already withdrawn.
	t.Setenv("SCION_CLI_MODE", "agent")
	outMulti, err := runDecisionsTestCmd(t, "decisions", "withdraw", "D1", "D2", "--reason", "No longer needed")
	require.NoError(t, err)
	assert.False(t, lastWithdrawReq.WholeSet)
	assert.Equal(t, []string{"D1", "D2"}, lastWithdrawReq.Keys)
	assert.Equal(t, "D1 v1 already withdrawn\nD2 v2 withdrawn\n", outMulti)

	// 2. --set sends wholeSet=true and prints both explicitly named and sibling withdrawn keys.
	outSet, err := runDecisionsTestCmd(t, "decisions", "withdraw", "D1", "--set", "--reason", "Set obsolete")
	require.NoError(t, err)
	assert.True(t, lastWithdrawReq.WholeSet)
	assert.Equal(t, "D1 v1 withdrawn\nD3 v1 withdrawn\n", outSet)

	// 3. list --withdrawn and list --all show status, withdrawal time, and reason; mutually exclusive with other state flags.
	_, err = runDecisionsTestCmd(t, "decisions", "list", "--withdrawn", "--decided")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--withdrawn")

	outListWd, err := runDecisionsTestCmd(t, "decisions", "list", "--withdrawn")
	require.NoError(t, err)
	assert.Contains(t, outListWd, "D234")
	assert.Contains(t, outListWd, "withdrawn")
	assert.Contains(t, outListWd, "2026-11-18 05:45 UTC")
	assert.Contains(t, outListWd, "CI found a regression")

	outListAll, err := runDecisionsTestCmd(t, "decisions", "list", "--all")
	require.NoError(t, err)
	assert.Contains(t, outListAll, "2026-11-18 05:45 UTC")
	assert.Contains(t, outListAll, "CI found a regression")

	// Old hub rejecting state=withdrawn surfaces update-hub error.
	listMode = "old_hub_state"
	_, err = runDecisionsTestCmd(t, "decisions", "list", "--withdrawn")
	require.Error(t, err)
	assert.Equal(t, "this hub does not support withdrawing decisions; update the hub", err.Error())
	listMode = "ok"

	// Human mode rejects --withdrawn as an agent-mode flag.
	t.Setenv("SCION_CLI_MODE", "human")
	_, err = runDecisionsTestCmd(t, "decisions", "list", "--withdrawn")
	require.Error(t, err)
	assert.True(t, isUsageError(err))

	// 4. show D234 renders v1 [withdrawn] (replaced by v2), Withdrawn timestamp/asker, and Reason.
	t.Setenv("SCION_CLI_MODE", "agent")
	outShow, err := runDecisionsTestCmd(t, "decisions", "show", "D234")
	require.NoError(t, err)
	assert.Contains(t, outShow, "v1 [withdrawn] (replaced by v2)")
	assert.Contains(t, outShow, "Withdrawn:    2026-11-18 05:45 UTC by release-bot")
	assert.Contains(t, outShow, "Reason:       CI found a regression")
}

func TestDecisionsDismissed_AgentListShowAndWithdrawFailure(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	fixedNow := time.Date(2026, 11, 18, 16, 0, 0, 0, time.UTC)
	restoreNow := clitime.SetNow(func() time.Time { return fixedNow })
	defer restoreNow()
	clitime.SetZone(time.UTC)
	defer clitime.SetZone(nil)

	dismissedAt := time.Date(2026, 11, 18, 15, 52, 0, 0, time.UTC)
	var seenStates []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/decisions", func(w http.ResponseWriter, r *http.Request) {
		seenStates = append(seenStates, r.URL.Query().Get("state"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hubclient.AgentDecisionListResponse{
			Items: []hubclient.AgentDecisionListItem{
				{
					RowID:          "row-dsm-1",
					Key:            "D234",
					Ref:            "ag/D234",
					Version:        1,
					Status:         "dismissed",
					Question:       "Ship v0.9 today?",
					ConversationID: "conv-rel",
					CreatedAt:      fixedNow.Add(-15 * time.Minute),
					Dismissed: &hubclient.DecisionDismissedView{
						At:                dismissedAt,
						ByUserID:          "u-alice",
						ByName:            "alice",
						Reason:            "Stale: the release already shipped",
						Surface:           "decisions_page",
						ResponseMessageID: "resp-dsm-1",
					},
				},
			},
		})
	})
	mux.HandleFunc("/api/v1/agent/decisions/D234", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hubclient.AgentDecisionShowResponse{
			Key: "D234",
			Ref: "ag/D234",
			Versions: []hubclient.AgentDecisionVersionView{
				{
					RowID:           "row-dsm-1",
					Key:             "D234",
					Ref:             "ag/D234",
					Version:         1,
					Status:          "dismissed",
					Question:        "Ship v0.9 today?",
					ConversationID:  "conv-rel",
					PromptMessageID: "prompt-dsm-1",
					Choices: []messages.Choice{
						{ID: "1", Label: "Yes"},
						{ID: "2", Label: "No"},
					},
					DefaultChoiceID: "1",
					CreatedAt:       dismissedAt.Add(-10 * time.Minute),
					Dismissed: &hubclient.DecisionDismissedView{
						At:                dismissedAt,
						ByUserID:          "u-alice",
						ByName:            "alice",
						Reason:            "Stale: the release already shipped",
						Surface:           "decisions_page",
						ResponseMessageID: "resp-dsm-1",
					},
					Answers: []hubclient.DecisionAnswerView{
						{
							ChoiceID:        "1",
							Label:           "Yes",
							AnsweredByName:  "alice",
							Provenance:      "agent_interpreted",
							Outcome:         "answer",
							SourceMessageID: "src-1",
							Surface:         "native",
							At:              dismissedAt.Add(-8 * time.Minute),
						},
						{
							ChoiceID:       "$dismissed",
							Label:          "Dismissed",
							AnsweredByName: "alice",
							Provenance:     "direct",
							Outcome:        "dismiss",
							DismissReason:  "Stale: the release already shipped",
							Surface:        "decisions_page",
							At:             dismissedAt,
						},
					},
				},
			},
		})
	})
	mux.HandleFunc("/api/v1/agent/decisions/withdraw", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"withdraw_failed","message":"D234: dismissed by alice","details":{"failures":[{"key":"D234","reason":"dismissed","decision":{"key":"D234","version":1,"status":"dismissed","dismissed":{"byName":"alice","reason":"Stale"}}}]}}}`))
	})
	withMockDecisionsServer(t, mux)

	// --dismissed is mutually exclusive with --pending/--decided/--withdrawn/--all.
	_, err := runDecisionsTestCmd(t, "decisions", "list", "--dismissed", "--pending")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at most one")

	// list --dismissed requests state=dismissed and prints "dismissed by alice · <time> · <reason>".
	listOut, err := runDecisionsTestCmd(t, "decisions", "list", "--dismissed")
	require.NoError(t, err)
	assert.Equal(t, []string{"dismissed"}, seenStates)
	assert.Contains(t, listOut, "dismissed by alice · 2026-11-18 15:52 UTC · Stale: the release already shipped")

	// show D234 prints v1 [dismissed], Dismissed summary line, and both [interpreted] and [dismissed] answer rows.
	showOut, err := runDecisionsTestCmd(t, "decisions", "show", "D234")
	require.NoError(t, err)
	assert.Contains(t, showOut, "v1 [dismissed]")
	assert.Contains(t, showOut, "Dismissed:    dismissed by alice · 2026-11-18 15:52 UTC · Stale: the release already shipped")
	assert.Contains(t, showOut, "Yes (1) [interpreted] by alice via native")
	assert.Contains(t, showOut, "Dismissed ($dismissed) [dismissed] by alice via decisions_page at 2026-11-18 15:52 UTC: Stale: the release already shipped")

	// withdraw on dismissed key prints per-key "D234: dismissed by alice".
	_, err = runDecisionsTestCmd(t, "decisions", "withdraw", "D234")
	require.Error(t, err)
	assert.Equal(t, "D234: dismissed by alice", err.Error())
}

// TestDecisionDismissedSummary_TruncatesReasonOnlyInList pins that the list
// table truncates a long dismiss reason like the withdrawn branch, while show
// keeps the full reason.
func TestDecisionDismissedSummary_TruncatesReasonOnlyInList(t *testing.T) {
	reason := strings.Repeat("stale ", 40)
	d := &hubclient.DecisionDismissedView{ByName: "alice", Reason: reason}

	list := decisionDismissedSummary(d, decisionQuestionMaxWidth)
	parts := strings.Split(list, " · ")
	require.Len(t, parts, 3)
	assert.Equal(t, truncateRunes(collapseSingleLine(reason), decisionQuestionMaxWidth, true), parts[2])
	assert.LessOrEqual(t, utf8.RuneCountInString(parts[2]), decisionQuestionMaxWidth)

	show := decisionDismissedSummary(d, 0)
	assert.True(t, strings.HasSuffix(show, " · "+collapseSingleLine(reason)))
}

func TestDecisionsOther_RecordShowListAndValidation(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	fixedNow := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	restoreNow := clitime.SetNow(func() time.Time { return fixedNow })
	defer restoreNow()
	clitime.SetZone(time.UTC)
	defer clitime.SetZone(nil)

	var recorded []hubclient.RecordInterpretedRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/decisions/interpreted", func(w http.ResponseWriter, r *http.Request) {
		var req hubclient.RecordInterpretedRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		recorded = append(recorded, req)
		decs := make([]hubclient.DecisionView, len(req.Answers))
		for i, a := range req.Answers {
			lbl := "Yes"
			if a.ChoiceID == messages.ChoiceIDOther {
				lbl = "Another region"
			}
			decs[i] = hubclient.DecisionView{
				RowID:   fmt.Sprintf("row-%d", i+1),
				Key:     a.Key,
				Version: 1,
				Status:  "decided",
				Decision: &hubclient.DecisionAnswerView{
					ChoiceID:   a.ChoiceID,
					Label:      lbl,
					OtherText:  a.OtherText,
					Provenance: "agent_interpreted",
					Outcome:    "answer",
					Surface:    "slack",
					At:         fixedNow,
				},
			}
		}
		batchID := ""
		if len(decs) > 1 {
			batchID = "batch-other-1"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hubclient.RecordInterpretedResponse{
			ResponseMessageID: "resp-other-1",
			BatchID:           batchID,
			Decisions:         decs,
		})
	})
	mux.HandleFunc("/api/v1/agent/decisions/region", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hubclient.AgentDecisionShowResponse{
			Key: "region",
			Ref: "ag/region",
			Versions: []hubclient.AgentDecisionVersionView{
				{
					RowID:           "row-reg-1",
					Key:             "region",
					Ref:             "ag/region",
					Version:         1,
					Status:          "decided",
					Question:        "Which region for the canary?",
					ConversationID:  "conv-rel",
					PromptMessageID: "prompt-reg-1",
					Choices:         []messages.Choice{{ID: "1", Label: "us-central1"}},
					DefaultChoiceID: "1",
					Other: &messages.OtherOption{
						Label:       "Another region",
						Placeholder: "e.g. europe-west4",
					},
					CreatedAt: fixedNow.Add(-10 * time.Minute),
					Answers: []hubclient.DecisionAnswerView{
						{
							ChoiceID:        messages.ChoiceIDOther,
							Label:           "Another region",
							OtherText:       "europe-west4",
							AnsweredByName:  "Alice",
							Provenance:      "agent_interpreted",
							Outcome:         "answer",
							SourceMessageID: "src-1",
							Surface:         "slack",
							At:              fixedNow.Add(-5 * time.Minute),
						},
						{
							ChoiceID:            messages.ChoiceIDOther,
							Label:               "Another region",
							OtherText:           "asia-east1 canary",
							PreviousChoiceLabel: "Another region",
							PreviousOtherText:   "europe-west4",
							AnsweredByName:      "Alice",
							Provenance:          "direct",
							Outcome:             "override",
							Surface:             "native",
							At:                  fixedNow,
						},
					},
				},
			},
		})
	})
	mux.HandleFunc("/api/v1/agent/decisions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hubclient.AgentDecisionListResponse{
			Items: []hubclient.AgentDecisionListItem{
				{
					RowID:          "row-reg-1",
					Key:            "region",
					Ref:            "ag/region",
					Version:        1,
					Status:         "decided",
					Question:       "Which region for the canary?",
					ConversationID: "conv-rel",
					CreatedAt:      fixedNow.Add(-10 * time.Minute),
					Answer: &hubclient.DecisionAnswerView{
						ChoiceID:       messages.ChoiceIDOther,
						Label:          "Another region",
						OtherText:      "europe-west4 canary",
						AnsweredByName: "Alice",
						Provenance:     "direct",
						Outcome:        "answer",
						Surface:        "native",
						At:             fixedNow,
					},
				},
			},
			Query: strings.TrimSpace(r.URL.Query().Get("q")),
		})
	})
	withMockDecisionsServer(t, mux)

	// 1. Client-side validation rejects positional $other (with --other hint), mixing, duplicates, and bad --other values.
	for name, args := range map[string][]string{
		"positional $other":    {"decisions", "record", "region", "$other", "--from", "msg-1"},
		"key=$other pair":      {"decisions", "record", "region=$other", "--from", "msg-1"},
		"mix positional other": {"decisions", "record", "proceed", "1", "--other", "region=eu", "--from", "msg-1"},
		"duplicate key":        {"decisions", "record", "region=1", "--other", "REGION=eu", "--from", "msg-1"},
		"missing equals":       {"decisions", "record", "--other", "region", "--from", "msg-1"},
		"empty text":           {"decisions", "record", "--other", "region=   ", "--from", "msg-1"},
		"newline text":         {"decisions", "record", "--other", "region=a\nb", "--from", "msg-1"},
		"too long text":        {"decisions", "record", "--other", "region=" + strings.Repeat("a", 201), "--from", "msg-1"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runDecisionsTestCmd(t, args...)
			require.Error(t, err)
			assert.True(t, isUsageError(err), "err=%v", err)
			if strings.Contains(name, "$other") {
				assert.Contains(t, err.Error(), "--other")
			}
		})
	}
	assert.Empty(t, recorded, "client-side validation sends no HTTP request")

	// 2. Single --other flag succeeds and prints label + quoted text.
	outSingle, err := runDecisionsTestCmd(t, "decisions", "record", "--from", "msg-1", "--other", "region=europe-west4 canary")
	require.NoError(t, err)
	assert.Contains(t, outSingle, `region v1 -> Another region "europe-west4 canary" ($other)`)
	require.Len(t, recorded, 1)
	require.Len(t, recorded[0].Answers, 1)
	assert.Equal(t, messages.ChoiceIDOther, recorded[0].Answers[0].ChoiceID)
	assert.Equal(t, "europe-west4 canary", recorded[0].Answers[0].OtherText)

	// 3. Batch combining <key>=<choice-id> and --other preserves commas in text (StringArray).
	outBatch, err := runDecisionsTestCmd(t, "decisions", "record", "--from", "msg-2", "proceed=1", "--other", "region=europe-west4, after 6 PM")
	require.NoError(t, err)
	assert.Contains(t, outBatch, `proceed v1 -> Yes (1)`)
	assert.Contains(t, outBatch, `region v1 -> Another region "europe-west4, after 6 PM" ($other)`)
	require.Len(t, recorded, 2)
	require.Len(t, recorded[1].Answers, 2)
	assert.Equal(t, "europe-west4, after 6 PM", recorded[1].Answers[1].OtherText)

	// 4. show text prints "$other  Another region (free-form)" and quoted answer text; --json carries other, otherText, previousOtherText.
	showOut, err := runDecisionsTestCmd(t, "decisions", "show", "region")
	require.NoError(t, err)
	assert.Contains(t, showOut, "    - $other  Another region (free-form)\n")
	assert.Contains(t, showOut, `Another region "europe-west4" ($other) [interpreted]`)
	assert.Contains(t, showOut, `Another region "asia-east1 canary" ($other) [overridden]`)

	showJSON, err := runDecisionsTestCmd(t, "decisions", "show", "region", "--json")
	require.NoError(t, err)
	var parsedShow hubclient.AgentDecisionShowResponse
	require.NoError(t, json.Unmarshal([]byte(showJSON), &parsedShow))
	require.NotNil(t, parsedShow.Versions[0].Other)
	assert.Equal(t, "Another region", parsedShow.Versions[0].Other.Label)
	assert.Equal(t, "asia-east1 canary", parsedShow.Versions[0].Answers[1].OtherText)
	assert.Equal(t, "europe-west4", parsedShow.Versions[0].Answers[1].PreviousOtherText)

	// 5. list text prints `<label>: "<text>"` and --json carries otherText.
	listOut, err := runDecisionsTestCmd(t, "decisions", "list", "--decided")
	require.NoError(t, err)
	assert.Contains(t, listOut, `Another region: "europe-west4 canary"`)

	listJSON, err := runDecisionsTestCmd(t, "decisions", "list", "--decided", "--json")
	require.NoError(t, err)
	var parsedList hubclient.AgentDecisionListResponse
	require.NoError(t, json.Unmarshal([]byte(listJSON), &parsedList))
	require.Len(t, parsedList.Items, 1)
	require.NotNil(t, parsedList.Items[0].Answer)
	assert.Equal(t, "europe-west4 canary", parsedList.Items[0].Answer.OtherText)
}
