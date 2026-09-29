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

// Package hub — F.2a tests for check 9 (the record-race rule), the agent
// secret list, and the material selection audit event. See
// F/design/f2-material-selection.md section 8.2.
package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// TestAgentSecretRead_RecordReplacedBetweenMetaAndGetNotDelivered (v6, O-2)
// covers check 9's ID comparison, plus the record-deleted case: the fake
// backend's Get returns a value with an empty ID and SecretType internal,
// as the GCP fallback does when there is no DB record. Neither delivers a
// value.
func TestAgentSecretRead_RecordReplacedBetweenMetaAndGetNotDelivered(t *testing.T) {
	t.Run("id_changed", func(t *testing.T) {
		f := newMaterialFixture(t, "record-replaced-id")
		seedSecret(t, f.Server.secretBackend, "RACE_ID_KEY", "v", "", "", f.ProjectID)

		race := &raceSecretBackend{SecretBackend: f.Server.secretBackend}
		race.overrideGet = func(sv *secret.SecretWithValue, err error) (*secret.SecretWithValue, error) {
			if err != nil || sv == nil {
				return sv, err
			}
			cp := *sv
			cp.ID = "a-different-id"
			return &cp, nil
		}
		f.Server.SetSecretBackend(race)

		rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/RACE_ID_KEY", nil, f.Token)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 (record_changed -> unavailable), got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("record_deleted", func(t *testing.T) {
		f := newMaterialFixture(t, "record-replaced-deleted")
		seedSecret(t, f.Server.secretBackend, "RACE_DELETED_KEY", "v", "", "", f.ProjectID)

		race := &raceSecretBackend{SecretBackend: f.Server.secretBackend}
		race.overrideGet = func(sv *secret.SecretWithValue, err error) (*secret.SecretWithValue, error) {
			return &secret.SecretWithValue{
				SecretMeta: secret.SecretMeta{ID: "", SecretType: store.SecretTypeInternal},
				Value:      "leaked-if-delivered",
			}, nil
		}
		f.Server.SetSecretBackend(race)

		rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/RACE_DELETED_KEY", nil, f.Token)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 (record_changed -> unavailable), got %d: %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "leaked-if-delivered") {
			t.Fatalf("value must never be delivered on record_changed: %s", rec.Body.String())
		}
	})
}

