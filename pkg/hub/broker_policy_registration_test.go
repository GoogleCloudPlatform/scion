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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setBrokerPolicyState(t *testing.T, srv *Server, s store.Store, expOn, policyOn bool) {
	t.Helper()
	setBrokerPolicyStateWithOverrides(t, srv, s, expOn, policyOn, nil)
}

func setBrokerPolicyStateWithOverrides(t *testing.T, srv *Server, s store.Store, expOn, policyOn bool, extraOverrides map[string]bool) {
	t.Helper()
	ctx := context.Background()
	overrides := map[string]bool{
		experiments.AdminManagedBrokers: expOn,
	}
	for k, v := range extraOverrides {
		overrides[k] = v
	}
	expDoc, err := json.Marshal(map[string]any{"overrides": overrides})
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "experiments", json.RawMessage(expDoc), "test", -1, "managed")
	require.NoError(t, err)

	policyDoc := json.RawMessage(`{"admin_managed_only":false}`)
	if policyOn {
		policyDoc = json.RawMessage(`{"admin_managed_only":true}`)
	}
	_, err = s.UpsertHubSetting(ctx, "broker_policy", policyDoc, "test", -1, "managed")
	require.NoError(t, err)

	ops := NewOperationalSettings(s, emptyKoanf(), emptyKoanf())
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	ApplySnapshot(srv, ops.Snapshot())
	srv.SetOperationalSettings(ops)
}

type captureBrokerPolicyAuditLogger struct {
	AuditLogger
	events []*BrokerAuthEvent
}

func (c *captureBrokerPolicyAuditLogger) LogBrokerAuth(_ context.Context, event *BrokerAuthEvent) {
	c.events = append(c.events, event)
}

func newHubAdminUser(t *testing.T, s store.Store, slug string) *store.User {
	t.Helper()
	u := newHubMemberUser(t, s, slug)
	ctx := context.Background()
	roleDef, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		PrincipalType:    "user",
		PrincipalID:      u.ID,
		RoleDefinitionID: roleDef.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return u
}

func mintHubBrokerCreateUAT(t *testing.T, srv *Server, userID string) string {
	t.Helper()
	return mintHubBrokerUAT(t, srv, userID, "broker:create")
}

func mintInteractiveUserJWT(t *testing.T, srv *Server, u *store.User) string {
	t.Helper()
	tok, _, _, err := srv.userTokenService.GenerateTokenPair(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeWeb)
	require.NoError(t, err)
	return tok
}

func doBrokerRequest(t *testing.T, srv *Server, method, path string, body any, auth func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	if auth != nil {
		auth(req)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeBrokerPolicyErrorCode(t *testing.T, rec *httptest.ResponseRecorder) (string, map[string]interface{}) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), "body: %s", rec.Body.String())
	return env.Error.Code, env.Error.Details
}

func TestIsAdminManagedBroker_Predicate(t *testing.T) {
	assert.False(t, isAdminManagedBroker(nil))
	assert.True(t, isAdminManagedBroker(&store.RuntimeBroker{ID: "b1", CreatedBy: ""}), "embedded/ownerless broker is admin-managed")
	assert.True(t, isAdminManagedBroker(&store.RuntimeBroker{ID: "b2", CreatedBy: "u1", AutoProvide: true}), "auto-provide broker is admin-managed")
	assert.True(t, isAdminManagedBroker(&store.RuntimeBroker{ID: "b3", CreatedBy: "u1", AdminManaged: true}), "explicitly approved broker is admin-managed")
	assert.False(t, isAdminManagedBroker(&store.RuntimeBroker{
		ID:        "b4",
		CreatedBy: "u1",
		Labels: map[string]string{
			"admin_managed":          "true",
			"adminManaged":           "true",
			"scion.io/admin-managed": "true",
		},
	}), "user-owned broker with fake labels must not be admin-managed")
}

