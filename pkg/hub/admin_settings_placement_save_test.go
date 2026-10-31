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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// placementRuntimesDoc is a stored runtimes section with a pool runtime.
const placementRuntimesDoc = `{"k8s":{"type":"kubernetes","namespace":"agents","node_selector":{"scion.dev/pool":"agents"}}}`

// requirePlacementRow asserts a section's stored revision and JSON value.
func requirePlacementRow(t *testing.T, f *fakeHubSettingStore, section string, rev int64, doc string) {
	t.Helper()
	f.mu.Lock()
	row := f.settings[section]
	f.mu.Unlock()
	require.NotNil(t, row, "%s row missing", section)
	assert.Equal(t, rev, row.Revision, section)
	assert.JSONEq(t, doc, string(row.Value), section)
}

// requireNoPlacementRow asserts that a section has no stored row.
func requireNoPlacementRow(t *testing.T, f *fakeHubSettingStore, section string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Nil(t, f.settings[section], "%s was written", section)
}

// seedPlacement stores rows and loads them into the operational settings.
func seedPlacement(t *testing.T, f *fakeHubSettingStore, ops *OperationalSettings, rows map[string]string) {
	t.Helper()
	for section, doc := range rows {
		f.seedWithOrigin(section, json.RawMessage(doc), "managed")
	}
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
}

// A body that sends runtimes or profiles spelt in another case, or twice,
// is refused before the typed decode reads it, and nothing is written: a
// case variant by the placement check (validation_failed), a repeated
// member by the earlier repeated-member check (invalid_request).
func TestPutSettingsRepeatedPlacementSectionRefused(t *testing.T) {
	tempSettingsHome(t)
	for _, tc := range []struct{ body, code string }{
		{`{"Runtimes":{"d":{"type":"docker"}}}`, "validation_failed"},
		{`{"PROFILES":{"p":{"runtime":"docker"}}}`, "validation_failed"},
		{`{"runtimes":{"k":{"type":"kubernetes"}},"runtimes":{"d":{"type":"docker"}}}`, ErrCodeInvalidRequest},
		{`{"runtimes":{"k":{"type":"kubernetes"}},"Runtimes":{"d":{"type":"docker"}}}`, ErrCodeInvalidRequest},
	} {
		t.Run(tc.body, func(t *testing.T) {
			srv, fake, ops := newTestDBServer(t)
			rr := putServerConfigDB(t, srv, ops, tc.body)
			require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), tc.code)
			requireNoPlacementRow(t, fake, "runtimes")
			requireNoPlacementRow(t, fake, "profiles")
		})
	}
}

// The placement rules across runtime entries and profiles are save checks:
// a problem the save introduces is refused with 422 naming the path, and
// nothing is written.
func TestPutSettingsPlacementPairRulesRefuseIntroducedProblems(t *testing.T) {
	tempSettingsHome(t)
	cases := []struct {
		name, body, path string
	}{
		{"profile pool conflicts with its runtime entry",
			`{"profiles":{"batch":{"runtime":"k8s","node_selector":{"scion.dev/pool":"batch"}}}}`, "profiles.batch.node_selector"},
		{"placed profile on a missing runtime entry",
			`{"profiles":{"p":{"runtime":"nowhere","runtime_class_name":"gvisor"}}}`, "profiles.p.runtime"},
		{"placement on a runtime entry that is not Kubernetes",
			`{"runtimes":{"k8s":{"type":"kubernetes","namespace":"agents","node_selector":{"scion.dev/pool":"agents"}},"d":{"type":"docker","runtime_class_name":"gvisor"}}}`, "runtimes.d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake, ops := newTestDBServer(t)
			seedPlacement(t, fake, ops, map[string]string{"runtimes": placementRuntimesDoc})
			rr := putServerConfigDB(t, srv, ops, tc.body)
			require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), tc.path)
			requirePlacementRow(t, fake, "runtimes", 1, placementRuntimesDoc)
			requireNoPlacementRow(t, fake, "profiles")
		})
	}
}

