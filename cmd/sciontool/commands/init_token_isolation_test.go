//go:build unix

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

package commands

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/supervisor"
)

func newIsolatedResetHarness(t *testing.T, mode hub.TokenStoreMode, alsoAccept ...string) (*resetHarness, string) {
	t.Helper()
	home := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))
	t.Cleanup(hub.SetRuntimeTokenStoreForTest())

	var runtimePath string
	if mode == hub.TokenStoreRuntimeFile {
		runtimePath = filepath.Join(t.TempDir(), "run-scion", hub.TokenFile)
	}
	if err := hub.ConfigureIsolatedTokenStore(mode, runtimePath); err != nil {
		t.Fatalf("ConfigureIsolatedTokenStore: %v", err)
	}

	now := time.Now().Truncate(time.Second)
	h := &resetHarness{
		t:      t,
		now:    now,
		oldTok: testCredential(t, "old", now.Add(-time.Hour)),
		newTok: testCredential(t, "new", now),
	}
	if err := hub.WriteTokenFile(h.oldTok, 0, 0); err != nil {
		t.Fatalf("WriteTokenFile(old): %v", err)
	}
	accept := append([]string{h.newTok}, alsoAccept...)
	h.hub = newCredHub(t, accept...)
	h.client = hub.NewClientWithConfig(h.hub.srv.URL, h.oldTok, "agent-1")

	refreshCtx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = h.client.StartTokenRefresh(refreshCtx, &hub.TokenRefreshConfig{
		RefreshAt: now.Add(time.Hour),
		ChownUID:  0,
		ChownGID:  0,
	})
	t.Cleanup(func() {
		if h.cancel != nil {
			h.cancel()
			if h.done != nil {
				<-h.done
			}
		}
	})

	sh := handlers.NewStatusHandler()
	sh.StatusPath = filepath.Join(home, "agent-info.json")
	h.r = &authResetter{
		hubClient:          h.client,
		tokenRefreshCancel: &h.cancel,
		tokenRefreshDone:   &h.done,
		statusHandler:      sh,
		targetUID:          1000,
		targetGID:          1000,
		isolated:           true,
		now:                func() time.Time { return h.now },
	}
	return h, runtimePath
}

func writeHandoffTokenForTest(t *testing.T, content string) string {
	t.Helper()
	p := hub.HandoffTokenFilePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAuthResetter_IsolatedMode_SIGUSR2Adopt(t *testing.T) {
	h, runtimePath := newIsolatedResetHarness(t, hub.TokenStoreRuntimeFile)
	handoff := writeHandoffTokenForTest(t, h.newTok)

	h.r.reset(authResetSignal)

	if got := h.client.AuthToken(); got != h.newTok {
		t.Fatalf("client.AuthToken() = %q, want newTok", got)
	}
	if got := hub.ReadTokenFile(); got != h.newTok {
		t.Fatalf("runtime store token = %q, want newTok", got)
	}
	info, err := os.Stat(runtimePath)
	if err != nil {
		t.Fatalf("stat runtime token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("runtime token file mode = %04o, want 0600", perm)
	}
	if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
		t.Fatalf("handoff file must be unlinked after SIGUSR2 adopt: lstat err=%v", err)
	}
}

func TestAuthResetter_IsolatedMode_RefusedCandidateUnlinksHandoff(t *testing.T) {
	t.Run("SIGUSR2 invalid token leaves current token and unlinks handoff", func(t *testing.T) {
		h, _ := newIsolatedResetHarness(t, hub.TokenStoreRuntimeFile)
		handoff := writeHandoffTokenForTest(t, "not-a-jwt")

		h.r.reset(authResetSignal)

		if got := h.client.AuthToken(); got != h.oldTok {
			t.Fatalf("client.AuthToken() changed on refused candidate")
		}
		if got := hub.ReadTokenFile(); got != h.oldTok {
			t.Fatalf("runtime store changed on refused candidate")
		}
		if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
			t.Fatalf("handoff file must be unlinked even when candidate is refused: err=%v", err)
		}
	})

	t.Run("file change hub-refused token leaves current token and unlinks handoff", func(t *testing.T) {
		h, _ := newIsolatedResetHarness(t, hub.TokenStoreRuntimeFile)
		badTok := testCredential(t, "rejected", h.now.Add(time.Minute))
		handoff := writeHandoffTokenForTest(t, badTok)

		h.r.reset(authResetFileChange)

		if got := h.client.AuthToken(); got != h.oldTok {
			t.Fatalf("client.AuthToken() changed on hub-refused candidate")
		}
		if got := hub.ReadTokenFile(); got != h.oldTok {
			t.Fatalf("runtime store changed on hub-refused candidate")
		}
		if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
			t.Fatalf("handoff file must be unlinked even when hub refuses candidate: err=%v", err)
		}
	})
}

