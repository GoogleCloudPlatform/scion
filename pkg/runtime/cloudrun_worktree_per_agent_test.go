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

package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestCloudRunRuntime_RejectsWorktreePerAgent pins that Cloud Run refuses
// worktree-per-agent before resolving NFS or making any API call, naming the
// supported modes (clone-per-agent and shared-plain), while allowing
// clone-per-agent and shared-plain.
func TestCloudRunRuntime_RejectsWorktreePerAgent(t *testing.T) {
	r := &CloudRunRuntime{}
	_, err := r.Run(context.Background(), RunConfig{
		Env:    []string{"SCION_AGENT_ID=a1", "SCION_WORKSPACE_MODE=worktree-per-agent"},
		Labels: map[string]string{"agent_id": "a1"},
	})
	if !errors.Is(err, errWorktreePerAgentCloudRun) {
		t.Fatalf("Run(worktree-per-agent) error = %v, want errWorktreePerAgentCloudRun", err)
	}
	msg := err.Error()
	for _, want := range []string{"worktree-per-agent", "clone-per-agent", "shared-plain"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q must mention %q", msg, want)
		}
	}

	for _, env := range [][]string{
		nil,
		{"SCION_WORKSPACE_MODE=clone-per-agent"},
		{"SCION_WORKSPACE_MODE=per-agent"},
		{"SCION_WORKSPACE_MODE=shared-plain"},
		{"SCION_WORKSPACE_MODE=shared"},
		{"OTHER=worktree-per-agent"},
	} {
		if err := rejectWorktreePerAgentOnCloudRun(RunConfig{Env: env}); err != nil {
			t.Errorf("rejectWorktreePerAgentOnCloudRun(%v) = %v, want nil", env, err)
		}
	}
}
