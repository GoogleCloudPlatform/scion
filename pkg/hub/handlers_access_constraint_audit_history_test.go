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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type constraintHistoryListStore struct {
	store.Store
	listErr   error
	listCalls int
}

func (s *constraintHistoryListStore) ListConstraintHistory(ctx context.Context, constraintID string) ([]*store.AccessConstraintHistory, error) {
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.Store.ListConstraintHistory(ctx, constraintID)
}

func appendConstraintHistory(t *testing.T, s store.Store, entries ...*store.AccessConstraintHistory) {
	t.Helper()
	require.NoError(t, s.WithTx(t.Context(), func(tx store.Store) error {
		for _, entry := range entries {
			if err := tx.AppendConstraintHistoryTx(t.Context(), entry); err != nil {
				return err
			}
		}
		return nil
	}))
}

func historyEntry(constraintID, eventID string, occurredAt time.Time) *store.AccessConstraintHistory {
	before, after := int64(1), int64(2)
	return &store.AccessConstraintHistory{
		EventID:           eventID,
		ConstraintID:      constraintID,
		OccurredAt:        occurredAt,
		Operation:         "update",
		ActorKind:         "user",
		ActorID:           DevUserID,
		CorrelationID:     "request-2405",
		BeforeRevision:    &before,
		AfterRevision:     &after,
		Classification:    "tighten",
		PreviewID:         "preview-2405",
		DraftHash:         "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ImpactCountsJSON:  `{"agents":1,"users":2,"projects":3}`,
		ChangedFieldsJSON: `["maximum_permissions"]`,
	}
}

func decodeAuditPage(t *testing.T, body []byte) auditListResponse {
	t.Helper()
	var page auditListResponse
	require.NoError(t, json.Unmarshal(body, &page))
	return page
}

func TestConstraintAuditHistory_TuplePaginationIsStable(t *testing.T) {
	srv, s := b7TestServer(t)
	constraint := b7SeedConstraint(t, s, "tuple-history")
	other := b7SeedConstraint(t, s, "tuple-history-other")
	base := time.Date(2026, 10, 2, 1, 2, 3, 456, time.UTC)

	appendConstraintHistory(t, s,
		historyEntry(constraint.ID, "00000000-0000-0000-0000-000000000003", base),
		historyEntry(constraint.ID, "00000000-0000-0000-0000-000000000002", base),
		historyEntry(constraint.ID, "00000000-0000-0000-0000-000000000001", base.Add(-time.Second)),
		historyEntry(other.ID, "00000000-0000-0000-0000-000000000009", base),
	)

	first := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/"+constraint.ID+"/audit?pageSize=1", nil)
	require.Equal(t, http.StatusOK, first.Code, "body: %s", first.Body.String())
	firstPage := decodeAuditPage(t, first.Body.Bytes())
	require.Len(t, firstPage.Items, 1)
	assert.Equal(t, "00000000-0000-0000-0000-000000000003", firstPage.Items[0].ID)
	assert.NotEmpty(t, firstPage.NextPageToken)
	assert.Equal(t, 3, firstPage.TotalCount)

	// A newer commit between pages must not perturb the strict older-than walk.
	appendConstraintHistory(t, s,
		historyEntry(constraint.ID, "00000000-0000-0000-0000-000000000004", base.Add(time.Second)),
	)

	second := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/"+constraint.ID+"/audit?pageSize=1&pageToken="+firstPage.NextPageToken, nil)
	require.Equal(t, http.StatusOK, second.Code, "body: %s", second.Body.String())
	secondPage := decodeAuditPage(t, second.Body.Bytes())
	require.Len(t, secondPage.Items, 1)
	assert.Equal(t, "00000000-0000-0000-0000-000000000002", secondPage.Items[0].ID)
	assert.Equal(t, 4, secondPage.TotalCount, "totalCount is the current retained count, not a snapshot")

	third := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/"+constraint.ID+"/audit?pageSize=1&pageToken="+secondPage.NextPageToken, nil)
	require.Equal(t, http.StatusOK, third.Code, "body: %s", third.Body.String())
	thirdPage := decodeAuditPage(t, third.Body.Bytes())
	require.Len(t, thirdPage.Items, 1)
	assert.Equal(t, "00000000-0000-0000-0000-000000000001", thirdPage.Items[0].ID)
	assert.Empty(t, thirdPage.NextPageToken)

	seen := []string{firstPage.Items[0].ID, secondPage.Items[0].ID, thirdPage.Items[0].ID}
	assert.Equal(t, []string{
		"00000000-0000-0000-0000-000000000003",
		"00000000-0000-0000-0000-000000000002",
		"00000000-0000-0000-0000-000000000001",
	}, seen)
	for _, item := range append(append(firstPage.Items, secondPage.Items...), thirdPage.Items...) {
		assert.Equal(t, constraint.ID, item.ConstraintID)
		assert.NotEqual(t, "00000000-0000-0000-0000-000000000009", item.ID)
	}

	// A cursor is bound to the live constraint and cannot be replayed elsewhere.
	cross := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/"+other.ID+"/audit?pageToken="+firstPage.NextPageToken, nil)
	assert.Equal(t, http.StatusBadRequest, cross.Code, "body: %s", cross.Body.String())
}

