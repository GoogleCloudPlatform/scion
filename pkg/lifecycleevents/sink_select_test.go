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

package lifecycleevents

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// TestSelectSinkKind pins design section 22 item 4: Cloud Logging when
// enabled; stdout only when every workstation condition holds (each one
// alone failing gives none, including hosted-like configurations that set
// loopback or dev auth); otherwise none.
func TestSelectSinkKind(t *testing.T) {
	workstation := SinkEnvironment{LoopbackOnly: true, SQLite: true, DevAuth: true}
	if got := SelectSinkKind(workstation); got != telemetryemit.SinkKindStdout {
		t.Fatalf("workstation: got %q, want stdout", got)
	}
	cloud := workstation
	cloud.CloudLogging, cloud.CloudRunService = true, true
	if got := SelectSinkKind(cloud); got != telemetryemit.SinkKindCloudLogging {
		t.Fatalf("cloud logging enabled: got %q, want cloud_logging", got)
	}
	fails := map[string]func(*SinkEnvironment){
		"not loopback":     func(e *SinkEnvironment) { e.LoopbackOnly = false },
		"postgres":         func(e *SinkEnvironment) { e.SQLite = false },
		"no dev auth":      func(e *SinkEnvironment) { e.DevAuth = false },
		"cloud run":        func(e *SinkEnvironment) { e.CloudRunService = true },
		"kubernetes":       func(e *SinkEnvironment) { e.Kubernetes = true },
		"hosted, loopback": func(e *SinkEnvironment) { *e = SinkEnvironment{LoopbackOnly: true, CloudRunService: true} },
		"hosted, dev auth": func(e *SinkEnvironment) {
			*e = SinkEnvironment{DevAuth: true, LoopbackOnly: true, SQLite: true, Kubernetes: true}
		},
	}
	for name, mutate := range fails {
		env := workstation
		mutate(&env)
		if got := SelectSinkKind(env); got != telemetryemit.SinkKindNone {
			t.Errorf("%s: got %q, want none (fail closed)", name, got)
		}
	}
}

// TestNewDispatchResult pins the broker-answer mapping of E(iv) (r7 N1/N2)
// and the F2 ruling (tele-wp1-arch 22:06): only an answer that itself said
// running is a start or a legacy running observation.
func TestNewDispatchResult(t *testing.T) {
	cases := []struct {
		name    string
		broker  string
		running bool
		want    DispatchResult
	}{
		{"named and running: E(iv)", "R1", true, DispatchResult{RunID: "R1", BrokerNamedRun: true, BrokerEvidence: true}},
		{"named, starting: correlated, no start", "R1", false, DispatchResult{RunID: "R1", BrokerNamedRun: true}},
		{"legacy and running: P1b", "", true, DispatchResult{RunID: "R1", NoBrokerRun: true}},
		{"legacy, starting, Hub-forced running: nothing", "", false, DispatchResult{RunID: "R1"}},
		{"another run (failed CAS): nothing", "L1", true, DispatchResult{RunID: "R1"}},
	}
	for _, c := range cases {
		if got := NewDispatchResult("R1", c.broker, c.running); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}
