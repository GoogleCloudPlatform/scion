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

package autoexpose

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variable names for auto-expose configuration.
const (
	EnvAutoExposePorts    = "SCION_AUTO_EXPOSE_PORTS"
	EnvAutoExposeMode     = "SCION_AUTO_EXPOSE_MODE"
	EnvAutoExposeList     = "SCION_AUTO_EXPOSE_PORTS_LIST"
	EnvAutoExposeInterval = "SCION_AUTO_EXPOSE_INTERVAL"
	EnvAutoExposeMinPort  = "SCION_AUTO_EXPOSE_MIN_PORT"
)

// Filter modes for auto-expose.
const (
	FilterModeAllowlist = "allowlist"
	FilterModeDenylist  = "denylist"
)

// Default configuration values.
const (
	DefaultInterval = 3 * time.Second
	DefaultMinPort  = 1024
	DefaultMaxPorts = 10
)

// DefaultDeniedPorts are infrastructure ports that must never be auto-exposed.
// Matches the hub-side deniedExposedPorts in port_forward_handlers.go.
// Port 8080 is intentionally excluded: the reverse tunnel architecture makes it
// safe to expose, and in many deployments the hub API is served on a different
// port behind a load balancer, making path-based forwarding on 8080 valid.
var DefaultDeniedPorts = []int{9810, 18380}

// Config holds the auto-expose configuration.
type Config struct {
	// Enabled gates whether the auto-expose loop runs.
	Enabled bool

	// Interval is the scan/reconcile cycle interval.
	Interval time.Duration

	// FilterMode is "allowlist" or "denylist".
	FilterMode string

	// FilterPorts is the list of ports for the active filter mode.
	// In allowlist mode: only these ports are auto-exposed.
	// In denylist mode: these ports are excluded from auto-exposure.
	FilterPorts []int

	// DeniedPorts are always denied regardless of filter mode (infrastructure ports).
	DeniedPorts []int

	// MaxPorts is the maximum number of auto-exposed ports (matches hub limit).
	MaxPorts int

	// MinPort is the minimum port number to auto-expose.
	MinPort int
}

// ConfigFromEnv reads auto-expose configuration from environment variables.
func ConfigFromEnv() Config {
	cfg := Config{
		Enabled:     envBool(EnvAutoExposePorts, false),
		Interval:    envDuration(EnvAutoExposeInterval, DefaultInterval),
		FilterMode:  envString(EnvAutoExposeMode, FilterModeAllowlist),
		FilterPorts: envIntList(EnvAutoExposeList),
		DeniedPorts: DefaultDeniedPorts,
		MaxPorts:    DefaultMaxPorts,
		MinPort:     envInt(EnvAutoExposeMinPort, DefaultMinPort),
	}

	// Enforce minimum interval floor to prevent hammering the hub.
	if cfg.Interval < time.Second {
		cfg.Interval = time.Second
	}

	// Normalize filter mode.
	switch strings.ToLower(cfg.FilterMode) {
	case FilterModeAllowlist, FilterModeDenylist:
		cfg.FilterMode = strings.ToLower(cfg.FilterMode)
	default:
		cfg.FilterMode = FilterModeAllowlist
	}

	return cfg
}

func envBool(key string, defaultVal bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	return strings.EqualFold(v, "true") || v == "1"
}

func envString(key, defaultVal string) string {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	return v
}

func envDuration(key string, defaultVal time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return defaultVal
	}
	return d
}

func envInt(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}
	return n
}

func envIntList(key string) []int {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	var result []int
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			continue
		}
		result = append(result, n)
	}
	return result
}
