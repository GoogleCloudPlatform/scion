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
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Repeated and concurrent health probes of a hung workspace mount share one
// stat, so the mount costs at most one stuck goroutine (and OS thread) no
// matter how often readiness is probed. Once the stat returns, the next probe
// starts a fresh one. See ptone/scion#3085.
func TestCheckWorkspaceStorageHealth_DedupesInFlightStat(t *testing.T) {
	srv, _ := testServer(t)

	mountRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share1"), 0755))
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: mountRoot,
			Shares:    []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/scion"}},
		},
	}
	mountPath := filepath.Join(mountRoot, "share1")
	key := workspaceHealthProbeKey{path: mountPath}

	var stats, doneChecks, inFlightAtDone atomic.Int32
	release := make(chan struct{})
	prevStat, prevTimeout := workspaceHealthStat, workspaceHealthTimeout
	prevBeforeDone := workspaceHealthProbeBeforeDone
	workspaceHealthStat = func(p string) (os.FileInfo, error) {
		stats.Add(1)
		<-release
		return os.Stat(p)
	}
	// Runs in the stat goroutine just before done is closed: the entry must
	// already be gone, so a waiter woken by done never finds it.
	workspaceHealthProbeBeforeDone = func(k workspaceHealthProbeKey) {
		doneChecks.Add(1)
		if _, inFlight := workspaceHealthProbesInFlight.Load(k); inFlight {
			inFlightAtDone.Add(1)
		}
	}
	workspaceHealthTimeout = 30 * time.Millisecond
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		require.Eventually(t, func() bool {
			_, inFlight := workspaceHealthProbesInFlight.Load(key)
			return !inFlight
		}, 5*time.Second, 5*time.Millisecond)
		workspaceHealthStat, workspaceHealthTimeout = prevStat, prevTimeout
		workspaceHealthProbeBeforeDone = prevBeforeDone
	})

	const timedOut = "unhealthy: mount check timed out"
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			checks := make(map[string]string)
			srv.checkWorkspaceStorageHealth(checks)
			assert.Equal(t, timedOut, checks["workspace_storage"])
		}()
	}
	wg.Wait()
	for i := 0; i < 3; i++ {
		checks := make(map[string]string)
		srv.checkWorkspaceStorageHealth(checks)
		assert.Equal(t, timedOut, checks["workspace_storage"])
	}
	assert.Equal(t, int32(1), stats.Load(), "a hung mount must have only one stat in flight")

	// Unblock the stuck stat. It removes itself from the in-flight map before
	// closing done, so once done fires the entry is already gone and the next
	// probe starts a fresh stat, with no polling needed.
	v, ok := workspaceHealthProbesInFlight.Load(key)
	require.True(t, ok, "the hung stat must be in flight")
	call := v.(*workspaceHealthProbeCall)
	close(release)
	released = true
	select {
	case <-call.done:
	case <-time.After(5 * time.Second):
		t.Fatal("stat did not finish after release")
	}
	assert.Equal(t, int32(1), doneChecks.Load())
	assert.Equal(t, int32(0), inFlightAtDone.Load(), "the in-flight entry must be removed before done is closed")
	_, inFlight := workspaceHealthProbesInFlight.Load(key)
	assert.False(t, inFlight, "the in-flight entry must be gone once done fires")

	// The last probe expects a real stat to finish, so give it the production
	// timeout rather than the shortened one; it only matters if the stat hangs.
	workspaceHealthTimeout = prevTimeout
	checks := make(map[string]string)
	srv.checkWorkspaceStorageHealth(checks)
	assert.Equal(t, "healthy", checks["workspace_storage"])
	assert.Equal(t, int32(2), stats.Load(), "after the stat returns, the next probe stats again")
}
