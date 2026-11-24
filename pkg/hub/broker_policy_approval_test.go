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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestBrokerRow(t *testing.T, s store.Store, name, createdBy string, autoProvide, adminManaged bool) *store.RuntimeBroker {
	t.Helper()
	b := &store.RuntimeBroker{
		ID:           api.NewUUID(),
		Name:         name,
		Slug:         slugify(name),
		Status:       store.BrokerStatusOnline,
		CreatedBy:    createdBy,
		AutoProvide:  autoProvide,
		AdminManaged: adminManaged,
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), b))
	return b
}

func TestBrokerPolicy_ApprovalPatch_AuthzAndAudit(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	capture := &captureBrokerPolicyAuditLogger{AuditLogger: srv.auditLogger}
	srv.auditLogger = capture

	owner := newHubMemberUser(t, s, "patch-owner")
	hubAdmin := newHubAdminUser(t, s, "patch-hubadmin")
	superAdmin := newSuperAdminUser(t, s, "patch-superadmin")

	ownerJWT := mintInteractiveUserJWT(t, srv, owner)
	hubAdminJWT := mintInteractiveUserJWT(t, srv, hubAdmin)
	superAdminJWT := mintInteractiveUserJWT(t, srv, superAdmin)

	ownerUAT := mintHubBrokerCreateUAT(t, srv, owner.ID)
	hubAdminUAT := mintHubBrokerCreateUAT(t, srv, hubAdmin.ID)
	superAdminUAT := mintHubBrokerCreateUAT(t, srv, superAdmin.ID)

	bearer := func(tok string) func(*http.Request) {
		return func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+tok) }
	}
	devAuth := func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+testDevToken)
	}

	broker := createTestBrokerRow(t, s, "user-owned-broker", owner.ID, false, false)

	// 1. Experiment OFF: hub-admin and super-admin get 412 experiment_disabled;
	// owner without broker.manage gets 403.
	setBrokerPolicyState(t, srv, s, false, false)

	rec := doBrokerRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID,
		map[string]any{"adminManaged": true}, bearer(ownerJWT))
	assert.Equal(t, http.StatusForbidden, rec.Code, "owner without broker.manage must be 403")

	for name, auth := range map[string]func(*http.Request){
		"hub-admin":   bearer(hubAdminJWT),
		"super-admin": bearer(superAdminJWT),
	} {
		rec := doBrokerRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID,
			map[string]any{"adminManaged": true}, auth)
		assert.Equal(t, http.StatusPreconditionFailed, rec.Code, "%s with experiment off must be 412", name)
		code, _ := decodeBrokerPolicyErrorCode(t, rec)
		assert.Equal(t, ErrCodeExperimentDisabled, code)
	}

	// 2. Experiment ON:
	setBrokerPolicyState(t, srv, s, true, true)

	// Owner without broker.manage -> 403 (no self-approval).
	rec = doBrokerRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID,
		map[string]any{"adminManaged": true}, bearer(ownerJWT))
	assert.Equal(t, http.StatusForbidden, rec.Code, "owner without broker.manage must be 403")

	// Owner can still update labels (broker.update) without flipping adminManaged.
	rec = doBrokerRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID,
		map[string]any{"labels": map[string]string{
			"adminManaged":           "true",
			"admin_managed":          "true",
			"scion.io/admin-managed": "true",
		}}, bearer(ownerJWT))
	require.Equal(t, http.StatusOK, rec.Code, "owner label update must succeed: %s", rec.Body.String())
	gotBroker, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.False(t, gotBroker.AdminManaged, "labels must not flip adminManaged")

	// Any UAT (owner, hub-admin, super-admin) -> 403.
	for name, tok := range map[string]string{
		"owner-UAT":       ownerUAT,
		"hub-admin-UAT":   hubAdminUAT,
		"super-admin-UAT": superAdminUAT,
	} {
		rec := doBrokerRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID,
			map[string]any{"adminManaged": true}, bearer(tok))
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s must be 403", name)
	}

	// Hub-admin attempting to change BOTH adminManaged and name on another user's broker -> 403
	// (hub-admin holds broker.manage, NOT broker.update).
	rec = doBrokerRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID,
		map[string]any{"adminManaged": true, "name": "renamed-by-hub-admin"}, bearer(hubAdminJWT))
	assert.Equal(t, http.StatusForbidden, rec.Code, "hub-admin without broker.update cannot rename broker")

	// Hub-admin (interactive) approves -> 200, audit records old=false, new=true + credential kind/id.
	capture.events = nil
	rec = doBrokerRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID,
		map[string]any{"adminManaged": true}, bearer(hubAdminJWT))
	require.Equal(t, http.StatusOK, rec.Code, "hub-admin interactive approve must succeed: %s", rec.Body.String())

	var patchResp RuntimeBrokerWithCapabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &patchResp))
	assert.True(t, patchResp.AdminManaged)
	assert.True(t, patchResp.PolicyEligible)
	require.NotNil(t, patchResp.Cap)
	assert.Contains(t, patchResp.Cap.Actions, string(ActionManage))

	gotBroker, err = s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.True(t, gotBroker.AdminManaged)

	require.NotEmpty(t, capture.events, "expected audit event on approve")
	ev := capture.events[len(capture.events)-1]
	assert.Equal(t, BrokerAuthEventUpdateSettings, ev.EventType)
	assert.Equal(t, "false", ev.Details["admin_managed.old"])
	assert.Equal(t, "true", ev.Details["admin_managed.new"])
	assert.Equal(t, string(CredentialKindInteractive), ev.Details["credential_kind"])

	// Dev credential revokes -> 200, audit records old=true, new=false + dev credential kind.
	capture.events = nil
	rec = doBrokerRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID,
		map[string]any{"adminManaged": false}, devAuth)
	require.Equal(t, http.StatusOK, rec.Code, "dev credential revoke must succeed: %s", rec.Body.String())

	var revokeResp RuntimeBrokerWithCapabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &revokeResp))
	assert.False(t, revokeResp.AdminManaged)
	assert.False(t, revokeResp.PolicyEligible)

	require.NotEmpty(t, capture.events, "expected audit event on revoke")
	ev = capture.events[len(capture.events)-1]
	assert.Equal(t, BrokerAuthEventUpdateSettings, ev.EventType)
	assert.Equal(t, "true", ev.Details["admin_managed.old"])
	assert.Equal(t, "false", ev.Details["admin_managed.new"])
	assert.Equal(t, string(CredentialKindDev), ev.Details["credential_kind"])

	// Super-admin (interactive) approves and hub-admin (interactive) revokes -> 200.
	rec = doBrokerRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID,
		map[string]any{"adminManaged": true}, bearer(superAdminJWT))
	require.Equal(t, http.StatusOK, rec.Code, "super-admin interactive approve must succeed: %s", rec.Body.String())
	gotBroker, err = s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.True(t, gotBroker.AdminManaged)

	rec = doBrokerRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID,
		map[string]any{"adminManaged": false}, bearer(hubAdminJWT))
	require.Equal(t, http.StatusOK, rec.Code, "hub-admin interactive revoke must succeed: %s", rec.Body.String())
	gotBroker, err = s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.False(t, gotBroker.AdminManaged)
}

