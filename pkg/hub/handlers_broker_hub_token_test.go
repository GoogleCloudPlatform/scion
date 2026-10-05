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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// POST /api/v1/brokers admits a hub-boundary user access token whose
// ceiling contains broker:create, held by a user who currently holds
// broker.create. A new broker is owned by the token's user. Re-registering
// an existing broker additionally requires that user to be the broker's
// creator; the super-admin arm admits only an interactive session or dev
// credential. Rotation admits no user access token.
// ============================================================================

// mintHubBrokerUAT mints a hub-boundary user access token for userID with
// the given scopes and returns its key.
func mintHubBrokerUAT(t *testing.T, srv *Server, userID string, scopes ...string) string {
	t.Helper()
	key, token, err := srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
		UserID:   userID,
		Name:     "hub-broker-token",
		Boundary: TokenBoundary{Kind: BoundaryKindHub},
		Scopes:   scopes,
	})
	require.NoError(t, err)
	require.Equal(t, string(BoundaryKindHub), token.BoundaryKind)
	return key
}

func decodeBrokerRegistration(t *testing.T, body *bytes.Buffer) CreateBrokerRegistrationResponse {
	t.Helper()
	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.NewDecoder(body).Decode(&resp))
	return resp
}

func assertNoBrokerNamed(t *testing.T, s store.Store, name string) {
	t.Helper()
	_, err := s.GetRuntimeBrokerByName(context.Background(), name)
	assert.ErrorIs(t, err, store.ErrNotFound, "a denied registration must not create a broker record")
}

// removeFromHubMembers removes userID from the hub-members group.
func removeFromHubMembers(t *testing.T, s store.Store, userID string) {
	t.Helper()
	require.NoError(t, removeHubMembershipTx(context.Background(), s, userID))
}

// ----------------------------------------------------------------------------
// First registration
// ----------------------------------------------------------------------------

func TestBrokerHubToken_NewRegistrationAdmitted(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	member := newHubMemberUser(t, s, "hubtoken-new-member")
	key := mintHubBrokerUAT(t, srv, member.ID, "broker:create")

	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name: "hubtoken-new-broker",
	})

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := decodeBrokerRegistration(t, rec.Body)
	assert.NotEmpty(t, resp.JoinToken)
	assert.False(t, resp.Reregistered)

	created, err := s.GetRuntimeBroker(ctx, resp.BrokerID)
	require.NoError(t, err)
	assert.Equal(t, member.ID, created.CreatedBy, "the token's user owns the new broker")
	assert.False(t, created.AutoProvide, "registration never turns on auto-provide")
	providers, err := s.GetBrokerProjects(ctx, resp.BrokerID)
	require.NoError(t, err)
	assert.Empty(t, providers, "registration never associates the broker with a project")
}

func TestBrokerHubToken_WithoutBrokerCreateScopeDenied(t *testing.T) {
	srv, s := testServer(t)
	member := newHubMemberUser(t, s, "hubtoken-noscope-member")
	key := mintHubBrokerUAT(t, srv, member.ID, "broker:read")

	const name = "hubtoken-noscope-broker"
	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{Name: name})

	assert.Equal(t, http.StatusForbidden, rec.Code, "the token ceiling must contain broker:create; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "joinToken")
	assertNoBrokerNamed(t, s, name)
}

func TestBrokerHubToken_UserRemovedFromHubMembersDenied(t *testing.T) {
	srv, s := testServer(t)
	member := newHubMemberUser(t, s, "hubtoken-removed-member")
	key := mintHubBrokerUAT(t, srv, member.ID, "broker:create")
	removeFromHubMembers(t, s, member.ID)

	const name = "hubtoken-removed-broker"
	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{Name: name})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"the token's user must currently hold broker.create; got: %s", rec.Body.String())
	assertNoBrokerNamed(t, s, name)
}

