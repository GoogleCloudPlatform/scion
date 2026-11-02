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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/unitdrift"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
)

func TestSecretMigrationExecutor_NoGCPBackend(t *testing.T) {
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}

	// Use a local backend (not GCP) — migration should fail.
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")

	executor := NewSecretMigrationExecutor(s, localBackend)

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

func TestRebuildServerExecutor_BuildsThenInstallsThroughSudoInstall(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rebuild-server only runs on linux")
	}

	repoDir := t.TempDir()
	stubDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "commands.log")
	binaryDest := filepath.Join(t.TempDir(), "scion")
	writeRebuildStubs(t, stubDir, logFile)
	t.Setenv("PATH", stubDir+":"+os.Getenv("PATH"))
	installer := rebuildTestInstaller(t, repoDir, binaryDest)
	if err := os.Remove(logFile); err != nil { // drop Detect's rule checks
		t.Fatal(err)
	}

	// The temp repo has no unit template, so the drift check stops before
	// locating the unit. Stub both lookups anyway so the command list
	// never depends on the host's systemctl or on extra git calls.
	executor := NewRebuildServerExecutor(RebuildServerOptions{
		RepoPath:    repoDir,
		ServiceName: "scion-hub",
		Installer:   installer,
		UnitLocator: func(context.Context, string) (unitdrift.UnitInfo, error) {
			return unitdrift.UnitInfo{}, fmt.Errorf("no systemd in tests")
		},
		UnitRevision: func(context.Context, string) (string, error) { return "", nil },
	})

	var buf bytes.Buffer
	if err := executor.Run(context.Background(), &buf, nil); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, buf.String())
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read command log: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(logData)), "\n") {
		if !strings.HasPrefix(l, "exited ") {
			lines = append(lines, l)
		}
	}
	staging := filepath.Join(repoDir, "scion.rebuild")
	// Preflight (rule checks only), checkout, build into a temporary
	// directory, then the starter hub's two sudoers commands.
	wantPrefixes := []string{
		"sudo -n -l install -m 755 " + staging + " " + binaryDest,
		"sudo -n -l systemctl restart --no-block scion-hub",
		"git rev-parse --abbrev-ref HEAD",
		"git fetch origin",
		"git reset --hard origin/main",
		"git rev-parse HEAD",
		"make web",
		"go build -ldflags -X github.com/GoogleCloudPlatform/scion/pkg/version.Commit=" + rebuildStubCommit +
			" -X github.com/GoogleCloudPlatform/scion/pkg/version.BuildTime=",
		"sudo -n install -m 755 " + staging + " " + binaryDest,
		"sudo -n systemctl restart --no-block scion-hub",
	}
	if len(lines) != len(wantPrefixes) {
		t.Fatalf("expected %d commands, got %d:\n%s", len(wantPrefixes), len(lines), strings.Join(lines, "\n"))
	}
	for i, want := range wantPrefixes {
		if !strings.HasPrefix(lines[i], want) {
			t.Errorf("command %d = %q, want prefix %q", i+1, lines[i], want)
		}
	}
	// The build goes to a temporary directory, never straight to the
	// staging path or the installed binary.
	if strings.Contains(lines[7], "-o "+staging) || strings.Contains(lines[7], "-o "+binaryDest) {
		t.Errorf("go build must target a temporary path, got: %s", lines[7])
	}
	if !strings.HasSuffix(lines[7], " ./cmd/scion") {
		t.Errorf("go build target = %s", lines[7])
	}
	got, err := os.ReadFile(staging)
	if err != nil || !strings.Contains(string(got), rebuildStubCommit) {
		t.Fatalf("staged binary = %q, %v", got, err)
	}
}

func TestRebuildServerExecutor_NonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("this test verifies non-linux rejection")
	}

	executor := NewRebuildServerExecutor(RebuildServerOptions{
		RepoPath:    "/tmp/fake",
		ServiceName: "test",
	})

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

	executor := NewRebuildServerExecutor(RebuildServerOptions{
		ServiceName: "test",
	})

	var buf bytes.Buffer
	err := executor.Run(context.Background(), &buf, nil)
	if err == nil {
		t.Fatal("expected error for missing repo path, got nil")
	}
	if !strings.Contains(err.Error(), "no repository path") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRebuildContainerBinariesExecutor_MissingRepoPath(t *testing.T) {
	executor := &RebuildContainerBinariesExecutor{}

	var buf bytes.Buffer
	err := executor.Run(context.Background(), &buf, nil)
	if err == nil {
		t.Fatal("expected error for missing repo path, got nil")
	}
	if !strings.Contains(err.Error(), "no repository path") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRebuildContainerBinariesExecutor_RunsMake(t *testing.T) {
	repoDir := t.TempDir()
	stubDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "commands.log")

	// Create a stub make script that records its invocation.
	script := fmt.Sprintf("#!/bin/sh\necho 'make' \"$@\" >> '%s'\n", logFile)
	stubPath := filepath.Join(stubDir, "make")
	if err := os.WriteFile(stubPath, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write stub make: %v", err)
	}

	t.Setenv("PATH", stubDir+":"+os.Getenv("PATH"))
	t.Setenv("SCION_DEV_BINARIES", "/tmp/test-dev-binaries")

	executor := NewRebuildContainerBinariesExecutor(repoDir)

	var buf bytes.Buffer
	err := executor.Run(context.Background(), &buf, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read command log: %v", err)
	}

	if !strings.Contains(string(logData), "make container-binaries") {
		t.Errorf("expected 'make container-binaries' in log, got: %s", string(logData))
	}

	output := buf.String()
	if !strings.Contains(output, "SCION_DEV_BINARIES=/tmp/test-dev-binaries") {
		t.Errorf("expected output to contain SCION_DEV_BINARIES value, got: %s", output)
	}
}
