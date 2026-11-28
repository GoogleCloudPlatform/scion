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
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/services"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/supervisor"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
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
	origBeforeVerify := beforeVerifyHandoffAbsentForTest
	origBeforeSup := beforeSupervisorRunForTest
	origServicesStart := runServicesStart
	runtimeTokenPathForInit = filepath.Join(t.TempDir(), "run-scion", hub.TokenFile)
	runSetupHostUser = func(bool) (int, int, bool) { return 0, 0, true }
	t.Cleanup(func() {
		runtimeTokenPathForInit = origRuntimePath
		runSetupHostUser = origSetup
		beforeVerifyHandoffAbsentForTest = origBeforeVerify
		beforeSupervisorRunForTest = origBeforeSup
		runServicesStart = origServicesStart
	})

	// Plant a directory at the handoff path so the first take fails closed.
	handoff := hub.HandoffTokenFilePath()
	if err := os.MkdirAll(handoff, 0o700); err != nil {
		t.Fatal(err)
	}
	// Add a sidecar service config to prove the first-take unlink failure exits
	// before sidecars or the pre-sup.Run reconcile step ever run (m7).
	svcYAML := "- name: testsvc\n  command: [\"true\"]\n"
	if err := os.WriteFile(filepath.Join(home, ".scion", "scion-services.yaml"), []byte(svcYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("SCION_HARNESS", "shell")
	t.Setenv(state.HarnessReadyEnv, `{"strategy":"idle"}`)
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_AGENT_ID", "")

	servicesReached := false
	runServicesStart = func(ctx context.Context, m *services.Manager, specs []api.ServiceSpec, uid, gid int, username string, requirePrivilegeDrop bool) error {
		servicesReached = true
		return nil
	}
	reconcileReached := false
	beforeVerifyHandoffAbsentForTest = func() {
		reconcileReached = true
	}
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
	if servicesReached {
		t.Fatal("sidecar services started despite first-take handoff unlink failure")
	}
	if reconcileReached {
		t.Fatal("pre-sup.Run reconcile step reached despite first-take handoff unlink failure")
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

func TestRunInit_ShellIsolation_TransportTokenHandoffAbsentBeforeSupervisorRun(t *testing.T) {
	t.Run("stale persistent-home transport token without env is unlinked and NOT adopted", func(t *testing.T) {
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

		handoffTransport := hub.HandoffTransportTokenFilePath()
		if err := os.MkdirAll(filepath.Dir(handoffTransport), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(handoffTransport, []byte("stale-persistent-transport"), 0o600); err != nil {
			t.Fatal(err)
		}
		staleTransportTmp := hub.HandoffTransportTokenTmpFilePath()
		if err := os.WriteFile(staleTransportTmp, []byte("stale-transport-tmp"), 0o600); err != nil {
			t.Fatal(err)
		}

		t.Setenv("HOME", home)
		t.Setenv("SCION_HARNESS", "shell")
		t.Setenv(state.HarnessReadyEnv, `{"strategy":"idle"}`)
		t.Setenv("SCION_HUB_ENDPOINT", "")
		t.Setenv("SCION_HUB_URL", "")
		t.Setenv("SCION_AGENT_ID", "")
		t.Setenv(transportauth.EnvTransportToken, "")
		t.Setenv(transportauth.EnvTransportTokenFile, "")

		var observed bool
		var transportHandoffExisted bool
		var transportTmpExisted bool
		var memTransportTok string
		beforeSupervisorRunForTest = func(cfg supervisor.Config) {
			observed = true
			_, err := os.Lstat(handoffTransport)
			transportHandoffExisted = !os.IsNotExist(err)
			_, tmpErr := os.Lstat(staleTransportTmp)
			transportTmpExisted = !os.IsNotExist(tmpErr)
			memTransportTok, _ = hub.ReadTransportTokenFileGuarded("")
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
		if transportHandoffExisted {
			t.Fatal("handoff transport-token still existed at sup.Run")
		}
		if transportTmpExisted {
			t.Fatal("stale transport-token.tmp still existed at sup.Run")
		}
		if memTransportTok != "" {
			t.Fatalf("stale persistent-home transport token was adopted into memory: %q", memTransportTok)
		}
	})

	t.Run("memory mode with SCION_TRANSPORT_TOKEN purges env via re-exec and adopts into memory", func(t *testing.T) {
		home := t.TempDir()
		t.Cleanup(hub.SetTokenHome(home))
		t.Cleanup(hub.SetRuntimeTokenStoreForTest())

		origSetup := runSetupHostUser
		origBeforeSup := beforeSupervisorRunForTest
		origReExec := reExecWithCleanEnv
		runSetupHostUser = func(bool) (int, int, bool) { return 0, 0, true }
		t.Cleanup(func() {
			runSetupHostUser = origSetup
			beforeSupervisorRunForTest = origBeforeSup
			reExecWithCleanEnv = origReExec
		})

		reExecCalled := false
		envVarAtReExec := "UNSET"
		reExecWithCleanEnv = func() error {
			reExecCalled = true
			envVarAtReExec = os.Getenv(transportauth.EnvTransportToken)
			hub.ResetTokenStore()
			return nil
		}

		t.Setenv("HOME", home)
		t.Setenv("SCION_HARNESS", "shell")
		t.Setenv(state.HarnessReadyEnv, `{"strategy":"idle"}`)
		t.Setenv("SCION_HUB_ENDPOINT", "")
		t.Setenv("SCION_HUB_URL", "")
		t.Setenv("SCION_AGENT_ID", "")
		t.Setenv(transportauth.EnvTransportToken, "bootstrap-iap-tok")
		t.Setenv(transportauth.EnvTransportTokenFile, "")

		var observed bool
		var transportHandoffExisted bool
		var envVarAtRun string
		var memTransportTok string
		beforeSupervisorRunForTest = func(cfg supervisor.Config) {
			observed = true
			_, err := os.Lstat(hub.HandoffTransportTokenFilePath())
			transportHandoffExisted = !os.IsNotExist(err)
			envVarAtRun = os.Getenv(transportauth.EnvTransportToken)
			memTransportTok, _ = hub.ReadTransportTokenFileGuarded("")
		}

		code := RunInit([]string{"true"}, InitRunOptions{
			DisableTermSignalForwarding: true,
			DisablePortForwarding:       true,
		})
		if code != 0 {
			t.Fatalf("RunInit exited %d, want 0", code)
		}
		if !reExecCalled {
			t.Fatal("expected reExecWithCleanEnv to purge SCION_TRANSPORT_TOKEN from /proc/1/environ in memory mode")
		}
		if envVarAtReExec != "" {
			t.Fatalf("SCION_TRANSPORT_TOKEN was still set at re-exec: %q", envVarAtReExec)
		}
		if !observed {
			t.Fatal("beforeSupervisorRunForTest was not called")
		}
		if envVarAtRun != "" {
			t.Fatalf("SCION_TRANSPORT_TOKEN still set at sup.Run: %q", envVarAtRun)
		}
		if transportHandoffExisted {
			t.Fatal("handoff transport-token still existed at sup.Run")
		}
		if memTransportTok != "bootstrap-iap-tok" {
			t.Fatalf("in-memory transport token = %q, want %q", memTransportTok, "bootstrap-iap-tok")
		}
	})
}

func TestRunInit_ShellIsolation_StagedHubTokenSurvivesReExecInMemoryMode(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))
	t.Cleanup(hub.SetRuntimeTokenStoreForTest())

	origSetup := runSetupHostUser
	origBeforeSup := beforeSupervisorRunForTest
	origReExec := reExecWithCleanEnv
	runSetupHostUser = func(bool) (int, int, bool) { return 0, 0, true }
	t.Cleanup(func() {
		runSetupHostUser = origSetup
		beforeSupervisorRunForTest = origBeforeSup
		reExecWithCleanEnv = origReExec
	})

	// Simulate a real syscall.Exec by wiping all package-level token-store
	// state (including any in-memory token) when reExecWithCleanEnv fires.
	reExecCalled := false
	reExecWithCleanEnv = func() error {
		reExecCalled = true
		hub.ResetTokenStore()
		return nil
	}

	encoded := stagedBlob(t, "staged-cloudrun-hub-tok")

	t.Setenv("HOME", home)
	t.Setenv("SCION_HARNESS", "shell")
	t.Setenv(state.HarnessReadyEnv, `{"strategy":"idle"}`)
	t.Setenv(stagedsecrets.EnvVar, encoded)
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_AGENT_ID", "")

	var observed bool
	var tokenAtRun string
	var handoffExistsAtRun bool
	var modeAtRun hub.TokenStoreMode
	beforeSupervisorRunForTest = func(cfg supervisor.Config) {
		observed = true
		tokenAtRun = hub.ReadTokenFile()
		_, lerr := os.Lstat(hub.HandoffTokenFilePath())
		handoffExistsAtRun = !os.IsNotExist(lerr)
		modeAtRun = hub.CurrentTokenStoreMode()
	}

	code := RunInit([]string{"true"}, InitRunOptions{
		DisableTermSignalForwarding: true,
		DisablePortForwarding:       true,
	})
	if code != 0 {
		t.Fatalf("RunInit exited %d, want 0", code)
	}
	if !reExecCalled {
		t.Fatal("expected reExecWithCleanEnv to be called for SCION_STAGED_SECRETS")
	}
	if !observed {
		t.Fatal("beforeSupervisorRunForTest was not called")
	}
	if modeAtRun != hub.TokenStoreMemory {
		t.Fatalf("mode at sup.Run = %v, want TokenStoreMemory", modeAtRun)
	}
	if tokenAtRun != "staged-cloudrun-hub-tok" {
		t.Fatalf("token at sup.Run = %q, want %q (staged hub token lost across re-exec!)", tokenAtRun, "staged-cloudrun-hub-tok")
	}
	if handoffExistsAtRun {
		t.Fatal("handoff token file still existed at sup.Run")
	}
}

func TestAuthResetter_IsolatedMode_PendingCandidateSurvivesUncheckedAndRefused(t *testing.T) {
	t.Run("watcher 503 then SIGUSR2 adopts pending candidate with no file on disk", func(t *testing.T) {
		h, _ := newIsolatedResetHarness(t, hub.TokenStoreRuntimeFile)
		h.hub.set(func(ch *credHub) { ch.checkStatus = 503 })

		handoff := writeHandoffTokenForTest(t, h.newTok)
		h.r.reset(authResetFileChange)

		// Handoff is unlinked, client stays on oldTok.
		if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
			t.Fatalf("handoff still exists after watcher 503: err=%v", err)
		}
		if got := h.client.AuthToken(); got != h.oldTok {
			t.Fatalf("client adopted token despite 503")
		}

		// Broker's SIGUSR2 arrives with no handoff file left on disk.
		h.r.reset(authResetSignal)
		if got := h.client.AuthToken(); got != h.newTok {
			t.Fatalf("SIGUSR2 did not adopt pending candidate after watcher 503: got %q", got)
		}
		if got := hub.ReadTokenFile(); got != h.newTok {
			t.Fatalf("runtime store not updated on SIGUSR2 adopt of pending candidate")
		}
		if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
			t.Fatalf("handoff path must never be written back: err=%v", err)
		}
	})

	t.Run("watcher 401 then SIGUSR2 adopts pending candidate with no file on disk", func(t *testing.T) {
		h, _ := newIsolatedResetHarness(t, hub.TokenStoreRuntimeFile)
		h.hub.set(func(ch *credHub) { ch.checkStatus = 401 })

		handoff := writeHandoffTokenForTest(t, h.newTok)
		h.r.reset(authResetFileChange)

		if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
			t.Fatalf("handoff still exists after watcher 401: err=%v", err)
		}
		if got := h.client.AuthToken(); got != h.oldTok {
			t.Fatalf("client adopted token despite 401")
		}

		// Broker's SIGUSR2 arrives and adopts under signal rules.
		h.hub.set(func(ch *credHub) { ch.checkStatus = 0 })
		h.r.reset(authResetSignal)
		if got := h.client.AuthToken(); got != h.newTok {
			t.Fatalf("SIGUSR2 did not adopt pending candidate after watcher 401: got %q", got)
		}
		if got := hub.ReadTokenFile(); got != h.newTok {
			t.Fatalf("runtime store not updated on SIGUSR2 adopt after 401")
		}
	})

	t.Run("watcher 503 then due retry adopts pending candidate with no file on disk", func(t *testing.T) {
		h, _ := newIsolatedResetHarness(t, hub.TokenStoreRuntimeFile)
		h.hub.set(func(ch *credHub) { ch.checkStatus = 503 })

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
		h.r.reset(authResetFileChange)

		if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
			t.Fatalf("handoff still exists after watcher 503: err=%v", err)
		}
		if got := h.client.AuthToken(); got != h.oldTok {
			t.Fatalf("client adopted token despite 503")
		}

		// Hub comes back up while no handoff file exists on disk.
		h.hub.set(func(ch *credHub) { ch.checkStatus = 0 })

		// Advance past credentialRecheckBase (5s) + interval + stable delay.
		deadline := time.Now().Add(2 * time.Second)
		notified := false
		for !notified && time.Now().Before(deadline) {
			fc.Advance(2 * time.Second)
			select {
			case <-notify:
				notified = true
			case <-time.After(15 * time.Millisecond):
			}
		}
		if !notified {
			t.Fatal("watcher did not fire due pending retry with no handoff file on disk")
		}

		h.r.reset(authResetFileChange)
		if got := h.client.AuthToken(); got != h.newTok {
			t.Fatalf("due retry did not adopt pending candidate: got %q", got)
		}
		if got := hub.ReadTokenFile(); got != h.newTok {
			t.Fatalf("runtime store not updated on due retry adopt")
		}
	})
}

