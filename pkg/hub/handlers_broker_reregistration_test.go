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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Broker ownership-gate tests: re-registration and secret rotation
//
// POST /api/v1/brokers matches an existing broker by name or by a
// caller-supplied ID and, on a match, updates that record and issues a new
// join token. POST /api/v1/brokers/{id}/rotate-secret replaces a broker's
// HMAC secret. Both require broker ownership, checked by
// authorizedForBrokerOwnerAction (handlers_brokers.go): the caller must be
// a system-scoped super-admin, be the broker itself (HMAC), or be the user
// recorded as the broker's creator. Holding the broker.read catalog
// permission alone does not satisfy this check for either action. A
// brand-new registration (no existing match) remains open to any
// authenticated user.
// ============================================================================

// grantSystemBrokerReadPermission binds a dedicated role granting only
// broker.read (system scope) to the given user, independent of hub-member
// catalog membership. Used to confirm that this permission by itself does
// not satisfy either ownership-gated action.
func grantSystemBrokerReadPermission(t *testing.T, s store.Store, userID string) {
	t.Helper()
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, "broker-read-only-compat", store.RoleScopeSystem)
	if err != nil {
		rd, err = s.CreateRoleDefinition(ctx, &store.RoleDefinition{
			Name:        "broker-read-only-compat",
			ScopeType:   store.RoleScopeSystem,
			Permissions: []string{"broker.read"},
		})
		require.NoError(t, err)
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("failed to create broker-read-only-compat role binding: %v", err)
	}
}

// newPlainUser creates a bare "member"-role user with no group memberships
// and no role bindings — no hub-members catalog access, no broker
// permission of any kind. This represents the boundary the gate actually
// enforces: a caller who is authenticated but holds none of the allowed
// grants (super-admin, broker-self, or CreatedBy).
func newPlainUser(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	u := &store.User{
		ID:          tid(name),
		Email:       name + "@test.com",
		DisplayName: name,
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

// newHubMemberUser creates an ordinary hub-members-group member, exercising
// the real curated hub-member role (which carries broker.read) rather than
// the synthetic single-permission role from grantSystemBrokerReadPermission.
func newHubMemberUser(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	u := newPlainUser(t, s, name)
	ensureHubMembership(context.Background(), s, u.ID)
	return u
}

// newSuperAdminUser creates a user with a system-scoped super-admin role
// binding, independent of hub-members catalog membership.
func newSuperAdminUser(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	userID := tid(name)
	createTestUserWithRole(t, s, userID, name+"@test.com", "admin", store.SystemRoleSuperAdmin)
	u, err := s.GetUser(context.Background(), userID)
	require.NoError(t, err)
	return u
}

// createReregistrationTestBroker inserts a broker directly into the store,
// skipping HTTP registration, so no join token exists for it yet.
func createReregistrationTestBroker(t *testing.T, s store.Store, name, createdBy string) *store.RuntimeBroker {
	t.Helper()
	broker := &store.RuntimeBroker{
		ID:          tid("reregistration-broker-" + name),
		Name:        name,
		Slug:        slugify(name),
		Status:      store.BrokerStatusOffline,
		AutoProvide: false,
		Labels:      map[string]string{"env": "baseline"},
		Created:     time.Now(),
		Updated:     time.Now(),
		CreatedBy:   createdBy,
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), broker))
	return broker
}

// assertBrokerUnchanged re-reads the broker and confirms none of the
// registration-mutable fields moved, and that no join token was created.
func assertBrokerUnchanged(t *testing.T, s store.Store, brokerID string) {
	t.Helper()
	ctx := context.Background()

	broker, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	assert.False(t, broker.AutoProvide, "AutoProvide must not be flipped by a denied re-registration")
	assert.Equal(t, "baseline", broker.Labels["env"], "labels must not be overwritten by a denied re-registration")
	assert.Empty(t, broker.GCPHostServiceAccountEmail, "GCP host identity must not be set by a denied re-registration")

	_, err = s.GetJoinTokenByBrokerID(ctx, brokerID)
	assert.True(t, errors.Is(err, store.ErrNotFound), "no join token should exist after a denied re-registration; got err=%v", err)
}

