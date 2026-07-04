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

func TestStartUpdateTimeout_StoresEntry(t *testing.T) {
	srv := &Server{
		updateTracker: newPendingUpdateTracker(),
	}

	srv.startUpdateTimeout("discord", "test-update-id")

	srv.updateTracker.mu.Lock()
	defer srv.updateTracker.mu.Unlock()

	entry, ok := srv.updateTracker.pending["discord"]
	if !ok {
		t.Fatal("expected pending entry for discord")
	}
	if entry.updateID != "test-update-id" {
		t.Errorf("expected update ID test-update-id, got %q", entry.updateID)
	}
	entry.timer.Stop()
}

func TestStartUpdateTimeout_ReplacesExisting(t *testing.T) {
	srv := &Server{
		updateTracker: newPendingUpdateTracker(),
	}

	srv.startUpdateTimeout("discord", "first-id")
	srv.startUpdateTimeout("discord", "second-id")

	srv.updateTracker.mu.Lock()
	defer srv.updateTracker.mu.Unlock()

	entry, ok := srv.updateTracker.pending["discord"]
	if !ok {
		t.Fatal("expected pending entry for discord")
	}
	if entry.updateID != "second-id" {
		t.Errorf("expected second-id, got %q", entry.updateID)
	}
	entry.timer.Stop()
}

func TestStartUpdateTimeout_NilTracker(t *testing.T) {
	srv := &Server{}
	// Should not panic.
	srv.startUpdateTimeout("discord", "test-id")
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
