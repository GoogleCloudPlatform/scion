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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
	inFlight    int           // Mount calls currently running
	maxInFlight int           // highest inFlight seen
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

func (m *syncMountChecker) IsMountpoint(_ context.Context, path string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.mountpoints[path]
	return ok, nil
}

func (m *syncMountChecker) ReadMountTable() (MountTable, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := MountTable{}
	for path, src := range m.mountpoints {
		t[filepath.Clean(path)] = src
	}
	return t, nil
}

func (m *syncMountChecker) Mount(ctx context.Context, server, export, target, options string) error {
	m.mu.Lock()
	m.mounts++
	m.inFlight++
	if m.inFlight > m.maxInFlight {
		m.maxInFlight = m.inFlight
	}
	block, enter, mountErr := m.block, m.mountEnter, m.mountErr
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.inFlight--
		m.mu.Unlock()
	}()
	if enter != nil {
		select {
		case enter <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return fmt.Errorf("mount cancelled: %w", ctx.Err())
		}
	}
	if mountErr != nil {
		return mountErr
	}
	m.setMounted(target, server+":"+export)
	return nil
}

func (m *syncMountChecker) Unmount(_ context.Context, target string) error {
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

	if err := r.Reconcile(context.Background()); err != nil {
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

	_ = r.Reconcile(context.Background())
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

	_ = r.Reconcile(context.Background())
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

func TestCheckNFSForDispatch_AutoMountOff_NoGate(t *testing.T) {
	mc := newMockMountChecker()
	cfg := nfsCfg(false)
	srv := &Server{
		config:             ServerConfig{NFSConfig: cfg},
		nfsMountReconciler: NewNFSMountReconciler(cfg, mc, nil),
	}
	if err := srv.checkNFSForDispatch(context.Background(), "a", "", "", ""); err != nil {
		t.Fatalf("checkNFSForDispatch with auto_mount off = %v, want nil", err)
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

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// getUntilServing polls url until the listener accepts a request, failing
// the test if it is not serving within 5 seconds.
func getUntilServing(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(url)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return resp.StatusCode, string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("broker is not serving %s: %v", url, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestServer_Start_NFSReconcileInBackground_StartAndStop verifies that Start
// does not wait on a slow mount, that the loop's first pass mounts the share
// with the injected (fake) mounter, and that cancelling stops the loop.
func TestServer_Start_NFSReconcileInBackground_StartAndStop(t *testing.T) {
	mc := newSyncMountChecker()
	mc.block = make(chan struct{})
	mc.mountEnter = make(chan struct{}, 1)
	release := sync.OnceFunc(func() { close(mc.block) })
	t.Cleanup(release)
	port := freePort(t)
	srv := New(ServerConfig{Host: "127.0.0.1", Port: port, NFSConfig: nfsCfg(true), NFSMountChecker: mc},
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
	// While the mount is blocked, the broker's listener is up and answers
	// health requests, reporting the share as not yet reconciled.
	code, body := getUntilServing(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if code != http.StatusOK || !strings.Contains(body, "not reconciled") {
		t.Fatalf("healthz during blocked mount = %d %s", code, body)
	}

	release()
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
	return newGateServer(t, nfsCfg(autoMount), runtimeName, runtimeName)
}

// newGateServer is gateTestServer with an explicit NFS config, broker
// default runtime and dispatch runtime.
func newGateServer(t *testing.T, nfs *config.V1NFSConfig, brokerRuntime, dispatchRuntime string) (*Server, *syncMountChecker) {
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
	cfg.NFSConfig = nfs
	cfg.NFSMountChecker = mc
	cfg.ForceRuntime = dispatchRuntime
	rt := &runtime.MockRuntime{NameFunc: func() string { return brokerRuntime }}
	srv := New(cfg, &mockManager{}, rt)
	if dispatchRuntime != brokerRuntime {
		// ForceRuntime selects an auxiliary runtime of that type.
		aux := &runtime.MockRuntime{NameFunc: func() string { return dispatchRuntime }}
		srv.auxiliaryRuntimesMu.Lock()
		srv.auxiliaryRuntimes[dispatchRuntime] = auxiliaryRuntime{Runtime: aux, Manager: &mockManager{}}
		srv.auxiliaryRuntimesMu.Unlock()
	}
	return srv, mc
}

func TestCheckNFSForDispatch(t *testing.T) {
	cases := []struct {
		name          string
		autoMount     bool
		brokerRuntime string // broker default runtime; "" = same as runtime
		runtime       string // the dispatch's resolved runtime
		localProj     bool
		wantRefused   bool
		wantMounts    bool
	}{
		{name: "auto_mount off: never gated, never mounts", autoMount: false, runtime: "docker", wantRefused: false, wantMounts: false},
		{name: "auto_mount on, nfs project, local-container runtime: refused", autoMount: true, runtime: "docker", wantRefused: true, wantMounts: true},
		{name: "auto_mount on, nfs project, kubernetes runtime: warned only, no mount", autoMount: true, runtime: "kubernetes", wantRefused: false, wantMounts: false},
		{name: "auto_mount on, nfs project, k8s runtime: warned only, no mount", autoMount: true, runtime: "k8s", wantRefused: false, wantMounts: false},
		{name: "auto_mount on, nfs project, cloudrun runtime: warned only, no mount", autoMount: true, runtime: "cloudrun", wantRefused: false, wantMounts: false},
		{name: "auto_mount on, nfs project, cloudrun-sandbox runtime: warned only, no mount", autoMount: true, runtime: "cloudrun-sandbox", wantRefused: false, wantMounts: false},
		{name: "docker broker, kubernetes dispatch: warned only, no mount", autoMount: true, brokerRuntime: "docker", runtime: "kubernetes", wantRefused: false, wantMounts: false},
		{name: "kubernetes broker, docker dispatch: verify-only, refused without mounting", autoMount: true, brokerRuntime: "kubernetes", runtime: "docker", wantRefused: true, wantMounts: false},
		{name: "cloudrun broker, docker dispatch: verify-only, refused without mounting", autoMount: true, brokerRuntime: "cloudrun", runtime: "docker", wantRefused: true, wantMounts: false},
		{name: "auto_mount on, project on local backend: not gated", autoMount: true, runtime: "docker", localProj: true, wantRefused: false, wantMounts: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			brokerRT := tc.brokerRuntime
			if brokerRT == "" {
				brokerRT = tc.runtime
			}
			srv, mc := newGateServer(t, nfsCfg(tc.autoMount), brokerRT, tc.runtime)
			projectPath := t.TempDir()
			if tc.localProj {
				writeSettings(t, projectPath, projectLocalSettings)
			} else {
				writeSettings(t, projectPath, "schema_version: \"1\"\n")
			}
			err := srv.checkNFSForDispatch(context.Background(), "agent-1", filepath.Join(projectPath, ".scion"), "", "")
			if (err != nil) != tc.wantRefused {
				t.Fatalf("checkNFSForDispatch err = %v, wantRefused %v", err, tc.wantRefused)
			}
			mounts, mkdirs, unmounts := mc.counts()
			if (mounts > 0) != tc.wantMounts {
				t.Errorf("mount attempts = %d, want attempts: %v", mounts, tc.wantMounts)
			}
			if !tc.wantMounts && (mkdirs > 0 || unmounts > 0) {
				t.Errorf("mkdirs = %d, unmounts = %d, want none", mkdirs, unmounts)
			}
		})
	}
}

// TestCheckNFSForDispatch_WarnOnlyRuntime_NeverChecksShare verifies that a
// Kubernetes or Cloud Run dispatch only reads the recorded status: it does
// not wait on the reconcile semaphore (held here by a stuck background
// pass), so it neither mounts nor blocks.
func TestCheckNFSForDispatch_WarnOnlyRuntime_NeverChecksShare(t *testing.T) {
	for _, rt := range []string{"kubernetes", "cloudrun"} {
		t.Run(rt, func(t *testing.T) {
			srv, mc := newGateServer(t, nfsCfg(true), "docker", rt)
			projectPath := writeSettings(t, t.TempDir(), "schema_version: \"1\"\n")
			srv.nfsMountReconciler.reconcileSem <- struct{}{} // a pass in progress
			defer func() { <-srv.nfsMountReconciler.reconcileSem }()

			done := make(chan error, 1)
			go func() {
				done <- srv.checkNFSForDispatch(context.Background(), "agent-1", filepath.Join(projectPath, ".scion"), "", "")
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("checkNFSForDispatch = %v, want nil", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("warn-only dispatch waited on the share check")
			}
			if mounts, _, _ := mc.counts(); mounts != 0 {
				t.Errorf("mounts = %d, want 0", mounts)
			}
		})
	}
}

// TestCheckNFSForDispatch_MountsFirstShareOnly verifies that the gate
// checks the first share, the one the nfs workspace backend uses.
func TestCheckNFSForDispatch_MountsFirstShareOnly(t *testing.T) {
	cfg := nfsCfg(true)
	cfg.Shares = append(cfg.Shares, config.V1NFSShare{ID: "ws2", Server: "10.0.0.3", Export: "/export-b"})
	srv, mc := newGateServer(t, cfg, "docker", "docker")
	mc.mountErr = nil
	projectPath := writeSettings(t, t.TempDir(), "schema_version: \"1\"\n")
	if err := srv.checkNFSForDispatch(context.Background(), "agent-1", filepath.Join(projectPath, ".scion"), "", ""); err != nil {
		t.Fatalf("checkNFSForDispatch = %v, want nil", err)
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if mc.mounts != 1 || mc.mountpoints["/mnt/nfs/ws1"] == "" || mc.mountpoints["/mnt/nfs/ws2"] != "" {
		t.Errorf("mounts = %d, table = %v; want only /mnt/nfs/ws1 mounted", mc.mounts, mc.mountpoints)
	}
}

// TestCheckNFSForDispatch_InvalidProjectSettings_StillGated verifies that
// a project whose settings cannot be loaded is treated as using the
// broker's nfs backend, so the gate still applies.
func TestCheckNFSForDispatch_InvalidProjectSettings_StillGated(t *testing.T) {
	srv, mc := gateTestServer(t, true, "docker")
	projectPath := writeSettings(t, t.TempDir(), "schema_version: \"1\"\nserver: [not, a, map\n")
	err := srv.checkNFSForDispatch(context.Background(), "agent-1", filepath.Join(projectPath, ".scion"), "", "")
	if err == nil {
		t.Fatal("checkNFSForDispatch = nil, want the dispatch refused")
	}
	if mounts, _, _ := mc.counts(); mounts == 0 {
		t.Error("no mount attempted, want the share checked")
	}
}

func TestCheckNFSForDispatch_NoNFSConfig(t *testing.T) {
	srv := New(ServerConfig{Host: "127.0.0.1"}, nil, nil)
	if err := srv.checkNFSForDispatch(context.Background(), "a", "", "", ""); err != nil {
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

// --- Health: when NFS degrades the overall status ---

func TestHealth_NFSDegradesOnlyWhenBrokerOwnsMounts(t *testing.T) {
	cases := []struct {
		name       string
		autoMount  bool
		pending    bool
		runtime    string
		wantStatus string
	}{
		{name: "auto_mount off", autoMount: false, runtime: "docker", wantStatus: "healthy"},
		{name: "auto_mount on, first pass pending", autoMount: true, pending: true, runtime: "docker", wantStatus: "healthy"},
		{name: "auto_mount on, kubernetes runtime", autoMount: true, runtime: "kubernetes", wantStatus: "healthy"},
		{name: "auto_mount on, cloudrun runtime", autoMount: true, runtime: "cloudrun", wantStatus: "healthy"},
		{name: "auto_mount on, local-container runtime", autoMount: true, runtime: "docker", wantStatus: "degraded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := newSyncMountChecker()
			mc.mountErr = errors.New("mount failed")
			rtName := tc.runtime
			srv := New(ServerConfig{Host: "127.0.0.1", NFSConfig: nfsCfg(tc.autoMount), NFSMountChecker: mc},
				nil, &runtime.MockRuntime{NameFunc: func() string { return rtName }})
			if !tc.pending {
				_ = srv.nfsMountReconciler.Reconcile(context.Background())
				close(srv.nfsStartupReconcileDone)
			}
			health := srv.GetHealthInfo(context.Background())
			if health.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q (checks %v)", health.Status, tc.wantStatus, health.Checks)
			}
			if got := health.Checks["nfs_mounts"]; got == "" || got == "healthy" {
				t.Errorf("nfs_mounts = %q, want the per-share problem reported", got)
			}
		})
	}
}

// TestDegradeHealthStatus verifies that NFS only lowers a healthy status:
// a status that is already unhealthy (or degraded) is kept.
func TestDegradeHealthStatus(t *testing.T) {
	for in, want := range map[string]string{
		"healthy":   "degraded",
		"degraded":  "degraded",
		"unhealthy": "unhealthy",
	} {
		if got := degradeHealthStatus(in); got != want {
			t.Errorf("degradeHealthStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHealth_NFSDegradedWithAnotherFailingCheck verifies that when another
// check has already lowered the status and NFS also degrades it, the
// status reflects the other check rather than being reset by NFS.
func TestHealth_NFSDegradedWithAnotherFailingCheck(t *testing.T) {
	mc := newSyncMountChecker()
	mc.mountErr = errors.New("mount failed")
	// No runtime: the runtime check reports "unavailable".
	srv := New(ServerConfig{Host: "127.0.0.1", NFSConfig: nfsCfg(true), NFSMountChecker: mc}, nil, nil)
	_ = srv.nfsMountReconciler.Reconcile(context.Background())
	close(srv.nfsStartupReconcileDone)
	if !srv.nfsHealthDegradesStatus() {
		t.Fatal("expected NFS to degrade the status in this setup")
	}

	health := srv.GetHealthInfo(context.Background())
	if health.Checks["runtime"] != "unavailable" {
		t.Fatalf("runtime check = %q, want unavailable", health.Checks["runtime"])
	}
	if health.Status != "degraded" {
		t.Errorf("status = %q, want degraded (checks %v)", health.Status, health.Checks)
	}
}

// --- Request-context bounds on dispatch-time mounts ---

// TestEnsureShareMounted_RequestCtxCancelsMount verifies that a mount run
// at dispatch time receives the request context and stops when it is
// cancelled.
func TestEnsureShareMounted_RequestCtxCancelsMount(t *testing.T) {
	mc := newSyncMountChecker()
	mc.block = make(chan struct{}) // never released: only ctx can end the mount
	r := NewNFSMountReconciler(nfsCfg(true), mc, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := r.EnsureShareMounted(ctx, "ws1")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("EnsureShareMounted = %v, want a cancellation error", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("EnsureShareMounted took %s after the request ctx ended", elapsed)
	}
}

// TestEnsureShareMounted_RequestCtxCancelsWait verifies that a dispatch
// waiting behind a slow background mount gives up when its request context
// ends, without touching the share's status.
func TestEnsureShareMounted_RequestCtxCancelsWait(t *testing.T) {
	mc := newSyncMountChecker()
	mc.block = make(chan struct{})
	mc.mountEnter = make(chan struct{}, 1)
	release := sync.OnceFunc(func() { close(mc.block) })
	defer release()
	r := NewNFSMountReconciler(nfsCfg(true), mc, nil)

	bgDone := make(chan struct{})
	go func() {
		defer close(bgDone)
		_ = r.Reconcile(context.Background())
	}()
	select {
	case <-mc.mountEnter:
	case <-time.After(5 * time.Second):
		t.Fatal("background reconcile never reached Mount")
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- r.EnsureShareMounted(ctx, "ws1") }()
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("EnsureShareMounted = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch kept waiting after its request ctx was cancelled")
	}
	if mounts, _, _ := mc.counts(); mounts != 1 {
		t.Errorf("mounts = %d, want 1 (only the background attempt)", mounts)
	}
	release()
	<-bgDone
}

// TestExecRunCommand_ParentCtxCancels verifies that the exec layer kills a
// running command when the parent (request) context is cancelled, well
// before mountCommandTimeout.
func TestExecRunCommand_ParentCtxCancels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := execRunCommand(ctx, "sleep", "30")
	if err == nil || !strings.Contains(err.Error(), "cancelled") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("execRunCommand = %v, want a cancellation error wrapping the parent ctx error", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("command ran for %s after the parent ctx ended", elapsed)
	}
}

// TestExecRunCommand_TimesOut verifies that a command that outlives
// mountCommandTimeout under a live parent is reported as timed out, not as
// cancelled.
func TestExecRunCommand_TimesOut(t *testing.T) {
	old := mountCommandTimeout
	mountCommandTimeout = 200 * time.Millisecond
	t.Cleanup(func() { mountCommandTimeout = old })

	start := time.Now()
	_, err := execRunCommand(context.Background(), "sleep", "30")
	if err == nil || !strings.Contains(err.Error(), "timed out after 200ms") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("execRunCommand = %v, want a timeout error wrapping context.DeadlineExceeded", err)
	}
	if strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("execRunCommand = %v, want timed out, not cancelled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("command ran for %s, want near the 200ms timeout", elapsed)
	}
}

// TestClassifyCommandError covers how a command's result is attributed to
// its contexts.
func TestClassifyCommandError(t *testing.T) {
	cmdErr := errors.New("exit status 32")
	live := context.Background()
	cancelledParent, cancel := context.WithCancel(context.Background())
	cancel()
	timedOut, cancelT := context.WithTimeout(context.Background(), 0)
	defer cancelT()

	tests := []struct {
		name        string
		err         error
		ctx, parent context.Context
		want        string // "" means nil
		wantIs      error
	}{
		{"success with live ctx", nil, live, live, "", nil},
		{"success as the parent ends", nil, cancelledParent, cancelledParent, "", nil},
		{"success as the timeout fires", nil, timedOut, live, "", nil},
		{"failure with live ctx", cmdErr, live, live, "exit status 32", cmdErr},
		{"failure after parent cancel", cmdErr, cancelledParent, cancelledParent, "mount cancelled", context.Canceled},
		{"failure after timeout", cmdErr, timedOut, live, "mount timed out after 1s", context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyCommandError("mount", time.Second, tt.err, tt.ctx, tt.parent)
			if tt.want == "" {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if got == nil || !strings.Contains(got.Error(), tt.want) || !errors.Is(got, tt.wantIs) {
				t.Fatalf("got %v, want %q wrapping %v", got, tt.want, tt.wantIs)
			}
		})
	}
}

// TestExecRunCommand_DescendantHoldsPipe verifies that a cancelled command
// returns near the deadline even when a child process (as mount.nfs is for
// mount) holds the output pipe open: the whole process group is killed.
func TestExecRunCommand_DescendantHoldsPipe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	// sh forks sleep (not exec'd, because of the trailing command), and
	// sleep inherits the output pipe.
	_, err := execRunCommand(ctx, "sh", "-c", "sleep 30; true")
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("execRunCommand = %v, want a cancellation error", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("execRunCommand returned %s after start, want near the 200ms deadline", elapsed)
	}
}

// TestExecRunCommand_DescendantOutsideGroup verifies the WaitDelay
// backstop: a descendant that moved to its own session survives the group
// kill and keeps the output pipe open, and the call still returns shortly
// after commandWaitDelay rather than when the descendant exits.
func TestExecRunCommand_DescendantOutsideGroup(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := execRunCommand(ctx, "sh", "-c", "setsid sleep 12 & wait")
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("execRunCommand = %v, want a cancellation error", err)
	}
	if limit := commandWaitDelay + 4*time.Second; elapsed > limit {
		t.Fatalf("execRunCommand returned after %s, want within %s", elapsed, limit)
	}
}

// TestEnsureShareMounted_SerializesMounts verifies that concurrent
// dispatch-time checks never run Mount for the share at the same time.
func TestEnsureShareMounted_SerializesMounts(t *testing.T) {
	mc := newSyncMountChecker()
	mc.block = make(chan struct{})
	mc.mountEnter = make(chan struct{}, 4)
	mc.mountErr = errors.New("mount failed") // every caller retries the mount
	r := NewNFSMountReconciler(nfsCfg(true), mc, nil)

	const callers = 3
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.EnsureShareMounted(context.Background(), "ws1")
		}()
	}
	<-mc.mountEnter
	// Give the other callers time to reach Mount if nothing stops them.
	time.Sleep(200 * time.Millisecond)
	close(mc.block)
	wg.Wait()

	mc.mu.Lock()
	defer mc.mu.Unlock()
	if mc.mounts != callers {
		t.Errorf("mounts = %d, want %d", mc.mounts, callers)
	}
	if mc.maxInFlight != 1 {
		t.Errorf("max concurrent Mount calls = %d, want 1", mc.maxInFlight)
	}
}

// TestEnsureShareMounted_CancelledRequestKeepsStatus verifies that a
// dispatch whose request ends during the mount does not record the share
// as unhealthy: only a real mount result or the exec timeout counts.
func TestEnsureShareMounted_CancelledRequestKeepsStatus(t *testing.T) {
	for _, prior := range []string{"none", "healthy"} {
		t.Run(prior, func(t *testing.T) {
			mc := newSyncMountChecker()
			r := NewNFSMountReconciler(nfsCfg(true), mc, nil)
			if prior == "healthy" {
				_ = r.Reconcile(context.Background()) // mounts; healthy
				mc.mu.Lock()
				delete(mc.mountpoints, "/mnt/nfs/ws1") // dropped since
				mc.mu.Unlock()
			}
			before, hadBefore := r.ShareStatus("ws1")
			mc.mu.Lock()
			mc.block = make(chan struct{}) // only ctx ends the mount
			mc.mu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if err := r.EnsureShareMounted(ctx, "ws1"); err == nil {
				t.Fatal("EnsureShareMounted = nil, want the cancellation")
			}
			after, hadAfter := r.ShareStatus("ws1")
			if hadAfter != hadBefore || after != before {
				t.Errorf("status after a cancelled request = %+v (%v), want unchanged %+v (%v)", after, hadAfter, before, hadBefore)
			}
		})
	}
}

// TestReconcile_ExecTimeoutRecorded verifies that a command that hits the
// exec timeout (not the caller's ctx) is recorded as unhealthy.
func TestReconcile_ExecTimeoutRecorded(t *testing.T) {
	mc := newMockMountChecker()
	mc.mountErr = fmt.Errorf("mount timed out after 1m30s: %w", context.DeadlineExceeded)
	r := NewNFSMountReconciler(nfsCfg(true), mc, nil)
	_ = r.Reconcile(context.Background())
	st, ok := r.ShareStatus("ws1")
	if !ok || st.Healthy || !strings.Contains(st.Message, "timed out") {
		t.Errorf("status = %+v (%v), want unhealthy with the timeout", st, ok)
	}
}

// TestExecMountChecker_IsMountpoint_TimeoutIsError verifies that a
// timed-out mountpoint(1) is an error, not "not mounted", and that the
// reconciler then records it and does not mount over the path.
func TestExecMountChecker_IsMountpoint_TimeoutIsError(t *testing.T) {
	for _, ctxErr := range []error{context.DeadlineExceeded, context.Canceled} {
		t.Run(ctxErr.Error(), func(t *testing.T) {
			checker := NewExecMountChecker(nil)
			checker.geteuid = func() int { return 0 }
			var ran []string
			checker.runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
				ran = append(ran, name)
				if name == "mountpoint" {
					return nil, fmt.Errorf("mountpoint timed out: %w", ctxErr)
				}
				return nil, nil
			}
			mounted, err := checker.IsMountpoint(context.Background(), "/mnt/nfs/ws1")
			if mounted || !errors.Is(err, ctxErr) {
				t.Fatalf("IsMountpoint = %v, %v; want false and %v", mounted, err, ctxErr)
			}

			root := t.TempDir()
			cfg := nfsCfg(true)
			cfg.MountRoot = root
			r := NewNFSMountReconciler(cfg, checker, nil)
			ran = nil
			_ = r.Reconcile(context.Background())
			if strings.Join(ran, ",") != "mountpoint" {
				t.Errorf("commands run = %v, want only mountpoint", ran)
			}
			if st, _ := r.ShareStatus("ws1"); st.Healthy || !strings.Contains(st.Message, "failed to check mountpoint") {
				t.Errorf("status = %+v, want failed to check mountpoint", st)
			}
		})
	}
}

// TestReconcile_MountpointGuard verifies that a path mountpoint(1) reports
// as mounted, although the mount table does not list it, is not mounted
// over.
func TestReconcile_MountpointGuard(t *testing.T) {
	mc := newMockMountChecker()
	mc.kernelOnlyMounts["/mnt/nfs/ws1"] = true
	r := NewNFSMountReconciler(nfsCfg(true), mc, nil)
	_ = r.Reconcile(context.Background())
	if len(mc.mountCalls) != 0 || len(mc.mkdirCalls) != 0 {
		t.Errorf("mounts = %d, mkdirs = %d, want none", len(mc.mountCalls), len(mc.mkdirCalls))
	}
	if st, _ := r.ShareStatus("ws1"); st.Healthy || !strings.Contains(st.Message, "not listed in the mount table") {
		t.Errorf("status = %+v, want the guard reason", st)
	}
}

// TestReconcile_DecidesFromMountTable verifies that the mounted state comes
// from the mount table: mountpoint(1), which can block on a hung mount, is
// not called when checking, only right before a mount.
func TestReconcile_DecidesFromMountTable(t *testing.T) {
	cases := []struct {
		name      string
		autoMount bool
		mounted   bool
		wantCalls int
	}{
		{name: "check-only, not mounted", autoMount: false, mounted: false, wantCalls: 0},
		{name: "check-only, mounted", autoMount: false, mounted: true, wantCalls: 0},
		{name: "auto_mount, mounted", autoMount: true, mounted: true, wantCalls: 0},
		{name: "auto_mount, not mounted", autoMount: true, mounted: false, wantCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMockMountChecker()
			if tc.mounted {
				mc.mountpoints["/mnt/nfs/ws1"] = "10.0.0.2:/scion-workspaces"
			}
			r := NewNFSMountReconciler(nfsCfg(tc.autoMount), mc, nil)
			_ = r.Reconcile(context.Background())
			if mc.isMountpointCalls != tc.wantCalls {
				t.Errorf("IsMountpoint calls = %d, want %d", mc.isMountpointCalls, tc.wantCalls)
			}
		})
	}
}

// TestServer_VerifyOnlyOnPlatformMountedRuntimes verifies that with
// auto_mount on, a broker whose default runtime is Kubernetes or Cloud Run
// never mounts in its background loop; it only verifies.
func TestServer_VerifyOnlyOnPlatformMountedRuntimes(t *testing.T) {
	for _, rtName := range []string{"kubernetes", "k8s", "remote", "cloudrun", "cloudrun-instances", "cloudrun-sandbox", "docker"} {
		t.Run(rtName, func(t *testing.T) {
			mc := newSyncMountChecker()
			name := rtName
			srv := New(ServerConfig{Host: "127.0.0.1", NFSConfig: nfsCfg(true), NFSMountChecker: mc},
				nil, &runtime.MockRuntime{NameFunc: func() string { return name }})
			_ = srv.nfsMountReconciler.Reconcile(context.Background())
			mounts, mkdirs, _ := mc.counts()
			wantMount := rtName == "docker"
			if (mounts > 0) != wantMount || (mkdirs > 0) != wantMount {
				t.Fatalf("mounts = %d, mkdirs = %d, want mounting: %v", mounts, mkdirs, wantMount)
			}
			if !wantMount {
				if hc := srv.nfsMountReconciler.HealthCheckString(); !strings.Contains(hc, "does not mount it") {
					t.Errorf("HealthCheckString = %q, want the verify-only reason", hc)
				}
			}
		})
	}
}

// --- Non-root brokers ---

func TestExecMountChecker_NonRoot_NoShellOut(t *testing.T) {
	checker := NewExecMountChecker(nil)
	checker.geteuid = func() int { return 1000 }
	var ran []string
	checker.runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		ran = append(ran, name)
		return nil, &exec.ExitError{} // mountpoint: not a mountpoint
	}

	if err := checker.Mount(context.Background(), "10.0.0.2", "/x", "/mnt/nfs/ws1", "vers=3"); err == nil || !strings.Contains(err.Error(), "requires root") {
		t.Errorf("Mount as non-root = %v, want a requires-root error", err)
	}
	if err := checker.Unmount(context.Background(), "/mnt/nfs/ws1"); err == nil || !strings.Contains(err.Error(), "requires root") {
		t.Errorf("Unmount as non-root = %v, want a requires-root error", err)
	}
	if len(ran) != 0 {
		t.Fatalf("ran %v as non-root, want nothing", ran)
	}

	// Through the reconciler: no command runs (the mount state comes from
	// the mount table), no mount directory is created, and the share
	// reports why.
	root := t.TempDir()
	cfg := nfsCfg(true)
	cfg.MountRoot = root
	r := NewNFSMountReconciler(cfg, checker, nil)
	_ = r.Reconcile(context.Background())
	if len(ran) != 0 {
		t.Errorf("commands run = %v, want none", ran)
	}
	if _, err := os.Stat(filepath.Join(root, "ws1")); !os.IsNotExist(err) {
		t.Errorf("mount directory created as non-root (stat err %v)", err)
	}
	if hc := r.HealthCheckString(); !strings.Contains(hc, "requires root") || !strings.Contains(hc, "uid 1000") {
		t.Errorf("HealthCheckString = %q, want the requires-root reason", hc)
	}
}

func TestExecMountChecker_Root_Mounts(t *testing.T) {
	checker := NewExecMountChecker(nil)
	checker.geteuid = func() int { return 0 }
	if err := checker.MountPrivilegeError(); err != nil {
		t.Fatalf("MountPrivilegeError as root = %v", err)
	}
}

// TestCheckNFSForDispatch_RequestCtxBoundsMount verifies that the dispatch
// gate passes the request context down to the mount, so a hung mount ends
// when the request does.
func TestCheckNFSForDispatch_RequestCtxBoundsMount(t *testing.T) {
	srv, mc := gateTestServer(t, true, "docker")
	mc.mu.Lock()
	mc.mountErr = nil
	mc.block = make(chan struct{})
	mc.mu.Unlock()
	release := sync.OnceFunc(func() { close(mc.block) })
	t.Cleanup(release)
	projectPath := writeSettings(t, t.TempDir(), "schema_version: \"1\"\n")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.checkNFSForDispatch(ctx, "agent-1", filepath.Join(projectPath, ".scion"), "", "")
	}()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("checkNFSForDispatch = nil, want an error after the request ctx ended")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch gate kept waiting on the mount after the request ctx ended")
	}
}
