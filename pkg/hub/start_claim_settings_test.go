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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

func TestStartClaimSettings_Normalize(t *testing.T) {
	d := DefaultStartClaimSettings()
	got, warns := StartClaimSettings{}.normalized()
	assert.Equal(t, d, got, "zero values take the defaults")
	assert.Empty(t, warns)

	cases := []struct {
		name string
		in   StartClaimSettings
		want StartClaimSettings
	}{
		{"lease too short", StartClaimSettings{LeaseTTL: 10 * time.Second}, d},
		{"lease too long", StartClaimSettings{LeaseTTL: 10 * time.Minute}, d},
		{"lease ok", StartClaimSettings{LeaseTTL: 30 * time.Second}, StartClaimSettings{LeaseTTL: 30 * time.Second, MaxDuration: d.MaxDuration, UnconfirmedHold: d.UnconfirmedHold, CreateUnconfirmedHold: d.CreateUnconfirmedHold}},
		{"max below pod-ready bound", StartClaimSettings{MaxDuration: 10 * time.Minute}, d},
		{"hold below start budget", StartClaimSettings{UnconfirmedHold: 12*time.Minute + 39*time.Second}, d},
		{"hold at start budget", StartClaimSettings{UnconfirmedHold: 12*time.Minute + 40*time.Second}, StartClaimSettings{LeaseTTL: d.LeaseTTL, MaxDuration: d.MaxDuration, UnconfirmedHold: 12*time.Minute + 40*time.Second, CreateUnconfirmedHold: d.CreateUnconfirmedHold}},
		{"create hold too short", StartClaimSettings{CreateUnconfirmedHold: 2 * time.Minute}, d},
		{"create hold above hold", StartClaimSettings{CreateUnconfirmedHold: 14 * time.Minute}, d},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warns := tc.in.normalized()
			assert.Equal(t, tc.want, got)
			if tc.want == d {
				assert.Len(t, warns, 1)
			} else {
				assert.Empty(t, warns)
			}
		})
	}

	assert.Equal(t, store.StartClaimHolds{Default: 13 * time.Minute, Create: 5 * time.Minute}, d.Holds())
	assert.Equal(t, 5*time.Minute, d.Holds().For(store.StartClaimStop))
	assert.Equal(t, 13*time.Minute, d.Holds().For(store.StartClaimRecovery))
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
