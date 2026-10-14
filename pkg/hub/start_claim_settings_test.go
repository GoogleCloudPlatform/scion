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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

func TestStartClaimSettings_Normalize(t *testing.T) {
	d := DefaultStartClaimSettings()
	got, warns := StartClaimSettings{}.normalized()
	assert.Equal(t, d, got, "zero values take the defaults")
	assert.Empty(t, warns)

	cases := []struct {
		name     string
		in       StartClaimSettings
		want     StartClaimSettings
		wantWarn bool
	}{
		{"lease too short", StartClaimSettings{LeaseTTL: 10 * time.Second}, d, true},
		{"lease too long", StartClaimSettings{LeaseTTL: 10 * time.Minute}, d, true},
		{"lease ok", StartClaimSettings{LeaseTTL: 30 * time.Second}, StartClaimSettings{LeaseTTL: 30 * time.Second, MaxDuration: d.MaxDuration, UnconfirmedHold: d.UnconfirmedHold, CreateUnconfirmedHold: d.CreateUnconfirmedHold}, false},
		{"max below pod-ready bound", StartClaimSettings{MaxDuration: 10 * time.Minute}, d, true},
		// An earlier release's hold below today's minimum is raised to the
		// minimum, with a warning (ptone/scion#4817).
		{"hold below start budget is raised", StartClaimSettings{UnconfirmedHold: 13 * time.Minute}, StartClaimSettings{LeaseTTL: d.LeaseTTL, MaxDuration: d.MaxDuration, UnconfirmedHold: minStartUnconfirmedHold, CreateUnconfirmedHold: d.CreateUnconfirmedHold}, true},
		{"hold at start budget", StartClaimSettings{UnconfirmedHold: minStartUnconfirmedHold}, StartClaimSettings{LeaseTTL: d.LeaseTTL, MaxDuration: d.MaxDuration, UnconfirmedHold: minStartUnconfirmedHold, CreateUnconfirmedHold: d.CreateUnconfirmedHold}, false},
		{"create hold below minimum is raised", StartClaimSettings{CreateUnconfirmedHold: 5 * time.Minute}, StartClaimSettings{LeaseTTL: d.LeaseTTL, MaxDuration: d.MaxDuration, UnconfirmedHold: d.UnconfirmedHold, CreateUnconfirmedHold: minStartCreateUnconfirmedHold}, true},
		{"create hold above hold", StartClaimSettings{CreateUnconfirmedHold: d.UnconfirmedHold + time.Minute}, d, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warns := tc.in.normalized()
			assert.Equal(t, tc.want, got)
			if tc.wantWarn {
				assert.Len(t, warns, 1)
			} else {
				assert.Empty(t, warns)
			}
		})
	}

	assert.Equal(t, store.StartClaimHolds{Default: 17 * time.Minute, Create: 16 * time.Minute, Stop: 5 * time.Minute}, d.Holds())
	assert.Equal(t, 16*time.Minute, d.Holds().For(store.StartClaimCreate))
	assert.Equal(t, 17*time.Minute, d.Holds().For(store.StartClaimRecovery))
}

// The unconfirmed-start bounds cover the broker's bound on a synchronous
// start that runs on after the hub gave up on it (api.SyncStartCeiling,
// ptone/scion#4817): below it, the reaper would stop a start that is still
// legitimately coming up, and run adoption could overtake it.
func TestStartClaimSettings_HoldsCoverSyncStartCeiling(t *testing.T) {
	d := DefaultStartClaimSettings()
	assert.GreaterOrEqual(t, minStartCreateUnconfirmedHold, api.SyncStartCeiling+time.Minute, "create hold minimum")
	assert.GreaterOrEqual(t, minStartUnconfirmedHold, minStartCreateUnconfirmedHold, "the create hold minimum fits under the hold minimum")
	assert.GreaterOrEqual(t, d.CreateUnconfirmedHold, minStartCreateUnconfirmedHold)
	assert.GreaterOrEqual(t, d.UnconfirmedHold, d.CreateUnconfirmedHold)
	assert.Equal(t, api.SyncStartCeiling+2*syncDispatchTimeout, runAdoptionQuietPeriod(), "run adoption quiet period")

	// The old 5m create hold and 13m hold are raised to the minimums, with
	// a warning each, and do not fail validation (a soft warning).
	got, hard, soft := StartClaimSettings{UnconfirmedHold: 13 * time.Minute, CreateUnconfirmedHold: 5 * time.Minute}.normalize()
	assert.Equal(t, minStartUnconfirmedHold, got.UnconfirmedHold)
	assert.Equal(t, minStartCreateUnconfirmedHold, got.CreateUnconfirmedHold)
	assert.Empty(t, hard)
	assert.Len(t, soft, 2)
}

func TestStartClaimSettings_ApplySnapshot(t *testing.T) {
	s := &Server{config: DefaultServerConfig()}
	s.config.StartClaim = StartClaimSettings{LeaseTTL: time.Minute}
	s.setStartClaimSettings(s.config.StartClaim)
	assert.Equal(t, time.Minute, s.startClaimSettings().LeaseTTL)

	res := ApplySnapshot(s, Layer1Snapshot{StartMaxDuration: "20m", StartCreateUnconfirmedHold: "bogus"})
	got := s.startClaimSettings()
	assert.Equal(t, 20*time.Minute, got.MaxDuration)
	assert.Equal(t, time.Minute, got.LeaseTTL, "unset keys keep the startup value")
	assert.Equal(t, DefaultStartClaimSettings().CreateUnconfirmedHold, got.CreateUnconfirmedHold, "an invalid value keeps the current one")
	assert.Contains(t, res["applied"], "start_claim")

	// Removing the override reverts to the startup value.
	ApplySnapshot(s, Layer1Snapshot{})
	assert.Equal(t, DefaultStartClaimSettings().MaxDuration, s.startClaimSettings().MaxDuration)

	// Out of range is replaced by the default.
	ApplySnapshot(s, Layer1Snapshot{StartClaimLeaseTTL: "1s"})
	assert.Equal(t, DefaultStartClaimSettings().LeaseTTL, s.startClaimSettings().LeaseTTL)

	var nilCfg Server
	assert.Equal(t, DefaultStartClaimSettings(), nilCfg.startClaimSettings())
}

// The stop claim hold (a queued stop being applied) keeps its old 5m when
// the create hold grows to cover the broker's start ceiling, whatever the
// configured create hold: a stop has no start to wait for.
func TestStartClaimSettings_StopHoldUnchanged(t *testing.T) {
	assert.Equal(t, 5*time.Minute, DefaultStartClaimSettings().Holds().For(store.StartClaimStop))
	c, _ := StartClaimSettings{UnconfirmedHold: 30 * time.Minute, CreateUnconfirmedHold: 25 * time.Minute}.normalized()
	assert.Equal(t, 25*time.Minute, c.Holds().For(store.StartClaimCreate))
	assert.Equal(t, 5*time.Minute, c.Holds().For(store.StartClaimStop), "a longer create hold does not lengthen the stop hold")
}
