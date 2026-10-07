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

// Package hub — tests for check 9 (the record-race rule), the agent secret
// list, and the material selection audit event.
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// assertRecordChangedAudited asserts that the last MaterialSelectionEvent
// recorded by rec carries exactly one item with Reason == ReasonRecordChanged:
// the record-race tests assert the audited reason, not only the HTTP status.
func assertRecordChangedAudited(t *testing.T, rec *recordingMaterialAuditor) {
	t.Helper()
	if len(rec.events) == 0 {
		t.Fatalf("expected at least 1 material selection event")
	}
	e := rec.events[len(rec.events)-1]
	if len(e.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(e.Items))
	}
	if e.Items[0].Reason != ReasonRecordChanged {
		t.Fatalf("expected reason %s, got %s", ReasonRecordChanged, e.Items[0].Reason)
	}
}

// assertBackendErrorAudited asserts that the last MaterialSelectionEvent
// recorded by rec carries exactly one item with Reason == ReasonBackendError:
// the backend-fault tests assert the audited reason, not only the HTTP
// status.
func assertBackendErrorAudited(t *testing.T, rec *recordingMaterialAuditor) {
	t.Helper()
	if len(rec.events) == 0 {
		t.Fatalf("expected at least 1 material selection event")
	}
	e := rec.events[len(rec.events)-1]
	if len(e.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(e.Items))
	}
	if e.Items[0].Reason != ReasonBackendError {
		t.Fatalf("expected reason %s, got %s", ReasonBackendError, e.Items[0].Reason)
	}
}

// TestAgentSecretRead_RecordReplacedBetweenMetaAndGetNotDelivered covers
// check 9's ID comparison, plus the record-deleted case: the fake
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

		auditor := newRecordingMaterialAuditor()
		f.Server.SetAuditLogger(auditor)

		rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/RACE_ID_KEY", nil, f.Token)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 (record_changed -> unavailable), got %d: %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "\"value\"") {
			t.Fatalf("value must never be delivered on record_changed: %s", rec.Body.String())
		}
		assertRecordChangedAudited(t, auditor)
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

		auditor := newRecordingMaterialAuditor()
		f.Server.SetAuditLogger(auditor)

		rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/RACE_DELETED_KEY", nil, f.Token)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 (record_changed -> unavailable), got %d: %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "leaked-if-delivered") {
			t.Fatalf("value must never be delivered on record_changed: %s", rec.Body.String())
		}
		assertRecordChangedAudited(t, auditor)
	})
}

// TestAgentGetSecret_ValueFetchErrorIsUnavailable covers check 9's Get call
// on the by-key get endpoint: a backend error fetching the value of an
// already-allowed item answers unavailable, not not found, with no backend
// error text in the response, and is audited as backend_error.
func TestAgentGetSecret_ValueFetchErrorIsUnavailable(t *testing.T) {
	f := newMaterialFixture(t, "get-value-fetch-error")
	seedSecret(t, f.Server.secretBackend, "GET_VALUE_FETCH_ERROR_KEY", "v", "", "", f.ProjectID)

	race := &raceSecretBackend{SecretBackend: f.Server.secretBackend}
	race.overrideGet = func(sv *secret.SecretWithValue, err error) (*secret.SecretWithValue, error) {
		return nil, errors.New("backend detail: connection reset by peer")
	}
	f.Server.SetSecretBackend(race)

	auditor := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(auditor)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet,
		"/api/v1/agents/"+f.AgentID+"/secrets/GET_VALUE_FETCH_ERROR_KEY", nil, f.Token)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "secret unavailable") {
		t.Fatalf("expected the fixed error message, got: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "connection reset by peer") {
		t.Fatalf("backend error text leaked into response: %s", rec.Body.String())
	}
	assertBackendErrorAudited(t, auditor)
}

