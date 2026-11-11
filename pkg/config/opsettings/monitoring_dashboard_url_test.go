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

package opsettings

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// server.hub.monitoring_dashboard_url is a Layer-1 key of the endpoints
// section, so the admin API can write it and SCION_SEED_ can seed it.
func TestMonitoringDashboardURL_OwnedByEndpoints(t *testing.T) {
	if got := OwningSection(config.MonitoringDashboardURLKey); got != "endpoints" {
		t.Fatalf("OwningSection = %q, want endpoints", got)
	}
}

// The endpoints section schema enforces the same shape as the server-side
// check for values written by other tooling.
func TestMonitoringDashboardURL_EndpointsSchema(t *testing.T) {
	cases := []struct {
		value string
		ok    bool
	}{
		{"https://dash.example.com/d/hub?orgId=1#x", true},
		{"http://grafana:3000/d", true},
		{"javascript:alert(1)", false},
		{"/relative", false},
		{"https://user:pw@dash.example.com/", false},
		{"https://dash.example.com/" + strings.Repeat("a", config.MonitoringDashboardURLMaxLength), false},
	}
	for _, tc := range cases {
		doc, err := json.Marshal(EndpointsSettings{MonitoringDashboardURL: tc.value})
		if err != nil {
			t.Fatal(err)
		}
		errs := Validate("endpoints", doc)
		if tc.ok && len(errs) > 0 {
			t.Errorf("%q: unexpected schema errors %v", tc.value, errs)
		}
		if !tc.ok && len(errs) == 0 {
			t.Errorf("%q: schema accepted it", tc.value)
		}
	}
}

// The section document round-trips through the koanf keyspace.
func TestMonitoringDashboardURL_SectionKoanfRoundTrip(t *testing.T) {
	doc := json.RawMessage(`{"monitoring_dashboard_url":"https://dash.example.com/d"}`)
	k, err := LoadSectionsIntoKoanf(map[string]json.RawMessage{"endpoints": doc})
	if err != nil {
		t.Fatal(err)
	}
	if got := k.String(config.MonitoringDashboardURLKey); got != "https://dash.example.com/d" {
		t.Fatalf("koanf value = %q", got)
	}
	out, err := ExtractSectionFromKoanf(k, "endpoints")
	if err != nil {
		t.Fatal(err)
	}
	var d EndpointsSettings
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatal(err)
	}
	if d.MonitoringDashboardURL != "https://dash.example.com/d" {
		t.Fatalf("extracted doc = %s", out)
	}
}
