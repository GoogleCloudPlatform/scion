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
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Embedded-broker ownership gate: POST /api/v1/projects/register
//
// The deprecated embedded-broker path (request carries a "broker" object
// rather than a brokerId) resolves an existing broker by ID or by name and,
// on a match, updates that record. It returns a secret only when it mints
// the broker's first one, never a stored secret. Updating a matched broker
// requires the caller to be authorized against the resolved broker (shared
// authorizedForBrokerRotate, handlers_brokers.go): a system-scoped
// super-admin, the broker itself (HMAC identity), or the user recorded as
// the broker's creator. Holding hub-scope project.create alone does not
// satisfy this check.
//
// The gate runs BEFORE any project mutation (quota reservation, project
// creation, group backfill), so a denial cannot orphan a caller-owned
// project or consume a quota slot.
//
// An explicit ID match that is denied is a hard 403: the caller named a
// specific broker they do not own. A NAME-only match that is denied instead
// falls through to create a brand-new caller-owned broker, leaving the
// matched broker untouched — names are not unique, so a hostname collision
// with another user's broker must not either overwrite it or hard-block the
// caller's own registration.
//
// First-time registration (no existing match at all) remains open to any
// caller who passes the project.create check, and records that caller as
// the new broker's owner.
// ============================================================================

func TestProjectRegisterEmbeddedBroker_IDMatchNonOwnerDenied(t *testing.T) {
	srv, s := testServer(t)
	owner := newHubMemberUser(t, s, "register-broker-owner-a")
	nonOwner := newHubMemberUser(t, s, "register-broker-nonowner-a")
	broker := registerBrokerTestExistingBroker(t, s, "register-broker-name-a", owner.ID)

	// Finite ceiling so the quota-unchanged assertion below actually has
	// teeth: against the default unlimited quota, CheckAndReserve never
	// records a reservation, so the count would read 0 before and after
	// regardless of gate ordering.
	setUserProjectQuotaCeiling(t, s, 5)
	quotaBefore := countProjectQuotaReservations(t, s, nonOwner.ID)
	gitRemote := "https://github.com/register-broker-test/nonowner-a"

	rec := doRequestAsUser(t, srv, nonOwner, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:      "register-broker-project-a",
		GitRemote: gitRemote,
		Broker: &RegisterProjectBrokerInfo{
			ID:      broker.ID,
			Name:    broker.Name,
			Version: "9.9.9-requested",
		},
	})

	require.Equal(t, http.StatusForbidden, rec.Code,
		"an explicit broker-ID match the caller does not own must be a hard deny; got: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "secretKey", "denied response must not carry a secret")

	// E1: the gate runs before any project mutation, so a denial must not
	// persist a project or consume a quota slot.
	projects, err := s.GetProjectsByGitRemote(context.Background(), util.NormalizeGitRemote(gitRemote))
	require.NoError(t, err)
	assert.Empty(t, projects, "a denied embedded-broker register must not persist a project")

	quotaAfter := countProjectQuotaReservations(t, s, nonOwner.ID)
	assert.Equal(t, quotaBefore, quotaAfter, "a denied embedded-broker register must not consume a quota slot")

	// The matched broker itself must be untouched.
	updated, err := s.GetRuntimeBroker(context.Background(), broker.ID)
	require.NoError(t, err)
	assert.Equal(t, broker.Name, updated.Name, "broker name must not change on a denied request")
	assert.Equal(t, broker.Version, updated.Version, "broker version must not change on a denied request")
	assert.Equal(t, broker.Capabilities, updated.Capabilities, "broker capabilities must not change on a denied request")
}