func TestBrokerHubToken_RevokedTokenRejected(t *testing.T) {
	srv, s := testServer(t)
	member := newHubMemberUser(t, s, "hubtoken-revoked-member")
	key, token, err := srv.uatService.CreateTokenWithParams(rs4MintContext(member.ID), CreateTokenParams{
		UserID: member.ID, Name: "hubtoken-revoked", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"broker:create"},
	})
	require.NoError(t, err)
	require.NoError(t, srv.uatService.RevokeToken(rs4MintContext(member.ID), member.ID, token.ID))

	const name = "hubtoken-revoked-broker"
	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{Name: name})

	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assertNoBrokerNamed(t, s, name)
}

func TestBrokerHubToken_ExpiredTokenRejected(t *testing.T) {
	srv, s := testServer(t)
	member := newHubMemberUser(t, s, "hubtoken-expired-member")

	mintClock := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	expiresAt := mintClock.Add(24 * time.Hour)
	serviceClock := srv.uatService.nowFunc
	srv.uatService.nowFunc = func() time.Time { return mintClock }
	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(member.ID), CreateTokenParams{
		UserID: member.ID, Name: "hubtoken-expired", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"broker:create"}, ExpiresAt: &expiresAt,
	})
	srv.uatService.nowFunc = serviceClock
	require.NoError(t, err)

	const name = "hubtoken-expired-broker"
	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{Name: name})

	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assertNoBrokerNamed(t, s, name)
}

func TestBrokerHubToken_SuspendedUserRejected(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	member := newHubMemberUser(t, s, "hubtoken-suspended-member")
	key := mintHubBrokerUAT(t, srv, member.ID, "broker:create")
	member.Status = store.UserStatusSuspended
	require.NoError(t, s.UpdateUser(ctx, member))

	const name = "hubtoken-suspended-broker"
	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{Name: name})

	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, "user_suspended", errResp.Error.Code)
	assertNoBrokerNamed(t, s, name)
}

func TestBrokerHubToken_PlainUserCannotMintBrokerCreate(t *testing.T) {
	srv, s := testServer(t)
	plain := newPlainUser(t, s, "hubtoken-plain-mint")

	key, token, err := srv.uatService.CreateTokenWithParams(rs4MintContext(plain.ID), CreateTokenParams{
		UserID: plain.ID, Name: "hubtoken-plain", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"broker:create"},
	})
	require.Error(t, err, "a user without broker.create cannot mint a token carrying broker:create")
	assert.Empty(t, key)
	assert.Nil(t, token)
	tokens, err := s.ListUserAccessTokens(context.Background(), plain.ID)
	require.NoError(t, err)
	assert.Empty(t, tokens, "a refused mint stores no token")
}

func TestBrokerHubToken_ExpiredHubMemberBindingSessionDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	user := newPlainUser(t, s, "hubtoken-expired-binding")

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)
	expired := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeSystem,
		ExpiresAt:        &expired,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	const name = "hubtoken-expired-binding-broker"
	rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{Name: name})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a hub-member binding outside its window grants no broker.create; got: %s", rec.Body.String())
	assertNoBrokerNamed(t, s, name)
}

// ----------------------------------------------------------------------------
// Re-registration
// ----------------------------------------------------------------------------

func TestBrokerHubToken_OwnerReregistersByName(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newHubMemberUser(t, s, "hubtoken-rereg-owner-name")
	broker := createReregistrationTestBroker(t, s, "hubtoken-rereg-owner-name-broker", owner.ID)
	key := mintHubBrokerUAT(t, srv, owner.ID, "broker:create")

	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name: broker.Name,
	})

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := decodeBrokerRegistration(t, rec.Body)
	assert.True(t, resp.Reregistered)
	assert.Equal(t, broker.ID, resp.BrokerID)
	assert.NotEmpty(t, resp.JoinToken)
	updated, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, owner.ID, updated.CreatedBy, "re-registration keeps the owner")
}