// seedBrokerSecret creates an active HMAC secret for brokerID directly in
// the store and returns the key bytes, so rotate-secret tests can assert
// whether that value moved.
func seedBrokerSecret(t *testing.T, s store.Store, brokerID string) []byte {
	t.Helper()
	key := []byte("test-fixture-secret-key-0123456789ab")
	require.NoError(t, s.CreateBrokerSecret(context.Background(), &store.BrokerSecret{
		BrokerID:  brokerID,
		SecretKey: key,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		CreatedAt: time.Now(),
		Status:    store.BrokerSecretStatusActive,
	}))
	return key
}

// ----------------------------------------------------------------------------
// Re-registration
// ----------------------------------------------------------------------------

func TestBrokerReregistration_NonOwnerByNameDenied(t *testing.T) {
	srv, s := testServer(t)
	owner := newPlainUser(t, s, "reregistration-owner-a")
	nonOwner := newPlainUser(t, s, "reregistration-nonowner-a")
	broker := createReregistrationTestBroker(t, s, "reregistration-broker-name-a", owner.ID)

	rec := doRequestAsUser(t, srv, nonOwner, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:        broker.Name,
		AutoProvide: true,
		Labels:      map[string]string{"env": "requested"},
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-owner re-registering by name should be denied; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "joinToken", "denied response must not carry a join token")
	assertBrokerUnchanged(t, s, broker.ID)
}

func TestBrokerReregistration_NonOwnerByIDDenied(t *testing.T) {
	srv, s := testServer(t)
	owner := newPlainUser(t, s, "reregistration-owner-b")
	nonOwner := newPlainUser(t, s, "reregistration-nonowner-b")
	broker := createReregistrationTestBroker(t, s, "reregistration-broker-id-b", owner.ID)

	rec := doRequestAsUser(t, srv, nonOwner, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		BrokerID:    broker.ID,
		Name:        "a different requested name",
		AutoProvide: true,
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-owner re-registering by caller-supplied ID should be denied; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "joinToken", "denied response must not carry a join token")
	assertBrokerUnchanged(t, s, broker.ID)
}

// TestBrokerReregistration_BrokerReadOnlyMemberDenied confirms that a user
// holding only the broker.read permission — and none of CreatedBy,
// super-admin, or broker-self — is denied re-registration.
func TestBrokerReregistration_BrokerReadOnlyMemberDenied(t *testing.T) {
	srv, s := testServer(t)
	owner := newPlainUser(t, s, "reregistration-owner-readonly")
	reader := newPlainUser(t, s, "reregistration-reader-readonly")
	grantSystemBrokerReadPermission(t, s, reader.ID)
	broker := createReregistrationTestBroker(t, s, "reregistration-broker-readonly", owner.ID)

	rec := doRequestAsUser(t, srv, reader, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:        broker.Name,
		AutoProvide: true,
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"broker.read alone must not authorize re-registration; got: %s", rec.Body.String())
	assertBrokerUnchanged(t, s, broker.ID)
}

// TestBrokerReregistration_OrdinaryHubMemberDenied is the same check as
// BrokerReadOnlyMemberDenied above, but against a real hub-members-group
// member (the curated hub-member role, not a synthetic single-permission
// role) to confirm the ordinary account shape is also denied.
func TestBrokerReregistration_OrdinaryHubMemberDenied(t *testing.T) {
	srv, s := testServer(t)
	owner := newPlainUser(t, s, "reregistration-owner-hubmember")
	member := newHubMemberUser(t, s, "reregistration-member-hubmember")
	broker := createReregistrationTestBroker(t, s, "reregistration-broker-hubmember", owner.ID)

	rec := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:        broker.Name,
		AutoProvide: true,
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"an ordinary hub member should be denied re-registration of a broker they did not create; got: %s", rec.Body.String())
	assertBrokerUnchanged(t, s, broker.ID)
}

