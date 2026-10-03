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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// spaceMembersStore wraps a real store to count ListAgents calls and to
// inject failures and hooks into the members endpoint's store reads.
type spaceMembersStore struct {
	store.Store
	listAgentsCalls int
	// failListAgentsOnCall makes the Nth ListAgents call (1-based) fail.
	failListAgentsOnCall int
	failProjectMembers   bool
	// onAgentPage, when set, runs after each successful ListAgents call with
	// the 1-based call number and whether that page was the last one.
	onAgentPage func(call int, last bool)
	// onEffectiveGroups, when set, runs on every GetEffectiveGroups call,
	// which the authorization service makes once per access decision for a
	// user principal.
	onEffectiveGroups func()
}

func (s *spaceMembersStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	s.listAgentsCalls++
	if s.failListAgentsOnCall == s.listAgentsCalls {
		return nil, errors.New("injected list agents failure")
	}
	page, err := s.Store.ListAgents(ctx, filter, opts)
	if err == nil && s.onAgentPage != nil {
		s.onAgentPage(s.listAgentsCalls, page.NextCursor == "")
	}
	return page, err
}

func (s *spaceMembersStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	if s.onEffectiveGroups != nil {
		s.onEffectiveGroups()
	}
	return s.Store.GetEffectiveGroups(ctx, userID)
}

func (s *spaceMembersStore) ListProjectMembers(ctx context.Context, projectID string) ([]*store.ProjectMembership, error) {
	if s.failProjectMembers {
		return nil, errors.New("injected list project members failure")
	}
	return s.Store.ListProjectMembers(ctx, projectID)
}

// createSpaceMembersAgents creates n agents in projectID owned by ownerID.
func createSpaceMembersAgents(t *testing.T, s store.Store, projectID, ownerID, prefix string, n int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%s-%03d", prefix, i)
		a := &store.Agent{
			ID:        tid(name),
			ProjectID: projectID,
			Name:      name,
			Slug:      name,
			Phase:     "running",
			OwnerID:   ownerID,
			CreatedBy: ownerID,
			Ancestry:  []string{ownerID},
		}
		if err := s.CreateAgent(ctx, a); err != nil {
			t.Fatalf("CreateAgent %s: %v", name, err)
		}
		ids = append(ids, a.ID)
	}
	return ids
}

func createSpaceMembersProject(t *testing.T, s store.Store, name string) *store.Project {
	t.Helper()
	proj := &store.Project{ID: tid(name), Name: name, Slug: name, Created: time.Now(), Updated: time.Now()}
	if err := s.CreateProject(context.Background(), proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return proj
}

func decodeSpaceMembers(t *testing.T, code int, body []byte) chatMembersResponse {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	var resp chatMembersResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

// A project with more agents than one store page must return all of them:
// the handler walks the store cursor rather than taking the first page.
func TestSpaceMembers_ReturnsAllAgentsAcrossStorePages(t *testing.T) {
	srv, s := testServer(t)
	proj := createSpaceMembersProject(t, s, "members-paginate-all")
	want := createSpaceMembersAgents(t, s, proj.ID, DevUserID, "pg-all", 450)

	counting := &spaceMembersStore{Store: s}
	srv.store = counting

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+proj.ID+"/members", nil)
	resp := decodeSpaceMembers(t, rec.Code, rec.Body.Bytes())

	if len(resp.Agents) != len(want) {
		t.Fatalf("agents = %d, want %d", len(resp.Agents), len(want))
	}
	got := make(map[string]bool, len(resp.Agents))
	for _, a := range resp.Agents {
		if got[a.ID] {
			t.Fatalf("agent %s returned twice", a.ID)
		}
		got[a.ID] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("agent %s missing from response", id)
		}
	}
	if minPages := (len(want) + projectAgentsPageSize - 1) / projectAgentsPageSize; counting.listAgentsCalls < minPages {
		t.Fatalf("ListAgents calls = %d, want at least %d store pages", counting.listAgentsCalls, minPages)
	}
}

