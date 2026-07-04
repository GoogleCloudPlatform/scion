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

const defaultUpdateTimeout = 10 * time.Minute

// updateTimeoutEntry tracks a pending update timeout.
type updateTimeoutEntry struct {
	timer    *time.Timer
	updateID string
}

// pendingUpdateTracker manages update timeout timers and reconnect-based
// completion detection for HA integrations.
type pendingUpdateTracker struct {
	mu      sync.Mutex
	pending map[string]*updateTimeoutEntry // integration name -> timeout entry
}

func newPendingUpdateTracker() *pendingUpdateTracker {
	return &pendingUpdateTracker{
		pending: make(map[string]*updateTimeoutEntry),
	}
}

// startUpdateTimeout starts a timeout timer for an HA update. If the update
// is not completed before the timeout, it is marked as failed.
func (s *Server) startUpdateTimeout(integrationName, updateID string) {
	if s.updateTracker == nil {
		return
	}

	s.updateTracker.mu.Lock()
	defer s.updateTracker.mu.Unlock()

	// Cancel any existing timer for this integration.
	if existing, ok := s.updateTracker.pending[integrationName]; ok {
		existing.timer.Stop()
	}

	timer := time.AfterFunc(defaultUpdateTimeout, func() {
		s.handleUpdateTimeout(integrationName, updateID)
	})

	s.updateTracker.pending[integrationName] = &updateTimeoutEntry{
		timer:    timer,
		updateID: updateID,
	}
}

// handleUpdateTimeout marks an update as failed due to timeout.
func (s *Server) handleUpdateTimeout(integrationName, updateID string) {
	if s.entClient == nil {
		return
	}

	s.updateTracker.mu.Lock()
	delete(s.updateTracker.pending, integrationName)
	s.updateTracker.mu.Unlock()

	uid, err := uuid.Parse(updateID)
	if err != nil {
		slog.Error("Invalid update ID in timeout handler", "id", updateID, "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Only mark as failed if still in a non-terminal state.
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

// checkUpdateCompletionOnReconnect is called when a gRPC adapter reconnects.
// It checks if there is a pending update for the integration and compares
// the new version against the pre-update version stored in the update row.
func (s *Server) checkUpdateCompletionOnReconnect(integrationName string) {
	if s.entClient == nil || s.updateTracker == nil {
		return
	}

	s.updateTracker.mu.Lock()
	entry, ok := s.updateTracker.pending[integrationName]
	if !ok {
		s.updateTracker.mu.Unlock()
		return
	}
	updateID := entry.updateID
	s.updateTracker.mu.Unlock()

	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	if mgr == nil {
		return
	}

	// Get current version from the reconnected integration.
	newVersion, _, _, err := mgr.BrokerInfo(integrationName)
	if err != nil {
		slog.Warn("Failed to get broker info after reconnect",
			"integration", integrationName, "error", err)
		return
	}

	uid, err := uuid.Parse(updateID)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Read the update row to get the pre-update version.
	row, err := s.entClient.IntegrationUpdate.Get(ctx, uid)
	if err != nil {
		slog.Error("Failed to read update row for completion check",
			"integration", integrationName, "id", updateID, "error", err)
		return
	}

	// Already in a terminal state — nothing to do.
	if row.State == integrationupdate.StateCompleted || row.State == integrationupdate.StateFailed {
		return
	}

	// Extract pre-update version from the detail field.
	preUpdateVersion := ""
	if strings.HasPrefix(row.Detail, "pre_update_version=") {
		preUpdateVersion = strings.TrimPrefix(row.Detail, "pre_update_version=")
	}

	// Version changed → update completed.
	if newVersion != "" && newVersion != preUpdateVersion {
		s.updateTracker.mu.Lock()
		if e, ok := s.updateTracker.pending[integrationName]; ok {
			e.timer.Stop()
			delete(s.updateTracker.pending, integrationName)
		}
		s.updateTracker.mu.Unlock()

		_, err := s.entClient.IntegrationUpdate.
			UpdateOneID(uid).
			SetState(integrationupdate.StateCompleted).
			SetNewVersion(newVersion).
			SetDetail("").
			Save(ctx)
		if err != nil {
			slog.Error("Failed to mark update as completed",
				"integration", integrationName, "id", updateID, "error", err)
			return
		}
		slog.Info("Update completed — version changed after reconnect",
			"integration", integrationName, "old_version", preUpdateVersion,
			"new_version", newVersion)
		return
	}

	slog.Info("Integration reconnected but version unchanged, waiting for timeout",
		"integration", integrationName, "version", newVersion)
}

// registerReconnectCallbacks sets up reconnect callbacks on all HA integration
// adapters to enable update completion detection.
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
			s.checkUpdateCompletionOnReconnect(integrationName)
		})
	}
}
