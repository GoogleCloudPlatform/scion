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
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// MountChecker abstracts the syscall/exec layer for NFS mount reconciliation.
// This allows unit tests to assert reconciliation logic (mountpoint check,
// server:export verify, idempotency) without real NFS.
type MountChecker interface {
	// IsMountpoint returns true if the given path is currently a mountpoint.
	IsMountpoint(path string) (bool, error)

	// MountInfo returns the server:export (e.g. "10.0.0.2:/scion-workspaces")
	// for a given mountpoint. Returns ("", nil) if the path is not mounted.
	MountInfo(path string) (serverExport string, err error)

	// Mount executes the NFS mount command.
	// Requires mount privilege (root or CAP_SYS_ADMIN/sudo).
	Mount(server, export, target, options string) error

	// Unmount unmounts the given mountpoint so it can be remounted.
	Unmount(target string) error

	// MkdirAll creates the directory tree for the mountpoint.
	MkdirAll(path string, perm os.FileMode) error
}

// NFSMountReconciler ensures configured NFS shares are mounted at the expected
// paths and reports health status. It is safe for concurrent use.
//
// Deploy note: The broker process requires mount privilege (root or
// CAP_SYS_ADMIN) to mount NFS shares. When running as a non-root user,
// either grant CAP_SYS_ADMIN via setcap or configure sudoers for mount/umount.
type NFSMountReconciler struct {
	cfg     *config.V1NFSConfig
	checker MountChecker
	log     *slog.Logger

	mu       sync.RWMutex
	statuses map[string]ShareMountStatus // keyed by share ID
}

// ShareMountStatus tracks the health of a single NFS share mount.
type ShareMountStatus struct {
	ShareID string `json:"shareId"`
	Target  string `json:"target"`
	Healthy bool   `json:"healthy"`
	Message string `json:"message,omitempty"`
}

// NewNFSMountReconciler creates a reconciler for the given NFS config.
// The checker abstracts mount syscalls for testability.
func NewNFSMountReconciler(cfg *config.V1NFSConfig, checker MountChecker, log *slog.Logger) *NFSMountReconciler {
	if log == nil {
		log = slog.Default()
	}
	return &NFSMountReconciler{
		cfg:      cfg,
		checker:  checker,
		log:      log,
		statuses: make(map[string]ShareMountStatus),
	}
}

// Reconcile ensures all configured NFS shares are mounted at the correct
// paths. It is idempotent: a broker restart calls Reconcile again without
// double-mounting or erroring on an already-correct state.
//
// For each configured share:
//   - target = <MountRoot>/<share.ID>
//   - if target is not a mountpoint → mkdir -p target → mount NFS
//   - if already a mountpoint → verify it points at the expected server:export;
//     if wrong → log + remount
//
// Returns an error only if no shares are configured. Individual share failures
// are tracked in per-share status (unhealthy) and logged, but do not block
// other shares from mounting.
func (r *NFSMountReconciler) Reconcile() error {
	if r.cfg == nil {
		return fmt.Errorf("NFS config is nil")
	}
	if len(r.cfg.Shares) == 0 {
		return fmt.Errorf("no NFS shares configured")
	}

	mountOpts := r.cfg.MountOptions
	if mountOpts == "" {
		mountOpts = "vers=3,hard,nconnect=4,_netdev"
	}

	for _, share := range r.cfg.Shares {
		r.reconcileShare(share, mountOpts)
	}

	return nil
}

