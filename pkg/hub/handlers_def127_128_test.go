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
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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
