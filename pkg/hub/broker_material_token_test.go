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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	brokerMaterialEnvKey    = "BMT_SENSITIVE_VAR"
	brokerMaterialEnvValue  = "bmt-plain-value-1"
	brokerMaterialSecretKey = "BMT_SECRET"
)

// brokerMaterialFixture is a server with a secret backend, a broker owned by
// a hub member, a second hub member who does not own it, a super-admin, and
// one sensitive broker-scoped variable and one broker-scoped secret.
type brokerMaterialFixture struct {
	srv        *Server
	store      store.Store
	broker     *store.RuntimeBroker
	owner      *store.User
	nonOwner   *store.User
	viewer     *store.User
	superAdmin *store.User
	project    string
}

func newBrokerMaterialFixture(t *testing.T) *brokerMaterialFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	backend := secret.NewLocalBackend(s, "bmt-hub-id", "bmt-secret")
	srv.SetSecretBackend(backend)

	owner := newHubMemberUser(t, s, "bmt-owner")
	nonOwner := newHubMemberUser(t, s, "bmt-non-owner")
	// A user bound to the seeded hub-viewer role, which holds broker.read
	// at system scope and no broker.update.
	seedRoleUser(t, s, tid("bmt-viewer"), store.SystemRoleHubViewer, false)
	viewer, err := s.GetUser(ctx, tid("bmt-viewer"))
	require.NoError(t, err)
	superAdmin := newSuperAdminUser(t, s, "bmt-super-admin")
	ensureHubMembership(ctx, s, superAdmin.ID)

	broker := &store.RuntimeBroker{
		ID: tid("bmt-broker"), Name: "bmt-broker", Slug: "bmt-broker",
		Status: store.BrokerStatusOnline, CreatedBy: owner.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID: tid("bmt-envvar"), Key: brokerMaterialEnvKey, Value: brokerMaterialEnvValue, Sensitive: true,
		Scope: store.ScopeRuntimeBroker, ScopeID: broker.ID, Created: time.Now(), Updated: time.Now(),
	}))
	_, _, err = backend.Set(ctx, &secret.SetSecretInput{
		Name: brokerMaterialSecretKey, Value: "1", SecretType: secret.TypeEnvironment,
		Scope: secret.ScopeRuntimeBroker, ScopeID: broker.ID,
	})
	require.NoError(t, err)

	// A project the owner belongs to, so a project-boundary token can be
	// requested for it.
	project := tid("bmt-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: project, Name: "BMT Project", Slug: "bmt-project"}))
	createTestUserWithProjectRole(t, s, owner.ID, owner.Email, project, store.ProjectRoleOwner)

	return &brokerMaterialFixture{srv: srv, store: s, broker: broker, owner: owner, nonOwner: nonOwner, viewer: viewer, superAdmin: superAdmin, project: project}
}

// mint returns a real token for user, or "" with the mint error when the
// boundary and selectors cannot be minted.
func (f *brokerMaterialFixture) mint(user *store.User, boundary TokenBoundary, scopes ...string) (string, error) {
	key, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(user.ID), CreateTokenParams{
		UserID: user.ID, Name: "bmt-" + tid("tok"), Boundary: boundary, Scopes: scopes,
	})
	return key, err
}

// brokerMaterialReadPaths are the four read paths on the broker routes and
// the same reads on the generic env and secret routes with
// scope=runtime_broker.
func brokerMaterialReadPaths(brokerID string) map[string]string {
	q := "?scope=runtime_broker&scopeId=" + brokerID
	return map[string]string{
		"env list":           "/api/v1/runtime-brokers/" + brokerID + "/env",
		"env get":            "/api/v1/runtime-brokers/" + brokerID + "/env/" + brokerMaterialEnvKey,
		"secret list":        "/api/v1/runtime-brokers/" + brokerID + "/secrets",
		"secret get":         "/api/v1/runtime-brokers/" + brokerID + "/secrets/" + brokerMaterialSecretKey,
		"scoped env list":    "/api/v1/env" + q,
		"scoped env get":     "/api/v1/env/" + brokerMaterialEnvKey + q,
		"scoped secret list": "/api/v1/secrets" + q,
		"scoped secret get":  "/api/v1/secrets/" + brokerMaterialSecretKey + q,
	}
}

