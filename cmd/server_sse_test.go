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
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestValidateServerPreflight_LongLivedRequestSettings: an out-of-range or
// malformed server.hub.sse.* or server.hub.platform_request_timeout fails
// hub startup with a message naming the setting (ptone/scion#5230).
func TestValidateServerPreflight_LongLivedRequestSettings(t *testing.T) {
	t.Cleanup(resetServerFlags)
	for _, tc := range []struct {
		name    string
		set     func(*config.GlobalConfig)
		wantErr string
	}{
		{name: "unset", set: func(*config.GlobalConfig) {}},
		{name: "valid", set: func(c *config.GlobalConfig) {
			c.Hub.SSE = config.HubSSEConfig{MaxConnectionAge: "5m", HeartbeatInterval: "20s"}
			c.Hub.PlatformRequestTimeout = "900s"
		}},
		{name: "max age", set: func(c *config.GlobalConfig) { c.Hub.SSE.MaxConnectionAge = "10s" },
			wantErr: "server.hub.sse.max_connection_age"},
		{name: "heartbeat", set: func(c *config.GlobalConfig) { c.Hub.SSE.HeartbeatInterval = "90s" },
			wantErr: "server.hub.sse.heartbeat_interval"},
		{name: "platform timeout", set: func(c *config.GlobalConfig) { c.Hub.PlatformRequestTimeout = "30s" },
			wantErr: "server.hub.platform_request_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetServerFlags()
			enableHub = true
			t.Setenv("K_SERVICE", "") // no Cloud Run warning in these runs
			cfg := &config.GlobalConfig{}
			tc.set(cfg)
			err := validateServerPreflight(cfg)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// ptone/scion#5230, ptone/scion#5245: the web server and the conduit relay
// get the configured bounds, clamped to 0.8 of the declared platform
// request timeout.
func TestLongLivedRequestSettings_Clamp(t *testing.T) {
	cfg := &config.GlobalConfig{}
	assert.Equal(t, 10*time.Minute, sseMaxConnectionAgeSetting(cfg), "default")
	assert.Equal(t, 30*time.Second, sseHeartbeatIntervalSetting(cfg), "default")
	assert.Equal(t, config.ConduitDefaultLifetimeCap, conduitLifetimeCapSetting(cfg), "no timeout declared")
	assert.Zero(t, platformRequestTimeoutSetting(cfg))

	cfg.Hub.PlatformRequestTimeout = "900s"
	assert.Equal(t, 10*time.Minute, sseMaxConnectionAgeSetting(cfg), "10m is below 0.8 x 900s")
	assert.Equal(t, 720*time.Second, conduitLifetimeCapSetting(cfg))

	cfg.Hub.SSE.MaxConnectionAge = "1h"
	cfg.Hub.SSE.HeartbeatInterval = "15s"
	assert.Equal(t, 720*time.Second, sseMaxConnectionAgeSetting(cfg))
	assert.Equal(t, 15*time.Second, sseHeartbeatIntervalSetting(cfg))

	cfg.Hub.Conduit.LifetimeCap = "10m"
	assert.Equal(t, 10*time.Minute, conduitLifetimeCapSetting(cfg), "a lower configured cap stays")
}

// ptone/scion#5245: the per-session conduit jitter is symmetric with a
// declared platform timeout and only lowers the cap without one.
func TestConduitLifetimeJitter(t *testing.T) {
	cfg := &config.GlobalConfig{}
	jitter := conduitLifetimeJitter(cfg)
	base := conduitLifetimeCapSetting(cfg)
	for i := 0; i < 500; i++ {
		d := jitter(base)
		require.GreaterOrEqual(t, d, base*8/10)
		require.LessOrEqual(t, d, base, "without a declared timeout the cap is never raised")
	}

	cfg.Hub.PlatformRequestTimeout = "900s"
	jitter = conduitLifetimeJitter(cfg)
	base = conduitLifetimeCapSetting(cfg)
	above := false
	for i := 0; i < 500; i++ {
		d := jitter(base)
		require.GreaterOrEqual(t, d, base*8/10)
		require.Less(t, d, 900*time.Second, "stays below the platform timeout")
		above = above || d > base
	}
	assert.True(t, above, "with a declared timeout the jitter also raises the cap")
}

// ptone/scion#5230: on Cloud Run (K_SERVICE) without a declared platform
// timeout, startup logs one warning naming the setting; with the setting,
// or off Cloud Run, nothing. validateServerPreflight logs it exactly once.
func TestWarnUndeclaredPlatformTimeout(t *testing.T) {
	getenv := func(service string) func(string) string {
		return func(k string) string {
			if k == "K_SERVICE" {
				return service
			}
			return ""
		}
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := &config.GlobalConfig{}
	assert.True(t, warnUndeclaredPlatformTimeout(cfg, getenv("scion-hub"), logger))
	out := buf.String()
	assert.Equal(t, 1, strings.Count(out, "level=WARN"))
	assert.Contains(t, out, "server.hub.platform_request_timeout")
	assert.Contains(t, out, "request slot")

	buf.Reset()
	assert.False(t, warnUndeclaredPlatformTimeout(cfg, getenv(""), logger), "not on Cloud Run")
	cfg.Hub.PlatformRequestTimeout = "900s"
	assert.False(t, warnUndeclaredPlatformTimeout(cfg, getenv("scion-hub"), logger), "timeout declared")
	assert.Empty(t, buf.String())
}

func TestValidateServerPreflight_CloudRunWarningOnce(t *testing.T) {
	t.Cleanup(resetServerFlags)
	resetServerFlags()
	enableHub = true
	t.Setenv("K_SERVICE", "scion-hub")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	require.NoError(t, validateServerPreflight(&config.GlobalConfig{}))
	assert.Equal(t, 1, strings.Count(buf.String(), "Running on Cloud Run without server.hub.platform_request_timeout"))
}

// ptone/scion#5245 review: in external relay mode the hub's ingress cap is
// the top of the relays' jitter range, so it never cuts the relay's
// per-session jitter back to one value; it stays below the platform
// timeout, and without a declared timeout it is the cap itself.
func TestConduitIngressLifetimeCapSetting(t *testing.T) {
	cfg := &config.GlobalConfig{}
	assert.Equal(t, config.ConduitDefaultLifetimeCap, conduitIngressLifetimeCapSetting(cfg))
	cfg.Hub.PlatformRequestTimeout = "900s"
	ingress := conduitIngressLifetimeCapSetting(cfg)
	assert.Equal(t, 864*time.Second, ingress, "1.2 x 720s")
	jitter := conduitLifetimeJitter(cfg)
	base := conduitLifetimeCapSetting(cfg)
	for i := 0; i < 500; i++ {
		require.LessOrEqual(t, jitter(base), ingress, "every jittered relay cap fits under the ingress cap")
	}
}

// ptone/scion#5245 review: external relay mode hands the hub the ingress
// cap (the top of the relays' jitter range), not the unjittered cap.
func TestConduitExternalOptions_LifetimeCapIsIngressCap(t *testing.T) {
	cfg := &config.GlobalConfig{}
	cfg.Hub.PlatformRequestTimeout = "900s"
	opts := conduitExternalOptions(cfg, "hub-1", nil, nil, "")
	assert.Equal(t, conduitIngressLifetimeCapSetting(cfg), opts.LifetimeCap)
	assert.Equal(t, 864*time.Second, opts.LifetimeCap, "the ingress cap, 1.2 x 720s, not the unjittered 720s")
	assert.Equal(t, "hub-1", opts.InstanceID)

	// Without a declared timeout the ingress cap equals the unjittered cap
	// (both 3500s), so this only pins the no-timeout default; the 900s case
	// above is what tells the two wirings apart.
	cfg.Hub.PlatformRequestTimeout = ""
	assert.Equal(t, config.ConduitDefaultLifetimeCap, conduitExternalOptions(cfg, "hub-1", nil, nil, "").LifetimeCap)
}
