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

package control

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

func TestDeliverEnabled_RefusesWhenDeliveryDisabledSignalSet(t *testing.T) {
	// Broker-written SCION_HARNESS_DELIVERY_DISABLED=1 disables delivery even
	// if SCION_HARNESS in start env claims an LLM harness.
	getenv := func(k string) string {
		switch k {
		case EnvHubExperiments:
			return ExperimentConduitDeliver
		case state.HarnessDeliveryDisabledEnv:
			return "1"
		case "SCION_HARNESS":
			return "claude"
		default:
			return ""
		}
	}
	if DeliverEnabled(getenv, true) {
		t.Fatal("DeliverEnabled must return false when SCION_HARNESS_DELIVERY_DISABLED=1, even if SCION_HARNESS=claude")
	}

	// Without the broker-written signal, delivery is enabled even if SCION_HARNESS=shell.
	getenvNoSignal := func(k string) string {
		switch k {
		case EnvHubExperiments:
			return ExperimentConduitDeliver
		case "SCION_HARNESS":
			return "shell"
		default:
			return ""
		}
	}
	if !DeliverEnabled(getenvNoSignal, true) {
		t.Fatal("DeliverEnabled must key on SCION_HARNESS_DELIVERY_DISABLED rather than SCION_HARNESS")
	}
}
