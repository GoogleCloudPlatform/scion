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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// MountChecker abstracts the syscall/exec layer for NFS mount reconciliation.
// This allows unit tests to assert reconciliation logic (mountpoint check,
// server:export verify, idempotency) without real NFS.
type MountChecker interface {
	// ReadMountTable returns the current mount table (/proc/mounts). It
	// does not stat any mountpoint, so a hung NFS mount cannot block it.
	// This is what decides whether a share is mounted, and from where. The
	// reconciler reads it once per pass.
	ReadMountTable() (MountTable, error)

	// IsMountpoint asks the kernel whether path is a mountpoint
	// (mountpoint(1)). It can block on a hung mount, so ctx bounds it. The
	// reconciler calls it only immediately before mounting, as a guard
	// against mounting over a mount the mount table did not show.
	IsMountpoint(ctx context.Context, path string) (bool, error)

	// Mount executes the NFS mount command.
	// Requires mount privilege (mount.nfs requires root). ctx bounds the
	// mount command.
	Mount(ctx context.Context, server, export, target, options string) error

	// Unmount unmounts the given mountpoint so it can be remounted.
	Unmount(ctx context.Context, target string) error

	// MkdirAll creates the directory tree for the mountpoint.
	MkdirAll(path string, perm os.FileMode) error
}

// NFSMountReconciler checks that configured NFS shares are mounted at the
// expected paths and reports health status. It is safe for concurrent use.
//
// Whether a share is mounted, and from where, is decided from the mount
// table (/proc/mounts) only, the same lookup scion doctor uses.
//
// It has two modes:
//   - Check-only: AutoMount false (the default), or verify-only set with
//     SetVerifyOnly. Each share is checked read-only (is <MountRoot>/<ID>
//     mounted from the expected server:export); nothing is created,
//     mounted or unmounted. The mounts are provided by the operator, or by
//     the platform on Kubernetes and Cloud Run.
//   - Mounting: AutoMount true and not verify-only. A share that is not
//     mounted is mounted, and one mounted from the wrong source is
//     remounted.
//
// Deploy note: auto-mount requires the broker process to run as root: the
// mount.nfs helper checks uid 0, so CAP_SYS_ADMIN alone is not enough (see
// NFS_DEPLOY_NOTES.md).
type NFSMountReconciler struct {
	cfg     *config.V1NFSConfig
	checker MountChecker
	log     *slog.Logger

	// reconcileSem (capacity 1) serializes share reconciliation so the
	// background loop and a dispatch-time EnsureShareMounted never mount
	// the same target concurrently. It is a channel rather than a mutex so
	// a dispatch waiting behind a slow background mount gives up when its
	// request context is done.
	reconcileSem chan struct{}

	// verifyOnlyReason, when set, keeps the reconciler from mounting even
	// with AutoMount on (for example, the broker's default runtime is
	// Kubernetes or Cloud Run, where the platform mounts the export). It is
	// set before the reconciler is used and not changed afterwards.
	verifyOnlyReason string

	mu       sync.RWMutex
	statuses map[string]ShareMountStatus // keyed by share ID
}

// AutoMount reports whether auto_mount is set in the configuration. See
// MountsShares for whether the reconciler actually mounts.
func (r *NFSMountReconciler) AutoMount() bool {
	return r.cfg != nil && r.cfg.AutoMount
}

// SetVerifyOnly makes the reconciler check shares without mounting,
// unmounting or remounting them, even with AutoMount on. reason is shown in
// share status messages. Call it before Run or any other method.
func (r *NFSMountReconciler) SetVerifyOnly(reason string) {
	r.verifyOnlyReason = reason
}

// MountsShares reports whether the reconciler mounts shares itself:
// AutoMount is on and the reconciler is not verify-only.
func (r *NFSMountReconciler) MountsShares() bool {
	return r.AutoMount() && r.verifyOnlyReason == ""
}

// noMountReason explains why a share that needs mounting is not mounted
// by the broker.
func (r *NFSMountReconciler) noMountReason() string {
	if !r.AutoMount() {
		return "auto_mount is off, so the export must be mounted externally"
	}
	return r.verifyOnlyReason
}

// mountPrivilegeChecker is implemented by MountCheckers that can tell in
// advance that this process cannot mount (for example, not running as
// root). The reconciler then reports the share without shelling out.
type mountPrivilegeChecker interface {
	MountPrivilegeError() error
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
		cfg:          cfg,
		checker:      checker,
		log:          log,
		statuses:     make(map[string]ShareMountStatus),
		reconcileSem: make(chan struct{}, 1),
	}
}