// TestAgentSecretFetch_ValueFetchErrorIsUnavailable covers check 9's Get call
// on the bulk fetch endpoint: a backend error fetching the value of an
// already-allowed item answers entitled_but_unavailable, not not_found, with
// no backend error text in the response, and is audited as backend_error.
func TestAgentSecretFetch_ValueFetchErrorIsUnavailable(t *testing.T) {
	f := newMaterialFixture(t, "fetch-value-fetch-error")
	seedSecret(t, f.Server.secretBackend, "FETCH_VALUE_FETCH_ERROR_KEY", "v", "", "", f.ProjectID)

	race := &raceSecretBackend{SecretBackend: f.Server.secretBackend}
	race.overrideGet = func(sv *secret.SecretWithValue, err error) (*secret.SecretWithValue, error) {
		return nil, errors.New("backend detail: connection reset by peer")
	}
	f.Server.SetSecretBackend(race)

	auditor := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(auditor)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{"FETCH_VALUE_FETCH_ERROR_KEY"}}, f.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "connection reset by peer") {
		t.Fatalf("backend error text leaked into response: %s", rec.Body.String())
	}
	var resp secretFetchResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	if len(resp.Secrets) != 1 || resp.Secrets[0].Status != "entitled_but_unavailable" || resp.Secrets[0].Error != "secret unavailable" {
		t.Fatalf("expected entitled_but_unavailable/secret unavailable, got %+v", resp.Secrets)
	}
	assertBackendErrorAudited(t, auditor)
}

// TestAgentSecretFetch_RecordChangedIsUnavailable covers check 9's
// record-race rule on the bulk fetch endpoint: a project secret whose
// record changes between GetMeta and Get is entitled_but_unavailable, not
// not_found, because the project-level decision already allowed it. Mirrors
// TestAgentSecretRead_RecordReplacedBetweenMetaAndGetNotDelivered's two
// subtests (an ID change, and a deleted record) on the fetch endpoint
// instead of get.
func TestAgentSecretFetch_RecordChangedIsUnavailable(t *testing.T) {
	t.Run("id_changed", func(t *testing.T) {
		f := newMaterialFixture(t, "fetch-record-replaced-id")
		seedSecret(t, f.Server.secretBackend, "FETCH_RACE_ID_KEY", "v", "", "", f.ProjectID)

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

		auditor := newRecordingMaterialAuditor()
		f.Server.SetAuditLogger(auditor)

		rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
			secretFetchRequest{Keys: []string{"FETCH_RACE_ID_KEY"}}, f.Token)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 (per-item status), got %d: %s", rec.Code, rec.Body.String())
		}
		var resp secretFetchResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		if len(resp.Secrets) != 1 {
			t.Fatalf("expected 1 result, got %d", len(resp.Secrets))
		}
		got := resp.Secrets[0]
		if got.Status != "entitled_but_unavailable" || got.Error != "secret unavailable" || got.Value != "" {
			t.Fatalf("expected entitled_but_unavailable/\"secret unavailable\" with no value, got %+v", got)
		}
		assertRecordChangedAudited(t, auditor)
	})

	t.Run("record_deleted", func(t *testing.T) {
		f := newMaterialFixture(t, "fetch-record-replaced-deleted")
		seedSecret(t, f.Server.secretBackend, "FETCH_RACE_DELETED_KEY", "v", "", "", f.ProjectID)

		race := &raceSecretBackend{SecretBackend: f.Server.secretBackend}
		race.overrideGet = func(sv *secret.SecretWithValue, err error) (*secret.SecretWithValue, error) {
			return &secret.SecretWithValue{
				SecretMeta: secret.SecretMeta{ID: "", SecretType: store.SecretTypeInternal},
				Value:      "leaked-if-delivered",
			}, nil
		}
		f.Server.SetSecretBackend(race)

		auditor := newRecordingMaterialAuditor()
		f.Server.SetAuditLogger(auditor)

		rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
			secretFetchRequest{Keys: []string{"FETCH_RACE_DELETED_KEY"}}, f.Token)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 (per-item status), got %d: %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "leaked-if-delivered") {
			t.Fatalf("value must never be delivered on record_changed: %s", rec.Body.String())
		}
		var resp secretFetchResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		if len(resp.Secrets) != 1 {
			t.Fatalf("expected 1 result, got %d", len(resp.Secrets))
		}
		got := resp.Secrets[0]
		if got.Status != "entitled_but_unavailable" || got.Error != "secret unavailable" || got.Value != "" {
			t.Fatalf("expected entitled_but_unavailable/\"secret unavailable\" with no value, got %+v", got)
		}
		assertRecordChangedAudited(t, auditor)
	})
}

