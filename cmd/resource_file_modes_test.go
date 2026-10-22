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
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func TestModeOnlyChanges(t *testing.T) {
	local := []hubclient.FileInfo{
		{Path: "bin/now-exec", Hash: "h1", Mode: "0755"},
		{Path: "bin/was-exec", Hash: "h2", Mode: "0644"},
		{Path: "bin/same", Hash: "h3", Mode: "0755"},
		{Path: "content-changed", Hash: "new", Mode: "0755"},
		{Path: "new-file", Hash: "h5", Mode: "0755"},
		{Path: "umask-variant", Hash: "h6", Mode: "0775"},
	}
	remote := []hubclient.DownloadURLInfo{
		{Path: "bin/now-exec", Hash: "h1", Mode: "0644"},
		{Path: "bin/was-exec", Hash: "h2", Mode: "0755"},
		{Path: "bin/same", Hash: "h3", Mode: "0755"},
		{Path: "content-changed", Hash: "old", Mode: "0644"},
		{Path: "umask-variant", Hash: "h6", Mode: "0755"},
	}
	got := modeOnlyChanges(local, remote)
	want := []string{"bin/now-exec", "bin/was-exec"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("modeOnlyChanges = %v, want %v", got, want)
	}

	// A hub that sends no modes: its files count as 0644.
	got = modeOnlyChanges([]hubclient.FileInfo{{Path: "x", Hash: "h", Mode: "0755"}, {Path: "y", Hash: "h", Mode: "0644"}},
		[]hubclient.DownloadURLInfo{{Path: "x", Hash: "h"}, {Path: "y", Hash: "h"}})
	if !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("old hub: modeOnlyChanges = %v, want [x]", got)
	}
}

func TestWriteHarnessConfigFile_Mode(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for p, mode := range map[string]string{"provision.py": "0755", "config.yaml": "0644", "legacy": ""} {
		if err := writeHarnessConfigFile(root, p, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
	}
	for p, want := range map[string]os.FileMode{"provision.py": 0o755, "config.yaml": 0o644, "legacy": 0o644} {
		info, err := os.Stat(filepath.Join(dir, p))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s: mode = %v, want %v", p, info.Mode().Perm(), want)
		}
	}
}
