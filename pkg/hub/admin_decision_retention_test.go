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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const decisionRetentionAdminPath = "/api/v1/admin/decision-retention"

func decisionRetentionAdminServer(t *testing.T, on bool) (*Server, store.Store, *store.User, *store.User) {
	t.Helper()
	srv, s := testServerWithOps(t, nil)
	ctx := context.Background()
	admin, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "dr-admin", store.SystemRoleSuperAdmin))
	require.NoError(t, err)
	member, err := s.GetUser(ctx, hubConfigTokenUser(t, s, "dr-member", store.SystemRoleHubMember))
	require.NoError(t, err)
	if on {
		ops := srv.GetOperationalSettings()
		doc := fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.DecisionRetention)
		_, err := ops.Update(ctx, "experiments", json.RawMessage(doc), "test", ops.SectionRevision("experiments"), "managed")
		require.NoError(t, err)
	}
	return srv, s, admin, member
}

func decodeDecisionRetention(t *testing.T, rec *httptest.ResponseRecorder) decisionRetentionResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body decisionRetentionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

// TestAdminDecisionRetention_ExperimentOff404 verifies GET and PUT return 404
// while hub.decision_retention is off and nothing is stored.
func TestAdminDecisionRetention_ExperimentOff404(t *testing.T) {
	srv, _, admin, _ := decisionRetentionAdminServer(t, false)
	rec := doRequestAsUser(t, srv, admin, http.MethodGet, decisionRetentionAdminPath, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled": true, "retention_days": 30, "expected_revision": 0,
	})
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Equal(t, int64(0), srv.GetOperationalSettings().SectionRevision(opsettings.DecisionRetentionSectionName))
}

// TestAdminDecisionRetention_NonAdmin403 verifies non-admin users receive 403.
func TestAdminDecisionRetention_NonAdmin403(t *testing.T) {
	srv, _, _, member := decisionRetentionAdminServer(t, true)
	rec := doRequestAsUser(t, srv, member, http.MethodGet, decisionRetentionAdminPath, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, member, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled": true, "expected_revision": 0,
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestAdminDecisionRetention_TokenRefused verifies bearer tokens (UATs) are
// refused on this session-only route (authzop catalog TestRef).
func TestAdminDecisionRetention_TokenRefused(t *testing.T) {
	srv, _, admin, _ := decisionRetentionAdminServer(t, true)
	key := mintHubConfigToken(t, srv, admin.ID, hubBoundary(), "hub_config:read", "hub_config:update")
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		var body any
		if method == http.MethodPut {
			body = map[string]any{"enabled": true, "retention_days": 30, "expected_revision": 0}
		}
		rec := doRequestWithToken(t, srv, key, method, decisionRetentionAdminPath, body)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s with UAT must be refused: %s", method, rec.Body.String())
		_, details := errorCodeAndDetails(t, rec)
		require.Equal(t, "IRREVERSIBLE_CASCADE", details["reason"], "%s: %s", method, rec.Body.String())
	}
	assert.False(t, srv.GetOperationalSettings().DecisionRetention().Enabled)
}

// TestAdminDecisionRetention_AdminRoundTrip verifies defaults, updates, null
// resets, bounds validation, and unknown-field rejection (incl. project_overrides).
func TestAdminDecisionRetention_AdminRoundTrip(t *testing.T) {
	srv, _, admin, _ := decisionRetentionAdminServer(t, true)

	rec := doRequestAsUser(t, srv, admin, http.MethodGet, decisionRetentionAdminPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"enabled":false,"retention_days":null,"sweep":{"page_size":200,"tick_budget_seconds":30,"tick_interval_minutes":60},"revision":0,"runs":[]}`, rec.Body.String())

	got := decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled": true, "retention_days": 45, "sweep": map[string]any{"page_size": 100}, "expected_revision": 0,
	}))
	assert.True(t, got.Enabled)
	require.NotNil(t, got.RetentionDays)
	assert.Equal(t, 45, *got.RetentionDays)
	assert.Equal(t, decisionRetentionSweepJSON{PageSize: 100, TickBudgetSeconds: 30, TickIntervalMinutes: 60}, got.Sweep)
	require.Positive(t, got.Revision)

	// Sweep-only update does not require expected_revision.
	got = decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"sweep": map[string]any{"tick_interval_minutes": 15, "page_size": nil},
	}))
	assert.Equal(t, decisionRetentionSweepJSON{PageSize: 200, TickBudgetSeconds: 30, TickIntervalMinutes: 15}, got.Sweep)
	assert.True(t, got.Enabled)

	// Null retention_days resets to keep forever; null sweep resets tuning.
	got = decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"retention_days": nil, "sweep": nil, "expected_revision": got.Revision,
	}))
	assert.Nil(t, got.RetentionDays)
	assert.Equal(t, decisionRetentionSweepJSON{PageSize: 200, TickBudgetSeconds: 30, TickIntervalMinutes: 60}, got.Sweep)

	// Bounds and unknown fields fail with 400.
	for _, bad := range []map[string]any{
		{"retention_days": 29, "expected_revision": got.Revision},
		{"retention_days": 36501, "expected_revision": got.Revision},
		{"project_overrides": map[string]any{}, "expected_revision": got.Revision},
		{"sweep": map[string]any{"page_size": 10}},
	} {
		rec = doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, bad)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "payload %v should be 400: %s", bad, rec.Body.String())
	}
}

// TestAdminDecisionRetention_RevisionConflictAndAudit verifies expected_revision
// enforcement, 409 conflicts, and mutation audit logging.
func TestAdminDecisionRetention_RevisionConflictAndAudit(t *testing.T) {
	srv, s, admin, _ := decisionRetentionAdminServer(t, true)

	// Missing expected_revision when changing enabled/retention_days -> 400.
	rec := doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled": true, "retention_days": 30,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// Valid update -> 200 and audit record written.
	got := decodeDecisionRetention(t, doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled": true, "retention_days": 30, "expected_revision": 0,
	}))
	require.Positive(t, got.Revision)

	// Stale expected_revision -> 409.
	rec = doRequestAsUser(t, srv, admin, http.MethodPut, decisionRetentionAdminPath, map[string]any{
		"enabled": false, "expected_revision": 0,
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	// Verify mutation audit record exists with surface admin-decision-retention.
	audits, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		TargetID: opsettings.DecisionRetentionSectionName,
		Limit:    100,
	})
	require.NoError(t, err)
	require.NotEmpty(t, audits)
	assert.Equal(t, decisionRetentionAuditSurface, audits[0].ExecutorID)
	assert.Contains(t, audits[0].BeforeSummary, "enabled=false,retention_days=null")
	assert.Contains(t, audits[0].AfterSummary, "enabled=true,retention_days=30")
}
