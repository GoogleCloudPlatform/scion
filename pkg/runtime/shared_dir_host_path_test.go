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
