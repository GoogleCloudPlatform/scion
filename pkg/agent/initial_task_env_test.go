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
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Start sets SCION_INITIAL_TASK=1 in the container env exactly when the
// start has a task, for sciontool init's first-turn watchdog
// (ptone/scion#7858). A task-less start and a resume drop any value the
// request env carried.
func TestStart_InitialTaskEnv(t *testing.T) {
	for _, tc := range []struct {
		name   string
		task   string
		resume bool
		env    map[string]string
		want   []string
	}{
		{name: "task", task: "fix the bug", want: []string{"1"}},
		{name: "task replaces inherited value", task: "fix the bug", env: map[string]string{"SCION_INITIAL_TASK": "0"}, want: []string{"1"}},
		{name: "no task", want: nil},
		{name: "no task drops inherited value", env: map[string]string{"SCION_INITIAL_TASK": "1"}, want: nil},
		{name: "resume drops inherited value", resume: true, env: map[string]string{"SCION_INITIAL_TASK": "1"}, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectScionDir, _ := startTZFixture(t, "", `""`)
			var env []string
			rt := &runtime.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
				RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
					env = cfg.Env
					return "mock-id", nil
				},
			}
			if _, err := NewManager(rt).Start(context.Background(), api.StartOptions{
				Name:        "tz-agent",
				ProjectPath: projectScionDir,
				BrokerMode:  true,
				NoAuth:      true,
				Task:        tc.task,
				Resume:      tc.resume,
				Env:         tc.env,
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			var got []string
			for _, kv := range env {
				if v, ok := strings.CutPrefix(kv, "SCION_INITIAL_TASK="); ok {
					got = append(got, v)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("env SCION_INITIAL_TASK = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWithInitialTaskEnv(t *testing.T) {
	in := []string{"A=1", "SCION_INITIAL_TASK=0", "SCION_INITIAL_TASKX=keep", "SCION_INITIAL_TASK=1"}
	if got, want := withInitialTaskEnv(in, true), []string{"A=1", "SCION_INITIAL_TASKX=keep", "SCION_INITIAL_TASK=1"}; !slices.Equal(got, want) {
		t.Errorf("hasTask: got %q, want %q", got, want)
	}
	if got, want := withInitialTaskEnv(in, false), []string{"A=1", "SCION_INITIAL_TASKX=keep"}; !slices.Equal(got, want) {
		t.Errorf("no task: got %q, want %q", got, want)
	}
}
