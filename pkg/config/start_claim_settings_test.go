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

package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The four start-claim settings round-trip through V1<->Global config and
// their schema env vars reach GlobalConfig.
func TestStartClaimSettings_RoundTripAndEnv(t *testing.T) {
	v1 := &V1ServerConfig{Hub: &V1ServerHubConfig{
		StartClaimLeaseTTL:         "60s",
		StartMaxDuration:           "15m",
		StartUnconfirmedHold:       "14m",
		StartCreateUnconfirmedHold: "6m",
	}}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, 60*time.Second, gc.Hub.StartClaimLeaseTTL)
	assert.Equal(t, 15*time.Minute, gc.Hub.StartMaxDuration)
	assert.Equal(t, 14*time.Minute, gc.Hub.StartUnconfirmedHold)
	assert.Equal(t, 6*time.Minute, gc.Hub.StartCreateUnconfirmedHold)

	back := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, back.Hub)
	assert.Equal(t, "1m0s", back.Hub.StartClaimLeaseTTL)
	assert.Equal(t, "15m0s", back.Hub.StartMaxDuration)
	assert.Equal(t, "14m0s", back.Hub.StartUnconfirmedHold)
	assert.Equal(t, "6m0s", back.Hub.StartCreateUnconfirmedHold)

	empty := ConvertGlobalToV1ServerConfig(&GlobalConfig{})
	assert.Empty(t, empty.Hub.StartClaimLeaseTTL, "zero is omitted")

	bad := ConvertV1ServerToGlobalConfig(&V1ServerConfig{Hub: &V1ServerHubConfig{StartMaxDuration: "soon"}})
	assert.Zero(t, bad.Hub.StartMaxDuration, "an invalid duration is ignored")

	t.Setenv("HOME", t.TempDir())
	t.Setenv(schemaEnvVar(t, "start_claim_lease_ttl"), "45s")
	t.Setenv(schemaEnvVar(t, "start_max_duration"), "20m")
	t.Setenv(schemaEnvVar(t, "start_unconfirmed_hold"), "15m")
	t.Setenv(schemaEnvVar(t, "start_create_unconfirmed_hold"), "4m")
	cfg, err := LoadGlobalConfig("")
	require.NoError(t, err)
	assert.Equal(t, 45*time.Second, cfg.Hub.StartClaimLeaseTTL)
	assert.Equal(t, 20*time.Minute, cfg.Hub.StartMaxDuration)
	assert.Equal(t, 15*time.Minute, cfg.Hub.StartUnconfirmedHold)
	assert.Equal(t, 4*time.Minute, cfg.Hub.StartCreateUnconfirmedHold)
}
