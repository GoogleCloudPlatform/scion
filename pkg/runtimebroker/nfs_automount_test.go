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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// syncMountChecker is a concurrency-safe MountChecker fake for tests that
// run the reconciler in its background loop. Mount can be made to block
// until release is closed.
type syncMountChecker struct {
	mu          sync.Mutex
	mountpoints map[string]string
	mountErr    error
	mounts      int
	mkdirs      int
	unmounts    int
	block       chan struct{} // if non-nil, Mount waits for it to close
	mountEnter  chan struct{} // if non-nil, signalled when Mount is entered
}

func newSyncMountChecker() *syncMountChecker {
	return &syncMountChecker{mountpoints: map[string]string{}}
}

func (m *syncMountChecker) setMounted(path, source string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mountpoints[path] = source
}

func (m *syncMountChecker) counts() (mounts, mkdirs, unmounts int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounts, m.mkdirs, m.unmounts
}

func (m *syncMountChecker) IsMountpoint(path string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.mountpoints[path]
	return ok, nil
}

func (m *syncMountChecker) MountInfo(path string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mountpoints[path], nil
}

func (m *syncMountChecker) Mount(server, export, target, options string) error {
	m.mu.Lock()
	m.mounts++
	block, enter, mountErr := m.block, m.mountEnter, m.mountErr
	m.mu.Unlock()
	if enter != nil {
		select {
		case enter <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	if mountErr != nil {
		return mountErr
	}
	m.setMounted(target, server+":"+export)
	return nil
}

func (m *syncMountChecker) Unmount(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unmounts++
	delete(m.mountpoints, target)
	return nil
}

func (m *syncMountChecker) MkdirAll(path string, perm os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mkdirs++
	return nil
}

func nfsCfg(autoMount bool) *config.V1NFSConfig {
	return &config.V1NFSConfig{
		MountRoot: "/mnt/nfs",
		AutoMount: autoMount,
		Shares: []config.V1NFSShare{
			{ID: "ws1", Server: "10.0.0.2", Export: "/scion-workspaces"},
		},
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// --- Reconciler modes ---

func TestReconcile_CheckOnly_NotMounted_NoMountAttempt(t *testing.T) {
	mc := newMockMountChecker()
	r := NewNFSMountReconciler(nfsCfg(false), mc, nil)

	if err := r.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(mc.mountCalls) != 0 || len(mc.mkdirCalls) != 0 || len(mc.unmountCalls) != 0 {
		t.Fatalf("check-only mode changed the system: mounts=%d mkdirs=%d unmounts=%d",
			len(mc.mountCalls), len(mc.mkdirCalls), len(mc.unmountCalls))
	}
	if r.IsHealthy() {
		t.Fatal("expected unhealthy when the share is not mounted")
	}
	if hc := r.HealthCheckString(); !strings.Contains(hc, "not mounted") || !strings.Contains(hc, "auto_mount is off") {
		t.Errorf("HealthCheckString = %q, want it to say not mounted and auto_mount is off", hc)
	}
}

func TestReconcile_CheckOnly_MountedCorrectly_Healthy(t *testing.T) {
	mc := newMockMountChecker()
	mc.mountpoints[filepath.Join("/mnt/nfs", "ws1")] = "10.0.0.2:/scion-workspaces"
	r := NewNFSMountReconciler(nfsCfg(false), mc, nil)

	_ = r.Reconcile()
	if !r.IsHealthy() {
		t.Fatalf("expected healthy, got %q", r.HealthCheckString())
	}
	if len(mc.mountCalls) != 0 {
		t.Errorf("mountCalls = %d, want 0", len(mc.mountCalls))
	}
}

func TestReconcile_CheckOnly_WrongSource_NoRemount(t *testing.T) {
	mc := newMockMountChecker()
	mc.mountpoints[filepath.Join("/mnt/nfs", "ws1")] = "10.9.9.9:/other"
	r := NewNFSMountReconciler(nfsCfg(false), mc, nil)

	_ = r.Reconcile()
	if r.IsHealthy() {
		t.Fatal("expected unhealthy for a share mounted from the wrong source")
	}
	if len(mc.unmountCalls) != 0 || len(mc.mountCalls) != 0 {
		t.Errorf("check-only mode remounted: unmounts=%d mounts=%d", len(mc.unmountCalls), len(mc.mountCalls))
	}
	if hc := r.HealthCheckString(); !strings.Contains(hc, "10.9.9.9:/other") {
		t.Errorf("HealthCheckString = %q, want the actual source named", hc)
	}
}

func TestEnsureNFSMountsReady_AutoMountOff_NoGate(t *testing.T) {
	mc := newMockMountChecker()
	cfg := nfsCfg(false)
	srv := &Server{
		config:             ServerConfig{NFSConfig: cfg},
		nfsMountReconciler: NewNFSMountReconciler(cfg, mc, nil),
	}
	if err := srv.ensureNFSMountsReady(); err != nil {
		t.Fatalf("ensureNFSMountsReady with auto_mount off = %v, want nil", err)
	}
	if len(mc.mountCalls) != 0 {
		t.Errorf("mountCalls = %d, want 0", len(mc.mountCalls))
	}
}

// --- Server wiring ---

func TestServer_New_UsesInjectedMountChecker(t *testing.T) {
	mc := newSyncMountChecker()
	srv := New(ServerConfig{Host: "127.0.0.1", NFSConfig: nfsCfg(true), NFSMountChecker: mc}, nil, nil)
	if srv.nfsMountReconciler == nil {
		t.Fatal("expected reconciler")
	}
	if srv.nfsMountReconciler.checker != mc {
		t.Fatal("expected the injected NFSMountChecker to be used")
	}
}

func TestServer_New_NoNFSConfig_NoReconcileLoop(t *testing.T) {
	srv := New(ServerConfig{Host: "127.0.0.1"}, nil, nil)
	if srv.nfsMountReconciler != nil || srv.nfsStartupReconcileDone != nil || srv.nfsReconcileStopped != nil {
		t.Fatal("expected no NFS reconciler state without NFS config")
	}
	// Starting the loop without NFS config is a no-op.
	srv.startNFSReconcileLoop(context.Background())
	if _, ok := srv.GetHealthInfo(context.Background()).Checks["nfs_mounts"]; ok {
		t.Error("nfs_mounts must not appear in health without NFS config")
	}
}

// startTestBroker runs srv.Start in the background on a loopback ephemeral
// port with an isolated HOME, and returns a cancel func and Start's result.
func startTestBroker(t *testing.T, srv *Server) (context.CancelFunc, <-chan error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()
	t.Cleanup(cancel)
	return cancel, done
}

// TestServer_Start_NFSReconcileInBackground_StartAndStop verifies that Start
// does not wait on a slow mount, that the loop's first pass mounts the share
// with the injected (fake) mounter, and that cancelling stops the loop.
func TestServer_Start_NFSReconcileInBackground_StartAndStop(t *testing.T) {
	mc := newSyncMountChecker()
	mc.block = make(chan struct{})
	mc.mountEnter = make(chan struct{}, 1)
	srv := New(ServerConfig{Host: "127.0.0.1", Port: 0, NFSConfig: nfsCfg(true), NFSMountChecker: mc},
		nil, &runtime.MockRuntime{NameFunc: func() string { return "mock" }})

	cancel, done := startTestBroker(t, srv)

	// The loop has entered Mount and is blocked there.
	select {
	case <-mc.mountEnter:
	case <-time.After(5 * time.Second):
		t.Fatal("reconcile loop never attempted the mount")
	}
	select {
	case <-srv.nfsStartupReconcileDone:
		t.Fatal("first pass reported done while the mount is still blocked")
	default:
	}
	// While the mount is blocked, the broker answers health requests and
	// reports the share as not yet reconciled.
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not reconciled") {
		t.Fatalf("healthz during blocked mount = %d %s", rec.Code, rec.Body.String())
	}

	close(mc.block)
	waitClosed(t, srv.nfsStartupReconcileDone, "first reconcile pass")
	if got := srv.GetHealthInfo(context.Background()).Checks["nfs_mounts"]; got != "healthy" {
		t.Fatalf("nfs_mounts after mount = %q, want healthy", got)
	}
	if mounts, mkdirs, _ := mc.counts(); mounts != 1 || mkdirs != 1 {
		t.Errorf("mounts=%d mkdirs=%d, want 1 and 1", mounts, mkdirs)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
	waitClosed(t, srv.nfsReconcileStopped, "reconcile loop stop")
}

// TestServer_Start_MountFailure_KeepsServing verifies that a failed mount
// is reported (health degraded, nfs_mounts names the failure) while the
// broker keeps running and stays ready.
func TestServer_Start_MountFailure_KeepsServing(t *testing.T) {
	mc := newSyncMountChecker()
	mc.mountErr = errors.New("mount.nfs: only root can do that")
	srv := New(ServerConfig{Host: "127.0.0.1", Port: 0, NFSConfig: nfsCfg(true), NFSMountChecker: mc},
		nil, &runtime.MockRuntime{NameFunc: func() string { return "mock" }})

	_, done := startTestBroker(t, srv)
	waitClosed(t, srv.nfsStartupReconcileDone, "first reconcile pass")

	select {
	case err := <-done:
		t.Fatalf("Start returned early after a mount failure: %v", err)
	default:
	}

	health := srv.GetHealthInfo(context.Background())
	if health.Status != "degraded" {
		t.Errorf("health status = %q, want degraded", health.Status)
	}
	if got := health.Checks["nfs_mounts"]; !strings.Contains(got, "only root can do that") {
		t.Errorf("nfs_mounts = %q, want the mount error", got)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("readyz = %d, want 200 (NFS state must not affect readiness)", rec.Code)
	}
}

// TestServer_ReconcileLoop_CheckOnly_PicksUpExternalMount verifies the
// periodic re-check in check-only mode: an operator mounting the share
// later turns nfs_mounts healthy, and the loop never mounts anything.
func TestServer_ReconcileLoop_CheckOnly_PicksUpExternalMount(t *testing.T) {
	mc := newSyncMountChecker()
	srv := New(ServerConfig{Host: "127.0.0.1", NFSConfig: nfsCfg(false), NFSMountChecker: mc}, nil, nil)
	srv.nfsReconcileInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.startNFSReconcileLoop(ctx)
	waitClosed(t, srv.nfsStartupReconcileDone, "first reconcile pass")
	if got := srv.GetHealthInfo(ctx).Checks["nfs_mounts"]; got == "healthy" {
		t.Fatal("expected unhealthy before the share is mounted")
	}

	mc.setMounted(filepath.Join("/mnt/nfs", "ws1"), "10.0.0.2:/scion-workspaces")
	deadline := time.Now().Add(5 * time.Second)
	for srv.GetHealthInfo(ctx).Checks["nfs_mounts"] != "healthy" {
		if time.Now().After(deadline) {
			t.Fatal("periodic re-check did not pick up the external mount")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if mounts, mkdirs, unmounts := mc.counts(); mounts+mkdirs+unmounts != 0 {
		t.Errorf("check-only loop changed the system: mounts=%d mkdirs=%d unmounts=%d", mounts, mkdirs, unmounts)
	}

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	waitClosed(t, srv.nfsReconcileStopped, "reconcile loop stop after Shutdown")
}

// --- Dispatch gate ---

// writeSettings writes a settings.yaml into dir/.scion.
func writeSettings(t *testing.T, dir, body string) string {
	t.Helper()
	scionDir := filepath.Join(dir, ".scion")
	if err := os.MkdirAll(scionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const globalNFSSettings = `schema_version: "1"
server:
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: /mnt/nfs
      shares:
        - id: ws1
          server: 10.0.0.2
          export: /scion-workspaces
`

const projectLocalSettings = `schema_version: "1"
server:
  workspace_storage:
    backend: local
`

// gateTestServer builds a server whose global settings select the nfs
// workspace backend, with a reconciler whose mounts always fail, running on
// a runtime with the given name.
func gateTestServer(t *testing.T, autoMount bool, runtimeName string) (*Server, *syncMountChecker) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSettings(t, home, globalNFSSettings)
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	mc := newSyncMountChecker()
	mc.mountErr = errors.New("mount failed")
	cfg := DefaultServerConfig()
	cfg.NFSConfig = nfsCfg(autoMount)
	cfg.NFSMountChecker = mc
	cfg.ForceRuntime = runtimeName
	rt := &runtime.MockRuntime{NameFunc: func() string { return runtimeName }}
	return New(cfg, &mockManager{}, rt), mc
}

func TestCheckNFSForDispatch(t *testing.T) {
	cases := []struct {
		name        string
		autoMount   bool
		runtime     string
		localProj   bool
		wantRefused bool
		wantMounts  bool
	}{
		{name: "auto_mount off: never gated, never mounts", autoMount: false, runtime: "docker", wantRefused: false, wantMounts: false},
		{name: "auto_mount on, nfs project, local-container runtime: refused", autoMount: true, runtime: "docker", wantRefused: true, wantMounts: true},
		{name: "auto_mount on, nfs project, kubernetes runtime: warned only", autoMount: true, runtime: "kubernetes", wantRefused: false, wantMounts: true},
		{name: "auto_mount on, project on local backend: not gated", autoMount: true, runtime: "docker", localProj: true, wantRefused: false, wantMounts: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, mc := gateTestServer(t, tc.autoMount, tc.runtime)
			projectPath := t.TempDir()
			if tc.localProj {
				writeSettings(t, projectPath, projectLocalSettings)
			} else {
				writeSettings(t, projectPath, "schema_version: \"1\"\n")
			}
			err := srv.checkNFSForDispatch("agent-1", filepath.Join(projectPath, ".scion"), "", "")
			if (err != nil) != tc.wantRefused {
				t.Fatalf("checkNFSForDispatch err = %v, wantRefused %v", err, tc.wantRefused)
			}
			mounts, _, _ := mc.counts()
			if (mounts > 0) != tc.wantMounts {
				t.Errorf("mount attempts = %d, want attempts: %v", mounts, tc.wantMounts)
			}
		})
	}
}

func TestCheckNFSForDispatch_NoNFSConfig(t *testing.T) {
	srv := New(ServerConfig{Host: "127.0.0.1"}, nil, nil)
	if err := srv.checkNFSForDispatch("a", "", "", ""); err != nil {
		t.Fatalf("checkNFSForDispatch without NFS config = %v, want nil", err)
	}
}

// TestCreateAgent_NFSGate_Returns503 drives the create handler: with
// auto_mount on and a failing mount, an NFS-backed create on a
// local-container runtime gets 503 nfs_unavailable.
func TestCreateAgent_NFSGate_Returns503(t *testing.T) {
	srv, _ := gateTestServer(t, true, "mock")
	projectPath := writeSettings(t, t.TempDir(), "schema_version: \"1\"\n")

	body := fmt.Sprintf(`{"name":"nfs-agent","id":"agent-nfs-1","projectPath":%q}`, filepath.Join(projectPath, ".scion"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "nfs_unavailable") {
		t.Fatalf("create = %d %s, want 503 nfs_unavailable", rec.Code, rec.Body.String())
	}
}
