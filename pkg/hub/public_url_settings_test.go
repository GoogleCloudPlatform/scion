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

//go:build !no_sqlite && (!hubshard || hubshard_5)

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
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
	srv, fake, ops := newTestDBServer(t)
	withSettingsAuditStore(srv)
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
// otherwise there is no row.
func newBootstrapPublicURLServer(t *testing.T, fileURL string, seeded bool) (*Server, *fakeHubSettingStore, *OperationalSettings) {
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
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)
	return srv, fake, ops
}

// TestPutServerConfigDB_BootstrapPublicURLNotStored checks that a settings
// save never stores the bootstrap public URL as a saved value: not when it
// saves another endpoints field, and not when it echoes the public URL the
// GET view shows. A later settings.yaml change then still takes effect at
// startup (ptone/scion#3905).
func TestPutServerConfigDB_BootstrapPublicURLNotStored(t *testing.T) {
	const fileURL = "http://hub.internal:8080" // a file value the database rule would reject
	for _, seeded := range []bool{true, false} {
		for name, body := range map[string]string{
			"sibling field only": `{"server":{"hub":{"monitoring_dashboard_url":"https://dash.example.com/d"}}}`,
			"echoed public URL":  `{"server":{"hub":{"public_url":"` + fileURL + `","monitoring_dashboard_url":"https://dash.example.com/d"}}}`,
			"image registry":     `{"image_registry":"registry.example.com"}`,
		} {
			t.Run(fmt.Sprintf("seeded=%v/%s", seeded, name), func(t *testing.T) {
				srv, fake, ops := newBootstrapPublicURLServer(t, fileURL, seeded)
				audits := withSettingsAuditStore(srv)
				rr := putServerConfigDB(t, srv, ops, body)
				require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

				row, err := fake.GetHubSetting(context.Background(), "endpoints")
				require.NoError(t, err)
				assert.Equal(t, "managed", row.Origin, "the save made the endpoints row managed")
				assert.Empty(t, publicURLFromSectionDoc(row.Value), "the bootstrap public URL is not stored")
				assert.Empty(t, audits.records(), "no public URL change is recorded")

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

// publicURLAuditFixture is a DB-mode server with a SQLite store for audit
// records.
func publicURLAuditFixture(t *testing.T) (*Server, *fakeHubSettingStore, *OperationalSettings, store.Store) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	srv, fake, ops := newTestDBServer(t)
	st := newGCPIdentitySettingsStore(t)
	srv.store = st
	return srv, fake, ops, st
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
	srv, _, ops, st := publicURLAuditFixture(t)

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
	srv, fake, ops, st := publicURLAuditFixture(t)

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
	srv, fake, ops, st := publicURLAuditFixture(t)
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

// publicURLFailingAuditStore is a store whose audit writes fail.
type publicURLFailingAuditStore struct {
	store.Store
}

func (publicURLFailingAuditStore) CreateMutationAudit(context.Context, *store.MutationAuditRecord) error {
	return errors.New("injected audit failure")
}

// TestPublicURLWrites_AuditFailureBlocksChange checks that a public URL
// change whose audit record cannot be written is not made, by a save or by
// a reset.
func TestPublicURLWrites_AuditFailureBlocksChange(t *testing.T) {
	srv, fake, ops, st := publicURLAuditFixture(t)
	_, err := fake.UpsertHubSetting(context.Background(), "endpoints",
		json.RawMessage(`{"public_url":"https://saved.example.com"}`), "admin@example.com", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.store = publicURLFailingAuditStore{Store: st}

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
	srv, fake, ops, st := publicURLAuditFixture(t)
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
