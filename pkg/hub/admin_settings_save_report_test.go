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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A github_app field that changes while the section has no app_id does not
// take effect (ApplySnapshot ignores the section then), so it is listed
// under neither heading. With an app_id it is applied; app_id itself is
// pending a restart (ptone/scion#3904).
func TestReportSavedKeys_GitHubAppFieldsNeedAppID(t *testing.T) {
	before := map[string]json.RawMessage{"github_app": json.RawMessage(`{}`)}

	r := reportSavedKeys(before, map[string]json.RawMessage{
		"github_app": json.RawMessage(`{"api_base_url":"https://ghe.example.com/api/v3","webhooks_enabled":true}`),
	})
	assert.Empty(t, r.Applied, "without an app_id the fields are not applied")
	assert.Empty(t, r.PendingRestart)

	r = reportSavedKeys(before, map[string]json.RawMessage{
		"github_app": json.RawMessage(`{"app_id":5,"api_base_url":"https://ghe.example.com/api/v3"}`),
	})
	assert.Equal(t, []string{"server.github_app.api_base_url"}, r.Applied)
	assert.Equal(t, []string{"server.github_app.app_id"}, r.PendingRestart)

	// Clearing app_id is still reported (pending a restart); the other
	// fields of the cleared section are not.
	r = reportSavedKeys(
		map[string]json.RawMessage{"github_app": json.RawMessage(`{"app_id":5,"api_base_url":"https://ghe.example.com/api/v3"}`)},
		map[string]json.RawMessage{"github_app": json.RawMessage(`{}`)},
	)
	assert.Empty(t, r.Applied)
	assert.Equal(t, []string{"server.github_app.app_id"}, r.PendingRestart)
}

// A save that hits a revision conflict part way reports the keys of the
// sections it wrote before the conflict, and its applied map lists only
// those sections; unchanged sections are in unchanged_revisions
// (ptone/scion#3904, ptone/scion#4453).
func TestPutServerConfigDB_ConflictReportsWrittenKeysOnly(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	// First save: writes the access section as a managed row.
	rr := putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"admin_emails":["a@example.com"]}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// The lifecycle row is at a revision the next save does not expect.
	fakeStore.seed("lifecycle", json.RawMessage(`{"soft_delete_retention":"1h"}`))
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)

	// access is unchanged, endpoints is written (alphabetically before
	// lifecycle), lifecycle conflicts.
	rr = putHubNameServerConfigDB(t, srv, ops, `{
		"server": {"hub": {"admin_emails": ["a@example.com"], "public_url": "https://conflict.example.com", "soft_delete_retention": "2h"}},
		"expected_revisions": {"lifecycle": 99}
	}`)
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())

	var resp struct {
		Error              string           `json:"error"`
		Applied            map[string]int64 `json:"applied"`
		UnchangedRevisions map[string]int64 `json:"unchanged_revisions"`
		AppliedKeys        []string         `json:"applied_keys"`
		PendingRestart     []string         `json:"pending_restart"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "revision_conflict", resp.Error)
	assert.Contains(t, resp.Applied, "endpoints")
	assert.NotContains(t, resp.Applied, "access", "an unchanged section is not listed as applied")
	assert.NotContains(t, resp.Applied, "lifecycle")
	assert.Contains(t, resp.UnchangedRevisions, "access")
	assert.Equal(t, []string{"server.hub.public_url"}, resp.PendingRestart, "the written endpoints key is reported")
	assert.NotContains(t, resp.AppliedKeys, "server.hub.soft_delete_retention", "the conflicting section's keys are not reported")
}

// For a key that returns to the startup value when unset, GET shows the
// value the hub runs with; a stored value is shown as stored.
func TestGetServerConfigDB_ShowsRunningValueForUnsetKeys(t *testing.T) {
	ctx := context.Background()
	srv, _, ops := newTestDBServer(t)
	srv.config.SoftDeleteRetention = 72 * time.Hour
	srv.config.SoftDeleteRetainFiles = true
	srv.config.MaintenanceConfig.ImageRegistry = "boot.example.com/scion"
	srv.recordStartupLayer1Values()

	_, err := ops.Update(ctx, "lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`), "admin", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Update(ctx, "endpoints", json.RawMessage(`{"public_url":"https://hub.example.com"}`), "admin", -1, "managed")
	require.NoError(t, err)
	ApplySnapshot(srv, ops.Snapshot())

	resp := getServerConfigDB(t, srv, ops)
	assert.Equal(t, "72h0m0s", resp.Server.Hub.SoftDeleteRetention)
	require.NotNil(t, resp.Server.Hub.SoftDeleteRetainFiles)
	assert.True(t, *resp.Server.Hub.SoftDeleteRetainFiles)
	assert.Equal(t, "boot.example.com/scion", resp.ImageRegistry)

	_, err = ops.Update(ctx, "lifecycle", json.RawMessage(`{"soft_delete_retention":"5h","soft_delete_retain_files":false}`), "admin", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Update(ctx, "endpoints", json.RawMessage(`{"image_registry":"db.example.com/scion"}`), "admin", -1, "managed")
	require.NoError(t, err)
	ApplySnapshot(srv, ops.Snapshot())

	resp = getServerConfigDB(t, srv, ops)
	assert.Equal(t, "5h", resp.Server.Hub.SoftDeleteRetention)
	require.NotNil(t, resp.Server.Hub.SoftDeleteRetainFiles)
	assert.False(t, *resp.Server.Hub.SoftDeleteRetainFiles)
	assert.Equal(t, "db.example.com/scion", resp.ImageRegistry)
}

