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

//go:build !no_sqlite

package hub

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// noopGlobalLayoutReporter discards every config.MigrateLegacyGlobalLayout
// report; these tests only care about the resulting filesystem state.
type noopGlobalLayoutReporter struct{}

func (noopGlobalLayoutReporter) Migrated(old, new string, tracked bool)      {}
func (noopGlobalLayoutReporter) Conflict(old, new, detail string)            {}
func (noopGlobalLayoutReporter) Skipped(old, reason, manual string)          {}
func (noopGlobalLayoutReporter) EnvIgnored(name, replacement string)         {}
func (noopGlobalLayoutReporter) PrecedenceChanged(path, value, other string) {}

func TestClassifyPath_NonExistent(t *testing.T) {
	pc, err := ClassifyPath(context.Background(), nil, "/nonexistent/path/abcxyz123456", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pc.Exists {
		t.Error("expected Exists=false for non-existent path")
	}
}

func TestClassifyPath_ExistingDir(t *testing.T) {
	dir := t.TempDir()
	pc, err := ClassifyPath(context.Background(), nil, dir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pc.Exists {
		t.Error("expected Exists=true")
	}
	if !pc.IsDir {
		t.Error("expected IsDir=true")
	}
	if pc.IsGit {
		t.Error("expected IsGit=false for dir without .git")
	}
}

func TestClassifyPath_GitDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	pc, err := ClassifyPath(context.Background(), nil, dir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pc.IsGit {
		t.Error("expected IsGit=true for dir with .git")
	}
}

func TestClassifyPath_ManagedPath(t *testing.T) {
	managedRoot := t.TempDir()
	sub := filepath.Join(managedRoot, "myproject")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}

	pc, err := ClassifyPath(context.Background(), nil, sub, managedRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pc.IsManaged {
		t.Error("expected IsManaged=true for path under managedRoot")
	}
}

// TestClassifyPath_SymlinkedManagedRoot guards against a managedRoot that is
// itself behind a symlink (a symlinked home directory, common on macOS): the
// prefix check must resolve managedRoot the same way it already resolves
// path, not just Clean it, or a real, managed path would be misclassified as
// unmanaged.
func TestClassifyPath_SymlinkedManagedRoot(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real-root")
	sub := filepath.Join(realRoot, "myproject")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	symlinkedRoot := filepath.Join(base, "symlinked-root")
	if err := os.Symlink(realRoot, symlinkedRoot); err != nil {
		t.Fatal(err)
	}

	// path is given through the symlinked root (as a caller resolving a
	// project under a symlinked home directory would see it), and so is
	// managedRoot itself.
	pathThroughSymlink := filepath.Join(symlinkedRoot, "myproject")
	pc, err := ClassifyPath(context.Background(), nil, pathThroughSymlink, symlinkedRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pc.IsManaged {
		t.Error("expected IsManaged=true when both path and managedRoot are given through the same symlink")
	}
}

func TestClassifyPath_ManagedThroughMigratedSymlink(t *testing.T) {
	// A path recorded before config.MigrateLegacyGlobalLayout moved its
	// project out of the legacy root still resolves through the per-entry
	// symlink the migrator leaves behind, so it is still detected as managed
	// with no separate legacy-path check.
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "groves", "old-project"), 0755); err != nil {
		t.Fatal(err)
	}
	config.MigrateLegacyGlobalLayout(base, noopGlobalLayoutReporter{})

	managedRoot := filepath.Join(base, "projects")
	if _, err := os.Stat(filepath.Join(managedRoot, "old-project")); err != nil {
		t.Fatalf("migration did not create the canonical project dir: %v", err)
	}
	legacyPath := filepath.Join(base, "groves", "old-project")
	if info, err := os.Lstat(legacyPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected %s to be a symlink after migration, got %v, err %v", legacyPath, info, err)
	}

	pc, err := ClassifyPath(context.Background(), nil, legacyPath, managedRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pc.IsManaged {
		t.Error("expected IsManaged=true for a path resolving through the migrated legacy symlink")
	}
}

func TestClassifyPath_UnmigratedLegacyDirNotManaged(t *testing.T) {
	// A bare directory literally named "groves" that is not a symlink to the
	// canonical root (i.e. never migrated) is not treated as managed: there
	// is no legacy-name fallback any more, only symlink resolution.
	base := t.TempDir()
	managedRoot := filepath.Join(base, "projects")
	if err := os.MkdirAll(managedRoot, 0755); err != nil {
		t.Fatal(err)
	}
	legacyRoot := filepath.Join(base, "groves")
	sub := filepath.Join(legacyRoot, "old-project")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}

	pc, err := ClassifyPath(context.Background(), nil, sub, managedRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pc.IsManaged {
		t.Error("expected IsManaged=false for an unmigrated legacy directory")
	}
}

func TestClassifyPath_NotManaged(t *testing.T) {
	managedRoot := t.TempDir()
	otherDir := t.TempDir()

	pc, err := ClassifyPath(context.Background(), nil, otherDir, managedRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pc.IsManaged {
		t.Error("expected IsManaged=false for path outside managedRoot")
	}
}

func TestClassifyPath_AlreadyLinked(t *testing.T) {
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	dir := t.TempDir()

	broker := &store.RuntimeBroker{
		ID:     uuid.NewString(),
		Name:   "test-broker",
		Slug:   "test-broker",
		Status: "online",
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}
	proj := &store.Project{
		ID:      uuid.NewString(),
		Slug:    "linked-test",
		Name:    "Linked Test",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  proj.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LocalPath:  dir,
		Status:     "online",
	}); err != nil {
		t.Fatal(err)
	}

	pc, err := ClassifyPath(ctx, s, dir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pc.AlreadyLinked {
		t.Error("expected AlreadyLinked=true for path matching a provider")
	}
}

func TestClassifyPath_NotLinked(t *testing.T) {
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	linkedDir := t.TempDir()
	otherDir := t.TempDir()

	broker := &store.RuntimeBroker{
		ID:     uuid.NewString(),
		Name:   "test-broker-nl",
		Slug:   "test-broker-nl",
		Status: "online",
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatal(err)
	}
	proj := &store.Project{
		ID:      uuid.NewString(),
		Slug:    "notlinked-test",
		Name:    "Not Linked Test",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  proj.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LocalPath:  linkedDir,
		Status:     "online",
	}); err != nil {
		t.Fatal(err)
	}

	pc, err := ClassifyPath(ctx, s, otherDir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pc.AlreadyLinked {
		t.Error("expected AlreadyLinked=false for unlinked path")
	}
}
