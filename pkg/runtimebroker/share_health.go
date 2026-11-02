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
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// shareHealthCheck is the /healthz check key for the project storage share
// on a shared Layout. It is present only on a shared Layout, once the
// first check has run.
const shareHealthCheck = "project_storage"

// shareHealthNotVisible is the leading text of the check's value when the
// share is not visible. Its leading word ("unhealthy") is what the
// heartbeat carries (api.NormalizeBrokerHealthReport); the rest names
// configuration fields only, never the path.
const shareHealthNotVisible = "unhealthy: share_not_visible"

// shareHealthInterval is how often the share's visibility is re-checked
// after the first check at start. A variable so tests can shorten it.
var shareHealthInterval = DefaultNFSReconcileInterval

// shareHealthState is the last result of the share-visibility check and
// the loop that refreshes it.
type shareHealthState struct {
	once   sync.Once
	cancel context.CancelFunc

	mu      sync.Mutex
	checked bool
	err     error
}

// shareHealthPath is the directory whose visibility decides the check:
// the share's subpath root (<HostBase>/<subpath_root>), the directory
// every project tree lives in. A mount point without it is not the share
// (the same rule as the start-time refusal in sharedHubProjectPath).
func shareHealthPath(layout projectfs.Layout) string {
	return projectfs.ShareSubPathRootDir(layout.HostBase, layout.SubPathRoot)
}

// checkShareHealth re-evaluates whether the project storage share is
// visible on this broker and records the result for GetHealthInfo. It is a
// no-op off a shared Layout. A change to not visible is logged once at
// ERROR with the remedy; a change back is logged at INFO. A check cut
// short by ctx records nothing. The Layout is not re-resolved.
func (s *Server) checkShareHealth(ctx context.Context) {
	layout := s.config.StorageLayout
	if !layout.Backend.Shared() {
		return
	}
	path := shareHealthPath(layout)
	err := projectfs.ShareDirVisible(ctx, path)
	if ctx.Err() != nil {
		return
	}
	c := &s.shareHealth
	c.mu.Lock()
	wasVisible := c.checked && c.err == nil
	wasNotVisible := c.checked && c.err != nil
	c.checked, c.err = true, err
	c.mu.Unlock()
	switch {
	case err != nil && !wasNotVisible:
		slog.Error("Project storage share is not visible on this broker; creates in shared-storage projects are refused until it is",
			"path", path, "error", err,
			"remedy", scionrt.ProjectShareUnavailableText(layout.HostBase, NFSWarnOnlyRuntime(s.RuntimeName())))
	case err == nil && wasNotVisible:
		slog.Info("Project storage share is visible on this broker again", "path", path)
	case err == nil && !wasVisible:
		slog.Info("Project storage share is visible on this broker", "path", path)
	}
}

// shareHealthValue returns the /healthz value of the share check and
// whether it applies: not on a non-shared Layout, and not before the
// first check.
func (s *Server) shareHealthValue() (value string, notVisible, ok bool) {
	if !s.config.StorageLayout.Backend.Shared() {
		return "", false, false
	}
	c := &s.shareHealth
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.checked {
		return "", false, false
	}
	if c.err == nil {
		return "healthy", false, true
	}
	return shareHealthNotVisible + " (" + s.shareUnavailableText() + ")", true, true
}

// startShareHealthLoop checks the share once now (after the NFS
// reconciler's first pass on a broker that mounts the share) and then every
// shareHealthInterval until ctx is cancelled or Shutdown is called, so a
// share made visible later clears the check without a restart. It is a
// no-op off a shared Layout and starts at most one loop per Server.
func (s *Server) startShareHealthLoop(ctx context.Context) {
	if !s.config.StorageLayout.Backend.Shared() {
		return
	}
	c := &s.shareHealth
	c.once.Do(func() {
		loopCtx, cancel := context.WithCancel(ctx)
		c.mu.Lock()
		c.cancel = cancel
		c.mu.Unlock()
		go func() {
			// A broker that mounts the share does so in the reconciler's
			// first pass; checking before it finishes would report a
			// share about to be mounted as not visible.
			if r, done := s.nfsMountReconciler, s.nfsStartupReconcileDone; r != nil && r.MountsShares() && done != nil {
				select {
				case <-done:
				case <-loopCtx.Done():
					return
				}
			}
			s.checkShareHealth(loopCtx)
			ticker := time.NewTicker(shareHealthInterval)
			defer ticker.Stop()
			for {
				select {
				case <-loopCtx.Done():
					return
				case <-ticker.C:
					s.checkShareHealth(loopCtx)
				}
			}
		}()
	})
}

// stopShareHealthLoop stops the loop started by startShareHealthLoop, if
// any. It does not wait for a check in progress (bounded by
// projectfs.ShareVisibleTimeout).
func (s *Server) stopShareHealthLoop() {
	c := &s.shareHealth
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