func TestProjectRegisterEmbeddedBroker_NameOnlyMismatchFallsThrough(t *testing.T) {
	srv, s := testServer(t)
	owner := newHubMemberUser(t, s, "register-broker-owner-e")
	requester := newHubMemberUser(t, s, "register-broker-requester-e")
	broker := registerBrokerTestExistingBroker(t, s, "register-broker-hostname-e", owner.ID)

	// requester supplies no ID — only a name that happens to collide with
	// owner's broker (e.g. same hostname). Denied by name alone must NOT
	// 403; it must fall through to create requester's own new broker.
	rec := doRequestAsUser(t, srv, requester, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "register-broker-project-e",
		Broker: &RegisterProjectBrokerInfo{
			Name:    broker.Name,
			Version: "2.0.0-collision",
		},
	})

	require.Equal(t, http.StatusOK, rec.Code,
		"a name-only collision with a broker the caller does not own must fall through to a new broker, not deny; got: %s", rec.Body.String())

	var resp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.SecretKey, "fallthrough create should mint a secret for the new broker")
	require.NotNil(t, resp.Broker)
	assert.NotEqual(t, broker.ID, resp.Broker.ID, "fallthrough must create a distinct broker, not reuse the matched one")

	created, err := s.GetRuntimeBroker(context.Background(), resp.Broker.ID)
	require.NoError(t, err)
	assert.Equal(t, requester.ID, created.CreatedBy, "the requester should own the newly created broker")

	// The matched (name-colliding) broker must be untouched.
	unchanged, err := s.GetRuntimeBroker(context.Background(), broker.ID)
	require.NoError(t, err)
	assert.Equal(t, broker.Version, unchanged.Version, "the name-colliding broker must not be modified by the fallthrough")
	assert.Equal(t, owner.ID, unchanged.CreatedBy, "the name-colliding broker's ownership must not change")
}

func TestProjectRegisterEmbeddedBroker_OwnerAllowed(t *testing.T) {
	srv, s := testServer(t)
	owner := newHubMemberUser(t, s, "register-broker-owner-b")
	broker := registerBrokerTestExistingBroker(t, s, "register-broker-name-b", owner.ID)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "register-broker-project-b",
		Broker: &RegisterProjectBrokerInfo{
			ID:      broker.ID,
			Name:    broker.Name,
			Version: "2.0.0-updated",
		},
	})

	require.Equal(t, http.StatusOK, rec.Code,
		"owner registering their own broker should succeed; got: %s", rec.Body.String())

	var resp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.SecretKey, "owner overwrite of a broker with no stored secret should mint its first secret")

	updated, err := s.GetRuntimeBroker(context.Background(), broker.ID)
	require.NoError(t, err)
	assert.Equal(t, "2.0.0-updated", updated.Version, "owner overwrite should apply requested fields")
}

func TestProjectRegisterEmbeddedBroker_SuperAdminAllowed(t *testing.T) {
	srv, s := testServer(t)
	owner := newHubMemberUser(t, s, "register-broker-owner-c")
	admin := newSuperAdminUser(t, s, "register-broker-admin-c")
	broker := registerBrokerTestExistingBroker(t, s, "register-broker-name-c", owner.ID)

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "register-broker-project-c",
		Broker: &RegisterProjectBrokerInfo{
			ID:      broker.ID,
			Name:    broker.Name,
			Version: "3.0.0-admin",
		},
	})

	require.Equal(t, http.StatusOK, rec.Code,
		"a super-admin should be able to register over a broker they did not create; got: %s", rec.Body.String())

	var resp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.SecretKey, "a broker with no stored secret gets its first secret minted")

	updated, err := s.GetRuntimeBroker(context.Background(), broker.ID)
	require.NoError(t, err)
	assert.Equal(t, "3.0.0-admin", updated.Version)
}

// TestOwnerForNewBroker_EmptyCallerIDNotRecorded pins ownerForNewBroker's
// return value for a nil identity, an empty-ID identity, and a real ID.
// ownerForNewBroker is called by the register path's create-new-broker
// branch (handlers_projects_core.go); the two-phase POST /brokers path sets
// CreatedBy directly in createBrokerRegistration instead of through this
// helper.
func TestOwnerForNewBroker_EmptyCallerIDNotRecorded(t *testing.T) {
	assert.Empty(t, ownerForNewBroker(nil), "no caller identity must not record an owner")

	emptyIDUser := NewAuthenticatedUser("", "empty-id-owner@test.com", "Empty ID", store.UserRoleMember, "api")
	assert.Empty(t, ownerForNewBroker(emptyIDUser), "an empty caller ID must not be recorded as CreatedBy")

	realUser := NewAuthenticatedUser("real-user-id", "real@test.com", "Real User", store.UserRoleMember, "api")
	assert.Equal(t, "real-user-id", ownerForNewBroker(realUser), "a real caller ID must still be recorded")
}

