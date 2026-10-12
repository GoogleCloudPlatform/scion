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
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// appliesWhenWire decodes only the appliesWhen object of a constraint
// response, keeping the raw strings so the test sees the exact wire form.
type appliesWhenWire struct {
	ID          string `json:"id"`
	Revision    string `json:"revision"`
	AppliesWhen struct {
		NotBefore *string `json:"notBefore"`
		ExpiresAt *string `json:"expiresAt"`
	} `json:"appliesWhen"`
}

func decodeAppliesWhen(t *testing.T, body []byte) appliesWhenWire {
	t.Helper()
	var w appliesWhenWire
	require.NoError(t, json.Unmarshal(body, &w), "body: %s", body)
	return w
}

func assertWireWindow(t *testing.T, label string, got appliesWhenWire, wantNotBefore, wantExpiresAt string) {
	t.Helper()
	require.NotNil(t, got.AppliesWhen.NotBefore, "%s: notBefore missing", label)
	require.NotNil(t, got.AppliesWhen.ExpiresAt, "%s: expiresAt missing", label)
	assert.Equal(t, wantNotBefore, *got.AppliesWhen.NotBefore, "%s: notBefore", label)
	assert.Equal(t, wantExpiresAt, *got.AppliesWhen.ExpiresAt, "%s: expiresAt", label)
}

// TestAccessConstraintWindow_OffsetInputsStoredAndReturnedUTC posts access
// windows with non-UTC offsets through the create and update handlers and
// checks that every response returns the same instants in UTC ("Z").
func TestAccessConstraintWindow_OffsetInputsStoredAndReturnedUTC(t *testing.T) {
	srv, s := b7TestServer(t)
	targetUserID := pvSeedUser(t, s, "utc-window-target")

	draft := func(notBefore, expiresAt string) map[string]any {
		return map[string]any{
			"name":    "utc-window-boundary",
			"purpose": "UTC window normalisation",
			"subject": map[string]any{
				"kind":          "principal",
				"principalType": "user",
				"principalId":   targetUserID,
			},
			"scope":              map[string]any{"type": "system"},
			"maximumPermissions": []string{"agent.read"},
			"appliesWhen": map[string]any{
				"notBefore": notBefore,
				"expiresAt": expiresAt,
			},
		}
	}

	// --- create: +02:00 and a four-digit half-hour offset (+05:45) ---
	const (
		createNotBefore = "2030-01-01T12:00:00+02:00"
		createExpiresAt = "2030-06-01T09:15:00.5+05:45"
		wantCreateNB    = "2030-01-01T10:00:00Z"
		wantCreateEA    = "2030-06-01T03:30:00.5Z"
	)
	previewResp := doRequest(t, srv, http.MethodPost, "/api/v1/admin/access-constraint-previews",
		map[string]any{"operation": "create", "draft": draft(createNotBefore, createExpiresAt)})
	require.Equal(t, http.StatusOK, previewResp.Code, "preview: %s", previewResp.Body.String())
	var preview PreviewResult
	require.NoError(t, json.Unmarshal(previewResp.Body.Bytes(), &preview))
	require.NotEmpty(t, preview.PreviewToken)

	createBody := draft(createNotBefore, createExpiresAt)
	createBody["previewToken"] = preview.PreviewToken
	createResp := doRequest(t, srv, http.MethodPost, "/api/v1/admin/access-constraints", createBody)
	require.Equal(t, http.StatusCreated, createResp.Code, "create: %s", createResp.Body.String())
	created := decodeAppliesWhen(t, createResp.Body.Bytes())
	assertWireWindow(t, "create response", created, wantCreateNB, wantCreateEA)

	getResp := doRequest(t, srv, http.MethodGet, "/api/v1/admin/access-constraints/"+created.ID, nil)
	require.Equal(t, http.StatusOK, getResp.Code, "get: %s", getResp.Body.String())
	assertWireWindow(t, "get after create", decodeAppliesWhen(t, getResp.Body.Bytes()), wantCreateNB, wantCreateEA)

	// The stored values are UTC instants equal to the inputs.
	stored, err := s.GetAccessConstraint(t.Context(), created.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.NotBefore)
	require.NotNil(t, stored.ExpiresAt)
	wantNB, _ := time.Parse(time.RFC3339Nano, createNotBefore)
	wantEA, _ := time.Parse(time.RFC3339Nano, createExpiresAt)
	assert.True(t, stored.NotBefore.Equal(wantNB), "stored notBefore %v != %v", stored.NotBefore, wantNB)
	assert.True(t, stored.ExpiresAt.Equal(wantEA), "stored expiresAt %v != %v", stored.ExpiresAt, wantEA)
	assert.Equal(t, time.UTC, stored.NotBefore.Location(), "stored notBefore location")
	assert.Equal(t, time.UTC, stored.ExpiresAt.Location(), "stored expiresAt location")

	// --- update: negative offset ---
	const (
		updNotBefore = "2031-03-01T00:00:00-07:00"
		updExpiresAt = "2031-03-02T23:59:59-03:30"
		wantUpdNB    = "2031-03-01T07:00:00Z"
		wantUpdEA    = "2031-03-03T03:29:59Z"
	)
	updPreviewResp := doRequest(t, srv, http.MethodPost, "/api/v1/admin/access-constraint-previews",
		map[string]any{
			"operation":    "update",
			"constraintId": created.ID,
			"baseRevision": created.Revision,
			"draft":        draft(updNotBefore, updExpiresAt),
		})
	require.Equal(t, http.StatusOK, updPreviewResp.Code, "update preview: %s", updPreviewResp.Body.String())
	var updPreview PreviewResult
	require.NoError(t, json.Unmarshal(updPreviewResp.Body.Bytes(), &updPreview))

	updBody := draft(updNotBefore, updExpiresAt)
	updBody["previewToken"] = updPreview.PreviewToken
	updResp := doRequestHeaders(t, srv, http.MethodPut, "/api/v1/admin/access-constraints/"+created.ID, updBody,
		map[string]string{"If-Match": fmt.Sprintf(`"%s"`, created.Revision)})
	require.Equal(t, http.StatusOK, updResp.Code, "update: %s", updResp.Body.String())
	assertWireWindow(t, "update response", decodeAppliesWhen(t, updResp.Body.Bytes()), wantUpdNB, wantUpdEA)

	getResp = doRequest(t, srv, http.MethodGet, "/api/v1/admin/access-constraints/"+created.ID, nil)
	require.Equal(t, http.StatusOK, getResp.Code)
	assertWireWindow(t, "get after update", decodeAppliesWhen(t, getResp.Body.Bytes()), wantUpdNB, wantUpdEA)
}

