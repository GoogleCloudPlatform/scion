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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestServer_NFSReconcilerWired_WhenNFSConfigured verifies that the
// NFSMountReconciler is constructed and stored on the Server when
// NFSConfig is present with shares.
func TestServer_NFSReconcilerWired_WhenNFSConfigured(t *testing.T) {
	cfg := ServerConfig{
		Port: 0,
		Host: "127.0.0.1",
		NFSConfig: &config.V1NFSConfig{
			MountRoot:    "/mnt/nfs",
			MountOptions: "vers=3,hard",
			Shares: []config.V1NFSShare{
				{ID: "ws1", Server: "10.0.0.2", Export: "/scion-workspaces"},
			},
		},
	}

	srv := New(cfg, nil, nil)

	if srv.nfsMountReconciler == nil {
		t.Fatal("expected nfsMountReconciler to be constructed when NFSConfig has shares")
	}
}

// TestServer_NFSReconcilerNil_WhenLocalBackend verifies that the
// NFSMountReconciler is NOT constructed when NFSConfig is nil
// (local backend).
func TestServer_NFSReconcilerNil_WhenLocalBackend(t *testing.T) {
	cfg := ServerConfig{
		Port: 0,
		Host: "127.0.0.1",
		// NFSConfig is nil — local backend
	}

	srv := New(cfg, nil, nil)

	if srv.nfsMountReconciler != nil {
		t.Fatal("expected nfsMountReconciler to be nil when NFSConfig is not set")
	}
}

// TestServer_NFSReconcilerNil_WhenNoShares verifies that the
// NFSMountReconciler is NOT constructed when NFSConfig exists but
// has no shares configured.
func TestServer_NFSReconcilerNil_WhenNoShares(t *testing.T) {
	cfg := ServerConfig{
		Port: 0,
		Host: "127.0.0.1",
		NFSConfig: &config.V1NFSConfig{
			MountRoot: "/mnt/nfs",
			Shares:    nil,
		},
	}

	srv := New(cfg, nil, nil)

	if srv.nfsMountReconciler != nil {
		t.Fatal("expected nfsMountReconciler to be nil when no shares configured")
	}
}

// TestServer_HealthIncludesNFS verifies that NFS mount health is surfaced
// in the broker's health response when NFS is configured.
func TestServer_HealthIncludesNFS(t *testing.T) {
	cfg := ServerConfig{
		Port: 0,
		Host: "127.0.0.1",
		NFSConfig: &config.V1NFSConfig{
			MountRoot:    "/mnt/nfs",
			MountOptions: "vers=3,hard",
			Shares: []config.V1NFSShare{
				{ID: "ws1", Server: "10.0.0.2", Export: "/scion-workspaces"},
			},
		},
	}

	srv := New(cfg, nil, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})

	// Before reconciliation: shares are unreconciled → unhealthy
	health := srv.GetHealthInfo(context.Background())

	nfsCheck, ok := health.Checks["nfs_mounts"]
	if !ok {
		t.Fatal("expected nfs_mounts key in health checks")
	}
	// Before Reconcile(), shares are not yet reconciled → "unhealthy"
	if nfsCheck == "healthy" {
		t.Error("expected unhealthy before reconciliation")
	}
	// Pending, and auto_mount off: reported per share only.
	if health.Status != "healthy" {
		t.Errorf("overall status = %q, want healthy (NFS state is reported per share only)", health.Status)
	}
}

// TestServer_HealthExcludesNFS_WhenLocal verifies that no nfs_mounts key
// appears in health when NFS is not configured.
func TestServer_HealthExcludesNFS_WhenLocal(t *testing.T) {
	cfg := ServerConfig{
		Port: 0,
		Host: "127.0.0.1",
	}

	srv := New(cfg, nil, nil)
	health := srv.GetHealthInfo(context.Background())

	if _, ok := health.Checks["nfs_mounts"]; ok {
		t.Error("did not expect nfs_mounts in health checks when NFS is not configured")
	}
}
