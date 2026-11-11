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

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newWorkaroundTestRuntime creates a CloudRunSandboxRuntime wired to a mock
// shell-script binary. The deleteTimeout is set short so tests complete
// quickly.
func newWorkaroundTestRuntime(t *testing.T, bin string) *CloudRunSandboxRuntime {
	t.Helper()
	stateFile := filepath.Join(t.TempDir(), "state.json")
	return &CloudRunSandboxRuntime{
		bin:          bin,
		state:        newSandboxStateStore(stateFile),
		rootDir:      t.TempDir(),
		watchCancels: make(map[string]context.CancelFunc),
	}
}

// writeMockBin writes a shell script as a mock sandbox binary and returns
// the path.
func writeMockBin(t *testing.T, script string) string {
	t.Helper()
	tmpDir := t.TempDir()
	mockBin := filepath.Join(tmpDir, "sandbox")
	if err := os.WriteFile(mockBin, []byte("#!/bin/sh\n"+script+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return mockBin
}

// stopWatchersOnCleanup stops, when the test ends, every exit watcher on
// statePath and waits up to 10 s for each to exit, so none writes into a
// t.TempDir while its RemoveAll runs (ptone/scion#6453). Call it after the
// TempDirs the watchers write to: t.Cleanup runs last-in, first-out.
//
// It cancels the watchers of rts and every watcher registered process-wide
// on statePath, whichever instance started it, then waits until no
// registration on statePath is left. That is a join: startWatcher's
// goroutine releases its registration (the deferred releaseSandboxWatch)
// only after watchSandbox has returned, so after its `sandbox wait` has
// exited and its state write is done.
//
// Limit: Delete (cancelSandboxWatch) and a same-name re-Run drop a
// registration while its watcher may still run: a dropped watcher whose
// `wait` has already returned still writes the state file, with no
// registration held. A test that does either must keep that watcher's
// `wait` blocked until it is cancelled.
func stopWatchersOnCleanup(t *testing.T, statePath string, rts ...*CloudRunSandboxRuntime) {
	t.Helper()
	t.Cleanup(func() {
		for _, rt := range rts {
			rt.watchMu.Lock()
			for name, cancel := range rt.watchCancels {
				cancel()
				delete(rt.watchCancels, name)
			}
			rt.watchMu.Unlock()
		}
		sandboxWatchersMu.Lock()
		for k, reg := range sandboxWatchers {
			if k.path == statePath && reg.cancel != nil {
				reg.cancel()
			}
		}
		sandboxWatchersMu.Unlock()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			held := false
			sandboxWatchersMu.Lock()
			for k := range sandboxWatchers {
				held = held || k.path == statePath
			}
			sandboxWatchersMu.Unlock()
			if !held {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Errorf("watchers on %s still registered 10 s after cancel", statePath)
	})
}