// TestDraftToStoreConstraint_WindowUTC covers the preview conversion, which
// builds a store.AccessConstraint from the draft without persisting it.
func TestDraftToStoreConstraint_WindowUTC(t *testing.T) {
	srv, _ := testServer(t)
	nb := time.Date(2030, 1, 1, 12, 0, 0, 0, time.FixedZone("", 2*3600))
	ea := time.Date(2030, 1, 2, 12, 0, 0, 0, time.FixedZone("", -5*3600))

	sc, err := srv.draftToStoreConstraint(&previewDraftRequest{
		Name:               "preview-utc",
		Purpose:            "p",
		Subject:            subjectSelectorRequest{Kind: "principal", PrincipalType: "user", PrincipalID: "u-1"},
		Scope:              constraintScopeRequest{Type: "system"},
		MaximumPermissions: []string{"agent.read"},
		AppliesWhen:        &constraintConditionReq{NotBefore: &nb, ExpiresAt: &ea},
	}, NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli"))
	require.NoError(t, err)
	require.NotNil(t, sc.NotBefore)
	require.NotNil(t, sc.ExpiresAt)
	assert.Equal(t, time.UTC, sc.NotBefore.Location())
	assert.Equal(t, time.UTC, sc.ExpiresAt.Location())
	assert.True(t, sc.NotBefore.Equal(nb))
	assert.True(t, sc.ExpiresAt.Equal(ea))
	// The request values are not modified in place.
	assert.NotEqual(t, time.UTC, nb.Location())
}

func TestConstraintConditionReqUTCWindow(t *testing.T) {
	t.Run("nil bounds stay nil", func(t *testing.T) {
		nb, ea := (&constraintConditionReq{}).utcWindow()
		assert.Nil(t, nb)
		assert.Nil(t, ea)
	})
	t.Run("one bound set", func(t *testing.T) {
		in := time.Date(2030, 1, 1, 9, 45, 0, 0, time.FixedZone("", 5*3600+45*60))
		req := &constraintConditionReq{ExpiresAt: &in}
		nb, ea := req.utcWindow()
		assert.Nil(t, nb)
		require.NotNil(t, ea)
		assert.Equal(t, "2030-01-01T04:00:00Z", ea.Format(time.RFC3339Nano))
		assert.NotSame(t, req.ExpiresAt, ea, "must return a copy, not the request pointer")
	})
}
