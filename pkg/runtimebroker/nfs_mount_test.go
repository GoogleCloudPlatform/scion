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
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// mockMountChecker is a test double for MountChecker that records calls and
// returns configurable results without touching the real filesystem.
type mockMountChecker struct {
	// mountpoints maps path → server:export. Present = mounted.
	mountpoints map[string]string

	// mountCalls records (server, export, target, options) for each Mount call.
	mountCalls []mountCall
	// unmountCalls records targets passed to Unmount.
	unmountCalls []string
	// mkdirCalls records paths passed to MkdirAll.
	mkdirCalls []string

	// Inject errors for specific operations.
	isMountpointErr map[string]error
	mountInfoErr    map[string]error
	mountErr        error
	unmountErr      error
	mkdirErr        error
}

type mountCall struct {
	Server, Export, Target, Options string
}

func newMockMountChecker() *mockMountChecker {
	return &mockMountChecker{
		mountpoints:     make(map[string]string),
		isMountpointErr: make(map[string]error),
		mountInfoErr:    make(map[string]error),
	}
}

func (m *mockMountChecker) IsMountpoint(path string) (bool, error) {
	if err, ok := m.isMountpointErr[path]; ok {
		return false, err
	}
	_, ok := m.mountpoints[path]
	return ok, nil
}

func (m *mockMountChecker) MountInfo(path string) (string, error) {
	if err, ok := m.mountInfoErr[path]; ok {
		return "", err
	}
	se, ok := m.mountpoints[path]
	if !ok {
		return "", nil
	}
	return se, nil
}

func (m *mockMountChecker) Mount(server, export, target, options string) error {
	m.mountCalls = append(m.mountCalls, mountCall{server, export, target, options})
	if m.mountErr != nil {
		return m.mountErr
	}
	m.mountpoints[target] = fmt.Sprintf("%s:%s", server, export)
	return nil
}

func (m *mockMountChecker) Unmount(target string) error {
	m.unmountCalls = append(m.unmountCalls, target)
	if m.unmountErr != nil {
		return m.unmountErr
	}
	delete(m.mountpoints, target)
	return nil
}

func (m *mockMountChecker) MkdirAll(path string, perm os.FileMode) error {
	m.mkdirCalls = append(m.mkdirCalls, path)
	if m.mkdirErr != nil {
		return m.mkdirErr
	}
	return nil
}

// --- Tests ---

func testNFSConfig() *config.V1NFSConfig {
	return &config.V1NFSConfig{
		MountRoot:    "/mnt/nfs",
		MountOptions: "vers=3,hard,nconnect=4,_netdev",
		SubPathRoot:  "projects",
		Shares: []config.V1NFSShare{
			{ID: "ws1", Server: "10.0.0.2", Export: "/scion-workspaces"},
		},
	}
}

func TestReconcile_MountAbsent_MkdirAndMount(t *testing.T) {
	mc := newMockMountChecker()
	cfg := testNFSConfig()
	r := NewNFSMountReconciler(cfg, mc, nil)

	if err := r.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Should have created the directory
	wantTarget := filepath.Join("/mnt/nfs", "ws1")
	if len(mc.mkdirCalls) != 1 || mc.mkdirCalls[0] != wantTarget {
		t.Errorf("mkdirCalls = %v, want [%q]", mc.mkdirCalls, wantTarget)
	}

	// Should have mounted
	if len(mc.mountCalls) != 1 {
		t.Fatalf("mountCalls = %d, want 1", len(mc.mountCalls))
	}
	call := mc.mountCalls[0]
	if call.Server != "10.0.0.2" || call.Export != "/scion-workspaces" || call.Target != wantTarget {
		t.Errorf("mount call = %+v, want server=10.0.0.2, export=/scion-workspaces, target=%s",
			call, wantTarget)
	}
	if call.Options != "vers=3,hard,nconnect=4,_netdev" {
		t.Errorf("mount options = %q, want default NFS options", call.Options)
	}

	// Should be healthy
	if !r.IsHealthy() {
		t.Error("expected healthy after successful mount")
	}
}

