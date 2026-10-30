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
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	yamlv3 "gopkg.in/yaml.v3"
)

// A stored Kubernetes runtime entry as an operator sets it through the API,
// with one key V1RuntimeConfig does not model (resources).
const storedK8sRuntimes = `{"gke":{"type":"kubernetes","namespace":"agents","gke":true,
"node_selector":{"scion.dev/pool":"agents"},
"tolerations":[{"key":"scion.dev/pool","operator":"Equal","value":"agents","effect":"NoSchedule"}],
"kubernetes_service_account_mappings":{"sa@example.iam.gserviceaccount.com":"agent-ksa"},
"resources":{"limits":{"memory":"16Gi"}}},
"local":{"type":"docker"}}`

func decodeRuntimesForTest(t *testing.T, doc []byte) map[string]map[string]any {
	t.Helper()
	var m map[string]map[string]any
	if err := json.Unmarshal(doc, &m); err != nil {
		t.Fatalf("decoding runtimes %s: %v", doc, err)
	}
	return m
}

func mergeRowForTest(t *testing.T, stored, body string) (map[string]map[string]any, []string) {
	t.Helper()
	var req ServerConfigUpdateRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	doc, warnings, err := mergeRuntimesRow([]byte(stored), req.Runtimes, sentRuntimesObject([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	return decodeRuntimesForTest(t, doc), warnings
}

func TestMergeRuntimesRow(t *testing.T) {
	t.Run("omitted keys are kept, sent keys replace", func(t *testing.T) {
		got, warnings := mergeRowForTest(t, storedK8sRuntimes,
			`{"runtimes":{"gke":{"type":"kubernetes","namespace":"agents-2"},"local":{"type":"docker"}}}`)
		gke := got["gke"]
		if gke["namespace"] != "agents-2" {
			t.Errorf("namespace = %v, want agents-2", gke["namespace"])
		}
		for _, k := range []string{"gke", "node_selector", "tolerations", "kubernetes_service_account_mappings", "resources"} {
			if _, ok := gke[k]; !ok {
				t.Errorf("stored key %s was dropped: %v", k, gke)
			}
		}
		if !reflect.DeepEqual(gke["resources"], map[string]any{"limits": map[string]any{"memory": "16Gi"}}) {
			t.Errorf("unmodeled resources not kept exactly: %v", gke["resources"])
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "runtimes.gke.resources") {
			t.Errorf("want one warning naming runtimes.gke.resources, got %v", warnings)
		}
	})
	t.Run("null and zero values clear", func(t *testing.T) {
		got, _ := mergeRowForTest(t, storedK8sRuntimes,
			`{"runtimes":{"gke":{"node_selector":null,"namespace":"","gke":false,"tolerations":[]},"local":{}}}`)
		gke := got["gke"]
		for _, k := range []string{"node_selector", "namespace", "gke", "tolerations"} {
			if _, ok := gke[k]; ok {
				t.Errorf("%s should be cleared: %v", k, gke)
			}
		}
		if gke["type"] != "kubernetes" {
			t.Errorf("type should be kept: %v", gke)
		}
	})
	t.Run("an omitted or null entry is deleted", func(t *testing.T) {
		got, _ := mergeRowForTest(t, storedK8sRuntimes, `{"runtimes":{"local":null}}`)
		if len(got) != 0 {
			t.Errorf("want no entries, got %v", got)
		}
	})
	t.Run("a new entry takes the sent keys", func(t *testing.T) {
		got, _ := mergeRowForTest(t, "", `{"runtimes":{"k":{"type":"kubernetes","runtime_class_name":"gvisor"}}}`)
		if !reflect.DeepEqual(got["k"], map[string]any{"type": "kubernetes", "runtime_class_name": "gvisor"}) {
			t.Errorf("got %v", got["k"])
		}
	})
	t.Run("a sent value never replaces an unmodeled stored key", func(t *testing.T) {
		got, _ := mergeRowForTest(t, storedK8sRuntimes,
			`{"runtimes":{"gke":{"resources":{"limits":{"memory":"1Gi"}},"bogus":"x"},"local":{}}}`)
		if !reflect.DeepEqual(got["gke"]["resources"], map[string]any{"limits": map[string]any{"memory": "16Gi"}}) {
			t.Errorf("a sent value must not replace an unmodeled stored key: %v", got["gke"]["resources"])
		}
		if _, ok := got["gke"]["bogus"]; ok {
			t.Errorf("an unmodeled key the row does not hold must not be stored: %v", got["gke"])
		}
	})
	t.Run("key spelling folds as the typed decode does", func(t *testing.T) {
		got, _ := mergeRowForTest(t, storedK8sRuntimes, `{"Runtimes":{"gke":{"Namespace":"x"},"local":{}}}`)
		if got["gke"]["namespace"] != "x" {
			t.Errorf("namespace = %v", got["gke"]["namespace"])
		}
		if _, ok := got["gke"]["Namespace"]; ok {
			t.Errorf("the canonical key must be stored: %v", got["gke"])
		}
	})
	t.Run("a non-canonical stored spelling is kept with a warning", func(t *testing.T) {
		got, warnings := mergeRowForTest(t, `{"k":{"type":"kubernetes","Namespace":"old"}}`,
			`{"runtimes":{"k":{"type":"kubernetes","namespace":"new"}}}`)
		if got["k"]["Namespace"] != "old" || got["k"]["namespace"] != "new" {
			t.Errorf("want Namespace kept and namespace written: %v", got["k"])
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "runtimes.k.Namespace") || !strings.Contains(warnings[0], "rename it to namespace") {
			t.Errorf("warnings = %v", warnings)
		}
	})
	t.Run("stored removed storage overrides are dropped with a warning", func(t *testing.T) {
		got, warnings := mergeRowForTest(t, `{"k":{"type":"kubernetes","home_storage_leaf":"pod"}}`, `{"runtimes":{"k":{"type":"kubernetes"}}}`)
		if _, ok := got["k"]["home_storage_leaf"]; ok {
			t.Errorf("home_storage_leaf should be dropped: %v", got["k"])
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "runtimes.k.home_storage_leaf") {
			t.Errorf("warnings = %v", warnings)
		}
	})
	t.Run("numbers are kept exactly", func(t *testing.T) {
		// Read the written document as text: decoding it into float64 for
		// the check would itself round the number.
		body := `{"runtimes":{"k":{"type":"docker"}}}`
		var req ServerConfigUpdateRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		doc, _, err := mergeRuntimesRow([]byte(`{"k":{"type":"docker","extra":{"n":12345678901234567890}}}`), req.Runtimes, sentRuntimesObject([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(doc), `"n":12345678901234567890`) {
			t.Errorf("doc = %s", doc)
		}
	})
}

// DB mode: a save that leaves out the Kubernetes keys of a stored runtime
// entry keeps them, and the response warns about the unmodeled key.
func TestPutServerConfigDB_RuntimesMergeKeepsUnsentKeys(t *testing.T) {
	srv, fake, ops := newTestDBServer(t)
	ctx := context.Background()
	if _, err := fake.UpsertHubSetting(ctx, "runtimes", json.RawMessage(storedK8sRuntimes), "test", -1, "managed"); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"runtimes":{"gke":{"type":"kubernetes","namespace":"agents","gke":true,"list_all_namespaces":true},"local":{"type":"docker"}}}`), ops)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "runtimes.gke.resources") {
		t.Errorf("response should warn about runtimes.gke.resources: %s", rr.Body.String())
	}
	row, err := fake.GetHubSetting(ctx, "runtimes")
	if err != nil {
		t.Fatal(err)
	}
	gke := decodeRuntimesForTest(t, row.Value)["gke"]
	if gke["list_all_namespaces"] != true {
		t.Errorf("list_all_namespaces should be saved: %v", gke)
	}
	for _, k := range []string{"node_selector", "tolerations", "kubernetes_service_account_mappings", "resources"} {
		if _, ok := gke[k]; !ok {
			t.Errorf("stored key %s was dropped by the save: %s", k, row.Value)
		}
	}
}

// File mode: the same save keeps the keys in settings.yaml.
func TestHandlePutServerConfig_RuntimesMergeKeepsUnsentKeys(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	dir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(storedK8sRuntimes), &stored); err != nil {
		t.Fatal(err)
	}
	data, err := yamlv3.Marshal(map[string]any{"schema_version": "1", "runtimes": stored})
	if err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(settingsPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &Server{}
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"runtimes":{"gke":{"type":"kubernetes","namespace":"agents-2"},"local":{"type":"docker"}}}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "runtimes.gke.resources") {
		t.Errorf("response should warn about runtimes.gke.resources: %s", rr.Body.String())
	}
	out, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Runtimes map[string]map[string]any `yaml:"runtimes"`
	}
	if err := yamlv3.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	gke := got.Runtimes["gke"]
	if gke["namespace"] != "agents-2" {
		t.Errorf("namespace = %v", gke["namespace"])
	}
	for _, k := range []string{"gke", "node_selector", "tolerations", "kubernetes_service_account_mappings", "resources"} {
		if _, ok := gke[k]; !ok {
			t.Errorf("stored key %s was dropped by the save: %s", k, out)
		}
	}
}

// The PUT refuses an unmodeled runtime key in the body, null included, in
// both modes, so the merge never sees one (pinned here because the merge's
// warning relies on it).
func TestPutServerConfig_UnmodeledRuntimeKeyRejected(t *testing.T) {
	for _, body := range []string{
		`{"runtimes":{"gke":{"type":"kubernetes","resources":{"limits":{"memory":"16Gi"}}}}}`,
		`{"runtimes":{"gke":{"type":"kubernetes","resources":null}}}`,
	} {
		srv, _, ops := newTestDBServer(t)
		rr := httptest.NewRecorder()
		srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
		if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "runtimes.gke.resources") {
			t.Errorf("db %s: expected 422 naming runtimes.gke.resources, got %d: %s", body, rr.Code, rr.Body.String())
		}
		frr, _ := fileModePutServerConfig(t, &Server{}, body)
		if frr.Code != http.StatusUnprocessableEntity || !strings.Contains(frr.Body.String(), "runtimes.gke.resources") {
			t.Errorf("file %s: expected 422 naming runtimes.gke.resources, got %d: %s", body, frr.Code, frr.Body.String())
		}
	}
}

// Resource quantities are checked on save in both modes: errors name the
// field and give an example; warnings are returned with a 200.
func TestPutServerConfig_ResourceQuantities(t *testing.T) {
	tests := []struct {
		name, body, want string
		wantErr          bool
	}{
		{"unitless memory below 1Mi", `{"profiles":{"gke":{"runtime":"gke","resources":{"limits":{"memory":"16"}}}}}`, "profiles.gke.resources.limits.memory", true},
		{"default resources", `{"default_resources":{"requests":{"memory":"512Ki"}}}`, "default_resources.requests.memory", true},
		{"not a quantity", `{"profiles":{"gke":{"runtime":"gke","resources":{"requests":{"cpu":"two"}}}}}`, "profiles.gke.resources.requests.cpu", true},
		{"request above limit", `{"profiles":{"gke":{"runtime":"gke","resources":{"requests":{"cpu":"8"},"limits":{"cpu":"7"}}}}}`, "profiles.gke.resources.requests.cpu", true},
		{"harness override", `{"profiles":{"gke":{"runtime":"gke","harness_overrides":{"claude":{"resources":{"disk":"0"}}}}}}`, "profiles.gke.harness_overrides.claude.resources.disk", true},
		{"unitless byte count warns", `{"profiles":{"gke":{"runtime":"gke","resources":{"limits":{"memory":"2147483648"}}}}}`, "2Gi", false},
		{"low memory warns", `{"profiles":{"gke":{"runtime":"gke","resources":{"limits":{"memory":"32Mi"}}}}}`, "below 64Mi", false},
	}
	for _, tt := range tests {
		t.Run("db/"+tt.name, func(t *testing.T) {
			srv, _, ops := newTestDBServer(t)
			rr := httptest.NewRecorder()
			srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", tt.body), ops)
			wantCode := http.StatusOK
			if tt.wantErr {
				wantCode = http.StatusUnprocessableEntity
			}
			if rr.Code != wantCode {
				t.Fatalf("expected %d, got %d: %s", wantCode, rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tt.want) {
				t.Errorf("body should contain %q: %s", tt.want, rr.Body.String())
			}
		})
		t.Run("file/"+tt.name, func(t *testing.T) {
			rr, _ := fileModePutServerConfig(t, &Server{}, tt.body)
			wantCode := http.StatusOK
			if tt.wantErr {
				wantCode = http.StatusBadRequest
			}
			if rr.Code != wantCode {
				t.Fatalf("expected %d, got %d: %s", wantCode, rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tt.want) {
				t.Errorf("body should contain %q: %s", tt.want, rr.Body.String())
			}
		})
	}
}

// runtimesRaceStore writes the runtimes row once, as another replica would,
// between the PUT's read of the row and its write.
type runtimesRaceStore struct {
	*fakeHubSettingStore
	other json.RawMessage
	once  sync.Once
}

func (c *runtimesRaceStore) GetHubSetting(ctx context.Context, section string) (*store.HubSetting, error) {
	row, err := c.fakeHubSettingStore.GetHubSetting(ctx, section)
	if section != "runtimes" || err != nil {
		return row, err
	}
	cp := *row
	c.once.Do(func() {
		_, _ = c.UpsertHubSetting(ctx, section, c.other, "other-replica", -1, "managed")
	})
	return &cp, nil
}

// The runtimes write is a compare-and-swap on the revision the merge read:
// a concurrent write, or a stale expected_revisions pin, yields 409 and the
// stored row stands.
func TestPutServerConfigDB_RuntimesMerge_Conflict409(t *testing.T) {
	const other = `{"gke":{"type":"kubernetes","namespace":"other-replica"}}`
	body := `{"runtimes":{"gke":{"type":"kubernetes","namespace":"mine"}}}`

	t.Run("concurrent write", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		fake := newFakeHubSettingStore()
		fake.seedWithOrigin("runtimes", json.RawMessage(storedK8sRuntimes), "managed")
		race := &runtimesRaceStore{fakeHubSettingStore: fake, other: json.RawMessage(other)}
		ops := NewOperationalSettings(race, emptyKoanf(), emptyKoanf())
		srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
		srv.SetOperationalSettings(ops)

		rr := putServerConfigDB(t, srv, ops, body)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
		}
		row, err := fake.GetHubSetting(context.Background(), "runtimes")
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeRuntimesForTest(t, row.Value)["gke"]["namespace"]; got != "other-replica" {
			t.Errorf("the other writer's row should stand, namespace = %v", got)
		}
	})

	t.Run("stale expected revision", func(t *testing.T) {
		srv, fake, ops := newTestDBServer(t)
		ctx := context.Background()
		first, err := fake.UpsertHubSetting(ctx, "runtimes", json.RawMessage(storedK8sRuntimes), "test", -1, "managed")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fake.UpsertHubSetting(ctx, "runtimes", json.RawMessage(other), "other-replica", -1, "managed"); err != nil {
			t.Fatal(err)
		}
		pinned := `{"expected_revisions":{"runtimes":` + strconv.FormatInt(first.Revision, 10) + `},` + body[1:]
		rr := putServerConfigDB(t, srv, ops, pinned)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
		}
	})
}
