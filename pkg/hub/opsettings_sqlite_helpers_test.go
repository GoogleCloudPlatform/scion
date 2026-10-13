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
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/knadh/koanf/v2"
)

// newSQLiteOpsServer builds a SQLite-driver Server backed by a real migrated
// SQLite store with OperationalSettings wired the way initOperationalSettings
// does it: seed rows are written first by the caller (via seed), then Refresh,
// ApplySnapshot, SetOperationalSettings. ops.server is set so ops.Update
// self-applies, as StartPropagation arranges in production.
func newSQLiteOpsServer(t *testing.T, bootstrap *koanf.Koanf, seed map[string]string) (*Server, store.Store, *OperationalSettings) {
	t.Helper()
	st, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}

	ctx := context.Background()
	for section, doc := range seed {
		if _, err := st.UpsertHubSetting(ctx, section, json.RawMessage(doc), "system", -1, "seeded"); err != nil {
			t.Fatalf("seed %s: %v", section, err)
		}
	}

	if bootstrap == nil {
		bootstrap = emptyKoanf()
	}
	ops := NewOperationalSettings(st, bootstrap, emptyKoanf())
	if _, err := ops.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	srv := &Server{
		dbDriver:       "sqlite",
		store:          st,
		maintenance:    NewMaintenanceState(false, ""),
		maintenanceLog: logging.Subsystem("hub.maintenance"),
	}
	snap := ops.Snapshot()
	ApplySnapshot(srv, snap)
	ApplyMaintenanceFromSnapshot(srv, snap)
	srv.SetOperationalSettings(ops)
	ops.server = srv
	return srv, st, ops
}

// unrelatedLifecycleUpdate is the trigger from the ptone/scion#1091 repro: another admin
// write to a different section, which re-applies the whole DB snapshot.
func unrelatedLifecycleUpdate(t *testing.T, ops *OperationalSettings) {
	t.Helper()
	if _, err := ops.Update(context.Background(), "lifecycle",
		json.RawMessage(`{"auto_suspend_stalled":true}`), "other@example.com", -1, "managed"); err != nil {
		t.Fatalf("unrelated lifecycle update: %v", err)
	}
}

func hubSettingDocMap(t *testing.T, st store.Store, section string) (*store.HubSetting, map[string]interface{}) {
	t.Helper()
	rec, err := st.GetHubSetting(context.Background(), section)
	if err != nil {
		t.Fatalf("get hub setting %s: %v", section, err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(rec.Value, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", section, err)
	}
	return rec, m
}

// hubSettingRevisions maps each hub_settings section to its revision.
func hubSettingRevisions(t *testing.T, st store.Store) map[string]int64 {
	t.Helper()
	rows, err := st.ListHubSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Section] = r.Revision
	}
	return out
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// putServerConfig issues PUT /api/v1/admin/server-config through the
// dispatcher and returns the recorder.
func putServerConfig(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
	return rr
}

func rejectedKeys(t *testing.T, rr *httptest.ResponseRecorder) (string, []string) {
	t.Helper()
	var resp struct {
		Error string   `json:"error"`
		Keys  []string `json:"keys"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %s: %v", rr.Body.String(), err)
	}
	return resp.Error, resp.Keys
}
