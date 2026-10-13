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

package agent

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestDefaultReadySpec_MatchesRuntimeConstants pins the harness-config ready
// defaults to the readiness constants the runtime uses today, so a bundle
// that leaves a value unset behaves exactly like the built-in rule.
func TestDefaultReadySpec_MatchesRuntimeConstants(t *testing.T) {
	delay := config.DefaultReadySpec(config.ReadyStrategyDelay)
	if delay.Settle != state.ReadySettleSession {
		t.Errorf("delay settle = %v, want state.ReadySettleSession %v", delay.Settle, state.ReadySettleSession)
	}
	hook := config.DefaultReadySpec(config.ReadyStrategyHook)
	if hook.Settle != defaultReadySettle {
		t.Errorf("hook settle = %v, want host defaultReadySettle %v", hook.Settle, defaultReadySettle)
	}
	if hook.Timeout != state.ReadyDeadline {
		t.Errorf("hook timeout = %v, want state.ReadyDeadline %v", hook.Timeout, state.ReadyDeadline)
	}
}