func TestAuthResetter_IsolatedMode_WatcherAdoptsAfterHubCheck(t *testing.T) {
	h, _ := newIsolatedResetHarness(t, hub.TokenStoreRuntimeFile)

	fc := clock.NewFake(h.now)
	notify := make(chan struct{}, 1)
	w := hub.NewTokenFileWatcher(hub.TokenWatchConfig{
		Interval:    time.Second,
		StableDelay: 100 * time.Millisecond,
		Notify:      notify,
		Clock:       fc,
	})
	h.r.watcher = w

	ctx, cancel := context.WithCancel(context.Background())
	done := w.Run(ctx)
	t.Cleanup(func() {
		cancel()
		<-done
	})

	handoff := writeHandoffTokenForTest(t, h.newTok)
	deadline := time.Now().Add(2 * time.Second)
	notified := false
	for !notified && time.Now().Before(deadline) {
		fc.Advance(time.Second)
		select {
		case <-notify:
			notified = true
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !notified {
		t.Fatal("watcher did not notify for handoff write")
	}

	h.r.reset(authResetFileChange)

	if !h.hub.checked(h.newTok) {
		t.Fatal("expected hub check before adopting watcher-detected handoff")
	}
	if got := h.client.AuthToken(); got != h.newTok {
		t.Fatalf("client.AuthToken() = %q, want newTok", got)
	}
	if got := hub.ReadTokenFile(); got != h.newTok {
		t.Fatalf("runtime store token = %q, want newTok", got)
	}
	if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
		t.Fatalf("handoff file must be unlinked after watcher adopt: err=%v", err)
	}
}

func TestAuthResetter_IsolatedMode_MemoryMode(t *testing.T) {
	h, _ := newIsolatedResetHarness(t, hub.TokenStoreMemory)
	handoff := writeHandoffTokenForTest(t, h.newTok)

	h.r.reset(authResetSignal)

	if got := h.client.AuthToken(); got != h.newTok {
		t.Fatalf("client.AuthToken() = %q, want newTok", got)
	}
	if got := hub.ReadTokenFile(); got != h.newTok {
		t.Fatalf("in-memory token = %q, want newTok", got)
	}
	if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
		t.Fatalf("handoff file must be unlinked in memory mode: err=%v", err)
	}
}

func TestSetupShellIsolatedTokenStore_FallbackToMemoryWhenRuntimeUnwritable(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))
	t.Cleanup(hub.SetRuntimeTokenStoreForTest())

	// Block runtime directory creation by placing a regular file where the
	// parent directory would be.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	origPath := runtimeTokenPathForInit
	origEUID := initEffectiveUID
	runtimeTokenPathForInit = filepath.Join(blocker, "scion-token")
	initEffectiveUID = func() int { return 0 }
	t.Cleanup(func() {
		runtimeTokenPathForInit = origPath
		initEffectiveUID = origEUID
	})

	handoff := writeHandoffTokenForTest(t, "fallback-tok")
	if err := setupShellIsolatedTokenStore(1000, false); err != nil {
		t.Fatalf("setupShellIsolatedTokenStore: %v", err)
	}
	if got := hub.CurrentTokenStoreMode(); got != hub.TokenStoreMemory {
		t.Fatalf("mode = %v, want TokenStoreMemory on unwritable runtime dir", got)
	}
	if got := hub.ReadTokenFile(); got != "fallback-tok" {
		t.Fatalf("ReadTokenFile() = %q, want %q", got, "fallback-tok")
	}
	if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
		t.Fatalf("handoff file must be unlinked on memory fallback: err=%v", err)
	}
}

