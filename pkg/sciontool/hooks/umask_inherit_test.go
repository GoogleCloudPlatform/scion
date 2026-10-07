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

//go:build linux

package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
)

// A lifecycle hook child inherits the umask init applies for nfs
// shared-dir groups (ptone/scion#3155).
func TestRunPreStart_HookInheritsSharedDirUmask(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	t.Setenv(suppgroups.EnvVar, "4242")
	t.Cleanup(suppgroups.SetGetgroupsForTest(func() ([]int, error) { return []int{4242}, nil }))
	if applied, _, _ := suppgroups.ApplySharedDirUmask(); !applied {
		t.Fatal("ApplySharedDirUmask did not apply")
	}

	dir := t.TempDir()
	marker := filepath.Join(dir, "umask")
	mustWriteExecutableScript(t, filepath.Join(dir, "pre-start.d", "30-umask"), "#!/bin/sh\numask > "+marker+"\n")
	m := &LifecycleManager{HooksDirs: []string{dir}, Handlers: map[string][]Handler{}}
	if err := m.RunPreStart(); err != nil {
		t.Fatalf("RunPreStart: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if s := strings.TrimSpace(string(got)); s != "0002" {
		t.Errorf("hook umask = %q, want 0002", s)
	}
}
