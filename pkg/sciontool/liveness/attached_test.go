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

func TestSampler_AttachedTakesPrecedenceOverBusyProcess(t *testing.T) {
	attached := true
	s := NewSampler(Config{
		AgentUID:      agentUID,
		IsManagedPID:  func(int) bool { return false },
		CheckAttached: func() bool { return attached },
	})
	f := &fakeProcTree{t: t, sampler: s}

	tree := []fakeProc{
		{pid: 1, ppid: 0, comm: "sciontool", uid: 0, ticks: 1, start: 1},
		{pid: 40, ppid: 1, comm: "tmux: server", uid: agentUID, ticks: 10, start: 10},
		{pid: 41, ppid: 40, comm: "sh", uid: agentUID, ticks: 10, start: 11, cmdline: wrapperCmdline},
		{pid: 42, ppid: 41, comm: "bash", uid: agentUID, ticks: 10, start: 12},
		{pid: 50, ppid: 42, comm: "make", uid: agentUID, ticks: 10, start: 20},
	}

	// First sample while attached: records CPU baseline and reports KindAttached.
	got := f.sample(tree...)
	if got == nil || got.Kind != KindAttached {
		t.Fatalf("initial attached sample = %+v, want KindAttached", got)
	}

	// Busy CPU work while still attached: KindAttached still wins so the hub
	// refreshes the shell's idle-suspend activity_since window.
	tree[4].ticks += 500
	got = f.sample(tree...)
	if got == nil || got.Kind != KindAttached {
		t.Fatalf("busy attached sample = %+v, want KindAttached", got)
	}

	// After detach, the still-busy process is reported as KindProcess using
	// the CPU baseline recorded during the attached sample.
	attached = false
	tree[4].ticks += 500
	got = f.sample(tree...)
	if got == nil || got.Kind != KindProcess {
		t.Fatalf("busy detached sample = %+v, want KindProcess", got)
	}
}
