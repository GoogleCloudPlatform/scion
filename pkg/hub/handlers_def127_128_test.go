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

// Tests for DEF-127, DEF-127a, DEF-128a, and DEF-128b.
//
// These tests verify the read-path and fallback fixes:
//
//   DEF-128b — participant scoping on the Cloud Logging path
//   DEF-127  — never-used DM returns empty 200 (covered in handlers_read_switch_test.go)
//   DEF-127  — thread path still returns 409 (covered in handlers_read_switch_test.go)

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// DEF-127: S3a thread path MUST still return 409
// ---------------------------------------------------------------------------

func TestDEF127_ThreadPath_Still409(t *testing.T) {
	// S3a: thread conversations are created by the topic system. Absence
	// is genuine drift, not a normal first-use state. The 409 must be
	// preserved here (unlike DM sites which now return empty 200).
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	projectID := rsProject(t, s, "def127-thread-project")
	agentID := rsAgent(t, s, "def127-thread-agent", projectID)
	threadID := "thread-" + tid("def127-thread")

	// Request with thread_id → S3a path.
	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/agents/"+agentID+"/messages?thread_id="+threadID, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("S3a thread path: expected 409, got %d: %s",
			rec.Code, rec.Body.String())
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if errResp.Error.Code != ErrCodeConversationNotResolved {
		t.Errorf("expected error code %q, got %q",
			ErrCodeConversationNotResolved, errResp.Error.Code)
	}
}

// ---------------------------------------------------------------------------
// DEF-127: DM empty-200 counter verified across all three DM sites
// ---------------------------------------------------------------------------

