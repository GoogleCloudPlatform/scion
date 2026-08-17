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
	"testing"

	"github.com/stretchr/testify/require"
)

// The signing-key gate used to compare against the exact string "true", so an
// operator who set the variable to "1" or "TRUE" silently got the disabled
// path while believing the key was pinned across HA replicas.

func TestParseBoolEnvAcceptsTruthyValues(t *testing.T) {
	cases := []struct {
		name string
		val  string
	}{
		{"true", "true"},
		{"TRUE", "TRUE"},
		{"True", "True"},
		{"TrUe", "TrUe"},
		{"1", "1"},
		{"t", "t"},
		{"T", "T"},
		{"yes", "yes"},
		{"YES", "YES"},
		{"lowercase y", "y"},
		{"uppercase Y", "Y"},
		{"on", "on"},
		{"ON", "ON"},
		{"leading space", " true"},
		{"trailing space", "true "},
		{"trailing newline", "true\n"},
		{"padded 1", " 1 "},
		{"padded yes", " yes "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCION_REQUIRE_STABLE_SIGNING_KEY", tc.val)
			require.True(t, parseBoolEnv("SCION_REQUIRE_STABLE_SIGNING_KEY"))
		})
	}
}

func TestParseBoolEnvRejectsFalsyValues(t *testing.T) {
	for _, val := range []string{"false", "FALSE", "0", "f", "no", "n", "N", "off", "OFF", "", "maybe"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("SCION_REQUIRE_STABLE_SIGNING_KEY", val)
			require.False(t, parseBoolEnv("SCION_REQUIRE_STABLE_SIGNING_KEY"))
		})
	}
}

func TestParseBoolEnvUnsetIsFalse(t *testing.T) {
	require.False(t, parseBoolEnv("SCION_BOOL_ENV_THAT_IS_NOT_SET"))
}

// A value like "enabled" reads as an intent to turn the feature on. Falling
// back to false is the safe behaviour, but it must be visible to the operator.
func TestParseBoolEnvWarnsOnUnrecognizedValue(t *testing.T) {
	t.Setenv("SCION_REQUIRE_STABLE_SIGNING_KEY", "enabled")
	var result bool
	logged := captureLog(t, func() {
		result = parseBoolEnv("SCION_REQUIRE_STABLE_SIGNING_KEY")
	})
	require.False(t, result)
	require.Contains(t, logged, "not a recognized boolean value")
	require.Contains(t, logged, "SCION_REQUIRE_STABLE_SIGNING_KEY")
	require.Contains(t, logged, `"enabled"`)
}

// Recognized values — truthy, falsy, or empty — are not a misconfiguration, so
// they must stay silent.
func TestParseBoolEnvNoWarningOnRecognizedValues(t *testing.T) {
	for _, val := range []string{"", "  ", "true", "false", "1", "0", "yes", "off"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("SCION_REQUIRE_STABLE_SIGNING_KEY", val)
			logged := captureLog(t, func() {
				parseBoolEnv("SCION_REQUIRE_STABLE_SIGNING_KEY")
			})
			require.Empty(t, logged)
		})
	}
}