func TestSetupShellIsolatedTokenStore_RootInitUsesRuntimeFile(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))
	t.Cleanup(hub.SetRuntimeTokenStoreForTest())

	runtimePath := filepath.Join(t.TempDir(), "run-scion", hub.TokenFile)
	origPath := runtimeTokenPathForInit
	origEUID := initEffectiveUID
	runtimeTokenPathForInit = runtimePath
	initEffectiveUID = func() int { return 0 }
	t.Cleanup(func() {
		runtimeTokenPathForInit = origPath
		initEffectiveUID = origEUID
	})

	handoff := writeHandoffTokenForTest(t, "root-init-tok")
	staleTmp := hub.HandoffTokenTmpFilePath()
	if err := os.WriteFile(staleTmp, []byte("stale-tmp"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := setupShellIsolatedTokenStore(1000, false); err != nil {
		t.Fatalf("setupShellIsolatedTokenStore: %v", err)
	}
	if got := hub.CurrentTokenStoreMode(); got != hub.TokenStoreRuntimeFile {
		t.Fatalf("mode = %v, want TokenStoreRuntimeFile", got)
	}
	if got := hub.ReadTokenFile(); got != "root-init-tok" {
		t.Fatalf("ReadTokenFile() = %q, want %q", got, "root-init-tok")
	}
	if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
		t.Fatalf("handoff file still exists: err=%v", err)
	}
	if _, err := os.Lstat(staleTmp); !os.IsNotExist(err) {
		t.Fatalf("stale tmp file still exists: err=%v", err)
	}
	info, err := os.Stat(runtimePath)
	if err != nil {
		t.Fatalf("stat runtime token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("runtime token file mode = %04o, want 0600", perm)
	}
}

func TestRunInit_ShellIsolation_HandoffAbsentBeforeSupervisorRun(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))
	t.Cleanup(hub.SetRuntimeTokenStoreForTest())

	origSetup := runSetupHostUser
	origBeforeSup := beforeSupervisorRunForTest
	runSetupHostUser = func(bool) (int, int, bool) { return 0, 0, true }
	t.Cleanup(func() {
		runSetupHostUser = origSetup
		beforeSupervisorRunForTest = origBeforeSup
	})

	// Pre-populate both the broker handoff token and a stale .tmp file.
	handoff := writeHandoffTokenForTest(t, "pre-sup-handoff-tok")
	staleTmp := hub.HandoffTokenTmpFilePath()
	if err := os.WriteFile(staleTmp, []byte("stale-tmp-tok"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("SCION_HARNESS", "shell")
	t.Setenv(state.HarnessReadyEnv, `{"strategy":"idle"}`)
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_AGENT_ID", "")

	var observed bool
	var handoffExistedAtRun bool
	var tmpExistedAtRun bool
	var runtimeTokenAtRun string
	var modeAtRun hub.TokenStoreMode
	beforeSupervisorRunForTest = func(cfg supervisor.Config) {
		observed = true
		_, err := os.Lstat(handoff)
		handoffExistedAtRun = !os.IsNotExist(err)
		_, tmpErr := os.Lstat(staleTmp)
		tmpExistedAtRun = !os.IsNotExist(tmpErr)
		runtimeTokenAtRun = hub.ReadTokenFile()
		modeAtRun = hub.CurrentTokenStoreMode()
	}

	code := RunInit([]string{"true"}, InitRunOptions{
		DisableReExec:               true,
		DisableTermSignalForwarding: true,
		DisablePortForwarding:       true,
	})
	if code != 0 {
		t.Fatalf("RunInit exited %d, want 0", code)
	}
	if !observed {
		t.Fatal("beforeSupervisorRunForTest was not called")
	}
	if handoffExistedAtRun {
		t.Fatal("handoff token file still existed at sup.Run")
	}
	if tmpExistedAtRun {
		t.Fatal("stale scion-token.tmp still existed at sup.Run")
	}
	if modeAtRun != hub.TokenStoreMemory {
		t.Fatalf("mode at sup.Run = %v, want TokenStoreMemory", modeAtRun)
	}
	if runtimeTokenAtRun != "pre-sup-handoff-tok" {
		t.Fatalf("runtime token at sup.Run = %q, want %q", runtimeTokenAtRun, "pre-sup-handoff-tok")
	}
}

func TestRunInit_ShellIsolation_RefusesStartWhenHandoffCannotBeUnlinked(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))
	t.Cleanup(hub.SetRuntimeTokenStoreForTest())

	origRuntimePath := runtimeTokenPathForInit
	origSetup := runSetupHostUser
	origBeforeSup := beforeSupervisorRunForTest
	runtimeTokenPathForInit = filepath.Join(t.TempDir(), "run-scion", hub.TokenFile)
	runSetupHostUser = func(bool) (int, int, bool) { return 0, 0, true }
	t.Cleanup(func() {
		runtimeTokenPathForInit = origRuntimePath
		runSetupHostUser = origSetup
		beforeSupervisorRunForTest = origBeforeSup
	})

	// Plant a directory at the handoff path so unlink fails closed.
	handoff := hub.HandoffTokenFilePath()
	if err := os.MkdirAll(handoff, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("SCION_HARNESS", "shell")
	t.Setenv(state.HarnessReadyEnv, `{"strategy":"idle"}`)
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_AGENT_ID", "")

	supReached := false
	beforeSupervisorRunForTest = func(cfg supervisor.Config) {
		supReached = true
	}

	code := RunInit([]string{"true"}, InitRunOptions{
		DisableReExec:               true,
		DisableTermSignalForwarding: true,
		DisablePortForwarding:       true,
	})
	if code == 0 {
		t.Fatal("RunInit exited 0, expected non-zero fail-closed exit when handoff cannot be unlinked")
	}
	if supReached {
		t.Fatal("sup.Run was reached despite unremovable handoff token path")
	}

	// Verify reportInitFailure wrote PhaseError to agent-info.json.
	data, err := os.ReadFile(filepath.Join(home, "agent-info.json"))
	if err != nil {
		t.Fatalf("expected agent-info.json from reportInitFailure: %v", err)
	}
	var info struct {
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("unmarshal agent-info.json: %v", err)
	}
	if info.Phase != string(state.PhaseError) {
		t.Fatalf("agent-info.json phase = %q, want %q", info.Phase, state.PhaseError)
	}
}