func TestBrokerHubToken_OwnerReregistersByIDWithNewName(t *testing.T) {
	srv, s := testServer(t)
	owner := newHubMemberUser(t, s, "hubtoken-rereg-owner-id")
	broker := createReregistrationTestBroker(t, s, "hubtoken-rereg-owner-id-broker", owner.ID)
	key := mintHubBrokerUAT(t, srv, owner.ID, "broker:create")

	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:     "hubtoken-rereg-owner-id-renamed",
		BrokerID: broker.ID,
	})

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := decodeBrokerRegistration(t, rec.Body)
	assert.True(t, resp.Reregistered)
	assert.Equal(t, broker.ID, resp.BrokerID)
}

func TestBrokerHubToken_SuperAdminTokenCannotReregisterOtherUsersBroker(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func(b *store.RuntimeBroker) CreateBrokerRegistrationRequest
	}{
		{"by name", func(b *store.RuntimeBroker) CreateBrokerRegistrationRequest {
			return CreateBrokerRegistrationRequest{Name: b.Name, Labels: map[string]string{"env": "other"}}
		}},
		{"by id", func(b *store.RuntimeBroker) CreateBrokerRegistrationRequest {
			return CreateBrokerRegistrationRequest{Name: "hubtoken-rereg-admin-unused-name", BrokerID: b.ID, Labels: map[string]string{"env": "other"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			audit := installBrokerAuditCapture(srv)
			owner := newHubMemberUser(t, s, "hubtoken-rereg-admin-owner")
			admin := newSuperAdminUser(t, s, "hubtoken-rereg-admin")
			broker := createReregistrationTestBroker(t, s, "hubtoken-rereg-admin-broker", owner.ID)
			key := mintHubBrokerUAT(t, srv, admin.ID, "broker:create")

			rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", tc.req(broker))

			assert.Equal(t, http.StatusForbidden, rec.Code,
				"a user access token re-registers only a broker its user created; got: %s", rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "joinToken")
			assertBrokerUnchanged(t, s, broker.ID)
			assertNoJoinToken(t, s, broker.ID)
			assertNoBrokerNamed(t, s, "hubtoken-rereg-admin-unused-name")
			assert.Empty(t, brokerAuditEventsOfType(audit, BrokerAuthEventRegister), "a denied re-registration records no register event")
		})
	}
}

func TestBrokerHubToken_SuperAdminTokenReregistersOwnBroker(t *testing.T) {
	srv, s := testServer(t)
	admin := newSuperAdminUser(t, s, "hubtoken-rereg-admin-own")
	broker := createReregistrationTestBroker(t, s, "hubtoken-rereg-admin-own-broker", admin.ID)
	key := mintHubBrokerUAT(t, srv, admin.ID, "broker:create")

	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name: broker.Name,
	})

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := decodeBrokerRegistration(t, rec.Body)
	assert.True(t, resp.Reregistered)
	assert.Equal(t, broker.ID, resp.BrokerID)
}

func TestBrokerHubToken_SuperAdminTokenCannotReregisterOwnerlessBroker(t *testing.T) {
	srv, s := testServer(t)
	audit := installBrokerAuditCapture(srv)
	admin := newSuperAdminUser(t, s, "hubtoken-rereg-admin-ownerless")
	broker := createReregistrationTestBroker(t, s, "hubtoken-rereg-admin-ownerless-broker", "")
	key := mintHubBrokerUAT(t, srv, admin.ID, "broker:create")

	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:   broker.Name,
		Labels: map[string]string{"env": "other"},
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"an ownerless broker matches no creator, so a user access token cannot re-register it; got: %s", rec.Body.String())
	assertBrokerUnchanged(t, s, broker.ID)
	assert.Empty(t, brokerAuditEventsOfType(audit, BrokerAuthEventRegister))
}

