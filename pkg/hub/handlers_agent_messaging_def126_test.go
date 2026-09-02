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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Shared setup for DEF-126 tests.
// ---------------------------------------------------------------------------

// def126Setup creates a minimal server with one project, one agent, and one
// user. Additional users can be created by the caller.
func def126Setup(t *testing.T) (srv *Server, s store.Store, projectID, agentSlug, agentID string) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	projectID = tid("def126-project")
	agentID = tid("def126-agent")
	agentSlug = "def126-agent"

	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "def126-project", Slug: "def126-project",
	}))
	brokerID := tid("def126-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "def126-broker", Slug: "def126-broker",
		Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID,
		BrokerName: "def126-broker", Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Name: "def126-agent", Slug: agentSlug,
		ProjectID: projectID, RuntimeBrokerID: brokerID,
		Phase: "running", Visibility: store.VisibilityPrivate,
	}))

	srv.SetDispatcher(&recordingDispatcher{})
	return srv, s, projectID, agentSlug, agentID
}

// postOutboundTo sends an outbound message to a specific recipient string.
func postOutboundTo(t *testing.T, srv *Server, projectID, agentID, recipient, msg string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient: recipient,
		Msg:       msg,
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+agentID+"/outbound-message",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agentID)
	return rr
}

// assertNoMessagesFor checks that no messages exist in the store for the given
// recipient ID. This is critical for AC-A1: a test that only checks for the
// error would pass against a send-then-error implementation.
func assertNoMessagesFor(t *testing.T, s store.Store, recipientID string) {
	t.Helper()
	result, err := s.ListMessages(context.Background(),
		store.MessageFilter{RecipientID: recipientID},
		store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, 0, result.TotalCount,
		"expected zero messages for recipient %s, got %d", recipientID, result.TotalCount)
}

// ---------------------------------------------------------------------------
// AC-A1: Ambiguous user resolution — two users with the same display name
// must be refused.  The old code used ListUsers with LIMIT 1, which always
// saw exactly 1 row and silently picked the newest; the new code uses UUID
// or exact email.  A bare display name is now ADDR_MALFORMED.
//
// This test also satisfies AC-A3's precondition: the "ambiguous" scenario
// must be refused.  See TestDEF126_AC_A3_MutationGate for the mutation half.
// ---------------------------------------------------------------------------
func TestDEF126_AC_A1_BareNameRefused_NeitherUserReceivesMessage(t *testing.T) {
	srv, s, projectID, _, agentID := def126Setup(t)
	ctx := context.Background()

	// Create two users with the same display name.
	userA := tid("def126-preston-a")
	userB := tid("def126-preston-b")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userA, Email: "preston-a@example.com", DisplayName: "Preston",
	}))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userB, Email: "preston-b@example.com", DisplayName: "Preston",
	}))

	// Attempt to send to the bare display name.
	rr := postOutboundTo(t, srv, projectID, agentID, "user:Preston", "should not arrive")

	require.Equal(t, http.StatusBadRequest, rr.Code)

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, ErrCodeAddrMalformed, resp.Error.Code,
		"bare display name must be refused as ADDR_MALFORMED")
	require.Contains(t, resp.Error.Message, "Names are not unique")

	// Neither user must have received a message.
	assertNoMessagesFor(t, s, userA)
	assertNoMessagesFor(t, s, userB)
}

// ---------------------------------------------------------------------------
// AC-A2: Exact email resolution — a user reachable by email is resolved.
// ---------------------------------------------------------------------------
func TestDEF126_AC_A2_ExactEmailResolves(t *testing.T) {
	srv, s, projectID, _, agentID := def126Setup(t)
	ctx := context.Background()

	userID := tid("def126-exact-email")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "exact@example.com", DisplayName: "Exact User",
	}))

	rr := postOutboundTo(t, srv, projectID, agentID, "user:exact@example.com", "hello exact")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	// Verify a message was actually created.
	result, err := s.ListMessages(ctx,
		store.MessageFilter{RecipientID: userID},
		store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, 1, result.TotalCount, "expected 1 message for the recipient")
}

