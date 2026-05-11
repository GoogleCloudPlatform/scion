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

package hub

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAgentMessageTopic(t *testing.T) {
	tests := []struct {
		name      string
		topic     string
		projectID   string
		agentSlug string
		wantErr   bool
	}{
		{
			name:      "valid topic",
			topic:     "scion.project.my-project-123.agent.coder.messages",
			projectID:   "my-project-123",
			agentSlug: "coder",
		},
		{
			name:      "valid topic with uuid project",
			topic:     "scion.project.abc-def-123.agent.code-reviewer.messages",
			projectID:   "abc-def-123",
			agentSlug: "code-reviewer",
		},
		{
			name:    "too few segments",
			topic:   "scion.project.g1.agent.coder",
			wantErr: true,
		},
		{
			name:    "too many segments",
			topic:   "scion.project.g1.agent.coder.messages.extra",
			wantErr: true,
		},
		{
			name:    "wrong prefix",
			topic:   "other.project.g1.agent.coder.messages",
			wantErr: true,
		},
		{
			name:    "wrong structure",
			topic:   "scion.topic.g1.agent.coder.messages",
			wantErr: true,
		},
		{
			name:    "broadcast topic not agent",
			topic:   "scion.project.g1.broadcast.all.messages",
			wantErr: true,
		},
		{
			name:    "empty string",
			topic:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projectID, agentSlug, err := parseAgentMessageTopic(tt.topic)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.projectID, projectID)
			assert.Equal(t, tt.agentSlug, agentSlug)
		})
	}
}