func TestConstraintAuditHistory_PageSizeBounds(t *testing.T) {
	srv, s := b7TestServer(t)
	constraint := b7SeedConstraint(t, s, "page-size-history")
	base := time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	entries := make([]*store.AccessConstraintHistory, 201)
	for i := range entries {
		entries[i] = historyEntry(constraint.ID,
			fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1), base.Add(time.Duration(i)*time.Second))
	}
	appendConstraintHistory(t, s, entries...)

	for _, tc := range []struct {
		name     string
		query    string
		expected int
	}{
		{name: "default", expected: 50},
		{name: "zero", query: "?pageSize=0", expected: 50},
		{name: "negative", query: "?pageSize=-1", expected: 50},
		{name: "non-numeric", query: "?pageSize=nope", expected: 50},
		{name: "maximum", query: "?pageSize=200", expected: 200},
		{name: "clamped", query: "?pageSize=500", expected: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doRequest(t, srv, http.MethodGet,
				"/api/v1/admin/access-constraints/"+constraint.ID+"/audit"+tc.query, nil)
			require.Equal(t, http.StatusOK, resp.Code, "body: %s", resp.Body.String())
			page := decodeAuditPage(t, resp.Body.Bytes())
			assert.Len(t, page.Items, tc.expected)
			assert.Equal(t, 201, page.TotalCount)
			assert.NotEmpty(t, page.NextPageToken)
		})
	}
}

func TestConstraintAuditHistory_InvalidTokensFailBeforeHistoryQuery(t *testing.T) {
	srv, realStore := b7TestServer(t)
	constraint := b7SeedConstraint(t, realStore, "invalid-token-history")
	spy := &constraintHistoryListStore{Store: realStore}
	srv.store = spy

	unknownVersion, err := json.Marshal(map[string]any{
		"version":      2,
		"constraintId": constraint.ID,
		"occurredAt":   time.Now().UTC().Format(time.RFC3339Nano),
		"eventId":      "event",
	})
	require.NoError(t, err)
	unknownToken := base64.RawURLEncoding.EncodeToString(unknownVersion)

	validCursor := fmt.Sprintf(
		`{"version":1,"constraintId":%q,"occurredAt":"2026-10-02T01:02:03Z","eventId":"event"}`,
		constraint.ID,
	)
	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "malformed base64", token: "not-base64!"},
		{name: "incomplete object", token: base64.RawURLEncoding.EncodeToString([]byte(`{"version":1}`))},
		{name: "unknown version", token: unknownToken},
		{name: "malformed timestamp", token: base64.RawURLEncoding.EncodeToString([]byte(`{"version":1,"constraintId":"x","occurredAt":"not-time","eventId":"e"}`))},
		{name: "unknown field", token: base64.RawURLEncoding.EncodeToString([]byte(validCursor[:len(validCursor)-1] + `,"unknown":"value"}`))},
		{name: "trailing JSON value", token: base64.RawURLEncoding.EncodeToString([]byte(validCursor + ` {}`))},
		{name: "encoded token over 2048 characters", token: strings.Repeat("a", 2049)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doRequest(t, srv, http.MethodGet,
				"/api/v1/admin/access-constraints/"+constraint.ID+"/audit?pageToken="+tc.token, nil)
			assert.Equal(t, http.StatusBadRequest, resp.Code, "body: %s", resp.Body.String())
			assert.Zero(t, spy.listCalls, "invalid cursor must fail before ListConstraintHistory")
		})
	}
}

