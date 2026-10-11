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
