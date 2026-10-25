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

package telemetrycontract

import (
	"bytes"
	"strings"
	"testing"
)

// TestLifecycleAbsentLaunchIDPresenceFixtures pins exactly the presence
// combinations the run attribution emits while the row has a run (WP1
// attribution design v2.8, section 10.3; schema_version 1 unchanged): each
// event without launch_id passes Validate and round-trips byte-identically.
// An absent launch_id means unknown or unverified run attribution (see
// the LifecycleEvent doc).
func TestLifecycleAbsentLaunchIDPresenceFixtures(t *testing.T) {
	seg := func(v int64) *int64 { return &v }
	cases := map[string]func() *LifecycleEvent{
		"started": func() *LifecycleEvent { return minimalLifecycleEvent(TransitionStarted) },
		"stopped(stopped)": func() *LifecycleEvent {
			return minimalLifecycleEvent(TransitionStopped)
		},
		"stopped(error)": func() *LifecycleEvent {
			e := minimalLifecycleEvent(TransitionStopped)
			e.StopKind = StopKindError
			return e
		},
		"stopped(start_unobserved)": func() *LifecycleEvent {
			e := minimalLifecycleEvent(TransitionStopped)
			e.Cause = CauseStartUnobserved
			return e
		},
		"stopped(superseded) with segment_metadata_revision and run_changed_at": func() *LifecycleEvent {
			e := minimalLifecycleEvent(TransitionStopped)
			e.StopKind = StopKindSuperseded
			e.SegmentMetadataRevision = seg(1)
			e.RunChangedAt = FormatTS(lcEventTS)
			return e
		},
		"stopped(restart_stop_leg) with segment_metadata_revision": func() *LifecycleEvent {
			e := minimalLifecycleEvent(TransitionStopped)
			e.Cause = CauseRestartStopLeg
			e.SegmentMetadataRevision = seg(1)
			return e
		},
		"halted": func() *LifecycleEvent { return minimalLifecycleEvent(TransitionHalted) },
		"deleted closure with segment_metadata_revision": func() *LifecycleEvent {
			e := minimalLifecycleEvent(TransitionDeleted)
			e.DeleteMode = DeleteModeSoft
			e.SegmentMetadataRevision = seg(3)
			return e
		},
		"deleted without segment": func() *LifecycleEvent {
			e := minimalLifecycleEvent(TransitionDeleted)
			e.DeleteMode = DeleteModeSoft
			return e
		},
		"restored":         func() *LifecycleEvent { return minimalLifecycleEvent(TransitionRestored) },
		"metadata_changed": func() *LifecycleEvent { return minimalLifecycleEvent(TransitionMetadataChanged) },
		"reincarnated":     func() *LifecycleEvent { return minimalLifecycleEvent(TransitionReincarnated) },
		"configured":       func() *LifecycleEvent { return minimalLifecycleEvent(TransitionConfigured) },
		"launch_accepted":  func() *LifecycleEvent { return minimalLifecycleEvent(TransitionLaunchAccepted) },
		"launch_failed(failed) closing a segment": func() *LifecycleEvent {
			return minimalLifecycleEvent(TransitionLaunchFailed)
		},
		"resynced(unobserved_change, gap): the malformed-segment discard gap": func() *LifecycleEvent {
			e := minimalLifecycleEvent(TransitionResynced)
			e.Cause = CauseUnobservedChange
			e.Gap = lifecycleBoolPtr(true)
			return e
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			e := mk()
			if e.LaunchID != "" {
				t.Fatal("fixture must omit launch_id")
			}
			if err := e.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			b1, err := Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b1), `"launch_id"`) {
				t.Fatalf("launch_id present in %s", b1)
			}
			var back LifecycleEvent
			if err := Unmarshal(b1, &back); err != nil {
				t.Fatal(err)
			}
			b2, err := Marshal(&back)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(b1, b2) {
				t.Fatalf("round trip differs:\n%s\n%s", b1, b2)
			}
		})
	}
}
