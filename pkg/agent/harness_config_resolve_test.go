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
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestResolveHarnessConfigDir_PrefersContextPath verifies that a Hub-hydrated
// harness-config path on the context is used directly, bypassing the on-disk
// FindHarnessConfigDir search. This is the §7.3 step-4 behavior that lets a
// broker without the harness-config locally use the copy fetched from the Hub.
func TestResolveHarnessConfigDir_PrefersContextPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("harness: claude\nimage: scion-claude:latest\nuser: scion\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx := api.ContextWithHarnessConfigPath(context.Background(), dir)

	// Use a bogus name and project path: if the bypass were not honored,
	// FindHarnessConfigDir would fail to find this config.
	hcDir, err := resolveHarnessConfigDir(ctx, "does-not-exist", "/no/such/project")
	if err != nil {
		t.Fatalf("expected hydrated path to load, got error: %v", err)
	}
	if hcDir.Path != dir {
		t.Errorf("expected hcDir.Path %q, got %q", dir, hcDir.Path)
	}
	if hcDir.Config.Harness != "claude" {
		t.Errorf("expected harness 'claude', got %q", hcDir.Config.Harness)
	}
	if hcDir.Config.Image != "scion-claude:latest" {
		t.Errorf("expected image 'scion-claude:latest', got %q", hcDir.Config.Image)
	}
}

// TestResolveHarnessConfigDir_FallsBackToDiskSearch verifies that without a
// context path, resolution falls back to the on-disk FindHarnessConfigDir search
// (which errors for an unknown config), preserving the existing behavior.
func TestResolveHarnessConfigDir_FallsBackToDiskSearch(t *testing.T) {
	_, err := resolveHarnessConfigDir(context.Background(), "definitely-not-a-real-config", t.TempDir())
	if err == nil {
		t.Fatal("expected error when no context path is set and config is not on disk")
	}
}