func TestBrokerReregistration_OwnerAllowed(t *testing.T) {
	srv, s := testServer(t)
	owner := newPlainUser(t, s, "reregistration-owner-c")
	broker := createReregistrationTestBroker(t, s, "reregistration-broker-owner-c", owner.ID)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:        broker.Name,
		AutoProvide: true,
		Labels:      map[string]string{"env": "updated"},
	})

	require.Equal(t, http.StatusCreated, rec.Code,
		"owner re-registering their own broker should succeed; got: %s", rec.Body.String())

	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, broker.ID, resp.BrokerID)
	assert.NotEmpty(t, resp.JoinToken, "owner re-registration should re-issue a join token")
	assert.True(t, resp.Reregistered)

	updated, err := s.GetRuntimeBroker(context.Background(), broker.ID)
	require.NoError(t, err)
	assert.True(t, updated.AutoProvide, "owner re-registration should apply the requested fields")
	assert.Equal(t, "updated", updated.Labels["env"])
}

func TestBrokerReregistration_SuperAdminAllowed(t *testing.T) {
	srv, s := testServer(t)
	owner := newPlainUser(t, s, "reregistration-owner-d")
	admin := newSuperAdminUser(t, s, "reregistration-admin-d")
	broker := createReregistrationTestBroker(t, s, "reregistration-broker-admin-d", owner.ID)

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:        broker.Name,
		AutoProvide: true,
	})

	require.Equal(t, http.StatusCreated, rec.Code,
		"a super-admin should be able to re-register a broker they did not create; got: %s", rec.Body.String())

	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.JoinToken)

	updated, err := s.GetRuntimeBroker(context.Background(), broker.ID)
	require.NoError(t, err)
	assert.True(t, updated.AutoProvide)
}

func TestBrokerReregistration_NewRegistrationStillOpenToAnyUser(t *testing.T) {
	srv, s := testServer(t)
	requester := newPlainUser(t, s, "reregistration-newuser-e")

	rec := doRequestAsUser(t, srv, requester, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name: "a brand new broker name never seen before",
	})

	require.Equal(t, http.StatusCreated, rec.Code,
		"first-time registration by any authenticated user should still succeed; got: %s", rec.Body.String())

	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.JoinToken)
	assert.False(t, resp.Reregistered)

	created, err := s.GetRuntimeBroker(context.Background(), resp.BrokerID)
	require.NoError(t, err)
	assert.Equal(t, requester.ID, created.CreatedBy, "the requester should become the new broker's owner")
}

// TestBrokerReregistration_OwnerlessBrokerDenied covers the record shape left
// behind by an embedded/co-located broker: registerGlobalProjectAndBroker
// (cmd/server_broker.go) writes and updates that broker record directly
// through the store, never through this HTTP handler, so it never populates
// CreatedBy. Confirmed by inspection: it only calls store methods
// (GetRuntimeBroker/CreateRuntimeBroker/UpdateRuntimeBroker) and has no
// dependency on BrokerAuthService.CreateBrokerRegistration or on any HTTP
// request context, so this gate does not run on that path at all — the
// embedded broker's own restart-time self-reassertion is unaffected either
// way. What the gate must still do is reject an ordinary user who tries to
// claim that same (CreatedBy == "") record over HTTP.
func TestBrokerReregistration_OwnerlessBrokerDenied(t *testing.T) {
	srv, s := testServer(t)
	nonOwner := newPlainUser(t, s, "reregistration-nonowner-f")
	broker := createReregistrationTestBroker(t, s, "reregistration-broker-ownerless-f", "" /* CreatedBy unset, as for an embedded broker */)

	rec := doRequestAsUser(t, srv, nonOwner, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:        broker.Name,
		AutoProvide: true,
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"an ownerless broker record must still reject a non-owner re-registration; got: %s", rec.Body.String())
	assertBrokerUnchanged(t, s, broker.ID)
}

// TestWriteBrokerRegistrationError_StalePinMapsToConflict and
// TestWriteBrokerRegistrationError_OtherErrorMapsToInternalError cover
// writeBrokerRegistrationError's status-code mapping directly. Driving
// ErrBrokerRegistrationAuthorizationStale through the full HTTP handler
// would require winning a real lookup race between the authorization check
// and the mutation (brokerauth_test.go exercises the pin logic itself by
// calling the pinned methods directly with a deliberately stale pin); there
// is no seam to force that race deterministically through the handler, so
// this asserts the handler's error-mapping helper directly instead.
func TestWriteBrokerRegistrationError_StalePinMapsToConflict(t *testing.T) {
	rec := httptest.NewRecorder()
	writeBrokerRegistrationError(rec, ErrBrokerRegistrationAuthorizationStale)
	assert.Equal(t, http.StatusConflict, rec.Code,
		"a stale authorization pin should map to 409 Conflict; got: %s", rec.Body.String())
}

