// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtimebroker

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// These tests pin how createAgent's admission steps compose: the
// shared-layout project root and the removed-storage-override refusal both
// run before the gather-env profile pre-check (ptone/scion#4888).

// sharedRootProfile is the active profile named only by the shared project
// root's own settings. The hub's stored profiles define it with a runtime
// they lack, so the pre-check reports it by name only when it reads the
// settings at the Layout's project root.
const sharedRootProfile = "shared-root"

// On a shared Layout a gather-env create reaches the profile pre-check with
// req.ProjectPath already set to the Layout's project root, so the
// pre-check resolves the profile from that root's settings: it reports the
// shared root's active profile, before any env check and before Provision.
// Any other settings source (the broker's working directory, a broker-local
// project directory, the global settings) names active profile "local",
// which the stored profiles lack, and gives a different refusal.
func TestCreateAgent_GatherEnvSharedLayoutPreCheckReadsProjectRoot(t *testing.T) {
	f := newSharedRootFixture(t, true)
	// No ForceRuntime: the pre-check must resolve the profile from settings.
	f.srv.config.ForceRuntime = ""
	if err := os.WriteFile(filepath.Join(f.scionDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nactive_profile: "+sharedRootProfile+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installOverlayProfiles(t, map[string]config.V1ProfileConfig{
		lifecycleK8sProfile: {Runtime: "kubernetes"},
		sharedRootProfile:   {Runtime: "gke"},
	})

	w := lifecyclePost(t, f.srv, "/api/v1/agents", map[string]any{
		"name":        "gather-shared",
		"slug":        "gather-shared",
		"id":          "agent-uuid-gather-shared",
		"projectId":   sharedRootProjectID,
		"projectSlug": "web",
		"gatherEnv":   true,
		"config":      map[string]any{"template": "claude", "sharedWorkspace": true},
	})
	assertDispatchRefused(t, w, "runtime profile's runtime cannot be resolved",
		`runtime "gke" not found for profile "`+sharedRootProfile+`"`)
	assertProfileUnresolvedDetails(t, w, sharedRootProfile)
	if f.mgr.provisionCalled {
		t.Error("Provision must not run after the pre-check refusal")
	}
}

// A create whose stored profile both carries a removed storage override and
// names a runtime the stored settings lack is refused by the storage check
// (422 storage_migration_required, ptone/scion#6371), which runs before the
// gather-env profile pre-check; the pre-check's 503 is not reached. The
// control case shows the same runtime fault alone gives the pre-check's 503.
func TestCreateAgent_StorageRefusalWinsOverUnresolvableProfileRuntime(t *testing.T) {
	const (
		bothFaults   = "nfs-dangling"
		runtimeFault = "dangling-only"
	)
	for _, tc := range []struct {
		name, profile string
		want422       bool
	}{
		{name: "storage override and missing runtime", profile: bothFaults, want422: true},
		{name: "missing runtime only (control)", profile: runtimeFault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			installOverlayProfiles(t, map[string]config.V1ProfileConfig{
				lifecycleK8sProfile: {Runtime: "kubernetes"},
				bothFaults:          {Runtime: "gke", SharedDirStorageBackend: "nfs"},
				runtimeFault:        {Runtime: "gke"},
			})
			w := lifecyclePost(t, f.srv, "/api/v1/agents", map[string]any{
				"name":        "precedence-agent",
				"slug":        "precedence-agent",
				"id":          "agent-uuid-precedence",
				"projectPath": f.projectPath,
				"gatherEnv":   true,
				"config":      map[string]any{"template": "claude", "profile": tc.profile},
			})
			if !tc.want422 {
				assertDispatchRefused(t, w, `runtime "gke" not found for profile "`+runtimeFault+`"`)
				assertProfileUnresolvedDetails(t, w, runtimeFault)
				f.assertNoRuntimeUsed(t)
				return
			}
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d (the storage refusal wins): %s", w.Code, http.StatusUnprocessableEntity, w.Body.String())
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode body: %v: %s", err, w.Body.String())
			}
			if resp.Error.Code != ErrCodeStorageMigrationRequired {
				t.Errorf("code = %q, want %q", resp.Error.Code, ErrCodeStorageMigrationRequired)
			}
			if !strings.Contains(w.Body.String(), "profiles."+bothFaults+".shared_dir_storage_backend") {
				t.Errorf("body does not name the removed override key: %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), ErrCodeRuntimeUnavailable) || strings.Contains(w.Body.String(), `runtime "gke" not found`) {
				t.Errorf("the pre-check's runtime refusal was reached: %s", w.Body.String())
			}
			f.assertNoRuntimeUsed(t)
		})
	}
}
