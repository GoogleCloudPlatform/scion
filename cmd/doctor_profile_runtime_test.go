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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
)

// TestDoctorPlatformHealth_ProfileRuntimeCheckLine: a broker that reports a
// profile runtime as unavailable gets a line naming the broker and the
// profile, also without --verbose; other checks add no such line.
func TestDoctorPlatformHealth_ProfileRuntimeCheckLine(t *testing.T) {
	s := &hubclient.HealthSummary{}
	s.Brokers.Total = 2
	s.Brokers.Items = []hubclient.HealthSummaryBroker{
		{Name: "broker-a", Status: "online", Health: &hubclient.HealthBrokerSelf{Status: "healthy", Checks: map[string]string{
			"docker":                  "available",
			"profile_runtime.old-gke": "unavailable",
		}}},
		{Name: "broker-b", Status: "online", Health: &hubclient.HealthBrokerSelf{Status: "healthy", Checks: map[string]string{"docker": "available"}}},
	}

	line := healthSummaryBrokersLine(s, false)
	assert.Contains(t, line, "broker broker-a: the runtime of profile old-gke is not available on this broker; check the profile's runtime configuration if agents use it")
	assert.Equal(t, 1, strings.Count(line, "is not available on this broker"), "a line for a broker without the check")
	assert.Contains(t, line, brokerProfileRuntimeNameNote, "the name note is missing")
	assert.NotContains(t, healthSummaryBrokersLine(&hubclient.HealthSummary{}, false), brokerProfileRuntimeNameNote, "the name note without any profile line")

	assert.Empty(t, brokerProfileRuntimeWarnings(hubclient.HealthSummaryBroker{Name: "x"}), "no health, no line")
}
