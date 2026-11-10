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
	"fmt"
	"time"
)

// Long-lived request bounds of the hub (ptone/scion#5230, ptone/scion#5245):
// the browser events stream (server.hub.sse) and the declared platform
// request timeout (server.hub.platform_request_timeout) that clamps both the
// events stream maximum age and the conduit session lifetime cap.
const (
	// SSEDefaultMaxConnectionAge is the default server.hub.sse.max_connection_age.
	SSEDefaultMaxConnectionAge = 10 * time.Minute
	// SSEMinMaxConnectionAge and SSEMaxMaxConnectionAge bound max_connection_age.
	SSEMinMaxConnectionAge = time.Minute
	SSEMaxMaxConnectionAge = 24 * time.Hour

	// SSEDefaultHeartbeatInterval is the default server.hub.sse.heartbeat_interval.
	SSEDefaultHeartbeatInterval = 30 * time.Second
	// SSEMinHeartbeatInterval and SSEMaxHeartbeatInterval bound
	// heartbeat_interval. The maximum stays below the web client's 75s
	// stale-stream threshold.
	SSEMinHeartbeatInterval = 5 * time.Second
	SSEMaxHeartbeatInterval = time.Minute

	// MinPlatformRequestTimeout and MaxPlatformRequestTimeout bound
	// server.hub.platform_request_timeout. The minimum keeps the clamped
	// conduit lifetime cap (0.8 of the timeout, jittered down to 0.8 of
	// that) at or above ConduitMinLifetimeCap.
	MinPlatformRequestTimeout = 150 * time.Second
	MaxPlatformRequestTimeout = 24 * time.Hour

	// PlatformTimeoutShare is the share of the declared platform request
	// timeout a long-lived request may use before the hub ends it.
	PlatformTimeoutShare = 0.8
	// LifetimeJitter is the per-connection jitter fraction applied to an
	// events stream maximum age and a conduit session lifetime cap, so
	// connections opened together do not all end together.
	LifetimeJitter = 0.2
)

// HubSSEConfig holds the browser events stream settings of the hub server
// (server.hub.sse). The values are kept as configured and checked by
// Validate at startup; an invalid value is a startup error.
type HubSSEConfig struct {
	// MaxConnectionAge is the maximum age of an events stream before the
	// hub sends "event: reconnect" and closes it ("" =
	// SSEDefaultMaxConnectionAge). Clamped by the platform request timeout
	// and jittered per stream (EffectiveSSEMaxAge, JitterLifetime).
	MaxConnectionAge string `json:"maxConnectionAge,omitempty" yaml:"maxConnectionAge,omitempty" koanf:"maxConnectionAge"`
	// HeartbeatInterval is the interval between events stream heartbeats
	// ("" = SSEDefaultHeartbeatInterval).
	HeartbeatInterval string `json:"heartbeatInterval,omitempty" yaml:"heartbeatInterval,omitempty" koanf:"heartbeatInterval"`
}

// IsZero reports whether no events stream setting is configured.
func (c HubSSEConfig) IsZero() bool { return c.MaxConnectionAge == "" && c.HeartbeatInterval == "" }

// MaxConnectionAgeDuration parses MaxConnectionAge ("" =
// SSEDefaultMaxConnectionAge).
func (c HubSSEConfig) MaxConnectionAgeDuration() (time.Duration, error) {
	return parseLongLivedBound("server.hub.sse.max_connection_age", c.MaxConnectionAge,
		SSEDefaultMaxConnectionAge, SSEMinMaxConnectionAge, SSEMaxMaxConnectionAge)
}

// HeartbeatIntervalDuration parses HeartbeatInterval ("" =
// SSEDefaultHeartbeatInterval).
func (c HubSSEConfig) HeartbeatIntervalDuration() (time.Duration, error) {
	return parseLongLivedBound("server.hub.sse.heartbeat_interval", c.HeartbeatInterval,
		SSEDefaultHeartbeatInterval, SSEMinHeartbeatInterval, SSEMaxHeartbeatInterval)
}

