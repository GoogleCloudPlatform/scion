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
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// staleWriteStore reports every upsert at operational-settings revision 1
// and every read at revision 5, and fails every read once failList is set:
// a write whose section the cache knows at revision 5 must be re-read, and
// the re-read fails (or, without failList, succeeds).
type staleWriteStore struct {
	*fakeHubSettingStore
	failList bool
}

func (s *staleWriteStore) UpsertHubSetting(ctx context.Context, section string, value json.RawMessage, updatedBy string, expectedRevision int64, origin string) (*store.HubSetting, error) {
	row, err := s.fakeHubSettingStore.UpsertHubSetting(ctx, section, value, updatedBy, expectedRevision, origin)
	if err != nil {
		return nil, err
	}
	out := *row
	out.SettingsRevision = 1
	return &out, nil
}

func (s *staleWriteStore) ListHubSettings(ctx context.Context) ([]store.HubSetting, error) {
	if s.failList {
		return nil, errors.New("injected read failure")
	}
	rows, err := s.fakeHubSettingStore.ListHubSettings(ctx)
	if err != nil {
		return nil, err
	}
	return append(rows, store.HubSetting{Section: store.HubSettingsRevisionSection, Revision: 5}), nil
}

// When the post-write re-read fails, the self-apply does not include the
// write, so the save report lists the written keys as skipped with the
// not-yet-applied reason instead of under applied_keys (ptone/scion#5691).
func TestPutServerConfigDB_ReportsKeysSkippedWhenRereadFails(t *testing.T) {
	srv, fake, ops := newTestDBServer(t)
	sws := &staleWriteStore{fakeHubSettingStore: fake}
	ops.store = sws

	// The cache knows lifecycle at revision 5, above the write's revision 1.
	ops.mu.Lock()
	ops.cache["lifecycle"] = sectionState{Value: json.RawMessage(`{"auto_suspend_stalled":false}`), Revision: 1, Origin: "managed", settingsRev: 5}
	ops.revision = 5
	ops.mu.Unlock()
	sws.failList = true

	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"auto_suspend_stalled":true}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp struct {
		Reload struct {
			AppliedKeys    []string     `json:"applied_keys"`
			PendingRestart []string     `json:"pending_restart"`
			Skipped        []skippedKey `json:"skipped"`
		} `json:"reload"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.NotContains(t, resp.Reload.AppliedKeys, "server.hub.auto_suspend_stalled")
	assert.Contains(t, resp.Reload.Skipped, skippedKey{Key: "server.hub.auto_suspend_stalled", Reason: notYetAppliedReason})
	stored := storedRow(fake, "lifecycle")
	require.NotNil(t, stored, "the write is saved")
	assert.JSONEq(t, `{"auto_suspend_stalled":true}`, string(stored.Value))

	// With a working re-read the key is applied. The poll catches the
	// cache up with the first write first.
	sws.failList = false
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	require.True(t, ops.Snapshot().AutoSuspendStalled)
	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"auto_suspend_stalled":false}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Contains(t, resp.Reload.AppliedKeys, "server.hub.auto_suspend_stalled")
	for _, sk := range resp.Reload.Skipped {
		assert.NotEqual(t, notYetAppliedReason, sk.Reason)
	}
}

// withNotYetApplied moves the pending sections' applied keys to Skipped and
// keeps their restart-required keys pending (ptone/scion#5691).
func TestSavedKeysReport_WithNotYetApplied(t *testing.T) {
	r := savedKeysReport{
		Applied:        []string{"a.one"},
		PendingRestart: []string{"z.restart"},
		Skipped:        []skippedKey{{Key: "s.old", Reason: "other"}},
	}
	got := r.withNotYetApplied(savedKeysReport{
		Applied:        []string{"b.two", "b.three"},
		PendingRestart: []string{"b.restart"},
	})
	assert.Equal(t, []string{"a.one"}, got.Applied)
	assert.Equal(t, []string{"b.restart", "z.restart"}, got.PendingRestart)
	assert.Equal(t, []skippedKey{
		{Key: "s.old", Reason: "other"},
		{Key: "b.two", Reason: notYetAppliedReason},
		{Key: "b.three", Reason: notYetAppliedReason},
	}, got.Skipped)
	assert.Equal(t, []string{"z.restart"}, r.PendingRestart, "the receiver is not modified")
}
