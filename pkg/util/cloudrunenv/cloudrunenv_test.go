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

package cloudrunenv

import "testing"

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

var detectionCases = []struct {
	name         string
	env          map[string]string
	wantName     string
	wantService  bool
	wantInstance bool
}{
	{name: "neither set", env: map[string]string{}},
	{name: "service", env: map[string]string{ServiceEnv: "scion-hub"}, wantName: "scion-hub", wantService: true},
	{name: "instance", env: map[string]string{InstanceEnv: "scion-broker"}, wantInstance: true},
	{name: "both set", env: map[string]string{ServiceEnv: "scion-hub", InstanceEnv: "scion-broker"}, wantName: "scion-hub", wantService: true, wantInstance: true},
	{name: "empty values", env: map[string]string{ServiceEnv: "", InstanceEnv: ""}},
}

// Fault check: IsServiceFrom reading InstanceEnv instead of ServiceEnv fails
// the "service" and "instance" cases.
func TestIsService(t *testing.T) {
	for _, tc := range detectionCases {
		if got := IsServiceFrom(envOf(tc.env)); got != tc.wantService {
			t.Errorf("%s: IsServiceFrom = %v, want %v", tc.name, got, tc.wantService)
		}
	}
	t.Setenv(ServiceEnv, "scion-hub")
	if !IsService() {
		t.Errorf("IsService = false with %s set, want true", ServiceEnv)
	}
	t.Setenv(ServiceEnv, "")
	if IsService() {
		t.Errorf("IsService = true with %s empty, want false", ServiceEnv)
	}
}

// Fault check: IsInstanceFrom comparing with == "" instead of != "" fails
// the "neither set" and "instance" cases.
func TestIsInstance(t *testing.T) {
	for _, tc := range detectionCases {
		if got := IsInstanceFrom(envOf(tc.env)); got != tc.wantInstance {
			t.Errorf("%s: IsInstanceFrom = %v, want %v", tc.name, got, tc.wantInstance)
		}
	}
	t.Setenv(InstanceEnv, "scion-broker")
	if !IsInstance() {
		t.Errorf("IsInstance = false with %s set, want true", InstanceEnv)
	}
	t.Setenv(InstanceEnv, "")
	if IsInstance() {
		t.Errorf("IsInstance = true with %s empty, want false", InstanceEnv)
	}
}

// Fault check: ServiceNameFrom returning "" fails the "service" and
// "both set" cases.
func TestServiceName(t *testing.T) {
	for _, tc := range detectionCases {
		if got := ServiceNameFrom(envOf(tc.env)); got != tc.wantName {
			t.Errorf("%s: ServiceNameFrom = %q, want %q", tc.name, got, tc.wantName)
		}
	}
	t.Setenv(ServiceEnv, "scion-hub")
	if got := ServiceName(); got != "scion-hub" {
		t.Errorf("ServiceName = %q, want %q", got, "scion-hub")
	}
}