func TestWarnBrokerPolicyExperimentMismatch(t *testing.T) {
	srv, s := testServer(t)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Experiment off, setting off -> no warning.
	setBrokerPolicyState(t, srv, s, false, false)
	srv.WarnBrokerPolicyExperimentMismatch()
	assert.Empty(t, buf.String())

	// Experiment on, setting on -> no warning.
	setBrokerPolicyState(t, srv, s, true, true)
	srv.WarnBrokerPolicyExperimentMismatch()
	assert.Empty(t, buf.String())

	// Experiment off, setting on -> warns.
	setBrokerPolicyState(t, srv, s, false, true)
	srv.WarnBrokerPolicyExperimentMismatch()
	assert.Contains(t, buf.String(), "broker_policy.admin_managed_only")
	assert.Contains(t, buf.String(), experiments.AdminManagedBrokers)
}

// TestBrokerPolicy_RegistrationMatrix exercises design §9's registration matrix:
// policy {off, on} x experiment {off, on} x caller {member, hub-admin, super-admin
// (sign-in), dev, member-UAT, admin-UAT} x {new, reregister-managed, reregister-unmanaged}
// x adminManaged {false, true}.
func TestBrokerPolicy_RegistrationMatrix(t *testing.T) {
	srv, s := testServer(t)
	member := newHubMemberUser(t, s, "bp-member")
	hubAdmin := newHubAdminUser(t, s, "bp-hub-admin")
	superAdmin := newSuperAdminUser(t, s, "bp-super-admin")

	memberSignIn := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+mintInteractiveUserJWT(t, srv, member))
	}
	hubAdminSignIn := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+mintInteractiveUserJWT(t, srv, hubAdmin))
	}
	superAdminSignIn := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+mintInteractiveUserJWT(t, srv, superAdmin))
	}
	devCred := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+testDevToken)
	}
	memberUAT := mintHubBrokerCreateUAT(t, srv, member.ID)
	memberUATAuth := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+memberUAT)
	}
	adminUAT := mintHubBrokerUAT(t, srv, superAdmin.ID, "broker:create", "project:create")
	adminUATAuth := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+adminUAT)
	}

	type callerSpec struct {
		name         string
		ownerID      string
		isAdmin      bool // holds broker.manage
		isSuperAdmin bool
		isUAT        bool
		auth         func(*http.Request)
	}
	callers := []callerSpec{
		{name: "member-signin", ownerID: member.ID, auth: memberSignIn},
		{name: "hubadmin-signin", ownerID: hubAdmin.ID, isAdmin: true, auth: hubAdminSignIn},
		{name: "superadmin-signin", ownerID: superAdmin.ID, isAdmin: true, isSuperAdmin: true, auth: superAdminSignIn},
		{name: "dev-cred", ownerID: DevUserID, isAdmin: true, isSuperAdmin: true, auth: devCred},
		{name: "member-uat", ownerID: member.ID, isUAT: true, auth: memberUATAuth},
		{name: "admin-uat", ownerID: superAdmin.ID, isAdmin: true, isSuperAdmin: true, isUAT: true, auth: adminUATAuth},
	}

	for _, expOn := range []bool{false, true} {
		for _, policyOn := range []bool{false, true} {
			effectiveOn := expOn && policyOn
			setBrokerPolicyState(t, srv, s, expOn, policyOn)

			for _, c := range callers {
				for _, target := range []string{"new", "reregister-managed", "reregister-unmanaged"} {
					for _, reqManaged := range []bool{false, true} {
						name := ""
						if expOn {
							name += "expOn/"
						} else {
							name += "expOff/"
						}
						if policyOn {
							name += "polOn/"
						} else {
							name += "polOff/"
						}
						name += c.name + "/" + target
						if reqManaged {
							name += "/managed"
						} else {
							name += "/unmanaged"
						}

						t.Run(name, func(t *testing.T) {
							brokerName := "bp-" + api.NewUUID()[:12]
							var existingID string
							if target != "new" {
								b := &store.RuntimeBroker{
									ID:           api.NewUUID(),
									Name:         brokerName,
									Slug:         api.Slugify(brokerName),
									Status:       store.BrokerStatusOffline,
									CreatedBy:    c.ownerID,
									AdminManaged: target == "reregister-managed",
								}
								require.NoError(t, s.CreateRuntimeBroker(context.Background(), b))
								existingID = b.ID
							}

							rec := doBrokerRequest(t, srv, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
								Name:         brokerName,
								AdminManaged: reqManaged,
							}, c.auth)

							// Compute expected status and error code.
							switch {
							case reqManaged && !expOn:
								assert.Equal(t, http.StatusPreconditionFailed, rec.Code, "body: %s", rec.Body.String())
								code, _ := decodeBrokerPolicyErrorCode(t, rec)
								assert.Equal(t, ErrCodeExperimentDisabled, code)
							case effectiveOn && c.isUAT:
								assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
								code, details := decodeBrokerPolicyErrorCode(t, rec)
								assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
								assert.Equal(t, "broker_policy.admin_managed_only", details["setting"])
							case reqManaged && (!c.isAdmin || c.isUAT):
								assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
								code, _ := decodeBrokerPolicyErrorCode(t, rec)
								if effectiveOn {
									assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
								} else {
									assert.Equal(t, ErrCodeForbidden, code)
								}
							case effectiveOn && target == "reregister-unmanaged":
								assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
								code, details := decodeBrokerPolicyErrorCode(t, rec)
								assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
								assert.Equal(t, "reregister", details["operation"])
								assert.Equal(t, existingID, details["brokerId"])
							case effectiveOn && target == "new" && !reqManaged:
								assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
								code, details := decodeBrokerPolicyErrorCode(t, rec)
								assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
								assert.Equal(t, "register", details["operation"])
								assert.Nil(t, details["brokerId"])
							default:
								require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
								var resp CreateBrokerRegistrationResponse
								require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
								got, err := s.GetRuntimeBroker(context.Background(), resp.BrokerID)
								require.NoError(t, err)
								wantManaged := reqManaged || target == "reregister-managed"
								assert.Equal(t, wantManaged, got.AdminManaged)
							}
						})
					}
				}
			}
		}
	}
}