// TestAgentSecretRead_SharingDisabledBetweenMetaAndGetNotDelivered covers
// check 9: AllowProgeny turned off between GetMeta and Get, on a user-scope
// shared secret, with the same ID and a bumped Version, still denies
// delivery. A user-scope secret is used so that the AllowProgeny comparison
// itself trips the race, rather than the SecretType check that a
// project-scope secret would exercise instead.
func TestAgentSecretRead_SharingDisabledBetweenMetaAndGetNotDelivered(t *testing.T) {
	f := newMaterialFixture(t, "sharing-disabled-race")
	ctx := context.Background()
	_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "RACE_SHARING_KEY", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "RACE_SHARING_KEY",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
	})
	require.NoError(t, err)

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

	auditor := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(auditor)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/RACE_SHARING_KEY?scope=user", nil, f.Token)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 (record_changed -> unavailable), got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "\"value\"") {
		t.Fatalf("value must never be delivered on record_changed: %s", rec.Body.String())
	}
	assertRecordChangedAudited(t, auditor)
}

// TestAgentSecretRead_MetaFieldChangedAtSameVersionNotDelivered covers
// check 9: UpdateSecretMeta is a read-modify-write with no version
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

			auditor := newRecordingMaterialAuditor()
			f.Server.SetAuditLogger(auditor)

			rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/META_CHANGE_KEY", nil, f.Token)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("expected 500 (record_changed), got %d: %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "\"value\"") {
				t.Fatalf("value must never be delivered on record_changed: %s", rec.Body.String())
			}
			assertRecordChangedAudited(t, auditor)
		})
	}
}

// TestMaterialSelection_NoValueAccessBeforeAuthorization (runtime subset)
// pins that the backend's Get is never called until check 7 or 8 allows.
func TestMaterialSelection_NoValueAccessBeforeAuthorization(t *testing.T) {
	t.Run("project_scope_get_and_fetch", func(t *testing.T) {
		f := newMaterialFixture(t, "no-value-before-auth-project")
		setBackfillCompleted(t, f.Store) // ceiling denies: no edge, post-backfill

		counting := &countingSecretBackend{SecretBackend: f.Server.secretBackend}
		seedSecret(t, counting, "NO_VALUE_KEY", "v", "", "", f.ProjectID)
		counting.getMetaCalls = 0
		counting.getCalls = 0
		f.Server.SetSecretBackend(counting)

		rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/NO_VALUE_KEY", nil, f.Token)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("get: expected 404, got %d: %s", rec.Code, rec.Body.String())
		}
		if counting.getCalls != 0 {
			t.Fatalf("get: expected no value access before authorization, got %d Get calls", counting.getCalls)
		}

		fetchRec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
			secretFetchRequest{Keys: []string{"NO_VALUE_KEY"}}, f.Token)
		if fetchRec.Code != http.StatusOK {
			t.Fatalf("fetch: expected 200, got %d: %s", fetchRec.Code, fetchRec.Body.String())
		}
		var fetchResp secretFetchResponse
		require.NoError(t, json.NewDecoder(fetchRec.Body).Decode(&fetchResp))
		if len(fetchResp.Secrets) != 1 || fetchResp.Secrets[0].Status != "not_found" || fetchResp.Secrets[0].Value != "" {
			t.Fatalf("fetch: expected not_found with no value, got %+v", fetchResp.Secrets)
		}
		if counting.getCalls != 0 {
			t.Fatalf("fetch: expected no value access before authorization, got %d Get calls", counting.getCalls)
		}
	})

	t.Run("user_scope_sharing_disabled", func(t *testing.T) {
		f := newMaterialFixture(t, "no-value-before-auth-user")
		ctx := context.Background()

		counting := &countingSecretBackend{SecretBackend: f.Server.secretBackend}
		_, _, err := counting.Set(ctx, &secret.SetSecretInput{
			Name: "NO_VALUE_USER_KEY", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "NO_VALUE_USER_KEY",
			Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: false, CreatedBy: f.UserID, UpdatedBy: f.UserID,
		})
		require.NoError(t, err)
		counting.getCalls = 0
		f.Server.SetSecretBackend(counting)

		rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/NO_VALUE_USER_KEY?scope=user", nil, f.Token)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 (sharing disabled), got %d: %s", rec.Code, rec.Body.String())
		}
		if counting.getCalls != 0 {
			t.Fatalf("expected no value access before authorization, got %d Get calls", counting.getCalls)
		}
	})
}