// TestBrokerMaterial_ReadsRequireBrokerRead pins the read rule for a
// broker's env vars and secret metadata: broker.read on the broker plus the
// owner relationship or broker.update. A token also needs the broker:read
// selector on a hub boundary. The seeded hub-member and hub-viewer roles
// hold broker.read without broker.update and are refused.
func TestBrokerMaterial_ReadsRequireBrokerRead(t *testing.T) {
	f := newBrokerMaterialFixture(t)

	ownerWithSelector, err := f.mint(f.owner, hubBoundary(), "broker:read")
	require.NoError(t, err)
	ownerWithoutSelector, err := f.mint(f.owner, hubBoundary(), "broker:list")
	require.NoError(t, err)
	nonOwnerWithSelector, err := f.mint(f.nonOwner, hubBoundary(), "broker:read")
	require.NoError(t, err)
	// broker:read is a hub-only selector, so a project token carrying it
	// cannot be minted.
	_, projectMintErr := f.mint(f.owner, projectBoundary(f.project), "broker:read")
	require.Error(t, projectMintErr, "a project token with broker:read must not be mintable")
	viewerToken, err := f.mint(f.viewer, hubBoundary(), "broker:read")
	require.NoError(t, err, "a hub-viewer holds broker.read, so it mints broker:read on a hub boundary")

	for name, path := range brokerMaterialReadPaths(f.broker.ID) {
		t.Run(name, func(t *testing.T) {
			// A hub member's broker.read reaches the broker directory, not
			// the material of a broker it does not own.
			rec := doRequestAsUser(t, f.srv, f.nonOwner, http.MethodGet, path, nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "non-owner session: %s", rec.Body.String())

			// The seeded hub-viewer role holds broker.read but not
			// broker.update.
			rec = doRequestAsUser(t, f.srv, f.viewer, http.MethodGet, path, nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "hub-viewer session: %s", rec.Body.String())
			rec = doRequestWithToken(t, f.srv, viewerToken, http.MethodGet, path, nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "hub-viewer hub token with broker:read: %s", rec.Body.String())

			rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet, path, nil)
			assert.Equal(t, http.StatusOK, rec.Code, "owner session: %s", rec.Body.String())

			rec = doRequestAsUser(t, f.srv, f.superAdmin, http.MethodGet, path, nil)
			assert.Equal(t, http.StatusOK, rec.Code, "super-admin session (broker.update): %s", rec.Body.String())

			rec = doRequestWithToken(t, f.srv, ownerWithSelector, http.MethodGet, path, nil)
			assert.Equal(t, http.StatusOK, rec.Code, "owner hub token with broker:read: %s", rec.Body.String())

			rec = doRequestWithToken(t, f.srv, ownerWithoutSelector, http.MethodGet, path, nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "owner hub token without broker:read: %s", rec.Body.String())

			rec = doRequestWithToken(t, f.srv, nonOwnerWithSelector, http.MethodGet, path, nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "non-owner hub token with broker:read: %s", rec.Body.String())

		})
	}
}

// TestBrokerMaterial_SensitiveValuesMaskedForAdmittedReaders pins that an
// admitted reader gets a sensitive broker variable masked on the list and
// the single-key read, on both route forms.
func TestBrokerMaterial_SensitiveValuesMaskedForAdmittedReaders(t *testing.T) {
	f := newBrokerMaterialFixture(t)
	token, err := f.mint(f.owner, hubBoundary(), "broker:read")
	require.NoError(t, err)

	paths := brokerMaterialReadPaths(f.broker.ID)
	for _, name := range []string{"env list", "env get", "scoped env list", "scoped env get"} {
		for _, caller := range []string{"owner session", "owner token"} {
			t.Run(name+"/"+caller, func(t *testing.T) {
				var rec *httptest.ResponseRecorder
				if caller == "owner session" {
					rec = doRequestAsUser(t, f.srv, f.owner, http.MethodGet, paths[name], nil)
				} else {
					rec = doRequestWithToken(t, f.srv, token, http.MethodGet, paths[name], nil)
				}
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.NotContains(t, rec.Body.String(), brokerMaterialEnvValue, "a sensitive value is never returned")
				assert.Contains(t, rec.Body.String(), `"********"`, "a sensitive value is returned masked")
			})
		}
	}
}

