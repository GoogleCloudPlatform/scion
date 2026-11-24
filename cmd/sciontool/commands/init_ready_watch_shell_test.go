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

package commands

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

func TestWatchHarnessReady_IdleStrategy(t *testing.T) {
	var mu sync.Mutex
	localAct := state.ActivityInitializing
	var reportedAct state.Activity
	var reportedMsg string
	reportCount := 0

	cfg := readyWatchConfig{
		Nonce:        "deadbeef",
		Mode:         state.ReadyModeSettle,
		Strategy:     state.ReadyStrategyIdle,
		Start:        time.Now().Add(-2 * time.Second),
		PollInterval: 5 * time.Millisecond,
		Settle:       50 * time.Millisecond,
		LocalActivity: func() state.Activity {
			mu.Lock()
			defer mu.Unlock()
			return localAct
		},
		ReplaceLocal: func(from, to state.Activity) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			if localAct != from {
				return false, nil
			}
			localAct = to
			return true, nil
		},
		Report: func(_ context.Context, act state.Activity, msg string) error {
			mu.Lock()
			defer mu.Unlock()
			reportedAct = act
			reportedMsg = msg
			reportCount++
			return nil
		},
	}

	outcome := watchHarnessReady(context.Background(), cfg)
	if outcome != readyOutcomeSettled {
		t.Fatalf("outcome = %q, want %q", outcome, readyOutcomeSettled)
	}
	if localAct != state.ActivityIdle {
		t.Errorf("local activity = %q, want %q", localAct, state.ActivityIdle)
	}
	if reportCount != 1 {
		t.Fatalf("reportCount = %d, want 1", reportCount)
	}
	if reportedAct != state.ActivityIdle {
		t.Errorf("reported activity = %q, want %q", reportedAct, state.ActivityIdle)
	}
	if reportedMsg != "Execution container ready" {
		t.Errorf("reported message = %q, want %q", reportedMsg, "Execution container ready")
	}
}

func TestLivenessAttachedChecker_IdleReadySpecOnly(t *testing.T) {
	idleEnv := state.FormatReadySpecEnv(state.DefaultReadySpec(state.ReadyStrategyIdle))
	// Broker-written ReadyStrategyIdle enables the attached checker even if
	// SCION_HARNESS in start env claims a different harness.
	getenvIdle := func(k string) string {
		switch k {
		case state.HarnessReadyEnv:
			return idleEnv
		case "SCION_HARNESS":
			return "claude"
		default:
			return ""
		}
	}
	if fn := livenessAttachedChecker(getenvIdle, 1000, 1000); fn == nil {
		t.Fatal("expected non-nil checker when SCION_HARNESS_READY has strategy idle")
	}

	// Non-idle strategies (and unset/malformed) return nil even if SCION_HARNESS=shell.
	for _, raw := range []string{
		"",
		"not-json",
		state.FormatReadySpecEnv(state.DefaultReadySpec(state.ReadyStrategyDelay)),
		state.FormatReadySpecEnv(state.DefaultReadySpec(state.ReadyStrategyHook)),
	} {
		getenv := func(k string) string {
			switch k {
			case state.HarnessReadyEnv:
				return raw
			case "SCION_HARNESS":
				return "shell"
			default:
				return ""
			}
		}
		if fn := livenessAttachedChecker(getenv, 1000, 1000); fn != nil {
			t.Errorf("expected nil checker for SCION_HARNESS_READY=%q", raw)
		}
	}
}
