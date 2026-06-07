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

package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- ClonePerAgent rejection ---

func TestProvision_RejectsClonePerAgent(t *testing.T) {
	err := ProvisionShared(ProvisionInput{
		ProjectID: "proj-1",
		Mode:      store.SharingModeClonePerAgent,
		Resolved: ResolvedWorkspace{
			HostPath: "/some/path",
		},
	})
	if err == nil {
		t.Fatal("expected error for ClonePerAgent on NFS backend")
	}
	if !strings.Contains(err.Error(), "ClonePerAgent") {
		t.Errorf("error should mention ClonePerAgent, got: %v", err)
	}
}

// --- Missing required fields ---

func TestProvision_MissingHostPath(t *testing.T) {
	err := ProvisionShared(ProvisionInput{
		ProjectID: "proj-1",
		Mode:      store.SharingModeSharedPlain,
		Resolved:  ResolvedWorkspace{},
	})
	if err == nil {
		t.Fatal("expected error for empty HostPath")
	}
}

func TestProvision_MissingProjectID(t *testing.T) {
	err := ProvisionShared(ProvisionInput{
		Mode: store.SharingModeSharedPlain,
		Resolved: ResolvedWorkspace{
			HostPath: "/some/path",
		},
	})
	if err == nil {
		t.Fatal("expected error for empty ProjectID")
	}
}

// --- sanitizeBranchName ---

func TestSanitizeBranchName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "simple"},
		{"with spaces", "with-spaces"},
		{"with/slash", "with-slash"},
		{"with..dots", "with-dots"},
		{"with~tilde", "with-tilde"},
		{".leading-dot", "leading-dot"},
		{"-leading-dash", "leading-dash"},
		{"trailing-.", "trailing"},
		{"", "agent"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeBranchName(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeBranchName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- writeSentinel ---

func TestWriteSentinel_Atomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ProvisionSentinelFile)

	if err := writeSentinel(path); err != nil {
		t.Fatalf("writeSentinel: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if !strings.Contains(string(data), "provisioned_at=") {
		t.Errorf("sentinel content unexpected: %s", string(data))
	}

	// Overwrite should also work (idempotent).
	if err := writeSentinel(path); err != nil {
		t.Fatalf("writeSentinel overwrite: %v", err)
	}
}

// --- acquireProvisionLock context cancellation ---

// alwaysLoseLocker is an AdvisoryLocker where TryAdvisoryLockObject always
// returns acquired=false (another node holds the lock).
type alwaysLoseLocker struct{}

func (l *alwaysLoseLocker) TryAdvisoryLock(_ context.Context, _ store.AdvisoryLockKey) (bool, func() error, error) {
	return false, func() error { return nil }, nil
}

func (l *alwaysLoseLocker) TryAdvisoryLockObject(_ context.Context, _ store.AdvisoryLockKey, _ int32) (bool, func() error, error) {
	return false, func() error { return nil }, nil
}

func TestAcquireProvisionLock_ContextCancellation(t *testing.T) {
	locker := &alwaysLoseLocker{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	in := ProvisionInput{
		ProjectID: "proj-cancel-test",
		Locker:    locker,
	}

	start := time.Now()
	_, err := acquireProvisionLock(ctx, in)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "context cancelled")
	assert.Less(t, elapsed, 2*time.Second, "should return promptly on context cancellation, not wait for all retries")
}
