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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Broker registration, rotation, link and unlink audit events record the
// credential that carried the request next to the acting principal:
// credential_kind, credential_id (a token or broker ID, never secret
// material), credential_boundary_kind for a user access token, and, for
// registration, operation=register or operation=reregister.
// ============================================================================

func installBrokerAuditCapture(srv *Server) *mockAuditLogger {
	m := &mockAuditLogger{}
	srv.SetAuditLogger(m)
	return m
}

func brokerAuditEventsOfType(m *mockAuditLogger, eventType BrokerAuthEventType) []*BrokerAuthEvent {
	var out []*BrokerAuthEvent
	for _, e := range m.brokerEvents {
		if e.EventType == eventType {
			out = append(out, e)
		}
	}
	return out
}

// assertNoSecretInDetails fails when any detail value contains one of the
// given secret strings.
func assertNoSecretInDetails(t *testing.T, details map[string]string, secrets ...string) {
	t.Helper()
	for k, v := range details {
		for _, secret := range secrets {
			if secret == "" {
				continue
			}
			assert.NotContains(t, v, secret, "audit detail %q must not carry secret material", k)
		}
		assert.NotContains(t, v, "scion_pat_", "audit detail %q must not carry a token value", k)
		assert.NotContains(t, v, "scion_join_", "audit detail %q must not carry a join token", k)
	}
}

func TestBrokerAudit_HubTokenRegistrationRecordsCredential(t *testing.T) {
	srv, s := testServer(t)
	audit := installBrokerAuditCapture(srv)
	member := newHubMemberUser(t, s, "audit-hubtoken-member")
	key, token, err := srv.uatService.CreateTokenWithParams(rs4MintContext(member.ID), CreateTokenParams{
		UserID: member.ID, Name: "audit-hubtoken", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"broker:create"},
	})
	require.NoError(t, err)

	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name: "audit-hubtoken-broker",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := decodeBrokerRegistration(t, rec.Body)

	events := brokerAuditEventsOfType(audit, BrokerAuthEventRegister)
	require.Len(t, events, 1)
	e := events[0]
	assert.Equal(t, resp.BrokerID, e.BrokerID)
	assert.Equal(t, member.ID, e.ActorID)
	assert.Equal(t, "user", e.ActorType)
	assert.Equal(t, map[string]string{
		"credential_kind":          string(CredentialKindUAT),
		"credential_id":            token.ID,
		"credential_boundary_kind": string(BoundaryKindHub),
		"operation":                "register",
	}, e.Details)
	assertNoSecretInDetails(t, e.Details, key, token.KeyHash, resp.JoinToken)
}

func TestBrokerAudit_SessionReregistrationRecordsCredential(t *testing.T) {
	srv, s := testServer(t)
	audit := installBrokerAuditCapture(srv)
	owner := newHubMemberUser(t, s, "audit-session-owner")
	broker := createReregistrationTestBroker(t, s, "audit-session-broker", owner.ID)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name: broker.Name,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := decodeBrokerRegistration(t, rec.Body)

	events := brokerAuditEventsOfType(audit, BrokerAuthEventRegister)
	require.Len(t, events, 1)
	e := events[0]
	assert.Equal(t, broker.ID, e.BrokerID)
	assert.Equal(t, owner.ID, e.ActorID)
	assert.Equal(t, string(CredentialKindInteractive), e.Details["credential_kind"])
	assert.Equal(t, "reregister", e.Details["operation"])
	assert.NotContains(t, e.Details, "credential_boundary_kind", "only a user access token has a boundary")
	assertNoSecretInDetails(t, e.Details, resp.JoinToken)
}

