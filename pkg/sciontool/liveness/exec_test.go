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

func TestSampler_ExecLiveWorkBeforeTiers(t *testing.T) {
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

	// Active exec work takes precedence over Tier 2 busy CPU process, with
	// SuppressStall=false so stall marking is unaffected.
	execWork = &LiveWork{Kind: KindExec, SuppressStall: false, Detail: "1 active exec run"}
	tree[4].ticks += 500
	got := f.sample(tree...)
	if got == nil || got.Kind != KindExec {
		t.Fatalf("expected KindExec, got %+v", got)
	}
	if got.SuppressStall {
		t.Error("SuppressStall = true, want false")
	}

	// Once exec work clears, the CPU baseline recorded during the exec sample
	// allows Tier 2 to detect the still-busy process on the next sample.
	execWork = nil
	tree[4].ticks += 500
	got = f.sample(tree...)
	if got == nil || got.Kind != KindProcess {
		t.Fatalf("expected KindProcess after exec clears, got %+v", got)
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
