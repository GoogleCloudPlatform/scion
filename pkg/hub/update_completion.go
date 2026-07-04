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
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/integrationupdate"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
)

const (
	defaultUpdateTimeout  = 10 * time.Minute
	defaultPollInterval   = 20 * time.Second
)

// pendingUpdateEntry tracks a single pending update with its poll goroutine
// and timeout timer.
type pendingUpdateEntry struct {
	updateID          string
	preUpdateVersion  string
	cancel            context.CancelFunc
	timer             *time.Timer
}

// pendingUpdateTracker manages poll-based completion detection for HA updates.
// While an update is pending, a goroutine polls BrokerInfo at a fixed interval
// and compares the version. OnReconnect callbacks serve as an optional
// fast-path hint to trigger an immediate poll.
type pendingUpdateTracker struct {
	mu      sync.Mutex
	pending map[string]*pendingUpdateEntry // integration name -> entry
}

func newPendingUpdateTracker() *pendingUpdateTracker {
	return &pendingUpdateTracker{
		pending: make(map[string]*pendingUpdateEntry),
	}
}

// hasPendingUpdate returns true if a non-terminal update is tracked for the
// given integration.
func (t *pendingUpdateTracker) hasPendingUpdate(integrationName string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.pending[integrationName]
	return ok
}

// startUpdateTracking starts poll-based completion detection for an HA update.
// A goroutine polls BrokerInfo every ~20s. A timeout timer marks the update
// failed after defaultUpdateTimeout if the version hasn't changed.
func (s *Server) startUpdateTracking(integrationName, updateID, preUpdateVersion string) {
	if s.updateTracker == nil {
		return
	}

	s.updateTracker.mu.Lock()
	defer s.updateTracker.mu.Unlock()

	// Cancel any existing tracking for this integration.
	if existing, ok := s.updateTracker.pending[integrationName]; ok {
		existing.cancel()
		existing.timer.Stop()
	}

	ctx, cancel := context.WithCancel(context.Background())

	timer := time.AfterFunc(defaultUpdateTimeout, func() {
		s.handleUpdateTimeout(integrationName, updateID)
	})

	s.updateTracker.pending[integrationName] = &pendingUpdateEntry{
		updateID:         updateID,
		preUpdateVersion: preUpdateVersion,
		cancel:           cancel,
		timer:            timer,
	}

	go s.pollUpdateCompletion(ctx, integrationName, updateID, preUpdateVersion)
}

// pollUpdateCompletion periodically checks BrokerInfo for a version change.
func (s *Server) pollUpdateCompletion(ctx context.Context, integrationName, updateID, preUpdateVersion string) {
	ticker := time.NewTicker(defaultPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkUpdateCompletion(integrationName, updateID, preUpdateVersion)
		}
	}
}

// triggerImmediatePoll is called on reconnect as a fast-path hint.
// It triggers an immediate completion check outside the regular poll interval.
func (s *Server) triggerImmediatePoll(integrationName string) {
	if s.updateTracker == nil {
		return
	}

	s.updateTracker.mu.Lock()
	entry, ok := s.updateTracker.pending[integrationName]
	if !ok {
		s.updateTracker.mu.Unlock()
		return
	}
	updateID := entry.updateID
	preUpdateVersion := entry.preUpdateVersion
	s.updateTracker.mu.Unlock()

	go s.checkUpdateCompletion(integrationName, updateID, preUpdateVersion)
}

// checkUpdateCompletion checks if the integration version has changed,
// indicating the update completed successfully.
func (s *Server) checkUpdateCompletion(integrationName, updateID, preUpdateVersion string) {
	if s.entClient == nil {
		return
	}

	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	if mgr == nil {
		return
	}

	newVersion, _, _, err := mgr.BrokerInfo(integrationName)
	if err != nil {
		slog.Debug("BrokerInfo poll failed (integration may be restarting)",
			"integration", integrationName, "error", err)
		return
	}

	uid, err := uuid.Parse(updateID)
	if err != nil {
		return
	}

	// Empty pre-update version means we couldn't capture baseline —
	// treat as inconclusive and let the timeout decide.
	if preUpdateVersion == "" {
		slog.Debug("No pre-update version baseline, skipping completion check",
			"integration", integrationName)
		return
	}

	// Version unchanged — continue polling.
	if newVersion == preUpdateVersion {
		return
	}

	// Version changed — mark completed (guarded write).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	affected, err := s.entClient.IntegrationUpdate.
		Update().
		Where(
			integrationupdate.IDEQ(uid),
			integrationupdate.StateNotIn(
				integrationupdate.StateCompleted,
				integrationupdate.StateFailed,
			),
		).
		SetState(integrationupdate.StateCompleted).
		SetNewVersion(newVersion).
		SetDetail("").
		Save(ctx)
	if err != nil {
		slog.Error("Failed to mark update as completed",
			"integration", integrationName, "id", updateID, "error", err)
		return
	}
	if affected == 0 {
		return
	}

	slog.Info("Update completed — version changed",
		"integration", integrationName, "old_version", preUpdateVersion,
		"new_version", newVersion)

	// Clean up tracker entry only if updateID matches.
	s.updateTracker.mu.Lock()
	if e, ok := s.updateTracker.pending[integrationName]; ok && e.updateID == updateID {
		e.cancel()
		e.timer.Stop()
		delete(s.updateTracker.pending, integrationName)
	}
	s.updateTracker.mu.Unlock()
}