// The per-agent attach decision must still run for agents beyond the first
// store page: an agent the caller cannot attach to, sitting on a later page,
// is returned with canAttach=false while the caller's own agents on that page
// stay attachable.
func TestSpaceMembers_CanAttachEvaluatedBeyondFirstPage(t *testing.T) {
	srv, s, owner, member, projectID := msgAuthzSetup(t)
	ctx := context.Background()

	// Created first, so it is the oldest and sorts onto the last page.
	denied := &store.Agent{
		ID:        tid("pg-denied"),
		ProjectID: projectID,
		Name:      "pg-denied",
		Slug:      "pg-denied",
		Phase:     "running",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Ancestry:  []string{owner.ID},
	}
	if err := s.CreateAgent(ctx, denied); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	own := createSpaceMembersAgents(t, s, projectID, member.ID, "pg-own", 450)

	// Fixture guard: the denied agent and the oldest member-owned agent must
	// not be on the first store page, or this test proves nothing.
	first, err := s.ListAgents(ctx, store.AgentFilter{ProjectID: projectID}, store.ListOptions{Limit: projectAgentsPageSize})
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	for _, a := range first.Items {
		if a.ID == denied.ID || a.ID == own[0] {
			t.Fatalf("fixture: agent %s is on the first store page", a.ID)
		}
	}

	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/members", nil)
	resp := decodeSpaceMembers(t, rec.Code, rec.Body.Bytes())

	byID := make(map[string]chatMemberEntry, len(resp.Agents))
	for _, a := range resp.Agents {
		byID[a.ID] = a
	}
	if len(byID) != len(own)+1 {
		t.Fatalf("agents = %d, want %d", len(byID), len(own)+1)
	}
	d, ok := byID[denied.ID]
	if !ok {
		t.Fatal("denied agent missing from response")
	}
	if d.CanAttach {
		t.Error("canAttach = true for an agent the caller cannot attach to")
	}
	if o := byID[own[0]]; !o.CanAttach {
		t.Error("canAttach = false for the caller's own agent on a later page")
	}
}

// The safety cap stops the walk: no more agents than the cap are returned, no
// further store pages are read once it is reached, and the handler logs one
// warning naming the project.
func TestSpaceMembers_SafetyCapStopsWalk(t *testing.T) {
	srv, s := testServer(t)
	proj := createSpaceMembersProject(t, s, "members-paginate-cap")
	createSpaceMembersAgents(t, s, proj.ID, DevUserID, "pg-cap", 450)
	setSpaceMembersMaxAgents(t, 250)

	counting := &spaceMembersStore{Store: s}
	srv.store = counting
	logs := captureSpaceMembersLogs(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+proj.ID+"/members", nil)
	resp := decodeSpaceMembers(t, rec.Code, rec.Body.Bytes())

	if len(resp.Agents) != 250 {
		t.Fatalf("agents = %d, want the cap of 250", len(resp.Agents))
	}
	if counting.listAgentsCalls != 2 {
		t.Fatalf("ListAgents calls = %d, want 2 (walk must stop at the cap)", counting.listAgentsCalls)
	}
	if n := strings.Count(logs.String(), spaceMembersCapWarning); n != 1 {
		t.Fatalf("cap warnings = %d, want 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "project="+proj.ID) {
		t.Fatalf("cap warning does not name the project:\n%s", logs.String())
	}
}

// A project whose agent count equals the cap is not truncated, so the
// handler returns every agent and logs no warning.
func TestSpaceMembers_AgentCountEqualToCapDoesNotWarn(t *testing.T) {
	srv, s := testServer(t)
	proj := createSpaceMembersProject(t, s, "members-paginate-at-cap")
	createSpaceMembersAgents(t, s, proj.ID, DevUserID, "pg-at-cap", 250)
	setSpaceMembersMaxAgents(t, 250)
	logs := captureSpaceMembersLogs(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+proj.ID+"/members", nil)
	resp := decodeSpaceMembers(t, rec.Code, rec.Body.Bytes())

	if len(resp.Agents) != 250 {
		t.Fatalf("agents = %d, want 250", len(resp.Agents))
	}
	if strings.Contains(logs.String(), spaceMembersCapWarning) {
		t.Fatalf("unexpected cap warning:\n%s", logs.String())
	}
}

// A store failure must surface as a 500, not as a 200 with an empty or
// partial member list.
func TestSpaceMembers_StoreErrorReturns500(t *testing.T) {
	cases := []struct {
		name    string
		store   func(s store.Store) *spaceMembersStore
		message string
	}{
		{"agents first page", func(s store.Store) *spaceMembersStore {
			return &spaceMembersStore{Store: s, failListAgentsOnCall: 1}
		}, "failed to list project agents"},
		{"agents later page", func(s store.Store) *spaceMembersStore {
			return &spaceMembersStore{Store: s, failListAgentsOnCall: 2}
		}, "failed to list project agents"},
		{"project members", func(s store.Store) *spaceMembersStore {
			return &spaceMembersStore{Store: s, failProjectMembers: true}
		}, "failed to list project members"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			proj := createSpaceMembersProject(t, s, "members-paginate-err")
			createSpaceMembersAgents(t, s, proj.ID, DevUserID, "pg-err", 250)
			srv.store = tc.store(s)
			logs := captureSpaceMembersLogs(t)

			rec := doRequest(t, srv, http.MethodGet, "/api/v1/chat/spaces/"+proj.ID+"/members", nil)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
			}
			// The panic-recovery middleware also answers 500, so check that
			// this one came from the handler's own error path.
			var errResp ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
				t.Fatalf("decode error body: %v: %s", err, rec.Body.String())
			}
			if errResp.Error.Code != "INTERNAL" || errResp.Error.Message != tc.message {
				t.Fatalf("error = {%q, %q}, want {%q, %q}",
					errResp.Error.Code, errResp.Error.Message, "INTERNAL", tc.message)
			}
			if strings.Contains(logs.String(), "Panic recovered") {
				t.Fatalf("handler panicked instead of returning an error:\n%s", logs.String())
			}
		})
	}
}

