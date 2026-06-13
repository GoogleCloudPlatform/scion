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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

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

func TestClassifyPath_ManagedLegacyGroves(t *testing.T) {
	// The legacy "groves" path should also be detected as managed
	base := t.TempDir()
	managedRoot := filepath.Join(base, "projects")
	legacyRoot := filepath.Join(base, "groves")
	if err := os.MkdirAll(legacyRoot, 0755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(legacyRoot, "old-project")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}

	pc, err := ClassifyPath(context.Background(), nil, sub, managedRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pc.IsManaged {
		t.Error("expected IsManaged=true for path under legacy groves root")
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
	s, err := newTestStore(":memory:")
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
	s, err := newTestStore(":memory:")
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
