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

func TestMyDecisionService_ListQueryAndOverviewGroups(t *testing.T) {
	var got url.Values
	var body string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/users/me/decisions", func(w http.ResponseWriter, r *http.Request) {
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

	body = `{"counts":{"waitingOnYou":2,"waitingOnOthers":1},
		"groups":[{"project":{"id":"proj-1","name":"Scion","slug":"scion"},"more":true,
		  "entries":[{"project":{"id":"proj-1","name":"Scion","slug":"scion"},"answerId":"ans-1",
		    "decision":{"rowId":"r1","key":"ship-now","version":1,"question":"Deploy?"},"choiceId":"1","label":"Yes"}]}],
		"agents":[{"id":"a1","name":"release-bot","projectId":"proj-1"}],
		"answerers":[{"id":"u1","name":"Alice"}],
		"conversations":[{"key":"t1","label":"#release","projectId":"proj-1"}],
		"projects":[{"id":"proj-1","name":"Scion","slug":"scion"}],
		"truncatedProjects":true}`
	resp, err := c.MyDecisions().List(context.Background(), &ListMyDecisionsOptions{
		Tab: ProjectDecisionsTabMade, Sort: DecisionsSortProject, Limit: 25,
		ProjectID: "proj-1", Query: "deploy", Conversation: "t1", AgentID: "a1",
		UserID: "u1", Provenance: "direct", From: "2026-11-01", To: "2026-11-02",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{
		"tab": {"made"}, "sort": {"project"}, "limit": {"25"}, "project": {"proj-1"},
		"q": {"deploy"}, "conversation": {"t1"}, "agent": {"a1"},
		"user": {"u1"}, "provenance": {"direct"}, "from": {"2026-11-01"}, "to": {"2026-11-02"},
	}
	if got.Encode() != want.Encode() {
		t.Errorf("query = %s, want %s", got.Encode(), want.Encode())
	}
	if !resp.SupportsSearch() {
		t.Error("a response with a conversations facet supports search")
	}
	if !resp.TruncatedProjects {
		t.Error("expected truncatedProjects=true")
	}
	if len(resp.Projects) != 1 || resp.Projects[0].Slug != "scion" {
		t.Errorf("projects = %+v", resp.Projects)
	}
	if len(resp.Agents) != 1 || resp.Agents[0].ProjectID != "proj-1" {
		t.Errorf("agents = %+v", resp.Agents)
	}
	if len(resp.Answerers) != 1 || resp.Answerers[0].ProjectID != "" {
		t.Errorf("answerers = %+v", resp.Answerers)
	}
	if len(resp.Groups) != 1 || !resp.Groups[0].More || len(resp.Groups[0].Entries) != 1 || resp.Groups[0].Entries[0].Decision.Key != "ship-now" {
		t.Errorf("groups = %+v", resp.Groups)
	}

	// Nil options send no query parameters.
	body = `{"counts":{"waitingOnYou":0,"waitingOnOthers":0},"prompts":[],"agents":[],"answerers":[],"projects":[],"truncatedProjects":false}`
	resp, err = c.MyDecisions().List(context.Background(), nil)
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
