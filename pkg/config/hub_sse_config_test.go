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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHubSSEConfig_Validate(t *testing.T) {
	tests := []struct {
		name      string
		cfg       HubSSEConfig
		wantAge   time.Duration
		wantBeat  time.Duration
		wantErrIn []string // substrings; nil = valid
	}{
		{name: "unset is the default", wantAge: 10 * time.Minute, wantBeat: 30 * time.Second},
		{name: "bounds", cfg: HubSSEConfig{MaxConnectionAge: "1m", HeartbeatInterval: "1m"}, wantAge: time.Minute, wantBeat: time.Minute},
		{name: "upper age", cfg: HubSSEConfig{MaxConnectionAge: "24h", HeartbeatInterval: "5s"}, wantAge: 24 * time.Hour, wantBeat: 5 * time.Second},
		{name: "age below min", cfg: HubSSEConfig{MaxConnectionAge: "59s"}, wantErrIn: []string{"server.hub.sse.max_connection_age", "between 1m0s and 24h0m0s"}},
		{name: "age above max", cfg: HubSSEConfig{MaxConnectionAge: "25h"}, wantErrIn: []string{"server.hub.sse.max_connection_age"}},
		{name: "age malformed", cfg: HubSSEConfig{MaxConnectionAge: "soon"}, wantErrIn: []string{"server.hub.sse.max_connection_age", "soon"}},
		{name: "heartbeat below min", cfg: HubSSEConfig{HeartbeatInterval: "4s"}, wantErrIn: []string{"server.hub.sse.heartbeat_interval", "between 5s and 1m0s"}},
		{name: "heartbeat at the client threshold", cfg: HubSSEConfig{HeartbeatInterval: "75s"}, wantErrIn: []string{"server.hub.sse.heartbeat_interval"}},
		{name: "heartbeat zero", cfg: HubSSEConfig{HeartbeatInterval: "0s"}, wantErrIn: []string{"server.hub.sse.heartbeat_interval"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErrIn != nil {
				require.Error(t, err)
				for _, sub := range tt.wantErrIn {
					assert.Contains(t, err.Error(), sub)
				}
				return
			}
			require.NoError(t, err)
			age, err := tt.cfg.MaxConnectionAgeDuration()
			require.NoError(t, err)
			assert.Equal(t, tt.wantAge, age)
			beat, err := tt.cfg.HeartbeatIntervalDuration()
			require.NoError(t, err)
			assert.Equal(t, tt.wantBeat, beat)
		})
	}
}

func TestPlatformRequestTimeoutDuration(t *testing.T) {
	d, err := PlatformRequestTimeoutDuration("")
	require.NoError(t, err)
	assert.Zero(t, d, "unset declares no timeout")

	d, err = PlatformRequestTimeoutDuration("900s")
	require.NoError(t, err)
	assert.Equal(t, 900*time.Second, d)

	for _, v := range []string{"149s", "25h", "0s", "-1m", "fifteen minutes"} {
		err := ValidatePlatformRequestTimeout(v)
		require.Error(t, err, v)
		assert.Contains(t, err.Error(), "server.hub.platform_request_timeout", v)
	}
	assert.NoError(t, ValidatePlatformRequestTimeout("150s"))
	assert.NoError(t, ValidatePlatformRequestTimeout("24h"))
}

// ptone/scion#5230: the effective maximum age is min(configured, 0.8 x
// platform timeout) when a timeout is declared.
func TestEffectiveSSEMaxAge(t *testing.T) {
	assert.Equal(t, 10*time.Minute, EffectiveSSEMaxAge(10*time.Minute, 0), "no timeout declared")
	assert.Equal(t, 10*time.Minute, EffectiveSSEMaxAge(10*time.Minute, 900*time.Second), "0.8 x 900s = 720s is above 10m")
	assert.Equal(t, 720*time.Second, EffectiveSSEMaxAge(time.Hour, 900*time.Second), "1h is clamped to 0.8 x 900s")
	assert.Equal(t, 240*time.Second, EffectiveSSEMaxAge(10*time.Minute, 300*time.Second), "10m is clamped to 0.8 x 300s")
}

// ptone/scion#5245: the conduit cap is clamped the same way, and never
// below ConduitMinLifetimeCap.
func TestEffectiveConduitLifetimeCap(t *testing.T) {
	assert.Equal(t, 3500*time.Second, EffectiveConduitLifetimeCap(ConduitDefaultLifetimeCap, 0))
	assert.Equal(t, 720*time.Second, EffectiveConduitLifetimeCap(ConduitDefaultLifetimeCap, 900*time.Second), "3500s is clamped to 0.8 x 900s")
	assert.Equal(t, 600*time.Second, EffectiveConduitLifetimeCap(600*time.Second, 900*time.Second), "a lower configured cap stays")
	assert.Equal(t, 120*time.Second, EffectiveConduitLifetimeCap(ConduitDefaultLifetimeCap, MinPlatformRequestTimeout))
	assert.Equal(t, ConduitMinLifetimeCap, EffectiveConduitLifetimeCap(ConduitMinLifetimeCap, MinPlatformRequestTimeout))
}