func TestConstraintAuditHistory_PrivacySafeNotFound(t *testing.T) {
	srv, s := b7TestServer(t)
	constraint := b7SeedConstraint(t, s, "privacy-history")
	projectA := pvSeedProject(t, s, "privacy-project-a")
	projectB := pvSeedProject(t, s, "privacy-project-b")
	projectConstraint := b7SeedConstraint(t, s, "privacy-project-history")
	projectConstraint.ScopeType = store.RoleScopeProject
	projectConstraint.ScopeID = projectA
	_, err := s.UpdateAccessConstraint(t.Context(), projectConstraint, projectConstraint.Revision)
	require.NoError(t, err)

	missing := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/00000000-0000-0000-0000-000000000099/audit", nil)
	require.Equal(t, http.StatusNotFound, missing.Code)
	wantBody := missing.Body.String()

	// The full production middleware chain keeps authentication fail-closed but
	// normalizes its outward failure to the endpoint's absent-resource shape.
	unauthenticated := doRequestNoAuth(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/"+constraint.ID+"/audit", nil)
	assert.Equal(t, http.StatusNotFound, unauthenticated.Code)
	assert.Equal(t, wantBody, unauthenticated.Body.String())
	assert.Equal(t, missing.Header().Get("Content-Type"), unauthenticated.Header().Get("Content-Type"))

	denied := setupNonAdminUser(t, s, []string{PermissionConstraintRead})
	deniedResp := doRequestAsIdentity(t, srv, denied, http.MethodGet,
		"/api/v1/admin/access-constraints/"+constraint.ID+"/audit", nil)
	assert.Equal(t, http.StatusNotFound, deniedResp.Code)
	assert.Equal(t, wantBody, deniedResp.Body.String())

	wrongProject := NewScopedUserIdentity(
		NewAuthenticatedUser(DevUserID, "dev@localhost", "Development User", "admin", "api"),
		projectB,
		[]string{"hub:audit:read"},
	)
	wrongScopeResp := doRequestAsIdentity(t, srv, wrongProject, http.MethodGet,
		"/api/v1/admin/access-constraints/"+projectConstraint.ID+"/audit", nil)
	assert.Equal(t, http.StatusNotFound, wrongScopeResp.Code)
	assert.Equal(t, wantBody, wrongScopeResp.Body.String())

	appendConstraintHistory(t, s, historyEntry(constraint.ID,
		"00000000-0000-0000-0000-000000000010", time.Now().UTC()))
	require.NoError(t, s.DeleteAccessConstraint(t.Context(), constraint.ID))
	deleted := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/"+constraint.ID+"/audit", nil)
	assert.Equal(t, http.StatusNotFound, deleted.Code)
	assert.Equal(t, wantBody, deleted.Body.String())
	rows, listErr := s.ListConstraintHistory(t.Context(), constraint.ID)
	require.NoError(t, listErr)
	assert.Empty(t, rows, "live constraint deletion must cascade retained history")
}

func TestConstraintAuditHistory_StoreUnavailableIsExplicit(t *testing.T) {
	srv, realStore := b7TestServer(t)
	constraint := b7SeedConstraint(t, realStore, "unavailable-history")
	srv.store = &constraintHistoryListStore{Store: realStore, listErr: errors.New("history store unavailable")}

	resp := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/"+constraint.ID+"/audit", nil)
	assert.Equal(t, http.StatusInternalServerError, resp.Code, "body: %s", resp.Body.String())
	assert.NotContains(t, resp.Body.String(), `"items":[]`)
}

func TestConstraintAuditHistory_OmitsEmptyCorrelationID(t *testing.T) {
	srv, s := b7TestServer(t)
	constraint := b7SeedConstraint(t, s, "empty-correlation-history")
	entry := historyEntry(constraint.ID,
		"00000000-0000-0000-0000-000000000011", time.Now().UTC())
	entry.CorrelationID = ""
	appendConstraintHistory(t, s, entry)

	response := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/"+constraint.ID+"/audit", nil)
	require.Equal(t, http.StatusOK, response.Code, "body: %s", response.Body.String())
	var payload struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	require.Len(t, payload.Items, 1)
	assert.NotContains(t, payload.Items[0], "correlationId")
}

func TestConstraintAuditHistory_CreateIdentityFlowsThroughEndpoint(t *testing.T) {
	srv, s := b7TestServer(t)
	targetUserID := pvSeedUser(t, s, "endpoint-history-target")
	principalType := "user"
	draft := &store.AccessConstraint{
		Name:                 "endpoint-history-create",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &principalType,
		SubjectPrincipalID:   &targetUserID,
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "prove retained endpoint identity",
		CreatedBy:            DevUserID,
	}
	operation, err := auditevent.NewOperationContext("request-audit-create")
	require.NoError(t, err)
	ctx := auditevent.ContextWithOperation(t.Context(), operation)
	result, err := commitAuditedConstraint(t, ctx, srv.governanceService, srv.previewService,
		draft, pvTestActor(DevUserID))
	require.NoError(t, err)

	response := doRequest(t, srv, http.MethodGet,
		"/api/v1/admin/access-constraints/"+result.Constraint.ID+"/audit", nil)
	require.Equal(t, http.StatusOK, response.Code, "body: %s", response.Body.String())
	page := decodeAuditPage(t, response.Body.Bytes())
	require.Len(t, page.Items, 1)
	assert.Equal(t, result.AuditID, page.Items[0].ID)
	assert.Equal(t, result.Constraint.ID, page.Items[0].ConstraintID)
	assert.Equal(t, "create", page.Items[0].Operation)
	assert.Equal(t, "request-audit-create", page.Items[0].CorrelationID)
	assert.Equal(t, result.Constraint.Revision, mustParseRevision(t, page.Items[0].AfterRevision))

	history, err := s.ListConstraintHistory(t.Context(), result.Constraint.ID)
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, history[0].EventID, page.Items[0].ID)
}

func mustParseRevision(t *testing.T, revision *string) int64 {
	t.Helper()
	require.NotNil(t, revision)
	parsed, err := strconv.ParseInt(*revision, 10, 64)
	require.NoError(t, err)
	return parsed
}
