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

package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentAllowlist_SetMessageMode(t *testing.T) {
	require.True(t, agentAllowed["set-message-mode"],
		"set-message-mode must be in agent allowlist")
}

func TestAgentAllowlist_ConversationCommands(t *testing.T) {
	// Verify conversation commands are in agent allowlist
	commands := []string{
		"conversation",
		"conversation.list",
		"conversation.messages",
		"conversation.get",
		"conversation.get-message",
		"conversation.create",
		"conversation.set-default",
		"conversation.participants",
		"conversation.join",
		"conversation.leave",
		"conversation.catch-up",
	}
	for _, cmd := range commands {
		require.True(t, agentAllowed[cmd], "%q must be in agent allowlist", cmd)
	}
}
