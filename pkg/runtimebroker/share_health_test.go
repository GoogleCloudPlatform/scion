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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// captureDefaultLog routes slog's default logger to a recordingHandler for
// the rest of the test.
func captureDefaultLog(t *testing.T) *recordingHandler {
	t.Helper()
	prev := slog.Default()
	h := &recordingHandler{}
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// A non-shared Layout has no project_storage check.
func TestShareHealth_NonSharedLayoutNoCheck(t *testing.T) {
	f := newShareVisibleFixture(t, "kubernetes", "")
	f.srv.config.StorageLayout = projectfs.Layout{}
	f.srv.checkShareHealth(context.Background())
	if v, ok := f.srv.GetHealthInfo(context.Background()).Checks[shareHealthCheck]; ok {
		t.Fatalf("checks[%s] = %q on a non-shared Layout, want no check", shareHealthCheck, v)
	}
}

// On a shared Layout the check reports the share's visibility: a share
// that is not visible degrades the status (readiness stays ready), is
// logged at ERROR once however often it is re-checked, and names no path;
// a share made visible again clears it on the next check, with no restart.
func TestShareHealth_SharedLayoutTransitions(t *testing.T) {
	logs := captureDefaultLog(t)
	f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")

	if _, ok := f.srv.GetHealthInfo(context.Background()).Checks[shareHealthCheck]; ok {
		t.Fatal("the check must not be reported before it has run")
	}

	f.srv.checkShareHealth(context.Background())
	info := f.srv.GetHealthInfo(context.Background())
	if got := info.Checks[shareHealthCheck]; got != "healthy" {
		t.Fatalf("checks[%s] with the share visible = %q, want healthy", shareHealthCheck, got)
	}
	if info.Status != "healthy" {
		t.Fatalf("status with the share visible = %q, want healthy (checks %v)", info.Status, info.Checks)
	}

	// The mount point without the subpath root is not the share.
	if err := os.RemoveAll(filepath.Join(f.hostBase, "projects")); err != nil {
		t.Fatal(err)
	}
	f.srv.checkShareHealth(context.Background())
	f.srv.checkShareHealth(context.Background())
	info = f.srv.GetHealthInfo(context.Background())
	got := info.Checks[shareHealthCheck]
	if !strings.HasPrefix(got, shareHealthNotVisible+" (") || !strings.Contains(got, f.srv.shareUnavailableText()) {
		t.Fatalf("checks[%s] with the share not visible = %q", shareHealthCheck, got)
	}
	assertNoPathValues(t, f, got)
	if info.Status != "degraded" {
		t.Fatalf("status with the share not visible = %q, want degraded", info.Status)
	}
	if n := logs.count(slog.LevelError); n != 1 {
		t.Fatalf("ERROR records after two failed checks = %d, want 1", n)
	}
	if hb := sendHealthHeartbeat(t, f.srv); hb.Health == nil || hb.Health.Checks[shareHealthCheck] != "unhealthy" || hb.Health.Status != "degraded" {
		t.Fatalf("heartbeat health = %+v, want project_storage unhealthy and status degraded", hb.Health)
	}
	w := httptest.NewRecorder()
	f.srv.handleReadyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/readyz with the share not visible = %d, want 200 (still ready)", w.Code)
	}

	if err := os.MkdirAll(filepath.Join(f.hostBase, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.srv.checkShareHealth(context.Background())
	info = f.srv.GetHealthInfo(context.Background())
	if got := info.Checks[shareHealthCheck]; got != "healthy" || info.Status != "healthy" {
		t.Fatalf("after the share is visible again: checks[%s] = %q, status %q; want healthy", shareHealthCheck, got, info.Status)
	}
	if n := logs.count(slog.LevelError); n != 1 {
		t.Fatalf("ERROR records after recovery = %d, want still 1", n)
	}
}

// A check cut short by its context records nothing.
func TestShareHealth_CancelledCheckRecordsNothing(t *testing.T) {
	f := newShareVisibleFixture(t, "kubernetes", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.srv.checkShareHealth(ctx)
	if _, ok := f.srv.GetHealthInfo(context.Background()).Checks[shareHealthCheck]; ok {
		t.Fatal("a cancelled check must not be recorded")
	}
}

// The loop checks at start and re-checks on its interval until stopped.
func TestShareHealth_LoopRechecks(t *testing.T) {
	prev := shareHealthInterval
	shareHealthInterval = 10 * time.Millisecond
	t.Cleanup(func() { shareHealthInterval = prev })

	f := newShareVisibleFixture(t, "kubernetes", "")
	f.srv.startShareHealthLoop(context.Background())
	t.Cleanup(f.srv.stopShareHealthLoop)

	waitShareHealth := func(want func(string) bool, what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if v, ok := f.srv.GetHealthInfo(context.Background()).Checks[shareHealthCheck]; ok && want(v) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for the share check to report %s", what)
	}
	waitShareHealth(func(v string) bool { return v == "healthy" }, "healthy")
	if err := os.RemoveAll(filepath.Join(f.hostBase, "projects")); err != nil {
		t.Fatal(err)
	}
	waitShareHealth(func(v string) bool { return strings.HasPrefix(v, shareHealthNotVisible) }, "not visible")
}

// On a broker that mounts the share, the first check waits for the NFS
// reconciler's first pass, which is what mounts it.
func TestShareHealth_MountingBrokerWaitsForFirstPass(t *testing.T) {
	f := newShareVisibleFixture(t, "docker", "10.0.0.2:/scion-workspaces")
	if !f.srv.nfsMountReconciler.MountsShares() {
		t.Skip("the fixture's reconciler does not mount shares in this environment")
	}
	f.srv.startShareHealthLoop(context.Background())
	t.Cleanup(f.srv.stopShareHealthLoop)
	time.Sleep(50 * time.Millisecond)
	if v, ok := f.srv.GetHealthInfo(context.Background()).Checks[shareHealthCheck]; ok {
		t.Fatalf("checks[%s] = %q before the reconciler's first pass, want no check yet", shareHealthCheck, v)
	}
	close(f.srv.nfsStartupReconcileDone)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v := f.srv.GetHealthInfo(context.Background()).Checks[shareHealthCheck]; v == "healthy" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the share check after the first pass")
}