// ptone/scion#5230: the per-stream jitter spans +/-20% of the base.
func TestJitterLifetime_Range(t *testing.T) {
	base := 10 * time.Minute
	assert.Equal(t, 8*time.Minute, JitterLifetime(base, 1, 0), "u=0 is the low end, 0.8 x base")
	assert.Equal(t, base, JitterLifetime(base, 1, 0.5), "u=0.5 is the middle, base")
	assert.InDelta(t, float64(12*time.Minute), float64(JitterLifetime(base, 1, 0.999999)), float64(time.Millisecond))
	assert.Equal(t, 8*time.Minute, JitterLifetime(base, 0, 0))
	assert.InDelta(t, float64(base), float64(JitterLifetime(base, 0, 0.999999)), float64(time.Millisecond), "down-only never exceeds base")
	for i := 0; i < 1000; i++ {
		u := float64(i) / 1000
		d := JitterLifetime(base, 1, u)
		require.GreaterOrEqual(t, d, 8*time.Minute)
		require.Less(t, d, 12*time.Minute)
	}
}

// ptone/scion#5230: with a 900s platform timeout every jittered maximum age
// ends well before 900s.
func TestJitteredSSEMaxAge_StaysBelowPlatformTimeout(t *testing.T) {
	platform := 900 * time.Second
	for _, configured := range []time.Duration{time.Minute, 10 * time.Minute, time.Hour, 24 * time.Hour} {
		base := EffectiveSSEMaxAge(configured, platform)
		hi := JitterLifetime(base, 1, 0.999999)
		assert.Less(t, hi, platform, "configured %s", configured)
		assert.LessOrEqual(t, hi, time.Duration(float64(platform)*0.96)+time.Millisecond, "configured %s", configured)
	}
}

// ptone/scion#5245: the conduit cap is jittered symmetrically only when a
// platform timeout is declared (and then stays below it); otherwise it is
// only lowered, never past min(base, ConduitMinLifetimeCap).
func TestJitterConduitLifetimeCap(t *testing.T) {
	platform := 900 * time.Second
	base := EffectiveConduitLifetimeCap(ConduitDefaultLifetimeCap, platform)
	assert.Equal(t, 576*time.Second, JitterConduitLifetimeCap(base, true, 0), "u=0 with a declared timeout is 0.8 x 720s")
	assert.Less(t, JitterConduitLifetimeCap(base, true, 0.999999), platform)
	assert.Greater(t, JitterConduitLifetimeCap(base, true, 0.999999), base)

	assert.Equal(t, 2800*time.Second, JitterConduitLifetimeCap(ConduitDefaultLifetimeCap, false, 0))
	assert.LessOrEqual(t, JitterConduitLifetimeCap(ConduitDefaultLifetimeCap, false, 0.999999), ConduitDefaultLifetimeCap,
		"without a declared timeout the cap is a platform limit and is never raised")

	assert.Equal(t, ConduitMinLifetimeCap, JitterConduitLifetimeCap(ConduitMinLifetimeCap, false, 0), "floored at the minimum")
	assert.Equal(t, 96*time.Second, JitterConduitLifetimeCap(120*time.Second, true, 0))
}

// ptone/scion#5245: a fleet of tunnels opened together spreads its redials
// over the jitter window instead of all redialling at once.
func TestJitterConduitLifetimeCap_FleetSpread(t *testing.T) {
	platform := 900 * time.Second
	base := EffectiveConduitLifetimeCap(ConduitDefaultLifetimeCap, platform)
	const n = 200
	buckets := map[time.Duration]int{}
	lo, hi := time.Duration(1<<62), time.Duration(0)
	for i := 0; i < n; i++ {
		d := JitterConduitLifetimeCap(base, true, (float64(i)+0.5)/n)
		lo, hi = min(lo, d), max(hi, d)
		buckets[d.Truncate(10*time.Second)]++
	}
	assert.GreaterOrEqual(t, hi-lo, 280*time.Second, "redials spread over most of the +/-20% window (288s)")
	for b, c := range buckets {
		assert.LessOrEqual(t, c, n/10, "no 10s bucket holds more than a tenth of the fleet (bucket %s)", b)
	}
}

func sseSchemaEnvVar(t *testing.T, path ...string) string {
	t.Helper()
	data, err := schemasFS.ReadFile(settingsSchemaFiles["1"])
	require.NoError(t, err)
	var root map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &root))
	prop := findSchemaProperty(t, root, path...)
	envVar, _ := prop["x-env-var"].(string)
	require.NotEmpty(t, envVar, "schema: %v has no x-env-var", path)
	return envVar
}

