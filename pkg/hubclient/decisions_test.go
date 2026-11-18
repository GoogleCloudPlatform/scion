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

package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func TestDecisionService_ListShowAndRecord(t *testing.T) {
	now := time.Date(2026, 11, 13, 4, 0, 0, 0, time.UTC)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/decisions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if got := r.Header.Get("X-Scion-Agent-Token"); got != "agent-tok-1" {
			t.Errorf("X-Scion-Agent-Token = %q, want agent-tok-1", got)
		}
		if got := r.URL.Query().Get("state"); got != "decided" {
			t.Errorf("state = %q, want decided", got)
		}
		if got := r.URL.Query().Get("cursor"); got != "cur-1" {
			t.Errorf("cursor = %q, want cur-1", got)
		}
		if got := r.URL.Query().Get("limit"); got != "25" {
			t.Errorf("limit = %q, want 25", got)
		}
		if r.URL.Query().Has("q") {
			t.Errorf("unexpected q parameter %q when Query is empty", r.URL.Query().Get("q"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AgentDecisionListResponse{
			Items: []AgentDecisionListItem{
				{
					RowID:           "row-1",
					Key:             "D234",
					Ref:             "agent-1/D234",
					Version:         1,
					Status:          "decided",
					Question:        "Ship now?",
					ConversationID:  "conv-1",
					PromptMessageID: "prompt-1",
					CreatedAt:       now,
					DecidedAt:       &now,
					Answer: &DecisionAnswerView{
						ChoiceID:        "1",
						Label:           "yes",
						AnsweredByName:  "Alice",
						Provenance:      "agent_interpreted",
						Outcome:         "answer",
						SourceMessageID: "reply-1",
						Surface:         "native",
						At:              now,
					},
				},
			},
			NextCursor: "cur-2",
		})
	})

	mux.HandleFunc("/api/v1/agent/decisions/D234", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AgentDecisionShowResponse{
			Key: "D234",
			Ref: "agent-1/D234",
			Versions: []AgentDecisionVersionView{
				{
					RowID:           "row-1",
					Key:             "D234",
					Ref:             "agent-1/D234",
					Version:         1,
					Status:          "decided",
					Question:        "Ship now?",
					ConversationID:  "conv-1",
					PromptMessageID: "prompt-1",
					Choices: []messages.Choice{
						{ID: "1", Label: "yes"},
						{ID: "2", Label: "no"},
					},
					DefaultChoiceID: "1",
					CreatedAt:       now,
					DecidedAt:       &now,
					Answers: []DecisionAnswerView{
						{ChoiceID: "1", Label: "yes", Provenance: "agent_interpreted", Outcome: "answer", At: now},
						{ChoiceID: "1", Label: "yes", Provenance: "direct", Outcome: "confirm", At: now.Add(time.Minute)},
					},
				},
			},
		})
	})

	mux.HandleFunc("/api/v1/agent/decisions/interpreted", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s", r.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req RecordInterpretedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if req.SourceMessageID != "reply-1" || req.IdempotencyKey != "idem-1" || len(req.Answers) != 2 {
			t.Errorf("unexpected request: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RecordInterpretedResponse{
			ResponseMessageID: "resp-msg-1",
			BatchID:           "batch-1",
			Decisions: []DecisionView{
				{RowID: "row-1", Key: "D234", Version: 1, Status: "decided"},
				{RowID: "row-2", Key: "17", Version: 1, Status: "decided"},
			},
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := New(srv.URL, WithAgentToken("agent-tok-1"))
	if err != nil {
		t.Fatalf("New client: %v", err)
	}

	ctx := context.Background()
	listResp, err := c.Decisions().List(ctx, &ListAgentDecisionsOptions{
		State:  "decided",
		Cursor: "cur-1",
		Limit:  25,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listResp.Items) != 1 || listResp.Items[0].Key != "D234" || listResp.NextCursor != "cur-2" {
		t.Fatalf("unexpected List response: %+v", listResp)
	}

	showResp, err := c.Decisions().Show(ctx, "D234")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if showResp.Key != "D234" || len(showResp.Versions) != 1 || len(showResp.Versions[0].Answers) != 2 {
		t.Fatalf("unexpected Show response: %+v", showResp)
	}

	recResp, err := c.Decisions().RecordInterpreted(ctx, &RecordInterpretedRequest{
		SourceMessageID: "reply-1",
		IdempotencyKey:  "idem-1",
		Answers: []InterpretedAnswerInput{
			{Key: "D234", ChoiceID: "1"},
			{Key: "17", ChoiceID: "a"},
		},
	})
	if err != nil {
		t.Fatalf("RecordInterpreted: %v", err)
	}
	if recResp.ResponseMessageID != "resp-msg-1" || recResp.BatchID != "batch-1" || len(recResp.Decisions) != 2 {
		t.Fatalf("unexpected RecordInterpreted response: %+v", recResp)
	}
}

func TestDecisionService_ListQueryParam(t *testing.T) {
	var seenRawQueries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenRawQueries = append(seenRawQueries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AgentDecisionListResponse{
			Query: r.URL.Query().Get("q"),
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithAgentToken("agent-tok-1"))
	if err != nil {
		t.Fatalf("New client: %v", err)
	}

	ctx := context.Background()
	withQ, err := c.Decisions().List(ctx, &ListAgentDecisionsOptions{State: "all", Query: "canary 50%"})
	if err != nil {
		t.Fatalf("List with Query: %v", err)
	}
	if withQ.Query != "canary 50%" {
		t.Errorf("withQ.Query = %q, want %q", withQ.Query, "canary 50%")
	}
	withoutQ, err := c.Decisions().List(ctx, &ListAgentDecisionsOptions{State: "pending"})
	if err != nil {
		t.Fatalf("List without Query: %v", err)
	}
	if withoutQ.Query != "" {
		t.Errorf("withoutQ.Query = %q, want empty", withoutQ.Query)
	}

	if len(seenRawQueries) != 2 {
		t.Fatalf("len(seenRawQueries) = %d, want 2", len(seenRawQueries))
	}
	if want := "q=canary+50%25&state=all"; seenRawQueries[0] != want {
		t.Errorf("seenRawQueries[0] = %q, want %q", seenRawQueries[0], want)
	}
	if want := "state=pending"; seenRawQueries[1] != want {
		t.Errorf("seenRawQueries[1] = %q, want %q", seenRawQueries[1], want)
	}
}

func TestDecisionService_Withdraw(t *testing.T) {
	now := time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/decisions/withdraw", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s", r.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if got := r.Header.Get("X-Scion-Agent-Token"); got != "agent-tok-1" {
			t.Errorf("X-Scion-Agent-Token = %q, want agent-tok-1", got)
		}
		var req WithdrawDecisionsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if len(req.Keys) != 1 || req.Keys[0] != "D234" || req.Reason != "Obsolete" || req.WholeSet {
			t.Errorf("unexpected request: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(WithdrawDecisionsResponse{
			Withdrawn: []DecisionView{
				{
					RowID:   "row-1",
					Key:     "D234",
					Version: 1,
					Status:  "withdrawn",
					Withdrawn: &DecisionWithdrawnView{
						At:     now,
						Reason: "Obsolete",
						ByName: "release-bot",
					},
				},
			},
			AlreadyWithdrawn: []DecisionView{},
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := New(srv.URL, WithAgentToken("agent-tok-1"))
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	resp, err := c.Decisions().Withdraw(context.Background(), &WithdrawDecisionsRequest{
		Keys:   []string{"D234"},
		Reason: "Obsolete",
	})
	if err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if len(resp.Withdrawn) != 1 || resp.Withdrawn[0].Withdrawn == nil || resp.Withdrawn[0].Withdrawn.Reason != "Obsolete" {
		t.Fatalf("unexpected Withdraw response: %+v", resp)
	}
}
