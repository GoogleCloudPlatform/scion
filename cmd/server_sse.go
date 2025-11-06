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
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/util/cloudrunenv"
)

// Long-lived request bounds (ptone/scion#5230, ptone/scion#5245): the
// browser events stream settings (server.hub.sse) and the declared platform
// request timeout (server.hub.platform_request_timeout), which clamps both
// the events stream maximum age and the conduit session lifetime cap.

// validateLongLivedRequestSettings checks server.hub.sse and
// server.hub.platform_request_timeout. Part of validateServerPreflight.
func validateLongLivedRequestSettings(cfg *config.GlobalConfig) error {
	if err := cfg.Hub.SSE.Validate(); err != nil {
		return err
	}
	return config.ValidatePlatformRequestTimeout(cfg.Hub.PlatformRequestTimeout)
}

// platformRequestTimeoutSetting returns the declared platform request
// timeout, or 0 when none is declared. validateServerPreflight has already
// rejected a malformed or out-of-range value.
func platformRequestTimeoutSetting(cfg *config.GlobalConfig) time.Duration {
	d, err := config.PlatformRequestTimeoutDuration(cfg.Hub.PlatformRequestTimeout)
	if err != nil {
		return 0
	}
	return d
}

// sseMaxConnectionAgeSetting returns the events stream maximum age before
// the per-stream jitter: server.hub.sse.max_connection_age (default 10m),
// clamped to 0.8 of the declared platform request timeout.
func sseMaxConnectionAgeSetting(cfg *config.GlobalConfig) time.Duration {
	d, err := cfg.Hub.SSE.MaxConnectionAgeDuration()
	if err != nil {
		d = config.SSEDefaultMaxConnectionAge
	}
	return config.EffectiveSSEMaxAge(d, platformRequestTimeoutSetting(cfg))
}

// sseHeartbeatIntervalSetting returns server.hub.sse.heartbeat_interval
// (default 30s).
func sseHeartbeatIntervalSetting(cfg *config.GlobalConfig) time.Duration {
	d, err := cfg.Hub.SSE.HeartbeatIntervalDuration()
	if err != nil {
		return config.SSEDefaultHeartbeatInterval
	}
	return d
}

// conduitLifetimeJitter returns the per-session jitter of the conduit
// lifetime cap (relay.Config.JitterLifetimeCap): symmetric +/-20% when a
// platform request timeout is declared, otherwise only downwards, because
// the cap is then the platform limit itself (config.JitterConduitLifetimeCap).
func conduitLifetimeJitter(cfg *config.GlobalConfig) func(time.Duration) time.Duration {
	declared := platformRequestTimeoutSetting(cfg) > 0
	return func(d time.Duration) time.Duration {
		return config.JitterConduitLifetimeCap(d, declared, rand.Float64())
	}
}

// platformTimeoutWarning is the startup warning logged on Cloud Run when no
// platform request timeout is declared.
const platformTimeoutWarning = "Running on Cloud Run without server.hub.platform_request_timeout: set it to the service's request timeout " +
	"so the hub ends events streams and conduit sessions before Cloud Run cuts them. Each open events stream, terminal and " +
	"tunnel holds one request slot (containerConcurrency) until it ends, so streams that run into the platform timeout " +
	"hold slots and reconnect in bursts"

// warnUndeclaredPlatformTimeout logs platformTimeoutWarning when getenv
// shows a Cloud Run service (K_SERVICE) and cfg declares no platform
// request timeout. It reports whether it logged. Called once, from
// validateServerPreflight.
func warnUndeclaredPlatformTimeout(cfg *config.GlobalConfig, getenv func(string) string, logger *slog.Logger) bool {
	if !cloudrunenv.IsServiceFrom(getenv) || cfg.Hub.PlatformRequestTimeout != "" {
		return false
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn(platformTimeoutWarning,
		"setting", "server.hub.platform_request_timeout",
		"env", "SCION_SERVER_HUB_PLATFORMREQUESTTIMEOUT",
		"events_stream_max_age", sseMaxConnectionAgeSetting(cfg).String(),
		"conduit_lifetime_cap", conduitLifetimeCapSetting(cfg).String())
	return true
}

// wireWebEventStreamMetrics hands the hub's scion.hub.sse.* recorder (set by
// wireHubCoreMetrics) to the web server's events stream registry
// (ptone/scion#5228).
func wireWebEventStreamMetrics(webSrv *hub.WebServer, hubSrv *hub.Server) {
	webSrv.SetEventStreamMetrics(hubSrv.EventStreamMetrics())
}