// TestMaterialSelection_NoProjectPermissionOnSecretResource (runtime
// subset) pins that a user-scope item never names project.secret_read (or
// any permission) and always carries the progeny grant, never a project
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
		t.Fatalf("expected no permission named for a user-scope item, got %q", item.Permission)
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

// TestAgentListSecrets_ProgenyFilterMatchesPerItemCheck pins parity between
// the list's progeny filter (ListProgenySecrets eligibility, lineage
// containment and source liveness) and the per-item check
// (authorizeRuntimeUserItem) on a fixture that exercises every filter where
// the two paths could diverge: a shared row authored outside the target's
// lineage, a shared row authored by a soft-deleted lineage agent, and a
// shared row authored by a live lineage agent (included), alongside a
// shared and an unshared row authored directly by the root.
func TestAgentListSecrets_ProgenyFilterMatchesPerItemCheck(t *testing.T) {
	f := newMaterialFixture(t, "progeny-filter-parity")
	ctx := context.Background()

	// The target agent (childID) descends root -> midAgentID (live) ->
	// deletedLineageAgentID (soft-deleted) -> childID, so its ancestry
	// contains both a live and a soft-deleted lineage agent. outsiderAgentID
	// is a sibling of midAgentID: it is not in childID's ancestry at all.
	midAgentID := tid("parity-mid-agent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: midAgentID, Slug: "parity-mid", Name: "mid", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID},
		Created: time.Now(), Updated: time.Now(),
	}))
	deletedLineageAgentID := tid("parity-deleted-lineage-agent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: deletedLineageAgentID, Slug: "parity-deleted-lineage", Name: "deleted-lineage", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID, midAgentID},
		Created: time.Now(), Updated: time.Now(),
	}))
	childID := tid("parity-child-agent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: childID, Slug: "parity-child", Name: "child", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID, midAgentID, deletedLineageAgentID},
		Created: time.Now(), Updated: time.Now(),
	}))
	childToken, err := f.Server.agentTokenService.GenerateAgentToken(childID, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{f.UserID, midAgentID, deletedLineageAgentID})
	require.NoError(t, err)

	outsiderAgentID := tid("parity-outsider-agent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: outsiderAgentID, Slug: "parity-outsider", Name: "outsider", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID},
		Created: time.Now(), Updated: time.Now(),
	}))

	keys := []string{
		"PARITY_SHARED", "PARITY_UNSHARED",
		"PARITY_OUTSIDE_LINEAGE", "PARITY_SOFT_DELETED_LINEAGE_AUTHOR", "PARITY_LIVE_LINEAGE_AUTHOR",
	}
	rows := []struct {
		key          string
		allowProgeny bool
		createdBy    string
	}{
		{"PARITY_SHARED", true, f.UserID},
		{"PARITY_UNSHARED", false, f.UserID},
		{"PARITY_OUTSIDE_LINEAGE", true, outsiderAgentID},
		{"PARITY_SOFT_DELETED_LINEAGE_AUTHOR", true, deletedLineageAgentID},
		{"PARITY_LIVE_LINEAGE_AUTHOR", true, midAgentID},
	}
	for _, row := range rows {
		_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
			Name: row.key, Value: "v", SecretType: store.SecretTypeEnvironment, Target: row.key,
			Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: row.allowProgeny,
			CreatedBy: row.createdBy, UpdatedBy: row.createdBy,
		})
		require.NoError(t, err)
	}

	deletedLineageAgent, err := f.Store.GetAgent(ctx, deletedLineageAgentID)
	require.NoError(t, err)
	deletedLineageAgent.DeletedAt = time.Now()
	require.NoError(t, f.Store.UpdateAgent(ctx, deletedLineageAgent))

	listRec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+childID+"/secrets?scope=user", nil, childToken)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", listRec.Code, listRec.Body.String())
	}
	var listResp AgentListSecretsResponse
	require.NoError(t, json.NewDecoder(listRec.Body).Decode(&listResp))
	listed := map[string]bool{}
	for _, s := range listResp.Secrets {
		listed[s.Key] = true
	}

	if !listed["PARITY_LIVE_LINEAGE_AUTHOR"] {
		t.Errorf("expected PARITY_LIVE_LINEAGE_AUTHOR (live lineage author) to be listed")
	}
	if listed["PARITY_OUTSIDE_LINEAGE"] {
		t.Errorf("PARITY_OUTSIDE_LINEAGE (authored outside the lineage) must not be listed")
	}
	if listed["PARITY_SOFT_DELETED_LINEAGE_AUTHOR"] {
		t.Errorf("PARITY_SOFT_DELETED_LINEAGE_AUTHOR (soft-deleted author) must not be listed")
	}

	for _, key := range keys {
		getRec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+childID+"/secrets/"+key+"?scope=user", nil, childToken)
		perItemAllowed := getRec.Code == http.StatusOK
		if listed[key] != perItemAllowed {
			t.Errorf("parity mismatch for %s: listed=%v per-item-allowed=%v", key, listed[key], perItemAllowed)
		}
	}
}

