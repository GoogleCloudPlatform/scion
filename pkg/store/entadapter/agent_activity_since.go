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

package entadapter

import (
	"time"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// stampActivitySince maintains activity_since on an agent write m that
// moves the row from prevPhase/prevActivity (the stored values before the
// write) to whatever phase and activity m sets, the stored ones where m
// sets none:
//
//   - phase not running, or no activity: activity_since is cleared;
//   - otherwise, when the activity value changes, or the row enters
//     running: it is set to now;
//   - otherwise (the same activity written again, or no activity in m):
//     it is left alone, so a re-report or hook traffic on a sticky
//     activity never moves it.
//
// Every store writer that knows the stored values calls it. Writers that
// only move phase rely on entc.ClearLaunchResumePhaseHook, which clears
// activity_since on any write of a phase other than running.
func stampActivitySince(m *ent.AgentMutation, prevPhase, prevActivity string, now time.Time) {
	phase, ok := m.Phase()
	if !ok {
		phase = prevPhase
	}
	activity, ok := m.Activity()
	if !ok {
		activity = prevActivity
	}
	running := string(state.PhaseRunning)
	switch {
	case phase != running || activity == "":
		m.ClearActivitySince()
	case activity != prevActivity || prevPhase != running:
		m.SetActivitySince(now)
	}
}
