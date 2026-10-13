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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func storedEndpointsPublicURL(t *testing.T, fake *fakeHubSettingStore) (string, bool) {
	t.Helper()
	row, err := fake.GetHubSetting(context.Background(), "endpoints")
	if err != nil {
		return "", false
	}
	return publicURLFromSectionDoc(row.Value), true
}

// TestPutServerConfigDB_PublicURLRejected checks each rejected form of a
// public URL saved in the settings database: 422 validation_error naming the
// key, the value not echoed, and nothing written (ptone/scion#3905).
func TestPutServerConfigDB_PublicURLRejected(t *testing.T) {
	for name, value := range publicURLRejectedForms {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv, fake, ops := newTestDBServer(t)
			body, err := json.Marshal(map[string]any{"server": map[string]any{"hub": map[string]any{"public_url": value}}})
			require.NoError(t, err)
			rr := putServerConfigDB(t, srv, ops, string(body))
			require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())

			var env struct {
				Error struct {
					Code    string         `json:"code"`
					Message string         `json:"message"`
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &env), rr.Body.String())
			assert.Equal(t, ErrCodeValidationError, env.Error.Code)
			assert.Equal(t, config.PublicURLKey, env.Error.Details["field"])
			assert.NotContains(t, env.Error.Message, value, "the message does not echo the value")

			_, ok := storedEndpointsPublicURL(t, fake)
			assert.False(t, ok, "nothing is written")
		})
	}
}

// TestPutServerConfigDB_PublicURLNormalized checks that an accepted value is
// stored without its trailing slash, and that the save reports the key as
// pending a restart.
func TestPutServerConfigDB_PublicURLNormalized(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fake, ops := newTestDBServerWithStore(t, &settingsAuditStore{})
	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"HTTPS://hub.example.com/"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, ok := storedEndpointsPublicURL(t, fake)
	require.True(t, ok)
	assert.Equal(t, "https://hub.example.com", got)
	assert.Contains(t, rr.Body.String(), `"pending_restart":["server.hub.public_url"]`)
}

// newBootstrapPublicURLServer boots a DB-mode server whose settings.yaml sets
// server.hub.public_url to fileURL. With seeded true, the endpoints row is
// seeded from that bootstrap, as syncHubSettings seeds it at every start;
// otherwise there is no row. The server's store is the returned
// settingsAuditStore.
func newBootstrapPublicURLServer(t *testing.T, fileURL string, seeded bool) (*Server, *fakeHubSettingStore, *OperationalSettings, *settingsAuditStore) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	fake := newFakeHubSettingStore()
	if seeded {
		doc := json.RawMessage(`{"public_url":"` + fileURL + `"}`)
		_, err := fake.UpsertHubSetting(context.Background(), "endpoints", doc, "seed", -1, "seeded")
		require.NoError(t, err)
	}
	ops := NewOperationalSettings(fake, newFileKoanf(t, map[string]any{config.PublicURLKey: fileURL}), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	audits := &settingsAuditStore{}
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, ""), store: audits}
	srv.SetOperationalSettings(ops)
	return srv, fake, ops, audits
}

