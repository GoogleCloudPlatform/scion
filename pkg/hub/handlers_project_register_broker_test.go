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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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
// on a match, updates that record and mints a fresh secret. This requires
// the caller to be authorized against the resolved broker (shared
// authorizedForBrokerOwnerAction, handlers_brokers.go): a system-scoped
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

// registerBrokerTestExistingBroker inserts a broker directly into the store
// (skipping HTTP registration) with a recorded creator and non-default
// field values, so a denied overwrite attempt can be checked for no effect.
func registerBrokerTestExistingBroker(t *testing.T, s store.Store, name, createdBy string) *store.RuntimeBroker {
	t.Helper()
	broker := &store.RuntimeBroker{
		ID:              tid("register-broker-" + name),
		Name:            name,
		Slug:            api.Slugify(name),
		Version:         "1.0.0-baseline",
		Status:          store.BrokerStatusOnline,
		ConnectionState: "connected",
		Capabilities:    &store.BrokerCapabilities{WebPTY: false, Sync: false, Attach: false},
		CreatedBy:       createdBy,
		Created:         time.Now(),
		Updated:         time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), broker))
	return broker
}

// countProjectQuotaReservations reads back the number of active
// max_projects_per_user reservations held by userID, so a test can assert a
// denied register call did not consume a quota slot.
func countProjectQuotaReservations(t *testing.T, s store.Store, userID string) int64 {
	t.Helper()
	ctx := context.Background()
	limitDef, err := s.GetLimitDefinitionByName(ctx, "max_projects_per_user")
	require.NoError(t, err)
	count, err := s.CountActiveReservations(ctx, limitDef.ID, userID, "system", "system")
	require.NoError(t, err)
	return count
}

// setUserProjectQuotaCeiling overrides the seeded max_projects_per_user limit
// (default 0, meaning unlimited/unenforced) to a finite value, so
// CheckAndReserve actually records a reservation instead of skipping
// enforcement entirely. Without this, a quota-unchanged assertion is
// vacuous: it would read 0 before and after regardless of ordering, because
// nothing ever reserves against an unlimited quota.
func setUserProjectQuotaCeiling(t *testing.T, s store.Store, value int64) {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxProjectsPerUser)
	require.NoError(t, err, "max_projects_per_user must be seeded by New()/seedLimitDefinitions")
	def.DefaultValue = value
	_, err = s.UpdateLimitDefinition(context.Background(), def)
	require.NoError(t, err)
}

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
	assert.NotEmpty(t, resp.SecretKey, "owner overwrite should mint a fresh secret")

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
	assert.NotEmpty(t, resp.SecretKey)

	updated, err := s.GetRuntimeBroker(context.Background(), broker.ID)
	require.NoError(t, err)
	assert.Equal(t, "3.0.0-admin", updated.Version)
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
