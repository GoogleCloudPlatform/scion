//go:build !no_sqlite

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

package hub

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
)

func TestSecretMigrationExecutor_NoGCPBackend(t *testing.T) {
	s, err := sqlite.New(":memory:")
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// Use a local backend (not GCP) — migration should fail.
	localBackend := secret.NewLocalBackend(s, "test-hub-id")

	executor := &SecretMigrationExecutor{
		store:         s,
		secretBackend: localBackend,
	}

	var buf bytes.Buffer
	err = executor.Run(context.Background(), &buf, map[string]string{})
	if err == nil {
		t.Fatal("expected error for non-GCP backend, got nil")
	}
	if !strings.Contains(err.Error(), "GCP Secret Manager") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSecretMigrationExecutor_NoSecrets(t *testing.T) {
	// This test verifies the executor handles zero secrets gracefully.
	// We can't easily mock a GCPBackend without a real GCP client,
	// but we can test the local-backend error path above.
	// A full integration test would require GCP SM mock infrastructure.
}

func TestRebuildServerExecutor_BuildsToStagingThenInstalls(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rebuild-server only runs on linux")
	}

	repoDir := t.TempDir()
	binDir := t.TempDir()
	stubDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "commands.log")
	binaryDest := filepath.Join(binDir, "scion")

	// Create stub scripts that record their invocation to a shared log file.
	for _, cmd := range []string{"git", "make", "go", "sudo", "systemctl"} {
		script := fmt.Sprintf("#!/bin/sh\necho '%s' \"$@\" >> '%s'\n", cmd, logFile)
		stubPath := filepath.Join(stubDir, cmd)
		if err := os.WriteFile(stubPath, []byte(script), 0o755); err != nil {
			t.Fatalf("failed to write stub %s: %v", cmd, err)
		}
	}

	t.Setenv("PATH", stubDir+":"+os.Getenv("PATH"))

	executor := &RebuildServerExecutor{
		repoPath:    repoDir,
		binaryDest:  binaryDest,
		serviceName: "test-scion",
	}

	var buf bytes.Buffer
	err := executor.Run(context.Background(), &buf, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read command log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(logData)), "\n")

	// Expect 5 commands: git pull, make web, go build, sudo install, systemctl restart.
	if len(lines) != 5 {
		t.Fatalf("expected 5 commands, got %d:\n%s", len(lines), string(logData))
	}

	// Verify go build targets the staging path inside the repo dir, not the final binary.
	goLine := lines[2]
	stagingBinary := filepath.Join(repoDir, "scion.rebuild")
	if !strings.Contains(goLine, "-o "+stagingBinary) {
		t.Errorf("go build should target staging path %q, got: %s", stagingBinary, goLine)
	}
	if strings.Contains(goLine, "-o "+binaryDest+" ") {
		t.Errorf("go build must NOT target the final binary directly, got: %s", goLine)
	}

	// Verify sudo install moves the staging binary to the final destination.
	sudoLine := lines[3]
	if !strings.Contains(sudoLine, "sudo install") {
		t.Errorf("expected sudo install command, got: %s", sudoLine)
	}
	if !strings.Contains(sudoLine, stagingBinary) || !strings.Contains(sudoLine, binaryDest) {
		t.Errorf("sudo install should reference staging %q and dest %q, got: %s", stagingBinary, binaryDest, sudoLine)
	}
}

func TestRebuildServerExecutor_NonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("this test verifies non-linux rejection")
	}

	executor := &RebuildServerExecutor{
		repoPath:    "/tmp/fake",
		binaryDest:  "/tmp/fake/scion",
		serviceName: "test",
	}

	var buf bytes.Buffer
	err := executor.Run(context.Background(), &buf, nil)
	if err == nil {
		t.Fatal("expected error on non-linux, got nil")
	}
	if !strings.Contains(err.Error(), "only supported on Linux") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRebuildServerExecutor_MissingRepoPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rebuild-server only runs on linux")
	}

	executor := &RebuildServerExecutor{
		binaryDest:  "/tmp/fake/scion",
		serviceName: "test",
	}

	var buf bytes.Buffer
	err := executor.Run(context.Background(), &buf, nil)
	if err == nil {
		t.Fatal("expected error for missing repo path, got nil")
	}
	if !strings.Contains(err.Error(), "no repository path") {
		t.Errorf("unexpected error: %v", err)
	}
}
