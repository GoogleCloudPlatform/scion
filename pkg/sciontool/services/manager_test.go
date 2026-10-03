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

package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
)

func setupTestEnv(t *testing.T) (cleanup func()) {
	t.Helper()
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpDir)
	log.SetLogPath(filepath.Join(tmpDir, "agent.log"))
	return func() {
		_ = os.Setenv("HOME", origHome)
	}
}

func (svc *managedService) currentFailures() int {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return svc.failures
}

func TestManager_StartAndShutdown(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	mgr := New(5 * time.Second)
	specs := []api.ServiceSpec{
		{Name: "sleeper1", Command: []string{"sleep", "60"}},
		{Name: "sleeper2", Command: []string{"sleep", "60"}},
	}

	ctx := context.Background()
	if err := mgr.Start(ctx, specs, 0, 0, "", false); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Verify services are running
	mgr.mu.Lock()
	if len(mgr.services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(mgr.services))
	}
	svcs := make([]*managedService, len(mgr.services))
	copy(svcs, mgr.services)
	mgr.mu.Unlock()
	for _, svc := range svcs {
		if svc.isExited() {
			t.Errorf("service %s should be running", svc.spec.Name)
		}
		cmd, _ := svc.snapshotProcess()
		if cmd == nil || cmd.Process == nil {
			t.Errorf("service %s has no process", svc.spec.Name)
		}
	}

	// Shutdown
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mgr.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}

	// Verify services have exited
	for _, svc := range svcs {
		if !svc.isExited() {
			t.Errorf("service %s should have exited after shutdown", svc.spec.Name)
		}
	}
}

func TestManager_RestartOnFailure(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	mgr := New(5 * time.Second)
	// "false" exits with code 1 — should trigger on-failure restart
	specs := []api.ServiceSpec{
		{Name: "failer", Command: []string{"false"}, Restart: "on-failure"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx, specs, 0, 0, "", false); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	mgr.mu.Lock()
	svc := mgr.services[0]
	mgr.mu.Unlock()

	deadline := time.After(10 * time.Second)
	for svc.currentFailures() == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for at least one restart attempt for on-failure policy")
		case <-time.After(100 * time.Millisecond):
		}
	}

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	_ = mgr.Shutdown(shutdownCtx)
}

func TestManager_RestartAlways(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	mgr := New(5 * time.Second)
	// "true" exits with code 0 — should still restart with "always" policy
	specs := []api.ServiceSpec{
		{Name: "exiter", Command: []string{"true"}, Restart: "always"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx, specs, 0, 0, "", false); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	mgr.mu.Lock()
	svc := mgr.services[0]
	mgr.mu.Unlock()

	deadline := time.After(10 * time.Second)
	for !svc.isAbandoned() && svc.currentFailures() == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for restart attempts under always policy")
		case <-time.After(100 * time.Millisecond):
		}
	}

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	_ = mgr.Shutdown(shutdownCtx)
}

