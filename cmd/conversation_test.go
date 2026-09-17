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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConversationCommandRegistered(t *testing.T) {
	// Verify conversation command is registered in rootCmd.
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "conversation" {
			found = true
			break
		}
	}
	assert.True(t, found, "conversation command should be registered in rootCmd")
}

func TestConversationAliases(t *testing.T) {
	assert.Contains(t, conversationCmd.Aliases, "conv", "conversation should have 'conv' alias")
}

func TestConversationSubcommands(t *testing.T) {
	subcommands := make(map[string]bool)
	for _, cmd := range conversationCmd.Commands() {
		subcommands[cmd.Name()] = true
	}

	assert.True(t, subcommands["list"], "should have 'list' subcommand")
	assert.True(t, subcommands["messages"], "should have 'messages' subcommand")
	assert.True(t, subcommands["create"], "should have 'create' subcommand")
	assert.True(t, subcommands["get"], "should have 'get' subcommand")
	assert.True(t, subcommands["set-default"], "should have 'set-default' subcommand")
	assert.True(t, subcommands["participants"], "should have 'participants' subcommand")
	assert.True(t, subcommands["join"], "should have 'join' subcommand")
	assert.True(t, subcommands["leave"], "should have 'leave' subcommand")
	assert.True(t, subcommands["catch-up"], "should have 'catch-up' subcommand")
}

func TestConversationListFlags(t *testing.T) {
	flags := conversationListCmd.Flags()

	f := flags.Lookup("kind")
	require.NotNil(t, f, "--kind flag should exist")
	assert.Equal(t, "", f.DefValue)

	f = flags.Lookup("surface")
	require.NotNil(t, f, "--surface flag should exist")
	assert.Equal(t, "", f.DefValue)

	f = flags.Lookup("project")
	require.NotNil(t, f, "--project flag should exist")
	assert.Equal(t, "", f.DefValue)

	f = flags.Lookup("json")
	require.NotNil(t, f, "--json flag should exist")
	assert.Equal(t, "false", f.DefValue)

	f = flags.Lookup("limit")
	require.NotNil(t, f, "--limit flag should exist")
	assert.Equal(t, "50", f.DefValue)
}

func TestConversationMessagesFlags(t *testing.T) {
	flags := conversationMessagesCmd.Flags()

	f := flags.Lookup("limit")
	require.NotNil(t, f, "--limit flag should exist")
	assert.Equal(t, "25", f.DefValue)

	f = flags.Lookup("before")
	require.NotNil(t, f, "--before flag should exist")

	f = flags.Lookup("after")
	require.NotNil(t, f, "--after flag should exist")

	f = flags.Lookup("json")
	require.NotNil(t, f, "--json flag should exist")
}

func TestConversationCreateFlags(t *testing.T) {
	flags := conversationCreateCmd.Flags()

	f := flags.Lookup("project")
	require.NotNil(t, f, "--project flag should exist")

	f = flags.Lookup("json")
	require.NotNil(t, f, "--json flag should exist")
}

func TestConversationGetFlags(t *testing.T) {
	flags := conversationGetCmd.Flags()

	f := flags.Lookup("json")
	require.NotNil(t, f, "--json flag should exist")
}

func TestConversationMessagesRequiresArgs(t *testing.T) {
	err := conversationMessagesCmd.Args(conversationMessagesCmd, []string{})
	assert.Error(t, err, "messages should require a conversation ref argument")
}

func TestConversationCreateRequiresArgs(t *testing.T) {
	err := conversationCreateCmd.Args(conversationCreateCmd, []string{})
	assert.Error(t, err, "create should require a name argument")
}

func TestConversationSetDefaultRequiresArgs(t *testing.T) {
	err := conversationSetDefaultCmd.Args(conversationSetDefaultCmd, []string{})
	assert.Error(t, err, "set-default should require two arguments")

	err = conversationSetDefaultCmd.Args(conversationSetDefaultCmd, []string{"ref"})
	assert.Error(t, err, "set-default should require two arguments")
}

func TestConversationGetRequiresArgs(t *testing.T) {
	err := conversationGetCmd.Args(conversationGetCmd, []string{})
	assert.Error(t, err, "get should require a conversation ref argument")
}

func TestConversationParticipantsRequiresArgs(t *testing.T) {
	err := conversationParticipantsCmd.Args(conversationParticipantsCmd, []string{})
	assert.Error(t, err, "participants should require a conversation ref argument")
}

func TestConversationParticipantsFlags(t *testing.T) {
	flags := conversationParticipantsCmd.Flags()

	f := flags.Lookup("json")
	require.NotNil(t, f, "--json flag should exist")
	assert.Equal(t, "false", f.DefValue)
}

func TestConversationJoinRequiresArgs(t *testing.T) {
	err := conversationJoinCmd.Args(conversationJoinCmd, []string{})
	assert.Error(t, err, "join should require three arguments")

	err = conversationJoinCmd.Args(conversationJoinCmd, []string{"ref"})
	assert.Error(t, err, "join should require three arguments")

	err = conversationJoinCmd.Args(conversationJoinCmd, []string{"ref", "agent"})
	assert.Error(t, err, "join should require three arguments")
}

func TestConversationLeaveRequiresArgs(t *testing.T) {
	err := conversationLeaveCmd.Args(conversationLeaveCmd, []string{})
	assert.Error(t, err, "leave should require a conversation ref argument")
}

func TestConversationCatchUpRequiresArgs(t *testing.T) {
	err := conversationCatchUpCmd.Args(conversationCatchUpCmd, []string{})
	assert.Error(t, err, "catch-up should require a conversation ref argument")
}

func TestConversationCatchUpFlags(t *testing.T) {
	flags := conversationCatchUpCmd.Flags()

	f := flags.Lookup("since")
	require.NotNil(t, f, "--since flag should exist")
	assert.Equal(t, "1h", f.DefValue)

	f = flags.Lookup("json")
	require.NotNil(t, f, "--json flag should exist")
	assert.Equal(t, "false", f.DefValue)
}

func TestFormatTimeAgo(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		expected string
	}{
		{"just now", 30 * time.Second, "just now"},
		{"1 minute", 1 * time.Minute, "1m ago"},
		{"5 minutes", 5 * time.Minute, "5m ago"},
		{"1 hour", 1 * time.Hour, "1h ago"},
		{"3 hours", 3 * time.Hour, "3h ago"},
		{"1 day", 24 * time.Hour, "1d ago"},
		{"7 days", 7 * 24 * time.Hour, "7d ago"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatTimeAgo(time.Now().Add(-tt.duration))
			assert.Equal(t, tt.expected, result)
		})
	}
}
