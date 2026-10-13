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
	"strings"
	"testing"
)

func TestValidateMonitoringDashboardURL(t *testing.T) {
	long := "https://dash.example.com/" + strings.Repeat("a", MonitoringDashboardURLMaxLength)
	atLimit := "https://dash.example.com/" + strings.Repeat("a", MonitoringDashboardURLMaxLength-len("https://dash.example.com/"))
	// 2048 characters but more than 2048 bytes: the limit counts characters.
	multiAtLimit := "https://dash.example.com/" + strings.Repeat("\u00e9", MonitoringDashboardURLMaxLength-len("https://dash.example.com/"))
	cases := []struct {
		name  string
		value string
		ok    bool
	}{
		{"empty means unset", "", true},
		{"https", "https://console.cloud.google.com/monitoring/dashboards/builder/abc?project=p", true},
		{"http with port", "http://grafana.internal:3000/d/hub", true},
		{"uppercase scheme", "HTTPS://dash.example.com", true},
		{"fragment", "https://dash.example.com/#/hub", true},
		{"at max length", atLimit, true},
		{"javascript scheme", "javascript:alert(1)", false},
		{"javascript scheme mixed case", "JavaScript:alert(1)", false},
		{"data scheme", "data:text/html,hi", false},
		{"ftp scheme", "ftp://dash.example.com/", false},
		{"relative path", "/monitoring", false},
		{"scheme-relative", "//dash.example.com/x", false},
		{"no host", "https:///path", false},
		{"opaque http", "http:dash.example.com", false},
		{"user credentials", "https://user:pw@dash.example.com/", false},
		{"whitespace", "https://dash.example.com/a b", false},
		{"leading space", " https://dash.example.com/", false},
		{"control character", "https://dash.example.com/\x01", false},
		{"DEL", "https://dash.example.com/\x7f", false},
		{"C1 NEL U+0085", "https://dash.example.com/\u0085", false},
		{"C1 U+009F", "https://dash.example.com/\u009f", false},
		{"no-break space U+00A0", "https://dash.example.com/\u00a0", false},
		{"trailing no-break space", "https://dash.example.com\u00a0", false},
		{"line separator U+2028", "https://dash.example.com/\u2028", false},
		{"paragraph separator U+2029", "https://dash.example.com/\u2029", false},
		{"ideographic space U+3000", "https://dash.example.com/?q=\u3000", false},
		{"arabic letter mark U+061C", "https://dash.example.com/\u061c", false},
		{"LRM U+200E", "https://dash.example.com/\u200e", false},
		{"RLM U+200F", "https://dash.example.com/\u200f", false},
		{"LRE U+202A", "https://dash.example.com/\u202a", false},
		{"RLO U+202E", "https://dash.example.com/\u202e", false},
		{"LRI U+2066", "https://dash.example.com/#\u2066", false},
		{"PDI U+2069", "https://dash.example.com/\u2069", false},
		{"BOM U+FEFF", "https://dash.example.com/\ufeff", false},
		{"invalid UTF-8", "https://dash.example.com/\xff", false},
		{"replacement character U+FFFD", "https://dash.example.com/\ufffd", false},
		{"soft hyphen U+00AD", "https://dash.example.com/\u00ad", false},
		{"mongolian vowel separator U+180E", "https://dash.example.com/\u180e", false},
		{"zero-width space U+200B", "https://dash.example.com/\u200b", false},
		{"zero-width non-joiner U+200C", "https://dash.example.com/\u200c", false},
		{"zero-width joiner U+200D", "https://dash.example.com/\u200d", false},
		{"word joiner U+2060", "https://dash.example.com/\u2060", false},
		{"C0 escape", "https://dash.example.com/\x1b", false},
		{"port 0", "https://dash.example.com:0/", false},
		{"port 65536", "https://dash.example.com:65536/", false},
		{"port 99999", "https://dash.example.com:99999/", false},
		{"port 1", "https://dash.example.com:1/", true},
		{"port 65535", "https://dash.example.com:65535/", true},
		{"IPv6 with port", "https://[::1]:3000/d", true},
		{"IDN host", "https://b\u00fccher.example/d", true},
		{"non-ASCII letters allowed", "https://dash.example.com/d/caf\u00e9", true},
		{"too long", long, false},
		{"multi-byte at max length", multiAtLimit, true},
		{"multi-byte over max length", multiAtLimit + "\u00e9", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMonitoringDashboardURL(tc.value)
			if tc.ok && err != nil {
				t.Fatalf("ValidateMonitoringDashboardURL(%q) = %v, want nil", tc.value, err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("ValidateMonitoringDashboardURL(%q) = nil, want an error", tc.value)
				}
				if !strings.HasPrefix(err.Error(), MonitoringDashboardURLKey+": ") {
					t.Errorf("error %q does not name the key", err)
				}
				if tc.value != "" && len(tc.value) > 8 && strings.Contains(err.Error(), tc.value) {
					t.Errorf("error %q echoes the value", err)
				}
				if got := MonitoringDashboardURLOrEmpty(tc.value); got != "" {
					t.Errorf("MonitoringDashboardURLOrEmpty(%q) = %q, want \"\"", tc.value, got)
				}
			}
		})
	}
}

// The env var names in the docs and the schema map to the registry key.
func TestMonitoringDashboardURLEnvKeys(t *testing.T) {
	t.Setenv("SCION_SERVER_HUB_MONITORINGDASHBOARDURL", "https://env.example.com/d")
	t.Setenv("SCION_SEED_SERVER_HUB_MONITORINGDASHBOARDURL", "https://seed.example.com/d")
	if got := LoadEnvKoanf().String(MonitoringDashboardURLKey); got != "https://env.example.com/d" {
		t.Errorf("SCION_SERVER_HUB_MONITORINGDASHBOARDURL -> %q, want the env value", got)
	}
	if got := LoadSeedEnvKoanf().String(MonitoringDashboardURLKey); got != "https://seed.example.com/d" {
		t.Errorf("SCION_SEED_SERVER_HUB_MONITORINGDASHBOARDURL -> %q, want the seed value", got)
	}
}

// settings.yaml (v1) and GlobalConfig carry the key both ways.
func TestMonitoringDashboardURLV1RoundTrip(t *testing.T) {
	v1 := &V1ServerConfig{Hub: &V1ServerHubConfig{MonitoringDashboardURL: "https://dash.example.com/x"}}
	gc := ConvertV1ServerToGlobalConfig(v1)
	if gc.Hub.MonitoringDashboardURL != "https://dash.example.com/x" {
		t.Fatalf("v1 -> GlobalConfig lost the value: %q", gc.Hub.MonitoringDashboardURL)
	}
	back := ConvertGlobalToV1ServerConfig(gc)
	if back.Hub == nil || back.Hub.MonitoringDashboardURL != "https://dash.example.com/x" {
		t.Fatalf("GlobalConfig -> v1 lost the value: %+v", back.Hub)
	}
}
