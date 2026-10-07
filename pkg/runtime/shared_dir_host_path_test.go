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

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const testSharedDirProjectID = "0123abcd-0000-4000-8000-000000000001"

func nfsSharedDirSettings(mountRoot string) *config.VersionedSettings {
	return &config.VersionedSettings{
		Server: &config.V1ServerConfig{
			SharedDirStorage: &config.V1SharedDirStorageConfig{
				Backend: "nfs",
				NFS: &config.V1NFSConfig{
					MountRoot: mountRoot,
					Shares:    []config.V1NFSShare{{ID: "share1"}},
				},
			},
		},
	}
}

func TestResolveSharedDirHostPath_Local(t *testing.T) {
	home := t.TempDir()
	want := config.SharedDirHostPath(home, "proj", testSharedDirProjectID, "scratchpad")
	for _, tc := range []struct {
		name string
		gs   *config.VersionedSettings
	}{
		{"nil settings", nil},
		{"no shared_dir_storage", &config.VersionedSettings{}},
		{"explicit local", &config.VersionedSettings{Server: &config.V1ServerConfig{
			SharedDirStorage: &config.V1SharedDirStorageConfig{Backend: "local"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveSharedDirHostPath(tc.gs, home, "proj", testSharedDirProjectID, "scratchpad")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Backend != "local" || got.Path != want {
				t.Fatalf("got %+v, want local %q", got, want)
			}
		})
	}
}

func TestResolveSharedDirHostPath_NFS(t *testing.T) {
	home := t.TempDir()
	mountRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mountRoot, "share1"), 0o755); err != nil {
		t.Fatal(err)
	}
	gs := nfsSharedDirSettings(mountRoot)

	got, err := ResolveSharedDirHostPath(gs, home, "proj", testSharedDirProjectID, "scratchpad")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resolvedRoot, _ := filepath.EvalSymlinks(mountRoot)
	want := filepath.Join(resolvedRoot, "share1", "projects", testSharedDirProjectID, "shared-dirs", "scratchpad")
	if got.Backend != "nfs" || got.Path != want {
		t.Fatalf("got %+v, want nfs %q", got, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("leaf not created: %v", err)
	}
	if _, err := os.Stat(config.SharedDirHostPath(home, "proj", testSharedDirProjectID, "scratchpad")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local layout must not be created for nfs, stat err = %v", err)
	}
}

func TestResolveSharedDirHostPath_PerDirBackend(t *testing.T) {
	home := t.TempDir()
	mountRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mountRoot, "share1"), 0o755); err != nil {
		t.Fatal(err)
	}
	gs := nfsSharedDirSettings(mountRoot)
	gs.Server.SharedDirStorage.Backend = "local"
	gs.ActiveProfile = "default"
	gs.Profiles = map[string]config.V1ProfileConfig{
		"default": {SharedDirStorageBackends: map[string]string{"scratchpad": "nfs"}},
	}

	got, err := ResolveSharedDirHostPath(gs, home, "proj", testSharedDirProjectID, "scratchpad")
	if err != nil || got.Backend != "nfs" {
		t.Fatalf("scratchpad: got %+v, err %v; want nfs", got, err)
	}
	other, err := ResolveSharedDirHostPath(gs, home, "proj", testSharedDirProjectID, "other")
	if err != nil || other.Backend != "local" {
		t.Fatalf("other: got %+v, err %v; want local", other, err)
	}
}

func TestResolveSharedDirHostPath_NFSUnavailable(t *testing.T) {
	home := t.TempDir()

	t.Run("mount missing", func(t *testing.T) {
		mountRoot := filepath.Join(t.TempDir(), "not-mounted")
		_, err := ResolveSharedDirHostPath(nfsSharedDirSettings(mountRoot), home, "proj", testSharedDirProjectID, "scratchpad")
		if !errors.Is(err, ErrSharedDirStorageUnavailable) {
			t.Fatalf("err = %v, want ErrSharedDirStorageUnavailable", err)
		}
		if _, statErr := os.Stat(mountRoot); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("mount root must not be created, stat err = %v", statErr)
		}
	})

	t.Run("incomplete nfs block", func(t *testing.T) {
		gs := nfsSharedDirSettings("")
		_, err := ResolveSharedDirHostPath(gs, home, "proj", testSharedDirProjectID, "scratchpad")
		if !errors.Is(err, ErrSharedDirStorageUnavailable) {
			t.Fatalf("err = %v, want ErrSharedDirStorageUnavailable", err)
		}
	})

	t.Run("invalid project id", func(t *testing.T) {
		mountRoot := t.TempDir()
		_ = os.MkdirAll(filepath.Join(mountRoot, "share1"), 0o755)
		_, err := ResolveSharedDirHostPath(nfsSharedDirSettings(mountRoot), home, "proj", "../victim", "scratchpad")
		if !errors.Is(err, ErrSharedDirStorageUnavailable) {
			t.Fatalf("err = %v, want ErrSharedDirStorageUnavailable", err)
		}
	})
}

