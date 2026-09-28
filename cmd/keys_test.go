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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeysCmd_RequiresExactArgs(t *testing.T) {
	// No args — should fail
	err := keysCmd.Args(keysCmd, []string{})
	require.Error(t, err)

	// One arg — should fail
	err = keysCmd.Args(keysCmd, []string{"agent1"})
	require.Error(t, err)

	// Two args — should pass
	err = keysCmd.Args(keysCmd, []string{"agent1", "Escape"})
	require.NoError(t, err)

	// Three args — should fail (ExactArgs(2))
	err = keysCmd.Args(keysCmd, []string{"agent1", "Escape", "extra"})
	require.Error(t, err)
}

func TestKeysCmd_HasCorrectUse(t *testing.T) {
	assert.Equal(t, "keys <agent-name> <keystrokes>", keysCmd.Use)
}

func TestKeysCmd_IsRegistered(t *testing.T) {
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "keys" {
			found = true
			break
		}
	}
	assert.True(t, found, "keys command should be registered on rootCmd")
}

// ---------------------------------------------------------------------------
// Hub-aware keys: Raw=true reaches the hub as a structured message, for both
// agent and user (human) senders. sendKeysViaHub reuses the same
// StructuredMessage + SendStructuredMessage path `scion message --raw` uses,
// so these tests mirror TestSendMessageViaHub_SingleAgent in message_test.go.
// ---------------------------------------------------------------------------

func TestSendKeysViaHub_AgentSender_RawReachesHub(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	t.Setenv("SCION_AGENT_NAME", "sender-agent")

	projectID := "project-keys-agent"
	server, sent := newMessageMockHubServer(t, projectID, nil)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	err = sendKeysViaHub(hubCtx, "target-agent", "Escape")
	require.NoError(t, err)

	require.Len(t, *sent, 1)
	assert.Equal(t, "target-agent", (*sent)[0].AgentName)
	require.NotNil(t, (*sent)[0].StructuredMsg)
	assert.True(t, (*sent)[0].StructuredMsg.Raw, "keys must set Raw=true on the structured message")
	assert.False(t, (*sent)[0].StructuredMsg.Plain)
	assert.Equal(t, "agent:sender-agent", (*sent)[0].StructuredMsg.Sender)
	assert.Equal(t, "Escape", (*sent)[0].StructuredMsg.Msg)
}

func TestSendKeysViaHub_UserSender_RawReachesHub(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	// Explicitly clear SCION_AGENT_NAME so resolveSenderIdentity takes the
	// human/user path — the test process itself may be running inside an
	// agent container where the variable is already set in the ambient
	// environment.
	t.Setenv("SCION_AGENT_NAME", "")

	projectID := "project-keys-user"
	server, sent := newMessageMockHubServer(t, projectID, nil)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	err = sendKeysViaHub(hubCtx, "target-agent", "C-c")
	require.NoError(t, err)

	require.Len(t, *sent, 1)
	assert.Equal(t, "target-agent", (*sent)[0].AgentName)
	require.NotNil(t, (*sent)[0].StructuredMsg)
	assert.True(t, (*sent)[0].StructuredMsg.Raw, "keys must set Raw=true on the structured message")
	assert.False(t, strings.HasPrefix((*sent)[0].StructuredMsg.Sender, "agent:"),
		"a human/user sender's structured message must not carry an agent: sender identity")
	assert.Equal(t, "C-c", (*sent)[0].StructuredMsg.Msg)
}