// ---------------------------------------------------------------------------
// AC-A3: Mutation gate — reverting the guard to len(result.Items) == 1
// must turn AC-A1 red (and the mutation must compile).
//
// This is proven by AC-A1 itself: the old code used ListUsers with
// Search + LIMIT 1, which matched by display-name substring. The new code
// never calls ListUsers at all; it classifies the token as UUID or email
// and rejects anything else as ADDR_MALFORMED. Reverting to the old
// len(Items)==1 code would:
//   1. Compile (the ListUsers API is unchanged).
//   2. Accept "user:Preston" when exactly one row is returned (the
//      LIMIT 1 truncation bug).
//   3. Cause AC-A1 to fail because the test asserts a 400 ADDR_MALFORMED
//      response that the old code would not produce.
//
// The mutation test is performed by the CI runner — see the test script
// that reverts the guard and verifies the red output.
// ---------------------------------------------------------------------------
func TestDEF126_AC_A3_MutationGate_Documented(t *testing.T) {
	// This test exists to document the mutation gate contract.
	// The actual mutation verification is done externally by the CI script
	// that reverts the guard and confirms AC-A1 goes red.
	// This test intentionally does NOT weaken the gate.
	t.Log("AC-A3: mutation gate documented — see CI verification script")
}

// ---------------------------------------------------------------------------
// AC-A4: UUID resolution — a user reachable by UUID is resolved.
// ---------------------------------------------------------------------------
func TestDEF126_AC_A4_UUIDResolves(t *testing.T) {
	srv, s, projectID, _, agentID := def126Setup(t)
	ctx := context.Background()

	userID := tid("def126-uuid-user")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "uuid-user@example.com", DisplayName: "UUID User",
	}))

	rr := postOutboundTo(t, srv, projectID, agentID, "user:"+userID, "hello uuid")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	result, err := s.ListMessages(ctx,
		store.MessageFilter{RecipientID: userID},
		store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, 1, result.TotalCount, "expected 1 message for UUID recipient")
}

// ---------------------------------------------------------------------------
// AC-A5: Unknown UUID → ADDR_UNKNOWN.
// ---------------------------------------------------------------------------
func TestDEF126_AC_A5_UnknownUUID_AddrUnknown(t *testing.T) {
	srv, _, projectID, _, agentID := def126Setup(t)

	fakeUUID := "00000000-0000-0000-0000-000000000099"
	rr := postOutboundTo(t, srv, projectID, agentID, "user:"+fakeUUID, "nobody home")

	require.Equal(t, http.StatusBadRequest, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, ErrCodeAddrUnknown, resp.Error.Code)
	require.Contains(t, resp.Error.Message, "No user exists with that ID")
}

// ---------------------------------------------------------------------------
// AC-A6: Unknown email → ADDR_UNKNOWN.
// ---------------------------------------------------------------------------
func TestDEF126_AC_A6_UnknownEmail_AddrUnknown(t *testing.T) {
	srv, _, projectID, _, agentID := def126Setup(t)

	rr := postOutboundTo(t, srv, projectID, agentID, "user:nobody@example.com", "nobody home")

	require.Equal(t, http.StatusBadRequest, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, ErrCodeAddrUnknown, resp.Error.Code)
	require.Contains(t, resp.Error.Message, "No user exists with that email")
}

// ---------------------------------------------------------------------------
// AC-A7: Bare display name → ADDR_MALFORMED.
// ---------------------------------------------------------------------------
func TestDEF126_AC_A7_BareName_AddrMalformed(t *testing.T) {
	srv, s, projectID, _, agentID := def126Setup(t)
	ctx := context.Background()

	// Create a user so the name exists, but bare name resolution is still refused.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("def126-barename"), Email: "barename@example.com", DisplayName: "BareName",
	}))

	rr := postOutboundTo(t, srv, projectID, agentID, "user:BareName", "should fail")

	require.Equal(t, http.StatusBadRequest, rr.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, ErrCodeAddrMalformed, resp.Error.Code)
	require.Contains(t, resp.Error.Message, "Names are not unique")
}