func TestManager_RestartNo(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	mgr := New(5 * time.Second)
	// "true" exits with code 0 — should NOT restart with "no" policy
	specs := []api.ServiceSpec{
		{Name: "oneshot", Command: []string{"true"}, Restart: "no"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx, specs, 0, 0, "", false); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Wait for process to exit
	time.Sleep(1 * time.Second)

	mgr.mu.Lock()
	svc := mgr.services[0]
	mgr.mu.Unlock()
	if !svc.isExited() {
		t.Error("expected service to have exited")
	}
	if f := svc.currentFailures(); f != 0 {
		t.Errorf("expected 0 failures (no restart), got %d", f)
	}

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	_ = mgr.Shutdown(shutdownCtx)
}

func TestManager_MaxRestarts(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	mgr := New(5 * time.Second)
	// "false" exits with code 1 — will be restarted up to 3 times then abandoned
	specs := []api.ServiceSpec{
		{Name: "crasher", Command: []string{"false"}, Restart: "on-failure"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx, specs, 0, 0, "", false); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	mgr.mu.Lock()
	svc := mgr.services[0]
	mgr.mu.Unlock()

	deadline := time.After(30 * time.Second)
	for !svc.isAbandoned() {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for service to be abandoned after max restarts")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if f := svc.currentFailures(); f < maxConsecutiveFailures {
		t.Errorf("expected at least %d failures, got %d", maxConsecutiveFailures, f)
	}

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	_ = mgr.Shutdown(shutdownCtx)
}

func TestManager_LogFiles(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	mgr := New(5 * time.Second)
	specs := []api.ServiceSpec{
		{Name: "echoer", Command: []string{"sh", "-c", "echo hello-stdout; echo hello-stderr >&2"}},
	}

	ctx := context.Background()
	if err := mgr.Start(ctx, specs, 0, 0, "", false); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Wait for process to finish
	time.Sleep(1 * time.Second)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = mgr.Shutdown(shutdownCtx)

	home := os.Getenv("HOME")
	logDir := filepath.Join(home, ".scion", "services", "logs")

	// Check stdout log
	stdoutData, err := os.ReadFile(filepath.Join(logDir, "echoer.stdout.log"))
	if err != nil {
		t.Fatalf("failed to read stdout log: %v", err)
	}
	if !strings.Contains(string(stdoutData), "hello-stdout") {
		t.Errorf("stdout log missing expected content, got: %q", string(stdoutData))
	}

	// Check stderr log
	stderrData, err := os.ReadFile(filepath.Join(logDir, "echoer.stderr.log"))
	if err != nil {
		t.Fatalf("failed to read stderr log: %v", err)
	}
	if !strings.Contains(string(stderrData), "hello-stderr") {
		t.Errorf("stderr log missing expected content, got: %q", string(stderrData))
	}

	// Check lifecycle log exists and has entries
	lifecycleData, err := os.ReadFile(filepath.Join(logDir, "echoer.lifecycle.log"))
	if err != nil {
		t.Fatalf("failed to read lifecycle log: %v", err)
	}
	if !strings.Contains(string(lifecycleData), "Service started") {
		t.Errorf("lifecycle log missing 'Service started', got: %q", string(lifecycleData))
	}
	if !strings.Contains(string(lifecycleData), "Service exited") {
		t.Errorf("lifecycle log missing 'Service exited', got: %q", string(lifecycleData))
	}
}

func TestManager_ServiceEnv(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	mgr := New(5 * time.Second)
	specs := []api.ServiceSpec{
		{
			Name:    "env-printer",
			Command: []string{"sh", "-c", "echo MY_CUSTOM_VAR=$MY_CUSTOM_VAR"},
			Env:     map[string]string{"MY_CUSTOM_VAR": "test-value-123"},
		},
	}

	ctx := context.Background()
	if err := mgr.Start(ctx, specs, 0, 0, "", false); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	time.Sleep(1 * time.Second)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = mgr.Shutdown(shutdownCtx)

	home := os.Getenv("HOME")
	logDir := filepath.Join(home, ".scion", "services", "logs")

	stdoutData, err := os.ReadFile(filepath.Join(logDir, "env-printer.stdout.log"))
	if err != nil {
		t.Fatalf("failed to read stdout log: %v", err)
	}
	if !strings.Contains(string(stdoutData), "MY_CUSTOM_VAR=test-value-123") {
		t.Errorf("expected env var in output, got: %q", string(stdoutData))
	}
}

func TestManager_StartOrder(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	mgr := New(5 * time.Second)

	specs := []api.ServiceSpec{
		{Name: "first", Command: []string{"sleep", "60"}},
		{Name: "second", Command: []string{"sleep", "60"}},
		{Name: "third", Command: []string{"sleep", "60"}},
	}

	ctx := context.Background()
	if err := mgr.Start(ctx, specs, 0, 0, "", false); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Verify services were started in order by checking lifecycle logs.
	// Lifecycle log entries are written synchronously before moving to the
	// next service, so the ordering is deterministic.
	home := os.Getenv("HOME")
	logDir := filepath.Join(home, ".scion", "services", "logs")

	for _, name := range []string{"first", "second", "third"} {
		logFile := filepath.Join(logDir, name+".lifecycle.log")
		data, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatalf("failed to read lifecycle log for %s: %v", name, err)
		}
		if !strings.Contains(string(data), "Service started") {
			t.Errorf("service %s lifecycle log missing 'Service started'", name)
		}
	}

	// Verify internal service order matches spec order
	mgr.mu.Lock()
	if len(mgr.services) != 3 {
		t.Fatalf("expected 3 services, got %d", len(mgr.services))
	}
	for i, name := range []string{"first", "second", "third"} {
		if mgr.services[i].spec.Name != name {
			t.Errorf("service at index %d: expected %q, got %q", i, name, mgr.services[i].spec.Name)
		}
	}
	mgr.mu.Unlock()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = mgr.Shutdown(shutdownCtx)
}

func TestMergeEnv(t *testing.T) {
	parent := []string{"FOO=bar", "PATH=/usr/bin"}
	serviceEnv := map[string]string{
		"FOO":    "override",
		"CUSTOM": "value",
	}
	result := mergeEnv(parent, serviceEnv, 0, "")

	found := map[string]string{}
	for _, e := range result {
		parts := strings.SplitN(e, "=", 2)
		found[parts[0]] = parts[1]
	}

	if found["FOO"] != "override" {
		t.Errorf("expected FOO=override, got FOO=%s", found["FOO"])
	}
	if found["PATH"] != "/usr/bin" {
		t.Errorf("expected PATH=/usr/bin, got PATH=%s", found["PATH"])
	}
	if found["CUSTOM"] != "value" {
		t.Errorf("expected CUSTOM=value, got CUSTOM=%s", found["CUSTOM"])
	}
}

var startProcreapReaperOnce sync.Once

// TestManagedService_StartSurvivesReaperRace is the regression test for the
// services-manager finding from code review: managedService.start() spawns
// cmd.Wait() in a background goroutine, exactly like
// supervisor.Supervisor.Run, so it needed the same gated Start()+Register
// and Unregister-after-Wait treatment to avoid sciontool init's SIGCHLD
// reaper racing that Wait call and corrupting the recorded exit code (the
// race would surface here as a fast, successful "true" service getting
// recorded with exitCode 1 instead of 0, since managedService.start's
// Wait goroutine maps a non-ExitError Wait failure to exitCode=1).
//
// A real, live procreap reaper runs for the process for this to be a
// faithful reproduction — a fake or absent reaper can't race anything.
func TestManagedService_StartSurvivesReaperRace(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	startProcreapReaperOnce.Do(procreap.StartReaper)

	logDir := t.TempDir()
	const iterations = 100

	logDirFd, err := syscall.Open(logDir, syscall.O_DIRECTORY|syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Close(logDirFd) }()

	var wg sync.WaitGroup
	results := make([]int, iterations)
	errs := make([]error, iterations)
	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			svc := &managedService{
				spec:   api.ServiceSpec{Name: fmt.Sprintf("svc-%d", i), Command: []string{"true"}},
				logDir: logDir,
				done:   make(chan struct{}),
				env:    os.Environ(),
			}
			if err := svc.openLogs(logDirFd, false); err != nil {
				errs[i] = fmt.Errorf("openLogs: %w", err)
				return
			}
			defer svc.closeLogs()
			if err := svc.start(); err != nil {
				errs[i] = fmt.Errorf("start: %w", err)
				return
			}
			<-svc.snapshotDone()
			results[i] = svc.snapshotExitCode()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("service %d: %v", i, err)
		}
	}
	for i, code := range results {
		if code != 0 {
			t.Errorf("service %d: exit code = %d, want 0 (a non-zero code here means the reaper stole "+
				"this service's exit status)", i, code)
		}
	}
}