func TestBrokerPolicy_FlatRegistrationAndPreserveSettings(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	auditCap := &captureBrokerPolicyAuditLogger{}
	srv.auditLogger = auditCap

	member := newHubMemberUser(t, s, "bp-flat-member")
	hubAdmin := newHubAdminUser(t, s, "bp-flat-admin")
	superAdmin := newSuperAdminUser(t, s, "bp-flat-super")

	setBrokerPolicyStateWithOverrides(t, srv, s, true, true, map[string]bool{
		experiments.FlatRuntimeBrokers: true,
	})

	memberAuth := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+mintInteractiveUserJWT(t, srv, member))
	}
	adminAuth := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+mintInteractiveUserJWT(t, srv, hubAdmin))
	}
	superAdminAuth := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+mintInteractiveUserJWT(t, srv, superAdmin))
	}

	// Member flat registration refused with policy 403.
	rec := doBrokerRequest(t, srv, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		BrokerID:      api.NewUUID(),
		Name:          "flat-member-denied",
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: "rt-1", Type: "docker"},
	}, memberAuth)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, _ := decodeBrokerPolicyErrorCode(t, rec)
	assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)

	// Hub-admin flat registration with adminManaged: true + PreserveSettings: true (join-token style) succeeds, stamps AdminManaged=true, and audits admin_managed=true.
	brokerID := api.NewUUID()
	rec = doBrokerRequest(t, srv, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		BrokerID:         brokerID,
		Name:             "flat-admin-managed",
		AdminManaged:     true,
		PreserveSettings: true,
		RuntimeTarget:    &api.RuntimeTargetDescriptor{ID: "rt-2", Type: "docker"},
	}, adminAuth)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	got, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	assert.True(t, got.AdminManaged)
	require.NotEmpty(t, auditCap.events)
	lastEvent := auditCap.events[len(auditCap.events)-1]
	assert.Equal(t, "true", lastEvent.Details["admin_managed"])
	assert.Empty(t, lastEvent.Details["prior_admin_managed"])

	// O1: Pre-approval via join-token (PreserveSettings: true + AdminManaged: true)
	// against existing unmanaged legacy and flat brokers while policy is off
	// upgrades AdminManaged=true while leaving preserved settings untouched, and
	// audits prior_admin_managed=false -> admin_managed=true.
	setBrokerPolicyStateWithOverrides(t, srv, s, true, false, map[string]bool{
		experiments.FlatRuntimeBrokers: true,
	})

	legacyExisting := &store.RuntimeBroker{
		ID:        api.NewUUID(),
		Name:      "legacy-preserve-upgrade",
		Slug:      "legacy-preserve-upgrade",
		Status:    store.BrokerStatusOffline,
		CreatedBy: member.ID,
		Labels:    map[string]string{"env": "staging"},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, legacyExisting))
	rec = doBrokerRequest(t, srv, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:             legacyExisting.Name,
		AdminManaged:     true,
		PreserveSettings: true,
		Labels:           map[string]string{"env": "should-not-overwrite", "extra": "ignored"},
	}, superAdminAuth)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	gotLegacy, err := s.GetRuntimeBroker(ctx, legacyExisting.ID)
	require.NoError(t, err)
	assert.True(t, gotLegacy.AdminManaged, "legacy PreserveSettings re-registration must honor authorized AdminManaged=true")
	assert.Equal(t, "staging", gotLegacy.Labels["env"], "preserved labels must remain untouched")
	assert.NotContains(t, gotLegacy.Labels, "extra", "preserved re-registration must not merge new labels")
	lastEvent = auditCap.events[len(auditCap.events)-1]
	assert.Equal(t, "true", lastEvent.Details["admin_managed"])
	assert.Equal(t, "false", lastEvent.Details["prior_admin_managed"])

	flatExisting := &store.RuntimeBroker{
		ID:            api.NewUUID(),
		Name:          "flat-preserve-upgrade",
		Slug:          "flat-preserve-upgrade",
		Status:        store.BrokerStatusOffline,
		CreatedBy:     member.ID,
		Labels:        map[string]string{"env": "prod"},
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: "rt-preserve", Type: "docker"},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, flatExisting))
	rec = doBrokerRequest(t, srv, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		BrokerID:         flatExisting.ID,
		Name:             flatExisting.Name,
		AdminManaged:     true,
		PreserveSettings: true,
		Labels:           map[string]string{"env": "should-not-overwrite", "extra": "ignored"},
		RuntimeTarget:    &api.RuntimeTargetDescriptor{ID: "rt-preserve", Type: "docker"},
	}, superAdminAuth)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	gotFlat, err := s.GetRuntimeBroker(ctx, flatExisting.ID)
	require.NoError(t, err)
	assert.True(t, gotFlat.AdminManaged, "flat PreserveSettings re-registration must honor authorized AdminManaged=true")
	assert.Equal(t, "prod", gotFlat.Labels["env"], "preserved flat labels must remain untouched")
	assert.NotContains(t, gotFlat.Labels, "extra", "preserved flat re-registration must not merge new labels")
	lastEvent = auditCap.events[len(auditCap.events)-1]
	assert.Equal(t, "true", lastEvent.Details["admin_managed"])
	assert.Equal(t, "false", lastEvent.Details["prior_admin_managed"])
}