// TestAgentGetSecret_InvalidScopeEmitsAudit covers check 6 on the by-key get
// endpoint: an invalid scope query parameter still emits the request's
// MaterialSelectionEvent, with RequestReason invalid_scope, rather than
// exiting silently.
func TestAgentGetSecret_InvalidScopeEmitsAudit(t *testing.T) {
	f := newMaterialFixture(t, "get-invalid-scope")
	seedSecret(t, f.Server.secretBackend, "INVALID_SCOPE_KEY", "v", "", "", f.ProjectID)

	auditor := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(auditor)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet,
		"/api/v1/agents/"+f.AgentID+"/secrets/INVALID_SCOPE_KEY?scope=bogus", nil, f.Token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(auditor.events) != 1 {
		t.Fatalf("expected 1 material selection event, got %d", len(auditor.events))
	}
	e := auditor.events[0]
	if e.RequestReason != ReasonInvalidScope {
		t.Fatalf("expected RequestReason %s, got %s", ReasonInvalidScope, e.RequestReason)
	}
	if len(e.Items) != 0 {
		t.Fatalf("expected no items, got %d", len(e.Items))
	}
}

// TestAgentListSecrets_DecisionErrorEmitsAudit covers the list's
// project-decision-error branch: a nil authz service makes the check-7
// decision fail, and the exit still emits the request's
// MaterialSelectionEvent with a request-level backend_error item, rather
// than exiting silently.
func TestAgentListSecrets_DecisionErrorEmitsAudit(t *testing.T) {
	f := newMaterialFixture(t, "list-decision-error")
	seedSecret(t, f.Server.secretBackend, "LIST_DECISION_ERROR_KEY", "v", "", "", f.ProjectID)
	f.Server.authzService = nil

	auditor := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(auditor)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets?scope=project", nil, f.Token)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(auditor.events) != 1 {
		t.Fatalf("expected 1 material selection event, got %d", len(auditor.events))
	}
	e := auditor.events[0]
	if len(e.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(e.Items))
	}
	item := e.Items[0]
	if item.Reason != ReasonBackendError {
		t.Fatalf("expected reason %s, got %s", ReasonBackendError, item.Reason)
	}
	if item.Grant != GrantProjectSecretRead {
		t.Fatalf("expected grant %s, got %s", GrantProjectSecretRead, item.Grant)
	}
}

// TestAgentListSecrets_ProjectListErrorEmitsAudit covers the list's project
// backend.List error exit: it still emits the request's
// MaterialSelectionEvent with a request-level backend_error item, rather
// than exiting silently, and answers with the fixed message rather than the
// backend error text.
func TestAgentListSecrets_ProjectListErrorEmitsAudit(t *testing.T) {
	f := newMaterialFixture(t, "list-project-list-error")
	seedSecret(t, f.Server.secretBackend, "LIST_PROJECT_LIST_ERROR_KEY", "v", "", "", f.ProjectID)
	f.Server.SetSecretBackend(&erroringListBackend{
		SecretBackend: f.Server.secretBackend,
		scope:         store.ScopeProject,
		err:           errors.New("injected project list failure"),
	})

	auditor := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(auditor)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets?scope=project", nil, f.Token)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to list secrets") {
		t.Fatalf("expected the fixed error message, got: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "injected project list failure") {
		t.Fatalf("backend error text leaked into the response body: %s", rec.Body.String())
	}
	if len(auditor.events) != 1 {
		t.Fatalf("expected 1 material selection event, got %d", len(auditor.events))
	}
	e := auditor.events[0]
	if len(e.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(e.Items))
	}
	item := e.Items[0]
	if item.Reason != ReasonBackendError || item.Grant != GrantProjectSecretRead || item.Scope != store.ScopeProject {
		t.Fatalf("expected a project-scope backend_error item, got %+v", item)
	}
}

