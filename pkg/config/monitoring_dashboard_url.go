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
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
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
// https URL with a host, no user credentials, and at most
// MonitoringDashboardURLMaxLength bytes, that contains no whitespace (any
// Unicode White_Space, e.g. U+00A0, U+2028, U+2029), no control character
// (C0, DEL or C1, e.g. U+0085), no bidirectional formatting character
// (U+061C, U+200E, U+200F, U+202A to U+202E, U+2066 to U+2069), no
// invisible format character (U+00AD, U+180E, U+200B to U+200D, U+2060,
// U+FEFF) and no U+FFFD, and whose port, if any, is 1 to 65535. A path,
// query and fragment are allowed, since dashboard links commonly carry
// them. This is the rule of record; web/src/utils/http-url.ts mirrors it
// for display.
//
// Invalid UTF-8 never reaches this function through the admin API:
// json.Unmarshal turns it into U+FFFD, which is rejected. The
// utf8.ValidString check covers values from settings.yaml, env vars or
// rows written by other tooling.
//
// Like ValidateAgentEndpoint, no error message echoes any part of raw.
func ValidateMonitoringDashboardURL(raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > MonitoringDashboardURLMaxLength {
		return fmt.Errorf("%s: must be at most %d characters", MonitoringDashboardURLKey, MonitoringDashboardURLMaxLength)
	}
	if !utf8.ValidString(raw) {
		return fmt.Errorf("%s: must be valid UTF-8", MonitoringDashboardURLKey)
	}
	for _, r := range raw {
		if isDisallowedMonitoringURLRune(r) {
			return fmt.Errorf("%s: must not contain whitespace, control or bidirectional formatting characters", MonitoringDashboardURLKey)
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
	if p := u.Port(); p != "" {
		// url.Parse only checks that the port is numeric.
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%s: port must be between 1 and 65535", MonitoringDashboardURLKey)
		}
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

// isDisallowedMonitoringURLRune reports whether r may not appear anywhere in
// a monitoring dashboard URL (see ValidateMonitoringDashboardURL).
func isDisallowedMonitoringURLRune(r rune) bool {
	if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
		return true
	}
	// Invisible format characters: soft hyphen, Mongolian vowel separator,
	// zero-width space / non-joiner / joiner, word joiner, and U+FEFF; and
	// U+FFFD, which is what invalid UTF-8 decodes to.
	switch r {
	case '\u00AD', '\u180E', '\u200B', '\u200C', '\u200D', '\u2060', '\uFEFF', '\uFFFD':
		return true
	}
	return false
}
