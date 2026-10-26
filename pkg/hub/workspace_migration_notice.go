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

package hub

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// WorkspaceMigrationNotice is shown as a banner on the project Files tab
// while the hub's legacy workspace trees have not yet been migrated into
// the project storage layout (ptone/scion#5819).
type WorkspaceMigrationNotice struct {
	Message      string `json:"message"`
	OperationKey string `json:"operationKey"`
}

// workspaceMigrationNoticeMessage is the banner text. It names no path.
const workspaceMigrationNoticeMessage = "Some files of this project are still in the hub's previous storage location and are not shown here. " +
	"A hub administrator can move them with the Project Storage Migration maintenance operation."

// workspaceMigrationIdentityMessage is added when a legacy tree of the
// project could not be verified as the project's own (slug reuse): the
// administrator must review it, and may adopt it per project.
const workspaceMigrationIdentityMessage = " The previous location could not be verified as belonging to this project; " +
	"an administrator must review the migration log and may adopt it by running the operation with the adoptUnverified parameter set to this project's slug."

// Bounds of the background plan behind the notice: the plan walks the
// share, so it never runs on a request path, at most one runs at a time,
// and a failed or timed-out plan is retried only after a pause.
var (
	storageMigrationNoticeTimeout = 2 * time.Minute
	storageMigrationNoticeRetry   = 5 * time.Minute
)

// storageMigrationNoticeCache holds the last dry-run plan of the project
// storage migration: the IDs of projects with something left to move, for
// the Layouts it was computed against.
type storageMigrationNoticeCache struct {
	mu       sync.Mutex
	key      string            // Layout identity of the plan; "" = none
	pending  map[string]string // project ID -> pending reason ("" or noticeReasonIdentity)
	running  bool
	failedAt time.Time
	gen      uint64 // bumped by invalidate; a plan started earlier is dropped
}

// invalidate drops the cached plan (after a real run of the operation).
func (c *storageMigrationNoticeCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.key, c.pending, c.failedAt = "", nil, time.Time{}
	c.gen++
}

// workspaceMigrationNotice returns the banner for project's Files tab, or
// nil when there is none. The notice shows while all of these hold: the
// hub's workspace backend is shared, the migration has not completed, and
// the cached dry-run plan lists the project as having legacy content. The
// plan is computed in the background (bounded, one at a time, never
// blocking the request or readiness) and recomputed when the Layouts
// change or the operation runs; until it is known, no notice is shown.
func (s *Server) workspaceMigrationNotice(ctx context.Context, project *store.Project) *WorkspaceMigrationNotice {
	if project == nil || !s.workspaceBackendShared() {
		return nil
	}
	op, err := s.store.GetMaintenanceOperation(ctx, projectStorageMigrateKey)
	switch {
	case err == nil && op.Status == store.MaintenanceStatusCompleted:
		return nil
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return nil
	}
	key, err := s.storageMigrationLayoutKey()
	if err != nil {
		return nil
	}
	pending, reason := s.storageMigrationPending(key, project.ID)
	if !pending {
		return nil
	}
	msg := workspaceMigrationNoticeMessage
	if reason == noticeReasonIdentity {
		msg += workspaceMigrationIdentityMessage
	}
	return &WorkspaceMigrationNotice{Message: msg, OperationKey: projectStorageMigrateKey}
}

// storageMigrationLayoutKey identifies the Layouts a plan is valid for.
func (s *Server) storageMigrationLayoutKey() (string, error) {
	ws, err := s.workspaceLayout()
	if err != nil {
		return "", err
	}
	sd, _, err := s.sharedDirLayout()
	if err != nil {
		return "", err
	}
	// Value keys: a Layout holds the SharedDirs pointer, so %#v would differ
	// on every call for the split configurations.
	return ws.Key() + "||" + sd.Key(), nil
}

// noticeReasonIdentity marks a project whose legacy tree failed the
// identity check (mismatch or unverified).
const noticeReasonIdentity = "identity"

// storageMigrationPending reports whether the cached plan for key lists
// projectID (and why), starting a background plan when there is none for
// key.
func (s *Server) storageMigrationPending(key, projectID string) (bool, string) {
	c := &s.storageMigrationNotice
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key == key && c.pending != nil {
		reason, ok := c.pending[projectID]
		return ok, reason
	}
	if !c.running && (c.failedAt.IsZero() || time.Since(c.failedAt) >= storageMigrationNoticeRetry) {
		c.running = true
		go s.computeStorageMigrationPlan(key, c.gen)
	}
	return false, ""
}

// startStorageMigrationNoticePlan starts the notice's plan at hub start, in
// the background, when the workspace backend is shared and the migration
// has not completed. It never blocks start or readiness.
func (s *Server) startStorageMigrationNoticePlan(ctx context.Context) {
	if !s.workspaceBackendShared() {
		return
	}
	go func() {
		op, err := s.store.GetMaintenanceOperation(ctx, projectStorageMigrateKey)
		if err == nil && op.Status == store.MaintenanceStatusCompleted {
			return
		}
		key, err := s.storageMigrationLayoutKey()
		if err != nil {
			return
		}
		s.storageMigrationPending(key, "")
	}()
}

// computeStorageMigrationPlan runs the migration as a dry run (no object
// storage listing) and caches the projects with something to move.
func (s *Server) computeStorageMigrationPlan(key string, gen uint64) {
	c := &s.storageMigrationNotice
	ctx, cancel := context.WithTimeout(context.Background(), storageMigrationNoticeTimeout)
	defer cancel()
	pending, err := s.planStorageMigration(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running = false
	if gen != c.gen {
		return // invalidated while planning
	}
	if err != nil {
		c.failedAt = time.Now()
		s.projectsLogger().Warn("project storage migration: notice plan failed; retrying later",
			"error_class", fsErrorClass(err), "retry_after", storageMigrationNoticeRetry)
		return
	}
	c.key, c.pending, c.failedAt = key, pending, time.Time{}
}
