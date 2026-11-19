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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsValidMessagePolicyName(t *testing.T) {
	assert.True(t, IsValidMessagePolicyName("no-prose-choices"))
	assert.True(t, IsValidMessagePolicyName("p1"))
	assert.True(t, IsValidMessagePolicyName(strings.Repeat("a", MaxMessagePolicyNameLen)))

	assert.False(t, IsValidMessagePolicyName(""))
	assert.False(t, IsValidMessagePolicyName("Upper-Case"))
	assert.False(t, IsValidMessagePolicyName("has_underscore"))
	assert.False(t, IsValidMessagePolicyName("-leading"))
	assert.False(t, IsValidMessagePolicyName("trailing-"))
	assert.False(t, IsValidMessagePolicyName(strings.Repeat("a", MaxMessagePolicyNameLen+1)))
}

func TestMessagePolicyScopeMatches_AndClone(t *testing.T) {
	pol := &MessagePolicy{
		ID:       "pol-1",
		Name:     "no-prose-choices",
		Enabled:  true,
		Mode:     MessagePolicyModeShadow,
		OnFailed: MessagePolicyOutcomeAdmit,
		OnFailedReasons: map[string]string{
			"budget_exhausted": MessagePolicyOutcomeReject,
		},
		SystemOneRefs: []string{"inst-a"},
		Scope: MessagePolicyScope{
			Surfaces: []string{MessagePolicySurfaceAgentToUser, MessagePolicySurfaceAgentToConversation},
			Sender: MessagePolicySenderSelector{
				AgentIDs: []string{"agent-a"},
			},
			Recipient: MessagePolicyRecipientSelector{
				Kinds: []string{"user", "conversation"},
			},
		},
	}

	assert.True(t, pol.Matches(MessagePolicySurfaceAgentToUser, "agent-a", "user", ""))
	assert.True(t, pol.Matches(MessagePolicySurfaceAgentToConversation, "agent-a", "conversation", ""))
	assert.False(t, pol.Matches(MessagePolicySurfaceAgentDM, "agent-a", "user", ""), "uncovered surface must not match")
	assert.False(t, pol.Matches(MessagePolicySurfaceAgentToUser, "agent-b", "user", ""), "unlisted sender must not match")
	assert.False(t, pol.Matches(MessagePolicySurfaceAgentToUser, "agent-a", "agent", "agent-c"), "unlisted recipient kind must not match")

	cp := pol.Clone()
	require.NotNil(t, cp)
	cp.Scope.Surfaces[0] = "mutated"
	cp.Scope.Sender.AgentIDs[0] = "mutated"
	cp.Scope.Recipient.Kinds[0] = "mutated"
	cp.SystemOneRefs[0] = "mutated"
	cp.OnFailedReasons["budget_exhausted"] = "mutated"

	assert.Equal(t, MessagePolicySurfaceAgentToUser, pol.Scope.Surfaces[0])
	assert.Equal(t, "agent-a", pol.Scope.Sender.AgentIDs[0])
	assert.Equal(t, "user", pol.Scope.Recipient.Kinds[0])
	assert.Equal(t, "inst-a", pol.SystemOneRefs[0])
	assert.Equal(t, MessagePolicyOutcomeReject, pol.OnFailedReasons["budget_exhausted"])

	cp.Enabled = false
	assert.False(t, cp.Matches(MessagePolicySurfaceAgentToUser, "agent-a", "user", ""), "disabled policy must not match")
}