// TestBrokerMaterial_WritesSessionOnly pins that broker material writes
// refuse every user access token with the GOV_PENDING reason and need
// broker.update (the owner relationship or a role binding) on a session.
func TestBrokerMaterial_WritesSessionOnly(t *testing.T) {
	f := newBrokerMaterialFixture(t)
	ownerToken, err := f.mint(f.owner, hubBoundary(), "broker:read")
	require.NoError(t, err)
	adminToken, err := f.mint(f.superAdmin, hubBoundary(), "broker:read")
	require.NoError(t, err)

	q := "?scope=runtime_broker&scopeId=" + f.broker.ID
	envBody := map[string]interface{}{"value": "v"}
	secretBody := map[string]interface{}{"value": "dg=="}
	// Each delete removes a key an earlier write (or the fixture) created,
	// so the owner's session write succeeds on every row.
	writes := []struct {
		name, method, path string
		body               interface{}
	}{
		{"broker env put", http.MethodPut, "/api/v1/runtime-brokers/" + f.broker.ID + "/env/BMT_WRITE", envBody},
		{"broker env delete", http.MethodDelete, "/api/v1/runtime-brokers/" + f.broker.ID + "/env/BMT_WRITE", nil},
		{"broker secret put", http.MethodPut, "/api/v1/runtime-brokers/" + f.broker.ID + "/secrets/BMT_WRITE_SECRET", secretBody},
		{"broker secret delete", http.MethodDelete, "/api/v1/runtime-brokers/" + f.broker.ID + "/secrets/BMT_WRITE_SECRET", nil},
		{"scoped env put", http.MethodPut, "/api/v1/env/BMT_SCOPED_WRITE", map[string]interface{}{"value": "v", "scope": store.ScopeRuntimeBroker, "scopeId": f.broker.ID}},
		{"scoped env delete", http.MethodDelete, "/api/v1/env/BMT_SCOPED_WRITE" + q, nil},
		{"scoped secret delete", http.MethodDelete, "/api/v1/secrets/" + brokerMaterialSecretKey + q, nil},
	}
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			for label, key := range map[string]string{"owner token": ownerToken, "super-admin token": adminToken} {
				rec := doRequestWithToken(t, f.srv, key, w.method, w.path, w.body)
				reason, credential := sessionOnlyDetailsOf(rec)
				assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", label, rec.Body.String())
				assert.Equal(t, string(authzop.ReasonGovernancePending), reason, "%s: %s", label, rec.Body.String())
				assert.Equal(t, sessionRequiredCredential, credential, "%s", label)
			}

			rec := doRequestAsUser(t, f.srv, f.nonOwner, w.method, w.path, w.body)
			assert.Equal(t, http.StatusForbidden, rec.Code, "non-owner session: %s", rec.Body.String())

			rec = doRequestAsUser(t, f.srv, f.owner, w.method, w.path, w.body)
			assert.Less(t, rec.Code, 300, "owner session: %s", rec.Body.String())
			assert.GreaterOrEqual(t, rec.Code, 200, "owner session: %s", rec.Body.String())
		})
	}
}