func TestBrokerPolicy_DeprecatedProjectsRegisterBrokerPath(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	member := newHubMemberUser(t, s, "bp-embed-member")
	superAdmin := newSuperAdminUser(t, s, "bp-embed-admin")
	setBrokerPolicyState(t, srv, s, true, true)

	memberAuth := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+mintInteractiveUserJWT(t, srv, member))
	}
	adminAuth := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+mintInteractiveUserJWT(t, srv, superAdmin))
	}
	adminUAT := mintHubBrokerUAT(t, srv, superAdmin.ID, "broker:create", "project:create")
	adminUATAuth := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+adminUAT)
	}

	// New row via deprecated embedded flow: refused outright even for super-admin.
	rec := doBrokerRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "bp-embed-proj-1",
		Broker: &RegisterProjectBrokerInfo{
			Name: "bp-embed-new-broker",
		},
	}, adminAuth)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, details := decodeBrokerPolicyErrorCode(t, rec)
	assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
	assert.Equal(t, "register", details["operation"])
	assert.Contains(t, rec.Body.String(), "scion runtime-broker register --admin-managed")

	// Existing unmanaged broker: refused.
	unmanaged := &store.RuntimeBroker{
		ID:        api.NewUUID(),
		Name:      "bp-embed-unmanaged",
		Slug:      "bp-embed-unmanaged",
		Status:    store.BrokerStatusOffline,
		CreatedBy: member.ID,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, unmanaged))
	rec = doBrokerRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "bp-embed-proj-2",
		Broker: &RegisterProjectBrokerInfo{
			ID:   unmanaged.ID,
			Name: unmanaged.Name,
		},
	}, memberAuth)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, details = decodeBrokerPolicyErrorCode(t, rec)
	assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
	assert.Equal(t, "reregister", details["operation"])
	assert.Equal(t, unmanaged.ID, details["brokerId"])

	// Existing managed broker: refused for UAT while policy is on.
	managed := &store.RuntimeBroker{
		ID:           api.NewUUID(),
		Name:         "bp-embed-managed",
		Slug:         "bp-embed-managed",
		Status:       store.BrokerStatusOffline,
		CreatedBy:    superAdmin.ID,
		AdminManaged: true,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, managed))
	rec = doBrokerRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "bp-embed-proj-3",
		Broker: &RegisterProjectBrokerInfo{
			ID:   managed.ID,
			Name: managed.Name,
		},
	}, adminUATAuth)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, details = decodeBrokerPolicyErrorCode(t, rec)
	assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
	assert.Equal(t, "reregister", details["operation"])
	assert.Equal(t, managed.ID, details["brokerId"])

	// Existing managed broker: allowed for interactive super-admin.
	rec = doBrokerRequest(t, srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "bp-embed-proj-4",
		Broker: &RegisterProjectBrokerInfo{
			ID:   managed.ID,
			Name: managed.Name,
		},
	}, adminAuth)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestBrokerPolicy_BrokerSuppliedLabelsAndJoinIgnored(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	member := newHubMemberUser(t, s, "bp-spoof-member")
	setBrokerPolicyState(t, srv, s, true, false) // experiment on, policy off

	memberAuth := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+mintInteractiveUserJWT(t, srv, member))
	}

	// Member registers with spoofed labels; AdminManaged must stay false.
	rec := doBrokerRequest(t, srv, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name: "spoof-labels-broker",
		Labels: map[string]string{
			"admin_managed":          "true",
			"adminManaged":           "true",
			"scion.io/admin-managed": "true",
		},
	}, memberAuth)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var regResp CreateBrokerRegistrationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &regResp))

	// Join payload carrying an extra "adminManaged": true JSON key must be ignored.
	joinBody := map[string]interface{}{
		"brokerId":      regResp.BrokerID,
		"joinToken":     regResp.JoinToken,
		"hostname":      "spoof-host",
		"adminManaged":  true,
		"admin_managed": true,
	}
	joinRec := doBrokerRequest(t, srv, http.MethodPost, "/api/v1/brokers/join", joinBody, nil)
	require.Equal(t, http.StatusOK, joinRec.Code, joinRec.Body.String())

	got, err := s.GetRuntimeBroker(ctx, regResp.BrokerID)
	require.NoError(t, err)
	assert.False(t, got.AdminManaged)
	assert.False(t, isAdminManagedBroker(got))
}

