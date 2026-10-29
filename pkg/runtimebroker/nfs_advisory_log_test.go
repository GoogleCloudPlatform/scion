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

package runtimebroker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// A failed share check is logged at the level an operator should act on:
// on a verify-only broker (it never mounts the share) whose share directory
// is visible, the failure is advisory (a warning); a share directory that
// is not visible, or a failure on a broker that may mount, stays an error.
func TestNFSShareFailureLogLevel(t *testing.T) {
	cases := []struct {
		name          string
		verifyOnly    bool
		dirExists     bool
		subPathExists bool
		inTable       bool
		wantLevel     slog.Level
		advisory      bool
	}{
		{name: "verify-only, directory visible, source differs", verifyOnly: true, dirExists: true, subPathExists: true, inTable: true, wantLevel: slog.LevelWarn, advisory: true},
		// The same rule as the start refusal and the health check: a mount
		// point without the subpath root is not the share.
		{name: "verify-only, mount point visible without the subpath root", verifyOnly: true, dirExists: true, inTable: true, wantLevel: slog.LevelError},
		{name: "verify-only, directory absent", verifyOnly: true, wantLevel: slog.LevelError},
		{name: "check-only broker that may mount, directory visible, source differs", dirExists: true, subPathExists: true, inTable: true, wantLevel: slog.LevelError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mountRoot := t.TempDir()
			cfg := &config.V1NFSConfig{
				MountRoot: mountRoot,
				Shares:    []config.V1NFSShare{{ID: "ws1", Server: "10.0.0.2", Export: "/scion-workspaces"}},
			}
			target := filepath.Join(mountRoot, "ws1")
			if tc.dirExists {
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.subPathExists {
				// The default subpath root (cfg.SubPathRoot is empty).
				if err := os.MkdirAll(filepath.Join(target, "projects"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			mc := newSyncMountChecker()
			if tc.inTable {
				// Differs from the configuration in form only.
				mc.setMounted(target, "10.0.0.2:/scion-workspaces/")
			}
			h := &recordingHandler{}
			r := NewNFSMountReconciler(cfg, mc, slog.New(h))
			if tc.verifyOnly {
				r.SetVerifyOnly("the broker's default runtime is kubernetes, so the broker does not mount it")
			}
			_ = r.Reconcile(context.Background())
			if r.IsHealthy() {
				t.Fatal("expected the share check to fail in this setup")
			}
			other := slog.LevelError
			if tc.wantLevel == slog.LevelError {
				other = slog.LevelWarn
			}
			if got := h.count(tc.wantLevel); got != 1 {
				t.Errorf("%v records = %d, want 1", tc.wantLevel, got)
			}
			if got := h.count(other); got != 0 {
				t.Errorf("%v records = %d, want 0", other, got)
			}
			if got := r.UnhealthyAdvisoryOnly(); got != tc.advisory {
				t.Errorf("UnhealthyAdvisoryOnly = %v, want %v", got, tc.advisory)
			}
		})
	}
}