// echoTestServer is a DB-backed server whose startup soft-delete and image
// registry values are set, with managed lifecycle and endpoints rows that
// leave those keys unset.
func echoTestServer(t *testing.T) (*Server, *fakeHubSettingStore, *OperationalSettings) {
	t.Helper()
	ctx := context.Background()
	srv, fakeStore, ops := newTestDBServer(t)
	srv.config.SoftDeleteRetention = 72 * time.Hour
	srv.config.SoftDeleteRetainFiles = true
	// As if from this replica's own environment
	// (SCION_MAINTENANCE_IMAGE_REGISTRY).
	srv.config.MaintenanceConfig.ImageRegistry = "replica-env.example.com/scion"
	srv.recordStartupLayer1Values()
	_, err := ops.Update(ctx, "lifecycle", json.RawMessage(`{"auto_suspend_stalled":true}`), "admin", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Update(ctx, "endpoints", json.RawMessage(`{"public_url":"https://hub.example.com"}`), "admin", -1, "managed")
	require.NoError(t, err)
	ApplySnapshot(srv, ops.Snapshot())
	return srv, fakeStore, ops
}

func rowDoc(t *testing.T, st *fakeHubSettingStore, section string) (map[string]interface{}, int64) {
	t.Helper()
	row, err := st.GetHubSetting(context.Background(), section)
	require.NoError(t, err)
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(row.Value, &m))
	return m, row.Revision
}

func savedKeys(t *testing.T, body []byte) (applied, pending []string) {
	t.Helper()
	var resp struct {
		Reload struct {
			AppliedKeys    []string `json:"applied_keys"`
			PendingRestart []string `json:"pending_restart"`
		} `json:"reload"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp.Reload.AppliedKeys, resp.Reload.PendingRestart
}

// A save that echoes the running values GET showed for unset keys leaves
// those keys unset: the lifecycle and endpoints rows are not written, the
// keys are not reported, and this replica's environment registry is not
// copied into the shared endpoints row (ptone/scion#3904).
func TestPutServerConfigDB_EchoOfRunningValuesKeepsKeysUnset(t *testing.T) {
	srv, fakeStore, ops := echoTestServer(t)

	get := getServerConfigDB(t, srv, ops)
	require.Equal(t, "72h0m0s", get.Server.Hub.SoftDeleteRetention)
	require.Equal(t, "replica-env.example.com/scion", get.ImageRegistry)
	lcBefore, lcRev := rowDoc(t, fakeStore, "lifecycle")
	epBefore, epRev := rowDoc(t, fakeStore, "endpoints")

	rr := putHubNameServerConfigDB(t, srv, ops, `{
		"server": {"hub": {"admin_emails": ["new@example.com"], "soft_delete_retention": "72h0m0s", "soft_delete_retain_files": true}},
		"image_registry": "replica-env.example.com/scion"
	}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	lcAfter, lcRevAfter := rowDoc(t, fakeStore, "lifecycle")
	epAfter, epRevAfter := rowDoc(t, fakeStore, "endpoints")
	assert.Equal(t, lcBefore, lcAfter, "the lifecycle row is unchanged")
	assert.Equal(t, lcRev, lcRevAfter)
	assert.Equal(t, epBefore, epAfter, "the endpoints row is unchanged")
	assert.Equal(t, epRev, epRevAfter)
	assert.NotContains(t, epAfter, "image_registry", "the replica's registry is not copied into the shared row")

	applied, pending := savedKeys(t, rr.Body.Bytes())
	assert.Equal(t, []string{"server.hub.admin_emails"}, applied)
	assert.Empty(t, pending)

	// The running values are unchanged.
	retention, retain := srv.softDeleteSettings()
	assert.Equal(t, 72*time.Hour, retention)
	assert.True(t, retain)
	assert.Equal(t, "replica-env.example.com/scion", srv.resolveImageRegistry())
}

// A value that differs from the running value is stored and reported, and
// an explicit clear still clears.
func TestPutServerConfigDB_ExplicitValuesAndClearsOfRunningValueKeys(t *testing.T) {
	srv, fakeStore, ops := echoTestServer(t)

	rr := putHubNameServerConfigDB(t, srv, ops, `{
		"server": {"hub": {"soft_delete_retention": "5h", "soft_delete_retain_files": false}},
		"image_registry": "shared.example.com/scion"
	}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	lc, _ := rowDoc(t, fakeStore, "lifecycle")
	ep, _ := rowDoc(t, fakeStore, "endpoints")
	assert.Equal(t, "5h", lc["soft_delete_retention"])
	assert.Equal(t, false, lc["soft_delete_retain_files"])
	assert.Equal(t, "shared.example.com/scion", ep["image_registry"])
	applied, _ := savedKeys(t, rr.Body.Bytes())
	assert.Subset(t, applied, []string{"image_registry", "server.hub.soft_delete_retain_files", "server.hub.soft_delete_retention"})

	// Explicit clears remove the stored values again.
	rr = putHubNameServerConfigDB(t, srv, ops, `{
		"server": {"hub": {"soft_delete_retention": "", "soft_delete_retain_files": null}},
		"image_registry": ""
	}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	lc, _ = rowDoc(t, fakeStore, "lifecycle")
	ep, _ = rowDoc(t, fakeStore, "endpoints")
	assert.NotContains(t, lc, "soft_delete_retention")
	assert.NotContains(t, lc, "soft_delete_retain_files")
	assert.NotContains(t, ep, "image_registry")
	ApplySnapshot(srv, ops.Snapshot())
	retention, retain := srv.softDeleteSettings()
	assert.Equal(t, 72*time.Hour, retention, "a cleared retention returns to the startup value")
	assert.True(t, retain)
	assert.Equal(t, "replica-env.example.com/scion", srv.resolveImageRegistry())
}