func TestBrokerPolicy_AdminServerConfigDB_ExperimentGateAndLiveApply(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	ops := NewOperationalSettings(s, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	propCtx, cancelProp := context.WithCancel(ctx)
	t.Cleanup(cancelProp)
	ops.StartPropagation(propCtx, srv)

	superAdmin := newSuperAdminUser(t, s, "bp-cfg-admin")
	hubAdmin := newHubAdminUser(t, s, "bp-cfg-hub-admin")
	member := newHubMemberUser(t, s, "bp-cfg-member")
	adminAuth := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+mintInteractiveUserJWT(t, srv, superAdmin))
	}
	hubAdminAuth := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+mintInteractiveUserJWT(t, srv, hubAdmin))
	}
	memberAuth := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+mintInteractiveUserJWT(t, srv, member))
	}

	// Experiment off: saving admin_managed_only=true returns 412 experiment_disabled.
	rec := doBrokerRequest(t, srv, http.MethodPut, "/api/v1/admin/server-config",
		map[string]interface{}{"broker_policy": map[string]interface{}{"admin_managed_only": true}}, adminAuth)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
	code, _ := decodeBrokerPolicyErrorCode(t, rec)
	assert.Equal(t, ErrCodeExperimentDisabled, code)
	assert.False(t, srv.adminManagedBrokersOnly())

	// Enable experiment, then save admin_managed_only=true -> 200 and live applied.
	_, err = s.UpsertHubSetting(ctx, "experiments",
		json.RawMessage(`{"overrides":{"`+experiments.AdminManagedBrokers+`":true}}`), "test", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)

	rec = doBrokerRequest(t, srv, http.MethodPut, "/api/v1/admin/server-config",
		map[string]interface{}{"broker_policy": map[string]interface{}{"admin_managed_only": true}}, adminAuth)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, srv.adminManagedBrokersOnly())

	// End-to-end chain (design §8): with the policy saved via Admin Server Config,
	// a member's registration gets 403 broker_policy_admin_managed_only naming the
	// setting, and a hub-admin's --admin-managed sign-in registration returns 201
	// with the row stamped adminManaged=true.
	memberDenied := doBrokerRequest(t, srv, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name: "e2e-member-denied",
	}, memberAuth)
	require.Equal(t, http.StatusForbidden, memberDenied.Code, memberDenied.Body.String())
	code, details := decodeBrokerPolicyErrorCode(t, memberDenied)
	assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
	assert.Equal(t, "broker_policy.admin_managed_only", details["setting"])

	adminCreated := doBrokerRequest(t, srv, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:         "e2e-admin-managed",
		AdminManaged: true,
	}, hubAdminAuth)
	require.Equal(t, http.StatusCreated, adminCreated.Code, adminCreated.Body.String())
	var adminResp CreateBrokerRegistrationResponse
	require.NoError(t, json.Unmarshal(adminCreated.Body.Bytes(), &adminResp))
	storedBroker, err := s.GetRuntimeBroker(ctx, adminResp.BrokerID)
	require.NoError(t, err)
	assert.True(t, storedBroker.AdminManaged)

	// GET returns broker_policy.admin_managed_only == true.
	getRec := doBrokerRequest(t, srv, http.MethodGet, "/api/v1/admin/server-config", nil, adminAuth)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var cfgResp struct {
		BrokerPolicy *opsettings.BrokerPolicySettings `json:"broker_policy"`
	}
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &cfgResp))
	require.NotNil(t, cfgResp.BrokerPolicy)
	require.NotNil(t, cfgResp.BrokerPolicy.AdminManagedOnly)
	assert.True(t, *cfgResp.BrokerPolicy.AdminManagedOnly)

	// Turn experiment back off: enforcement turns off, and echoing stored true passes.
	_, err = s.UpsertHubSetting(ctx, "experiments",
		json.RawMessage(`{"overrides":{"`+experiments.AdminManagedBrokers+`":false}}`), "test", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	assert.False(t, srv.adminManagedBrokersOnly())

	rec = doBrokerRequest(t, srv, http.MethodPut, "/api/v1/admin/server-config",
		map[string]interface{}{"broker_policy": map[string]interface{}{"admin_managed_only": true}}, adminAuth)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Re-enable experiment, then clear the setting with null -> live-applies off,
	// and a member registration succeeds again (201).
	_, err = s.UpsertHubSetting(ctx, "experiments",
		json.RawMessage(`{"overrides":{"`+experiments.AdminManagedBrokers+`":true}}`), "test", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)

	rec = doBrokerRequest(t, srv, http.MethodPut, "/api/v1/admin/server-config",
		map[string]interface{}{"broker_policy": map[string]interface{}{"admin_managed_only": nil}}, adminAuth)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, srv.brokerPolicyAdminManagedOnlyConfigured())
	assert.False(t, srv.adminManagedBrokersOnly())

	memberAllowed := doBrokerRequest(t, srv, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name: "e2e-member-allowed-after-clear",
	}, memberAuth)
	require.Equal(t, http.StatusCreated, memberAllowed.Code, memberAllowed.Body.String())
}