func TestRunInit_ShellIsolation_PreSupervisorVerifyRefusesLatePlantedHandoff(t *testing.T) {
	t.Run("late regular-file handoff is taken and start proceeds with handoff absent at Run", func(t *testing.T) {
		home := t.TempDir()
		t.Cleanup(hub.SetTokenHome(home))
		t.Cleanup(hub.SetRuntimeTokenStoreForTest())

		origSetup := runSetupHostUser
		origBeforeVerify := beforeVerifyHandoffAbsentForTest
		origBeforeSup := beforeSupervisorRunForTest
		runSetupHostUser = func(bool) (int, int, bool) { return 0, 0, true }
		t.Cleanup(func() {
			runSetupHostUser = origSetup
			beforeVerifyHandoffAbsentForTest = origBeforeVerify
			beforeSupervisorRunForTest = origBeforeSup
		})

		lateTok := testCredential(t, "late-reset", time.Now())
		var latePath string
		beforeVerifyHandoffAbsentForTest = func() {
			latePath = writeHandoffTokenForTest(t, lateTok)
		}

		supReached := false
		handoffExistsAtRun := true
		beforeSupervisorRunForTest = func(cfg supervisor.Config) {
			supReached = true
			_, err := os.Lstat(latePath)
			handoffExistsAtRun = !os.IsNotExist(err)
		}

		t.Setenv("HOME", home)
		t.Setenv("SCION_HARNESS", "shell")
		t.Setenv(state.HarnessReadyEnv, `{"strategy":"idle"}`)
		t.Setenv("SCION_HUB_ENDPOINT", "")
		t.Setenv("SCION_HUB_URL", "")
		t.Setenv("SCION_AGENT_ID", "")

		code := RunInit([]string{"true"}, InitRunOptions{
			DisableReExec:               true,
			DisableTermSignalForwarding: true,
			DisablePortForwarding:       true,
		})
		if code != 0 {
			t.Fatalf("RunInit exited %d, want 0 when late regular-file handoff can be taken", code)
		}
		if !supReached {
			t.Fatal("sup.Run was not reached")
		}
		if handoffExistsAtRun {
			t.Fatal("late-planted handoff file still existed at sup.Run")
		}
	})

	t.Run("late unremovable directory at handoff path fails closed", func(t *testing.T) {
		home := t.TempDir()
		t.Cleanup(hub.SetTokenHome(home))
		t.Cleanup(hub.SetRuntimeTokenStoreForTest())

		origSetup := runSetupHostUser
		origBeforeVerify := beforeVerifyHandoffAbsentForTest
		origBeforeSup := beforeSupervisorRunForTest
		runSetupHostUser = func(bool) (int, int, bool) { return 0, 0, true }
		t.Cleanup(func() {
			runSetupHostUser = origSetup
			beforeVerifyHandoffAbsentForTest = origBeforeVerify
			beforeSupervisorRunForTest = origBeforeSup
		})

		// Plant an unremovable directory AFTER setupShellIsolatedTokenStore has
		// already run, right before reconcileShellHandoffBeforeSupervisorRun.
		beforeVerifyHandoffAbsentForTest = func() {
			if err := os.MkdirAll(hub.HandoffTokenFilePath(), 0o700); err != nil {
				t.Fatal(err)
			}
		}

		supReached := false
		beforeSupervisorRunForTest = func(cfg supervisor.Config) {
			supReached = true
		}

		t.Setenv("HOME", home)
		t.Setenv("SCION_HARNESS", "shell")
		t.Setenv(state.HarnessReadyEnv, `{"strategy":"idle"}`)
		t.Setenv("SCION_HUB_ENDPOINT", "")
		t.Setenv("SCION_HUB_URL", "")
		t.Setenv("SCION_AGENT_ID", "")

		code := RunInit([]string{"true"}, InitRunOptions{
			DisableReExec:               true,
			DisableTermSignalForwarding: true,
			DisablePortForwarding:       true,
		})
		if code == 0 {
			t.Fatal("RunInit exited 0, expected non-zero fail-closed exit on unremovable late handoff")
		}
		if supReached {
			t.Fatal("sup.Run was reached despite unremovable late handoff path")
		}
	})
}