func TestResolveSharedDirHostPath_InvalidName(t *testing.T) {
	if _, err := ResolveSharedDirHostPath(nil, t.TempDir(), "proj", testSharedDirProjectID, "../x"); err == nil {
		t.Fatal("expected an error for an invalid shared dir name")
	}
}

// writeGlobalSettings points HOME at a temp dir whose global settings file
// holds content, so LoadSharedDirStorageSettings reads only fake config.
func writeGlobalSettings(t *testing.T, content string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSharedDirStorageSettings(t *testing.T) {
	t.Run("malformed yaml mentioning shared_dir_storage fails closed", func(t *testing.T) {
		writeGlobalSettings(t, "schema_version: \"1\"\nserver:\n  shared_dir_storage: [unclosed\n")
		gs, err := LoadSharedDirStorageSettings()
		if !errors.Is(err, ErrSharedDirStorageUnavailable) {
			t.Fatalf("err = %v, want ErrSharedDirStorageUnavailable", err)
		}
		if gs != nil {
			t.Fatalf("settings = %+v, want nil", gs)
		}
	})

	t.Run("legacy format with a shared_dir_storage block fails closed", func(t *testing.T) {
		// No schema_version: the legacy loader drops the server block.
		writeGlobalSettings(t, `active_profile: local
server:
  shared_dir_storage:
    backend: nfs
    nfs:
      mount_root: /srv
      shares:
        - id: share
`)
		gs, err := LoadSharedDirStorageSettings()
		if !errors.Is(err, ErrSharedDirStorageUnavailable) {
			t.Fatalf("err = %v, want ErrSharedDirStorageUnavailable", err)
		}
		if gs != nil {
			t.Fatalf("settings = %+v, want nil", gs)
		}
	})

	t.Run("malformed yaml without the key means the local layout", func(t *testing.T) {
		writeGlobalSettings(t, "schema_version: \"1\"\nactive_profile: [unclosed\n")
		gs, err := LoadSharedDirStorageSettings()
		if err != nil || gs != nil {
			t.Fatalf("got settings %+v, err %v; want nil, nil", gs, err)
		}
	})

	t.Run("v1 file mentioning the key only in a comment resolves to local", func(t *testing.T) {
		writeGlobalSettings(t, `schema_version: "1"
# server:
#   shared_dir_storage:
#     backend: nfs
`)
		gs, err := LoadSharedDirStorageSettings()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gs == nil {
			t.Fatal("settings = nil, want the loaded v1 settings")
		}
		home := t.TempDir()
		got, err := ResolveSharedDirHostPath(gs, home, "proj", testSharedDirProjectID, "scratchpad")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		want := config.SharedDirHostPath(home, "proj", testSharedDirProjectID, "scratchpad")
		if got.Backend != "local" || got.Path != want {
			t.Fatalf("got %+v, want local %q", got, want)
		}
	})
}

// TestResolveSharedDirHostPath_NFSBackstopRefusesSwappedLeaf replaces the
// leaf with a symlink after the EnsureLeaf walk has finished, which the
// walk itself cannot see, and checks that the fresh re-resolution refuses
// it.
func TestResolveSharedDirHostPath_NFSBackstopRefusesSwappedLeaf(t *testing.T) {
	home := t.TempDir()
	mountRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mountRoot, "share1"), 0o755); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()

	orig := ensureSharedDirLeaf
	t.Cleanup(func() { ensureSharedDirLeaf = orig })
	ensureSharedDirLeaf = func(hostBase, rel string) (int, bool, error) {
		fd, existed, err := orig(hostBase, rel)
		if err != nil {
			return fd, existed, err
		}
		leaf := filepath.Join(hostBase, rel)
		if err := os.Rename(leaf, leaf+".moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, leaf); err != nil {
			t.Fatal(err)
		}
		return fd, existed, nil
	}

	got, err := ResolveSharedDirHostPath(nfsSharedDirSettings(mountRoot), home, "proj", testSharedDirProjectID, "scratchpad")
	if !errors.Is(err, ErrSharedDirStorageUnavailable) {
		t.Fatalf("got %+v, err %v; want ErrSharedDirStorageUnavailable", got, err)
	}
	if !strings.Contains(err.Error(), "resolves through a symlink") {
		t.Fatalf("err = %v, want the backstop refusal", err)
	}
	if got.Path != "" {
		t.Fatalf("path = %q, want empty", got.Path)
	}
}
