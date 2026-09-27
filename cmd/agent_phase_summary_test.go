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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/stretchr/testify/assert"
)

func sumBuckets(pc phaseCounts) int {
	n := pc.Other
	for _, v := range pc.ByPhase {
		n += v
	}
	return n
}

func TestBucketPhases_SumsToTotalAndUnknownGoesToOther(t *testing.T) {
	raw := map[string]int{
		"running":      6,
		"stopped":      25,
		"suspended":    4,
		"provisioning": 2,
		"error":        1,
		"hibernating":  1, // unknown to this client (e.g. newer Hub)
		"":             1, // missing phase
	}
	pc := bucketPhases(raw)

	assert.Equal(t, 40, pc.Total)
	assert.Equal(t, pc.Total, sumBuckets(pc), "buckets must sum to Total")
	assert.Equal(t, 2, pc.Other, "unknown and empty phases land in Other")
	assert.Equal(t, 6, pc.ByPhase[state.PhaseRunning])
}

func TestBucketPhases_CoversEveryKnownPhase(t *testing.T) {
	raw := map[string]int{}
	for _, p := range state.Phases() {
		raw[string(p)] = 1
	}
	pc := bucketPhases(raw)
	assert.Equal(t, len(state.Phases()), pc.Total)
	assert.Equal(t, 0, pc.Other)
	assert.Equal(t, pc.Total, sumBuckets(pc))
}

func TestPhaseCountsString(t *testing.T) {
	t.Run("headline phases always shown, others only when non-zero", func(t *testing.T) {
		pc := bucketPhases(map[string]int{"running": 2, "starting": 1})
		assert.Equal(t, "Total=3 | Running=2 | Error=0 | Stopped=0 | Starting=1", pc.String())
	})

	t.Run("extra phases follow lifecycle order, Other last", func(t *testing.T) {
		pc := bucketPhases(map[string]int{
			"suspended":    4,
			"provisioning": 2,
			"stopped":      25,
			"running":      6,
			"weird":        3,
		})
		assert.Equal(t,
			"Total=40 | Running=6 | Error=0 | Stopped=25 | Provisioning=2 | Suspended=4 | Other=3",
			pc.String())
	})

	t.Run("empty", func(t *testing.T) {
		assert.Equal(t, "Total=0 | Running=0 | Error=0 | Stopped=0", bucketPhases(nil).String())
	})
}

func TestProjectHealthSummaryPhaseRoundTrip(t *testing.T) {
	pc := bucketPhases(map[string]int{"running": 3, "cloning": 1, "mystery": 2})
	var s ProjectHealthSummary
	s.setPhaseCounts(pc)

	assert.Equal(t, 6, s.Total)
	assert.Equal(t, 3, s.Running)
	assert.Equal(t, 1, s.Cloning)
	assert.Equal(t, 2, s.OtherPhase)
	assert.Equal(t, pc.String(), formatPhaseSummary(s),
		"project status and hub health must render phases identically")
}