// TestOpenLogs_RefusesPreplantedSymlink is the core deterministic
// regression test: a symlink already sitting at a service's log path
// (planted by a scion-uid process during the window root spends blocked on
// an earlier service's ReadyCheck) must never be opened through —
// os.OpenFile's O_CREATE|O_APPEND (no O_NOFOLLOW) would follow it and
// create/append to the target as root. dirfd.OpenAt with O_NOFOLLOW here
// refuses it outright instead.
func TestOpenLogs_RefusesPreplantedSymlink(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	home := os.Getenv("HOME")
	logDir := filepath.Join(home, ".scion", "services", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatal(err)
	}

	victim := t.TempDir()
	victimTarget := filepath.Join(victim, "victim.log")
	link := filepath.Join(logDir, "evil.stdout.log")
	if err := os.Symlink(victimTarget, link); err != nil {
		t.Fatal(err)
	}

	logDirFd, err := syscall.Open(logDir, syscall.O_DIRECTORY|syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Close(logDirFd) }()

	svc := &managedService{spec: api.ServiceSpec{Name: "evil"}, logDir: logDir}
	if err := svc.openLogs(logDirFd, false); err == nil {
		t.Fatal("expected openLogs to refuse the pre-planted symlink, got nil error")
	}

	if _, err := os.Stat(victimTarget); !os.IsNotExist(err) {
		t.Errorf("victim target must not have been created through the symlink, stat err=%v", err)
	}
	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Error("expected the log path to still be the symlink the test planted")
	}
}