// Validate checks every events stream setting.
func (c HubSSEConfig) Validate() error {
	if _, err := c.MaxConnectionAgeDuration(); err != nil {
		return err
	}
	_, err := c.HeartbeatIntervalDuration()
	return err
}

// PlatformRequestTimeoutDuration parses a server.hub.platform_request_timeout
// value. "" means none is declared and returns 0.
func PlatformRequestTimeoutDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	return parseLongLivedBound("server.hub.platform_request_timeout", s, 0,
		MinPlatformRequestTimeout, MaxPlatformRequestTimeout)
}

// ValidatePlatformRequestTimeout checks a server.hub.platform_request_timeout value.
func ValidatePlatformRequestTimeout(s string) error {
	_, err := PlatformRequestTimeoutDuration(s)
	return err
}

// parseLongLivedBound parses s as a duration in [lo, hi] ("" = def).
func parseLongLivedBound(key, s string, def, lo, hi time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", key, s, err)
	}
	if d < lo || d > hi {
		return 0, fmt.Errorf("invalid %s %q: must be between %s and %s", key, s, lo, hi)
	}
	return d, nil
}

// platformShare is PlatformTimeoutShare of platform.
func platformShare(platform time.Duration) time.Duration {
	return time.Duration(float64(platform) * PlatformTimeoutShare)
}

// EffectiveSSEMaxAge is the events stream maximum age before jitter: the
// configured age, lowered to PlatformTimeoutShare of the platform request
// timeout when one is declared (platform > 0).
func EffectiveSSEMaxAge(configured, platform time.Duration) time.Duration {
	if platform > 0 {
		return min(configured, platformShare(platform))
	}
	return configured
}

// EffectiveConduitLifetimeCap is the conduit session lifetime cap before
// jitter: the configured cap, lowered to PlatformTimeoutShare of the
// platform request timeout when one is declared (platform > 0), and never
// below ConduitMinLifetimeCap.
func EffectiveConduitLifetimeCap(configured, platform time.Duration) time.Duration {
	if platform > 0 {
		return max(min(configured, platformShare(platform)), ConduitMinLifetimeCap)
	}
	return configured
}

// JitterLifetime returns base scaled by a factor drawn from
// [1-LifetimeJitter, 1+up*LifetimeJitter], where u in [0, 1) picks the point
// in that range (u = 0 gives the lowest value). up is 1 for a symmetric
// jitter and 0 for a jitter that only lowers base (used when base is itself
// a hard platform limit).
func JitterLifetime(base time.Duration, up float64, u float64) time.Duration {
	lo := 1 - LifetimeJitter
	hi := 1 + up*LifetimeJitter
	return time.Duration(float64(base) * (lo + u*(hi-lo)))
}

// JitterConduitLifetimeCap is one conduit session's lifetime cap: base
// (EffectiveConduitLifetimeCap) jittered by JitterLifetime, with u in [0, 1).
// With a declared platform request timeout the jitter is symmetric (base is
// at most PlatformTimeoutShare of the timeout, so the result stays below
// it); without one, base is itself the platform cap and the jitter only
// lowers it. The result is never below min(base, ConduitMinLifetimeCap), so
// the GoAway lead and drain still fit.
func JitterConduitLifetimeCap(base time.Duration, platformDeclared bool, u float64) time.Duration {
	up := 0.0
	if platformDeclared {
		up = 1
	}
	return max(JitterLifetime(base, up, u), min(base, ConduitMinLifetimeCap))
}

// ConduitLifetimeCapCeiling is the highest per-session cap
// JitterConduitLifetimeCap can give for base: 1.2 x base with a declared
// platform request timeout, base without one. A hub in external relay mode
// sends it as the ingress lifetime cap, so the relay's own per-session
// jitter (the one side that jitters) is never cut back to a single value by
// the hub's cap, while the session still ends below the platform timeout.
func ConduitLifetimeCapCeiling(base time.Duration, platformDeclared bool) time.Duration {
	if platformDeclared {
		return time.Duration(float64(base) * (1 + LifetimeJitter))
	}
	return base
}
