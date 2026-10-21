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

package store

import "testing"

// A move recorded without a transfer mode predates the field and was an
// export move; a plain reincarnation has no transfer mode at all.
func TestAgentReincarnation_EffectiveTransferMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  AgentReincarnation
		want string
	}{
		{"plain reincarnation", AgentReincarnation{}, ""},
		{"legacy move", AgentReincarnation{SourceBrokerID: "a", TargetBrokerID: "b"}, TransferModeExport},
		{"export move", AgentReincarnation{SourceBrokerID: "a", TargetBrokerID: "b", TransferMode: TransferModeExport}, TransferModeExport},
		{"forced move", AgentReincarnation{SourceBrokerID: "a", TargetBrokerID: "b", TransferMode: TransferModeRecreate}, TransferModeRecreate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rec.EffectiveTransferMode(); got != tc.want {
				t.Fatalf("EffectiveTransferMode() = %q, want %q", got, tc.want)
			}
		})
	}
}