// TestManager_Start_DropsOnlyTheServiceWithASymlinkedLogPath is the
// call-site-level regression test: a symlink planted at one service's log
// path must never be opened/created/appended through — but it must also not
// prevent any OTHER service (including ones later in specs) from starting.
// An all-or-nothing policy here would hand a workload process a
// denial-of-service lever against every sidecar merely by planting one
// symlink, which contradicts the "a planted symlink must not be able to
// stop the workload from starting" principle applied elsewhere
// (see cmd/sciontool/commands/init.go's own hardening).
func TestManager_Start_DropsOnlyTheServiceWithASymlinkedLogPath(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	home := os.Getenv("HOME")
	logDir := filepath.Join(home, ".scion", "services", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatal(err)
	}
	victim := t.TempDir()
	victimTarget := filepath.Join(victim, "victim.log")
	link := filepath.Join(logDir, "second.stdout.log")
	if err := os.Symlink(victimTarget, link); err != nil {
		t.Fatal(err)
	}

	mgr := New(5 * time.Second)
	specs := []api.ServiceSpec{
		{Name: "first", Command: []string{"sleep", "60"}},
		{Name: "second", Command: []string{"sleep", "60"}},
		{Name: "third", Command: []string{"sleep", "60"}},
	}

	err := mgr.Start(context.Background(), specs, 0, 0, "", false)
	if err == nil {
		t.Fatal("expected Start to report an error for the symlinked log path")
	}

	if _, statErr := os.Stat(victimTarget); !os.IsNotExist(statErr) {
		t.Errorf("victim target must not have been created through the symlink, stat err=%v", statErr)
	}

	mgr.mu.Lock()
	started := make([]string, len(mgr.services))
	for i, svc := range mgr.services {
		started[i] = svc.spec.Name
	}
	mgr.mu.Unlock()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mgr.Shutdown(shutdownCtx)
	}()

	if len(started) != 2 || started[0] != "first" || started[1] != "third" {
		t.Fatalf("started services = %v, want [first third] — only \"second\" (the one with the symlinked log) should be dropped", started)
	}
}

