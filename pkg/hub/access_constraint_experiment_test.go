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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccessConstraintsExperiment_GatingAndEnforcement(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Grant dev super-admin explicit constraint permissions so route checks
	// reach the experiment gate rather than RBAC denials.
	rd := createTestRoleDefinition(t, s, "exp-ac-admin", store.RoleScopeSystem,
		[]string{PermissionConstraintAdmin, PermissionConstraintRead, "agent.read", "agent.create", "agent.delete", "agent.list", "project.read"})
	pvSeedRoleBinding(t, s, rd.ID, "user", DevUserID, store.RoleScopeSystem, "")
	if srv.previewService == nil {
		srv.initBoundaryServices()
	}

	// 1. Flag off + no stored constraints: routes answer 404 and /api/v1/experiments reports false.
	expRec := doRequest(t, srv, http.MethodGet, "/api/v1/experiments", nil)
	require.Equal(t, http.StatusOK, expRec.Code)
	var expBody struct {
		Experiments map[string]bool `json:"experiments"`
	}
	require.NoError(t, json.Unmarshal(expRec.Body.Bytes(), &expBody))
	val, present := expBody.Experiments[experiments.AccessConstraints]
	assert.True(t, present, "hub.access_constraints must be present in GET /api/v1/experiments")
	assert.False(t, val, "hub.access_constraints must default to false")

	assertAllGatedOff := func(label, constraintID string) {
		t.Helper()
		gatedRoutes := []struct {
			method string
			path   string
			body   any
		}{
			{http.MethodGet, "/api/v1/admin/access-constraints", nil},
			{http.MethodPost, "/api/v1/admin/access-constraints", map[string]any{}},
			{http.MethodGet, "/api/v1/admin/access-constraints/" + constraintID, nil},
			{http.MethodPut, "/api/v1/admin/access-constraints/" + constraintID, map[string]any{}},
			{http.MethodDelete, "/api/v1/admin/access-constraints/" + constraintID, map[string]any{}},
			{http.MethodGet, "/api/v1/admin/access-constraints/" + constraintID + "/affected-principals", nil},
			{http.MethodGet, "/api/v1/admin/access-constraints/" + constraintID + "/audit", nil},
			{http.MethodPost, "/api/v1/admin/access-constraint-previews", map[string]any{}},
			{http.MethodGet, "/api/v1/admin/access-constraint-previews/job-1", nil},
			{http.MethodGet, "/api/v1/admin/access-constraint-previews/job-1/result", nil},
		}
		for _, rt := range gatedRoutes {
			rec := doRequest(t, srv, rt.method, rt.path, rt.body)
			assert.Equal(t, http.StatusNotFound, rec.Code, "%s: %s %s should return 404 (got %s)", label, rt.method, rt.path, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "route not found", "%s: %s %s must be refused by requireExperiment gate, not handler", label, rt.method, rt.path)
		}
	}
	assertAllGatedOff("flag-off-initial", "00000000-0000-0000-0000-000000000001")

	// 2. Flag on: routes behave unchanged (preview, create, list, get, audit, update, delete).
	setExperimentOverrides(t, srv, map[string]bool{experiments.AccessConstraints: true})

	expRec = doRequest(t, srv, http.MethodGet, "/api/v1/experiments", nil)
	require.Equal(t, http.StatusOK, expRec.Code)
	require.NoError(t, json.Unmarshal(expRec.Body.Bytes(), &expBody))
	assert.True(t, expBody.Experiments[experiments.AccessConstraints])

	targetUserID := pvSeedUser(t, s, "exp-ac-target")
	memberRole := createTestRoleDefinition(t, s, "exp-ac-member-role", store.RoleScopeSystem,
		[]string{"agent.read", "agent.delete", "agent.list"})
	pvSeedRoleBinding(t, s, memberRole.ID, "user", targetUserID, store.RoleScopeSystem, "")

	draft := map[string]any{
		"name":    "exp-ac-boundary",
		"purpose": "Restrict target user to agent.read only",
		"subject": map[string]any{
			"kind":          "principal",
			"principalType": "user",
			"principalId":   targetUserID,
		},
		"scope":              map[string]any{"type": "system"},
		"maximumPermissions": []string{"agent.read"},
	}

	prevRec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/access-constraint-previews", map[string]any{
		"operation": "create",
		"draft":     draft,
	})
	require.Equal(t, http.StatusOK, prevRec.Code, "preview create: %s", prevRec.Body.String())
	var prevResp map[string]any
	require.NoError(t, json.Unmarshal(prevRec.Body.Bytes(), &prevResp))
	previewToken, _ := prevResp["previewToken"].(string)
	require.NotEmpty(t, previewToken)

	createPayload := map[string]any{"previewToken": previewToken}
	for k, v := range draft {
		createPayload[k] = v
	}
	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/access-constraints", createPayload)
	require.Equal(t, http.StatusCreated, createRec.Code, "create constraint: %s", createRec.Body.String())
	var created struct {
		ID       string `json:"id"`
		Revision string `json:"revision"`
	}
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	require.NotEmpty(t, created.ID)

	listRec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/access-constraints", nil)
	assert.Equal(t, http.StatusOK, listRec.Code)
	getRec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/access-constraints/"+created.ID, nil)
	assert.Equal(t, http.StatusOK, getRec.Code)
	auditRec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/access-constraints/"+created.ID+"/audit", nil)
	assert.Equal(t, http.StatusOK, auditRec.Code)

	// 3. Upgraded hub / flag toggled off with stored constraints:
	// enforcement MUST remain active (never widen access), including when
	// the experiments settings document is malformed.
	targetIdentity := NewAuthenticatedUser(targetUserID, "exp-ac-target@test.com", "Target", "member", "api")

	assertConstrained := func(label string) {
		t.Helper()
		allowDec := srv.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(targetIdentity),
			Credential: credentialContextForIdentity(targetIdentity),
			Resource:   Resource{Type: "agent", ID: "any"},
			Action:     ActionRead,
			Permission: "agent.read",
		})
		assert.True(t, allowDec.Allowed, "%s: permitted action agent.read should stay allowed", label)

		denyDec := srv.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(targetIdentity),
			Credential: credentialContextForIdentity(targetIdentity),
			Resource:   Resource{Type: "agent", ID: "any"},
			Action:     ActionDelete,
			Permission: "agent.delete",
		})
		assert.False(t, denyDec.Allowed, "%s: constrained action agent.delete must remain denied", label)

		listRes, err := srv.authzService.ResolveListScopes(ctx, targetIdentity, "agent.list")
		require.NoError(t, err)
		assert.True(t, listRes.Scopes.IsNone(), "%s: ResolveListScopes must still enforce stored constraint", label)
	}

	// Verify enforcement while flag is on.
	assertConstrained("flag-on")

	// Turn flag OFF explicitly and verify every route (with real created.ID)
	// returns the experiment gate's 404 while stored enforcement stays active.
	setExperimentOverrides(t, srv, map[string]bool{experiments.AccessConstraints: false})
	assertAllGatedOff("flag-off-with-stored", created.ID)
	assertConstrained("flag-off")

	// Simulate malformed experiments document (which forces all LayerServer experiments off).
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", json.RawMessage(`{not valid json`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	assert.False(t, srv.experimentEnabled(experiments.AccessConstraints))
	assert.Equal(t, http.StatusNotFound, doRequest(t, srv, http.MethodGet, "/api/v1/admin/access-constraints", nil).Code)
	assertConstrained("experiments-malformed")

	// Also verify constraint-bearing group protection remains active while flag is off.
	groupID := tid("exp-ac-group")
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID:        groupID,
		Name:      "Constrained Group",
		Slug:      "exp-ac-constrained-group",
		GroupType: store.GroupTypeExplicit,
		OwnerID:   targetUserID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}))
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:               "exp-ac-group-boundary",
		Purpose:            "Constrain group members",
		SubjectKind:        store.ConstraintSubjectGroupClosure,
		SubjectGroupID:     &groupID,
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		CreatedBy:          DevUserID,
		UpdatedBy:          DevUserID,
	})
	require.NoError(t, err)
	isCB, err := srv.isConstraintBearingGroup(ctx, groupID)
	require.NoError(t, err)
	assert.True(t, isCB, "constraint-bearing group guard must stay active while experiment is off")

	// 4. Re-enabling the flag restores management routes without restart, and update/delete work.
	setExperimentOverrides(t, srv, map[string]bool{experiments.AccessConstraints: true})
	getAfter := doRequest(t, srv, http.MethodGet, "/api/v1/admin/access-constraints/"+created.ID, nil)
	assert.Equal(t, http.StatusOK, getAfter.Code)

	delPrevRec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/access-constraint-previews", map[string]any{
		"operation":    "delete",
		"constraintId": created.ID,
		"baseRevision": created.Revision,
	})
	require.Equal(t, http.StatusOK, delPrevRec.Code, "preview delete: %s", delPrevRec.Body.String())
	var delPrev map[string]any
	require.NoError(t, json.Unmarshal(delPrevRec.Body.Bytes(), &delPrev))
	delRec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/access-constraints/"+created.ID, map[string]any{
		"previewToken": delPrev["previewToken"],
	})
	assert.Equal(t, http.StatusOK, delRec.Code, "delete constraint: %s", delRec.Body.String())
}

// lockedWarnLogBuffer guards a bytes.Buffer used as an slog.SetDefault sink
// so concurrent background goroutine logs cannot race with test reads or resets.
type lockedWarnLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedWarnLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedWarnLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedWarnLogBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func TestWarnStoredAccessConstraintsExperimentOff(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	var buf lockedWarnLogBuffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	// Flag off + 0 constraints: no warning.
	srv.WarnStoredAccessConstraintsExperimentOff(ctx)
	assert.Empty(t, buf.String(), "no warning expected when no constraints are stored")

	// Flag off + 1 stored constraint: warning logged.
	b7SeedConstraint(t, s, "warn-startup-constraint")
	srv.WarnStoredAccessConstraintsExperimentOff(ctx)
	assert.Contains(t, buf.String(), "hub.access_constraints")
	assert.Contains(t, buf.String(), "count=1")

	// Flag on + 1 stored constraint: no warning.
	buf.Reset()
	setExperimentOverrides(t, srv, map[string]bool{experiments.AccessConstraints: true})
	srv.WarnStoredAccessConstraintsExperimentOff(ctx)
	assert.Empty(t, buf.String(), "no warning expected when hub.access_constraints is on")
}