func TestBrokerAudit_DeniedRegistrationRecordsNoEvent(t *testing.T) {
	srv, s := testServer(t)
	audit := installBrokerAuditCapture(srv)
	owner := newHubMemberUser(t, s, "audit-denied-owner")
	other := newHubMemberUser(t, s, "audit-denied-other")
	broker := createReregistrationTestBroker(t, s, "audit-denied-broker", owner.ID)

	rec := doRequestAsUser(t, srv, other, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name: broker.Name,
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Empty(t, brokerAuditEventsOfType(audit, BrokerAuthEventRegister))
}

func TestBrokerAudit_SessionRotationRecordsCredential(t *testing.T) {
	srv, s := testServer(t)
	audit := installBrokerAuditCapture(srv)
	owner := newHubMemberUser(t, s, "audit-rotate-owner")
	broker := createReregistrationTestBroker(t, s, "audit-rotate-broker", owner.ID)
	originalKey := seedBrokerSecret(t, s, broker.ID)

	rec := rotateSecretAsUser(t, srv, owner, broker.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	events := brokerAuditEventsOfType(audit, BrokerAuthEventRotate)
	require.Len(t, events, 1)
	e := events[0]
	assert.Equal(t, owner.ID, e.ActorID)
	assert.Equal(t, "user", e.ActorType)
	assert.Equal(t, string(CredentialKindInteractive), e.Details["credential_kind"])
	stored, err := s.GetBrokerSecret(context.Background(), broker.ID)
	require.NoError(t, err)
	assertNoSecretInDetails(t, e.Details, string(originalKey), string(stored.SecretKey))
}

func TestBrokerAudit_SelfRotationRecordsBrokerCredential(t *testing.T) {
	srv, s := testServer(t)
	audit := installBrokerAuditCapture(srv)
	broker, key := newOnboardingSigningBroker(t, s, "audit-self-rotate")

	rec := doBrokerSignedRequest(t, srv, broker.ID, key, "", http.MethodPost, "/api/v1/brokers/"+broker.ID+"/rotate-secret", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	events := brokerAuditEventsOfType(audit, BrokerAuthEventRotate)
	require.Len(t, events, 1)
	e := events[0]
	assert.Equal(t, broker.ID, e.ActorID)
	assert.Equal(t, "broker", e.ActorType)
	assert.Equal(t, map[string]string{
		"credential_kind": string(CredentialKindBroker),
		"credential_id":   broker.ID,
	}, e.Details)
	stored, err := s.GetBrokerSecret(context.Background(), broker.ID)
	require.NoError(t, err)
	assertNoSecretInDetails(t, e.Details, string(key), string(stored.SecretKey))
}

// A link uses a session (a project token cannot carry the broker
// side of the association); the unlink uses a project token that
// carries project:update. Each event records its own credential.
func TestBrokerAudit_SessionLinkAndProjectTokenUnlinkRecordCredential(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	audit := installBrokerAuditCapture(srv)
	projectID := tid("audit-link-project")
	ownerID := tid("audit-link-owner")
	createRS1Project(t, s, projectID, ownerID)
	owner, err := s.GetUser(ctx, ownerID)
	require.NoError(t, err)
	broker := &store.RuntimeBroker{
		ID: tid("audit-link-broker"), Name: "audit-link-broker", Slug: "audit-link-broker",
		Status: store.BrokerStatusOnline, CreatedBy: ownerID,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	key, token, err := srv.uatService.CreateTokenWithParams(rs4MintContext(ownerID), CreateTokenParams{
		UserID: ownerID, Name: "audit-link", ProjectID: projectID, Scopes: []string{"project:update"},
	})
	require.NoError(t, err)

	// The project token cannot link: no link event is recorded.
	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/projects/"+projectID+"/providers", AddProviderRequest{BrokerID: broker.ID})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Empty(t, brokerAuditEventsOfType(audit, BrokerAuthEventLink))

	rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+projectID+"/providers", AddProviderRequest{BrokerID: broker.ID})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	links := brokerAuditEventsOfType(audit, BrokerAuthEventLink)
	require.Len(t, links, 1)
	assert.Equal(t, broker.ID, links[0].BrokerID)
	assert.Equal(t, ownerID, links[0].ActorID)
	assert.Equal(t, map[string]string{
		"credential_kind": string(CredentialKindInteractive),
		"projectId":       projectID,
	}, links[0].Details)

	rec = doRequestWithToken(t, srv, key, http.MethodDelete, "/api/v1/projects/"+projectID+"/providers/"+broker.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	unlinks := brokerAuditEventsOfType(audit, BrokerAuthEventUnlink)
	require.Len(t, unlinks, 1)
	assert.Equal(t, ownerID, unlinks[0].ActorID)
	assert.Equal(t, map[string]string{
		"credential_kind":          string(CredentialKindUAT),
		"credential_id":            token.ID,
		"credential_boundary_kind": string(BoundaryKindProject),
		"projectId":                projectID,
	}, unlinks[0].Details)
	assertNoSecretInDetails(t, unlinks[0].Details, key, token.KeyHash)
}

func TestBrokerAuditCredentialDetails_NoCredential(t *testing.T) {
	assert.Empty(t, brokerAuditCredentialDetails(context.Background()))
}

func TestMergeBrokerAuditDetails(t *testing.T) {
	base := map[string]string{"credential_kind": "uat", "projectId": "caller-supplied"}
	got := mergeBrokerAuditDetails(base, "projectId", "p-1", "operation", "register")
	assert.Equal(t, map[string]string{"credential_kind": "uat", "projectId": "p-1", "operation": "register"}, got)
	assert.Equal(t, "caller-supplied", base["projectId"], "the input map is not modified")
	assert.Nil(t, mergeBrokerAuditDetails(nil))
	assert.Equal(t, map[string]string{"operation": "reregister"}, mergeBrokerAuditDetails(nil, "operation", "reregister"))
}

// assertSessionLinkEvent checks one link event recorded with an interactive
// session credential on the given path.
func assertSessionLinkEvent(t *testing.T, e *BrokerAuthEvent, brokerID, projectID, actorID, path string) {
	t.Helper()
	assert.Equal(t, brokerID, e.BrokerID)
	assert.Equal(t, actorID, e.ActorID)
	assert.Equal(t, "user", e.ActorType)
	assert.Equal(t, map[string]string{
		"credential_kind": string(CredentialKindInteractive),
		"projectId":       projectID,
		"path":            path,
	}, e.Details)
}

func TestBrokerAudit_ProjectRegisterLinkRecordsCredential(t *testing.T) {
	f := brokerAssocSetup(t, "audit-reg-link")
	audit := installBrokerAuditCapture(f.srv)

	rec := doRequestAsUser(t, f.srv, f.brokerOwner, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:     "Audit Register Link",
		BrokerID: f.otherBroker.ID,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	events := brokerAuditEventsOfType(audit, BrokerAuthEventLink)
	require.Len(t, events, 1)
	assertSessionLinkEvent(t, events[0], f.otherBroker.ID, resp.Project.ID, f.brokerOwner.ID, "project_register")
	assert.Empty(t, brokerAuditEventsOfType(audit, BrokerAuthEventRegister), "linking an existing broker registers nothing")
}

func TestBrokerAudit_ProjectRegisterDeniedLinkRecordsNoEvent(t *testing.T) {
	f := brokerAssocSetup(t, "audit-reg-link-denied")
	audit := installBrokerAuditCapture(f.srv)

	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:     "Audit Register Link Denied",
		BrokerID: f.otherBroker.ID,
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Empty(t, brokerAuditEventsOfType(audit, BrokerAuthEventLink))
}

func TestBrokerAudit_EmbeddedRegisterRecordsCredential(t *testing.T) {
	for _, tc := range []struct {
		name      string
		existing  bool
		operation string
	}{
		{"new broker", false, "register"},
		{"existing broker", true, "reregister"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			audit := installBrokerAuditCapture(srv)
			owner := newHubMemberUser(t, s, "audit-embedded-owner")
			info := &RegisterProjectBrokerInfo{Name: "audit-embedded-new-broker", Version: "1.0.0"}
			if tc.existing {
				broker := registerBrokerTestExistingBroker(t, s, "audit-embedded-broker", owner.ID)
				info = &RegisterProjectBrokerInfo{ID: broker.ID, Name: broker.Name, Version: "2.0.0"}
			}

			rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
				Name:   "Audit Embedded Project",
				Broker: info,
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp RegisterProjectResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			require.NotNil(t, resp.Broker)
			require.NotEmpty(t, resp.SecretKey)

			registered := brokerAuditEventsOfType(audit, BrokerAuthEventRegister)
			require.Len(t, registered, 1)
			e := registered[0]
			assert.Equal(t, resp.Broker.ID, e.BrokerID)
			assert.Equal(t, owner.ID, e.ActorID)
			assert.Equal(t, map[string]string{
				"credential_kind": string(CredentialKindInteractive),
				"operation":       tc.operation,
				"path":            "embedded",
			}, e.Details)
			assertNoSecretInDetails(t, e.Details, resp.SecretKey)

			links := brokerAuditEventsOfType(audit, BrokerAuthEventLink)
			require.Len(t, links, 1)
			assertSessionLinkEvent(t, links[0], resp.Broker.ID, resp.Project.ID, owner.ID, "embedded")
			assertNoSecretInDetails(t, links[0].Details, resp.SecretKey)
		})
	}
}

func TestBrokerAudit_AgentCreateLinkRecordsCredential(t *testing.T) {
	f := brokerLinkAuthzSetup(t)
	audit := installBrokerAuditCapture(f.srv)

	rec := createAgentAsOwner(t, f.bypassAgentsFixture, CreateAgentRequest{
		Name:            "audit-agent-link",
		RuntimeBrokerID: f.unlinked.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	events := brokerAuditEventsOfType(audit, BrokerAuthEventLink)
	require.Len(t, events, 1)
	assertSessionLinkEvent(t, events[0], f.unlinked.ID, f.proj.ID, f.owner.ID, "agent_create")
}

func TestBrokerAudit_BrokerDeleteUnlinkRecordsCredential(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	audit := installBrokerAuditCapture(srv)
	owner := newHubMemberUser(t, s, "audit-delete-owner")
	broker := createReregistrationTestBroker(t, s, "audit-delete-broker", owner.ID)

	projectIDs := []string{tid("audit-delete-project-a"), tid("audit-delete-project-b")}
	for i, projectID := range projectIDs {
		createRS1Project(t, s, projectID, tid("audit-delete-project-owner-"+string(rune('a'+i))))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: projectID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
		}))
	}

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete, "/api/v1/runtime-brokers/"+broker.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	events := brokerAuditEventsOfType(audit, BrokerAuthEventUnlink)
	require.Len(t, events, 2)
	var got []string
	for _, e := range events {
		assert.Equal(t, broker.ID, e.BrokerID)
		assert.Equal(t, owner.ID, e.ActorID)
		assert.Equal(t, string(CredentialKindInteractive), e.Details["credential_kind"])
		assert.NotContains(t, e.Details, "credential_boundary_kind")
		got = append(got, e.Details["projectId"])
	}
	assert.ElementsMatch(t, projectIDs, got, "one unlink event per provider link, each with its own project")
}