func TestBrokerPolicy_ApprovalPatch_TestAdminRefused(t *testing.T) {
	srv, s, sink := tsaServer(t)
	setBrokerPolicyState(t, srv, s, true, true)
	ctx := context.Background()

	issuerSuper := newSuperAdminUser(t, s, "tsa-issuer-super")
	issuerHub := newSuperAdminUser(t, s, "tsa-issuer-hub")

	for _, tc := range []struct {
		name   string
		issuer *store.User
		role   string
	}{
		{"test_super_admin", issuerSuper, testAdminRoleSuperAdmin},
		{"test_hub_admin", issuerHub, testAdminRoleHubAdmin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issued := tsaIssue(t, srv, tc.issuer, tc.role)

			// Even on a broker the test-admin itself created, approving adminManaged must be refused.
			ownBroker := createTestBrokerRow(t, s, tc.name+"-own-broker", issued.Identity.ID, false, false)
			rec := doBrokerRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+ownBroker.ID,
				map[string]any{"adminManaged": true},
				func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+issued.AccessToken) })
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s must be refused approving adminManaged", tc.name)

			got, err := s.GetRuntimeBroker(ctx, ownBroker.ID)
			require.NoError(t, err)
			assert.False(t, got.AdminManaged)

			// Pin the census gate: verify test_admin_policy_denial was recorded with
			// subject == "broker_policy.go:authorizeBrokerManage" for this issuance.
			denials := tsaSinkLines(sink, testAdminAuditEventPolicyDenial)
			foundGate := false
			for _, d := range denials {
				if d["issuance_id"] == issued.Identity.IssuanceID &&
					d["class"] == "role_gate" &&
					d["subject"] == "broker_policy.go:authorizeBrokerManage" {
					foundGate = true
					break
				}
			}
			assert.True(t, foundGate, "%s: expected test_admin_policy_denial sink line with subject=broker_policy.go:authorizeBrokerManage, got %v", tc.name, denials)
		})
	}
}