func TestBrokerHubToken_SuperAdminDevCredentialReregistersOtherUsersBroker(t *testing.T) {
	srv, s := testServer(t)
	owner := newHubMemberUser(t, s, "hubtoken-rereg-dev-owner")
	broker := createReregistrationTestBroker(t, s, "hubtoken-rereg-dev-broker", owner.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name: broker.Name,
	})

	require.Equal(t, http.StatusCreated, rec.Code,
		"a super-admin dev credential may re-register a broker another user created; got: %s", rec.Body.String())
	resp := decodeBrokerRegistration(t, rec.Body)
	assert.True(t, resp.Reregistered)
	assert.Equal(t, broker.ID, resp.BrokerID)
}

// TestBrokerRemintTargetAuthorized_SuperAdminArmNeedsSessionCredential pins
// the shared target helper used by POST /api/v1/brokers and the embedded
// broker path of POST /api/v1/projects/register: the super-admin arm admits
// an interactive or dev credential, and a user access token is held to the
// creator arm.
func TestBrokerRemintTargetAuthorized_SuperAdminArmNeedsSessionCredential(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newHubMemberUser(t, s, "remint-helper-owner")
	admin := newSuperAdminUser(t, s, "remint-helper-admin")
	other := createReregistrationTestBroker(t, s, "remint-helper-other-broker", owner.ID)
	own := createReregistrationTestBroker(t, s, "remint-helper-own-broker", admin.ID)
	ownerless := createReregistrationTestBroker(t, s, "remint-helper-ownerless-broker", "")

	session := NewAuthenticatedUser(admin.ID, admin.Email, admin.DisplayName, admin.Role, string(ClientTypeWeb))
	scoped := NewScopedUserIdentityWithCredentialID(session, "", []string{"broker:create"}, "remint-helper-token")
	sessionCtx := contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindInteractive})
	devCtx := contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindDev})
	uatCtx := contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindUAT, ID: "remint-helper-token"})

	for _, tc := range []struct {
		name   string
		ctx    context.Context
		user   UserIdentity
		broker *store.RuntimeBroker
		want   bool
	}{
		{"user access token, other user's broker", uatCtx, scoped, other, false},
		{"user access token, ownerless broker", uatCtx, scoped, ownerless, false},
		{"user access token, own broker", uatCtx, scoped, own, true},
		{"interactive session, other user's broker", sessionCtx, session, other, true},
		{"interactive session, ownerless broker", sessionCtx, session, ownerless, true},
		{"dev credential, other user's broker", devCtx, session, other, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, srv.brokerRemintTargetAuthorized(tc.ctx, tc.user, tc.broker))
		})
	}
}

func TestBrokerHubToken_SuperAdminTokenWithoutBrokerCreateDenied(t *testing.T) {
	srv, s := testServer(t)
	owner := newHubMemberUser(t, s, "hubtoken-rereg-narrow-owner")
	admin := newSuperAdminUser(t, s, "hubtoken-rereg-narrow-admin")
	broker := createReregistrationTestBroker(t, s, "hubtoken-rereg-narrow-broker", owner.ID)
	key := mintHubBrokerUAT(t, srv, admin.ID, "agent:read")

	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name: broker.Name,
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a super-admin's token re-registers only when its ceiling contains broker:create; got: %s", rec.Body.String())
	assertBrokerUnchanged(t, s, broker.ID)
}

