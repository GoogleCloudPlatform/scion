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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/imagecheck"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The loopback system registry and runtime endpoints write Layer-1 keys.
// On a hub with OperationalSettings (any DB driver, SQLite included) the DB
// row takes precedence over settings.yaml, so these writes must land in the
// DB sections, not the file (ptone/scion#3060).

func TestSystemRegistry_DBBackedWritesEndpointsSection(t *testing.T) {
	settingsPath := tempSettingsHome(t)
	srv, st, ops := newSQLiteOpsServer(t, nil, nil)
	if _, err := ops.Update(t.Context(), "endpoints",
		json.RawMessage(`{"public_url":"https://hub.example.com","image_registry":"old.example.com"}`),
		"admin@example.com", -1, "managed"); err != nil {
		t.Fatalf("seed endpoints: %v", err)
	}

	rr := httptest.NewRecorder()
	req := adminRequest(http.MethodPut, "/api/v1/system/registry", `{"image_registry":"new.example.com"}`)
	req.RemoteAddr = "127.0.0.1:1234"
	srv.handleSystemRegistry(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("registry PUT: %d %s", rr.Code, rr.Body.String())
	}

	rec, doc := hubSettingDocMap(t, st, "endpoints")
	if doc["image_registry"] != "new.example.com" {
		t.Errorf("endpoints.image_registry = %v, want new.example.com", doc["image_registry"])
	}
	if doc["public_url"] != "https://hub.example.com" {
		t.Errorf("endpoints.public_url = %v, want it carried forward", doc["public_url"])
	}
	if rec.Origin != "managed" {
		t.Errorf("endpoints origin = %q, want managed", rec.Origin)
	}
	if got := ops.Snapshot().ImageRegistry; got != "new.example.com" {
		t.Errorf("snapshot image_registry = %q, want new.example.com", got)
	}
	if got := srv.config.MaintenanceConfig.ImageRegistry; got != "new.example.com" {
		t.Errorf("applied image_registry = %q, want new.example.com", got)
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "image_registry") {
		t.Errorf("settings.yaml should not be written on a DB-backed hub:\n%s", data)
	}
}

func TestSystemRuntime_DBBackedWritesProfilesSection(t *testing.T) {
	settingsPath := tempSettingsHome(t)
	active := activeProfileName()
	srv, st, _ := newSQLiteOpsServer(t, nil, map[string]string{
		"profiles": `{"` + active + `":{"runtime":"docker","default_template":"keep-me"},"other":{"runtime":"podman"}}`,
	})
	srv.imageChecker = imagecheck.NewChecker()

	rr := httptest.NewRecorder()
	req := adminRequest(http.MethodPut, "/api/v1/system/runtime", `{"runtime":"container"}`)
	req.RemoteAddr = "127.0.0.1:1234"
	srv.handleSystemRuntime(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("runtime PUT: %d %s", rr.Code, rr.Body.String())
	}

	rec, doc := hubSettingDocMap(t, st, "profiles")
	activeDoc, _ := doc[active].(map[string]interface{})
	if activeDoc["runtime"] != "container" {
		t.Errorf("profiles.%s.runtime = %v, want container", active, activeDoc["runtime"])
	}
	if activeDoc["default_template"] != "keep-me" {
		t.Errorf("profiles.%s.default_template = %v, want it kept", active, activeDoc["default_template"])
	}
	otherDoc, _ := doc["other"].(map[string]interface{})
	if otherDoc["runtime"] != "podman" {
		t.Errorf("profiles.other.runtime = %v, want podman kept", otherDoc["runtime"])
	}
	if rec.Origin != "managed" {
		t.Errorf("profiles origin = %q, want managed", rec.Origin)
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "container") {
		t.Errorf("settings.yaml should not be written on a DB-backed hub:\n%s", data)
	}

	// GET reports the DB value.
	rr = httptest.NewRecorder()
	req = adminRequest(http.MethodGet, "/api/v1/system/runtime", "")
	req.RemoteAddr = "127.0.0.1:1234"
	srv.handleSystemRuntime(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("runtime GET: %d %s", rr.Code, rr.Body.String())
	}
	var resp systemRuntimeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Configured != "container" {
		t.Errorf("GET configured = %q, want container", resp.Configured)
	}
}

func TestSystemRuntime_DBBackedNoRowStartsFromEffectiveProfiles(t *testing.T) {
	tempSettingsHome(t)
	active := activeProfileName()
	bootstrap := newFileKoanf(t, map[string]interface{}{
		"profiles.other.runtime": "podman",
	})
	srv, st, _ := newSQLiteOpsServer(t, bootstrap, nil)
	srv.imageChecker = imagecheck.NewChecker()

	rr := httptest.NewRecorder()
	req := adminRequest(http.MethodPut, "/api/v1/system/runtime", `{"runtime":"docker"}`)
	req.RemoteAddr = "127.0.0.1:1234"
	srv.handleSystemRuntime(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("runtime PUT: %d %s", rr.Code, rr.Body.String())
	}

	_, doc := hubSettingDocMap(t, st, "profiles")
	activeDoc, _ := doc[active].(map[string]interface{})
	if activeDoc["runtime"] != "docker" {
		t.Errorf("profiles.%s.runtime = %v, want docker", active, activeDoc["runtime"])
	}
	otherDoc, _ := doc["other"].(map[string]interface{})
	if otherDoc["runtime"] != "podman" {
		t.Errorf("profiles.other.runtime = %v, want the bootstrap value carried into the new row", otherDoc["runtime"])
	}
}