// reconcileShare handles a single share's mount reconciliation.
func (r *NFSMountReconciler) reconcileShare(share config.V1NFSShare, mountOpts string) {
	target := filepath.Join(r.cfg.MountRoot, share.ID)
	wantServerExport := fmt.Sprintf("%s:%s", share.Server, share.Export)

	r.log.Info("Reconciling NFS share",
		"shareID", share.ID, "target", target,
		"server", share.Server, "export", share.Export)

	mounted, err := r.checker.IsMountpoint(target)
	if err != nil {
		r.setStatus(share.ID, target, false,
			fmt.Sprintf("failed to check mountpoint: %v", err))
		return
	}

	if !mounted {
		// Not mounted — create directory and mount.
		if err := r.checker.MkdirAll(target, 0755); err != nil {
			r.setStatus(share.ID, target, false,
				fmt.Sprintf("failed to create mount directory: %v", err))
			return
		}

		if err := r.checker.Mount(share.Server, share.Export, target, mountOpts); err != nil {
			r.setStatus(share.ID, target, false,
				fmt.Sprintf("mount failed: %v", err))
			return
		}

		r.setStatus(share.ID, target, true, "mounted successfully")
		r.log.Info("NFS share mounted", "shareID", share.ID, "target", target)
		return
	}

	// Already mounted — verify it points at the expected server:export.
	currentServerExport, err := r.checker.MountInfo(target)
	if err != nil {
		r.setStatus(share.ID, target, false,
			fmt.Sprintf("failed to read mount info: %v", err))
		return
	}

	if currentServerExport == wantServerExport {
		// Correct mount — no action needed.
		r.setStatus(share.ID, target, true, "already mounted correctly")
		r.log.Debug("NFS share already mounted correctly",
			"shareID", share.ID, "target", target)
		return
	}

	// Wrong server:export — remount.
	r.log.Warn("NFS share mounted with wrong source, remounting",
		"shareID", share.ID, "target", target,
		"current", currentServerExport, "expected", wantServerExport)

	if err := r.checker.Unmount(target); err != nil {
		r.setStatus(share.ID, target, false,
			fmt.Sprintf("failed to unmount for remount: %v", err))
		return
	}
	if err := r.checker.Mount(share.Server, share.Export, target, mountOpts); err != nil {
		r.setStatus(share.ID, target, false,
			fmt.Sprintf("remount failed: %v", err))
		return
	}

	r.setStatus(share.ID, target, true, "remounted with correct source")
	r.log.Info("NFS share remounted", "shareID", share.ID, "target", target)
}

// setStatus records the health status of a share.
func (r *NFSMountReconciler) setStatus(shareID, target string, healthy bool, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses[shareID] = ShareMountStatus{
		ShareID: shareID,
		Target:  target,
		Healthy: healthy,
		Message: message,
	}
	if !healthy {
		r.log.Error("NFS share unhealthy",
			"shareID", shareID, "target", target, "reason", message)
	}
}

// IsHealthy returns true if all configured shares are mounted and healthy.
// Returns false if any share is unhealthy or has not been reconciled yet.
func (r *NFSMountReconciler) IsHealthy() bool {
	if r.cfg == nil || len(r.cfg.Shares) == 0 {
		return true // no NFS configured — healthy by default
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, share := range r.cfg.Shares {
		status, ok := r.statuses[share.ID]
		if !ok || !status.Healthy {
			return false
		}
	}
	return true
}

// ShareStatuses returns the current mount status of all configured shares.
func (r *NFSMountReconciler) ShareStatuses() []ShareMountStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]ShareMountStatus, 0, len(r.statuses))
	for _, share := range r.cfg.Shares {
		if status, ok := r.statuses[share.ID]; ok {
			result = append(result, status)
		}
	}
	return result
}

// HealthCheckString returns a summary string for health reporting.
// Returns "healthy" if all shares are mounted, or "unhealthy: <details>"
// listing failed shares.
func (r *NFSMountReconciler) HealthCheckString() string {
	if r.IsHealthy() {
		return "healthy"
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	var unhealthy []string
	for _, share := range r.cfg.Shares {
		status, ok := r.statuses[share.ID]
		if !ok {
			unhealthy = append(unhealthy, fmt.Sprintf("%s: not reconciled", share.ID))
		} else if !status.Healthy {
			unhealthy = append(unhealthy, fmt.Sprintf("%s: %s", share.ID, status.Message))
		}
	}
	return "unhealthy: " + strings.Join(unhealthy, "; ")
}

// EnsureShareMounted is called before each NFS-backed dispatch to verify
// the share for a given share ID is still mounted. It re-reconciles if needed.
// Returns an error if the share cannot be verified or mounted.
func (r *NFSMountReconciler) EnsureShareMounted(shareID string) error {
	if r.cfg == nil {
		return fmt.Errorf("NFS config is nil")
	}

	mountOpts := r.cfg.MountOptions
	if mountOpts == "" {
		mountOpts = "vers=3,hard,nconnect=4,_netdev"
	}

	for _, share := range r.cfg.Shares {
		if share.ID == shareID {
			r.reconcileShare(share, mountOpts)

			r.mu.RLock()
			status, ok := r.statuses[shareID]
			r.mu.RUnlock()

			if !ok || !status.Healthy {
				msg := "mount not healthy"
				if ok {
					msg = status.Message
				}
				return fmt.Errorf("NFS share %q is unhealthy: %s", shareID, msg)
			}
			return nil
		}
	}

	return fmt.Errorf("NFS share %q not found in config", shareID)
}
