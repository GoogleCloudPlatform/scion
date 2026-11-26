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

// TestSampler_ExecLiveWorkPreservesStallSuppression verifies R2 / §4.10:
// active or uncollected Conduit exec work guards auto-suspend (KindExec,
// SuppressStall=false) when the agent is otherwise idle, without masking
// Tier 1 (tool call) or Tier 2 (CPU process) stall suppression.
func TestSampler_ExecLiveWorkPreservesStallSuppression(t *testing.T) {
	var execWork *LiveWork
	s := NewSampler(Config{
		AgentUID:     agentUID,
		IsManagedPID: func(int) bool { return false },
		CheckExec:    func() *LiveWork { return execWork },
	})
	f := &fakeProcTree{t: t, sampler: s}

	tree := []fakeProc{
		{pid: 1, ppid: 0, comm: "sciontool", uid: 0, ticks: 1, start: 1},
		{pid: 40, ppid: 1, comm: "tmux: server", uid: agentUID, ticks: 10, start: 10},
		{pid: 41, ppid: 40, comm: "sh", uid: agentUID, ticks: 10, start: 11, cmdline: wrapperCmdline},
		{pid: 42, ppid: 41, comm: "claude", uid: agentUID, ticks: 10, start: 12},
		{pid: 50, ppid: 42, comm: "make", uid: agentUID, ticks: 10, start: 20},
	}

	// Idle with no exec work -> records CPU baseline, returns nil.
	if got := f.sample(tree...); got != nil {
		t.Fatalf("expected nil with no exec work, got %+v", got)
	}

	// Exec work active while otherwise idle -> KindExec with SuppressStall=false.
	execWork = &LiveWork{Kind: KindExec, SuppressStall: false, Detail: "1 active exec run"}
	got := f.sample(tree...)
	if got == nil || got.Kind != KindExec {
		t.Fatalf("idle with exec work = %+v, want KindExec", got)
	}
	if got.SuppressStall {
		t.Error("KindExec SuppressStall = true, want false")
	}

	// Exec work active + Tier 2 busy CPU process -> Tier 2 wins so stall
	// suppression is preserved (SuppressStall=true).
	tree[4].ticks += 500
	got = f.sample(tree...)
	if got == nil || got.Kind != KindProcess || !got.SuppressStall {
		t.Fatalf("busy CPU + exec work = %+v, want KindProcess with SuppressStall=true", got)
	}

	// Exec work active + Tier 1 live tool call -> Tier 1 wins so stall
	// suppression is preserved (SuppressStall=true).
	boot := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	s.boot = boot
	actSince := boot.Add(150 * time.Millisecond) // tool pid 50 started at tick 20 (200ms after boot)
	got = s.sample(boot.Add(5*time.Second), state.ActivityExecuting, actSince)
	if got == nil || got.Kind != KindToolCall || !got.SuppressStall {
		t.Fatalf("live tool call + exec work = %+v, want KindToolCall with SuppressStall=true", got)
	}

	// Once exec work clears and no Tier 1/2 work is present -> nil.
	execWork = nil
	got = f.sample(tree...)
	if got != nil {
		t.Fatalf("idle after exec clears = %+v, want nil", got)
	}
}

func TestSampler_AttachedPrecedesExec(t *testing.T) {
	attached := true
	execWork := &LiveWork{Kind: KindExec, SuppressStall: false, Detail: "1 active exec run"}
	s := NewSampler(Config{
		AgentUID:      1 << 30,
		IsManagedPID:  func(int) bool { return false },
		CheckAttached: func() bool { return attached },
		CheckExec:     func() *LiveWork { return execWork },
	})
	s.procRoot = t.TempDir()

	got := s.sample(time.Now(), state.ActivityIdle, time.Now())
	if got == nil || got.Kind != KindAttached {
		t.Fatalf("attached+exec sample = %+v, want KindAttached", got)
	}

	attached = false
	got = s.sample(time.Now(), state.ActivityIdle, time.Now())
	if got == nil || got.Kind != KindExec {
		t.Fatalf("detached+exec sample = %+v, want KindExec", got)
	}
}