func TestWriteBrokerRegistrationError_OtherErrorMapsToInternalError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeBrokerRegistrationError(rec, errors.New("some other failure"))
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"a non-sentinel error should still map to 500; got: %s", rec.Body.String())
}

// ----------------------------------------------------------------------------
// Secret rotation (handleBrokerRotateSecret) — same ownership gate as
// re-registration.
// ----------------------------------------------------------------------------

func rotateSecretAsUser(t *testing.T, srv *Server, user *store.User, brokerID string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	if user == nil {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/brokers/"+brokerID+"/rotate-secret", nil)
		srv.handleBrokerRotateSecret(rec, req, brokerID)
		return rec
	}
	return doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/brokers/"+brokerID+"/rotate-secret", nil)
}

// TestBrokerRotateSecret_NonOwnerBrokerReadOnlyDenied confirms that a user
// holding only the broker.read permission — and none of CreatedBy,
// super-admin, or broker-self — is denied secret rotation, and that the
// stored secret is left unchanged.
func TestBrokerRotateSecret_NonOwnerBrokerReadOnlyDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newPlainUser(t, s, "rotate-owner-readonly")
	reader := newPlainUser(t, s, "rotate-reader-readonly")
	grantSystemBrokerReadPermission(t, s, reader.ID)
	broker := &store.RuntimeBroker{ID: tid("rotate-broker-readonly"), Name: "Rotate Broker Readonly", Slug: "rotate-broker-readonly", CreatedBy: owner.ID}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	originalKey := seedBrokerSecret(t, s, broker.ID)

	rec := rotateSecretAsUser(t, srv, reader, broker.ID)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"broker.read alone must not authorize secret rotation; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "secretKey", "denied response must not carry a secret")

	stored, err := s.GetBrokerSecret(ctx, broker.ID)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(originalKey, stored.SecretKey), "the stored secret must not change on a denied rotation")
}

// TestBrokerRotateSecret_OrdinaryHubMemberDenied is the same check as
// NonOwnerBrokerReadOnlyDenied above, but against a real hub-members-group
// member (the curated hub-member role, not a synthetic single-permission
// role) to confirm the ordinary account shape is also denied.
func TestBrokerRotateSecret_OrdinaryHubMemberDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newPlainUser(t, s, "rotate-owner-hubmember")
	member := newHubMemberUser(t, s, "rotate-member-hubmember")
	broker := &store.RuntimeBroker{ID: tid("rotate-broker-hubmember"), Name: "Rotate Broker Hub Member", Slug: "rotate-broker-hubmember", CreatedBy: owner.ID}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	originalKey := seedBrokerSecret(t, s, broker.ID)

	rec := rotateSecretAsUser(t, srv, member, broker.ID)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"an ordinary hub member should be denied rotation of a broker's secret they did not create; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "secretKey", "denied response must not carry a secret")

	stored, err := s.GetBrokerSecret(ctx, broker.ID)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(originalKey, stored.SecretKey), "the stored secret must not change on a denied rotation")
}

func TestBrokerRotateSecret_OwnerAllowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newPlainUser(t, s, "rotate-owner-allowed")
	broker := &store.RuntimeBroker{ID: tid("rotate-broker-owner-allowed"), Name: "Rotate Broker Owner", Slug: "rotate-broker-owner-allowed", CreatedBy: owner.ID}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	originalKey := seedBrokerSecret(t, s, broker.ID)

	rec := rotateSecretAsUser(t, srv, owner, broker.ID)

	require.Equal(t, http.StatusOK, rec.Code,
		"broker owner should be able to rotate their own broker's secret; got: %s", rec.Body.String())

	var resp RotateSecretResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.SecretKey, "a successful rotation should return the new secret")

	stored, err := s.GetBrokerSecret(ctx, broker.ID)
	require.NoError(t, err)
	assert.False(t, bytes.Equal(originalKey, stored.SecretKey), "the stored secret should change on a successful rotation")
}