// TestPutServerConfigDB_BootstrapPublicURLNotStored checks that a settings
// save never stores the bootstrap public URL as a saved value: not when it
// saves another endpoints field, and not when it echoes the public URL the
// GET view shows, either on the first save or on a second save once the
// endpoints row is managed. A later settings.yaml change then still takes
// effect at startup (ptone/scion#3905, ptone/scion#4707).
func TestPutServerConfigDB_BootstrapPublicURLNotStored(t *testing.T) {
	const fileURL = "http://hub.internal:8080" // a file value the database rule would reject
	for _, seeded := range []bool{true, false} {
		for name, body := range map[string]string{
			"sibling field only": `{"server":{"hub":{"monitoring_dashboard_url":"https://dash.example.com/d"}}}`,
			"echoed public URL":  `{"server":{"hub":{"public_url":"` + fileURL + `","monitoring_dashboard_url":"https://dash.example.com/d"}}}`,
			"image registry":     `{"image_registry":"registry.example.com"}`,
		} {
			t.Run(fmt.Sprintf("seeded=%v/%s", seeded, name), func(t *testing.T) {
				srv, fake, ops, audits := newBootstrapPublicURLServer(t, fileURL, seeded)
				rr := putServerConfigDB(t, srv, ops, body)
				require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

				row, err := fake.GetHubSetting(context.Background(), "endpoints")
				require.NoError(t, err)
				assert.Equal(t, "managed", row.Origin, "the save made the endpoints row managed")
				assert.Empty(t, publicURLFromSectionDoc(row.Value), "the bootstrap public URL is not stored")
				assert.Empty(t, audits.records(), "no public URL change is recorded")
				report := saveReport(t, rr)
				assert.NotContains(t, report.PendingRestart, config.PublicURLKey, "the value in effect does not change, so it is not reported")
				assert.NotContains(t, report.AppliedKeys, config.PublicURLKey)
				_, err = ops.Refresh(context.Background())
				require.NoError(t, err)
				assert.Equal(t, fileURL, getPublicURL(t, srv, ops), "GET keeps showing the value in effect")

				// A second save, now on the managed endpoints row, echoes the
				// public URL GET shows. It is still an echo, not a saved value.
				rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"`+fileURL+`"}}}`)
				require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
				row, err = fake.GetHubSetting(context.Background(), "endpoints")
				require.NoError(t, err)
				assert.Equal(t, "managed", row.Origin)
				assert.Empty(t, publicURLFromSectionDoc(row.Value), "the echo on the managed row is not stored")
				assert.Empty(t, audits.records(), "no public URL change is recorded for the echo")
				report = saveReport(t, rr)
				assert.NotContains(t, report.PendingRestart, config.PublicURLKey, "the echo changes nothing in effect")
				assert.NotContains(t, report.AppliedKeys, config.PublicURLKey)
				_, err = ops.Refresh(context.Background())
				require.NoError(t, err)
				assert.Equal(t, fileURL, getPublicURL(t, srv, ops), "GET keeps showing the value in effect")

				stored, err := StoredPublicURL(context.Background(), fake)
				require.NoError(t, err)
				assert.Empty(t, stored)
				resolved, _ := resolveForTest(t, HubEndpoints{Operator: "https://changed-file.example.com", OperatorSource: "file"}, stored)
				assert.Equal(t, "https://changed-file.example.com", resolved.Public, "a later settings.yaml change takes effect")
				assert.Equal(t, "file", resolved.PublicSource)
			})
		}
	}
}

// publicURLSaveReport is the key-level report of a settings save.
type publicURLSaveReport struct {
	AppliedKeys    []string `json:"applied_keys"`
	PendingRestart []string `json:"pending_restart"`
}

// saveReport decodes the key-level report of a successful settings save.
func saveReport(t *testing.T, rr *httptest.ResponseRecorder) publicURLSaveReport {
	t.Helper()
	var resp struct {
		Reload publicURLSaveReport `json:"reload"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp), rr.Body.String())
	return resp.Reload
}

// getPublicURL returns the server.hub.public_url the GET view shows.
func getPublicURL(t *testing.T, srv *Server, ops *OperationalSettings) string {
	t.Helper()
	resp := getServerConfigDB(t, srv, ops)
	require.NotNil(t, resp.Server)
	require.NotNil(t, resp.Server.Hub)
	return resp.Server.Hub.PublicURL
}

// publicURLSuperseded reports whether a GET response lists the public URL
// among the superseded endpoints keys.
func publicURLSuperseded(resp ServerConfigDBResponse) bool {
	for _, sk := range resp.SupersededKeys["endpoints"] {
		if sk.Key == config.PublicURLKey {
			return true
		}
	}
	return false
}