// Once the request context is cancelled mid-loop, the handler must stop
// running per-agent attach checks and must not write a member list for a
// client that has gone away.
func TestSpaceMembers_CancelledContextStopsAttachChecks(t *testing.T) {
	const agentCount = 5
	srv, s := testServer(t)
	proj := createSpaceMembersProject(t, s, "members-paginate-cancel")
	createSpaceMembersAgents(t, s, proj.ID, DevUserID, "pg-cancel", agentCount)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel from inside the attach loop: on the first access decision made
	// after the agent walk has finished. The project read check before the
	// walk also makes a decision, so it must not count.
	walkDone := false
	attachReads := 0
	wrapped := &spaceMembersStore{
		Store:       s,
		onAgentPage: func(_ int, last bool) { walkDone = walkDone || last },
		onEffectiveGroups: func() {
			if !walkDone {
				return
			}
			attachReads++
			if attachReads == 1 {
				cancel()
			}
		},
	}
	srv.store = wrapped
	srv.authzService.store = wrapped

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/spaces/"+proj.ID+"/members", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if attachReads == 0 {
		t.Fatal("fixture: no access decision was made after the agent walk")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("expected no response body after cancellation, got %d: %s", rec.Code, rec.Body.String())
	}
	if attachReads >= agentCount {
		t.Fatalf("attach decisions after cancellation = %d, want fewer than %d", attachReads, agentCount)
	}
}

// A client that goes away during the agent walk is not a server failure: the
// handler must return without writing a 500 or logging an error.
func TestSpaceMembers_CancelledDuringWalkIsSilent(t *testing.T) {
	srv, s := testServer(t)
	proj := createSpaceMembersProject(t, s, "members-paginate-walk-cancel")
	createSpaceMembersAgents(t, s, proj.ID, DevUserID, "pg-walk-cancel", 250)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wrapped := &spaceMembersStore{Store: s, onAgentPage: func(call int, last bool) {
		if call == 1 && !last {
			cancel()
		}
	}}
	srv.store = wrapped
	logs := captureSpaceMembersLogs(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/spaces/"+proj.ID+"/members", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if ctx.Err() == nil {
		t.Fatal("fixture: context was not cancelled during the walk")
	}
	if wrapped.listAgentsCalls != 1 {
		t.Fatalf("ListAgents calls = %d, want 1 (walk must stop after cancellation)", wrapped.listAgentsCalls)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("expected no response body after cancellation, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(logs.String(), "failed to list project agents") {
		t.Fatalf("walk cancellation was logged as an error:\n%s", logs.String())
	}
}