// TestHubTokenBrokerOwner_ReadsOwnBrokerThroughOwnerGrant pins that a hub
// token reaches the owner relationship on a broker: the owner's token with
// broker:read reads the broker record and its material, which beyond
// broker.read is open only through the owner relationship (a hub member
// holds no broker.update binding); the same token of a non-owner reads the
// record but not the material.
func TestHubTokenBrokerOwner_ReadsOwnBrokerThroughOwnerGrant(t *testing.T) {
	f := newBrokerMaterialFixture(t)
	ownerToken, err := f.mint(f.owner, hubBoundary(), "broker:read")
	require.NoError(t, err)
	nonOwnerToken, err := f.mint(f.nonOwner, hubBoundary(), "broker:read")
	require.NoError(t, err)

	record := "/api/v1/runtime-brokers/" + f.broker.ID
	material := record + "/env"

	rec := doRequestWithToken(t, f.srv, ownerToken, http.MethodGet, record, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestWithToken(t, f.srv, ownerToken, http.MethodGet, material, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "the owner relationship admits the owner's hub token: %s", rec.Body.String())

	rec = doRequestWithToken(t, f.srv, nonOwnerToken, http.MethodGet, record, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "the hub-member binding reads the record: %s", rec.Body.String())
	rec = doRequestWithToken(t, f.srv, nonOwnerToken, http.MethodGet, material, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "no owner relationship, no material: %s", rec.Body.String())
}

// TestBrokerHeartbeat_UserPathSessionOnly pins that a user heartbeat needs
// an interactive session or dev credential and broker.update from a role
// binding: every user access token is refused with HOST_OPERATIONS, the
// broker owner's session is refused, and the dev super-admin is admitted.
func TestBrokerHeartbeat_UserPathSessionOnly(t *testing.T) {
	f := newBrokerMaterialFixture(t)
	path := "/api/v1/runtime-brokers/" + f.broker.ID + "/heartbeat"
	body := brokerHeartbeatRequest{Status: string(store.BrokerStatusOnline)}

	adminToken, err := f.mint(f.superAdmin, hubBoundary(), "broker:read")
	require.NoError(t, err)
	ownerToken, err := f.mint(f.owner, hubBoundary(), "broker:read")
	require.NoError(t, err)
	for label, key := range map[string]string{"super-admin token": adminToken, "owner token": ownerToken} {
		rec := doRequestWithToken(t, f.srv, key, http.MethodPost, path, body)
		reason, credential := sessionOnlyDetailsOf(rec)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", label, rec.Body.String())
		assert.Equal(t, string(authzop.ReasonHostOperations), reason, "%s: %s", label, rec.Body.String())
		assert.Equal(t, sessionRequiredCredential, credential, "%s", label)
	}

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, path, body)
	assert.Equal(t, http.StatusForbidden, rec.Code, "owner session without a broker.update binding: %s", rec.Body.String())

	rec = doRequestAsUser(t, f.srv, f.superAdmin, http.MethodPost, path, body)
	assert.Equal(t, http.StatusOK, rec.Code, "super-admin session: %s", rec.Body.String())

	rec = doRequest(t, f.srv, http.MethodPost, path, body)
	assert.Equal(t, http.StatusOK, rec.Code, "dev credential: %s", rec.Body.String())
}

// brokerListEntry is one row of a broker listing response.
type brokerListEntry struct {
	ID  string        `json:"id"`
	Cap *Capabilities `json:"_capabilities"`
}