// TestAgentListSecrets_ProgenyEligibleIDsErrorEmitsAudit covers the list's
// progenyEligibleSecretIDs error exit: it still emits the request's
// MaterialSelectionEvent with a request-level backend_error item, rather
// than exiting silently, and answers with the fixed message rather than the
// backend error text.
func TestAgentListSecrets_ProgenyEligibleIDsErrorEmitsAudit(t *testing.T) {
	f := newMaterialFixture(t, "list-progeny-eligible-error")
	seedSecret(t, f.Server.secretBackend, "LIST_PROGENY_ELIGIBLE_ERROR_KEY", "v", "", "", f.ProjectID)
	f.Server.store = &materialFailingStore{
		Store:                 f.Store,
		listProgenySecretsErr: errors.New("injected progeny list failure"),
	}

	auditor := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(auditor)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets?scope=user", nil, f.Token)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to list secrets") {
		t.Fatalf("expected the fixed error message, got: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "injected progeny list failure") {
		t.Fatalf("backend error text leaked into the response body: %s", rec.Body.String())
	}
	if len(auditor.events) != 1 {
		t.Fatalf("expected 1 material selection event, got %d", len(auditor.events))
	}
	e := auditor.events[0]
	if len(e.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(e.Items))
	}
	item := e.Items[0]
	if item.Reason != ReasonBackendError || item.Grant != GrantProgeny || item.Scope != store.ScopeUser {
		t.Fatalf("expected a user-scope backend_error item, got %+v", item)
	}
}

// TestAgentListSecrets_UserListErrorEmitsAudit covers the list's user
// backend.List error exit: it still emits the request's
// MaterialSelectionEvent with a request-level backend_error item, rather
// than exiting silently, and answers with the fixed message rather than the
// backend error text.
func TestAgentListSecrets_UserListErrorEmitsAudit(t *testing.T) {
	f := newMaterialFixture(t, "list-user-list-error")
	seedSecret(t, f.Server.secretBackend, "LIST_USER_LIST_ERROR_KEY", "v", "", "", f.ProjectID)
	f.Server.SetSecretBackend(&erroringListBackend{
		SecretBackend: f.Server.secretBackend,
		scope:         store.ScopeUser,
		err:           errors.New("injected user list failure"),
	})

	auditor := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(auditor)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets?scope=user", nil, f.Token)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to list secrets") {
		t.Fatalf("expected the fixed error message, got: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "injected user list failure") {
		t.Fatalf("backend error text leaked into the response body: %s", rec.Body.String())
	}
	if len(auditor.events) != 1 {
		t.Fatalf("expected 1 material selection event, got %d", len(auditor.events))
	}
	e := auditor.events[0]
	if len(e.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(e.Items))
	}
	item := e.Items[0]
	if item.Reason != ReasonBackendError || item.Grant != GrantProgeny || item.Scope != store.ScopeUser {
		t.Fatalf("expected a user-scope backend_error item, got %+v", item)
	}
}