// TestOpenLogs_Enforced_RefusesHardlinkedLogPath proves the hard-link guard
// on every one of openLogs' three call sites: a pre-planted hard
// link to an unrelated regular file at a log path is refused when
// requirePrivilegeDrop is true. Each subtest hard-links exactly ONE of the
// three paths (the other two are fresh), so each subtest fails if and only
// if that one call site stops forwarding requirePrivilegeDrop as checkNlink,
// exercised across all three log streams, not just stdout.
func TestOpenLogs_Enforced_RefusesHardlinkedLogPath(t *testing.T) {
	for _, suffix := range []string{".stdout.log", ".stderr.log", ".lifecycle.log"} {
		t.Run(suffix, func(t *testing.T) {
			cleanup := setupTestEnv(t)
			defer cleanup()

			home := os.Getenv("HOME")
			logDir := filepath.Join(home, ".scion", "services", "logs")
			if err := os.MkdirAll(logDir, 0755); err != nil {
				t.Fatal(err)
			}

			target := filepath.Join(logDir, "unrelated-target")
			if err := os.WriteFile(target, []byte("existing content"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(target, filepath.Join(logDir, "evil"+suffix)); err != nil {
				t.Fatal(err)
			}

			logDirFd, err := syscall.Open(logDir, syscall.O_DIRECTORY|syscall.O_RDONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = syscall.Close(logDirFd) }()

			svc := &managedService{spec: api.ServiceSpec{Name: "evil"}, logDir: logDir}
			err = svc.openLogs(logDirFd, true)
			svc.closeLogs()
			if err == nil {
				t.Fatalf("expected openLogs to refuse the hard-linked evil%s in enforced mode, got nil", suffix)
			}
			if !strings.Contains(err.Error(), "hard-linked") || !strings.Contains(err.Error(), "evil"+suffix) {
				t.Errorf("expected the hard-link refusal for evil%s, got: %v", suffix, err)
			}
		})
	}
}

// TestManager_Start_Enforced_DropsServiceWithHardlinkedLogPath proves that
// Manager.Start itself forwards requirePrivilegeDrop to openLogs: with
// requirePrivilegeDrop=true, a service whose <name>.stdout.log is
// a pre-planted hard link to a victim file is dropped (never started), the
// victim's content and size are unchanged, and a sibling service still
// starts. The enforced=false twin below proves the fixture itself is not
// what refuses the service.
func TestManager_Start_Enforced_DropsServiceWithHardlinkedLogPath(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires CAP_SETGID (root) for the sibling service's own Credential drop; os/exec calls setgroups(2) whenever SysProcAttr.Credential is set, even to a uid>0 target — see TestManager_Start_EnforcedRefusesUndroppableCredentials for the unprivileged-safe equivalent of this test's enforced-mode refusal")
	}
	cleanup := setupTestEnv(t)
	defer cleanup()

	home := os.Getenv("HOME")
	logDir := filepath.Join(home, ".scion", "services", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(home, "victim")
	if err := os.WriteFile(victim, []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(victim, filepath.Join(logDir, "evil.stdout.log")); err != nil {
		t.Fatal(err)
	}

	mgr := New(5 * time.Second)
	specs := []api.ServiceSpec{
		{Name: "evil", Command: []string{"sh", "-c", "echo PWNED"}},
		{Name: "ok", Command: []string{"sleep", "60"}},
	}
	// uid/gid are a real, non-zero target (this test requires root, via
	// the Geteuid check above) rather than 0: with requirePrivilegeDrop=true,
	// uid/gid<=0 refuses to start ANY service (see
	// TestManager_Start_EnforcedRefusesUndroppableCredentials), which would
	// mask the hard-link refusal this test is actually about.
	const dropUID, dropGID = 1000, 1000
	err := mgr.Start(context.Background(), specs, dropUID, dropGID, "", true)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mgr.Shutdown(shutdownCtx)
	}()
	if err == nil || !strings.Contains(err.Error(), "hard-linked") {
		t.Errorf("expected Start to report the hard-linked log refusal, got: %v", err)
	}

	mgr.mu.Lock()
	started := make([]string, len(mgr.services))
	for i, svc := range mgr.services {
		started[i] = svc.spec.Name
	}
	mgr.mu.Unlock()
	if len(started) != 1 || started[0] != "ok" {
		t.Fatalf("started services = %v, want [ok]: the service with the hard-linked log must be dropped in enforced mode", started)
	}

	got, rerr := os.ReadFile(victim)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != "v" {
		t.Errorf("victim content = %q, want %q (unchanged)", got, "v")
	}
}

// TestManager_Start_EnforcedRefusesUndroppableCredentials proves that
// managedService.start's own Credential-decision guard — not just the
// caller-side requirePrivilegeDropOrFail clamp one layer up — refuses to
// start a service whose uid/gid do not both pass the Credential predicate
// when requirePrivilegeDrop is set, instead of silently starting it with no
// Credential (i.e. as whatever this process is, root in production). The
// marker file's absence proves the command was never exec'd.
func TestManager_Start_EnforcedRefusesUndroppableCredentials(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	cases := []struct {
		name     string
		uid, gid int
	}{
		{name: "uid0", uid: 0, gid: 1000},
		{name: "gid0", uid: 1000, gid: 0},
		{name: "both0", uid: 0, gid: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			mgr := New(5 * time.Second)
			specs := []api.ServiceSpec{
				{Name: "svc", Command: []string{"sh", "-c", "touch " + marker}},
			}
			err := mgr.Start(context.Background(), specs, tc.uid, tc.gid, "", true)
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = mgr.Shutdown(shutdownCtx)
			}()
			if !errors.Is(err, ErrPrivilegeDropRequired) {
				t.Errorf("Start(uid=%d, gid=%d, requirePrivilegeDrop) err = %v, want ErrPrivilegeDropRequired", tc.uid, tc.gid, err)
			}
			// Give a wrongly-started process a moment to create the marker
			// before asserting its absence.
			time.Sleep(50 * time.Millisecond)
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Errorf("the service ran (marker exists, stat err=%v); it must not start without a droppable credential in enforced mode", statErr)
			}
		})
	}
}

