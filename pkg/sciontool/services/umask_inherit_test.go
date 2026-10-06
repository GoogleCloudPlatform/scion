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

package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
)

// A service child inherits the umask init applies for nfs shared-dir
// groups (ptone/scion#3155).
func TestManager_ServiceInheritsSharedDirUmask(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	t.Setenv(suppgroups.EnvVar, "4242")
	t.Cleanup(suppgroups.SetGetgroupsForTest(func() ([]int, error) { return []int{4242}, nil }))
	if applied, _, _ := suppgroups.ApplySharedDirUmask(); !applied {
		t.Fatal("ApplySharedDirUmask did not apply")
	}

	mgr := New(5 * time.Second)
	specs := []api.ServiceSpec{{Name: "umask-printer", Command: []string{"sh", "-c", "umask"}}}
	if err := mgr.Start(context.Background(), specs, 0, 0, "", false); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	time.Sleep(1 * time.Second)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = mgr.Shutdown(shutdownCtx)

	data, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".scion", "services", "logs", "umask-printer.stdout.log"))
	if err != nil {
		t.Fatalf("read stdout log: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "0002" {
		t.Errorf("service umask = %q, want 0002", got)
	}
}
