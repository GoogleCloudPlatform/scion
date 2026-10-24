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

package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// dirFileReader serves LoadHarnessConfigFromFiles from the files of dir.
func dirFileReader(dir string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, name))
	}
}

// LoadHarnessConfigFromFiles gives LoadHarnessConfigDir's result for the
// same files: the same entry, name and errors.
func TestLoadHarnessConfigFromFiles_MatchesLoadHarnessConfigDir(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		sidecar string
		wantErr string
	}{
		{name: "minimal", config: sidecarTestConfig},
		{name: "named", config: sidecarTestConfig + "name: other-name\n"},
		{name: "with sidecar", config: sidecarTestConfig, sidecar: "support: \"partial\"\nreason: r\n"},
		{name: "invalid sidecar degrades", config: sidecarTestConfig, sidecar: "support: [\n"},
		{name: "name refused by the schema", config: sidecarTestConfig + "name: ../escape\n", wantErr: "invalid config.yaml"},
		{name: "schema violation", config: "image: scion-base:test\n", wantErr: "invalid config.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeSidecarTestDir(t, tt.config, tt.sidecar)
			fromDir, dirErr := LoadHarnessConfigDir(dir)
			fromFiles, filesErr := LoadHarnessConfigFromFiles(filepath.Base(dir), dir, dirFileReader(dir))
			if tt.wantErr != "" {
				if dirErr == nil || filesErr == nil {
					t.Fatalf("errors: dir %v, files %v; want both", dirErr, filesErr)
				}
				if !strings.Contains(filesErr.Error(), tt.wantErr) || dirErr.Error() != filesErr.Error() {
					t.Fatalf("errors differ or miss %q: dir %q, files %q", tt.wantErr, dirErr, filesErr)
				}
				return
			}
			if dirErr != nil || filesErr != nil {
				t.Fatalf("errors: dir %v, files %v", dirErr, filesErr)
			}
			if fromFiles.Name != fromDir.Name {
				t.Errorf("name: files %q, dir %q", fromFiles.Name, fromDir.Name)
			}
			// where is the absolute directory here, as LoadHarnessConfigDir
			// resolves it; LoadHarnessConfigFromFiles returns where as given.
			if fromFiles.Path != fromDir.Path {
				t.Errorf("path: files %q, dir %q", fromFiles.Path, fromDir.Path)
			}
			if !reflect.DeepEqual(fromFiles.Config, fromDir.Config) {
				t.Errorf("config differs:\nfiles %+v\ndir   %+v", fromFiles.Config, fromDir.Config)
			}
		})
	}
}

func TestLoadHarnessConfigFromFiles_MissingConfigFails(t *testing.T) {
	_, err := LoadHarnessConfigFromFiles("x", "x", func(string) ([]byte, error) {
		return nil, fs.ErrNotExist
	})
	if err == nil || !strings.Contains(err.Error(), "failed to read config.yaml") {
		t.Fatalf("err = %v, want a config.yaml read failure", err)
	}
}

func TestLoadHarnessConfigFromFiles_ConfigReadErrorFails(t *testing.T) {
	// A reader that refuses config.yaml (for example verified content that
	// changed) fails the load.
	_, err := LoadHarnessConfigFromFiles("x", "x", func(string) ([]byte, error) {
		return nil, errors.New("changed since it was collected")
	})
	if err == nil || !strings.Contains(err.Error(), "changed since it was collected") {
		t.Fatalf("err = %v, want the reader's error", err)
	}
}

func TestLoadHarnessConfigFromFiles_SidecarReadErrorDegrades(t *testing.T) {
	hc, err := LoadHarnessConfigFromFiles("x", "where", func(name string) ([]byte, error) {
		if name == "config.yaml" {
			return []byte(sidecarTestConfig), nil
		}
		return nil, errors.New("unreadable")
	})
	if err != nil {
		t.Fatal(err)
	}
	got := hc.Config.AgentPluginsCapability()
	if got.Support != api.SupportNo || !strings.Contains(got.Reason, "unreadable") {
		t.Fatalf("agent_plugins = %+v, want support no with the read error", got)
	}
	if hc.Name != "x" || hc.Path != "where" {
		t.Errorf("name %q path %q, want x and where", hc.Name, hc.Path)
	}
}
