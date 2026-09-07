//go:build !no_sqlite

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

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// =============================================================================
// C2a tests: runtime-broker inbound sender status containment
//
// These tests prove that the runtime-broker inbound handler
// (handlers_broker_inbound.go) rejects senders whose user status is not
// active, with zero dispatch calls (the load-bearing assertion).
// =============================================================================

// c2aTestEnv sets up a project, a running agent with project message mode,
// and a dispatcher spy for C2a runtime-broker inbound tests.
type c2aTestEnv struct {
	srv     *Server
	store   store.Store
	spy     *containmentDispatchSpy
	project *store.Project
	agent   *store.Agent
}

func setupC2aEnv(t *testing.T) *c2aTestEnv {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	spy := &containmentDispatchSpy{}
	srv.SetDispatcher(spy)

	// Seed an owner user so the project has a valid owner.
	owner := &store.User{
		ID:      tid("c2a-owner"),
		Email:   "c2a-owner@example.com",
		Role:    store.UserRoleMember,
		Status:  store.UserStatusActive,
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	project := &store.Project{
		ID:        tid("c2a-proj"),
		Slug:      "c2a-proj",
		Name:      "C2a Test Project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)
	msgAuthzAddProjectMember(t, s, owner.ID, project.ID, project.Slug, store.GroupMemberRoleMember)

	agent := &store.Agent{
		ID:           tid("c2a-agent"),
		Slug:         "c2a-agent",
		Name:         "C2a Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		MessageMode:  store.MessageModeProject,
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	return &c2aTestEnv{srv: srv, store: s, spy: spy, project: project, agent: agent}
}

// c2aInboundRequest builds a broker inbound HTTP request for the given sender.
func (env *c2aTestEnv) c2aInboundRequest(t *testing.T, senderEmail string) *http.Request {
	t.Helper()
	topic := "scion.project." + env.project.ID + ".agent." + env.agent.Slug + ".messages"
	payload := inboundMessageRequest{
		Topic: topic,
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "discord",
			Sender:    "user:" + senderEmail,
			Recipient: "agent:" + env.agent.Slug,
			Msg:       "test message from broker",
			Type:      messages.TypeInstruction,
		},
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("c2a-test-broker")))
	return req
}

func TestC2a_RuntimeBrokerInbound_SuspendedSender(t *testing.T) {
	// A suspended user referenced as sender in a runtime-broker inbound
	// message must be rejected with 403, and zero dispatch calls must occur.
	env := setupC2aEnv(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("c2a-suspended"),
		Email:       "c2a-suspended@example.com",
		DisplayName: "Suspended Sender",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusSuspended,
		Created:     time.Now(),
	}
	require.NoError(t, env.store.CreateUser(ctx, user))
	ensureHubMembership(ctx, env.store, user.ID)
	msgAuthzAddProjectMember(t, env.store, user.ID, env.project.ID, env.project.Slug, store.GroupMemberRoleMember)

	rec := httptest.NewRecorder()
	env.srv.mux.ServeHTTP(rec, env.c2aInboundRequest(t, user.Email))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "sender identity is not active")

	// Load-bearing assertion: zero dispatch calls.
	assert.Empty(t, env.spy.getCalls(), "suspended sender must produce zero dispatch calls")
}

func TestC2a_RuntimeBrokerInbound_InvitedSender(t *testing.T) {
	// An invited user (non-standard non-active status) referenced as sender
	// must also be rejected, proving the fail-closed behavior of C2a.
	env := setupC2aEnv(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("c2a-invited"),
		Email:       "c2a-invited@example.com",
		DisplayName: "Invited Sender",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusInvited,
		Created:     time.Now(),
	}
	require.NoError(t, env.store.CreateUser(ctx, user))
	ensureHubMembership(ctx, env.store, user.ID)
	msgAuthzAddProjectMember(t, env.store, user.ID, env.project.ID, env.project.Slug, store.GroupMemberRoleMember)

	rec := httptest.NewRecorder()
	env.srv.mux.ServeHTTP(rec, env.c2aInboundRequest(t, user.Email))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "sender identity is not active")
	assert.Empty(t, env.spy.getCalls(), "invited sender must produce zero dispatch calls")
}

// Note: novel statuses (e.g. "deactivated") cannot be tested at the store
// layer because the ent schema restricts User.status to {"active",
// "suspended", "invited"}. The fail-closed behavior is proven by the invited
// test above: the C2a code uses `!= store.UserStatusActive`, so any future
// enum addition that is not "active" will also be denied. The ent enum
// constraint provides a second layer of defense — new statuses require a
// schema migration, at which point the C2a check is explicitly re-evaluated.