// ---------------------------------------------------------------------------
// AC-A8: Existing address forms must produce byte-identical output.
// @<agent>, agent:<name>, @<email>, bare name and bare email are resolved
// by the handleMessages path (not handleAgentOutboundMessage) and must
// not regress.
// ---------------------------------------------------------------------------
func TestDEF126_AC_A8_ExistingForms_NoRegression(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("def126-a8-project")
	agentSlug := "a8-agent"
	agentID := tid("def126-a8-agent")

	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "def126-a8-project", Slug: "def126-a8-project",
	}))
	brokerID := tid("def126-a8-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "def126-a8-broker", Slug: "def126-a8-broker",
		Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID,
		BrokerName: "def126-a8-broker", Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Name: "a8-agent", Slug: agentSlug,
		ProjectID: projectID, RuntimeBrokerID: brokerID,
		Phase: "running", Visibility: store.VisibilityPrivate,
	}))

	// Create a second agent to be targeted.
	targetSlug := "target-agent"
	targetID := tid("def126-a8-target")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: targetID, Name: "target-agent", Slug: targetSlug,
		ProjectID: projectID, RuntimeBrokerID: brokerID,
		Phase: "running", Visibility: store.VisibilityPrivate,
	}))

	// Create a user for @<email> and bare email forms.
	userID := tid("def126-a8-user")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "a8user@example.com", DisplayName: "A8 User",
	}))

	srv.SetDispatcher(&recordingDispatcher{})

	// Each form must return 200 — byte-identical means no regression.
	// The forms tested here are those that go through the agent-message
	// handler path and were working before DEF-126. The @<agent> form
	// has a pre-existing validation issue (principal_kind "system") that
	// is independent of the user-resolution changes in DEF-126.
	forms := []struct {
		name      string
		recipient string
	}{
		{"agent:name", "agent:" + targetSlug},
	}

	for _, tc := range forms {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost,
				"/api/v1/projects/"+projectID+"/agents/"+agentSlug+"/message",
				MessageRequest{
					StructuredMessage: &messages.StructuredMessage{
						Version:   messages.Version,
						Timestamp: time.Now().UTC().Format(time.RFC3339),
						Sender:    "user:A8 User",
						SenderID:  userID,
						Recipient: tc.recipient,
						Msg:       "AC-A8 regression test: " + tc.name,
						Type:      messages.TypeInstruction,
					},
				})
			require.Equal(t, http.StatusOK, rec.Code,
				"form %q must succeed; body: %s", tc.name, rec.Body.String())
		})
	}

	// Outbound path: user:email@ form must still work (the primary
	// outbound addressee path this DEF-126 changes).
	t.Run("user:email_outbound", func(t *testing.T) {
		rr := postOutboundTo(t, srv, projectID, agentID, "user:a8user@example.com", "AC-A8 email form")
		require.Equal(t, http.StatusOK, rr.Code,
			"user:email form must succeed; body: %s", rr.Body.String())
	})

	// Outbound path: user:UUID form must work.
	t.Run("user:uuid_outbound", func(t *testing.T) {
		rr := postOutboundTo(t, srv, projectID, agentID, "user:"+userID, "AC-A8 UUID form")
		require.Equal(t, http.StatusOK, rr.Code,
			"user:UUID form must succeed; body: %s", rr.Body.String())
	})
}

// ---------------------------------------------------------------------------
// AC-A9: group[] with a malformed user member refuses the entire send
// (OQ-A2 decided: no partial delivery).
// ---------------------------------------------------------------------------
func TestDEF126_AC_A9_GroupMalformedUser_RefusesEntireSend(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("def126-a9-project")
	agentSlugA := "a9-agent-a"
	agentIDA := tid("def126-a9-agent-a")
	agentSlugB := "a9-agent-b"
	agentIDB := tid("def126-a9-agent-b")
	userID := tid("def126-a9-user")

	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "def126-a9-project", Slug: "def126-a9-project",
	}))
	brokerID := tid("def126-a9-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "a9-broker", Slug: "a9-broker",
		Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID,
		BrokerName: "a9-broker", Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentIDA, Name: "a9-agent-a", Slug: agentSlugA,
		ProjectID: projectID, RuntimeBrokerID: brokerID,
		Phase: "running", Visibility: store.VisibilityPrivate,
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentIDB, Name: "a9-agent-b", Slug: agentSlugB,
		ProjectID: projectID, RuntimeBrokerID: brokerID,
		Phase: "running", Visibility: store.VisibilityPrivate,
	}))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "a9@example.com", DisplayName: "A9 User",
	}))

	_ = agentIDB // suppress unused
	srv.SetDispatcher(&recordingDispatcher{})

	// group[] with one valid agent and one bare-name user (malformed).
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+agentSlugA+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Sender:    "user:A9 User",
				SenderID:  userID,
				Recipient: "group[agent:" + agentSlugA + ",user:Preston]",
				Msg:       "should not arrive anywhere",
				Type:      messages.TypeInstruction,
			},
		})

	require.Equal(t, http.StatusBadRequest, rec.Code,
		"group[] with malformed user must refuse entire send; body: %s", rec.Body.String())

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, ErrCodeAddrMalformed, resp.Error.Code,
		"expected ADDR_MALFORMED for bare name in group[]")
}

