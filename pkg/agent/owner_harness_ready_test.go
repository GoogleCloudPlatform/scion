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
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// twoProjectWorkerRuntime lists one "worker" agent in each of two projects
// of the same Runtime Broker instance and records every Exec.
func twoProjectWorkerRuntime() (*runtime.MockRuntime, func() []string) {
	var mu sync.Mutex
	var execs []string
	rt := &runtime.MockRuntime{
		ListFunc: func(_ context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			all := []api.AgentInfo{
				{ContainerID: "c-1", Name: "worker", ContainerStatus: "Up 1 minute", Labels: map[string]string{"scion.name": "worker", "scion.project_id": "proj-1", api.LabelRuntimeBrokerID: "broker-a"}},
				{ContainerID: "c-2", Name: "worker", ContainerStatus: "Up 1 minute", Labels: map[string]string{"scion.name": "worker", "scion.project_id": "proj-2", api.LabelRuntimeBrokerID: "broker-a"}},
			}
			var out []api.AgentInfo
			for _, a := range all {
				if p := filter["scion.project_id"]; p == "" || a.Labels["scion.project_id"] == p {
					out = append(out, a)
				}
			}
			return out, nil
		},
		ExecFunc: func(_ context.Context, id string, _ []string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			execs = append(execs, id)
			return "", nil
		},
	}
	return rt, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), execs...)
	}
}

// TestOwner_HarnessReadyWithoutProjectNeverProbesAmongSeveral: an owned
// manager's harness-readiness wait for a name that matches several of its
// agents in different projects, with no project given, returns before any
// probe (no Exec into either agent), and delivery refuses the name with
// the same text. With a project the wait probes only that project's agent,
// and a legacy manager still probes as before.
func TestOwner_HarnessReadyWithoutProjectNeverProbesAmongSeveral(t *testing.T) {
	timings := fastReadyTimings
	timings.Deadline = 200 * time.Millisecond

	t.Run("owned, no project", func(t *testing.T) {
		rt, execs := twoProjectWorkerRuntime()
		m := NewManager(rt).(*AgentManager)
		m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a"})
		long := timings
		long.Deadline = time.Minute
		m.readyTimings = long

		start := time.Now()
		m.waitHarnessReady(context.Background(), "worker", "")
		assert.Less(t, time.Since(start), time.Second, "the wait returns at once, without waiting for readiness")
		assert.Empty(t, execs(), "no readiness probe into any agent of the ambiguous name")

		want := m.refuseAmbiguousOwnedTarget("worker", "", 2)
		require.Error(t, want)
		err := m.deliverImmediate(context.Background(), "worker", "", "hello", true)
		require.Error(t, err)
		assert.Equal(t, want.Error(), err.Error(), "delivery refuses with the same text")
		assert.Contains(t, err.Error(), "ambiguous without a project")
		assert.Empty(t, execs(), "nothing delivered either")
	})

	t.Run("owned, with project", func(t *testing.T) {
		rt, execs := twoProjectWorkerRuntime()
		m := NewManager(rt).(*AgentManager)
		m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a"})
		m.readyTimings = timings

		m.waitHarnessReady(context.Background(), "worker", "proj-2")
		got := execs()
		require.NotEmpty(t, got, "the project's agent is probed")
		for _, id := range got {
			assert.Equal(t, "c-2", id)
		}
	})

	t.Run("legacy, no project", func(t *testing.T) {
		rt, execs := twoProjectWorkerRuntime()
		m := NewManager(rt).(*AgentManager)
		m.readyTimings = timings

		m.waitHarnessReady(context.Background(), "worker", "")
		got := execs()
		require.NotEmpty(t, got, "a legacy manager probes the first match, as before")
		for _, id := range got {
			assert.Equal(t, "c-1", id)
		}
	})
}