// newManagedPublicURLServer boots a DB-mode server whose bootstrap material
// sets server.hub.public_url to bootURL, with a managed endpoints row that
// stores savedURL, and endpoints resolved with the operator pin pin ("" for
// none).
func newManagedPublicURLServer(t *testing.T, bootURL, savedURL, pin string) (*Server, *OperationalSettings) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	fake := newFakeHubSettingStore()
	fake.seedWithOrigin("endpoints", json.RawMessage(`{"public_url":"`+savedURL+`"}`), "managed")
	ops := NewOperationalSettings(fake, newFileKoanf(t, map[string]any{config.PublicURLKey: bootURL}), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, ""), store: &settingsAuditStore{}}
	source := "file"
	if pin != "" {
		source = "env"
	}
	srv.config.HubEndpoints = &HubEndpoints{Operator: bootURL, OperatorSource: source, Pin: pin, Public: bootURL, PublicSource: source}
	srv.SetOperationalSettings(ops)
	return srv, ops
}

// TestGetServerConfigDB_PinnedPublicURLNotSuperseded checks that a saved
// public URL is not reported as superseding the bootstrap value while an
// operator setting pins the endpoint (the pinned value wins), and is
// reported when nothing pins it (ptone/scion#3905).
func TestGetServerConfigDB_PinnedPublicURLNotSuperseded(t *testing.T) {
	const bootURL, savedURL = "https://env.example.com", "https://saved.example.com"

	srv, ops := newManagedPublicURLServer(t, bootURL, savedURL, "SCION_SERVER_HUB_ENDPOINT")
	assert.False(t, publicURLSuperseded(getServerConfigDB(t, srv, ops)),
		"a pinned public URL is not superseded by the database value")

	srv, ops = newManagedPublicURLServer(t, bootURL, savedURL, "")
	assert.True(t, publicURLSuperseded(getServerConfigDB(t, srv, ops)),
		"without a pin the saved value supersedes the bootstrap value")
}

// TestGetServerConfigDB_PinnedPublicURLKeepsSiblingsSuperseded checks that a
// pin drops only the public URL from the superseded endpoints keys: another
// endpoints key the database value supersedes is still listed. The hub owns
// this filter; the admin page shows the list as it is sent
// (ptone/scion#4707).
func TestGetServerConfigDB_PinnedPublicURLKeepsSiblingsSuperseded(t *testing.T) {
	const bootURL = "https://env.example.com"
	t.Setenv("HOME", t.TempDir())
	fake := newFakeHubSettingStore()
	fake.seedWithOrigin("endpoints",
		json.RawMessage(`{"public_url":"https://saved.example.com","image_registry":"saved.example.com"}`), "managed")
	ops := NewOperationalSettings(fake, newFileKoanf(t, map[string]any{
		config.PublicURLKey: bootURL,
		"image_registry":    "file.example.com",
	}), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, ""), store: &settingsAuditStore{}}
	srv.config.HubEndpoints = &HubEndpoints{Operator: bootURL, OperatorSource: "env", Pin: "SCION_SERVER_HUB_ENDPOINT", Public: bootURL, PublicSource: "env"}
	srv.SetOperationalSettings(ops)

	resp := getServerConfigDB(t, srv, ops)
	assert.False(t, publicURLSuperseded(resp), "a pinned public URL is not superseded")
	var keys []string
	for _, sk := range resp.SupersededKeys["endpoints"] {
		keys = append(keys, sk.Key)
	}
	assert.Equal(t, []string{"image_registry"}, keys, "the other superseded endpoints key is still listed")
}

// TestPutServerConfigDB_ClearedPublicURLNotSuperseded checks that clearing
// a saved public URL leaves the bootstrap value in effect: GET shows it, it
// is no longer reported as superseded, and the save reports the change as
// pending a restart (ptone/scion#3905).
func TestPutServerConfigDB_ClearedPublicURLNotSuperseded(t *testing.T) {
	const bootURL, savedURL = "https://file.example.com", "https://saved.example.com"
	srv, ops := newManagedPublicURLServer(t, bootURL, savedURL, "")
	require.True(t, publicURLSuperseded(getServerConfigDB(t, srv, ops)))

	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":""}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, []string{config.PublicURLKey}, saveReport(t, rr).PendingRestart)
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)

	assert.False(t, publicURLSuperseded(getServerConfigDB(t, srv, ops)), "a cleared public URL supersedes nothing")
	assert.Equal(t, bootURL, getPublicURL(t, srv, ops), "GET shows the bootstrap value now in effect")
}