func TestBrokerRotateSecret_SuperAdminAllowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newPlainUser(t, s, "rotate-owner-admin")
	admin := newSuperAdminUser(t, s, "rotate-admin-allowed")
	broker := &store.RuntimeBroker{ID: tid("rotate-broker-admin-allowed"), Name: "Rotate Broker Admin", Slug: "rotate-broker-admin-allowed", CreatedBy: owner.ID}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	originalKey := seedBrokerSecret(t, s, broker.ID)

	rec := rotateSecretAsUser(t, srv, admin, broker.ID)

	require.Equal(t, http.StatusOK, rec.Code,
		"a super-admin should be able to rotate any broker's secret; got: %s", rec.Body.String())

	var resp RotateSecretResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.SecretKey, "a successful rotation should return the new secret")

	stored, err := s.GetBrokerSecret(ctx, broker.ID)
	require.NoError(t, err)
	assert.False(t, bytes.Equal(originalKey, stored.SecretKey), "the stored secret should change on a successful rotation")
}

func TestBrokerRotateSecret_SelfAllowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newPlainUser(t, s, "rotate-owner-self")
	broker := &store.RuntimeBroker{ID: tid("rotate-broker-self-allowed"), Name: "Rotate Broker Self", Slug: "rotate-broker-self-allowed", CreatedBy: owner.ID}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	originalKey := seedBrokerSecret(t, s, broker.ID)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/brokers/"+broker.ID+"/rotate-secret", nil)
	brokerIdent := NewBrokerIdentity(broker.ID)
	reqCtx := contextWithBrokerIdentity(req.Context(), brokerIdent)
	reqCtx = contextWithIdentity(reqCtx, brokerIdent)
	req = req.WithContext(reqCtx)
	rec := httptest.NewRecorder()

	srv.handleBrokerRotateSecret(rec, req, broker.ID)

	require.Equal(t, http.StatusOK, rec.Code,
		"a broker should be able to rotate its own secret; got: %s", rec.Body.String())

	var resp RotateSecretResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.SecretKey, "a successful rotation should return the new secret")

	stored, err := s.GetBrokerSecret(ctx, broker.ID)
	require.NoError(t, err)
	assert.False(t, bytes.Equal(originalKey, stored.SecretKey), "the stored secret should change on a successful rotation")
}

// ----------------------------------------------------------------------------
// Empty caller ID (G1) — authorizedForBrokerOwnerAction is the single shared
// predicate behind re-registration, secret rotation, and the embedded
// register path (see handlers_project_register_broker_test.go), so a direct
// call here covers all three. An identity whose ID() is "" must never match
// an ownerless broker's empty CreatedBy: without the both-non-empty guard,
// "" == "" is true.
// ----------------------------------------------------------------------------

func TestAuthorizedForBrokerOwnerAction_EmptyCallerIDNeverMatchesOwnerlessBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker := &store.RuntimeBroker{ID: tid("empty-id-broker"), Name: "Empty ID Broker", Slug: "empty-id-broker", CreatedBy: ""}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	emptyIDUser := NewAuthenticatedUser("", "empty-id@test.com", "Empty ID", store.UserRoleMember, "api")

	allowed, err := srv.authorizedForBrokerOwnerAction(ctx, emptyIDUser, nil, broker.ID,
		func() (*store.RuntimeBroker, error) { return broker, nil })

	require.NoError(t, err)
	assert.False(t, allowed, "an empty caller ID must never match an ownerless broker's empty CreatedBy")
}

func TestBrokerRotateSecret_NonOwnerNoGrantDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newPlainUser(t, s, "rotate-owner-nogrant")
	nonOwner := newPlainUser(t, s, "rotate-nonowner-nogrant")
	broker := &store.RuntimeBroker{ID: tid("rotate-broker-nogrant"), Name: "Rotate Broker No Grant", Slug: "rotate-broker-nogrant", CreatedBy: owner.ID}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	originalKey := seedBrokerSecret(t, s, broker.ID)

	rec := rotateSecretAsUser(t, srv, nonOwner, broker.ID)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a user with none of the allowed grants must be denied secret rotation; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "secretKey", "denied response must not carry a secret")

	stored, err := s.GetBrokerSecret(ctx, broker.ID)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(originalKey, stored.SecretKey), "the stored secret must not change on a denied rotation")
}
