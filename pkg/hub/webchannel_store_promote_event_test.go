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
)

// TestTitleCase verifies the titleCase helper function.
func TestTitleCase(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"code-reviewer", "Code Reviewer"},
		{"my_agent", "My Agent"},
		{"simple", "Simple"},
		{"multi-word-agent", "Multi Word Agent"},
		{"already Title", "Already Title"},
	}
	for _, tc := range tests {
		got := titleCase(tc.input)
		require.Equal(t, tc.expected, got, "titleCase(%q)", tc.input)
	}
}

// TestDMPromotedEvent verifies the DMPromotedEvent struct serialization.
func TestDMPromotedEvent(t *testing.T) {
	evt := DMPromotedEvent{
		OldConversationKey: "dm:agent:a1:user:u1",
		NewTopic: WebChatTopic{
			ID:        "topic-1",
			ProjectID: "proj-1",
			Name:      "Test Thread",
		},
	}
	require.Equal(t, "dm:agent:a1:user:u1", evt.OldConversationKey)
	require.Equal(t, "topic-1", evt.NewTopic.ID)
}