// publicURLAuditFixture is a DB-mode server with a SQLite store for audit
// records. The store is installed behind a publicURLFailingAuditStore whose
// audit writes fail once the returned switch is armed.
func publicURLAuditFixture(t *testing.T) (*Server, *fakeHubSettingStore, *OperationalSettings, store.Store, *storeFaultSwitch) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	srv, fake, ops := newTestDBServer(t)
	st := newGCPIdentitySettingsStore(t)
	_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *publicURLFailingAuditStore {
		require.Nil(t, inner, "the DB-mode test server has no store of its own")
		return &publicURLFailingAuditStore{Store: st, fault: f}
	})
	return srv, fake, ops, st, fault
}

// publicURLAudits returns the audit records of mutationType for the public
// URL key.
func publicURLAudits(t *testing.T, st store.Store, mutationType string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := st.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: mutationType})
	require.NoError(t, err)
	var out []*store.MutationAuditRecord
	for _, r := range recs {
		if r.TargetID == config.PublicURLKey {
			out = append(out, r)
		}
	}
	return out
}

func resetEndpoints(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfigSectionReset(rr,
		adminRequest(http.MethodDelete, "/api/v1/admin/server-config/sections/endpoints", ""))
	return rr
}

// TestPublicURLWrites_Audited checks that a change to the saved public URL,
// by a save or by a section reset, is recorded through the hub settings
// audit path with the actor and the old and new values, and that an
// unchanged value records nothing.
func TestPublicURLWrites_Audited(t *testing.T) {
	srv, _, ops, st, _ := publicURLAuditFixture(t)

	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"https://one.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"https://two.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"https://two.example.com"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	reset := resetEndpoints(t, srv)
	require.Equal(t, http.StatusOK, reset.Code, reset.Body.String())

	recs := publicURLAudits(t, st, gcpIAMAuditMutation)
	require.Len(t, recs, 3, "two saves and a reset")
	pairs := map[string]string{}
	for _, r := range recs {
		assert.Equal(t, gcpIAMAuditTarget, r.TargetType)
		assert.NotEmpty(t, r.ActorPrincipalID, "the actor is recorded")
		pairs[r.BeforeSummary] = r.AfterSummary
	}
	assert.Equal(t, map[string]string{
		"":                        "https://one.example.com",
		"https://one.example.com": "https://two.example.com",
		"https://two.example.com": "",
	}, pairs)
	assert.Empty(t, publicURLAudits(t, st, gcpIAMNotAppliedMutation))
}