// Reconcile checks every configured NFS share and, when MountsShares is
// true, mounts or remounts it as needed. It is idempotent: a broker restart
// calls Reconcile again without double-mounting or erroring on an
// already-correct state.
//
// For each configured share (target = <MountRoot>/<share.ID>):
//   - mounted from the expected server:export → healthy
//   - not mounted → mountpoint(1) guard → mkdir -p target → mount
//   - mounted from another source → unmount → mount
//
// Returns an error if no shares are configured or ctx is done. Individual
// share failures are tracked in per-share status (unhealthy) and logged,
// but do not block other shares.
func (r *NFSMountReconciler) Reconcile(ctx context.Context) error {
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

	return r.reconcileShares(ctx, r.cfg.Shares, mountOpts)
}

// reconcileShares reconciles shares as one pass: it holds the reconcile
// semaphore for the whole pass and reads the mount table once, before the
// first share. It returns an error only when ctx is done before or during
// the pass; mount problems are recorded in each share's status.
func (r *NFSMountReconciler) reconcileShares(ctx context.Context, shares []config.V1NFSShare, mountOpts string) error {
	select {
	case r.reconcileSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.reconcileSem }()

	// Each share's mount or remount touches only its own target, so one
	// read serves every share in the pass.
	table, tableErr := r.checker.ReadMountTable()
	for _, share := range shares {
		r.reconcileShareLocked(ctx, share, mountOpts, table, tableErr)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

func (r *NFSMountReconciler) reconcileShareLocked(ctx context.Context, share config.V1NFSShare, mountOpts string, table MountTable, tableErr error) {
	target := filepath.Join(r.cfg.MountRoot, share.ID)
	wantServerExport := fmt.Sprintf("%s:%s", share.Server, share.Export)
	mounts := r.MountsShares()

	// fail records the share as unhealthy, unless ctx (a dispatch request,
	// or the loop at shutdown) is done: a step cut short by the caller says
	// nothing about the share, so the previous status is kept. The exec
	// timeout does not cancel ctx, so a timed-out command is recorded.
	fail := func(msg string) {
		if ctx.Err() != nil {
			r.log.Debug("NFS share check interrupted; status unchanged",
				"shareID", share.ID, "target", target, "reason", msg)
			return
		}
		r.setStatus(share.ID, target, false, msg)
	}

	r.log.Debug("Reconciling NFS share",
		"shareID", share.ID, "target", target,
		"server", share.Server, "export", share.Export,
		"mountsShares", mounts)

	if tableErr != nil {
		fail(fmt.Sprintf("failed to read mount table: %v", tableErr))
		return
	}
	currentServerExport, mounted := table.Lookup(target)

	if mounted && currentServerExport == wantServerExport {
		r.setStatus(share.ID, target, true, "already mounted correctly")
		return
	}

	if !mounted {
		if !mounts {
			fail(fmt.Sprintf("not mounted (expected %s; %s)", wantServerExport, r.noMountReason()))
			return
		}
		if err := r.mountPrivilegeError(); err != nil {
			fail(fmt.Sprintf("not mounted (expected %s) and auto_mount cannot mount it: %v", wantServerExport, err))
			return
		}
		// Guard: the mount table says not mounted; confirm with the kernel
		// before mounting, so a mount the table did not show is never
		// mounted over. A timeout here usually means a hung mount.
		isMP, err := r.checker.IsMountpoint(ctx, target)
		if err != nil {
			fail(fmt.Sprintf("failed to check mountpoint: %v", err))
			return
		}
		if isMP {
			fail(fmt.Sprintf("%s is a mountpoint not listed in the mount table; not mounting over it", target))
			return
		}
		if err := r.checker.MkdirAll(target, 0755); err != nil {
			fail(fmt.Sprintf("failed to create mount directory: %v", err))
			return
		}
		if err := r.checker.Mount(ctx, share.Server, share.Export, target, mountOpts); err != nil {
			fail(fmt.Sprintf("mount failed: %v", err))
			return
		}
		r.setStatus(share.ID, target, true, "mounted successfully")
		r.log.Info("NFS share mounted", "shareID", share.ID, "target", target)
		return
	}

	// Mounted from the wrong source.
	if !mounts {
		reason := "auto_mount is off, so it is not remounted"
		if r.AutoMount() {
			reason = r.verifyOnlyReason
		}
		fail(fmt.Sprintf("mounted from %s, expected %s (%s)", currentServerExport, wantServerExport, reason))
		return
	}
	if err := r.mountPrivilegeError(); err != nil {
		fail(fmt.Sprintf("mounted from %s, expected %s, and auto_mount cannot remount it: %v", currentServerExport, wantServerExport, err))
		return
	}

	r.log.Warn("NFS share mounted with wrong source, remounting",
		"shareID", share.ID, "target", target,
		"current", currentServerExport, "expected", wantServerExport)

	if err := r.checker.Unmount(ctx, target); err != nil {
		fail(fmt.Sprintf("failed to unmount for remount: %v", err))
		return
	}
	if err := r.checker.Mount(ctx, share.Server, share.Export, target, mountOpts); err != nil {
		fail(fmt.Sprintf("remount failed: %v", err))
		return
	}

	r.setStatus(share.ID, target, true, "remounted with correct source")
	r.log.Info("NFS share remounted", "shareID", share.ID, "target", target)
}

// mountPrivilegeError returns why this process cannot mount, when the
// checker can tell in advance; nil otherwise.
func (r *NFSMountReconciler) mountPrivilegeError() error {
	if pc, ok := r.checker.(mountPrivilegeChecker); ok {
		return pc.MountPrivilegeError()
	}
	return nil
}

// setStatus records the health status of a share. A change of state is
// logged; an unchanged status (the periodic re-check finding the same
// problem again) is not, so a persistently failing share does not flood
// the log.
func (r *NFSMountReconciler) setStatus(shareID, target string, healthy bool, message string) {
	r.mu.Lock()
	prev, had := r.statuses[shareID]
	next := ShareMountStatus{
		ShareID: shareID,
		Target:  target,
		Healthy: healthy,
		Message: message,
	}
	r.statuses[shareID] = next
	r.mu.Unlock()

	if had && prev == next {
		return
	}
	if !healthy {
		r.log.Error("NFS share unhealthy",
			"shareID", shareID, "target", target, "reason", message)
	} else if had && !prev.Healthy {
		r.log.Info("NFS share healthy again",
			"shareID", shareID, "target", target, "status", message)
	}
}

// DefaultNFSReconcileInterval is how often Run re-checks the shares after
// the first pass.
const DefaultNFSReconcileInterval = time.Minute

// Run performs a first reconciliation pass immediately, calls firstPassDone
// (if non-nil), then re-runs Reconcile every interval until ctx is
// cancelled. In check-only mode this keeps /healthz current when an operator
// mounts or unmounts a share; with AutoMount it also restores a dropped
// mount. Run never returns an error: failures are per-share status.
func (r *NFSMountReconciler) Run(ctx context.Context, interval time.Duration, firstPassDone func()) {
	if interval <= 0 {
		interval = DefaultNFSReconcileInterval
	}
	if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
		r.log.Warn("NFS mount reconciliation returned error", "error", err)
	}
	if firstPassDone != nil {
		firstPassDone()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
				r.log.Warn("NFS mount reconciliation returned error", "error", err)
			}
		}
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

// ShareStatus returns the last recorded status of a share, and false if it
// has not been checked yet. It never checks or mounts.
func (r *NFSMountReconciler) ShareStatus(shareID string) (ShareMountStatus, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.statuses[shareID]
	return st, ok
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

// EnsureShareMounted is called before each NFS-backed dispatch to a
// local-container runtime to verify the share for a given share ID is still
// mounted. It re-reconciles the share, mounting it when MountsShares is true.
// Returns an error if the share cannot be verified or mounted, or if ctx
// (the dispatch request's context) is done first; ctx also bounds any mount
// command it runs.
func (r *NFSMountReconciler) EnsureShareMounted(ctx context.Context, shareID string) error {
	if r.cfg == nil {
		return fmt.Errorf("NFS config is nil")
	}

	mountOpts := r.cfg.MountOptions
	if mountOpts == "" {
		mountOpts = "vers=3,hard,nconnect=4,_netdev"
	}

	for _, share := range r.cfg.Shares {
		if share.ID == shareID {
			if err := r.reconcileShares(ctx, []config.V1NFSShare{share}, mountOpts); err != nil {
				return fmt.Errorf("NFS share %q not checked: %w", shareID, err)
			}
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("NFS share %q not checked: %w", shareID, err)
			}

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