// TestManager_Start_NonEnforced_StartsServiceWithHardlinkedLogPath is the
// twin of the test above: the identical fixture with
// requirePrivilegeDrop=false starts the service, proving the enforced test's
// refusal comes from the forwarded flag, not from something else in the
// fixture.
func TestManager_Start_NonEnforced_StartsServiceWithHardlinkedLogPath(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	home := os.Getenv("HOME")
	logDir := filepath.Join(home, ".scion", "services", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(home, "shared")
	if err := os.WriteFile(shared, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(shared, filepath.Join(logDir, "linked.stdout.log")); err != nil {
		t.Fatal(err)
	}

	mgr := New(5 * time.Second)
	if err := mgr.Start(context.Background(), []api.ServiceSpec{{Name: "linked", Command: []string{"sleep", "60"}}}, 0, 0, "", false); err != nil {
		t.Fatalf("non-enforced Start: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mgr.Shutdown(shutdownCtx)
	}()
	mgr.mu.Lock()
	n := len(mgr.services)
	mgr.mu.Unlock()
	if n != 1 {
		t.Fatalf("started %d services, want 1 (non-enforced mode must accept a hard-linked log)", n)
	}
}

// TestOpenLogs_NonEnforced_AllowsHardlinkedLogPath proves the gating: a
// hard-linked log path is NOT refused when requirePrivilegeDrop is false —
// a legitimately hard-linked log file under a non-substrate container must
// keep working exactly as it did before the hard-link guard existed.
func TestOpenLogs_NonEnforced_AllowsHardlinkedLogPath(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	home := os.Getenv("HOME")
	logDir := filepath.Join(home, ".scion", "services", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(logDir, "unrelated-target")
	if err := os.WriteFile(target, []byte("existing content"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(logDir, "ok.stdout.log")
	if err := os.Link(target, link); err != nil {
		t.Fatal(err)
	}

	logDirFd, err := syscall.Open(logDir, syscall.O_DIRECTORY|syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Close(logDirFd) }()

	svc := &managedService{spec: api.ServiceSpec{Name: "ok"}, logDir: logDir}
	if err := svc.openLogs(logDirFd, false); err != nil {
		t.Fatalf("expected non-enforced mode to allow a hard-linked log path, got: %v", err)
	}
	svc.closeLogs()
}

// countLogDirFds returns the number of this process's currently open file
// descriptors that refer to a path inside logDir, by reading
// /proc/self/fd and resolving each entry's symlink target.
func countLogDirFds(t *testing.T, logDir string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("ReadDir /proc/self/fd: %v", err)
	}
	count := 0
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err != nil {
			continue // fd closed between ReadDir and Readlink; not ours
		}
		if strings.HasPrefix(target, logDir+"/") {
			count++
		}
	}
	return count
}

// TestManager_Start_NoFdLeakOnPartialOpenOrStartFailure is the core
// regression test for the two fd-leak paths: closing a service's own
// partial fds when its own log-open fails, and closing the fds of every
// service that never gets a chance to start because an earlier one's
// start() call failed. Neither path is exercised by
// TestManager_Start_DropsOnlyTheServiceWithASymlinkedLogPath, which plants
// its symlink at the FIRST file openLogs opens (so no partial-open fd ever
// exists) and never drives a start() failure.
//
// "second"'s stderr log is a planted symlink, so its stdout opens fine and
// its stderr fails — a genuine partial-open. "bad" names a nonexistent
// binary, so its logs open fine but its start() fails, which must close
// both "bad"'s own fds and "fourth"'s (which never got a chance to start).
// Only "first" should have any fds left open under logDir once Start
// returns, and none once Shutdown completes.
func TestManager_Start_NoFdLeakOnPartialOpenOrStartFailure(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires CAP_SETGID (root) for \"first\"'s and \"warmup\"'s own Credential drop; os/exec calls setgroups(2) whenever SysProcAttr.Credential is set, even to a uid>0 target — see TestManager_Start_EnforcedRefusesUndroppableCredentials for the unprivileged-safe equivalent of the enforced-mode refusal this test's fixture also exercises")
	}
	cleanup := setupTestEnv(t)
	defer cleanup()

	// A real, non-zero target (this test requires root, via the
	// Geteuid check above): with requirePrivilegeDrop=true, uid/gid<=0
	// refuses to start ANY service (see
	// TestManager_Start_EnforcedRefusesUndroppableCredentials), which would
	// prevent "first"/"warmup" from starting at all and change this test's
	// fd-count assertions.
	const dropUID, dropGID = 1000, 1000

	home := os.Getenv("HOME")
	logDir := filepath.Join(home, ".scion", "services", "logs")

	// Warmup: absorb the logger's own lazy agent.log open (a one-time fd
	// this package's log calls create, unrelated to service log fds) and
	// establish logDir on disk before the real test measures fd deltas.
	warmup := New(5 * time.Second)
	if err := warmup.Start(context.Background(), []api.ServiceSpec{{Name: "warmup", Command: []string{"true"}}}, dropUID, dropGID, "", true); err != nil {
		t.Fatalf("warmup Start: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	warmupCtx, warmupCancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = warmup.Shutdown(warmupCtx)
	warmupCancel()

	victim := t.TempDir()
	if err := os.Symlink(filepath.Join(victim, "victim.log"), filepath.Join(logDir, "second.stderr.log")); err != nil {
		t.Fatal(err)
	}

	// openLogNoFollow's OWN refusal paths (not just the caller's) must close
	// the fd they opened. "fifo"'s stderr log is a FIFO with a
	// reader attached (so the write-side open succeeds and only the S_IFREG
	// check refuses it); "linked"'s stderr log is a hard link (refused only
	// by the enforced Nlink check). Their reader fd is opened before the
	// baseline is taken, so it cancels out of the delta.
	fifoPath := filepath.Join(logDir, "fifo.stderr.log")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatal(err)
	}
	rfd, err := syscall.Open(fifoPath, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open FIFO reader: %v", err)
	}
	defer func() { _ = syscall.Close(rfd) }()
	linkTarget := filepath.Join(logDir, "link-target")
	if err := os.WriteFile(linkTarget, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(linkTarget, filepath.Join(logDir, "linked.stderr.log")); err != nil {
		t.Fatal(err)
	}

	specs := []api.ServiceSpec{
		{Name: "first", Command: []string{"sleep", "60"}},
		{Name: "second", Command: []string{"sleep", "60"}},
		{Name: "fifo", Command: []string{"sleep", "60"}},
		{Name: "linked", Command: []string{"sleep", "60"}},
		{Name: "bad", Command: []string{"/nonexistent/binary-for-fd-leak-test"}},
		{Name: "fourth", Command: []string{"sleep", "60"}},
	}

	before := countLogDirFds(t, logDir)

	mgr := New(5 * time.Second)
	_ = mgr.Start(context.Background(), specs, dropUID, dropGID, "", true) // error expected; fds are what this checks

	afterStart := countLogDirFds(t, logDir)
	if delta := afterStart - before; delta != 3 {
		t.Errorf("open fds under logDir after Start = %d (delta %d), want delta 3 (only \"first\"'s stdout+stderr+lifecycle)", afterStart, delta)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = mgr.Shutdown(shutdownCtx)
	cancel()

	afterShutdown := countLogDirFds(t, logDir)
	if afterShutdown != before {
		t.Errorf("open fds under logDir after Shutdown = %d, want %d (back to baseline, no leak)", afterShutdown, before)
	}
}

// TestOpenLogNoFollow_RefusesFifoWithoutBlocking proves the
// S_IFREG check in openLogNoFollow refuses a FIFO planted at a log path —
// even one with a reader already attached, so open(2) itself would
// otherwise succeed immediately were it not for the S_IFREG check —
// rather than silently writing into a pipe nothing reads from correctly.
//
// The reader is attached SYNCHRONOUSLY, in the test body, before
// openLogNoFollow is ever called — not in a background goroutine racing
// against it. A non-blocking read-open of a FIFO succeeds immediately
// (there is no reader-arrival race to win or lose), so by the time
// openLogNoFollow runs, the write-side open() is guaranteed to succeed on
// the FIFO itself: with no reader, the write-side open would instead fail
// with ENXIO, which also satisfies a bare err!=nil check and would make
// this test pass regardless of whether the S_IFREG check exists at all —
// a race between a reader-attach goroutine and the write-side open, with
// no synchronisation, would let the write side win almost every time,
// exercising the ENXIO path instead of S_IFREG.
func TestOpenLogNoFollow_RefusesFifoWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "evil.stdout.log")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatal(err)
	}

	rfd, err := syscall.Open(fifoPath, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(rfd) })

	dirFd, err := syscall.Open(dir, syscall.O_DIRECTORY|syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Close(dirFd) }()

	done := make(chan error, 1)
	go func() {
		flags := syscall.O_APPEND | syscall.O_CREAT | syscall.O_WRONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
		f, err := openLogNoFollow(dirFd, "evil.stdout.log", flags, 0644, false)
		if f != nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error refusing the FIFO, got nil")
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("expected the S_IFREG refusal message, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("openLogNoFollow blocked on a FIFO instead of refusing it")
	}
}

// TestValidateServiceName covers the rejection table for the service-Name-
// as-path-component content-trust class: a workload-writable
// scion-services.yaml can set Name to anything, and openLogs builds a path
// by simple string concatenation (Name + ".stdout.log", passed straight to
// an openat(2) that does not split on "/"), so a Name containing ".." or a
// path separator can escape the log directory entirely.
func TestValidateServiceName(t *testing.T) {
	tests := []struct {
		name    string
		valid   bool
		wantErr bool
	}{
		{name: "chrome", valid: true},
		{name: "vnc-server_2", valid: true},
		{name: "../escape", wantErr: true},
		{name: "a/b", wantErr: true},
		{name: "", wantErr: true},
		{name: ".", wantErr: true},
		{name: "..", wantErr: true},
		{name: "has\x00nul", wantErr: true},
		{name: "a\nb", wantErr: true},     // newline: log-line-forging vector
		{name: "a\x1bb", wantErr: true},   // ESC: terminal/log-escape-sequence vector
		{name: "a\x7fb", wantErr: true},   // DEL: outside C0, still a control character
		{name: "a\u0085b", wantErr: true}, // NEL (C1): a line break to some log consumers
		{name: "a\u009bb", wantErr: true}, // CSI (C1): 8-bit escape introducer on some terminals
		{name: strings.Repeat("x", maxServiceNameLen+1), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.name), func(t *testing.T) {
			err := ValidateServiceName(tt.name)
			if tt.valid && err != nil {
				t.Errorf("ValidateServiceName(%q) = %v, want nil", tt.name, err)
			}
			if tt.wantErr && err == nil {
				t.Errorf("ValidateServiceName(%q) = nil, want an error", tt.name)
			}
			if tt.wantErr && !errors.Is(err, ErrInvalidServiceName) {
				t.Errorf("ValidateServiceName(%q) error %v does not wrap ErrInvalidServiceName", tt.name, err)
			}
		})
	}
}

func TestSafeNameForLog_TruncatesAndEscapes(t *testing.T) {
	got := SafeNameForLog("line1\nline2")
	if strings.Contains(got, "\n") {
		t.Errorf("SafeNameForLog left a literal newline in %q — log injection risk", got)
	}
	long := strings.Repeat("x", 200)
	got2 := SafeNameForLog(long)
	if len(got2) > 100 {
		t.Errorf("SafeNameForLog did not bound the output length: len=%d", len(got2))
	}
}

// TestManager_Start_DropsInvalidNamesButStartsOthers is the core
// regression test: a table of invalid Names, each dropped without creating
// anything outside logDir, alongside a normal Name that still starts and
// opens its three logs normally. Without ValidateServiceName's call in
// Start, the "../escape" case would create a file outside logDir.
func TestManager_Start_DropsInvalidNamesButStartsOthers(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	home := os.Getenv("HOME")
	escapeTarget := filepath.Join(home, ".scion", "services", "escape.stdout.log")

	specs := []api.ServiceSpec{
		{Name: "../escape", Command: []string{"true"}},
		{Name: "a/b", Command: []string{"true"}},
		{Name: "", Command: []string{"true"}},
		{Name: ".", Command: []string{"true"}},
		{Name: "..", Command: []string{"true"}},
		{Name: "chrome", Command: []string{"sleep", "60"}},
	}

	mgr := New(5 * time.Second)
	err := mgr.Start(context.Background(), specs, 0, 0, "", false)
	if err == nil {
		t.Fatal("expected Start to report errors for the invalid names")
	}

	if _, statErr := os.Stat(escapeTarget); !os.IsNotExist(statErr) {
		t.Errorf("\"../escape\" must not have created a file outside logDir, stat err=%v", statErr)
	}

	mgr.mu.Lock()
	started := make([]string, len(mgr.services))
	for i, svc := range mgr.services {
		started[i] = svc.spec.Name
	}
	mgr.mu.Unlock()
	if len(started) != 1 || started[0] != "chrome" {
		t.Fatalf("started services = %v, want [chrome] — only the valid name should start", started)
	}

	logDir := filepath.Join(home, ".scion", "services", "logs")
	for _, suffix := range []string{"stdout.log", "stderr.log", "lifecycle.log"} {
		if _, err := os.Stat(filepath.Join(logDir, "chrome."+suffix)); err != nil {
			t.Errorf("expected chrome.%s to exist: %v", suffix, err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = mgr.Shutdown(shutdownCtx)
}

// TestWriteLifecycle_TimestampIsUTC checks that service lifecycle log lines
// carry the same UTC RFC 3339 timestamp as agent.log, whatever the process
// zone.
func TestWriteLifecycle_TimestampIsUTC(t *testing.T) {
	origLocal := time.Local
	time.Local = time.FixedZone("JST", 9*60*60)
	defer func() { time.Local = origLocal }()

	path := filepath.Join(t.TempDir(), "svc.lifecycle.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	svc := &managedService{lifecycleFile: f}
	svc.writeLifecycle("Service started (pid %d)", 42)
	_ = f.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	line := strings.TrimSuffix(string(data), "\n")
	if !strings.HasPrefix(line, "[") {
		t.Fatalf("lifecycle line = %q, want a bracketed timestamp", line)
	}
	ts, rest, ok := strings.Cut(line[1:], "] ")
	if !ok {
		t.Fatalf("lifecycle line = %q, want \"[<timestamp>] <message>\"", line)
	}
	if rest != "Service started (pid 42)" {
		t.Errorf("lifecycle message = %q, want %q", rest, "Service started (pid 42)")
	}
	if !strings.HasSuffix(ts, "Z") {
		t.Errorf("lifecycle timestamp %q is not UTC (want a trailing Z)", ts)
	}
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Errorf("lifecycle timestamp %q is not RFC 3339: %v", ts, err)
	}
}