// handleUpdateTimeout marks an update as failed due to timeout.
func (s *Server) handleUpdateTimeout(integrationName, updateID string) {
	if s.entClient == nil {
		return
	}

	// Only delete tracker entry if updateID matches.
	s.updateTracker.mu.Lock()
	if e, ok := s.updateTracker.pending[integrationName]; ok && e.updateID == updateID {
		e.cancel()
		delete(s.updateTracker.pending, integrationName)
	}
	s.updateTracker.mu.Unlock()

	uid, err := uuid.Parse(updateID)
	if err != nil {
		slog.Error("Invalid update ID in timeout handler", "id", updateID, "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	affected, err := s.entClient.IntegrationUpdate.
		Update().
		Where(
			integrationupdate.IDEQ(uid),
			integrationupdate.StateNotIn(
				integrationupdate.StateCompleted,
				integrationupdate.StateFailed,
			),
		).
		SetState(integrationupdate.StateFailed).
		SetDetail("Update timed out — version unchanged after restart").
		Save(ctx)
	if err != nil {
		slog.Error("Failed to mark update as timed out",
			"integration", integrationName, "id", updateID, "error", err)
		return
	}
	if affected > 0 {
		slog.Warn("Update timed out",
			"integration", integrationName, "id", updateID)
	}
}

// registerReconnectCallbacks sets up reconnect callbacks on all HA integration
// adapters. Callbacks serve as fast-path hints to trigger an immediate
// completion check — they are not the sole detection mechanism.
func (s *Server) registerReconnectCallbacks(mgr IntegrationManager) {
	for _, key := range mgr.ListPlugins() {
		name := pluginNameFromKey(key)
		if name == "" {
			continue
		}
		if mgr.GetDeploymentMode("broker", name) != plugin.DeploymentModeHA {
			continue
		}
		adapter := mgr.GetGRPCBrokerAdapter(name)
		if adapter == nil {
			continue
		}
		integrationName := name
		adapter.OnReconnect(func() {
			s.triggerImmediatePoll(integrationName)
		})
	}
}

// sweepOrphanedUpdates recovers non-terminal integration_updates rows that
// were pending when the hub last shut down. Recent rows get their tracking
// re-armed; old rows are marked failed.
func (s *Server) sweepOrphanedUpdates() {
	if s.entClient == nil || s.updateTracker == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := s.entClient.IntegrationUpdate.Query().
		Where(
			integrationupdate.StateNotIn(
				integrationupdate.StateCompleted,
				integrationupdate.StateFailed,
			),
		).
		All(ctx)
	if err != nil {
		slog.Error("Failed to sweep orphaned integration updates", "error", err)
		return
	}

	if len(rows) == 0 {
		return
	}

	now := time.Now()
	for _, row := range rows {
		age := now.Sub(row.CreateTime)
		if age >= defaultUpdateTimeout {
			// Too old — mark failed.
			_, fErr := s.entClient.IntegrationUpdate.
				Update().
				Where(
					integrationupdate.IDEQ(row.ID),
					integrationupdate.StateNotIn(
						integrationupdate.StateCompleted,
						integrationupdate.StateFailed,
					),
				).
				SetState(integrationupdate.StateFailed).
				SetDetail("Update orphaned — hub restarted after timeout expired").
				Save(ctx)
			if fErr != nil {
				slog.Error("Failed to fail-mark orphaned update",
					"integration", row.Integration, "id", row.ID, "error", fErr)
			} else {
				slog.Info("Marked orphaned update as failed",
					"integration", row.Integration, "id", row.ID, "age", age)
			}
			continue
		}

		// Recent enough — re-arm tracking with remaining timeout.
		preUpdateVersion := ""
		if strings.HasPrefix(row.Detail, "pre_update_version=") {
			preUpdateVersion = strings.TrimPrefix(row.Detail, "pre_update_version=")
		}

		remaining := defaultUpdateTimeout - age
		s.rearmUpdateTracking(row.Integration, row.ID.String(), preUpdateVersion, remaining)
		slog.Info("Re-armed tracking for orphaned update",
			"integration", row.Integration, "id", row.ID,
			"remaining_timeout", remaining)
	}
}

// rearmUpdateTracking re-arms tracking for an update with a custom timeout.
func (s *Server) rearmUpdateTracking(integrationName, updateID, preUpdateVersion string, timeout time.Duration) {
	if s.updateTracker == nil {
		return
	}

	s.updateTracker.mu.Lock()
	defer s.updateTracker.mu.Unlock()

	if existing, ok := s.updateTracker.pending[integrationName]; ok {
		existing.cancel()
		existing.timer.Stop()
	}

	ctx, cancelFn := context.WithCancel(context.Background())

	timer := time.AfterFunc(timeout, func() {
		s.handleUpdateTimeout(integrationName, updateID)
	})

	s.updateTracker.pending[integrationName] = &pendingUpdateEntry{
		updateID:         updateID,
		preUpdateVersion: preUpdateVersion,
		cancel:           cancelFn,
		timer:            timer,
	}

	go s.pollUpdateCompletion(ctx, integrationName, updateID, preUpdateVersion)
}
