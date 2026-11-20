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
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestProjectDecisionService_ListQueryAndSearchSupport(t *testing.T) {
	var got url.Values
	var body string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/proj-1/decisions", func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c, err := New(srv.URL, WithBearerToken("tok"))
	if err != nil {
		t.Fatal(err)
	}

	body = `{"counts":{"waitingOnYou":3,"waitingOnOthers":1},"filteredCounts":{"waitingOnYou":1,"waitingOnOthers":0},
		"prompts":[{"project":{"id":"proj-1","name":"Scion","slug":"scion"},"promptMessageId":"m1","chatKey":"t1",
		"conversationLabel":"#release","waitingOnYou":true,"decisions":[{"rowId":"r1","key":"ship-now","version":1,"status":"pending"}]}],
		"agents":[{"id":"a1","name":"release-bot","hint":"deleted · a1"}],"answerers":[],
		"conversations":[{"key":"t1","label":"#release","projectId":"proj-1"}]}`
	resp, err := c.ProjectDecisions("proj-1").List(context.Background(), &ListProjectDecisionsOptions{
		Tab: ProjectDecisionsTabMade, Limit: 25, Cursor: "c1", Query: "release", Conversation: "t1", AgentID: "a1",
		UserID: "u1", Provenance: "direct", From: "2026-11-01", To: "2026-11-02",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{
		"tab": {"made"}, "limit": {"25"}, "cursor": {"c1"}, "q": {"release"}, "conversation": {"t1"}, "agent": {"a1"},
		"user": {"u1"}, "provenance": {"direct"}, "from": {"2026-11-01"}, "to": {"2026-11-02"},
	}
	if got.Encode() != want.Encode() {
		t.Errorf("query = %s, want %s", got.Encode(), want.Encode())
	}
	if !resp.SupportsSearch() {
		t.Error("a response with a conversations facet supports search")
	}
	if resp.FilteredCounts == nil || resp.FilteredCounts.WaitingOnYou != 1 || resp.Counts.WaitingOnYou != 3 {
		t.Errorf("counts = %+v filtered = %+v", resp.Counts, resp.FilteredCounts)
	}
	if len(resp.Prompts) != 1 || resp.Prompts[0].Project.Slug != "scion" || resp.Prompts[0].Decisions[0].Key != "ship-now" {
		t.Errorf("prompts = %+v", resp.Prompts)
	}
	if resp.Agents[0].Hint != "deleted · a1" {
		t.Errorf("agents = %+v", resp.Agents)
	}

	// Made response with recorded choices, defaultChoiceId, status, replacedBy, interpretedAnswer, and awaitingConfirmation.
	body = `{"counts":{"waitingOnYou":1,"waitingOnOthers":0},
		"entries":[
		  {"project":{"id":"proj-1","name":"Scion","slug":"scion"},"answerId":"ans-1",
		   "decision":{"rowId":"r1","promptMessageId":"m1","promptAvailable":true,"key":"db","ref":"ag/db","version":1,
		     "status":"replaced","question":"Which DB?","defaultChoiceId":"pg",
		     "choices":[{"id":"pg","label":"Postgres","description":"Managed"},{"id":"sq","label":"SQLite"}],
		     "replacedBy":{"rowId":"r2","version":2},"askerAgentId":"a1","askerAgentName":"bot"},
		   "choiceId":"pg","label":"Postgres","matchedDefault":true,"provenance":"direct","outcome":"confirm",
		   "interpretedAnswer":{"choiceId":"pg","label":"Postgres","provenance":"agent_interpreted","outcome":"answer","surface":"native","responseMessageId":"rm-0","at":"2026-11-01T10:00:00Z"},
		   "answeredByName":"Alice","surface":"decisions_page","at":"2026-11-01T10:05:00Z","chatKey":"t1","conversationLabel":"#release","conversationAvailable":true},
		  {"project":{"id":"proj-1","name":"Scion","slug":"scion"},"answerId":"ans-2",
		   "decision":{"rowId":"r3","promptMessageId":"m3","promptAvailable":true,"key":"cache","ref":"ag/cache","version":1,
		     "status":"decided","question":"Cache?","choices":[{"id":"yes","label":"Yes"},{"id":"no","label":"No"}],
		     "askerAgentId":"a1","askerAgentName":"bot"},
		   "choiceId":"yes","label":"Yes","matchedDefault":false,"provenance":"agent_interpreted","outcome":"answer",
		   "awaitingConfirmation":true,"answeredByName":"Alice","surface":"native","at":"2026-11-01T10:10:00Z","chatKey":"t1","conversationLabel":"#release","conversationAvailable":true}
		],
		"agents":[],"answerers":[],"conversations":[]}`
	madeResp, err := c.ProjectDecisions("proj-1").List(context.Background(), &ListProjectDecisionsOptions{Tab: ProjectDecisionsTabMade})
	if err != nil {
		t.Fatal(err)
	}
	if len(madeResp.Entries) != 2 {
		t.Fatalf("len(madeResp.Entries) = %d, want 2", len(madeResp.Entries))
	}
	e0 := madeResp.Entries[0]
	if e0.Decision.Status != "replaced" || e0.Decision.DefaultChoiceID != "pg" || len(e0.Decision.Choices) != 2 || e0.Decision.Choices[0].Description != "Managed" {
		t.Fatalf("unexpected e0.Decision: %+v", e0.Decision)
	}
	if e0.Decision.ReplacedBy == nil || e0.Decision.ReplacedBy.RowID != "r2" || e0.Decision.ReplacedBy.Version != 2 {
		t.Fatalf("unexpected e0.Decision.ReplacedBy: %+v", e0.Decision.ReplacedBy)
	}
	if e0.InterpretedAnswer == nil || e0.InterpretedAnswer.ChoiceID != "pg" || e0.AwaitingConfirmation {
		t.Fatalf("unexpected e0 fold fields: %+v", e0)
	}
	if !madeResp.Entries[1].AwaitingConfirmation || madeResp.Entries[1].InterpretedAnswer != nil {
		t.Fatalf("unexpected e1 awaitingConfirmation: %+v", madeResp.Entries[1])
	}

	// No options: no query string. An older hub sends no conversations facet.
	body = `{"counts":{"waitingOnYou":0,"waitingOnOthers":0},"prompts":[],"agents":[],"answerers":[]}`
	resp, err = c.ProjectDecisions("proj-1").List(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("query = %v, want none", got)
	}
	if resp.SupportsSearch() {
		t.Error("a response without a conversations facet does not support search")
	}
}
