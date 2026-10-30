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

package cmd

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// TestResolveHubProjectStorage_RefusesInvalidFieldNamingIt: a hub start
// with an invalid server.project_storage field in the settings file is
// refused, and the error names the field.
func TestResolveHubProjectStorage_RefusesInvalidFieldNamingIt(t *testing.T) {
	block := func(id, server, export string) string {
		return "  project_storage:\n    backend: nfs\n    nfs:\n      mount_root: /mnt/nfs\n      shares:\n" +
			"        - id: \"" + id + "\"\n          server: \"" + server + "\"\n          export: \"" + export + "\"\n"
	}
	for _, tc := range []struct {
		name, extra, wantField string
	}{
		{"share id traversal", block("../x", "10.0.0.2", "/x"), "server.project_storage.nfs.shares[0].id"},
		{"share id nested", block("a/b", "10.0.0.2", "/x"), "server.project_storage.nfs.shares[0].id"},
		{"server leading dash", block("ws1", "-oexec", "/x"), "server.project_storage.nfs.shares[0].server"},
		{"export leading dash", block("ws1", "10.0.0.2", "-oexec"), "server.project_storage.nfs.shares[0].export"},
		{"mount option leading dash", "  project_storage:\n    backend: nfs\n    nfs:\n      mount_root: /mnt/nfs\n      mount_options: \"-oexec\"\n" +
			"      shares:\n        - id: ws1\n          server: 10.0.0.2\n          export: /x\n", "server.project_storage.nfs.mount_options"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			dir := t.TempDir()
			settings := "schema_version: \"1\"\nserver:\n  mode: hosted\n" + tc.extra
			if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(settings), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadGlobalConfig(dir)
			if err != nil {
				t.Fatalf("LoadGlobalConfig: %v", err)
			}
			if cfg.ProjectStorage == nil {
				t.Fatal("project_storage was not loaded; the test would pass vacuously")
			}
			err = resolveHubProjectStorage(cfg)
			if err == nil {
				t.Fatal("resolveHubProjectStorage accepted an invalid block")
			}
			if !strings.Contains(err.Error(), "invalid project storage settings") || !strings.Contains(err.Error(), tc.wantField) {
				t.Fatalf("error = %q, want it to name %s", err, tc.wantField)
			}
			if cfg.EffectiveProjectStorage != nil {
				t.Error("an invalid block was stored as the effective block")
			}
		})
	}
}

// TestBrokerStorageLayout_InvalidBlockNoLocalFallback: a broker whose
// block is invalid gets no Layout (not the local one) and a storage error
// naming the field; its registration descriptor is marked invalid and
// carries no placement.
func TestBrokerStorageLayout_InvalidBlockNoLocalFallback(t *testing.T) {
	vs := &config.VersionedSettings{Server: &config.V1ServerConfig{ProjectStorage: &config.V1ProjectStorageConfig{
		Backend: "nfs", Homes: "nfs",
		NFS: &config.V1NFSConfig{MountRoot: "/mnt/scion-nfs", Shares: []config.V1NFSShare{{ID: "../x", Server: "10.0.0.2", Export: "/vol-a"}}},
	}}}
	layout, _, storageErr := brokerStorageLayout(vs)
	if storageErr == nil || !strings.Contains(storageErr.Error(), "server.project_storage.nfs.shares[0].id") {
		t.Fatalf("storageErr = %v, want one naming shares[0].id", storageErr)
	}
	if layout != (projectfs.Layout{}) {
		t.Errorf("layout = %+v, want the zero Layout (no local fallback)", layout)
	}
	if !strings.Contains(brokerStorageUnavailableWarning(storageErr), "refuses agent starts") {
		t.Errorf("warning %q does not say agent starts are refused", brokerStorageUnavailableWarning(storageErr))
	}

	if cfg, warn := brokerNFSConfig(vs); cfg != nil || !strings.HasPrefix(warn, "NFS mount checks disabled:") {
		t.Errorf("brokerNFSConfig = %v, %q; want nil and a warning", cfg, warn)
	}

	desc := brokerRegistrationWorkspaceStorage(vs)
	want := api.BrokerWorkspaceStorage{Backend: "nfs", Invalid: true}
	if desc == nil || *desc != want {
		t.Errorf("registration descriptor = %+v, want %+v", desc, want)
	}

	// A valid block still builds its Layout with no storage error.
	vs.Server.ProjectStorage.NFS.Shares[0].ID = "a"
	layout, _, storageErr = brokerStorageLayout(vs)
	if storageErr != nil || layout.Backend != projectfs.BackendNFS {
		t.Errorf("valid block: layout %+v, err %v", layout, storageErr)
	}
}

// TestBrokerStartup_WiresStorageError pins the wiring: both broker entry
// points pass brokerStorageLayout's error to the Runtime Broker server
// configuration, so an invalid block refuses agent starts instead of
// falling back to the local layout.
func TestBrokerStartup_WiresStorageError(t *testing.T) {
	for file, want := range map[string]*regexp.Regexp{
		"server_foreground.go": regexp.MustCompile(`StorageError:\s+brokerStorageErr,`),
		"server_flat_host.go":  regexp.MustCompile(`StorageError:\s+sh\.storageError,`),
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !want.Match(src) {
			t.Errorf("%s does not set StorageError (%s)", file, want)
		}
		if !regexp.MustCompile(`brokerLayout, layoutWarnings, brokerStorageErr = brokerStorageLayout\(globalVS\)`).Match(src) {
			t.Errorf("%s does not take the storage error from brokerStorageLayout", file)
		}
	}
}

// TestProjectStorageValidator_SingleEntryPoint: the project storage field
// rules have one entry point, config.ValidateProjectStorage, and the
// broker's former copy of the share checks is gone.
func TestProjectStorageValidator_SingleEntryPoint(t *testing.T) {
	defs := 0
	removed := regexp.MustCompile(`\bvalidateBrokerNFS\b`)
	def := regexp.MustCompile(`(?m)^func ValidateProjectStorage\(`)
	for _, root := range []string{".", filepath.Join("..", "pkg")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if removed.Match(src) {
				t.Errorf("%s still refers to validateBrokerNFS", path)
			}
			defs += len(def.FindAll(src, -1))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if defs != 1 {
		t.Errorf("found %d definitions of ValidateProjectStorage, want 1", defs)
	}
}
