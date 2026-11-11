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

package cloudrun

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func fakeGetenv(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

// TestWarnCPUAllocationIfNeeded covers ptone/scion#4763 mutant W1: the
// warning appears exactly once when every condition holds, and never when
// any one of them fails.
func TestWarnCPUAllocationIfNeeded(t *testing.T) {
	allMet := func() map[string]string {
		return map[string]string{"K_SERVICE": "scion-hub"}
	}
	cases := []struct {
		name          string
		brokerEnabled bool
		env           map[string]string
		want          int
	}{
		{name: "broker off", brokerEnabled: false, env: allMet(), want: 0},
		{name: "K_SERVICE unset", brokerEnabled: true, env: map[string]string{}, want: 0},
		{name: "CLOUD_RUN_INSTANCE set", brokerEnabled: true, env: map[string]string{"K_SERVICE": "scion-hub", "CLOUD_RUN_INSTANCE": "scion-broker"}, want: 0},
		{name: "opt-out true", brokerEnabled: true, env: map[string]string{"K_SERVICE": "scion-hub", CPUAlwaysAllocatedEnv: "true"}, want: 0},
		{name: "opt-out yes", brokerEnabled: true, env: map[string]string{"K_SERVICE": "scion-hub", CPUAlwaysAllocatedEnv: " YES\n"}, want: 0},
		{name: "opt-out false", brokerEnabled: true, env: map[string]string{"K_SERVICE": "scion-hub", CPUAlwaysAllocatedEnv: "false"}, want: 1},
		{name: "all conditions met", brokerEnabled: true, env: allMet(), want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			WarnCPUAllocationIfNeeded(logger, fakeGetenv(tc.env), tc.brokerEnabled)
			out := buf.String()
			if got := strings.Count(out, "async agent launches will stall after the create response"); got != tc.want {
				t.Fatalf("warning count = %d, want %d; logs:\n%s", got, tc.want, out)
			}
			if tc.want == 1 && !strings.Contains(out, "level=WARN") {
				t.Fatalf("warning not logged at WARN level; logs:\n%s", out)
			}
		})
	}
}