// TestPublicURLWrites_ConflictRecordsNotApplied checks that a save that
// hits a revision conflict leaves the change recorded as not applied, as
// the GCP permission-check settings do.
func TestPublicURLWrites_ConflictRecordsNotApplied(t *testing.T) {
	srv, fake, ops, st, _ := publicURLAuditFixture(t)

	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"https://new.example.com"}},"expected_revisions":{"endpoints":99}}`)
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	_, err := fake.GetHubSetting(context.Background(), "endpoints")
	require.ErrorIs(t, err, store.ErrNotFound, "nothing is written")

	notApplied := publicURLAudits(t, st, gcpIAMNotAppliedMutation)
	require.Len(t, notApplied, 1)
	assert.Equal(t, "", notApplied[0].BeforeSummary)
	assert.Equal(t, "https://new.example.com", notApplied[0].AfterSummary)
}

// TestPublicURLWrites_SeededResetRecordsNothing checks that resetting a
// seeded endpoints row records nothing: it holds bootstrap material, not a
// saved value.
func TestPublicURLWrites_SeededResetRecordsNothing(t *testing.T) {
	srv, fake, ops, st, _ := publicURLAuditFixture(t)
	_, err := fake.UpsertHubSetting(context.Background(), "endpoints",
		json.RawMessage(`{"public_url":"https://file.example.com"}`), "seed", -1, "seeded")
	require.NoError(t, err)
	_, err = ops.Refresh(context.Background())
	require.NoError(t, err)

	reset := resetEndpoints(t, srv)
	require.Equal(t, http.StatusOK, reset.Code, reset.Body.String())
	assert.Empty(t, publicURLAudits(t, st, gcpIAMAuditMutation))
	assert.Empty(t, publicURLAudits(t, st, gcpIAMNotAppliedMutation))
}

// publicURLFailingAuditStore is a store whose audit writes fail while its
// fault switch is armed.
type publicURLFailingAuditStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (f *publicURLFailingAuditStore) CreateMutationAudit(ctx context.Context, rec *store.MutationAuditRecord) error {
	if !f.fault.Active() {
		return f.Store.CreateMutationAudit(ctx, rec)
	}
	return errors.New("injected audit failure")
}

// TestPublicURLWrites_AuditFailureBlocksChange checks that a public URL
// change whose audit record cannot be written is not made, by a save or by
// a reset.
func TestPublicURLWrites_AuditFailureBlocksChange(t *testing.T) {
	srv, fake, ops, _, fault := publicURLAuditFixture(t)
	_, err := fake.UpsertHubSetting(context.Background(), "endpoints",
		json.RawMessage(`{"public_url":"https://saved.example.com"}`), "admin@example.com", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Refresh(context.Background())
	require.NoError(t, err)
	fault.Arm()

	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"https://new.example.com"}}}`)
	require.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())
	got, ok := storedEndpointsPublicURL(t, fake)
	require.True(t, ok)
	assert.Equal(t, "https://saved.example.com", got, "the save is not made")

	reset := resetEndpoints(t, srv)
	require.Equal(t, http.StatusInternalServerError, reset.Code, reset.Body.String())
	got, ok = storedEndpointsPublicURL(t, fake)
	require.True(t, ok)
	assert.Equal(t, "https://saved.example.com", got, "the reset is not made")
}

// publicURLFailingSettingStore is a settings store whose writes fail once
// failWrites is set.
type publicURLFailingSettingStore struct {
	*fakeHubSettingStore
	failWrites bool
}

func (f *publicURLFailingSettingStore) UpsertHubSetting(ctx context.Context, section string, value json.RawMessage, updatedBy string, expectedRevision int64, origin string) (*store.HubSetting, error) {
	if f.failWrites {
		return nil, errors.New("injected settings write failure")
	}
	return f.fakeHubSettingStore.UpsertHubSetting(ctx, section, value, updatedBy, expectedRevision, origin)
}

func (f *publicURLFailingSettingStore) DeleteHubSetting(ctx context.Context, section string) error {
	if f.failWrites {
		return errors.New("injected settings write failure")
	}
	return f.fakeHubSettingStore.DeleteHubSetting(ctx, section)
}