// TestAgentSecretRead_SharingDisabledBetweenMetaAndGetNotDelivered (O-1)
// covers check 9: AllowProgeny turned off between GetMeta and Get, with the
// same ID and a bumped Version, still denies delivery.
func TestAgentSecretRead_SharingDisabledBetweenMetaAndGetNotDelivered(t *testing.T) {
	f := newMaterialFixture(t, "sharing-disabled-race")
	seedSecret(t, f.Server.secretBackend, "RACE_SHARING_KEY", "v", "", "", f.ProjectID)

	race := &raceSecretBackend{SecretBackend: f.Server.secretBackend}
	race.overrideGet = func(sv *secret.SecretWithValue, err error) (*secret.SecretWithValue, error) {
		if err != nil || sv == nil {
			return sv, err
		}
		cp := *sv
		cp.AllowProgeny = !cp.AllowProgeny
		cp.Version = cp.Version + 1
		return &cp, nil
	}
	f.Server.SetSecretBackend(race)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/RACE_SHARING_KEY", nil, f.Token)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 (record_changed -> unavailable), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentSecretRead_MetaFieldChangedAtSameVersionNotDelivered (v5, O-3)
// covers check 9: UpdateSecretMeta is a read-modify-write with no version
// predicate, so two concurrent updates can share a Version with different
// metadata. Comparing AllowProgeny, CreatedBy and ScopeID closes that
// window even at the same ID and Version.
func TestAgentSecretRead_MetaFieldChangedAtSameVersionNotDelivered(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(sv *secret.SecretWithValue)
	}{
		{"allow_progeny", func(sv *secret.SecretWithValue) { sv.AllowProgeny = !sv.AllowProgeny }},
		{"created_by", func(sv *secret.SecretWithValue) { sv.CreatedBy = "someone-else" }},
		{"scope_id", func(sv *secret.SecretWithValue) { sv.ScopeID = "a-different-scope" }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newMaterialFixture(t, "meta-field-changed-"+tc.name)
			seedSecret(t, f.Server.secretBackend, "META_CHANGE_KEY", "v", "", "", f.ProjectID)

			race := &raceSecretBackend{SecretBackend: f.Server.secretBackend}
			race.overrideGet = func(sv *secret.SecretWithValue, err error) (*secret.SecretWithValue, error) {
				if err != nil || sv == nil {
					return sv, err
				}
				cp := *sv
				tc.mutate(&cp)
				return &cp, nil
			}
			f.Server.SetSecretBackend(race)

			rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/META_CHANGE_KEY", nil, f.Token)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("expected 500 (record_changed), got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestMaterialSelection_NoValueAccessBeforeAuthorization (runtime subset)
// pins that the backend's Get is never called until check 7 or 8 allows.
func TestMaterialSelection_NoValueAccessBeforeAuthorization(t *testing.T) {
	f := newMaterialFixture(t, "no-value-before-auth")
	setBackfillCompleted(t, f.Store) // ceiling denies: no edge, post-backfill

	counting := &countingSecretBackend{SecretBackend: f.Server.secretBackend}
	seedSecret(t, counting, "NO_VALUE_KEY", "v", "", "", f.ProjectID)
	counting.getMetaCalls = 0
	counting.getCalls = 0
	f.Server.SetSecretBackend(counting)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/NO_VALUE_KEY", nil, f.Token)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if counting.getCalls != 0 {
		t.Fatalf("expected no value access before authorization, got %d Get calls", counting.getCalls)
	}
}

// TestMaterialSelection_NoProjectPermissionOnSecretResource (runtime
// subset; OQ-3) pins that a user-scope item never names project.secret_read
// (or any permission) and always carries the progeny grant, never a project
// grant.
func TestMaterialSelection_NoProjectPermissionOnSecretResource(t *testing.T) {
	f := newMaterialFixture(t, "no-project-permission-secret-resource")
	ctx := context.Background()

	_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "NO_PROJECT_PERM_KEY", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "NO_PROJECT_PERM_KEY",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
	})
	require.NoError(t, err)

	rec := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(rec)

	httpRec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/NO_PROJECT_PERM_KEY?scope=user", nil, f.Token)
	if httpRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", httpRec.Code, httpRec.Body.String())
	}

	if len(rec.events) != 1 || len(rec.events[0].Items) != 1 {
		t.Fatalf("expected 1 event with 1 item, got %d events", len(rec.events))
	}
	item := rec.events[0].Items[0]
	if item.Permission != "" {
		t.Fatalf("expected no permission named for a user-scope item (OQ-3), got %q", item.Permission)
	}
	if item.Grant != GrantProgeny {
		t.Fatalf("expected Grant=%s, got %q", GrantProgeny, item.Grant)
	}
}

// TestAgentListSecrets_OnlyReadableKeysListed pins that the list contains
// exactly the keys the agent could read: no internal secrets, no unshared
// user secrets, and no secrets authored outside the agent's lineage.
func TestAgentListSecrets_OnlyReadableKeysListed(t *testing.T) {
	f := newMaterialFixture(t, "only-readable-keys")
	ctx := context.Background()

	seedSecret(t, f.Server.secretBackend, "PROJECT_VISIBLE", "v", "", "", f.ProjectID)
	_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "PROJECT_INTERNAL", Value: "v", SecretType: store.SecretTypeInternal, Target: "PROJECT_INTERNAL",
		Scope: store.ScopeProject, ScopeID: f.ProjectID, CreatedBy: "test", UpdatedBy: "test",
	})
	require.NoError(t, err)
	_, _, err = f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "USER_SHARED", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "USER_SHARED",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
	})
	require.NoError(t, err)
	_, _, err = f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "USER_UNSHARED", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "USER_UNSHARED",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: false, CreatedBy: f.UserID, UpdatedBy: f.UserID,
	})
	require.NoError(t, err)

	outsiderAgentID := tid("outsider-list-agent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: outsiderAgentID, Slug: "outsider-list", Name: "outsider", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID},
		Created: time.Now(), Updated: time.Now(),
	}))
	_, _, err = f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "USER_OUTSIDE_LINEAGE", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "USER_OUTSIDE_LINEAGE",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: outsiderAgentID, UpdatedBy: outsiderAgentID,
	})
	require.NoError(t, err)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets", nil, f.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp AgentListSecretsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	got := map[string]bool{}
	for _, s := range resp.Secrets {
		got[s.Key] = true
	}

	if !got["PROJECT_VISIBLE"] {
		t.Errorf("expected PROJECT_VISIBLE to be listed")
	}
	if got["PROJECT_INTERNAL"] {
		t.Errorf("internal secret must never be listed")
	}
	if !got["USER_SHARED"] {
		t.Errorf("expected USER_SHARED to be listed")
	}
	if got["USER_UNSHARED"] {
		t.Errorf("unshared user secret must not be listed")
	}
	if got["USER_OUTSIDE_LINEAGE"] {
		t.Errorf("secret authored outside lineage must not be listed")
	}
}