func TestProjectRegisterEmbeddedBroker_NewBrokerSetsOwnership(t *testing.T) {
	srv, s := testServer(t)
	requester := newHubMemberUser(t, s, "register-broker-newuser-d")

	rec := doRequestAsUser(t, srv, requester, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "register-broker-project-d",
		Broker: &RegisterProjectBrokerInfo{
			Name:    "a brand new embedded broker name never seen before",
			Version: "1.0.0",
		},
	})

	require.Equal(t, http.StatusOK, rec.Code,
		"first-time embedded-broker registration should still succeed; got: %s", rec.Body.String())

	var resp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotEmpty(t, resp.SecretKey, "first-time registration should mint a secret")
	require.NotNil(t, resp.Broker)

	created, err := s.GetRuntimeBroker(context.Background(), resp.Broker.ID)
	require.NoError(t, err)
	assert.Equal(t, requester.ID, created.CreatedBy, "the requester should become the new broker's owner")
}

// storeKnownBrokerSecret stores a fixed, recognisable secret for brokerID
// and returns its base64 form, so a test can search a response body for it.
func storeKnownBrokerSecret(t *testing.T, s store.Store, brokerID string) (string, []byte) {
	t.Helper()
	key := bytes.Repeat([]byte{0x5a}, 32)
	require.NoError(t, s.CreateBrokerSecret(context.Background(), &store.BrokerSecret{
		BrokerID:  brokerID,
		SecretKey: key,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		CreatedAt: time.Now(),
		Status:    store.BrokerSecretStatusActive,
	}))
	return base64.StdEncoding.EncodeToString(key), key
}

// assertBodyCarriesNoStoredSecret checks that a response body carries
// neither the stored secret (base64 or raw) nor a secretKey field.
func assertBodyCarriesNoStoredSecret(t *testing.T, body, encoded string, raw []byte) {
	t.Helper()
	assert.NotContains(t, body, encoded, "the response must not carry the stored secret")
	assert.NotContains(t, body, string(raw), "the response must not carry the raw stored secret")
	assert.NotContains(t, body, "secretKey", "the response must not carry a secretKey field")
}

// TestProjectRegisterEmbeddedBroker_ExistingSecretNotReturned pins that the
// deprecated embedded-broker path never returns the stored secret of a
// broker that already has one, for every caller allowed to re-register it,
// while the registration itself still succeeds.
func TestProjectRegisterEmbeddedBroker_ExistingSecretNotReturned(t *testing.T) {
	for _, tc := range []struct {
		name   string
		admin  bool
		byName bool
	}{
		{name: "owner by ID"},
		{name: "owner by name", byName: true},
		{name: "super-admin by ID", admin: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			owner := newHubMemberUser(t, s, "register-secret-owner")
			caller := owner
			if tc.admin {
				caller = newSuperAdminUser(t, s, "register-secret-admin")
			}
			broker := registerBrokerTestExistingBroker(t, s, "register-secret-broker", owner.ID)
			encoded, raw := storeKnownBrokerSecret(t, s, broker.ID)

			info := &RegisterProjectBrokerInfo{ID: broker.ID, Name: broker.Name, Version: "4.0.0-reregister"}
			if tc.byName {
				info.ID = ""
			}
			rec := doRequestAsUser(t, srv, caller, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
				Name:   "register-secret-project",
				Broker: info,
			})
			require.Equal(t, http.StatusOK, rec.Code, "re-registration must still succeed; got: %s", rec.Body.String())
			body := rec.Body.String()
			assertBodyCarriesNoStoredSecret(t, body, encoded, raw)

			var resp RegisterProjectResponse
			require.NoError(t, json.Unmarshal([]byte(body), &resp))
			assert.Empty(t, resp.SecretKey)
			assert.Empty(t, resp.BrokerToken, "no substitute token is issued for a broker that already has a secret")
			require.NotNil(t, resp.Broker)
			assert.Equal(t, broker.ID, resp.Broker.ID, "the existing broker is re-registered, not replaced")

			stored, err := s.GetBrokerSecret(context.Background(), broker.ID)
			require.NoError(t, err)
			assert.True(t, bytes.Equal(raw, stored.SecretKey), "the stored secret must not change")

			updated, err := s.GetRuntimeBroker(context.Background(), broker.ID)
			require.NoError(t, err)
			assert.Equal(t, "4.0.0-reregister", updated.Version, "the re-registration still applies the requested fields")

			providers, err := s.GetProjectProviders(context.Background(), resp.Project.ID)
			require.NoError(t, err)
			require.Len(t, providers, 1)
			assert.Equal(t, broker.ID, providers[0].BrokerID, "the re-registered broker still becomes a provider")
		})
	}
}

