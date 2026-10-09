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
	"net/url"
	"strings"
)

// MonitoringDashboardURLKey is the settings key of the monitoring dashboard
// link shown on the hub Health page.
const MonitoringDashboardURLKey = "server.hub.monitoring_dashboard_url"

// MonitoringDashboardURLMaxLength is the longest accepted
// server.hub.monitoring_dashboard_url, in bytes. It matches the maxLength
// of the key in settings-v1.schema.json.
const MonitoringDashboardURLMaxLength = 2048

// ValidateMonitoringDashboardURL reports whether raw is an acceptable
// server.hub.monitoring_dashboard_url: "" (unset), or an absolute http or
// https URL with a host, no user credentials, no whitespace or control
// characters, and at most MonitoringDashboardURLMaxLength bytes. A path,
// query and fragment are allowed, since dashboard links commonly carry
// them.
//
// Like ValidateAgentEndpoint, no error message echoes any part of raw.
func ValidateMonitoringDashboardURL(raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > MonitoringDashboardURLMaxLength {
		return fmt.Errorf("%s: must be at most %d characters", MonitoringDashboardURLKey, MonitoringDashboardURLMaxLength)
	}
	for _, r := range raw {
		if r <= 0x20 || r == 0x7f {
			return fmt.Errorf("%s: must not contain whitespace or control characters", MonitoringDashboardURLKey)
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL", MonitoringDashboardURLKey)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("%s: must be an absolute http or https URL", MonitoringDashboardURLKey)
	}
	if u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("%s: must be an absolute http or https URL with a host", MonitoringDashboardURLKey)
	}
	if u.User != nil {
		return fmt.Errorf("%s: must not contain user credentials", MonitoringDashboardURLKey)
	}
	return nil
}

// MonitoringDashboardURLOrEmpty returns raw when it passes
// ValidateMonitoringDashboardURL, and "" otherwise. Readers use it so a
// value that never went through the admin API (a hand-edited settings.yaml,
// an env var or a DB row written by other tooling) is never served.
func MonitoringDashboardURLOrEmpty(raw string) string {
	if ValidateMonitoringDashboardURL(raw) != nil {
		return ""
	}
	return raw
}