// sectionRaceStore simulates another writer updating one section between
// the handler's read of it and its CAS write. With noRow, the first read
// reports the row as missing and the other writer creates it.
type sectionRaceStore struct {
	store.HubSettingStore
	section string
	doc     string
	noRow   bool
	once    sync.Once
}

func (c *sectionRaceStore) GetHubSetting(ctx context.Context, section string) (*store.HubSetting, error) {
	if section != c.section {
		return c.HubSettingStore.GetHubSetting(ctx, section)
	}
	row, err := c.HubSettingStore.GetHubSetting(ctx, section)
	raced := false
	c.once.Do(func() {
		raced = true
		if _, uerr := c.HubSettingStore.UpsertHubSetting(ctx, section,
			json.RawMessage(c.doc), "other@example.com", -1, "managed"); uerr != nil {
			panic(uerr)
		}
	})
	if raced && c.noRow {
		return nil, store.ErrNotFound
	}
	return row, err
}

func newRacingOpsServer(t *testing.T, seed map[string]string, race *sectionRaceStore) (*Server, store.Store) {
	t.Helper()
	tempSettingsHome(t)
	srv, st, ops := newSQLiteOpsServer(t, nil, seed)
	srv.imageChecker = imagecheck.NewChecker()
	race.HubSettingStore = ops.store
	ops.store = race
	return srv, st
}

func putSystem(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := adminRequest(http.MethodPut, path, body)
	req.RemoteAddr = "127.0.0.1:1234"
	if strings.HasSuffix(path, "/registry") {
		srv.handleSystemRegistry(rr, req)
	} else {
		srv.handleSystemRuntime(rr, req)
	}
	return rr
}

// The registry write is CAS-guarded on the endpoints revision it read, so a
// concurrent endpoints write yields 409 instead of a lost update; with no
// row the write is create-only, so a concurrent create also yields 409.
func TestSystemRegistry_DBBackedConcurrentWrite409(t *testing.T) {
	for _, noRow := range []bool{false, true} {
		t.Run(fmt.Sprintf("noRow=%v", noRow), func(t *testing.T) {
			seed := map[string]string{"endpoints": `{"image_registry":"old.example.com"}`}
			if noRow {
				seed = nil
			}
			race := &sectionRaceStore{section: "endpoints", doc: `{"image_registry":"other.example.com"}`, noRow: noRow}
			srv, st := newRacingOpsServer(t, seed, race)

			rr := putSystem(t, srv, "/api/v1/system/registry", `{"image_registry":"new.example.com"}`)
			if rr.Code != http.StatusConflict {
				t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
			}
			if _, doc := hubSettingDocMap(t, st, "endpoints"); doc["image_registry"] != "other.example.com" {
				t.Errorf("the concurrent writer's row must stand, got %v", doc)
			}
		})
	}
}

// Same CAS guarantees for the runtime write to the profiles section.
func TestSystemRuntime_DBBackedConcurrentWrite409(t *testing.T) {
	for _, noRow := range []bool{false, true} {
		t.Run(fmt.Sprintf("noRow=%v", noRow), func(t *testing.T) {
			seed := map[string]string{"profiles": `{"other":{"runtime":"docker"}}`}
			if noRow {
				seed = nil
			}
			race := &sectionRaceStore{section: "profiles", doc: `{"other":{"runtime":"podman"}}`, noRow: noRow}
			srv, st := newRacingOpsServer(t, seed, race)

			rr := putSystem(t, srv, "/api/v1/system/runtime", `{"runtime":"container"}`)
			if rr.Code != http.StatusConflict {
				t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
			}
			_, doc := hubSettingDocMap(t, st, "profiles")
			other, _ := doc["other"].(map[string]interface{})
			if len(doc) != 1 || other["runtime"] != "podman" {
				t.Errorf("the concurrent writer's row must stand, got %v", doc)
			}
		})
	}
}

// A profiles document that fails schema validation (here an invalid entry
// carried forward from the current row) is a client error, not a 500.
func TestSystemRuntime_DBBackedValidationFailure400(t *testing.T) {
	tempSettingsHome(t)
	srv, _, _ := newSQLiteOpsServer(t, nil, map[string]string{
		"profiles": `{"bad":{"runtime":"docker","harness_overrides":{"claude":{"image_pull_policy":"Sometimes"}}}}`,
	})
	srv.imageChecker = imagecheck.NewChecker()

	rr := putSystem(t, srv, "/api/v1/system/runtime", `{"runtime":"container"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}