func TestRunInit_ShellIsolation_TakeBeforeNewClientUsesHandedOffToken(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))
	t.Cleanup(hub.SetRuntimeTokenStoreForTest())

	origSetup := runSetupHostUser
	origBeforeSup := beforeSupervisorRunForTest
	origWindow := childStartupCheckWindow
	runSetupHostUser = func(bool) (int, int, bool) { return 0, 0, true }
	childStartupCheckWindow = 20 * time.Millisecond
	t.Cleanup(func() {
		runSetupHostUser = origSetup
		beforeSupervisorRunForTest = origBeforeSup
		childStartupCheckWindow = origWindow
	})

	handoffTok := testCredential(t, "initial-handoff", time.Now())
	handoffPath := writeHandoffTokenForTest(t, handoffTok)

	var mu sync.Mutex
	var firstBearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if firstBearer == "" {
			firstBearer = r.Header.Get("X-Scion-Agent-Token")
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"agent-1"}`))
	}))
	defer srv.Close()

	t.Setenv("HOME", home)
	t.Setenv("SCION_HARNESS", "shell")
	t.Setenv(state.HarnessReadyEnv, `{"strategy":"idle"}`)
	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_AGENT_ID", "agent-1")

	var handoffExistsAtRun bool
	beforeSupervisorRunForTest = func(cfg supervisor.Config) {
		_, err := os.Lstat(handoffPath)
		handoffExistsAtRun = !os.IsNotExist(err)
	}

	code := RunInit([]string{"sh", "-c", "sleep 0.15"}, InitRunOptions{
		DisableReExec:               true,
		DisableTermSignalForwarding: true,
		DisablePortForwarding:       true,
	})
	if code != 0 {
		t.Fatalf("RunInit exited %d, want 0", code)
	}
	if handoffExistsAtRun {
		t.Fatal("handoff token still existed at sup.Run")
	}
	mu.Lock()
	gotBearer := firstBearer
	mu.Unlock()
	if gotBearer != handoffTok {
		t.Fatalf("first Hub request bearer = %q, want handed-off token %q", gotBearer, handoffTok)
	}
}

func TestRunInit_ShellIsolation_RuntimeFileModeAndStagingUIDZeroAndSidecarIsolation(t *testing.T) {
	home := t.TempDir()
	base := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))
	t.Cleanup(hub.SetRuntimeTokenStoreForTest(base))

	runtimePath := filepath.Join(base, "sciontool", "auth", hub.TokenFile)
	origRuntimePath := runtimeTokenPathForInit
	origEUID := initEffectiveUID
	origSetup := runSetupHostUser
	origBeforeSup := beforeSupervisorRunForTest
	origWrite := runWriteTokenFile
	origServicesStart := runServicesStart
	origReExec := reExecWithCleanEnv

	withLookupUserByID(t, func(string) (*user.User, error) {
		return &user.User{Username: "scion", HomeDir: home}, nil
	})
	runtimeTokenPathForInit = runtimePath
	initEffectiveUID = func() int { return 0 }
	runSetupHostUser = func(bool) (int, int, bool) { return 1000, 1000, false }
	reExecWithCleanEnv = func() error { return nil }
	t.Cleanup(func() {
		runtimeTokenPathForInit = origRuntimePath
		initEffectiveUID = origEUID
		runSetupHostUser = origSetup
		beforeSupervisorRunForTest = origBeforeSup
		runWriteTokenFile = origWrite
		runServicesStart = origServicesStart
		reExecWithCleanEnv = origReExec
	})

	// Pre-plant a far-future iat hub token to also verify O2 (shell staging
	// overwrites rather than keeping the pre-existing handoff).
	farFutureTok := testCredential(t, "planted-future", time.Now().Add(24*time.Hour))
	writeHandoffTokenForTest(t, farFutureTok)

	freshStagedTok := testCredential(t, "fresh-staged", time.Now())
	encoded := stagedBlob(t, freshStagedTok)

	// Add a valid scion-services.yaml to verify svcManager.SetWorkloadIsolation(true).
	svcYAML := "- name: testsvc\n  command: [\"true\"]\n"
	if err := os.WriteFile(filepath.Join(home, ".scion", "scion-services.yaml"), []byte(svcYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	var stagedUIDs []int
	runWriteTokenFile = func(token string, uid, gid int) error {
		stagedUIDs = append(stagedUIDs, uid)
		return hub.WriteTokenFile(token, uid, gid)
	}

	var svcManagerIsolated bool
	var svcStartCalled bool
	runServicesStart = func(ctx context.Context, m *services.Manager, specs []api.ServiceSpec, uid, gid int, username string, requirePrivilegeDrop bool) error {
		svcStartCalled = true
		svcManagerIsolated = m.WorkloadIsolation()
		return nil
	}

	t.Setenv("HOME", home)
	t.Setenv("SCION_HARNESS", "shell")
	t.Setenv(state.HarnessReadyEnv, `{"strategy":"idle"}`)
	t.Setenv(stagedsecrets.EnvVar, encoded)
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_AGENT_ID", "")

	var observed bool
	var modeAtRun hub.TokenStoreMode
	var tokAtRun string
	var handoffExistsAtRun bool
	beforeSupervisorRunForTest = func(cfg supervisor.Config) {
		observed = true
		modeAtRun = hub.CurrentTokenStoreMode()
		tokAtRun = hub.ReadTokenFile()
		_, err := os.Lstat(hub.HandoffTokenFilePath())
		handoffExistsAtRun = !os.IsNotExist(err)
	}

	// Note: when running unprivileged, sup.Run will fail setuid(1000) after
	// beforeSupervisorRunForTest; that's expected and we assert all pre-Run
	// invariants observed at the exact boundary.
	_ = RunInit([]string{"true"}, InitRunOptions{
		DisableTermSignalForwarding: true,
		DisablePortForwarding:       true,
	})

	if !observed {
		t.Fatal("beforeSupervisorRunForTest was not called")
	}
	if modeAtRun != hub.TokenStoreRuntimeFile {
		t.Fatalf("mode at sup.Run = %v, want TokenStoreRuntimeFile", modeAtRun)
	}
	if tokAtRun != freshStagedTok {
		t.Fatalf("runtime token at sup.Run = %q, want fresh staged token (not far-future planted token)", tokAtRun)
	}
	if handoffExistsAtRun {
		t.Fatal("handoff token still existed at sup.Run")
	}
	if len(stagedUIDs) == 0 || stagedUIDs[0] != 0 {
		t.Fatalf("stageHubTokenForShell wrote with uids=%v, want [0]", stagedUIDs)
	}
	if !svcStartCalled || !svcManagerIsolated {
		t.Fatalf("expected runServicesStart called with WorkloadIsolation=true (called=%v, isolated=%v)", svcStartCalled, svcManagerIsolated)
	}
}

func TestRunInit_ShellIsolation_TransportStagingOverwritesFarFuturePlantedToken(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))
	t.Cleanup(hub.SetRuntimeTokenStoreForTest())

	origSetup := runSetupHostUser
	origBeforeSup := beforeSupervisorRunForTest
	origReExec := reExecWithCleanEnv
	runSetupHostUser = func(bool) (int, int, bool) { return 0, 0, true }
	reExecWithCleanEnv = func() error {
		hub.ResetTokenStore()
		return nil
	}
	t.Cleanup(func() {
		runSetupHostUser = origSetup
		beforeSupervisorRunForTest = origBeforeSup
		reExecWithCleanEnv = origReExec
	})

	// Plant a far-future-exp transport token in ~/.scion/transport-token.
	farFutureTransport := testCredential(t, "planted-far-future", time.Now().Add(365*24*time.Hour))
	freshTransport := testCredential(t, "fresh-bootstrap", time.Now())

	handoffTransport := hub.HandoffTransportTokenFilePath()
	if err := os.MkdirAll(filepath.Dir(handoffTransport), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handoffTransport, []byte(farFutureTransport), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("SCION_HARNESS", "shell")
	t.Setenv(state.HarnessReadyEnv, `{"strategy":"idle"}`)
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_AGENT_ID", "")
	t.Setenv(transportauth.EnvTransportToken, freshTransport)
	t.Setenv(transportauth.EnvTransportTokenFile, "")

	var observed bool
	var gotTransport string
	var handoffExistsAtRun bool
	beforeSupervisorRunForTest = func(cfg supervisor.Config) {
		observed = true
		gotTransport, _ = hub.ReadTransportTokenFileGuarded("")
		_, err := os.Lstat(handoffTransport)
		handoffExistsAtRun = !os.IsNotExist(err)
	}

	code := RunInit([]string{"true"}, InitRunOptions{
		DisableTermSignalForwarding: true,
		DisablePortForwarding:       true,
	})
	if code != 0 {
		t.Fatalf("RunInit exited %d, want 0", code)
	}
	if !observed {
		t.Fatal("beforeSupervisorRunForTest was not called")
	}
	if handoffExistsAtRun {
		t.Fatal("handoff transport-token still existed at sup.Run")
	}
	if gotTransport != freshTransport {
		t.Fatalf("runtime transport token = %q, want fresh bootstrap token (far-future planted file must not win)", gotTransport)
	}
}

func TestSetupShellIsolatedTokenStore_TransportMarkerGating(t *testing.T) {
	t.Run("without SCION_TRANSPORT_TOKEN_FILE marker, handoff is unlinked and NOT seeded", func(t *testing.T) {
		home := t.TempDir()
		base := t.TempDir()
		t.Cleanup(hub.SetTokenHome(home))
		t.Cleanup(hub.SetRuntimeTokenStoreForTest(base))

		runtimePath := filepath.Join(base, "sciontool", "auth", hub.TokenFile)
		origPath := runtimeTokenPathForInit
		origEUID := initEffectiveUID
		runtimeTokenPathForInit = runtimePath
		initEffectiveUID = func() int { return 0 }
		t.Cleanup(func() {
			runtimeTokenPathForInit = origPath
			initEffectiveUID = origEUID
		})

		handoffTransport := hub.HandoffTransportTokenFilePath()
		if err := os.MkdirAll(filepath.Dir(handoffTransport), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(handoffTransport, []byte("unmarked-stale-transport"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(transportauth.EnvTransportTokenFile, "")

		if err := setupShellIsolatedTokenStore(1000, false); err != nil {
			t.Fatalf("setupShellIsolatedTokenStore: %v", err)
		}
		if _, err := os.Lstat(handoffTransport); !os.IsNotExist(err) {
			t.Fatalf("handoff transport file still exists: err=%v", err)
		}
		if _, err := os.Lstat(hub.TransportTokenFilePath()); !os.IsNotExist(err) {
			t.Fatalf("runtime transport file should NOT be seeded without staged marker: err=%v", err)
		}
	})

	t.Run("with SCION_TRANSPORT_TOKEN_FILE marker, handoff is unlinked and seeded into runtime store", func(t *testing.T) {
		home := t.TempDir()
		base := t.TempDir()
		t.Cleanup(hub.SetTokenHome(home))
		t.Cleanup(hub.SetRuntimeTokenStoreForTest(base))

		runtimePath := filepath.Join(base, "sciontool", "auth", hub.TokenFile)
		origPath := runtimeTokenPathForInit
		origEUID := initEffectiveUID
		runtimeTokenPathForInit = runtimePath
		initEffectiveUID = func() int { return 0 }
		t.Cleanup(func() {
			runtimeTokenPathForInit = origPath
			initEffectiveUID = origEUID
		})

		handoffTransport := hub.HandoffTransportTokenFilePath()
		if err := os.MkdirAll(filepath.Dir(handoffTransport), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(handoffTransport, []byte("marked-staged-transport"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(transportauth.EnvTransportTokenFile, handoffTransport)

		if err := setupShellIsolatedTokenStore(1000, false); err != nil {
			t.Fatalf("setupShellIsolatedTokenStore: %v", err)
		}
		if _, err := os.Lstat(handoffTransport); !os.IsNotExist(err) {
			t.Fatalf("handoff transport file still exists: err=%v", err)
		}
		got, err := hub.ReadTransportTokenFileGuarded(hub.TransportTokenFilePath())
		if err != nil || got != "marked-staged-transport" {
			t.Fatalf("runtime transport file = %q (err=%v), want %q", got, err, "marked-staged-transport")
		}
		if gotEnv := os.Getenv(transportauth.EnvTransportTokenFile); gotEnv != hub.TransportTokenFilePath() {
			t.Fatalf("SCION_TRANSPORT_TOKEN_FILE = %q, want %q", gotEnv, hub.TransportTokenFilePath())
		}
	})
}

func TestRunInit_ShellIsolation_LateHandoffDeferredNotAdoptedWithoutHubCheck(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))
	t.Cleanup(hub.SetRuntimeTokenStoreForTest())

	origSetup := runSetupHostUser
	origBeforeVerify := beforeVerifyHandoffAbsentForTest
	origBeforeSup := beforeSupervisorRunForTest
	origWindow := childStartupCheckWindow
	runSetupHostUser = func(bool) (int, int, bool) { return 0, 0, true }
	childStartupCheckWindow = 20 * time.Millisecond
	t.Cleanup(func() {
		runSetupHostUser = origSetup
		beforeVerifyHandoffAbsentForTest = origBeforeVerify
		beforeSupervisorRunForTest = origBeforeSup
		childStartupCheckWindow = origWindow
	})

	now := time.Now().Truncate(time.Second)
	initTok := testCredential(t, "initial-valid", now.Add(-time.Minute))
	forgedLateTok := testCredential(t, "forged-late", now)
	writeHandoffTokenForTest(t, initTok)

	// Hub accepts only initTok and refuses forgedLateTok.
	ch := newCredHub(t, initTok)

	beforeVerifyHandoffAbsentForTest = func() {
		writeHandoffTokenForTest(t, forgedLateTok)
	}

	var runtimeTokAtRun string
	var handoffExistsAtRun bool
	beforeSupervisorRunForTest = func(cfg supervisor.Config) {
		runtimeTokAtRun = hub.ReadTokenFile()
		_, err := os.Lstat(hub.HandoffTokenFilePath())
		handoffExistsAtRun = !os.IsNotExist(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("SCION_HARNESS", "shell")
	t.Setenv(state.HarnessReadyEnv, `{"strategy":"idle"}`)
	t.Setenv("SCION_HUB_ENDPOINT", ch.srv.URL)
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_AGENT_ID", "agent-1")

	code := RunInit([]string{"sh", "-c", "sleep 0.15"}, InitRunOptions{
		DisableReExec:               true,
		DisableTermSignalForwarding: true,
		DisablePortForwarding:       true,
	})
	if code != 0 {
		t.Fatalf("RunInit exited %d, want 0", code)
	}
	if handoffExistsAtRun {
		t.Fatal("late handoff file still existed at sup.Run")
	}
	if runtimeTokAtRun != initTok {
		t.Fatalf("runtime store at sup.Run = %q, want initial token (late handoff must NOT be adopted without hub check)", runtimeTokAtRun)
	}
	if ch.presented(initTok) == 0 {
		t.Fatal("expected Hub requests to use initial token")
	}
	if ch.presented(forgedLateTok) > 0 && ch.checkCountExcept(initTok) == 0 {
		t.Fatal("hubClient sent forged late token on non-check request")
	}

	// Verify the returned pending candidate flows cleanly into authResetter:
	// file-change rules refuse it (hub returns 401), while SIGUSR2 adopts it
	// under signal rules.
	h, _ := newIsolatedResetHarness(t, hub.TokenStoreRuntimeFile)
	h.hub.set(func(c *credHub) { c.checkStatus = 401 })
	writeHandoffTokenForTest(t, h.newTok)
	pending, err := reconcileShellHandoffBeforeSupervisorRun(h.client)
	if err != nil {
		t.Fatalf("reconcileShellHandoffBeforeSupervisorRun: %v", err)
	}
	if pending != h.newTok {
		t.Fatalf("pending = %q, want h.newTok", pending)
	}
	if got := h.client.AuthToken(); got != h.oldTok {
		t.Fatal("reconcile adopted late handoff directly instead of deferring to authResetter")
	}
	if got := hub.ReadTokenFile(); got != h.oldTok {
		t.Fatal("reconcile persisted late handoff to runtime store directly")
	}
	h.r.pendingCandidate = pending
	h.r.reset(authResetFileChange)
	if got := h.client.AuthToken(); got != h.oldTok {
		t.Fatal("authResetFileChange adopted hub-refused pending candidate")
	}
	h.hub.set(func(c *credHub) { c.checkStatus = 0 })
	h.r.reset(authResetSignal)
	if got := h.client.AuthToken(); got != h.newTok {
		t.Fatal("SIGUSR2 did not adopt pending candidate under signal rules")
	}
}

func TestChownTokenFileForStartup_SkippedUnderShellIsolation(t *testing.T) {
	origChown := runChownTokenFile
	t.Cleanup(func() { runChownTokenFile = origChown })

	called := 0
	var gotUID, gotGID int
	runChownTokenFile = func(uid, gid int) error {
		called++
		gotUID, gotGID = uid, gid
		return nil
	}

	// Under shellIsolation with targetUID > 0, ChownTokenFile must be skipped.
	chownTokenFileForStartup(true, "some-token", 1000, 1000)
	if called != 0 {
		t.Fatalf("runChownTokenFile called %d times under shellIsolation, want 0", called)
	}

	// Non-shell regression: targetUID > 0 must call ChownTokenFile(1000, 1000).
	chownTokenFileForStartup(false, "some-token", 1000, 1000)
	if called != 1 || gotUID != 1000 || gotGID != 1000 {
		t.Fatalf("non-shell runChownTokenFile called=%d uid=%d gid=%d, want 1, 1000, 1000", called, gotUID, gotGID)
	}
}

func TestStartCredentialFileWatch_IsolatedTakesPreExistingAgentHandoff(t *testing.T) {
	h, _ := newIsolatedResetHarness(t, hub.TokenStoreRuntimeFile)

	// Write an agent handoff BEFORE startCredentialFileWatch creates the watcher.
	handoff := writeHandoffTokenForTest(t, h.newTok)

	fc := clock.NewFake(h.now)
	notify := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := startCredentialFileWatch(ctx, h.client, hub.TokenWatchConfig{
		Interval:    time.Second,
		StableDelay: 100 * time.Millisecond,
		Notify:      notify,
		Clock:       fc,
	})
	w.RecheckAgent()
	h.r.watcher = w

	// Must notify within one interval + stable delay.
	deadline := time.Now().Add(2 * time.Second)
	notified := false
	for !notified && time.Now().Before(deadline) {
		fc.Advance(time.Second)
		select {
		case <-notify:
			notified = true
		case <-time.After(15 * time.Millisecond):
		}
	}
	if !notified {
		t.Fatal("watcher did not report pre-existing agent handoff within one interval")
	}

	h.r.reset(authResetFileChange)
	if got := h.client.AuthToken(); got != h.newTok {
		t.Fatalf("client.AuthToken() = %q, want newTok", got)
	}
	if _, err := os.Lstat(handoff); !os.IsNotExist(err) {
		t.Fatalf("pre-existing agent handoff still exists after watcher cycle: err=%v", err)
	}
}

func TestStartCredentialFileWatch_IsolatedTakesPreExistingTransportHandoff(t *testing.T) {
	h, _ := newIsolatedResetHarness(t, hub.TokenStoreRuntimeFile)

	// Configure a hub-provided transport source with an initial token.
	curTransport := testCredential(t, "cur-transport", h.now)
	if err := hub.WriteTransportTokenFile(curTransport, 0, 0); err != nil {
		t.Fatalf("WriteTransportTokenFile: %v", err)
	}
	h.client.SetTransportAuth(hub.NewTransportTokenFileSource(), transportauth.HeaderProxyAuthorization)

	// Write a transport handoff (even with the same value as current) BEFORE
	// startCredentialFileWatch creates the watcher.
	handoffTransport := hub.HandoffTransportTokenFilePath()
	if err := os.MkdirAll(filepath.Dir(handoffTransport), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handoffTransport, []byte(curTransport), 0o600); err != nil {
		t.Fatal(err)
	}

	fc := clock.NewFake(h.now)
	notify := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := startCredentialFileWatch(ctx, h.client, hub.TokenWatchConfig{
		Interval:    time.Second,
		StableDelay: 100 * time.Millisecond,
		Notify:      notify,
		Clock:       fc,
	})
	w.RecheckAgent()
	w.RecheckTransport()
	h.r.watcher = w

	deadline := time.Now().Add(2 * time.Second)
	notified := false
	for !notified && time.Now().Before(deadline) {
		fc.Advance(time.Second)
		select {
		case <-notify:
			notified = true
		case <-time.After(15 * time.Millisecond):
		}
	}
	if !notified {
		t.Fatal("watcher did not report pre-existing transport handoff within one interval")
	}

	h.r.reset(authResetFileChange)
	if _, err := os.Lstat(handoffTransport); !os.IsNotExist(err) {
		t.Fatalf("pre-existing transport handoff still exists after watcher cycle: err=%v", err)
	}
}

func TestRunInit_ShellIsolation_TransportStagingFailureFailsClosed(t *testing.T) {
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

	// Block staging to ~/.scion/transport-token by placing a regular file at ~/.scion.
	if err := os.WriteFile(filepath.Join(home, ".scion"), []byte("not-a-dir"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("SCION_HARNESS", "shell")
	t.Setenv(state.HarnessReadyEnv, `{"strategy":"idle"}`)
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("SCION_AGENT_ID", "")
	t.Setenv(transportauth.EnvTransportToken, "bootstrap-iap-secret")
	t.Setenv(transportauth.EnvTransportTokenFile, "")

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
		t.Fatal("RunInit exited 0, expected non-zero fail-closed exit when transport token staging fails")
	}
	if supReached {
		t.Fatal("sup.Run was reached despite failed transport token staging under shellIsolation")
	}

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