func TestDEF127_DMAbsent_AllSitesIncrementCounter(t *testing.T) {
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	// S1: chat conversations endpoint
	agentUUID := tid("def127-counter-s1")
	key := makeDMKey(agentUUID, DevUserID)

	before := messaging.DMAbsentMetrics.Count()
	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/chat/conversations/"+key+"/messages", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("S1: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	delta := messaging.DMAbsentMetrics.Count() - before
	if delta != 1 {
		t.Errorf("S1: expected DMAbsentMetrics delta 1, got %d", delta)
	}

	// S2: messages endpoint with agent filter
	projectID := rsProject(t, s, "def127-counter-s2-project")
	agentID2 := rsAgent(t, s, "def127-counter-s2-agent", projectID)

	before = messaging.DMAbsentMetrics.Count()
	rec = doRequest(t, srv, http.MethodGet,
		"/api/v1/messages?agent="+agentID2, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("S2: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	delta = messaging.DMAbsentMetrics.Count() - before
	if delta != 1 {
		t.Errorf("S2: expected DMAbsentMetrics delta 1, got %d", delta)
	}

	// S3b: agent messages endpoint (DM default)
	agentID3 := rsAgent(t, s, "def127-counter-s3b-agent", projectID)

	before = messaging.DMAbsentMetrics.Count()
	rec = doRequest(t, srv, http.MethodGet,
		"/api/v1/agents/"+agentID3+"/messages", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("S3b: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	delta = messaging.DMAbsentMetrics.Count() - before
	if delta != 1 {
		t.Errorf("S3b: expected DMAbsentMetrics delta 1, got %d", delta)
	}
}

// ---------------------------------------------------------------------------
// DEF-127: empty response body verification
// ---------------------------------------------------------------------------

func TestDEF127_S1_EmptyResponseBody(t *testing.T) {
	srv, _ := testServer(t)
	enableReadSwitch(t, srv)

	agentUUID := tid("def127-empty-s1")
	key := makeDMKey(agentUUID, DevUserID)

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/chat/conversations/"+key+"/messages", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp chatHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Messages) != 0 {
		t.Errorf("expected 0 messages, got %d", len(resp.Messages))
	}
	if resp.TotalCount != 0 {
		t.Errorf("expected totalCount 0, got %d", resp.TotalCount)
	}
}

func TestDEF127_S3b_EmptyResponseBody(t *testing.T) {
	srv, s := testServer(t)
	enableReadSwitch(t, srv)

	projectID := rsProject(t, s, "def127-empty-s3b-project")
	agentID := rsAgent(t, s, "def127-empty-s3b-agent", projectID)

	rec := doRequest(t, srv, http.MethodGet,
		"/api/v1/agents/"+agentID+"/messages", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp store.ListResult[store.Message]
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Errorf("expected 0 items, got %d", len(resp.Items))
	}
}

// ---------------------------------------------------------------------------
// DEF-128b: BuildLogFilter participant scoping — mutation test
// ---------------------------------------------------------------------------

func TestDEF128b_ParticipantFilter_MutationTest(t *testing.T) {
	// This test demonstrates that removing the ParticipantID constraint
	// from BuildLogFilter causes a non-manage user's messages to be
	// unscoped. The test constructs the filter with and without a
	// ParticipantID to show the difference.

	// With ParticipantID: filter includes user-scoped constraint.
	withParticipant := BuildLogFilter(LogQueryOptions{
		AgentID:       "agent-123",
		ParticipantID: "user-456",
		LogID:         "scion-messages",
	}, "my-project")

	// Without ParticipantID: filter is agent-only (no user scoping).
	withoutParticipant := BuildLogFilter(LogQueryOptions{
		AgentID: "agent-123",
		LogID:   "scion-messages",
	}, "my-project")

	// The with-participant filter MUST be strictly longer (more constraints).
	if len(withParticipant) <= len(withoutParticipant) {
		t.Fatalf("participant filter did not add constraints:\n  with:    %q\n  without: %q",
			withParticipant, withoutParticipant)
	}

	// The participant ID MUST appear in the scoped filter.
	if want := `labels.recipient_id = "user-456"`; !strings.Contains(withParticipant, want) {
		t.Errorf("participant filter missing recipient_id constraint: %q", withParticipant)
	}
	if want := `labels.sender_id = "user-456"`; !strings.Contains(withParticipant, want) {
		t.Errorf("participant filter missing sender_id constraint: %q", withParticipant)
	}

	// The participant ID must NOT appear in the unscoped filter (mutation baseline).
	if strings.Contains(withoutParticipant, "user-456") {
		t.Errorf("unscoped filter should not contain user-456: %q", withoutParticipant)
	}
}

// ---------------------------------------------------------------------------
// DEF-128b defect #1: non-manage + no user identity → deny (fail closed)
// ---------------------------------------------------------------------------

// nonUserIdentity is a minimal Identity that does NOT implement UserIdentity.
// This simulates a caller kind (future or synthetic) that passes basic
// authentication but is not a user — testing the fail-closed guard.
type nonUserIdentity struct {
	id string
}

func (n *nonUserIdentity) ID() string   { return n.id }
func (n *nonUserIdentity) Type() string { return "synthetic" }

func TestDEF128b_NonManageNonUser_Denied(t *testing.T) {
	// A non-manage caller with no UserIdentity must be denied (403), not
	// served an unscoped query. This is the fail-closed invariant.
	srv, s := testServer(t)
	ctx := context.Background()

	// Create an agent to query.
	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "def128b-deny-project",
		Slug: "def128b-deny-project",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	agent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "def128b-deny-agent",
		Slug:      "def128b-deny-agent",
		ProjectID: project.ID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Construct a request with a non-user identity injected directly into
	// the context. This bypasses the auth middleware but lets us test the
	// handler's fail-closed guard in isolation.
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/agents/"+agent.ID+"/message-logs", nil)
	reqCtx := contextWithIdentity(req.Context(), &nonUserIdentity{id: "synth-001"})
	req = req.WithContext(reqCtx)

	rec := httptest.NewRecorder()
	srv.handleAgentMessageLogs(rec, req, agent.ID)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-manage + non-user: expected 403, got %d: %s",
			rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// MUT-A: fail-closed guard — agent identity (not UserIdentity) with read
// access reaches the guard; removing the guard changes 403 → 501.
// ---------------------------------------------------------------------------

func TestDEF128b_MutA_AgentNonUser_FailClosed(t *testing.T) {
	// This test catches MUT-A: reverting the fail-closed guard
	//   if user == nil { Forbidden(w); return }
	// to a no-op. An agent identity passes CheckAccess(ActionRead) via
	// the project read baseline but is NOT a UserIdentity. With the fix,
	// GetUserIdentityFromContext returns nil → 403. Without the fix, the
	// handler falls through to logQueryService (nil) → 501. The test
	// asserts 403, so MUT-A produces a red (501 ≠ 403).
	//
	// The Cloud Logging query is never issued because logQueryService is
	// nil — the distinction is purely between the guard (403) and the
	// nil service check (501).
	srv, s := testServer(t)
	ctx := context.Background()

	// srv.logQueryService is nil by default from testServer — the
	// handler's "not_implemented" path returns 501 if the guard is absent.

	// Create a project and agent.
	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "def128b-muta-project",
		Slug: "def128b-muta-project",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	agent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "def128b-muta-agent",
		Slug:      "def128b-muta-agent",
		ProjectID: project.ID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Build an agent identity in the SAME project with ScopeProjectRead.
	// This passes checkAgentReadScope and, via the "agent project read
	// baseline" in CheckAccess, passes ActionRead while failing ActionManage.
	// Crucially, GetUserIdentityFromContext returns nil for an agent.
	callerAgent := authzHelperAgent(project.ID, ScopeProjectRead)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/agents/"+agent.ID+"/message-logs", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), callerAgent))

	rec := httptest.NewRecorder()
	srv.handleAgentMessageLogs(rec, req, agent.ID)

	// With the fix: 403 (fail-closed guard fires).
	// With MUT-A:  501 (logQueryService == nil, guard deleted).
	if rec.Code != http.StatusForbidden {
		t.Fatalf("MUT-A: expected 403 from fail-closed guard, got %d: %s",
			rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// MUT-B: logAuthzDenial audit trail — removing the call must fail the test.
// ---------------------------------------------------------------------------

func TestDEF128b_MutB_DenialAuditLog(t *testing.T) {
	// This test catches MUT-B: deleting the logAuthzDenial(...) call in
	// handleAgentMessageLogs. A non-manage user who also fails ActionRead
	// must produce both a 403 AND a structured "authorization denied" log
	// record. Removing logAuthzDenial still returns 403 but drops the
	// audit record — the test's log assertion fails.
	srv, s := testServer(t)
	ctx := context.Background()

	buf := authzHelperCaptureLogs(t)

	// Create a project and agent.
	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "def128b-mutb-project",
		Slug: "def128b-mutb-project",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	agent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "def128b-mutb-agent",
		Slug:      "def128b-mutb-agent",
		ProjectID: project.ID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Use a member user: not admin, not owner, no policies granting read
	// on this agent. CheckAccess(ActionManage) and CheckAccess(ActionRead)
	// both deny. The handler should call logAuthzDenial then return 403.
	member := authzHelperMember()

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/agents/"+agent.ID+"/message-logs", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), member))

	rec := httptest.NewRecorder()
	srv.handleAgentMessageLogs(rec, req, agent.ID)

	// Assert 403.
	if rec.Code != http.StatusForbidden {
		t.Fatalf("MUT-B: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	// Assert the denial audit record was emitted (MUT-B deletes this call).
	denial := authzHelperDenialRecord(t, buf)
	if denial == nil {
		t.Fatalf("MUT-B: expected 'authorization denied' log record, got: %s",
			buf.String())
	}

	// Verify the record carries the correct context fields.
	for key, want := range map[string]any{
		"resource_type": "agent",
		"resource_id":   agent.ID,
		"action":        string(ActionRead),
	} {
		if got := denial[key]; got != want {
			t.Errorf("denial log %q = %v, want %v", key, got, want)
		}
	}
	if _, ok := denial["reason"]; !ok {
		t.Errorf("denial log missing 'reason' field: %v", denial)
	}
}

// ---------------------------------------------------------------------------
// DEF-128b defect #3: ParticipantID unconditional on LogID
// ---------------------------------------------------------------------------

func TestDEF128b_ParticipantFilter_Unconditional(t *testing.T) {
	// ParticipantID must be emitted regardless of LogID. An access-control
	// constraint conditional on a log-routing value is a silent drop.
	for _, logID := range []string{"scion-messages", "scion-agents", "scion-server", ""} {
		t.Run("LogID="+logID, func(t *testing.T) {
			result := BuildLogFilter(LogQueryOptions{
				AgentID:       "agent-123",
				ParticipantID: "user-456",
				LogID:         logID,
			})
			want := `(labels.recipient_id = "user-456" OR labels.sender_id = "user-456")`
			if !strings.Contains(result, want) {
				t.Errorf("ParticipantID filter missing for LogID=%q: %q", logID, result)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// DEF-128b mutation test: remove participant constraint → red
// ---------------------------------------------------------------------------

func TestDEF128b_MutationTest_Compiling(t *testing.T) {
	// Mutation: if BuildLogFilter ignores ParticipantID, the filter for a
	// scoped query equals the filter for an unscoped query. This test
	// catches that mutation.
	scoped := BuildLogFilter(LogQueryOptions{
		AgentID:       "agent-123",
		ParticipantID: "user-456",
		LogID:         "scion-messages",
	}, "my-project")

	unscoped := BuildLogFilter(LogQueryOptions{
		AgentID: "agent-123",
		LogID:   "scion-messages",
	}, "my-project")

	if scoped == unscoped {
		t.Fatalf("MUTATION DETECTED: scoped and unscoped filters are identical — "+
			"ParticipantID constraint is not being applied.\n  scoped:   %q\n  unscoped: %q",
			scoped, unscoped)
	}

	// The scoped filter MUST contain the participant constraint.
	if !strings.Contains(scoped, `labels.recipient_id = "user-456"`) {
		t.Errorf("scoped filter missing recipient_id: %q", scoped)
	}
	if !strings.Contains(scoped, `labels.sender_id = "user-456"`) {
		t.Errorf("scoped filter missing sender_id: %q", scoped)
	}

	// The unscoped filter must NOT contain the user ID.
	if strings.Contains(unscoped, "user-456") {
		t.Errorf("unscoped filter contains user-456: %q", unscoped)
	}
}
