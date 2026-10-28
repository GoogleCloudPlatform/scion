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

package runtimebroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// storageRefusalErr is the agent manager's refusal of a start whose
// profile carries a removed storage override, wrapped as a caller might.
func storageRefusalErr(t *testing.T) error {
	t.Helper()
	sd := &config.V1SharedDirStorageConfig{NFS: &config.V1NFSConfig{SubPathRoot: "tree/projects",
		Shares: []config.V1NFSShare{{ID: "a", PVName: "share-a-pv"}}}}
	err := config.RemovedStorageOverridesError(config.GlobalSettingsSource, nil,
		map[string]config.V1ProfileConfig{"p1": {SharedDirStorageBackend: "nfs"}}, sd)
	if err == nil {
		t.Fatal("fixture: expected a refusal")
	}
	return fmt.Errorf("start: %w", err)
}

func assertStorageRefusalResponse(t *testing.T, code int, body string) ErrorResponse {
	t.Helper()
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", code, body)
	}
	var resp ErrorResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("not an ErrorResponse: %v: %s", err, body)
	}
	if resp.Error.Code != ErrCodeStorageMigrationRequired {
		t.Errorf("code = %q, want %q", resp.Error.Code, ErrCodeStorageMigrationRequired)
	}
	for _, want := range []string{"profiles.p1.shared_dir_storage_backend", `server.shared_dir_storage.backend: \"nfs\"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %s does not contain %q", body, want)
		}
	}
	if strings.Contains(body, "share-a-pv") || strings.Contains(body, "tree/projects") {
		t.Errorf("response names the share's claim or subpath root: %s", body)
	}
	return resp
}

// Every synchronous start path answers a storage refusal with 422
// storage_migration_required and the public message (ptone/scion#6371).
func TestStorageMigrationRequired_DispatchPathsAnswer422(t *testing.T) {
	cases := []struct {
		name, path, body string
		startAttempted   bool
	}{
		{"create sync start", "/api/v1/agents", `{"name": "s1", "id": "id-s1", "slug": "s1", "config": {"template": "none"}}`, false},
		{"start", "/api/v1/agents/s2/start", `{"runId": "run-s2"}`, true},
		{"restart", "/api/v1/agents/s3/restart", `{"runId": "run-s3"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := dispatchTestEnv(t, true)
			srv.manager = &unusableManager{at: "start", err: storageRefusalErr(t)}
			code, body := postJSON(t, srv, tc.path, tc.body)
			resp := assertStorageRefusalResponse(t, code, body)
			if tc.startAttempted && resp.Error.Details[api.BrokerErrorDetailStartAttempted] != true {
				t.Errorf("expected %s=true in details, got %v", api.BrokerErrorDetailStartAttempted, resp.Error.Details)
			}
		})
	}
}

// An async launch reports the storage_migration_required code with the
// public message.
func TestClassifyStartError_StorageMigrationRequired(t *testing.T) {
	code, _, msg := classifyStartError(context.Background(), storageRefusalErr(t), "", "")
	if code != ErrCodeStorageMigrationRequired {
		t.Errorf("code = %q, want %q", code, ErrCodeStorageMigrationRequired)
	}
	if !strings.Contains(msg, "profiles.p1.shared_dir_storage_backend") || strings.Contains(msg, "share-a-pv") {
		t.Errorf("message should name the key without the claim, got %q", msg)
	}
}

// /healthz reports storage_overrides and a degraded status while the
// check finds removed overrides, and clears when they are gone.
func TestHealth_StorageOverridesCheck(t *testing.T) {
	old := storageOverridesCacheTTL
	storageOverridesCacheTTL = 0
	t.Cleanup(func() { storageOverridesCacheTTL = old })

	srv, _, _ := dispatchTestEnv(t, true)
	checkErr := errors.New("removed overrides")
	calls := 0
	srv.config.StorageOverridesCheck = func() error { calls++; return checkErr }

	info := srv.GetHealthInfo(context.Background())
	if info.Status != "degraded" || info.Checks[storageOverridesCheck] != storageOverridesCheckDegraded {
		t.Fatalf("want degraded with %s, got %s %v", storageOverridesCheck, info.Status, info.Checks)
	}
	if strings.Contains(info.Checks[storageOverridesCheck], "p1") {
		t.Errorf("the health text must not name profiles: %q", info.Checks[storageOverridesCheck])
	}

	checkErr = nil
	info = srv.GetHealthInfo(context.Background())
	if _, ok := info.Checks[storageOverridesCheck]; ok {
		t.Errorf("check still reported after the overrides are gone: %v", info.Checks)
	}
	if calls != 2 {
		t.Errorf("check called %d times, want 2 with no cache", calls)
	}
}