func decodeBrokerListing(t *testing.T, rec *httptest.ResponseRecorder) map[string]*Capabilities {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Brokers []brokerListEntry `json:"brokers"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	out := map[string]*Capabilities{}
	for _, b := range resp.Brokers {
		out[b.ID] = b.Cap
	}
	return out
}

// TestBrokerListing_DispatchCapabilityForDispatchableBrokers pins that the
// listing reports dispatch for every broker the caller may dispatch to:
// without projectId by the project-independent rule (owned and auto-provide
// brokers), with projectId by the project-aware rule (a provider linked with
// its owner's consent included). The single-broker read uses the
// project-independent rule.
func TestBrokerListing_DispatchCapabilityForDispatchableBrokers(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	member := newHubMemberUser(t, s, "bld-member")
	other := newHubMemberUser(t, s, "bld-other")

	mk := func(name, owner string, auto bool) *store.RuntimeBroker {
		b := &store.RuntimeBroker{
			ID: tid(name), Name: name, Slug: name, Status: store.BrokerStatusOnline,
			CreatedBy: owner, AutoProvide: auto, Created: time.Now(), Updated: time.Now(),
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, b))
		return b
	}
	owned := mk("bld-owned", member.ID, false)
	auto := mk("bld-auto", other.ID, true)
	consented := mk("bld-consented", other.ID, false)
	unconsented := mk("bld-unconsented", other.ID, false)

	project := tid("bld-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: project, Name: "BLD Project", Slug: "bld-project"}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: project, BrokerID: consented.ID, BrokerName: consented.Name, LinkedBy: other.ID}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: project, BrokerID: unconsented.ID, BrokerName: unconsented.Name}))

	all := decodeBrokerListing(t, doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/runtime-brokers", nil))
	assert.True(t, capabilityAllows(all[owned.ID], ActionDispatch), "owned broker")
	assert.True(t, capabilityAllows(all[auto.ID], ActionDispatch), "auto-provide broker")
	assert.False(t, capabilityAllows(all[consented.ID], ActionDispatch), "a consented provider needs the project context")
	assert.False(t, capabilityAllows(all[unconsented.ID], ActionDispatch), "a broker the member may not dispatch to")

	scoped := decodeBrokerListing(t, doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/runtime-brokers?projectId="+project, nil))
	require.Contains(t, scoped, consented.ID)
	require.Contains(t, scoped, unconsented.ID)
	assert.True(t, capabilityAllows(scoped[consented.ID], ActionDispatch), "a provider linked with its owner's consent")
	assert.False(t, capabilityAllows(scoped[unconsented.ID], ActionDispatch), "a provider linked without its owner's consent")

	for _, b := range []*store.RuntimeBroker{owned, auto, unconsented} {
		rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/runtime-brokers/"+b.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var one brokerListEntry
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &one))
		assert.Equal(t, b.ID != unconsented.ID, capabilityAllows(one.Cap, ActionDispatch), "GET %s", b.Name)
	}
}

// TestBrokerListing_TokenWithoutBrokerReadListsNoBrokers pins the broker
// list's token rule: rows are kept only where broker.read is allowed, so a
// hub token without broker:read lists no brokers and one with it lists them.
func TestBrokerListing_TokenWithoutBrokerReadListsNoBrokers(t *testing.T) {
	f := newBrokerMaterialFixture(t)
	without, err := f.mint(f.superAdmin, hubBoundary(), "project:read")
	require.NoError(t, err)
	with, err := f.mint(f.superAdmin, hubBoundary(), "broker:read")
	require.NoError(t, err)

	assert.Empty(t, decodeBrokerListing(t, doRequestWithToken(t, f.srv, without, http.MethodGet, "/api/v1/runtime-brokers", nil)))
	assert.Contains(t, decodeBrokerListing(t, doRequestWithToken(t, f.srv, with, http.MethodGet, "/api/v1/runtime-brokers", nil)), f.broker.ID)
}

// brokerImageOpRefusal is the 403 message of the image-op broker check.
const brokerImageOpRefusal = "You do not have permission to perform image operations on this broker"

// TestHarnessImageOps_DispatchAuthorityCheckedFirst pins the broker check of
// the harness-config image operations with broker_id: broker.dispatch is
// decided first; a user access token it refuses is refused even on an
// auto-provide broker; a session caller it refuses still uses an
// auto-provide broker and is refused on any other; a holder of
// broker.dispatch is admitted. An admitted request reaches the broker call,
// which fails against the unreachable test endpoint (not 403).
func TestHarnessImageOps_DispatchAuthorityCheckedFirst(t *testing.T) {
	srv, s := testServer(t)
	srv.brokerClient = &HybridBrokerClient{
		controlChannel: &ControlChannelBrokerClient{manager: &neverConnectedTunnel{}},
		httpClient:     newBrokerHTTPTransport(false, nil),
	}
	ctx := context.Background()

	member := newHubMemberUser(t, s, "hio-member")
	grantPermissionViaRoleBinding(t, s, member.ID, "harness_config.update", store.RoleScopeSystem, "")
	superAdmin := newSuperAdminUser(t, s, "hio-super-admin")
	ensureHubMembership(ctx, s, superAdmin.ID)
	adminToken := mintHubBrokerUAT(t, srv, superAdmin.ID, "harness_config:update")

	mk := func(name string, auto bool) *store.RuntimeBroker {
		b := &store.RuntimeBroker{
			ID: tid(name), Name: name, Slug: name, Endpoint: "http://127.0.0.1:1",
			Status: store.BrokerStatusOnline, AutoProvide: auto,
			Profiles: []store.BrokerProfile{{Name: "docker", Type: "docker", Available: true}},
			Created:  time.Now(), Updated: time.Now(),
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, b))
		return b
	}
	shared := mk("hio-auto", true)
	private := mk("hio-private", false)
	hc := createTestHarnessConfig(t, s, "hio-hc", "hio-image:latest")

	ops := []struct{ name, method, action string }{
		{"pull", http.MethodPost, "pull-image"},
		{"delete", http.MethodDelete, "local-image"},
	}
	for _, op := range ops {
		path := func(b *store.RuntimeBroker) string {
			return "/api/v1/harness-configs/" + hc.ID + "/" + op.action + "?broker_id=" + b.ID
		}
		t.Run(op.name, func(t *testing.T) {
			// The super-admin holds broker.dispatch, but its token carries
			// no broker dispatch selector: refused on both brokers.
			// The refusal names the broker check, so the token passed the
			// harness-config route gate and was refused by the dispatch
			// decision, ahead of the auto-provide rule.
			for _, b := range []*store.RuntimeBroker{shared, private} {
				rec := doRequestWithToken(t, srv, adminToken, op.method, path(b), nil)
				assert.Equal(t, http.StatusForbidden, rec.Code, "token on %s: %s", b.Name, rec.Body.String())
				assert.Contains(t, rec.Body.String(), brokerImageOpRefusal, "token on %s", b.Name)
			}

			rec := doRequestAsUser(t, srv, member, op.method, path(shared), nil)
			assert.NotEqual(t, http.StatusForbidden, rec.Code, "session on the auto-provide broker: %s", rec.Body.String())
			rec = doRequestAsUser(t, srv, member, op.method, path(private), nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "session without broker.dispatch: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), brokerImageOpRefusal)

			for _, b := range []*store.RuntimeBroker{shared, private} {
				rec = doRequestAsUser(t, srv, superAdmin, op.method, path(b), nil)
				assert.NotEqual(t, http.StatusForbidden, rec.Code, "broker.dispatch holder on %s: %s", b.Name, rec.Body.String())
			}
		})
	}
}

// TestBrokerImageOps_OnlySessionCredentialsReachAutoProvide pins the
// credential allow-list of the image-op broker check on an auto-provide
// broker the caller holds no broker.dispatch on: an interactive session or
// dev credential uses the auto-provide rule; a user access token, a
// federation credential and a request with no credential kind are refused.
func TestBrokerImageOps_OnlySessionCredentialsReachAutoProvide(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	user := newHubMemberUser(t, s, "bio-member")
	broker := &store.RuntimeBroker{
		ID: tid("bio-auto"), Name: "bio-auto", Slug: "bio-auto", Status: store.BrokerStatusOnline,
		AutoProvide: true, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, "member", "web")
	for kind, want := range map[CredentialKind]bool{
		CredentialKindInteractive: true,
		CredentialKindDev:         true,
		CredentialKindUAT:         false,
		CredentialKindFederation:  false,
		"":                        false,
	} {
		reqCtx := contextWithCredentialContext(contextWithIdentity(ctx, identity), CredentialContext{Kind: kind})
		assert.Equal(t, want, srv.canRunBrokerImageOps(reqCtx, broker), "credential kind %q", kind)
	}
}

// TestBrokerMaterial_TestAdminIdentitiesRefused pins the test-admin policy
// gate of broker material: a test identity with an admin role is refused
// broker env and secret reads and writes on every credential kind, also on
// a broker it registered, and the refusal is recorded as the
// authorizeBrokerMaterial role gate. Human super-admins, including one who
// did not register the broker, are admitted.
func TestBrokerMaterial_TestAdminIdentitiesRefused(t *testing.T) {
	srv, s, buf := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "bmt-ta-iss", store.SystemRoleSuperAdmin)
	issuer2 := taUser(t, s, "bmt-ta-iss2", store.SystemRoleSuperAdmin)
	kinds := map[string]TestAdminIdentityTokenResponse{
		"test_super_admin": tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin),
		"test_hub_admin":   tsaIssue(t, srv, issuer2, testAdminRoleHubAdmin),
	}

	mkBroker := func(name, owner string) *store.RuntimeBroker {
		b := &store.RuntimeBroker{
			ID: tid(name), Name: name, Slug: name, Status: store.BrokerStatusOnline,
			CreatedBy: owner, Created: time.Now(), Updated: time.Now(),
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, b))
		return b
	}
	shared := mkBroker("bmt-ta-broker", issuer.ID)

	decide := func(id UserIdentity, kind CredentialKind, b *store.RuntimeBroker, isWrite bool) (bool, int) {
		reqCtx := contextWithCredentialContext(contextWithIdentityAndUser(id), CredentialContext{Kind: kind})
		r := httptest.NewRequest(http.MethodGet, "/api/v1/runtime-brokers/"+b.ID+"/env", nil).WithContext(reqCtx)
		w := httptest.NewRecorder()
		return srv.authorizeBrokerMaterial(w, r, b, isWrite), w.Code
	}
	credentialKinds := []CredentialKind{CredentialKindInteractive, CredentialKindDev, CredentialKindUAT, CredentialKindFederation, ""}

	for who, k := range kinds {
		id := tsaIdentity(t, s, k.Identity.ID)
		own := mkBroker("bmt-ta-own-"+who, k.Identity.ID)
		for _, b := range []*store.RuntimeBroker{shared, own} {
			for _, kind := range credentialKinds {
				for _, isWrite := range []bool{false, true} {
					ok, code := decide(id, kind, b, isWrite)
					assert.False(t, ok, "%s on %s, credential %q, write=%v", who, b.Name, kind, isWrite)
					assert.Equal(t, http.StatusForbidden, code, "%s on %s, credential %q, write=%v", who, b.Name, kind, isWrite)
				}
			}
			// Over HTTP with the identity's own token.
			rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodGet, "/api/v1/runtime-brokers/"+b.ID+"/env", nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s read on %s: %s", who, b.Name, rec.Body.String())
			rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPut, "/api/v1/runtime-brokers/"+b.ID+"/env/BMT_TA_VAR", map[string]interface{}{"value": "v"})
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s write on %s: %s", who, b.Name, rec.Body.String())
			// The broker secret routes and the generic env and secret
			// routes with scope=runtime_broker refuse the same way.
			q := "?scope=runtime_broker&scopeId=" + b.ID
			for _, path := range []string{
				"/api/v1/runtime-brokers/" + b.ID + "/secrets",
				"/api/v1/runtime-brokers/" + b.ID + "/secrets/BMT_TA_SECRET",
				"/api/v1/env" + q,
				"/api/v1/secrets" + q,
			} {
				rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodGet, path, nil)
				assert.Equal(t, http.StatusForbidden, rec.Code, "%s read %s: %s", who, path, rec.Body.String())
			}
			rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPut, "/api/v1/runtime-brokers/"+b.ID+"/secrets/BMT_TA_SECRET", map[string]interface{}{"value": "dg=="})
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s secret write on %s: %s", who, b.Name, rec.Body.String())
		}
	}

	denied := false
	for _, d := range tsaSinkLines(buf, testAdminAuditEventPolicyDenial) {
		if d["class"] == "role_gate" && d["subject"] == "handlers_env_secrets.go:authorizeBrokerMaterial" {
			denied = true
		}
	}
	assert.True(t, denied, "the refusal is recorded as the authorizeBrokerMaterial role gate")

	// Human super-admins are admitted for reads and writes: the one who
	// registered the broker, and one who did not (broker.update from the
	// super-admin role, not the owner relationship).
	other := taUser(t, s, "bmt-ta-human", store.SystemRoleSuperAdmin)
	require.NotEqual(t, shared.CreatedBy, other.ID)
	for _, u := range []*store.User{issuer, other} {
		human := tsaIdentity(t, s, u.ID)
		for _, isWrite := range []bool{false, true} {
			ok, code := decide(human, CredentialKindInteractive, shared, isWrite)
			assert.True(t, ok, "human super-admin %s, write=%v: %d", u.ID, isWrite, code)
		}
		rec := doRequestAsUser(t, srv, u, http.MethodGet, "/api/v1/runtime-brokers/"+shared.ID+"/env", nil)
		assert.Equal(t, http.StatusOK, rec.Code, "human super-admin %s session read: %s", u.ID, rec.Body.String())
	}
}

// TestBrokerUpdateDelete_TestAdminIdentitiesOnlyOwnBroker pins the
// test-admin policy gates of broker update and deregistration: a test
// identity with an admin role is refused PATCH and DELETE on a broker it did
// not register and admitted on a broker it registered; the refusals are
// recorded as the updateRuntimeBroker and deleteRuntimeBroker role gates. A
// human super-admin updates and deregisters another user's broker.
func TestBrokerUpdateDelete_TestAdminIdentitiesOnlyOwnBroker(t *testing.T) {
	srv, s, buf := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "bud-ta-iss", store.SystemRoleSuperAdmin)
	issuer2 := taUser(t, s, "bud-ta-iss2", store.SystemRoleSuperAdmin)
	kinds := map[string]TestAdminIdentityTokenResponse{
		"test_super_admin": tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin),
		"test_hub_admin":   tsaIssue(t, srv, issuer2, testAdminRoleHubAdmin),
	}
	mkBroker := func(name, owner string) *store.RuntimeBroker {
		b := &store.RuntimeBroker{
			ID: tid(name), Name: name, Slug: name, Status: store.BrokerStatusOffline,
			CreatedBy: owner, Created: time.Now(), Updated: time.Now(),
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, b))
		return b
	}
	path := func(b *store.RuntimeBroker) string { return "/api/v1/runtime-brokers/" + b.ID }

	others := mkBroker("bud-others", issuer.ID)
	for who, k := range kinds {
		own := mkBroker("bud-own-"+who, k.Identity.ID)

		rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodPatch, path(others), map[string]interface{}{"name": "bud-renamed-" + who})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s update of another user's broker: %s", who, rec.Body.String())
		rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodDelete, path(others), nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s delete of another user's broker: %s", who, rec.Body.String())
		_, err := s.GetRuntimeBroker(ctx, others.ID)
		require.NoError(t, err, "%s: the other user's broker is kept", who)

		rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPatch, path(own), map[string]interface{}{"name": "bud-own-renamed-" + who})
		assert.Equal(t, http.StatusOK, rec.Code, "%s update of its own broker: %s", who, rec.Body.String())
		rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodDelete, path(own), nil)
		assert.Equal(t, http.StatusNoContent, rec.Code, "%s delete of its own broker: %s", who, rec.Body.String())
	}

	gates := map[string]bool{}
	for _, d := range tsaSinkLines(buf, testAdminAuditEventPolicyDenial) {
		if d["class"] == "role_gate" {
			gates[d["subject"].(string)] = true
		}
	}
	assert.True(t, gates["handlers_runtime_brokers.go:updateRuntimeBroker"], "update refusal recorded: %v", gates)
	assert.True(t, gates["handlers_runtime_brokers.go:deleteRuntimeBroker"], "delete refusal recorded: %v", gates)

	// A human super-admin who did not register the broker is admitted.
	human := taUser(t, s, "bud-human", store.SystemRoleSuperAdmin)
	rec := doRequestAsUser(t, srv, human, http.MethodPatch, path(others), map[string]interface{}{"name": "bud-human-renamed"})
	assert.Equal(t, http.StatusOK, rec.Code, "human super-admin update: %s", rec.Body.String())
	rec = doRequestAsUser(t, srv, human, http.MethodDelete, path(others), nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "human super-admin delete: %s", rec.Body.String())
}