// TestAgentListSecrets_ProjectDenialCarriesDetail covers the list's project
// branch when the check-7 decision denies: the project part of the list
// stays empty, but the denial is still recorded as a request-level item
// carrying a non-empty Detail, rather than leaving no trace of the project
// scope having been evaluated.
func TestAgentListSecrets_ProjectDenialCarriesDetail(t *testing.T) {
	f := newMaterialFixture(t, "list-project-denial")
	setBackfillCompleted(t, f.Store) // ceiling denial: no edge, post-backfill
	seedSecret(t, f.Server.secretBackend, "LIST_DENIAL_KEY", "v", "", "", f.ProjectID)

	auditor := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(auditor)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets?scope=project", nil, f.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp AgentListSecretsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	if len(resp.Secrets) != 0 {
		t.Fatalf("expected an empty project list, got %+v", resp.Secrets)
	}
	if len(auditor.events) != 1 {
		t.Fatalf("expected 1 material selection event, got %d", len(auditor.events))
	}
	e := auditor.events[0]
	if len(e.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(e.Items))
	}
	item := e.Items[0]
	if item.Reason != ReasonDeniedByPolicy {
		t.Fatalf("expected reason %s, got %s", ReasonDeniedByPolicy, item.Reason)
	}
	if item.Detail == "" {
		t.Fatalf("expected a non-empty Detail carrying Decision.Reason verbatim")
	}
}

// TestAgentListSecrets_LivenessErrorRecordsAuditItem covers the list's
// per-row liveness-check-error branch: the row is skipped from the visible
// list, but the request's MaterialSelectionEvent still records a
// backend_error item for it, rather than leaving no audit trace of the
// skipped row.
func TestAgentListSecrets_LivenessErrorRecordsAuditItem(t *testing.T) {
	f := newMaterialFixture(t, "list-liveness-error")
	ctx := context.Background()

	_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "LIST_LIVENESS_ERROR_KEY", Value: "v", SecretType: store.SecretTypeEnvironment, Target: "LIST_LIVENESS_ERROR_KEY",
		Scope: store.ScopeUser, ScopeID: f.UserID, AllowProgeny: true, CreatedBy: f.UserID, UpdatedBy: f.UserID,
	})
	require.NoError(t, err)

	// The whole-request precheck already calls GetUser three times for the
	// root user (check 5, then the agent standing check's ancestry-root
	// lookup and admission check, ptone/scion#3433) and must succeed; only
	// the per-row liveness check's own GetUser call, made after them, is
	// made to fail.
	f.Server.store = &materialFailingStore{
		Store:                f.Store,
		getUserErr:           errors.New("injected liveness lookup failure"),
		getUserErrAfterCalls: 3,
	}

	auditor := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(auditor)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets?scope=user", nil, f.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp AgentListSecretsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	if len(resp.Secrets) != 0 {
		t.Fatalf("expected the row to be skipped from the list, got %+v", resp.Secrets)
	}
	if len(auditor.events) != 1 {
		t.Fatalf("expected 1 material selection event, got %d", len(auditor.events))
	}
	e := auditor.events[0]
	if len(e.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(e.Items))
	}
	item := e.Items[0]
	if item.Reason != ReasonBackendError || item.Grant != GrantProgeny || item.Scope != store.ScopeUser {
		t.Fatalf("expected a user-scope backend_error item, got %+v", item)
	}
	if item.Key != "LIST_LIVENESS_ERROR_KEY" {
		t.Fatalf("expected the item to identify the skipped key, got %+v", item)
	}
	if item.Allowed {
		t.Fatalf("expected the skipped item to not be marked allowed, got %+v", item)
	}
}

// TestMaterialAudit_SeparatesActorTargetAndSource pins that the audit event
// separates the actor, target agent, and sharing source, that
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
		// Each request reads exactly one key, so a vacuous item loop below
		// (no items at all) must not let this test pass unnoticed.
		if len(e.Items) != 1 {
			t.Fatalf("expected 1 item, got %d", len(e.Items))
		}
		for _, item := range e.Items {
			switch item.Scope {
			case store.ScopeProject:
				if item.Grant != GrantProjectSecretRead {
					t.Errorf("expected a project-scope item to carry Grant=%s, got %q", GrantProjectSecretRead, item.Grant)
				}
			case store.ScopeUser:
				if item.Grant != GrantProgeny {
					t.Errorf("expected a user-scope item to carry Grant=%s, got %q", GrantProgeny, item.Grant)
				}
				if item.SharingSource == nil {
					t.Fatalf("expected a SharingSource on the user-scope item")
				}
				if item.SharingSource.Kind != "user" || item.SharingSource.ID != f.UserID {
					t.Errorf("expected SharingSource {user, %s}, got %+v", f.UserID, item.SharingSource)
				}
			default:
				t.Errorf("unexpected scope %q", item.Scope)
			}
		}
	}
}