// Deleting a runtime entry that a stored profile with placement names is a
// problem the save introduces.
func TestPutSettingsDeletingPlacedProfilesRuntimeRefused(t *testing.T) {
	tempSettingsHome(t)
	srv, fake, ops := newTestDBServer(t)
	seedPlacement(t, fake, ops, map[string]string{
		"runtimes": `{"pool-rt":{"type":"kubernetes","namespace":"agents"},"d":{"type":"docker"}}`,
		"profiles": `{"fast":{"runtime":"pool-rt","runtime_class_name":"gvisor"}}`,
	})
	rr := putServerConfigDB(t, srv, ops, `{"runtimes":{"d":{"type":"docker"}}}`)
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "profiles.fast.runtime")
	requirePlacementRow(t, fake, "runtimes", 1, `{"pool-rt":{"type":"kubernetes","namespace":"agents"},"d":{"type":"docker"}}`)
}

// A placement problem already stored never blocks a save, also when the
// save touches the entry that carries it: it is a save warning.
func TestPutSettingsStoredPlacementProblemWarns(t *testing.T) {
	tempSettingsHome(t)
	srv, fake, ops := newTestDBServer(t)
	stored := `{"batch":{"runtime":"k8s","node_selector":{"scion.dev/pool":"batch"}}}`
	seedPlacement(t, fake, ops, map[string]string{"runtimes": placementRuntimesDoc, "profiles": stored})

	// An unrelated profile.
	rr := putServerConfigDB(t, srv, ops, `{"profiles":{"batch":{"runtime":"k8s","node_selector":{"scion.dev/pool":"batch"}},"fast":{"runtime":"k8s","runtime_class_name":"gvisor"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "profiles.batch.node_selector")

	// The profile with the stored conflict, changed in another field.
	rr = putServerConfigDB(t, srv, ops, `{"profiles":{"batch":{"runtime":"k8s","node_selector":{"scion.dev/pool":"batch"},"runtime_class_name":"gvisor"},"fast":{"runtime":"k8s","runtime_class_name":"gvisor"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "profiles.batch.node_selector")
}

// The runtimes schema holds the JSON types only (GCP#4339): an invalid
// placement value the save sets is refused by the save check (422
// validation_error naming the field), not by the section schema (400
// validation_failed).
func TestPutSettingsTouchedBadPlacementRefusedBySaveCheck(t *testing.T) {
	tempSettingsHome(t)
	srv, fake, ops := newTestDBServer(t)
	rr := putServerConfigDB(t, srv, ops, `{"runtimes":{"k":{"type":"kubernetes","runtime_class_name":"Not_Valid"}}}`)
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "runtimes.k.runtime_class_name")
	assert.NotContains(t, rr.Body.String(), "validation_failed")
	requireNoPlacementRow(t, fake, "runtimes")
}

// A save that does not send a stored runtime key that is not a runtime
// setting (kept by the merge, ptone/scion#6580) passes, keeps the key and
// warns, also for keys nested in a kept member (a toleration with a key
// the toleration type does not model, a cloudrun sub-key).
func TestPutSettingsKeptRuntimeKeyNotSentPasses(t *testing.T) {
	tempSettingsHome(t)
	srv, fake, ops := newTestDBServer(t)
	stored := `{"k8s":{"type":"kubernetes","namespace":"agents","future_field":{"x":1},` +
		`"tolerations":[{"key":"a","operator":"Exists","tolerationSeconds":5}],"cloudrun":{"extra":"kept"}}}`
	seedPlacement(t, fake, ops, map[string]string{"runtimes": stored})

	rr := putServerConfigDB(t, srv, ops, `{"runtimes":{"k8s":{"type":"kubernetes","namespace":"agents","node_selector":{"scion.dev/pool":"agents"}}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "runtimes.k8s.future_field")
	requirePlacementRow(t, fake, "runtimes", 2,
		`{"k8s":{"type":"kubernetes","namespace":"agents","node_selector":{"scion.dev/pool":"agents"},"future_field":{"x":1},`+
			`"tolerations":[{"key":"a","operator":"Exists","tolerationSeconds":5}],"cloudrun":{"extra":"kept"}}}`)

	// A profiles-only save reads the same stored runtimes and passes too.
	rr = putServerConfigDB(t, srv, ops, `{"profiles":{"fast":{"runtime":"k8s","runtime_class_name":"gvisor"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
}

// racingProfilesStore changes the profiles row (another writer) just
// before the guarded write runs.
type racingProfilesStore struct {
	*fakeHubSettingStore
}

func (s *racingProfilesStore) UpsertHubSettingsGuarded(ctx context.Context, writes []store.HubSettingWrite, reads map[string]int64, updatedBy, origin string) ([]store.HubSetting, error) {
	if _, err := s.fakeHubSettingStore.UpsertHubSetting(ctx, "profiles",
		json.RawMessage(`{"plain":{"runtime":"k8s"},"other":{"runtime":"k8s"}}`), "other@example.com", -1, "managed"); err != nil {
		return nil, err
	}
	return s.fakeHubSettingStore.UpsertHubSettingsGuarded(ctx, writes, reads, updatedBy, origin)
}

// The revision of the omitted section the save checks used is compared in
// the same write: when another writer changes it in between, the save is a
// 409 that lists that section as read and writes nothing.
func TestPutSettingsPlacementDependencyRevisionConflict(t *testing.T) {
	tempSettingsHome(t)
	srv, fake, ops := newTestDBServer(t)
	seedPlacement(t, fake, ops, map[string]string{"profiles": `{"plain":{"runtime":"k8s"}}`})
	ops.store = &racingProfilesStore{fakeHubSettingStore: fake}

	rr := putServerConfigDB(t, srv, ops, `{"runtimes":`+placementRuntimesDoc+`}`)
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	var resp struct {
		Conflicted []map[string]interface{} `json:"conflicted"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Conflicted, 2, rr.Body.String())
	assert.Equal(t, "runtimes", resp.Conflicted[0]["section"])
	assert.Equal(t, map[string]interface{}{
		"section": "profiles", "expected_revision": float64(1), "current_revision": float64(2), "read": true,
	}, resp.Conflicted[1])
	requireNoPlacementRow(t, fake, "runtimes")
}

// racingRuntimesStore changes the runtimes row (another writer) just
// before the guarded write, after the save merged onto the row it read.
type racingRuntimesStore struct {
	*fakeHubSettingStore
}

func (s *racingRuntimesStore) UpsertHubSettingsGuarded(ctx context.Context, writes []store.HubSettingWrite, reads map[string]int64, updatedBy, origin string) ([]store.HubSetting, error) {
	if _, err := s.fakeHubSettingStore.UpsertHubSetting(ctx, "runtimes",
		json.RawMessage(`{"k8s":{"type":"kubernetes","namespace":"other"}}`), "other@example.com", -1, "managed"); err != nil {
		return nil, err
	}
	return s.fakeHubSettingStore.UpsertHubSettingsGuarded(ctx, writes, reads, updatedBy, origin)
}

// The runtimes write is the compare-and-swap on the row the merge read
// (ptone/scion#6580), in the same transaction as profiles: a concurrent
// runtimes write makes the save a 409 that writes neither section.
func TestPutSettingsRuntimesMergeCASConflict(t *testing.T) {
	tempSettingsHome(t)
	srv, fake, ops := newTestDBServer(t)
	seedPlacement(t, fake, ops, map[string]string{"runtimes": `{"k8s":{"type":"kubernetes","namespace":"agents"}}`})
	ops.store = &racingRuntimesStore{fakeHubSettingStore: fake}

	rr := putServerConfigDB(t, srv, ops, `{"runtimes":{"k8s":{"type":"kubernetes","namespace":"agents","node_selector":{"scion.dev/pool":"agents"}}},
		"profiles":{"fast":{"runtime":"k8s","runtime_class_name":"gvisor"}}}`)
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"runtimes"`)
	requirePlacementRow(t, fake, "runtimes", 2, `{"k8s":{"type":"kubernetes","namespace":"other"}}`)
	requireNoPlacementRow(t, fake, "profiles")
}

// A save of both sections writes neither when one of them conflicts, and
// no other section of the save is written either.
func TestPutSettingsPlacementNoPartialWrite(t *testing.T) {
	tempSettingsHome(t)
	srv, fake, ops := newTestDBServer(t)
	seedPlacement(t, fake, ops, map[string]string{"profiles": `{"plain":{"runtime":"k8s"}}`})

	rr := putServerConfigDB(t, srv, ops, `{"runtimes":`+placementRuntimesDoc+`,
		"profiles":{"plain":{"runtime":"k8s"},"fast":{"runtime":"k8s","runtime_class_name":"gvisor"}},
		"server":{"hub":{"auto_suspend_stalled":true}},
		"expected_revisions":{"profiles":7}}`)
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	requireNoPlacementRow(t, fake, "runtimes")
	requirePlacementRow(t, fake, "profiles", 1, `{"plain":{"runtime":"k8s"}}`)
	requireNoPlacementRow(t, fake, "lifecycle")

	rr = putServerConfigDB(t, srv, ops, `{"runtimes":`+placementRuntimesDoc+`,
		"profiles":{"plain":{"runtime":"k8s"},"fast":{"runtime":"k8s","runtime_class_name":"gvisor"}},
		"expected_revisions":{"profiles":1}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	requirePlacementRow(t, fake, "runtimes", 1, placementRuntimesDoc)
	requirePlacementRow(t, fake, "profiles", 2, `{"plain":{"runtime":"k8s"},"fast":{"runtime":"k8s","runtime_class_name":"gvisor"}}`)
}

// unguardedHubSettingStore hides the guarded batch method of the fake.
type unguardedHubSettingStore struct {
	store.HubSettingStore
}

// A settings store that cannot write runtimes and profiles in one guarded
// transaction fails the save closed, and writes nothing.
func TestPutSettingsGuardedBatchUnsupportedFailsClosed(t *testing.T) {
	tempSettingsHome(t)
	srv, fake, ops := newTestDBServer(t)
	ops.store = unguardedHubSettingStore{HubSettingStore: fake}
	rr := putServerConfigDB(t, srv, ops, `{"runtimes":`+placementRuntimesDoc+`}`)
	require.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "cannot save them in one transaction")
	requireNoPlacementRow(t, fake, "runtimes")
}

// resetPlacementSectionDB sends DELETE .../sections/{section}.
func resetPlacementSectionDB(srv *Server, section string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfigSectionReset(rr, adminRequest(http.MethodDelete, "/api/v1/admin/server-config/sections/"+section, ""))
	return rr
}

// Resetting runtimes or profiles runs the placement save checks on the
// pair the reset leaves: a reset that would leave a profile's placement
// without its runtime entry is refused with a 422 in the form of a save's
// (the finding's path in field, every finding in errors), and the row
// stays; a reset that leaves a valid pair removes the row.
func TestResetPlacementSectionChecksPair(t *testing.T) {
	tempSettingsHome(t)
	srv, fake, ops := newTestDBServer(t)
	seedPlacement(t, fake, ops, map[string]string{
		"runtimes": `{"pool-rt":{"type":"kubernetes","namespace":"agents"}}`,
		"profiles": `{"fast":{"runtime":"pool-rt","runtime_class_name":"gvisor"}}`,
	})

	rr := resetPlacementSectionDB(srv, "runtimes")
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Field  string   `json:"field"`
				Errors []string `json:"errors"`
			} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp), rr.Body.String())
	assert.Equal(t, ErrCodeValidationError, resp.Error.Code)
	assert.Equal(t, "profiles.fast.runtime", resp.Error.Details.Field)
	require.Len(t, resp.Error.Details.Errors, 1, rr.Body.String())
	assert.Contains(t, resp.Error.Details.Errors[0], "profiles.fast.runtime")
	requirePlacementRow(t, fake, "runtimes", 1, `{"pool-rt":{"type":"kubernetes","namespace":"agents"}}`)

	// Without the profile's placement the same reset is valid.
	_, err := fake.UpsertHubSetting(context.Background(), "profiles", json.RawMessage(`{"fast":{"runtime":"pool-rt"}}`), "test", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Refresh(context.Background())
	require.NoError(t, err)
	rr = resetPlacementSectionDB(srv, "runtimes")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	requireNoPlacementRow(t, fake, "runtimes")

	// A section with no row is still a 404.
	rr = resetPlacementSectionDB(srv, "runtimes")
	require.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())
}

// A reset whose other section changes between the checks and the guarded
// delete is a 409 listing that section as read, and deletes nothing.
func TestResetPlacementSectionRevisionConflict(t *testing.T) {
	tempSettingsHome(t)
	srv, fake, ops := newTestDBServer(t)
	seedPlacement(t, fake, ops, map[string]string{
		"runtimes": placementRuntimesDoc,
		"profiles": `{"plain":{"runtime":"k8s"}}`,
	})
	ops.store = &racingProfilesStore{fakeHubSettingStore: fake}

	rr := resetPlacementSectionDB(srv, "runtimes")
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	var resp struct {
		Conflicted []map[string]interface{} `json:"conflicted"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Conflicted, 2, rr.Body.String())
	assert.Equal(t, "runtimes", resp.Conflicted[0]["section"])
	assert.Equal(t, true, resp.Conflicted[1]["read"])
	requirePlacementRow(t, fake, "runtimes", 1, placementRuntimesDoc)
}

// A store without the guarded batch method fails a placement section reset
// closed, deleting nothing.
func TestResetPlacementSectionGuardedBatchUnsupported(t *testing.T) {
	tempSettingsHome(t)
	srv, fake, ops := newTestDBServer(t)
	seedPlacement(t, fake, ops, map[string]string{"profiles": `{"plain":{"runtime":"k8s"}}`})
	ops.store = unguardedHubSettingStore{HubSettingStore: fake}
	rr := resetPlacementSectionDB(srv, "profiles")
	require.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())
	requirePlacementRow(t, fake, "profiles", 1, `{"plain":{"runtime":"k8s"}}`)
}

// setPlacementSnapshotHook sets placementSnapshotHook for one test.
func setPlacementSnapshotHook(t *testing.T, fn func()) {
	t.Helper()
	old := placementSnapshotHook
	placementSnapshotHook = fn
	t.Cleanup(func() { placementSnapshotHook = old })
}

// writeProfilesAfterSnapshot makes another writer store profiles, and the
// Hub cache take it, right after a save or reset took the snapshot its
// checks use.
func writeProfilesAfterSnapshot(t *testing.T, fake *fakeHubSettingStore, ops *OperationalSettings, doc string) {
	t.Helper()
	setPlacementSnapshotHook(t, func() {
		_, err := fake.UpsertHubSetting(context.Background(), "profiles", json.RawMessage(doc), "other@example.com", -1, "managed")
		require.NoError(t, err)
		_, err = ops.Refresh(context.Background())
		require.NoError(t, err)
	})
}

// The guard revision of an omitted section is the one read before the
// snapshot the save checks: a write landing after the snapshot (here a
// profile placement on the runtime entry the save makes non-Kubernetes)
// makes the save a 409 that lists the section as read, and nothing is
// written.
func TestPutSettingsPlacementGuardReadBeforeSnapshot(t *testing.T) {
	tempSettingsHome(t)
	srv, fake, ops := newTestDBServer(t)
	seedPlacement(t, fake, ops, map[string]string{"profiles": `{"plain":{"runtime":"pool-rt"}}`})
	writeProfilesAfterSnapshot(t, fake, ops, `{"plain":{"runtime":"pool-rt"},"late":{"runtime":"pool-rt","runtime_class_name":"gvisor"}}`)

	rr := putServerConfigDB(t, srv, ops, `{"runtimes":{"pool-rt":{"type":"docker"}}}`)
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	var resp struct {
		Conflicted []map[string]interface{} `json:"conflicted"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Conflicted, 2, rr.Body.String())
	assert.Equal(t, map[string]interface{}{
		"section": "profiles", "expected_revision": float64(1), "current_revision": float64(2), "read": true,
	}, resp.Conflicted[1])
	requireNoPlacementRow(t, fake, "runtimes")
}

// A reset reads the other section's guard revision before the snapshot
// its checks use: a write landing after the snapshot (here a profile
// placement that needs the runtime entry being reset) makes the reset a
// 409, and the row stays.
func TestResetPlacementSectionGuardReadBeforeSnapshot(t *testing.T) {
	tempSettingsHome(t)
	srv, fake, ops := newTestDBServer(t)
	seedPlacement(t, fake, ops, map[string]string{
		"runtimes": `{"pool-rt":{"type":"kubernetes","namespace":"agents"}}`,
		"profiles": `{"fast":{"runtime":"pool-rt"}}`,
	})
	writeProfilesAfterSnapshot(t, fake, ops, `{"fast":{"runtime":"pool-rt","runtime_class_name":"gvisor"}}`)

	rr := resetPlacementSectionDB(srv, "runtimes")
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	var resp struct {
		Conflicted []map[string]interface{} `json:"conflicted"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Conflicted, 2, rr.Body.String())
	assert.Equal(t, true, resp.Conflicted[1]["read"])
	requirePlacementRow(t, fake, "runtimes", 1, `{"pool-rt":{"type":"kubernetes","namespace":"agents"}}`)
}
