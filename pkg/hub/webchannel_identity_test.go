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

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// --- identityFromTopic tests ---

func TestIdentityFromTopic_UserTopic(t *testing.T) {
	msg := &messages.StructuredMessage{
		Sender:   "agent:coder",
		SenderID: "agent-uuid",
	}
	userID, projectID, agentID, ok := identityFromTopic("scion.project.proj1.user.user1.messages", msg)
	require.True(t, ok)
	require.Equal(t, "user1", userID)
	require.Equal(t, "proj1", projectID)
	require.Equal(t, "agent-uuid", agentID)
}

func TestIdentityFromTopic_UserTopic_SenderIDFallback(t *testing.T) {
	// When SenderID is empty, fall back to stripping "agent:" prefix from Sender.
	msg := &messages.StructuredMessage{
		Sender: "agent:coder",
	}
	userID, projectID, agentID, ok := identityFromTopic("scion.project.proj1.user.user1.messages", msg)
	require.True(t, ok)
	require.Equal(t, "user1", userID)
	require.Equal(t, "proj1", projectID)
	require.Equal(t, "coder", agentID)
}

func TestIdentityFromTopic_AgentTopic(t *testing.T) {
	msg := &messages.StructuredMessage{
		Sender:   "user:alice",
		SenderID: "alice-uuid",
	}
	userID, projectID, agentID, ok := identityFromTopic("scion.project.proj1.agent.coder.messages", msg)
	require.True(t, ok)
	require.Equal(t, "alice-uuid", userID)
	require.Equal(t, "proj1", projectID)
	require.Equal(t, "coder", agentID)
}

func TestIdentityFromTopic_Broadcast_ReturnsFalse(t *testing.T) {
	msg := &messages.StructuredMessage{
		Sender: "system",
	}
	_, _, _, ok := identityFromTopic("scion.project.proj1.broadcast", msg)
	require.False(t, ok)
}

func TestIdentityFromTopic_MalformedTopic_ReturnsFalse(t *testing.T) {
	msg := &messages.StructuredMessage{
		Sender: "agent:coder",
	}
	_, _, _, ok := identityFromTopic("not.a.valid.topic", msg)
	require.False(t, ok)
}

func TestIdentityFromTopic_MissingUserID_ReturnsFalse(t *testing.T) {
	// Agent topic, but sender has no SenderID and is not a user.
	msg := &messages.StructuredMessage{
		Sender: "agent:coder",
	}
	_, _, _, ok := identityFromTopic("scion.project.proj1.agent.coder.messages", msg)
	require.False(t, ok)
}
