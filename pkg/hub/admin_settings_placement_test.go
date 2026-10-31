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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// Kubernetes placement and identity checks on a server-config save
// (ptone/scion#6580, config.RuntimeSaveChecks): a value the save sets is
// refused naming the field; a stored problem the save does not touch is
// accepted with a warning; a sibling disagreement the save creates is
// refused, one already stored is a warning, and fixing it is accepted.

const placementStored = `{"a":{"type":"kubernetes","context":"c","namespace":"agents","node_selector":{"pool":"a"}},
"b":{"type":"kubernetes","context":"c","namespace":"agents","node_selector":{"pool":"b"}},
"bad":{"type":"kubernetes","context":"c","namespace":"other","runtime_class_name":"Not_Valid"}}`

func placementDBPut(t *testing.T, stored, body string) *httptest.ResponseRecorder {
	t.Helper()
	srv, fake, ops := newTestDBServer(t)
	if stored != "" {
		if _, err := fake.UpsertHubSetting(context.Background(), "runtimes", json.RawMessage(stored), "test", -1, "managed"); err != nil {
			t.Fatal(err)
		}
	}
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
	return rr
}

func placementFilePut(t *testing.T, stored, body string) *httptest.ResponseRecorder {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if stored != "" {
		var m map[string]any
		if err := json.Unmarshal([]byte(stored), &m); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(map[string]any{"schema_version": "1", "runtimes": m})
		// JSON is valid YAML.
		if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rr := httptest.NewRecorder()
	(&Server{}).handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
	return rr
}

func TestPutServerConfig_PlacementSaveChecks(t *testing.T) {
	// bodies send every stored entry (as the admin page does) plus the change.
	keep := `"a":{"type":"kubernetes","context":"c","namespace":"agents","node_selector":{"pool":"a"}},"b":{"type":"kubernetes","context":"c","namespace":"agents","node_selector":{"pool":"b"}},"bad":{"type":"kubernetes","context":"c","namespace":"other","runtime_class_name":"Not_Valid"}`
	tests := []struct {
		name, stored, body string
		wantErr            bool
		want               string
	}{
		{"invalid selector the save sets", "", `{"runtimes":{"n":{"type":"kubernetes","node_selector":{"bad key!":"v"}}}}`, true, "runtimes.n.node_selector"},
		{"invalid toleration the save sets", "", `{"runtimes":{"n":{"type":"kubernetes","tolerations":[{"key":"k","operator":"Exists","value":"x"}]}}}`, true, "runtimes.n.tolerations"},
		{"invalid mapping the save sets", "", `{"runtimes":{"n":{"type":"kubernetes","kubernetes_service_account_mappings":{"sa@p.iam.gserviceaccount.com":"Bad_Name"}}}}`, true, "runtimes.n.kubernetes_service_account_mappings"},
		{"invalid block SA the save sets", "", `{"runtimes":{"n":{"type":"kubernetes","kubernetes_block_service_account":"Bad_Name"}}}`, true, "runtimes.n.kubernetes_block_service_account"},
		{"valid full entry", "", `{"runtimes":{"n":{"type":"kubernetes","node_selector":{"scion.dev/pool":"agents"},"tolerations":[{"key":"scion.dev/pool","operator":"Equal","value":"agents","effect":"NoSchedule"}],"runtime_class_name":"gvisor","kubernetes_service_account_mappings":{"sa@p.iam.gserviceaccount.com":"agent-ksa"},"kubernetes_block_service_account":"block-ksa"}}}`, false, `"status"`},
		{"untouched stored problems are warnings", placementStored, `{"runtimes":{` + keep + `,"x":{"type":"docker"}}}`, false, "runtimes.bad.runtime_class_name"},
		{"untouched stored disagreement is a warning", placementStored, `{"runtimes":{` + keep + `,"x":{"type":"docker"}}}`, false, "same cluster and namespace"},
		{"a new disagreeing entry is refused", placementStored, `{"runtimes":{` + keep + `,"c":{"type":"kubernetes","context":"c","namespace":"agents","node_selector":{"pool":"c"}}}}`, true, "runtimes.a"},
		{"fixing the disagreement is accepted", placementStored, `{"runtimes":{"a":{"type":"kubernetes","context":"c","namespace":"agents","node_selector":{"pool":"a"}},"b":{"type":"kubernetes","context":"c","namespace":"agents","node_selector":{"pool":"a"}},"bad":{"type":"kubernetes","context":"c","namespace":"other","runtime_class_name":"Not_Valid"}}}`, false, `"status"`},
	}
	for _, tt := range tests {
		t.Run("db/"+tt.name, func(t *testing.T) {
			rr := placementDBPut(t, tt.stored, tt.body)
			want := http.StatusOK
			if tt.wantErr {
				want = http.StatusUnprocessableEntity
			}
			if rr.Code != want || !strings.Contains(rr.Body.String(), tt.want) {
				t.Fatalf("got %d, want %d containing %q: %s", rr.Code, want, tt.want, rr.Body.String())
			}
		})
		t.Run("file/"+tt.name, func(t *testing.T) {
			rr := placementFilePut(t, tt.stored, tt.body)
			want := http.StatusOK
			if tt.wantErr {
				want = http.StatusBadRequest
			}
			if rr.Code != want || !strings.Contains(rr.Body.String(), tt.want) {
				t.Fatalf("got %d, want %d containing %q: %s", rr.Code, want, tt.want, rr.Body.String())
			}
		})
	}
}

// A refused save writes nothing: the stored runtimes row is unchanged.
func TestPutServerConfigDB_PlacementRefusalWritesNothing(t *testing.T) {
	srv, fake, ops := newTestDBServer(t)
	ctx := context.Background()
	if _, err := fake.UpsertHubSetting(ctx, "runtimes", json.RawMessage(placementStored), "test", -1, "managed"); err != nil {
		t.Fatal(err)
	}
	before, _ := fake.GetHubSetting(ctx, "runtimes")
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"runtimes":{"a":{"type":"kubernetes","context":"c","namespace":"agents","runtime_class_name":"Not_Valid"}}}`), ops)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	after, _ := fake.GetHubSetting(ctx, "runtimes")
	if after.Revision != before.Revision || string(after.Value) != string(before.Value) {
		t.Errorf("the runtimes row changed on a refused save")
	}
}

// A hub whose runtimes come from bootstrap settings (no runtimes row yet)
// treats those values as stored: an untouched invalid value or
// disagreement is a warning on the first save, not a refusal, and a save
// that fixes it is accepted.
func TestPutServerConfigDB_PlacementBootstrapOriginIsStored(t *testing.T) {
	newSrv := func(t *testing.T) (*Server, *OperationalSettings) {
		t.Helper()
		fileK := koanf.New(".")
		if err := fileK.Load(confmap.Provider(map[string]interface{}{
			"runtimes.a.type":                 "kubernetes",
			"runtimes.a.context":              "c",
			"runtimes.a.namespace":            "agents",
			"runtimes.a.node_selector.pool":   "a",
			"runtimes.b.type":                 "kubernetes",
			"runtimes.b.context":              "c",
			"runtimes.b.namespace":            "agents",
			"runtimes.b.node_selector.pool":   "b",
			"runtimes.bad.type":               "kubernetes",
			"runtimes.bad.namespace":          "other",
			"runtimes.bad.runtime_class_name": "Not_Valid",
		}, "."), nil); err != nil {
			t.Fatal(err)
		}
		ops := NewOperationalSettings(newFakeHubSettingStore(), fileK, emptyKoanf())
		srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
		srv.SetOperationalSettings(ops)
		return srv, ops
	}
	untouched := `{"runtimes":{"a":{"type":"kubernetes","context":"c","namespace":"agents","node_selector":{"pool":"a"}},"b":{"type":"kubernetes","context":"c","namespace":"agents","node_selector":{"pool":"b"}},"bad":{"type":"kubernetes","namespace":"other","runtime_class_name":"Not_Valid"},"x":{"type":"docker"}}}`
	fixed := `{"runtimes":{"a":{"type":"kubernetes","context":"c","namespace":"agents","node_selector":{"pool":"a"}},"b":{"type":"kubernetes","context":"c","namespace":"agents","node_selector":{"pool":"a"}},"bad":{"type":"kubernetes","namespace":"other","runtime_class_name":"gvisor"}}}`
	t.Run("untouched stored problems warn", func(t *testing.T) {
		srv, ops := newSrv(t)
		rr := putServerConfigDB(t, srv, ops, untouched)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		for _, want := range []string{"runtimes.bad.runtime_class_name", "same cluster and namespace"} {
			if !strings.Contains(rr.Body.String(), want) {
				t.Errorf("warnings should contain %q: %s", want, rr.Body.String())
			}
		}
	})
	t.Run("a fix is accepted", func(t *testing.T) {
		srv, ops := newSrv(t)
		rr := putServerConfigDB(t, srv, ops, fixed)
		if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "runtimes.bad.runtime_class_name") {
			t.Fatalf("expected a clean 200, got %d: %s", rr.Code, rr.Body.String())
		}
	})
}

// priority_class_name and the identity fields follow the save rule now
// that the section schema carries their types only.
func TestPutServerConfig_IdentityFieldsSaveRule(t *testing.T) {
	stored := `{"k":{"type":"kubernetes","priority_class_name":"Bad_Name","kubernetes_service_account_mappings":{"NotAnEmail":"ksa"},"kubernetes_block_service_account":"Bad_Name"}}`
	tests := []struct {
		name, stored, body string
		wantErr            bool
		want               string
	}{
		{"new invalid priority class refused", "", `{"runtimes":{"k":{"type":"kubernetes","priority_class_name":"Bad_Name"}}}`, true, "runtimes.k.priority_class_name"},
		{"new invalid mapping key refused", "", `{"runtimes":{"k":{"type":"kubernetes","kubernetes_service_account_mappings":{"NotAnEmail":"ksa"}}}}`, true, "runtimes.k.kubernetes_service_account_mappings"},
		{"untouched stored invalid values warn", stored, `{"runtimes":{"k":{"type":"kubernetes","priority_class_name":"Bad_Name","kubernetes_service_account_mappings":{"NotAnEmail":"ksa"},"kubernetes_block_service_account":"Bad_Name","gke":true}}}`, false, "runtimes.k.priority_class_name"},
		{"fixed in the same save accepted", stored, `{"runtimes":{"k":{"type":"kubernetes","priority_class_name":"agents","kubernetes_service_account_mappings":{"sa@p.iam.gserviceaccount.com":"ksa"},"kubernetes_block_service_account":"block-ksa"}}}`, false, `"status"`},
	}
	for _, tt := range tests {
		t.Run("db/"+tt.name, func(t *testing.T) {
			rr := placementDBPut(t, tt.stored, tt.body)
			want := http.StatusOK
			if tt.wantErr {
				want = http.StatusUnprocessableEntity
			}
			if rr.Code != want || !strings.Contains(rr.Body.String(), tt.want) {
				t.Fatalf("got %d, want %d containing %q: %s", rr.Code, want, tt.want, rr.Body.String())
			}
		})
		t.Run("file/"+tt.name, func(t *testing.T) {
			rr := placementFilePut(t, tt.stored, tt.body)
			want := http.StatusOK
			if tt.wantErr {
				want = http.StatusBadRequest
			}
			if rr.Code != want || !strings.Contains(rr.Body.String(), tt.want) {
				t.Fatalf("got %d, want %d containing %q: %s", rr.Code, want, tt.want, rr.Body.String())
			}
		})
	}
}