// The check result is cached for storageOverridesCacheTTL.
func TestHealth_StorageOverridesCheckCached(t *testing.T) {
	srv, _, _ := dispatchTestEnv(t, true)
	calls := 0
	srv.config.StorageOverridesCheck = func() error { calls++; return nil }
	srv.GetHealthInfo(context.Background())
	srv.GetHealthInfo(context.Background())
	if calls != 1 {
		t.Errorf("check called %d times, want 1 within the cache TTL", calls)
	}
}

// createAdmissionSettings is a global settings file whose "nfs-p" profile
// carries a removed storage override and whose "local" profile does not.
const createAdmissionSettings = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: mock
    nfs-p:
        runtime: mock
        shared_dir_storage_backend: nfs
runtimes:
    mock:
        type: mock
`

// A create whose profile carries a removed storage override is refused at
// admission with 422 storage_migration_required, before any preparation:
// no workspace download, no provisioning, no start and so no runtime
// object (ptone/scion#6371). A create of another profile is admitted.
func TestCreateAgent_StorageRefusalBeforeAnyPreparation(t *testing.T) {
	srv, mgr, globalScion := dispatchTestEnv(t, true)
	if err := os.WriteFile(filepath.Join(globalScion, "settings.yaml"), []byte(createAdmissionSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	downloads := 0
	srv.SetWorkspaceDownloader(func(ctx context.Context, bucket, prefix, localPath string) error {
		downloads++
		return nil
	})
	runs := 0
	srv.runtime = &runtime.MockRuntime{
		NameFunc: func() string { return "mock" },
		RunFunc:  func(ctx context.Context, cfg runtime.RunConfig) (string, error) { runs++; return "id", nil },
	}

	for _, provisionOnly := range []bool{false, true} {
		body := fmt.Sprintf(`{"name": "r1", "id": "id-r1", "slug": "r1", "requestId": "req-r1-%t", "provisionOnly": %t,
			"workspaceStoragePath": "workspaces/r1", "workspaceStorageBucket": "bucket",
			"config": {"template": "none", "profile": "nfs-p"}}`, provisionOnly, provisionOnly)
		code, resp := postJSON(t, srv, "/api/v1/agents", body)
		if code != http.StatusUnprocessableEntity || !strings.Contains(resp, ErrCodeStorageMigrationRequired) ||
			!strings.Contains(resp, "profiles.nfs-p.shared_dir_storage_backend") {
			t.Fatalf("provisionOnly=%t: want 422 %s naming the key, got %d: %s", provisionOnly, ErrCodeStorageMigrationRequired, code, resp)
		}
	}
	if mgr.provisionCalled || mgr.startCalled || downloads != 0 || runs != 0 {
		t.Fatalf("a refused create prepared the start: provision=%v start=%v downloads=%d runs=%d",
			mgr.provisionCalled, mgr.startCalled, downloads, runs)
	}
	if _, err := os.Stat(filepath.Join(globalScion, "agents", "r1")); !os.IsNotExist(err) {
		t.Errorf("a refused create wrote agent files: %v", err)
	}

	// The unaffected profile is admitted and provisioned.
	code, resp := postJSON(t, srv, "/api/v1/agents",
		`{"name": "ok1", "id": "id-ok1", "slug": "ok1", "provisionOnly": true, "config": {"template": "none", "profile": "local"}}`)
	if code == http.StatusUnprocessableEntity || strings.Contains(resp, ErrCodeStorageMigrationRequired) {
		t.Fatalf("unaffected profile refused: %d %s", code, resp)
	}
	if !mgr.provisionCalled {
		t.Errorf("the unaffected profile's create was not provisioned (code %d: %s)", code, resp)
	}
}
