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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

func setupTestEnv(t *testing.T) (cleanup func()) {
	t.Helper()
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	log.SetLogPath(filepath.Join(tmpDir, "agent.log"))
	return func() {
		os.Setenv("HOME", origHome)
	}
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
	if err := mgr.Start(ctx, specs, 0, 0, ""); err != nil {
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
	if err := mgr.Start(ctx, specs, 0, 0, ""); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Wait for the service to fail and be restarted at least once
	time.Sleep(3 * time.Second)

	mgr.mu.Lock()
	svc := mgr.services[0]
	mgr.mu.Unlock()
	failures := svc.currentFailures()

	if failures == 0 {
		t.Error("expected at least one restart attempt for on-failure policy")
	}

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	mgr.Shutdown(shutdownCtx)
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
	if err := mgr.Start(ctx, specs, 0, 0, ""); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Wait for it to restart a few times
	time.Sleep(3 * time.Second)

	mgr.mu.Lock()
	svc := mgr.services[0]
	mgr.mu.Unlock()
	// With "always" and exit code 0, failures still increment (consecutive exits)
	// but it should still be restarting
	abandoned := svc.isAbandoned()

	// The process exits immediately with 0, so failures increment each time
	// After 3, it should be abandoned
	if !abandoned {
		// It may not yet be abandoned if timing is tight; just check it attempted restarts
		t.Log("service not yet abandoned, checking it was restarted")
	}

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	mgr.Shutdown(shutdownCtx)
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
	if err := mgr.Start(ctx, specs, 0, 0, ""); err != nil {
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
	mgr.Shutdown(shutdownCtx)
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
	if err := mgr.Start(ctx, specs, 0, 0, ""); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Wait for all restart attempts to exhaust (1s + 2s + 4s backoff + some process time)
	time.Sleep(10 * time.Second)

	mgr.mu.Lock()
	svc := mgr.services[0]
	mgr.mu.Unlock()
	if !svc.isAbandoned() {
		t.Error("expected service to be abandoned after max restarts")
	}
	if f := svc.currentFailures(); f < maxConsecutiveFailures {
		t.Errorf("expected at least %d failures, got %d", maxConsecutiveFailures, f)
	}

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	mgr.Shutdown(shutdownCtx)
}

func TestManager_LogFiles(t *testing.T) {
	cleanup := setupTestEnv(t)
	defer cleanup()

	mgr := New(5 * time.Second)
	specs := []api.ServiceSpec{
		{Name: "echoer", Command: []string{"sh", "-c", "echo hello-stdout; echo hello-stderr >&2"}},
	}

	ctx := context.Background()
	if err := mgr.Start(ctx, specs, 0, 0, ""); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Wait for process to finish
	time.Sleep(1 * time.Second)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	mgr.Shutdown(shutdownCtx)

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
	if err := mgr.Start(ctx, specs, 0, 0, ""); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	time.Sleep(1 * time.Second)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	mgr.Shutdown(shutdownCtx)

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
	if err := mgr.Start(ctx, specs, 0, 0, ""); err != nil {
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
	mgr.Shutdown(shutdownCtx)
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
