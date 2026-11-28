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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerConfigDB_ArtifactsRoundTripAndMerge(t *testing.T) {
	srv, _, ops := newTestDBServer(t)

	// (1) Initial GET omits artifacts and reports section_metadata.artifacts as default.
	getRR := httptest.NewRecorder()
	srv.handleGetServerConfigDB(getRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())

	var getResp ServerConfigDBResponse
	require.NoError(t, json.Unmarshal(getRR.Body.Bytes(), &getResp))
	assert.Nil(t, getResp.Artifacts)
	assert.Equal(t, "default", getResp.SectionMeta["artifacts"].Source)

	// (2) PUT sets initial overrides and hot-reloads ops.Artifacts() immediately.
	putRR := httptest.NewRecorder()
	srv.handlePutServerConfigDB(putRR, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"artifacts":{"default_retention_days":30,"gc_grace_hours":48,"enabled":false},"expected_revisions":{"artifacts":0}}`), ops)
	require.Equal(t, http.StatusOK, putRR.Code, putRR.Body.String())

	live := ops.Artifacts()
	assert.Equal(t, 30, live.DefaultRetentionDays)
	assert.Equal(t, 48, live.GCGraceHours)
	assert.False(t, live.Enabled)

	getRR = httptest.NewRecorder()
	srv.handleGetServerConfigDB(getRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
	getResp = ServerConfigDBResponse{}
	require.NoError(t, json.Unmarshal(getRR.Body.Bytes(), &getResp))
	require.NotNil(t, getResp.Artifacts)
	require.NotNil(t, getResp.Artifacts.DefaultRetentionDays)
	assert.Equal(t, 30, *getResp.Artifacts.DefaultRetentionDays)
	require.NotNil(t, getResp.Artifacts.GCGraceHours)
	assert.Equal(t, 48, *getResp.Artifacts.GCGraceHours)
	require.NotNil(t, getResp.Artifacts.Enabled)
	assert.False(t, *getResp.Artifacts.Enabled)
	assert.Nil(t, getResp.Artifacts.MaxFiles)
	assert.Equal(t, "db", getResp.SectionMeta["artifacts"].Source)
	rev1 := getResp.SectionMeta["artifacts"].Revision
	require.Equal(t, int64(1), rev1)

	// (3) Partial PUT merges onto the stored row without clobbering sibling fields.
	putRR = httptest.NewRecorder()
	srv.handlePutServerConfigDB(putRR, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"artifacts":{"max_files":50,"link_default_ttl_hours":24},"expected_revisions":{"artifacts":1}}`), ops)
	require.Equal(t, http.StatusOK, putRR.Code, putRR.Body.String())

	live = ops.Artifacts()
	assert.Equal(t, 30, live.DefaultRetentionDays)
	assert.Equal(t, 48, live.GCGraceHours)
	assert.False(t, live.Enabled)
	assert.Equal(t, 50, live.MaxFiles)
	assert.Equal(t, 24, live.LinkDefaultTTLHours)

	// (4) Explicit null on a single field clears it back to its compiled default
	// while preserving other stored fields.
	putRR = httptest.NewRecorder()
	srv.handlePutServerConfigDB(putRR, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"artifacts":{"default_retention_days":null},"expected_revisions":{"artifacts":2}}`), ops)
	require.Equal(t, http.StatusOK, putRR.Code, putRR.Body.String())

	live = ops.Artifacts()
	assert.Equal(t, opsettings.ArtifactsDefaultRetentionDays, live.DefaultRetentionDays)
	assert.Equal(t, 48, live.GCGraceHours)
	assert.Equal(t, 50, live.MaxFiles)

	getRR = httptest.NewRecorder()
	srv.handleGetServerConfigDB(getRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
	getResp = ServerConfigDBResponse{}
	require.NoError(t, json.Unmarshal(getRR.Body.Bytes(), &getResp))
	require.NotNil(t, getResp.Artifacts)
	assert.Nil(t, getResp.Artifacts.DefaultRetentionDays)
	require.NotNil(t, getResp.Artifacts.GCGraceHours)
	assert.Equal(t, 48, *getResp.Artifacts.GCGraceHours)
	rev3 := getResp.SectionMeta["artifacts"].Revision
	require.Equal(t, int64(3), rev3)

	// (5) Explicit 0 for default_retention_days is preserved as a non-nil override.
	putRR = httptest.NewRecorder()
	srv.handlePutServerConfigDB(putRR, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"artifacts":{"default_retention_days":0},"expected_revisions":{"artifacts":3}}`), ops)
	require.Equal(t, http.StatusOK, putRR.Code, putRR.Body.String())

	getRR = httptest.NewRecorder()
	srv.handleGetServerConfigDB(getRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
	getResp = ServerConfigDBResponse{}
	require.NoError(t, json.Unmarshal(getRR.Body.Bytes(), &getResp))
	require.NotNil(t, getResp.Artifacts)
	require.NotNil(t, getResp.Artifacts.DefaultRetentionDays)
	assert.Equal(t, 0, *getResp.Artifacts.DefaultRetentionDays)
	rev4 := getResp.SectionMeta["artifacts"].Revision
	require.Equal(t, int64(4), rev4)

	// (6) Echoing the stored value is a no-op (does not bump revision).
	echoBody, err := json.Marshal(map[string]any{
		"artifacts":          getResp.Artifacts,
		"expected_revisions": map[string]int64{"artifacts": rev4},
	})
	require.NoError(t, err)
	putRR = httptest.NewRecorder()
	srv.handlePutServerConfigDB(putRR, adminRequest(http.MethodPut, "/api/v1/admin/server-config", string(echoBody)), ops)
	require.Equal(t, http.StatusOK, putRR.Code, putRR.Body.String())
	var echoResp map[string]any
	require.NoError(t, json.Unmarshal(putRR.Body.Bytes(), &echoResp))
	assert.Contains(t, echoResp["unchanged"], "artifacts")

	// (7) Stale expected_revisions returns 409 revision_conflict.
	putRR = httptest.NewRecorder()
	srv.handlePutServerConfigDB(putRR, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"artifacts":{"default_retention_days":7},"expected_revisions":{"artifacts":1}}`), ops)
	require.Equal(t, http.StatusConflict, putRR.Code, putRR.Body.String())
	assert.Contains(t, putRR.Body.String(), "revision_conflict")

	// (8) Top-level null clears all overrides back to compiled defaults.
	putRR = httptest.NewRecorder()
	srv.handlePutServerConfigDB(putRR, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"artifacts":null,"expected_revisions":{"artifacts":4}}`), ops)
	require.Equal(t, http.StatusOK, putRR.Code, putRR.Body.String())
	assert.Equal(t, opsettings.DefaultArtifactsConfig(), ops.Artifacts())

	getRR = httptest.NewRecorder()
	srv.handleGetServerConfigDB(getRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
	getResp = ServerConfigDBResponse{}
	require.NoError(t, json.Unmarshal(getRR.Body.Bytes(), &getResp))
	assert.Nil(t, getResp.Artifacts)
}