// The three settings have schema env vars that map to their keys in the
// opsettings keyspace and the hub config, and override the file.
func TestSSESettings_EnvMapping(t *testing.T) {
	cases := []struct {
		path      []string
		env       string
		opsKey    string
		configKey string
	}{
		{[]string{"server", "hub", "sse", "max_connection_age"}, "SCION_SERVER_HUB_SSE_MAXCONNECTIONAGE",
			"server.hub.sse.max_connection_age", "hub.sse.maxConnectionAge"},
		{[]string{"server", "hub", "sse", "heartbeat_interval"}, "SCION_SERVER_HUB_SSE_HEARTBEATINTERVAL",
			"server.hub.sse.heartbeat_interval", "hub.sse.heartbeatInterval"},
		{[]string{"server", "hub", "platform_request_timeout"}, "SCION_SERVER_HUB_PLATFORMREQUESTTIMEOUT",
			"server.hub.platform_request_timeout", "hub.platformRequestTimeout"},
	}
	for _, tc := range cases {
		t.Run(tc.opsKey, func(t *testing.T) {
			assert.Equal(t, tc.env, sseSchemaEnvVar(t, tc.path...))
			suffix := tc.env[len("SCION_SERVER_"):]
			assert.Equal(t, tc.opsKey, serverEnvToOpsettingsKey(suffix))
			assert.Equal(t, tc.configKey, envKeyToConfigKey(suffix))
		})
	}

	t.Setenv("SCION_SERVER_HUB_SSE_MAXCONNECTIONAGE", "5m")
	t.Setenv("SCION_SERVER_HUB_SSE_HEARTBEATINTERVAL", "20s")
	t.Setenv("SCION_SERVER_HUB_PLATFORMREQUESTTIMEOUT", "900s")
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	configPath := filepath.Join(tmpDir, "settings.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("schema_version: \"1\"\nserver:\n  hub:\n    platform_request_timeout: 3600s\n    sse:\n      max_connection_age: 20m\n      heartbeat_interval: 40s\n"), 0644))
	cfg, err := LoadGlobalConfig(configPath)
	require.NoError(t, err)
	assert.Equal(t, "5m", cfg.Hub.SSE.MaxConnectionAge, "the env var overrides the file")
	assert.Equal(t, "20s", cfg.Hub.SSE.HeartbeatInterval)
	assert.Equal(t, "900s", cfg.Hub.PlatformRequestTimeout)
}

// The file values load as configured; an out-of-range value loads and
// fails Validate.
func TestLoadGlobalConfig_SSESettings(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	configPath := filepath.Join(tmpDir, "settings.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("schema_version: \"1\"\nserver:\n  hub:\n    platform_request_timeout: 60s\n    sse:\n      max_connection_age: 20m\n      heartbeat_interval: 2m\n"), 0644))
	cfg, err := LoadGlobalConfig(configPath)
	require.NoError(t, err)
	assert.Equal(t, "20m", cfg.Hub.SSE.MaxConnectionAge)
	assert.Equal(t, "2m", cfg.Hub.SSE.HeartbeatInterval)
	assert.Equal(t, "60s", cfg.Hub.PlatformRequestTimeout)
	assert.ErrorContains(t, cfg.Hub.SSE.Validate(), "server.hub.sse.heartbeat_interval")
	assert.ErrorContains(t, ValidatePlatformRequestTimeout(cfg.Hub.PlatformRequestTimeout), "server.hub.platform_request_timeout")
}

// The settings survive the V1 <-> global conversions, and unset ones are
// not written.
func TestSSESettings_V1RoundTrip(t *testing.T) {
	gc := &GlobalConfig{}
	gc.Hub.SSE = HubSSEConfig{MaxConnectionAge: "15m", HeartbeatInterval: "25s"}
	gc.Hub.PlatformRequestTimeout = "900s"
	v1 := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1.Hub)
	require.NotNil(t, v1.Hub.SSE)
	assert.Equal(t, "15m", v1.Hub.SSE.MaxConnectionAge)
	assert.Equal(t, "25s", v1.Hub.SSE.HeartbeatInterval)
	assert.Equal(t, "900s", v1.Hub.PlatformRequestTimeout)
	back := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, gc.Hub.SSE, back.Hub.SSE)
	assert.Equal(t, "900s", back.Hub.PlatformRequestTimeout)

	empty := ConvertGlobalToV1ServerConfig(&GlobalConfig{})
	assert.Nil(t, empty.Hub.SSE, "an unset block is not written")
	assert.Empty(t, empty.Hub.PlatformRequestTimeout)
}

// The ingress cap ceiling is the top of the jitter range: every jittered
// cap fits under it, and with a declared timeout it stays below the
// timeout.
func TestConduitLifetimeCapCeiling(t *testing.T) {
	platform := 900 * time.Second
	base := EffectiveConduitLifetimeCap(ConduitDefaultLifetimeCap, platform)
	ceiling := ConduitLifetimeCapCeiling(base, true)
	assert.Equal(t, 864*time.Second, ceiling, "1.2 x 720s")
	assert.Less(t, ceiling, platform)
	assert.Equal(t, ConduitDefaultLifetimeCap, ConduitLifetimeCapCeiling(ConduitDefaultLifetimeCap, false), "no declared timeout: the cap itself")
	for i := 0; i < 1000; i++ {
		u := float64(i) / 1000
		require.LessOrEqual(t, JitterConduitLifetimeCap(base, true, u), ceiling)
		require.LessOrEqual(t, JitterConduitLifetimeCap(ConduitDefaultLifetimeCap, false, u), ConduitDefaultLifetimeCap)
	}
}