// TestAgentListSecrets_ProgenyFilterMatchesPerItemCheck (O-3) pins parity
// between the list's progeny filter and the per-item check on a mixed
// fixture.
func TestAgentListSecrets_ProgenyFilterMatchesPerItemCheck(t *testing.T) {
	f := newMaterialFixture(t, "progeny-filter-parity")
	ctx := context.Background()

	keys := []string{"PARITY_SHARED", "PARITY_UNSHARED"}
	_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "PARITY_SHARED", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "PARITY_SHARED",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
	})
	require.NoError(t, err)
	_, _, err = f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "PARITY_UNSHARED", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "PARITY_UNSHARED",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: false, CreatedBy: f.UserID, UpdatedBy: f.UserID,
	})
	require.NoError(t, err)

	listRec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets?scope=user", nil, f.Token)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", listRec.Code, listRec.Body.String())
	}
	var listResp AgentListSecretsResponse
	require.NoError(t, json.NewDecoder(listRec.Body).Decode(&listResp))
	listed := map[string]bool{}
	for _, s := range listResp.Secrets {
		listed[s.Key] = true
	}

	for _, key := range keys {
		getRec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/"+key+"?scope=user", nil, f.Token)
		perItemAllowed := getRec.Code == http.StatusOK
		if listed[key] != perItemAllowed {
			t.Errorf("parity mismatch for %s: listed=%v per-item-allowed=%v", key, listed[key], perItemAllowed)
		}
	}
}

// TestMaterialAudit_SeparatesActorTargetAndSource (v6, O-1) pins that the
// audit event separates the actor, target agent, and sharing source, that
// project items carry Grant=project_secret_read and user items carry
// Grant=progeny, and that no runtime item ever carries a delivery grant such
// as project_association.
func TestMaterialAudit_SeparatesActorTargetAndSource(t *testing.T) {
	f := newMaterialFixture(t, "audit-separates-fields")
	ctx := context.Background()

	seedSecret(t, f.Server.secretBackend, "AUDIT_PROJECT_KEY", "v", "", "", f.ProjectID)
	_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "AUDIT_USER_KEY", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "AUDIT_USER_KEY",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
	})
	require.NoError(t, err)

	rec := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(rec)

	rec1 := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{"AUDIT_PROJECT_KEY"}}, f.Token)
	if rec1.Code != http.StatusOK {
		t.Fatalf("fetch: expected 200, got %d: %s", rec1.Code, rec1.Body.String())
	}
	rec2 := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/AUDIT_USER_KEY?scope=user", nil, f.Token)
	if rec2.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	if len(rec.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(rec.events))
	}
	for _, e := range rec.events {
		if e.ActorID != f.AgentID {
			t.Errorf("expected actor %s, got %s", f.AgentID, e.ActorID)
		}
		if e.TargetAgent.AgentID != f.AgentID {
			t.Errorf("expected target agent %s, got %s", f.AgentID, e.TargetAgent.AgentID)
		}
		if e.TargetAgent.ProjectID != f.ProjectID {
			t.Errorf("expected target project %s, got %s", f.ProjectID, e.TargetAgent.ProjectID)
		}
		if e.ProvenanceRoot.ID != f.UserID {
			t.Errorf("expected provenance root %s, got %s", f.UserID, e.ProvenanceRoot.ID)
		}
		for _, item := range e.Items {
			if item.Grant != GrantProjectSecretRead && item.Grant != GrantProgeny {
				t.Errorf("unexpected grant %q; a runtime item must never carry a delivery grant such as project_association", item.Grant)
			}
			if item.Scope == store.ScopeUser && item.SharingSource == nil {
				t.Errorf("expected a SharingSource on the user-scope item")
			}
		}
	}
}