// =============================================================================
// C2b tests: runtime-broker on-behalf-of fail-closed status containment
//
// These tests exercise the BrokerAuthMiddleware on-behalf-of path
// (brokerauth.go) to prove that non-active statuses other than "suspended"
// are also denied by the C2b fail-closed logic.
// =============================================================================

func TestC2b_RuntimeBrokerOnBehalfOf_InvitedUser(t *testing.T) {
	// C2b fail-closed: an invited user must be denied, not just suspended.
	// This is the improvement over the original string-literal check.
	svc, s := setupTestBrokerAuthService(t)
	ctx := context.Background()
	_, signReq := setupSignedBrokerRequest(t, svc, s)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          uuid.New().String(),
		Email:       "invited@example.com",
		DisplayName: "Invited User",
		Role:        "member",
		Status:      store.UserStatusInvited,
	}))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Handler should not be called for invited user")
		w.WriteHeader(http.StatusOK)
	})

	wrapped := BrokerAuthMiddleware(svc)(handler)
	req := signReq(http.MethodPost, "/api/v1/test", map[string]string{
		HeaderOnBehalfOf: "user:invited@example.com",
	})
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "on-behalf-of principal is not active")
}

// Note: novel statuses cannot be tested via the ent-backed store (see C2a
// note above). The invited test proves the fail-closed pattern: C2b uses
// `!= store.UserStatusActive`, so both "suspended" (covered by existing
// TestBrokerAuthMiddleware_OnBehalfOf_SuspendedUser) and "invited" (above)
// are denied, and any future enum addition will also be denied.

// =============================================================================
// Characterization test: runtime-broker active-user identity assertion
//
// This test documents (not approves) the current behavior: a runtime-broker
// with valid HMAC can assert the identity of ANY active user via
// X-Scion-On-Behalf-Of, regardless of whether the user authorized the broker.
// This is an open finding (F-RS6-33), cross-linked to product decisions A-5
// and C2c. The test exists so that:
//   - the behavior is explicitly recorded, not silently relied upon, and
//   - any future restriction (allow-list, consent token) causes a clear test
//     failure, forcing an intentional decision rather than an accidental break.
// =============================================================================

func TestC2_Characterization_RuntimeBrokerCanAssertAnyActiveUser(t *testing.T) {
	// OPEN FINDING F-RS6-33: runtime-broker HMAC can assert any active user
	// identity without user consent. This test proves the current behavior
	// exists and records it as an explicit finding rather than a silent policy
	// approval. See authz-audit-findings.md F-RS6-33, product decisions A-5
	// and C2c.
	svc, s := setupTestBrokerAuthService(t)
	ctx := context.Background()
	_, signReq := setupSignedBrokerRequest(t, svc, s)

	// Create an active user who has NO relationship with this broker.
	arbitraryUser := &store.User{
		ID:          uuid.New().String(),
		Email:       "arbitrary-active@example.com",
		DisplayName: "Arbitrary Active User",
		Role:        "member",
		Status:      store.UserStatusActive,
	}
	require.NoError(t, s.CreateUser(ctx, arbitraryUser))

	var capturedIdentity Identity
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedIdentity = GetIdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	wrapped := BrokerAuthMiddleware(svc)(handler)
	req := signReq(http.MethodPost, "/api/v1/test", map[string]string{
		HeaderOnBehalfOf: "user:arbitrary-active@example.com",
	})
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)

	// CHARACTERIZATION: the request succeeds. The broker asserts an arbitrary
	// active user identity without that user's consent or knowledge.
	// This is the current behavior, NOT the desired policy.
	assert.Equal(t, http.StatusOK, w.Code,
		"CHARACTERIZATION: broker CAN currently assert any active user — "+
			"if this fails, a consent/allow-list mechanism was added (see F-RS6-33)")

	require.NotNil(t, capturedIdentity,
		"CHARACTERIZATION: identity should be set when broker asserts active user")
	assert.Equal(t, arbitraryUser.ID, capturedIdentity.ID(),
		"CHARACTERIZATION: asserted identity matches the arbitrary user")
	assert.Equal(t, "user", capturedIdentity.Type(),
		"CHARACTERIZATION: asserted identity type is 'user', not 'broker'")
}