// TestProjectRegisterEmbeddedBroker_LeftoverSecretNotReturned pins that a
// new broker row created under an ID whose secret is still stored does not
// receive that secret.
func TestProjectRegisterEmbeddedBroker_LeftoverSecretNotReturned(t *testing.T) {
	srv, s := testServer(t)
	requester := newHubMemberUser(t, s, "register-leftover-requester")
	brokerID := tid("register-leftover-broker")
	encoded, raw := storeKnownBrokerSecret(t, s, brokerID)

	rec := doRequestAsUser(t, srv, requester, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "register-leftover-project",
		Broker: &RegisterProjectBrokerInfo{
			ID:      brokerID,
			Name:    "register leftover broker never seen before",
			Version: "1.0.0",
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assertBodyCarriesNoStoredSecret(t, rec.Body.String(), encoded, raw)
}

// TestProjectRegisterEmbeddedBroker_LostSecretInsertIsKeyless pins that a
// first registration whose secret insert loses to a concurrent insert for
// the same broker answers like a broker that already has a secret: no
// secretKey and no brokerToken, and the concurrently stored secret is
// unchanged.
func TestProjectRegisterEmbeddedBroker_LostSecretInsertIsKeyless(t *testing.T) {
	srv, s := testServer(t)
	requester := newHubMemberUser(t, s, "register-lost-insert-requester")
	winnerKey := bytes.Repeat([]byte{0x6b}, 32)
	race := &mintRaceStore{Store: s, winnerKey: winnerKey}
	require.NotNil(t, srv.brokerAuthService)
	srv.brokerAuthService.store = race

	rec := doRequestAsUser(t, srv, requester, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "register-lost-insert-project",
		Broker: &RegisterProjectBrokerInfo{
			Name:    "register lost insert broker",
			Version: "1.0.0",
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assertBodyCarriesNoStoredSecret(t, body, base64.StdEncoding.EncodeToString(winnerKey), winnerKey)
	assert.NotContains(t, body, "brokerToken", "the response must not fall back to a broker token")
	assert.Equal(t, 1, race.creates, "one insert")
	assert.Equal(t, 1, race.gets, "only the lookup before the insert reads the secret")

	var resp RegisterProjectResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Broker)
	stored, err := s.GetBrokerSecret(context.Background(), resp.Broker.ID)
	require.NoError(t, err)
	assert.Equal(t, winnerKey, stored.SecretKey, "the concurrently stored secret must not change")
}

// TestProjectRegisterEmbeddedBroker_DeniedResponseIsKeyless pins that a
// denied re-registration of a broker with a stored secret answers with a
// fixed error that carries no key material.
func TestProjectRegisterEmbeddedBroker_DeniedResponseIsKeyless(t *testing.T) {
	srv, s := testServer(t)
	owner := newHubMemberUser(t, s, "register-denied-owner")
	nonOwner := newHubMemberUser(t, s, "register-denied-nonowner")
	broker := registerBrokerTestExistingBroker(t, s, "register-denied-broker", owner.ID)
	encoded, raw := storeKnownBrokerSecret(t, s, broker.ID)

	rec := doRequestAsUser(t, srv, nonOwner, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:   "register-denied-project",
		Broker: &RegisterProjectBrokerInfo{ID: broker.ID, Name: broker.Name, Version: "1.0.0"},
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assertBodyCarriesNoStoredSecret(t, rec.Body.String(), encoded, raw)
	assert.Contains(t, rec.Body.String(), "not authorized to modify this broker")
}

// TestProjectRegisterEmbeddedBroker_FirstRegistrationReturnsStoredSecret
// pins that a first-time registration still returns the secret it minted,
// and that it is the one stored for the new broker.
func TestProjectRegisterEmbeddedBroker_FirstRegistrationReturnsStoredSecret(t *testing.T) {
	srv, s := testServer(t)
	requester := newHubMemberUser(t, s, "register-first-requester")

	rec := doRequestAsUser(t, srv, requester, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:   "register-first-project",
		Broker: &RegisterProjectBrokerInfo{Name: "register first broker never seen before", Version: "1.0.0"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotEmpty(t, resp.SecretKey, "first-time registration must still return the minted secret")
	require.NotNil(t, resp.Broker)

	stored, err := s.GetBrokerSecret(context.Background(), resp.Broker.ID)
	require.NoError(t, err)
	assert.Equal(t, resp.SecretKey, base64.StdEncoding.EncodeToString(stored.SecretKey))
}