func TestReconcile_AlreadyMountedCorrectly_NoOp(t *testing.T) {
	mc := newMockMountChecker()
	target := filepath.Join("/mnt/nfs", "ws1")
	mc.mountpoints[target] = "10.0.0.2:/scion-workspaces"

	cfg := testNFSConfig()
	r := NewNFSMountReconciler(cfg, mc, nil)

	if err := r.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// No mkdir or mount calls
	if len(mc.mkdirCalls) != 0 {
		t.Errorf("mkdirCalls = %v, want none (already mounted)", mc.mkdirCalls)
	}
	if len(mc.mountCalls) != 0 {
		t.Errorf("mountCalls = %d, want 0 (already mounted correctly)", len(mc.mountCalls))
	}

	if !r.IsHealthy() {
		t.Error("expected healthy for correctly mounted share")
	}
}

func TestReconcile_WrongServerExport_Remount(t *testing.T) {
	mc := newMockMountChecker()
	target := filepath.Join("/mnt/nfs", "ws1")
	mc.mountpoints[target] = "10.0.0.99:/wrong-export" // wrong source

	cfg := testNFSConfig()
	r := NewNFSMountReconciler(cfg, mc, nil)

	if err := r.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Should have unmounted
	if len(mc.unmountCalls) != 1 || mc.unmountCalls[0] != target {
		t.Errorf("unmountCalls = %v, want [%q]", mc.unmountCalls, target)
	}

	// Should have remounted with correct source
	if len(mc.mountCalls) != 1 {
		t.Fatalf("mountCalls = %d, want 1 (remount)", len(mc.mountCalls))
	}
	call := mc.mountCalls[0]
	if call.Server != "10.0.0.2" || call.Export != "/scion-workspaces" {
		t.Errorf("remount call = %+v, want correct server:export", call)
	}

	if !r.IsHealthy() {
		t.Error("expected healthy after remount")
	}
}

func TestReconcile_MultipleShares(t *testing.T) {
	mc := newMockMountChecker()
	cfg := &config.V1NFSConfig{
		MountRoot:    "/mnt/nfs",
		MountOptions: "vers=4.1,hard",
		Shares: []config.V1NFSShare{
			{ID: "ws1", Server: "10.0.0.2", Export: "/export-a"},
			{ID: "ws2", Server: "10.0.0.3", Export: "/export-b"},
		},
	}
	r := NewNFSMountReconciler(cfg, mc, nil)

	if err := r.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(mc.mountCalls) != 2 {
		t.Fatalf("mountCalls = %d, want 2 (one per share)", len(mc.mountCalls))
	}

	// Both should be healthy
	if !r.IsHealthy() {
		t.Error("expected healthy after mounting both shares")
	}

	statuses := r.ShareStatuses()
	if len(statuses) != 2 {
		t.Errorf("ShareStatuses len = %d, want 2", len(statuses))
	}
}

func TestReconcile_MountFailure_UnhealthySignal(t *testing.T) {
	mc := newMockMountChecker()
	mc.mountErr = fmt.Errorf("permission denied")

	cfg := testNFSConfig()
	r := NewNFSMountReconciler(cfg, mc, nil)

	// Reconcile itself does not return an error for individual share failures
	if err := r.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if r.IsHealthy() {
		t.Error("expected unhealthy after mount failure")
	}

	hc := r.HealthCheckString()
	if hc == "healthy" {
		t.Error("HealthCheckString should not be 'healthy' after mount failure")
	}
}

func TestReconcile_NilConfig_Error(t *testing.T) {
	mc := newMockMountChecker()
	r := NewNFSMountReconciler(nil, mc, nil)

	if err := r.Reconcile(); err == nil {
		t.Error("expected error for nil config")
	}
}

func TestReconcile_NoShares_Error(t *testing.T) {
	mc := newMockMountChecker()
	cfg := &config.V1NFSConfig{
		MountRoot: "/mnt/nfs",
		Shares:    nil,
	}
	r := NewNFSMountReconciler(cfg, mc, nil)

	if err := r.Reconcile(); err == nil {
		t.Error("expected error for no shares")
	}
}

