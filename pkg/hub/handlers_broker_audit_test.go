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

func TestBrokerAudit_ProjectTokenLinkAndUnlinkRecordCredential(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	audit := installBrokerAuditCapture(srv)
	projectID := tid("audit-link-project")
	ownerID := tid("audit-link-owner")
	createRS1Project(t, s, projectID, ownerID)
	broker := &store.RuntimeBroker{
		ID: tid("audit-link-broker"), Name: "audit-link-broker", Slug: "audit-link-broker",
		Status: store.BrokerStatusOnline, CreatedBy: ownerID,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	key, token, err := srv.uatService.CreateTokenWithParams(rs4MintContext(ownerID), CreateTokenParams{
		UserID: ownerID, Name: "audit-link", ProjectID: projectID, Scopes: []string{"project:update"},
	})
	require.NoError(t, err)

	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/projects/"+projectID+"/providers", AddProviderRequest{BrokerID: broker.ID})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	want := map[string]string{
		"credential_kind":          string(CredentialKindUAT),
		"credential_id":            token.ID,
		"credential_boundary_kind": string(BoundaryKindProject),
		"projectId":                projectID,
	}
	links := brokerAuditEventsOfType(audit, BrokerAuthEventLink)
	require.Len(t, links, 1)
	assert.Equal(t, broker.ID, links[0].BrokerID)
	assert.Equal(t, ownerID, links[0].ActorID)
	assert.Equal(t, want, links[0].Details)
	assertNoSecretInDetails(t, links[0].Details, key, token.KeyHash)

	rec = doRequestWithToken(t, srv, key, http.MethodDelete, "/api/v1/projects/"+projectID+"/providers/"+broker.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	unlinks := brokerAuditEventsOfType(audit, BrokerAuthEventUnlink)
	require.Len(t, unlinks, 1)
	assert.Equal(t, ownerID, unlinks[0].ActorID)
	assert.Equal(t, want, unlinks[0].Details)
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
