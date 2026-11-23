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

import (
	"context"
	"time"
)

// AgentSuspendScreen is the visible terminal pane captured just before an
// agent's most recent hub-initiated suspend (ptone/scion#8530).
type AgentSuspendScreen struct {
	AgentID    string
	Content    string
	Truncated  bool
	CapturedAt time.Time
}

// AgentSuspendScreenStore defines persistence for per-agent saved terminal
// screens. At most one row exists per agent; rows are replaced on each suspend
// and deleted with the agent via ON DELETE CASCADE.
type AgentSuspendScreenStore interface {
	// PutAgentSuspendScreen upserts the agent's saved screen row
	// (ON CONFLICT(agent_id) DO UPDATE). Returns ErrNotFound if the agent does
	// not exist, or ErrInvalidInput if the screen or agent ID is invalid.
	PutAgentSuspendScreen(ctx context.Context, screen *AgentSuspendScreen) error

	// GetAgentSuspendScreen returns the agent's saved screen, or ErrNotFound
	// when no row exists.
	GetAgentSuspendScreen(ctx context.Context, agentID string) (*AgentSuspendScreen, error)

	// DeleteAgentSuspendScreen removes the agent's saved screen. It is a no-op
	// when no row exists.
	DeleteAgentSuspendScreen(ctx context.Context, agentID string) error
}