func TestBrokerPolicy_HMACCannotSetAdminManaged(t *testing.T) {
	srv, s := testServer(t)
	setBrokerPolicyState(t, srv, s, true, true)
	ctx := context.Background()

	owner := newHubMemberUser(t, s, "hmac-owner")
	broker := createTestBrokerRow(t, s, "hmac-broker", owner.ID, false, false)
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	require.NoError(t, s.CreateBrokerSecret(ctx, &store.BrokerSecret{
		BrokerID:  broker.ID,
		SecretKey: secret,
		Status:    store.BrokerSecretStatusActive,
	}))

	nonceCounter := 0
	signHMAC := func(req *http.Request) {
		nonceCounter++
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		nonce := "p2-hmac-nonce-" + strconv.Itoa(nonceCounter)
		req.Header.Set(HeaderBrokerID, broker.ID)
		req.Header.Set(HeaderTimestamp, ts)
		req.Header.Set(HeaderNonce, nonce)
		canonical := srv.brokerAuthService.buildCanonicalString(req, ts, nonce)
		h := hmac.New(sha256.New, secret)
		h.Write(canonical)
		req.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(h.Sum(nil)))
	}

	// 1. Broker HMAC PATCH /api/v1/runtime-brokers/{id} -> 403.
	rec := doBrokerRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID,
		map[string]any{"adminManaged": true}, signHMAC)
	assert.Equal(t, http.StatusForbidden, rec.Code, "broker HMAC PATCH must be refused")

	// 2. Broker HMAC POST /api/v1/runtime-brokers/{id}/heartbeat with adminManaged: true -> ignored.
	rec = doBrokerRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat",
		map[string]any{
			"status":       "online",
			"adminManaged": true,
			"labels":       map[string]string{"adminManaged": "true", "scion.io/admin-managed": "true"},
		}, signHMAC)
	require.Equal(t, http.StatusOK, rec.Code, "heartbeat should succeed: %s", rec.Body.String())

	got, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.False(t, got.AdminManaged, "broker HMAC heartbeat must never set adminManaged")
}

