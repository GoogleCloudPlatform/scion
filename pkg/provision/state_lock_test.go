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
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// newWorkspaceTree makes <root>/<pid>/{workspace,provision} and returns the
// workspace path.
func newWorkspaceTree(t *testing.T) string {
	t.Helper()
	project := filepath.Join(t.TempDir(), "projects", "pid")
	ws := filepath.Join(project, "workspace")
	for _, d := range []string{ws, filepath.Join(project, ProvisionStateDirName)} {
		if err := os.MkdirAll(d, 0o2775); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

func TestWorkspaceProvisionLockDirs(t *testing.T) {
	ws := "/mnt/nfs/share/projects/pid/workspace"
	got := WorkspaceProvisionLockDirs(ws)
	want := []string{"/mnt/nfs/share/projects/pid", "/mnt/nfs/share/projects/pid/" + ProvisionStateDirName}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestWithWorkspaceProvisionLocks(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "projects", "pid", "workspace")
	// The lock directories must exist; nothing is created here.
	if err := WithWorkspaceProvisionLocks(context.Background(), ws, func(context.Context) error { return nil }); err == nil {
		t.Fatal("want an error for missing lock directories")
	}
	if _, err := os.Lstat(filepath.Dir(ws)); !os.IsNotExist(err) {
		t.Fatalf("a lock directory was created: %v", err)
	}
	if err := WithWorkspaceProvisionLocks(context.Background(), "rel/ws", func(context.Context) error { return nil }); err == nil {
		t.Fatal("want an error for a relative workspace path")
	}

	ws = newWorkspaceTree(t)
	ran := 0
	if err := WithWorkspaceProvisionLocks(context.Background(), ws, func(ctx context.Context) error {
		ran++
		if ctx.Err() != nil {
			t.Fatalf("lock context already done: %v", ctx.Err())
		}
		return nil
	}); err != nil || ran != 1 {
		t.Fatalf("err=%v ran=%d", err, ran)
	}
	// Released: a second section can take them, and fn's error is returned.
	sentinel := errors.New("boom")
	if err := WithWorkspaceProvisionLocks(context.Background(), ws, func(context.Context) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the section's error", err)
	}
}

// While the locks are held, neither provisioner shape can take its lock,
// using ProvisionShared's own derivation (acquireProvisionLock): the
// hub-side Cloud Run provisioner (no SentinelDir: it locks the project
// root) and the Kubernetes init container (SentinelDir = state dir,
// LegacyDir = workspace).
func TestWithWorkspaceProvisionLocks_ExcludesEveryProvisioner(t *testing.T) {
	ws := newWorkspaceTree(t)
	stateDir := ProjectStateDir(ws)
	shapes := map[string]ProvisionInput{
		"cloudrun-hub": {Resolved: ResolvedWorkspace{HostPath: ws}},
		"k8s-init":     {Resolved: ResolvedWorkspace{HostPath: ws}, SentinelDir: stateDir, LegacyDir: ws},
	}
	err := WithWorkspaceProvisionLocks(context.Background(), ws, func(context.Context) error {
		for name, in := range shapes {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			// acquireProvisionLock waits up to its budget; the context
			// bounds the test.
			h, err := acquireProvisionLock(ctx, in, resolveSentinelDir(in))
			cancel()
			if err == nil {
				_ = h.release()
				return errors.New(name + ": acquired its provisioning lock while the migration held the locks")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// After release both shapes acquire.
	for name, in := range shapes {
		h, err := acquireProvisionLock(context.Background(), in, resolveSentinelDir(in))
		if err != nil {
			t.Fatalf("%s: acquire after release: %v", name, err)
		}
		if err := h.release(); err != nil {
			t.Fatal(err)
		}
	}
}

// A panic inside the section still releases the locks: the panic reaches
// the caller, and the next section takes the locks.
func TestWithWorkspaceProvisionLocks_ReleasesOnPanic(t *testing.T) {
	withFastFileLockTiming(t)
	origRetries := fileLockRetries
	fileLockRetries = 5 // about 100 ms, so a leaked lock fails fast
	t.Cleanup(func() { fileLockRetries = origRetries })
	ws := newWorkspaceTree(t)

	func() {
		defer func() {
			if r := recover(); r != "section panic" {
				t.Fatalf("recovered %v, want the section's panic", r)
			}
		}()
		_ = WithWorkspaceProvisionLocks(context.Background(), ws, func(context.Context) error {
			panic("section panic")
		})
	}()

	if err := WithWorkspaceProvisionLocks(context.Background(), ws, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("locks not released after a panic: %v", err)
	}
}
