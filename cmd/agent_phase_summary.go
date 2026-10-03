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

package cmd

import (
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

// phaseCounts is a complete bucketing of agents by lifecycle phase. Every
// agent lands in exactly one bucket: a known state.Phase, or Other for phase
// strings this client does not recognise (e.g. a newer Hub). The buckets
// therefore always sum to Total.
//
// It is shared by 'scion hub health' (fleet-wide, from the Hub's GROUP BY
// phase aggregate) and 'scion project status' (per-project, from the agent
// list) so both commands render phases identically.
type phaseCounts struct {
	Total   int
	ByPhase map[state.Phase]int
	Other   int
}

// alwaysShownPhases are rendered even when zero, in this order, so the
// headline numbers are always present. All other phases are rendered only
// when non-zero, in state.Phases() order.
var alwaysShownPhases = []state.Phase{state.PhaseRunning, state.PhaseError, state.PhaseStopped}

// bucketPhases converts raw phase -> count tallies into phaseCounts.
// Unknown phase keys (including the empty string) are counted as Other.
func bucketPhases(raw map[string]int) phaseCounts {
	pc := phaseCounts{ByPhase: make(map[state.Phase]int, len(raw))}
	for k, n := range raw {
		pc.Total += n
		p := state.Phase(k)
		if p.IsValid() {
			pc.ByPhase[p] += n
		} else {
			pc.Other += n
		}
	}
	return pc
}

// String renders e.g. "Total=40 | Running=6 | Error=0 | Stopped=30 | Suspended=4".
func (pc phaseCounts) String() string {
	parts := []string{fmt.Sprintf("Total=%d", pc.Total)}
	shown := make(map[state.Phase]bool, len(alwaysShownPhases))
	for _, p := range alwaysShownPhases {
		parts = append(parts, fmt.Sprintf("%s=%d", phaseLabel(p), pc.ByPhase[p]))
		shown[p] = true
	}
	for _, p := range state.Phases() {
		if shown[p] {
			continue
		}
		if n := pc.ByPhase[p]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", phaseLabel(p), n))
		}
	}
	if pc.Other > 0 {
		parts = append(parts, fmt.Sprintf("Other=%d", pc.Other))
	}
	return strings.Join(parts, " | ")
}

// phaseLabel title-cases a phase for display ("running" -> "Running").
func phaseLabel(p state.Phase) string {
	s := string(p)
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