func TestBrokerPolicy_ResponsesAndCapabilities(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := newHubMemberUser(t, s, "resp-owner")
	hubAdmin := newHubAdminUser(t, s, "resp-hubadmin")
	superAdmin := newSuperAdminUser(t, s, "resp-superadmin")

	ownerJWT := mintInteractiveUserJWT(t, srv, owner)
	hubAdminJWT := mintInteractiveUserJWT(t, srv, hubAdmin)
	superAdminJWT := mintInteractiveUserJWT(t, srv, superAdmin)
	hubAdminUAT := mintHubBrokerUAT(t, srv, hubAdmin.ID, "broker:list", "broker:read")

	embedded := createTestBrokerRow(t, s, "resp-embedded", "", false, false)
	autoProv := createTestBrokerRow(t, s, "resp-autoprov", owner.ID, true, false)
	approved := createTestBrokerRow(t, s, "resp-approved", owner.ID, false, true)
	unmanaged := createTestBrokerRow(t, s, "resp-unmanaged", owner.ID, false, false)

	proj := &store.Project{
		ID:      api.NewUUID(),
		Name:    "resp-project",
		Slug:    "resp-project",
		OwnerID: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	for _, b := range []*store.RuntimeBroker{embedded, autoProv, approved, unmanaged} {
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID:  proj.ID,
			BrokerID:   b.ID,
			BrokerName: b.Name,
			Status:     store.BrokerStatusOnline,
		}))
	}

	bearer := func(tok string) func(*http.Request) {
		return func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+tok) }
	}

	// Policy OFF (experiment ON, setting OFF): policyEligible must be true for ALL brokers.
	setBrokerPolicyState(t, srv, s, true, false)

	rec := doBrokerRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers", nil, bearer(hubAdminJWT))
	require.Equal(t, http.StatusOK, rec.Code)
	var listResp struct {
		Brokers []struct {
			ID             string        `json:"id"`
			AdminManaged   bool          `json:"adminManaged"`
			PolicyEligible bool          `json:"policyEligible"`
			Cap            *Capabilities `json:"_capabilities"`
		} `json:"brokers"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))
	byID := map[string]struct {
		adminManaged   bool
		policyEligible bool
		actions        []string
	}{}
	for _, b := range listResp.Brokers {
		var acts []string
		if b.Cap != nil {
			acts = b.Cap.Actions
		}
		byID[b.ID] = struct {
			adminManaged   bool
			policyEligible bool
			actions        []string
		}{b.AdminManaged, b.PolicyEligible, acts}
	}
	for _, b := range []*store.RuntimeBroker{embedded, autoProv, approved, unmanaged} {
		entry, ok := byID[b.ID]
		require.True(t, ok, "missing broker %s in list", b.Name)
		assert.Equal(t, b.AdminManaged, entry.adminManaged, "%s adminManaged", b.Name)
		assert.True(t, entry.policyEligible, "%s policyEligible must be true when policy is off", b.Name)
		assert.Contains(t, entry.actions, string(ActionManage), "hub-admin must see manage capability")
	}

	// Policy ON (experiment ON, setting ON): unmanaged has policyEligible=false; others true.
	setBrokerPolicyState(t, srv, s, true, true)

	// 1. GET /api/v1/runtime-brokers
	rec = doBrokerRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers", nil, bearer(superAdminJWT))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))
	for _, b := range listResp.Brokers {
		switch b.ID {
		case embedded.ID:
			assert.False(t, b.AdminManaged)
			assert.True(t, b.PolicyEligible)
		case autoProv.ID:
			assert.False(t, b.AdminManaged)
			assert.True(t, b.PolicyEligible)
		case approved.ID:
			assert.True(t, b.AdminManaged)
			assert.True(t, b.PolicyEligible)
		case unmanaged.ID:
			assert.False(t, b.AdminManaged)
			assert.False(t, b.PolicyEligible)
		}
		require.NotNil(t, b.Cap)
		assert.Contains(t, b.Cap.Actions, string(ActionManage), "super-admin must see manage capability")
	}

	// 2. GET /api/v1/runtime-brokers/{id} — owner does NOT get manage; hub-admin UAT does NOT get manage; hub-admin interactive DOES.
	rec = doBrokerRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers/"+unmanaged.ID, nil, bearer(ownerJWT))
	require.Equal(t, http.StatusOK, rec.Code)
	var detailResp struct {
		AdminManaged   bool          `json:"adminManaged"`
		PolicyEligible bool          `json:"policyEligible"`
		Cap            *Capabilities `json:"_capabilities"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detailResp))
	assert.False(t, detailResp.AdminManaged)
	assert.False(t, detailResp.PolicyEligible)
	require.NotNil(t, detailResp.Cap)
	assert.NotContains(t, detailResp.Cap.Actions, string(ActionManage), "owner without broker.manage must not get manage capability")

	rec = doBrokerRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers/"+unmanaged.ID, nil, bearer(hubAdminUAT))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detailResp))
	require.NotNil(t, detailResp.Cap)
	assert.NotContains(t, detailResp.Cap.Actions, string(ActionManage), "UAT must not get manage capability")

	rec = doBrokerRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers/"+unmanaged.ID, nil, bearer(hubAdminJWT))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detailResp))
	require.NotNil(t, detailResp.Cap)
	assert.Contains(t, detailResp.Cap.Actions, string(ActionManage), "hub-admin interactive must get manage capability")

	// 3. GET /api/v1/runtime-brokers?projectId=... (project-filtered list)
	rec = doBrokerRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers?projectId="+proj.ID, nil, bearer(hubAdminJWT))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))
	for _, b := range listResp.Brokers {
		if b.ID == unmanaged.ID {
			assert.False(t, b.PolicyEligible, "project-filtered list must mark unmanaged broker policyEligible=false")
		} else if b.ID == approved.ID {
			assert.True(t, b.AdminManaged)
			assert.True(t, b.PolicyEligible)
		}
	}

	// 4. GET /api/v1/projects/{id}/providers
	rec = doBrokerRequest(t, srv, http.MethodGet, "/api/v1/projects/"+proj.ID+"/providers", nil, bearer(superAdminJWT))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var provResp struct {
		Providers []struct {
			BrokerID       string `json:"brokerId"`
			AdminManaged   bool   `json:"adminManaged"`
			PolicyEligible bool   `json:"policyEligible"`
		} `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &provResp))
	require.Len(t, provResp.Providers, 4)
	for _, p := range provResp.Providers {
		switch p.BrokerID {
		case embedded.ID, autoProv.ID:
			assert.False(t, p.AdminManaged)
			assert.True(t, p.PolicyEligible)
		case approved.ID:
			assert.True(t, p.AdminManaged)
			assert.True(t, p.PolicyEligible)
		case unmanaged.ID:
			assert.False(t, p.AdminManaged)
			assert.False(t, p.PolicyEligible)
		}
	}

	// Policy OFF again: providers list reports policyEligible=true for unmanaged.
	setBrokerPolicyState(t, srv, s, true, false)
	rec = doBrokerRequest(t, srv, http.MethodGet, "/api/v1/projects/"+proj.ID+"/providers", nil, bearer(superAdminJWT))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &provResp))
	for _, p := range provResp.Providers {
		assert.True(t, p.PolicyEligible, "provider %s must be policyEligible=true when policy is off", p.BrokerID)
	}
}

// concurrentAdminManagedFlipStore flips admin_managed via the dedicated writer
// just before UpdateRuntimeBroker runs for target, simulating a concurrent
// admin change between an owner PATCH's initial read and its whole-row write.
type concurrentAdminManagedFlipStore struct {
	store.Store
	fault  *storeFaultSwitch
	target string
	value  bool
	hooked bool
}

func (w *concurrentAdminManagedFlipStore) UpdateRuntimeBroker(ctx context.Context, b *store.RuntimeBroker) error {
	if w.fault.Active() && b.ID == w.target && !w.hooked {
		w.hooked = true
		if _, err := w.Store.SetRuntimeBrokerAdminManaged(ctx, b.ID, w.value); err != nil {
			return err
		}
	}
	return w.Store.UpdateRuntimeBroker(ctx, b)
}

// TestBrokerPolicy_WholeRowWriteBackPreservesAdminManaged verifies that whole-row
// Runtime Broker updates (direct stale write-backs, broker join, and owner
// re-registration/settings updates) cannot overwrite an admin's change to
// admin_managed (ptone/scion#8915).
func TestBrokerPolicy_WholeRowWriteBackPreservesAdminManaged(t *testing.T) {
	srv, s, flipStore, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *concurrentAdminManagedFlipStore {
		return &concurrentAdminManagedFlipStore{Store: inner, fault: f}
	})
	ctx := context.Background()

	capture := &captureBrokerPolicyAuditLogger{AuditLogger: srv.auditLogger}
	srv.auditLogger = capture

	owner := newHubMemberUser(t, s, "race-owner")
	hubAdmin := newHubAdminUser(t, s, "race-hubadmin")

	ownerJWT := mintInteractiveUserJWT(t, srv, owner)
	hubAdminJWT := mintInteractiveUserJWT(t, srv, hubAdmin)

	bearer := func(tok string) func(*http.Request) {
		return func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+tok) }
	}

	// Policy off, experiment on so owner re-registration and admin PATCH are both allowed.
	setBrokerPolicyState(t, srv, s, true, false)

	// 1. Stale whole-row UpdateRuntimeBroker cannot undo an admin PATCH revocation.
	broker := createTestBrokerRow(t, s, "race-stale-broker", owner.ID, false, true)
	stale, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	require.True(t, stale.AdminManaged)

	rec := doBrokerRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID,
		map[string]any{"adminManaged": false}, bearer(hubAdminJWT))
	require.Equal(t, http.StatusOK, rec.Code, "revoke must succeed: %s", rec.Body.String())

	// Simulate a concurrent whole-row write-back holding the pre-revocation copy.
	stale.Version = "9.9.9"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, stale))

	got, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.False(t, got.AdminManaged, "stale whole-row update must not undo adminManaged=false")
	assert.Equal(t, "9.9.9", got.Version, "non-adminManaged fields must still be written")

	// 2. Broker join racing after admin revocation preserves adminManaged=false.
	regRec := doBrokerRequest(t, srv, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name:         "race-join-broker",
		AdminManaged: true,
	}, bearer(hubAdminJWT))
	require.Equal(t, http.StatusCreated, regRec.Code, regRec.Body.String())
	var regResp CreateBrokerRegistrationResponse
	require.NoError(t, json.Unmarshal(regRec.Body.Bytes(), &regResp))

	// Admin revokes adminManaged before join completes.
	rec = doBrokerRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+regResp.BrokerID,
		map[string]any{"adminManaged": false}, bearer(hubAdminJWT))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	joinResp, err := srv.brokerAuthService.CompleteBrokerJoin(ctx, BrokerJoinRequest{
		BrokerID:  regResp.BrokerID,
		JoinToken: regResp.JoinToken,
		Version:   "1.2.3",
	}, "http://localhost:8080")
	require.NoError(t, err)
	require.NotNil(t, joinResp)

	gotJoined, err := s.GetRuntimeBroker(ctx, regResp.BrokerID)
	require.NoError(t, err)
	assert.False(t, gotJoined.AdminManaged, "CompleteBrokerJoin must preserve adminManaged=false")
	assert.Equal(t, "1.2.3", gotJoined.Version)

	// 3. Owner label PATCH racing with a concurrent admin_managed change
	//    preserves the concurrent change and does not attribute it in the
	//    owner's audit event.
	capture.events = nil
	flipStore.target = broker.ID
	flipStore.value = true
	fault.Arm()
	rec = doBrokerRequest(t, srv, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID,
		map[string]any{"labels": map[string]string{"env": "dev"}}, bearer(ownerJWT))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, flipStore.hooked, "expected concurrent admin_managed flip during owner PATCH")
	got, err = s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.True(t, got.AdminManaged, "owner label PATCH must preserve concurrent admin_managed=true")
	assert.Equal(t, "dev", got.Labels["env"])
	require.NotEmpty(t, capture.events)
	ev := capture.events[len(capture.events)-1]
	assert.NotContains(t, ev.Details, "admin_managed.old")
	assert.NotContains(t, ev.Details, "admin_managed.new")
}
