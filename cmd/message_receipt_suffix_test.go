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
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReceiptSuffix(t *testing.T) {
	assert.Equal(t, " (conversation c1, message m1)", receiptSuffix("m1", "c1"))
	assert.Equal(t, " (message m1)", receiptSuffix("m1", ""))
	assert.Equal(t, " (conversation c1)", receiptSuffix("", "c1"))
	assert.Equal(t, "", receiptSuffix("", ""))
	assert.Equal(t,
		"agent x is reincarnating; message saved to history and will be seen on catch-up (conversation c1, message m1).",
		captureLastLine(t, func() {
			printAgentSendConfirmation("x", "'x'", &hubclient.MessageResponse{MessageID: "m1", ConversationID: "c1", Status: "deferred"})
		}))
}

func TestGroupRecipientResult_ConversationIDInJSON(t *testing.T) {
	b, err := json.Marshal(groupRecipientResult{Recipient: "agent:a", Status: groupStatusDelivered, MessageID: "m1", ConversationID: "c1"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"recipient":"agent:a","status":"delivered","message_id":"m1","conversation_id":"c1"}`, string(b))
}

func captureLastLine(t *testing.T, fn func()) string {
	t.Helper()
	return lastLine(captureStdout(t, fn))
}

// A group send's JSON results carry each recipient's conversation.
func TestSendReceipt_GroupJSONCarriesConversationID(t *testing.T) {
	msgID3881State(t, "sender-agent")
	outputFormat = "json" // restored by msgID3881State's cleanup
	hubCtx := newReceiptConvHub(t)
	recips := []messages.GroupRecipient{
		{Kind: messages.RecipientAgent, Name: "agent-a"},
		{Kind: messages.RecipientUser, Name: "alice"},
	}
	var sendErr error
	out := captureStdout(t, func() { sendErr = sendGroupMessageViaHub(hubCtx, recips, "hi", false) })
	require.NoError(t, sendErr)
	var got groupSendResult
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Len(t, got.Results, 2)
	byRecipient := map[string]string{}
	for _, r := range got.Results {
		byRecipient[r.Recipient] = r.ConversationID
	}
	assert.Equal(t, "conv-agent-a", byRecipient["agent:agent-a"])
	assert.Equal(t, "conv-out", byRecipient["user:alice"])
}
