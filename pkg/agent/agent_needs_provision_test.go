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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentNeedsProvision covers the three states GetAgent distinguishes:
// a provisioned agent is loaded, while a missing agent directory and a stale
// one without scion-agent.json are provisioned again (ptone/scion#2157).
func TestAgentNeedsProvision(t *testing.T) {
	fx := newFreshProvisionManagerFixture(t)

	needs, err := AgentNeedsProvision(context.Background(), fx.opts)
	require.NoError(t, err)
	assert.False(t, needs, "a provisioned agent is loaded, not provisioned again")

	missing := fx.opts
	missing.Name = "missing-agent"
	needs, err = AgentNeedsProvision(context.Background(), missing)
	require.NoError(t, err)
	assert.True(t, needs, "an agent with no state directory is provisioned again")

	staleDir := filepath.Join(fx.projectScionDir, "agents", "stale-agent")
	mkdirAll(t, filepath.Join(staleDir, "home"))
	stale := fx.opts
	stale.Name = "stale-agent"
	needs, err = AgentNeedsProvision(context.Background(), stale)
	require.NoError(t, err)
	assert.True(t, needs, "a state directory without scion-agent.json is provisioned again")
	assert.DirExists(t, staleDir, "AgentNeedsProvision must not change anything")
}