func TestBrokerHubToken_NonOwnerReregistrationDenied(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func(b *store.RuntimeBroker) CreateBrokerRegistrationRequest
	}{
		{"by name", func(b *store.RuntimeBroker) CreateBrokerRegistrationRequest {
			return CreateBrokerRegistrationRequest{Name: b.Name, Labels: map[string]string{"env": "other"}}
		}},
		{"by id", func(b *store.RuntimeBroker) CreateBrokerRegistrationRequest {
			return CreateBrokerRegistrationRequest{Name: "hubtoken-nonowner-unused-name", BrokerID: b.ID, Labels: map[string]string{"env": "other"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			owner := newHubMemberUser(t, s, "hubtoken-nonowner-owner")
			other := newHubMemberUser(t, s, "hubtoken-nonowner-other")
			broker := createReregistrationTestBroker(t, s, "hubtoken-nonowner-broker", owner.ID)
			key := mintHubBrokerUAT(t, srv, other.ID, "broker:create")

			rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", tc.req(broker))

			assert.Equal(t, http.StatusForbidden, rec.Code,
				"a hub member who does not own the broker must not re-register it; got: %s", rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "joinToken")
			assertBrokerUnchanged(t, s, broker.ID)
			assertNoBrokerNamed(t, s, "hubtoken-nonowner-unused-name")
		})
	}
}

func TestBrokerHubToken_OwnNameWithOtherBrokerIDReregistersOwnBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newHubMemberUser(t, s, "hubtoken-namewins-owner")
	other := newHubMemberUser(t, s, "hubtoken-namewins-other")
	own := createReregistrationTestBroker(t, s, "hubtoken-namewins-own", owner.ID)
	target := createReregistrationTestBroker(t, s, "hubtoken-namewins-target", other.ID)
	key := mintHubBrokerUAT(t, srv, owner.ID, "broker:create")

	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:     own.Name,
		BrokerID: target.ID,
		Labels:   map[string]string{"env": "updated"},
	})

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := decodeBrokerRegistration(t, rec.Body)
	assert.Equal(t, own.ID, resp.BrokerID, "the name match selects the caller's own broker")
	updated, err := s.GetRuntimeBroker(ctx, own.ID)
	require.NoError(t, err)
	assert.Equal(t, "updated", updated.Labels["env"])
	assertBrokerUnchanged(t, s, target.ID)
}

func TestBrokerHubToken_OwnerTokenCannotRotate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newHubMemberUser(t, s, "hubtoken-rotate-owner")
	broker := createReregistrationTestBroker(t, s, "hubtoken-rotate-broker", owner.ID)
	originalKey := seedBrokerSecret(t, s, broker.ID)
	key := mintHubBrokerUAT(t, srv, owner.ID, "broker:create")

	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers/"+broker.ID+"/rotate-secret", map[string]interface{}{})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"no user access token selector authorizes rotation; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "secretKey")
	stored, err := s.GetBrokerSecret(ctx, broker.ID)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(originalKey, stored.SecretKey), "the stored secret must not change on a denied rotation")
}

// ----------------------------------------------------------------------------
// Lookup changes between authorization and mutation
// ----------------------------------------------------------------------------

// brokerLookupSwapStore wraps a store and runs onLookup, once, on the first
// GetRuntimeBrokerByName call issued by the registration service's own
// lookup (its context carries the marker set by createBrokerRegistration).
// The handler's authorization lookup carries no marker and reaches the
// wrapped store, however many lookups either side performs.
type brokerLookupSwapStore struct {
	store.Store
	mu       sync.Mutex
	fired    bool
	onLookup func(ctx context.Context, name string) (*store.RuntimeBroker, error)
}

func (w *brokerLookupSwapStore) GetRuntimeBrokerByName(ctx context.Context, name string) (*store.RuntimeBroker, error) {
	w.mu.Lock()
	hook := !w.fired && w.onLookup != nil && isBrokerRegistrationLookup(ctx)
	if hook {
		w.fired = true
	}
	w.mu.Unlock()
	if hook {
		return w.onLookup(ctx, name)
	}
	return w.Store.GetRuntimeBrokerByName(ctx, name)
}

// hookFired reports whether onLookup ran on the service's lookup.
func (w *brokerLookupSwapStore) hookFired() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}

func installBrokerLookupSwap(srv *Server, s store.Store, onLookup func(ctx context.Context, name string) (*store.RuntimeBroker, error)) *brokerLookupSwapStore {
	w := &brokerLookupSwapStore{Store: s, onLookup: onLookup}
	srv.brokerAuthService.store = w
	return w
}