func TestServerConfigDB_ArtifactsValidation(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	invalidCases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{
			name:     "negative default_retention_days",
			body:     `{"artifacts":{"default_retention_days":-1}}`,
			wantCode: "validation_error",
		},
		{
			name:     "gc_grace_hours below minimum",
			body:     `{"artifacts":{"gc_grace_hours":12}}`,
			wantCode: "validation_error",
		},
		{
			name:     "max_file_bytes exceeds max_bundle_bytes",
			body:     `{"artifacts":{"max_file_bytes":2048,"max_bundle_bytes":1024}}`,
			wantCode: "validation_error",
		},
		{
			name:     "link_default_ttl_hours exceeds link_max_ttl_hours",
			body:     `{"artifacts":{"link_default_ttl_hours":100,"link_max_ttl_hours":50}}`,
			wantCode: "validation_error",
		},
		{
			name:     "remote_image_total_budget_s exceeds cap",
			body:     `{"artifacts":{"remote_image_total_budget_s":61}}`,
			wantCode: "validation_error",
		},
		{
			name:     "remote_image_total_budget_s below fetch timeout",
			body:     `{"artifacts":{"remote_image_fetch_timeout_s":20,"remote_image_total_budget_s":10}}`,
			wantCode: "validation_error",
		},
		{
			name:     "unknown nested key rejected",
			body:     `{"artifacts":{"default_retention_days":30,"bogus":1}}`,
			wantCode: "unpersisted_keys_rejected",
		},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", tc.body), ops)
			require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), tc.wantCode)
		})
	}

	// Atomic validation: an invalid artifacts payload paired with an earlier
	// alphabetical section ("access") must write neither section.
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"server":{"hub":{"admin_emails":["new@example.com"]}},"artifacts":{"default_retention_days":-5}}`), ops)
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	_, accessErr := fakeStore.GetHubSetting(context.Background(), "access")
	assert.Error(t, accessErr, "earlier section 'access' must not be written when 'artifacts' fails validation")
}

func TestServerConfigDB_ArtifactsTokenAndWorkstationClassification(t *testing.T) {
	srv, _, ops := newTestDBServer(t)

	// Token classification: artifacts is settingsTokenConfiguration, so tokens
	// may write artifacts fields or clear the section.
	assert.Empty(t, tokenRefusedServerConfigKeys([]byte(`{"artifacts":{"default_retention_days":14}}`)))
	assert.Empty(t, tokenRefusedServerConfigKeys([]byte(`{"artifacts":null}`)))

	// Hosted bootstrap check must not classify artifacts as Layer-0 or unclassified.
	l0, u, err := srv.hostedBootstrapChanges(context.Background(), ops, []byte(`{"artifacts":{"default_retention_days":14,"enabled":false}}`))
	require.NoError(t, err)
	assert.Empty(t, l0)
	assert.Empty(t, u)

	// Workstation file routing must keep artifacts in DB, never settings.yaml.
	assert.False(t, isWorkstationFileLeaf([]string{"artifacts", "default_retention_days"}))
	assert.False(t, isWorkstationFileLeaf([]string{"artifacts"}))
}