func TestRunInit_NonShell_PreservesHomeTokenFile(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))

	origSetup := runSetupHostUser
	origBeforeSup := beforeSupervisorRunForTest
	runSetupHostUser = func(bool) (int, int, bool) { return 0, 0, true }
	t.Cleanup(func() {
		runSetupHostUser = origSetup
		beforeSupervisorRunForTest = origBeforeSup
	})

	handoff := writeHandoffTokenForTest(t, "non-shell-token")

	t.Setenv("HOME", home)
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv(state.HarnessReadyEnv, `{"strategy":"delay","timeout_ms":15000}`)
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_AGENT_ID", "")

	var observed bool
	var homeTokenExisted bool
	var mode hub.TokenStoreMode
	beforeSupervisorRunForTest = func(cfg supervisor.Config) {
		observed = true
		_, err := os.Stat(handoff)
		homeTokenExisted = err == nil
		mode = hub.CurrentTokenStoreMode()
	}

	code := RunInit([]string{"true"}, InitRunOptions{
		DisableReExec:               true,
		DisableTermSignalForwarding: true,
		DisablePortForwarding:       true,
	})
	if code != 0 {
		t.Fatalf("RunInit exited %d, want 0", code)
	}
	if !observed {
		t.Fatal("beforeSupervisorRunForTest was not called")
	}
	if !homeTokenExisted {
		t.Fatal("non-shell agent must preserve ~/.scion/scion-token")
	}
	if mode != hub.TokenStoreHome {
		t.Fatalf("non-shell mode = %v, want TokenStoreHome", mode)
	}
}