func assertNoJoinToken(t *testing.T, s store.Store, brokerID string) {
	t.Helper()
	_, err := s.GetJoinTokenByBrokerID(context.Background(), brokerID)
	assert.True(t, errors.Is(err, store.ErrNotFound), "no join token may exist for broker %s; got err=%v", brokerID, err)
}

func TestBrokerHubToken_SameNameBrokerAppearsDuringNewRegistration(t *testing.T) {
	srv, s := testServer(t)
	member := newHubMemberUser(t, s, "hubtoken-race-new-member")
	other := newHubMemberUser(t, s, "hubtoken-race-new-other")
	key := mintHubBrokerUAT(t, srv, member.ID, "broker:create")

	const name = "hubtoken-race-new-broker"
	var inserted *store.RuntimeBroker
	swap := installBrokerLookupSwap(srv, s, func(ctx context.Context, n string) (*store.RuntimeBroker, error) {
		inserted = createReregistrationTestBroker(t, s, name, other.ID)
		return s.GetRuntimeBrokerByName(ctx, n)
	})

	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{Name: name})

	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "joinToken")
	require.True(t, swap.hookFired(), "the change must land on the registration service's own lookup")
	require.NotNil(t, inserted, "the service lookup must have run")
	assertBrokerUnchanged(t, s, inserted.ID)
	got, err := s.GetRuntimeBroker(context.Background(), inserted.ID)
	require.NoError(t, err)
	assert.Equal(t, other.ID, got.CreatedBy)
}

func TestBrokerHubToken_NameMatchChangesDuringOwnerReregistration(t *testing.T) {
	srv, s := testServer(t)
	owner := newHubMemberUser(t, s, "hubtoken-race-rereg-owner")
	other := newHubMemberUser(t, s, "hubtoken-race-rereg-other")
	own := createReregistrationTestBroker(t, s, "hubtoken-race-rereg-own", owner.ID)
	target := createReregistrationTestBroker(t, s, "hubtoken-race-rereg-target", other.ID)
	key := mintHubBrokerUAT(t, srv, owner.ID, "broker:create")

	swap := installBrokerLookupSwap(srv, s, func(ctx context.Context, _ string) (*store.RuntimeBroker, error) {
		return s.GetRuntimeBroker(ctx, target.ID)
	})

	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:   own.Name,
		Labels: map[string]string{"env": "updated"},
	})

	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "joinToken")
	assertBrokerUnchanged(t, s, own.ID)
	assertBrokerUnchanged(t, s, target.ID)
	assertNoJoinToken(t, s, own.ID)
	assertNoJoinToken(t, s, target.ID)
	assert.True(t, swap.hookFired(), "the change must land on the registration service's own lookup")
}

// A re-registration that keeps auto-provide on skips the
// broker.auto_provide check, so it is pinned to the broker still having
// auto-provide on when the service re-reads it.
func TestBrokerHubToken_AutoProvideTurnedOffDuringOwnerReregistration(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	owner := newHubMemberUser(t, s, "hubtoken-race-autoprovide-owner")
	broker := createReregistrationTestBroker(t, s, "hubtoken-race-autoprovide-broker", owner.ID)
	broker.AutoProvide = true
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	swap := installBrokerLookupSwap(srv, s, func(ctx context.Context, n string) (*store.RuntimeBroker, error) {
		current, err := s.GetRuntimeBrokerByName(ctx, n)
		require.NoError(t, err)
		current.AutoProvide = false
		require.NoError(t, s.UpdateRuntimeBroker(ctx, current))
		return s.GetRuntimeBrokerByName(ctx, n)
	})

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:        broker.Name,
		AutoProvide: true,
		Labels:      map[string]string{"env": "updated"},
	})

	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "joinToken")
	require.True(t, swap.hookFired(), "the change must land on the registration service's own lookup")
	assertBrokerUnchanged(t, s, broker.ID)
}