func TestReconcile_Idempotent_DoubleCall(t *testing.T) {
	mc := newMockMountChecker()
	cfg := testNFSConfig()
	r := NewNFSMountReconciler(cfg, mc, nil)

	// First call: mounts the share
	if err := r.Reconcile(); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if len(mc.mountCalls) != 1 {
		t.Fatalf("expected 1 mount call after first Reconcile, got %d", len(mc.mountCalls))
	}

	// Second call: share is already mounted correctly — no-op
	if err := r.Reconcile(); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if len(mc.mountCalls) != 1 {
		t.Errorf("expected still 1 mount call after second Reconcile (idempotent), got %d",
			len(mc.mountCalls))
	}
}

func TestEnsureShareMounted_Healthy(t *testing.T) {
	mc := newMockMountChecker()
	cfg := testNFSConfig()
	r := NewNFSMountReconciler(cfg, mc, nil)

	if err := r.EnsureShareMounted("ws1"); err != nil {
		t.Fatalf("EnsureShareMounted: %v", err)
	}

	// Should have mounted
	if len(mc.mountCalls) != 1 {
		t.Errorf("mountCalls = %d, want 1", len(mc.mountCalls))
	}
}

func TestEnsureShareMounted_UnknownShare(t *testing.T) {
	mc := newMockMountChecker()
	cfg := testNFSConfig()
	r := NewNFSMountReconciler(cfg, mc, nil)

	if err := r.EnsureShareMounted("nonexistent"); err == nil {
		t.Error("expected error for unknown share ID")
	}
}

func TestEnsureShareMounted_MountFailure(t *testing.T) {
	mc := newMockMountChecker()
	mc.mountErr = fmt.Errorf("network unreachable")

	cfg := testNFSConfig()
	r := NewNFSMountReconciler(cfg, mc, nil)

	if err := r.EnsureShareMounted("ws1"); err == nil {
		t.Error("expected error when mount fails")
	}
}

func TestHealthCheckString_Healthy(t *testing.T) {
	mc := newMockMountChecker()
	cfg := testNFSConfig()
	r := NewNFSMountReconciler(cfg, mc, nil)

	// Mount the share
	_ = r.Reconcile()

	got := r.HealthCheckString()
	if got != "healthy" {
		t.Errorf("HealthCheckString = %q, want %q", got, "healthy")
	}
}

func TestHealthCheckString_Unhealthy(t *testing.T) {
	mc := newMockMountChecker()
	mc.mountErr = fmt.Errorf("denied")

	cfg := testNFSConfig()
	r := NewNFSMountReconciler(cfg, mc, nil)
	_ = r.Reconcile()

	got := r.HealthCheckString()
	if got == "healthy" {
		t.Error("HealthCheckString should not be 'healthy' after mount failure")
	}
}

func TestReconcile_DefaultMountOptions(t *testing.T) {
	mc := newMockMountChecker()
	cfg := &config.V1NFSConfig{
		MountRoot:    "/mnt/nfs",
		MountOptions: "", // should use default
		Shares: []config.V1NFSShare{
			{ID: "ws1", Server: "10.0.0.2", Export: "/scion-workspaces"},
		},
	}
	r := NewNFSMountReconciler(cfg, mc, nil)

	if err := r.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(mc.mountCalls) != 1 {
		t.Fatalf("mountCalls = %d, want 1", len(mc.mountCalls))
	}
	if mc.mountCalls[0].Options != "vers=3,hard,nconnect=4,_netdev" {
		t.Errorf("options = %q, want default NFS options", mc.mountCalls[0].Options)
	}
}

func TestIsHealthy_NoNFSConfigured(t *testing.T) {
	mc := newMockMountChecker()
	r := NewNFSMountReconciler(nil, mc, nil)

	// No NFS configured → healthy by default (local backend)
	if !r.IsHealthy() {
		t.Error("expected healthy when no NFS is configured")
	}
}
