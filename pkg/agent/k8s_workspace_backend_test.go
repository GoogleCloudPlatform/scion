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

package agent

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// resetK8sWorkspaceBackendWarned forgets that the warning was logged.
func resetK8sWorkspaceBackendWarned() {
	k8sWorkspaceBackendWarned.Store(false)
}

func TestK8sWorkspaceBackendDropped(t *testing.T) {
	tests := []struct {
		runtime string
		backend string
		want    bool
	}{
		{"kubernetes", "gke-shared-volume", true},
		{"kubernetes", "nfs", false},
		{"kubernetes", "local", false},
		{"kubernetes", "", false},
		{"kubernetes", "cloudrun-volume", false},
		{"docker", "gke-shared-volume", false},
		{"podman", "gke-shared-volume", false},
	}
	for _, tt := range tests {
		t.Run(tt.runtime+"/"+tt.backend, func(t *testing.T) {
			if got := k8sWorkspaceBackendDropped(tt.runtime, tt.backend); got != tt.want {
				t.Fatalf("k8sWorkspaceBackendDropped(%q, %q) = %v; want %v", tt.runtime, tt.backend, got, tt.want)
			}
		})
	}
}

func TestWarnK8sWorkspaceBackendDropped_OncePerProcess(t *testing.T) {
	resetK8sWorkspaceBackendWarned()
	t.Cleanup(resetK8sWorkspaceBackendWarned)

	type entry struct {
		msg  string
		args []any
	}
	var (
		mu     sync.Mutex
		logged []entry
	)
	logf := func(msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, entry{msg, args})
	}

	// Silent cases never log and do not consume the once-per-process slot.
	silent := []struct {
		runtime string
		cfg     *config.V1WorkspaceStorageConfig
	}{
		{"kubernetes", nil},
		{"kubernetes", &config.V1WorkspaceStorageConfig{Backend: "nfs"}},
		{"kubernetes", &config.V1WorkspaceStorageConfig{}},
		{"docker", &config.V1WorkspaceStorageConfig{Backend: "gke-shared-volume"}},
	}
	for _, s := range silent {
		if warnK8sWorkspaceBackendDropped(s.runtime, s.cfg, logf) {
			t.Fatalf("warned for %s with %+v", s.runtime, s.cfg)
		}
	}
	if len(logged) != 0 {
		t.Fatalf("logged %v for silent cases", logged)
	}

	// Concurrent starts log exactly once.
	gke := &config.V1WorkspaceStorageConfig{
		Backend:         "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{VolumeName: "test-volume", PVClaimName: "test-claim"},
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			warnK8sWorkspaceBackendDropped("kubernetes", gke, logf)
		}()
	}
	wg.Wait()
	if warnK8sWorkspaceBackendDropped("kubernetes", gke, logf) {
		t.Fatal("warned again after the first warning")
	}
	if len(logged) != 1 {
		t.Fatalf("logged %d warnings; want 1", len(logged))
	}

	got := logged[0]
	for _, want := range []string{"gke-shared-volume", "kubernetes", "emptyDir", "nfs", "server.project_storage.backend", "server.workspace_storage.backend"} {
		if !strings.Contains(got.msg, want) {
			t.Errorf("warning %q does not mention %q", got.msg, want)
		}
	}
	wantArgs := []any{"runtime", "kubernetes", "backend", "gke-shared-volume"}
	if len(got.args) != len(wantArgs) {
		t.Fatalf("warning attrs = %v; want %v", got.args, wantArgs)
	}
	for i := range wantArgs {
		if got.args[i] != wantArgs[i] {
			t.Fatalf("warning attrs = %v; want %v", got.args, wantArgs)
		}
	}
	// The volume and claim names stay out of the log.
	if all := fmt.Sprint(got.msg, got.args); strings.Contains(all, "test-volume") || strings.Contains(all, "test-claim") {
		t.Errorf("warning %q carries a volume or claim name", all)
	}

	// A new process (simulated by the reset) warns again.
	resetK8sWorkspaceBackendWarned()
	if !warnK8sWorkspaceBackendDropped("kubernetes", gke, logf) {
		t.Fatal("did not warn after reset")
	}
}