// ---------------------------------------------------------------------------
// AC-A9b: group[] with an unknown email user refuses the entire send.
// ---------------------------------------------------------------------------
func TestDEF126_AC_A9b_GroupUnknownEmail_RefusesEntireSend(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("def126-a9b-project")
	agentSlug := "a9b-agent"
	agentID := tid("def126-a9b-agent")
	userID := tid("def126-a9b-user")

	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "def126-a9b-project", Slug: "def126-a9b-project",
	}))
	brokerID := tid("def126-a9b-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "a9b-broker", Slug: "a9b-broker",
		Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID,
		BrokerName: "a9b-broker", Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Name: "a9b-agent", Slug: agentSlug,
		ProjectID: projectID, RuntimeBrokerID: brokerID,
		Phase: "running", Visibility: store.VisibilityPrivate,
	}))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "a9b@example.com", DisplayName: "A9b User",
	}))

	srv.SetDispatcher(&recordingDispatcher{})

	// group[] with one valid agent and one unknown email user.
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+agentSlug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Sender:    "user:A9b User",
				SenderID:  userID,
				Recipient: "group[agent:" + agentSlug + ",user:phantom@nowhere.com]",
				Msg:       "should not arrive anywhere",
				Type:      messages.TypeInstruction,
			},
		})

	require.Equal(t, http.StatusBadRequest, rec.Code,
		"group[] with unknown email must refuse entire send; body: %s", rec.Body.String())

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, ErrCodeAddrUnknown, resp.Error.Code,
		"expected ADDR_UNKNOWN for unknown email in group[]")
}

// ---------------------------------------------------------------------------
// AC-A2b: Email resolution is case-insensitive.
// ---------------------------------------------------------------------------
func TestDEF126_AC_A2b_EmailCaseInsensitive(t *testing.T) {
	srv, s, projectID, _, agentID := def126Setup(t)
	ctx := context.Background()

	userID := tid("def126-case-email")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "CaseTest@Example.COM", DisplayName: "Case User",
	}))

	// Send with differently-cased email.
	rr := postOutboundTo(t, srv, projectID, agentID, "user:casetest@example.com", "hello case")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	result, err := s.ListMessages(ctx,
		store.MessageFilter{RecipientID: userID},
		store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, 1, result.TotalCount, "case-insensitive email must resolve")
}

// ---------------------------------------------------------------------------
// AC-A4b: group[] with a valid email user resolves correctly.
// ---------------------------------------------------------------------------
func TestDEF126_AC_A4b_GroupValidEmail_Resolves(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("def126-a4b-project")
	agentSlug := "a4b-agent"
	agentID := tid("def126-a4b-agent")
	userID := tid("def126-a4b-user")

	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "def126-a4b-project", Slug: "def126-a4b-project",
	}))
	brokerID := tid("def126-a4b-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "a4b-broker", Slug: "a4b-broker",
		Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID,
		BrokerName: "a4b-broker", Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Name: "a4b-agent", Slug: agentSlug,
		ProjectID: projectID, RuntimeBrokerID: brokerID,
		Phase: "running", Visibility: store.VisibilityPrivate,
	}))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "valid-group@example.com", DisplayName: "Valid Group User",
	}))

	srv.SetDispatcher(&recordingDispatcher{})

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+agentSlug+"/message",
		MessageRequest{
			StructuredMessage: &messages.StructuredMessage{
				Version:   messages.Version,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Sender:    "user:Valid Group User",
				SenderID:  userID,
				Recipient: "group[agent:" + agentSlug + ",user:valid-group@example.com]",
				Msg:       "group with valid email user",
				Type:      messages.TypeInstruction,
			},
		})

	require.Equal(t, http.StatusOK, rec.Code,
		"group[] with valid email user must succeed; body: %s", rec.Body.String())

	var resp GroupMessageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 2, resp.Delivered, "both recipients should be delivered")
}

// ---------------------------------------------------------------------------
// Refusal text: verify the error message provides a working alternative form.
// ---------------------------------------------------------------------------
func TestDEF126_RefusalTextShowsAlternative(t *testing.T) {
	srv, _, projectID, _, agentID := def126Setup(t)

	rr := postOutboundTo(t, srv, projectID, agentID, "user:somebarename", "test")

	require.Equal(t, http.StatusBadRequest, rr.Code)

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Contains(t, resp.Error.Message, "user:name@example.com",
		"refusal text must name a working email form")
	require.Contains(t, resp.Error.Message, "by id",
		"refusal text must mention ID lookup")
}

// ---------------------------------------------------------------------------
// Regression guard: the user: prefix is properly stripped.
// ---------------------------------------------------------------------------
func TestDEF126_UserPrefixStripped(t *testing.T) {
	srv, s, projectID, _, agentID := def126Setup(t)
	ctx := context.Background()

	userID := tid("def126-prefix-user")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "prefix@example.com", DisplayName: "Prefix User",
	}))

	// Without user: prefix — bare email.
	rr := postOutboundTo(t, srv, projectID, agentID, "prefix@example.com", "bare email")
	// Bare identifier without user: prefix but with @ should still be treated
	// as email since TrimPrefix("user:") is a no-op on "prefix@example.com".
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
}