// TestAgentGetSecret_AuthFailureCompatEventUnchanged (v5, N-1; v6, N-2) pins
// that the validateAgentSecretAccess failure path emits exactly the compat
// event main writes today, with Derived=false and no CorrelationID, and no
// MaterialSelectionEvent.
func TestAgentGetSecret_AuthFailureCompatEventUnchanged(t *testing.T) {
	f := newMaterialFixture(t, "auth-failure-compat")

	rec := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(rec)

	wrongAgentID := tid("wrong-agent-auth-failure")
	httpRec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+wrongAgentID+"/secrets/SOME_KEY", nil, f.Token)
	if httpRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for agent ID mismatch, got %d: %s", httpRec.Code, httpRec.Body.String())
	}

	if len(rec.events) != 0 {
		t.Fatalf("expected no MaterialSelectionEvent on the auth-failure path, got %d", len(rec.events))
	}
	if len(rec.secretReadEvents) != 1 {
		t.Fatalf("expected 1 compat event, got %d", len(rec.secretReadEvents))
	}
	ev := rec.secretReadEvents[0]
	if ev.AgentID != wrongAgentID {
		t.Errorf("expected AgentID %s (the unverified path agent ID), got %s", wrongAgentID, ev.AgentID)
	}
	if ev.ProjectID != "" || ev.Scope != "" || ev.ScopeID != "" {
		t.Errorf("expected empty project/scope, got %+v", ev)
	}
	if ev.FailReason != "auth failed" {
		t.Errorf("expected FailReason %q, got %q", "auth failed", ev.FailReason)
	}
	if ev.Derived {
		t.Errorf("expected Derived=false")
	}
	if ev.CorrelationID != "" {
		t.Errorf("expected empty CorrelationID")
	}
}

// TestMaterialAudit_NilAuditLoggerSafe (v5, N-1) pins that fetch, get and
// list neither panic nor fail when the audit logger is nil.
func TestMaterialAudit_NilAuditLoggerSafe(t *testing.T) {
	f := newMaterialFixture(t, "nil-audit-logger-safe")
	seedSecret(t, f.Server.secretBackend, "NIL_LOGGER_KEY", "v", "", "", f.ProjectID)
	f.Server.SetAuditLogger(nil)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{"NIL_LOGGER_KEY"}}, f.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec2 := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/NIL_LOGGER_KEY", nil, f.Token)
	if rec2.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	rec3 := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets", nil, f.Token)
	if rec3.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", rec3.Code, rec3.Body.String())
	}
}

// TestMaterialAudit_EmittedWithoutAuditLoggerInterfaceChange (R-5) pins that
// the recording fake receives the event, and that a plain AuditLogger which
// does not implement materialSelectionAuditor falls back to slog rather than
// erroring — no AuditLogger interface change was needed.
func TestMaterialAudit_EmittedWithoutAuditLoggerInterfaceChange(t *testing.T) {
	f := newMaterialFixture(t, "no-interface-change")
	seedSecret(t, f.Server.secretBackend, "NO_IFACE_KEY", "v", "", "", f.ProjectID)

	rec := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(rec)
	httpRec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/NO_IFACE_KEY", nil, f.Token)
	if httpRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", httpRec.Code, httpRec.Body.String())
	}
	if len(rec.events) != 1 {
		t.Fatalf("expected the recording fake to receive 1 event, got %d", len(rec.events))
	}

	f.Server.SetAuditLogger(plainAuditLogger{})
	httpRec2 := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/NO_IFACE_KEY", nil, f.Token)
	if httpRec2.Code != http.StatusOK {
		t.Fatalf("expected 200 with a plain AuditLogger (slog fallback), got %d: %s", httpRec2.Code, httpRec2.Body.String())
	}
}
