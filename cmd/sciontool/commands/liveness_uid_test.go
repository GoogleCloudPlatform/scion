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
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/liveness"
)

func TestLivenessAgentUID(t *testing.T) {
	assert.Equal(t, 1000, livenessAgentUID(1000), "privilege drop: the target UID")
	assert.Equal(t, os.Getuid(), livenessAgentUID(0), "no drop: the harness inherits init's UID")
}

func TestLivenessHarnessRootDepth(t *testing.T) {
	envMap := func(m map[string]string) func(string) string {
		return func(k string) string {
			return m[k]
		}
	}
	assert.Equal(t, 1, livenessHarnessRootDepth(envMap(nil)), "unset")
	assert.Equal(t, 2, livenessHarnessRootDepth(envMap(map[string]string{state.LivenessHarnessRootDepthEnv: "2"})))
	assert.Equal(t, 2, livenessHarnessRootDepth(envMap(map[string]string{state.LivenessHarnessRootDepthEnv: " 2 "})))
	assert.Equal(t, 1, livenessHarnessRootDepth(envMap(map[string]string{state.LivenessHarnessRootDepthEnv: "two"})), "unparsable")
	assert.Equal(t, 2, livenessHarnessRootDepth(envMap(map[string]string{state.LivenessHarnessRootDepthHintEnv: "2"})), "hint fallback")
	assert.Equal(t, 3, livenessHarnessRootDepth(envMap(map[string]string{
		state.LivenessHarnessRootDepthEnv:     "3",
		state.LivenessHarnessRootDepthHintEnv: "2",
	})), "SCION_* wins over hint")
}

// The live-work callback reads the activity and its time afresh on every
// heartbeat, so the tool-call tier follows the agent's current tool call.
func TestLivenessProvider_ReadsActivityEachHeartbeat(t *testing.T) {
	calls := 0
	activity := func() (state.Activity, time.Time) {
		calls++
		return state.ActivityExecuting, time.Now()
	}
	// No process on this host runs as this UID, so nothing is work.
	s := liveness.NewSampler(liveness.Config{AgentUID: 1 << 30, IsManagedPID: func(int) bool { return false }})
	provide := livenessProvider(s, activity)
	assert.Nil(t, provide())
	assert.Nil(t, provide())
	assert.Equal(t, 2, calls)
}