// TestAgentGetSecret_AuthFailureCompatEventUnchanged pins that the
// validateAgentSecretAccess failure path emits exactly the compat
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
	if ev.SecretKey != "SOME_KEY" {
		t.Errorf("expected SecretKey %q, got %q", "SOME_KEY", ev.SecretKey)
	}
	if ev.Success {
		t.Errorf("expected Success=false")
	}
	if ev.Derived {
		t.Errorf("expected Derived=false")
	}
	if ev.CorrelationID != "" {
		t.Errorf("expected empty CorrelationID")
	}
}

// TestMaterialAudit_NilAuditLoggerSafe pins that fetch, get and list
// neither panic nor fail when the audit logger is nil.
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

// TestMaterialAudit_TypedNilAuditLoggerSafe pins that LogMaterialSelectionEvent
// does not panic on a typed nil *LogAuditLogger receiver, both called
// directly and reached through logMaterialSelection with s.auditLogger set
// to a nil *LogAuditLogger. That shape is distinct from a nil AuditLogger
// interface value (TestMaterialAudit_NilAuditLoggerSafe): the interface
// value itself is non-nil here (it carries a type), so the
// materialSelectionAuditor type assertion in logMaterialSelection succeeds
// and the call reaches l with l == nil.
func TestMaterialAudit_TypedNilAuditLoggerSafe(t *testing.T) {
	var l *LogAuditLogger
	require.NoError(t, l.LogMaterialSelectionEvent(context.Background(), &MaterialSelectionEvent{EventType: "material_selection"}))

	f := newMaterialFixture(t, "typed-nil-audit-logger-safe")
	f.Server.SetAuditLogger((*LogAuditLogger)(nil))
	f.Server.logMaterialSelection(context.Background(), &MaterialSelectionEvent{EventType: "material_selection"})
}

// TestMaterialAudit_ProductionSinkPreservesPerItemFields points
// LogAuditLogger directly at a buffer-backed slog.Handler and asserts that
// the per-item reason, Detail, Grant and SharingSource all reach the
// production audit sink for both a denied and an allowed item, and that no
// value is ever present.
func TestMaterialAudit_ProductionSinkPreservesPerItemFields(t *testing.T) {
	var buf bytes.Buffer
	logger := &LogAuditLogger{log: slog.New(slog.NewJSONHandler(&buf, nil))}

	event := &MaterialSelectionEvent{
		EventType:     "material_selection",
		CorrelationID: "corr-1",
		Purpose:       "runtime_read",
		Endpoint:      "get",
		Items: []MaterialSelectionEventItem{
			{
				Kind: MaterialKindSecret, Key: "DENIED_KEY", Scope: store.ScopeProject, ScopeID: "proj-1",
				Grant: GrantProjectSecretRead, Allowed: false, Selected: false,
				Reason: ReasonDeniedByPolicy, Permission: "project.secret_read",
				Detail: "ceiling check failed: injected edge lookup failure",
			},
			{
				Kind: MaterialKindSecret, Key: "ALLOWED_KEY", Scope: store.ScopeUser, ScopeID: "user-1",
				Grant:         GrantProgeny,
				SharingSource: &SourceRef{Kind: "user", ID: "user-1"},
				Allowed:       true, Selected: true, Reason: ReasonAllowed,
			},
		},
	}
	require.NoError(t, logger.LogMaterialSelectionEvent(context.Background(), event))

	out := buf.String()
	if strings.Contains(out, "\"value\"") {
		t.Fatalf("audit log must never contain a secret value: %s", out)
	}
	for _, want := range []string{
		`"reason":"denied_by_policy"`,
		`"detail":"ceiling check failed: injected edge lookup failure"`,
		`"grant":"project_secret_read"`,
		`"reason":"allowed"`,
		`"grant":"progeny"`,
		`"sharing_source_kind":"user"`,
		`"sharing_source_id":"user-1"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected the audit log to contain %s, got: %s", want, out)
		}
	}
}

// TestMaterialAudit_EmittedWithoutAuditLoggerInterfaceChange pins that the
// recording fake receives the event, and that a plain AuditLogger which
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