// TestPublicURLWrites_StoreErrorRecordsNotApplied checks that a recorded
// public URL change whose write fails with a store error, by a save or by a
// reset, returns 500 and leaves one not-applied record each.
func TestPublicURLWrites_StoreErrorRecordsNotApplied(t *testing.T) {
	srv, fake, ops, st, _ := publicURLAuditFixture(t)
	_, err := fake.UpsertHubSetting(context.Background(), "endpoints",
		json.RawMessage(`{"public_url":"https://saved.example.com"}`), "admin@example.com", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Refresh(context.Background())
	require.NoError(t, err)
	failing := &publicURLFailingSettingStore{fakeHubSettingStore: fake, failWrites: true}
	ops.store = failing

	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"https://new.example.com"}}}`)
	require.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())
	reset := resetEndpoints(t, srv)
	require.Equal(t, http.StatusInternalServerError, reset.Code, reset.Body.String())

	got, ok := storedEndpointsPublicURL(t, fake)
	require.True(t, ok)
	assert.Equal(t, "https://saved.example.com", got, "neither write is made")

	notApplied := publicURLAudits(t, st, gcpIAMNotAppliedMutation)
	require.Len(t, notApplied, 2, "one for the save and one for the reset")
	pairs := map[string]string{}
	surfaces := map[string]bool{}
	for _, r := range notApplied {
		pairs[r.AfterSummary] = r.BeforeSummary
		surfaces[r.ExecutorID] = true
	}
	assert.Equal(t, map[string]string{
		"https://new.example.com": "https://saved.example.com",
		"":                        "https://saved.example.com",
	}, pairs)
	assert.Equal(t, map[string]bool{gcpIAMSurfaceSave: true, gcpIAMSurfaceReset: true}, surfaces)
}

// newSeededPublicURLServer boots a DB-mode server whose hub endpoint is
// supplied as a seed value (SCION_SEED_SERVER_HUB_ENDPOINT=seedURL) and, when
// fileURL is not "", in settings.yaml. The bootstrap material is loaded and
// wired as startup does (config.LoadBootstrapKoanf, SetUnseededBootstrap).
// With seeded true, the endpoints row is seeded from the full bootstrap, as
// syncHubSettings seeds it at every start; otherwise there is no row. The
// endpoints are resolved as startup resolves them: the stored value
// (StoredPublicURL) over the operator endpoint, which is fileURL (source
// "file") or, without one, the fallback operator (source "localhost"); a
// seed value sets neither.
func newSeededPublicURLServer(t *testing.T, seedURL, fileURL, operator string, seeded bool) (*Server, *fakeHubSettingStore, *OperationalSettings, *settingsAuditStore) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_SERVER_HUB_ENDPOINT", "")
	t.Setenv("SCION_SEED_SERVER_HUB_ENDPOINT", seedURL)
	base := HubEndpoints{Operator: operator, OperatorSource: "localhost"}
	if fileURL != "" {
		scionDir := filepath.Join(home, ".scion")
		require.NoError(t, os.MkdirAll(scionDir, 0o755))
		content := "schema_version: \"1\"\nserver:\n  hub:\n    public_url: " + fileURL + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(content), 0o644))
		base = HubEndpoints{Operator: fileURL, OperatorSource: "file"}
	}
	bootK := config.LoadBootstrapKoanf()
	wantBoot := seedURL
	if fileURL != "" {
		wantBoot = fileURL
	}
	require.Equal(t, wantBoot, bootK.String(config.PublicURLKey), "the bootstrap material holds the seed value under settings.yaml")

	fake := newFakeHubSettingStore()
	if seeded {
		doc, err := opsettings.ExtractSectionFromKoanf(bootK, "endpoints")
		require.NoError(t, err)
		require.Equal(t, wantBoot, publicURLFromSectionDoc(doc), "the seeded row carries the bootstrap value")
		_, err = fake.UpsertHubSetting(ctx, "endpoints", doc, "seed", -1, "seeded")
		require.NoError(t, err)
	}
	ops := NewOperationalSettings(fake, bootK, emptyKoanf())
	ops.SetUnseededBootstrap(config.LoadUnseededBootstrapKoanfWithConfigPath(""))
	_, err := ops.Refresh(ctx)
	require.NoError(t, err)

	stored, err := StoredPublicURL(ctx, fake)
	require.NoError(t, err)
	require.Empty(t, stored, "startup ignores seeded rows")
	endpoints, _ := resolveForTest(t, base, stored)

	audits := &settingsAuditStore{}
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, ""), store: audits}
	srv.config.HubEndpoints = &endpoints
	srv.SetOperationalSettings(ops)
	return srv, fake, ops, audits
}

// TestServerConfigDB_SeedOnlyPublicURL checks a hub whose endpoint is
// supplied only as a seed value, which startup ignores: GET shows an empty
// public_url field and the endpoint in effect in public_url_status; saving
// the empty field stores nothing; saving the seed value is a change, not an
// echo, so it is stored, recorded and reported as pending a restart, and GET
// then shows it as the saved value (ptone/scion#4826).
func TestServerConfigDB_SeedOnlyPublicURL(t *testing.T) {
	const seedURL = "https://seed.example.com"
	const operator = "http://localhost:8080"
	for _, seeded := range []bool{true, false} {
		t.Run(fmt.Sprintf("seeded=%v", seeded), func(t *testing.T) {
			srv, fake, ops, audits := newSeededPublicURLServer(t, seedURL, "", operator, seeded)

			resp := getServerConfigDB(t, srv, ops)
			require.NotNil(t, resp.Server)
			require.NotNil(t, resp.Server.Hub)
			assert.Empty(t, resp.Server.Hub.PublicURL, "the field does not show the seed value")
			require.NotNil(t, resp.PublicURLStatus)
			assert.Equal(t, PublicURLStatus{Effective: operator, Source: "localhost"}, *resp.PublicURLStatus,
				"the in-effect hint shows the endpoint startup resolved")

			// Saving the field as GET shows it (empty) stores nothing.
			rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"","monitoring_dashboard_url":"https://dash.example.com/d"}}}`)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			got, _ := storedEndpointsPublicURL(t, fake)
			assert.Empty(t, got, "no public URL is stored")
			assert.Empty(t, audits.records(), "no public URL change is recorded")
			report := saveReport(t, rr)
			assert.NotContains(t, report.PendingRestart, config.PublicURLKey, "the endpoint in effect does not change")
			_, err := ops.Refresh(context.Background())
			require.NoError(t, err)
			assert.Empty(t, getPublicURL(t, srv, ops))

			// Saving the seed value is a change: it is not an echo of the
			// value GET shows.
			rr = putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"`+seedURL+`"}}}`)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			got, ok := storedEndpointsPublicURL(t, fake)
			require.True(t, ok)
			assert.Equal(t, seedURL, got, "the seed value is stored as a saved value")
			recs := audits.records()
			require.Len(t, recs, 1, "the change is recorded")
			assert.Empty(t, recs[0].BeforeSummary)
			assert.Equal(t, seedURL, recs[0].AfterSummary)
			report = saveReport(t, rr)
			assert.Equal(t, []string{config.PublicURLKey}, report.PendingRestart, "the endpoint in effect changes at the next start")
			_, err = ops.Refresh(context.Background())
			require.NoError(t, err)
			assert.Equal(t, seedURL, getPublicURL(t, srv, ops), "a saved value shows")

			stored, err := StoredPublicURL(context.Background(), fake)
			require.NoError(t, err)
			assert.Equal(t, seedURL, stored, "startup now uses the saved value")
		})
	}
}

// TestServerConfigDB_SeedUnderFilePublicURL checks that with a seed value
// and a settings.yaml value, GET shows the settings.yaml value, which
// startup uses, and an echo of it is still not stored (ptone/scion#3905,
// ptone/scion#4826).
func TestServerConfigDB_SeedUnderFilePublicURL(t *testing.T) {
	const seedURL, fileURL = "https://seed.example.com", "https://file.example.com"
	for _, seeded := range []bool{true, false} {
		t.Run(fmt.Sprintf("seeded=%v", seeded), func(t *testing.T) {
			srv, fake, ops, audits := newSeededPublicURLServer(t, seedURL, fileURL, "", seeded)

			resp := getServerConfigDB(t, srv, ops)
			require.NotNil(t, resp.Server)
			require.NotNil(t, resp.Server.Hub)
			assert.Equal(t, fileURL, resp.Server.Hub.PublicURL, "the field shows the settings.yaml value")
			require.NotNil(t, resp.PublicURLStatus)
			assert.Equal(t, PublicURLStatus{Effective: fileURL, Source: "file"}, *resp.PublicURLStatus)

			rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"public_url":"`+fileURL+`","monitoring_dashboard_url":"https://dash.example.com/d"}}}`)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			got, _ := storedEndpointsPublicURL(t, fake)
			assert.Empty(t, got, "the echoed settings.yaml value is not stored")
			assert.Empty(t, audits.records(), "no public URL change is recorded")
			assert.NotContains(t, saveReport(t, rr).PendingRestart, config.PublicURLKey)
			_, err := ops.Refresh(context.Background())
			require.NoError(t, err)
			assert.Equal(t, fileURL, getPublicURL(t, srv, ops), "GET keeps showing the settings.yaml value")
		})
	}
}
