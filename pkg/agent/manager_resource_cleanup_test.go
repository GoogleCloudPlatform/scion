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
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

type cleanerRuntime struct {
	runtime.MockRuntime
	agentName, projectID string
	err                  error
}

func (c *cleanerRuntime) CleanupAgentResources(_ context.Context, agentName, projectID string) error {
	c.agentName, c.projectID = agentName, projectID
	return c.err
}

func TestAgentManager_CleanupAgentResources_Delegates(t *testing.T) {
	want := errors.New("list failed")
	rt := &cleanerRuntime{err: want}
	m := &AgentManager{Runtime: rt}
	if err := m.CleanupAgentResources(context.Background(), "dev", "p1"); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if rt.agentName != "dev" || rt.projectID != "p1" {
		t.Errorf("runtime got (%q, %q), want (dev, p1)", rt.agentName, rt.projectID)
	}
}

func TestAgentManager_CleanupAgentResources_UnsupportedRuntimeNoOp(t *testing.T) {
	m := &AgentManager{Runtime: &runtime.MockRuntime{}}
	if err := m.CleanupAgentResources(context.Background(), "dev", "p1"); err != nil {
		t.Fatalf("unsupported runtime should be a no-op, got %v", err)
	}
}
