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
	"testing"
)

func TestNewPendingUpdateTracker(t *testing.T) {
	tracker := newPendingUpdateTracker()
	if tracker == nil {
		t.Fatal("expected non-nil tracker")
	}
	if len(tracker.pending) != 0 {
		t.Errorf("expected empty pending map, got %d", len(tracker.pending))
	}
}

func TestStartUpdateTracking_StoresEntry(t *testing.T) {
	srv := &Server{
		updateTracker: newPendingUpdateTracker(),
	}

	srv.startUpdateTracking("discord", "test-update-id", "1.0.0")

	srv.updateTracker.mu.Lock()
	defer srv.updateTracker.mu.Unlock()

	entry, ok := srv.updateTracker.pending["discord"]
	if !ok {
		t.Fatal("expected pending entry for discord")
	}
	if entry.updateID != "test-update-id" {
		t.Errorf("expected update ID test-update-id, got %q", entry.updateID)
	}
	if entry.preUpdateVersion != "1.0.0" {
		t.Errorf("expected preUpdateVersion 1.0.0, got %q", entry.preUpdateVersion)
	}
	entry.cancel()
	entry.timer.Stop()
}

func TestStartUpdateTracking_ReplacesExisting(t *testing.T) {
	srv := &Server{
		updateTracker: newPendingUpdateTracker(),
	}

	srv.startUpdateTracking("discord", "first-id", "1.0.0")
	srv.startUpdateTracking("discord", "second-id", "1.1.0")

	srv.updateTracker.mu.Lock()
	defer srv.updateTracker.mu.Unlock()

	entry, ok := srv.updateTracker.pending["discord"]
	if !ok {
		t.Fatal("expected pending entry for discord")
	}
	if entry.updateID != "second-id" {
		t.Errorf("expected second-id, got %q", entry.updateID)
	}
	entry.cancel()
	entry.timer.Stop()
}

func TestStartUpdateTracking_NilTracker(t *testing.T) {
	srv := &Server{}
	// Should not panic.
	srv.startUpdateTracking("discord", "test-id", "1.0.0")
}

func TestHasPendingUpdate(t *testing.T) {
	tracker := newPendingUpdateTracker()

	if tracker.hasPendingUpdate("discord") {
		t.Error("expected no pending update initially")
	}

	srv := &Server{updateTracker: tracker}
	srv.startUpdateTracking("discord", "test-id", "1.0.0")

	if !tracker.hasPendingUpdate("discord") {
		t.Error("expected pending update after tracking started")
	}

	// Clean up.
	tracker.mu.Lock()
	e := tracker.pending["discord"]
	e.cancel()
	e.timer.Stop()
	tracker.mu.Unlock()
}

func TestTriggerImmediatePoll_NoPending(t *testing.T) {
	srv := &Server{
		updateTracker: newPendingUpdateTracker(),
	}
	// Should not panic when no pending update exists.
	srv.triggerImmediatePoll("discord")
}

func TestTriggerImmediatePoll_NilTracker(t *testing.T) {
	srv := &Server{}
	// Should not panic with nil tracker.
	srv.triggerImmediatePoll("discord")
}

func TestHandleUpdateTimeout_UpdateIDMismatch(t *testing.T) {
	tracker := newPendingUpdateTracker()
	srv := &Server{updateTracker: tracker}

	srv.startUpdateTracking("discord", "current-id", "1.0.0")

	// Simulate a stale timeout for a different updateID.
	// Should not delete the current entry.
	srv.handleUpdateTimeout("discord", "stale-id")

	tracker.mu.Lock()
	_, ok := tracker.pending["discord"]
	tracker.mu.Unlock()

	if !ok {
		t.Error("expected current entry to survive stale timeout")
	}

	// Clean up.
	tracker.mu.Lock()
	e := tracker.pending["discord"]
	e.cancel()
	e.timer.Stop()
	tracker.mu.Unlock()
}

func TestRegisterReconnectCallbacks_SkipsNonHA(t *testing.T) {
	mgr := newMockIntegrationManager()
	mgr.plugins["telegram"] = map[string]string{}

	srv := &Server{
		updateTracker: newPendingUpdateTracker(),
	}

	// Should not panic even though telegram is not HA.
	srv.registerReconnectCallbacks(mgr)
}
