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

package liveness

import (
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

func TestSampler_AttachedTerminalGuard(t *testing.T) {
	attached := false
	s := NewSampler(Config{
		AgentUID:      1 << 30,
		IsManagedPID:  func(int) bool { return false },
		CheckAttached: func() bool { return attached },
	})
	s.procRoot = t.TempDir()

	// Not attached -> nil.
	if got := s.sample(time.Now(), state.ActivityIdle, time.Now()); got != nil {
		t.Fatalf("expected nil when not attached, got %+v", got)
	}

	// Attached -> KindAttached, SuppressStall=false, Detail="terminal attached".
	attached = true
	got := s.sample(time.Now(), state.ActivityIdle, time.Now())
	if got == nil {
		t.Fatal("expected LiveWork when attached, got nil")
	}
	if got.Kind != KindAttached {
		t.Errorf("Kind = %q, want %q", got.Kind, KindAttached)
	}
	if got.SuppressStall {
		t.Error("SuppressStall = true, want false")
	}
	if got.Detail != "terminal attached" {
		t.Errorf("Detail = %q, want %q", got.Detail, "terminal attached")
	}

	// Detached again -> nil.
	attached = false
	if got := s.sample(time.Now(), state.ActivityIdle, time.Now()); got != nil {
		t.Fatalf("expected nil after detach, got %+v", got)
	}
}
