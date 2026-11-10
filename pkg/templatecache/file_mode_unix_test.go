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

//go:build unix

package templatecache

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

func TestPutWithModes_WritesExactModesDespiteUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	cache, err := New(t.TempDir(), DefaultMaxSize)
	if err != nil {
		t.Fatal(err)
	}
	path, err := cache.PutWithModes("k", map[string][]byte{
		"bin/run":   []byte("#!/bin/sh\n"),
		"README.md": []byte("docs"),
		"unnamed":   []byte("no mode given"),
	}, map[string]os.FileMode{"bin/run": transfer.ModeExecutable, "README.md": transfer.ModeRegular})
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{"bin/run": 0o755, "README.md": 0o644, "unnamed": 0o644} {
		if got := statMode(t, filepath.Join(path, filepath.FromSlash(p))); got != want {
			t.Errorf("%s: mode = %v, want %v", p, got, want)
		}
	}
}
