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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveConversationRef_AcceptsBareUUID covers ptone/scion#8469 item 1:
// conversation subcommands accept a bare 36-character UUID without requiring
// the `conv:` prefix, since inbound message metadata and `conversation list`
// emit bare UUIDs.
func TestResolveConversationRef_AcceptsBareUUID(t *testing.T) {
	const convID = "69e36cbf-69ac-4782-8ec1-e0604abeb687"
	got, err := resolveConversationRef(context.Background(), nil, convID)
	require.NoError(t, err)
	assert.Equal(t, convID, got)

	// Uppercase UUIDs normalize cleanly too.
	gotUpper, err := resolveConversationRef(context.Background(), nil, strings.ToUpper(convID))
	require.NoError(t, err)
	assert.Equal(t, convID, gotUpper)
}

// TestResolveConversationRef_InvalidAndShortRefsHintConvPrefix covers
// ptone/scion#8469 items 1 & 3 (and shared hint for ptone/scion#8303):
// unrecognized or truncated references suggest `conv:<uuid>` and state that
// conversation IDs must be full 36-character UUIDs.
func TestResolveConversationRef_InvalidAndShortRefsHintConvPrefix(t *testing.T) {
	_, shortErr := resolveConversationRef(context.Background(), nil, "69e36cbf")
	require.Error(t, shortErr)
	assert.Contains(t, shortErr.Error(), "conv:<uuid>")
	assert.Contains(t, shortErr.Error(), "36-character UUID")

	_, shortPrefixedErr := resolveConversationRef(context.Background(), nil, "conv:69e36cbf")
	require.Error(t, shortPrefixedErr)
	assert.Contains(t, shortPrefixedErr.Error(), "36-character UUID")

	_, otherErr := resolveConversationRef(context.Background(), nil, "not_a_valid_ref")
	require.Error(t, otherErr)
	assert.Contains(t, otherErr.Error(), "conv:<uuid>")
}

// TestParseRecipientArg_AcceptsBareUUIDAndRejectsShortConvRef covers
// ptone/scion#8469 items 1 & 3 on `scion message`: a bare 36-char UUID routes
// as `conv:<uuid>`, while a truncated `conv:69e36cbf` fails immediately at
// send time with a 36-character UUID message.
func TestParseRecipientArg_AcceptsBareUUIDAndRejectsShortConvRef(t *testing.T) {
	const convID = "69e36cbf-69ac-4782-8ec1-e0604abeb687"
	parsed, err := parseRecipientArg(convID)
	require.NoError(t, err)
	require.NotNil(t, parsed.convRef, "bare 36-char UUID should parse as a conversation reference")
	assert.Equal(t, messaging.RefConversation, parsed.convRef.Kind)
	assert.Equal(t, convID, parsed.convRef.Value)
	assert.Equal(t, "conv:"+convID, parsed.convRef.Raw)

	_, shortErr := parseRecipientArg("conv:69e36cbf")
	require.Error(t, shortErr)
	assert.Contains(t, shortErr.Error(), "36-character UUID")
}

// TestSendMessage_WarnsOnTruncatedConvRefInBody covers ptone/scion#8469 item 3:
// when an agent sends a message or brief containing a truncated `conv:...`
// reference in the body, `scion message` warns at send time so the sender can
// fix it before the recipient fails to resolve it.
func TestSendMessage_WarnsOnTruncatedConvRefInBody(t *testing.T) {
	orig := saveMessageTestState()
	origFormat := outputFormat
	t.Cleanup(func() {
		orig.restore()
		outputFormat = origFormat
	})
	outputFormat = ""
	t.Setenv("SCION_AGENT_NAME", "")

	projectID := "proj-short-conv-warn"
	server, _ := newMessageMockHubServer(t, projectID, nil)
	defer server.Close()
	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID}

	t.Run("warns_on_short_conv_id", func(t *testing.T) {
		var sendErr error
		_, stderr := captureStdoutStderr(t, func() {
			sendErr = sendMessageViaHub(hubCtx, "worker", "Reply in conv:69e36cbf when done.", false, false, false)
		})
		require.NoError(t, sendErr)
		assert.Contains(t, stderr, "Warning:")
		assert.Contains(t, stderr, "conv:69e36cbf")
		assert.Contains(t, stderr, "36-character UUID")
	})

	t.Run("warns_on_ellipsis_truncated_conv_id", func(t *testing.T) {
		var sendErr error
		_, stderr := captureStdoutStderr(t, func() {
			sendErr = sendMessageViaHub(hubCtx, "worker", "Use `conv:69e36cbf...` for updates", false, false, false)
		})
		require.NoError(t, sendErr)
		assert.Contains(t, stderr, "Warning:")
		assert.Contains(t, stderr, "conv:69e36cbf...")
	})

	t.Run("no_warning_on_full_uuid_or_placeholder", func(t *testing.T) {
		var sendErr error
		_, stderr := captureStdoutStderr(t, func() {
			sendErr = sendMessageViaHub(hubCtx, "worker",
				"Use conv:69e36cbf-69ac-4782-8ec1-e0604abeb687 (syntax: conv:<uuid> or conv:<id>).",
				false, false, false)
		})
		require.NoError(t, sendErr)
		assert.NotContains(t, stderr, "truncated or invalid conversation reference")
	})
}

// TestConversationMessages_ChronologicalOrderAndFullFlag covers
// ptone/scion#8469 item 2 on `scion conversation messages`:
//   - messages returned newest-first by the Hub API are displayed in
//     chronological (oldest-first) order;
//   - `--full` prints complete untruncated message bodies (including
//     multi-line text > 60 runes) and message IDs in chronological order.
func TestConversationMessages_ChronologicalOrderAndFullFlag(t *testing.T) {
	orig := saveConversationListTestState()
	defer orig.restore()
	origMsgJSON := convMsgJSON
	defer func() {
		convMsgJSON = origMsgJSON
		if f := conversationMessagesCmd.Flags().Lookup("full"); f != nil {
			_ = f.Value.Set("false")
		}
	}()

	const convID = "55555555-5555-5555-5555-555555555555"
	olderTime := time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC)
	newerTime := time.Date(2026, 11, 19, 20, 5, 0, 0, time.UTC)
	longOlderBody := "FIRST_MSG_START: This message body is deliberately much longer than sixty characters\nand spans a second line withTAIL_MARKER_OLDER."
	newerBody := "SECOND_MSG_NEWER: short follow-up reply."

	// Hub returns newest-first (DESC), matching pkg/store/entadapter/message_store.go.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/conversations/"+convID+"/messages" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []store.Message{
					{
						ID:             "msg-newer-2222",
						ConversationID: convID,
						Sender:         "agent:reviewer-with-very-long-name",
						Msg:            newerBody,
						CreatedAt:      newerTime,
					},
					{
						ID:             "msg-older-1111",
						ConversationID: convID,
						Sender:         "user:alice@example.com",
						Msg:            longOlderBody,
						CreatedAt:      olderTime,
					},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	isolateHubEnvForTest(t, server.URL, "proj-8469")
	t.Setenv("SCION_AUTH_TOKEN", "test-agent-token")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	projectPath = setupProjectWithoutHubEnabled(t, tmpHome)
	convMsgJSON = false
	outputFormat = ""

	fullFlag := conversationMessagesCmd.Flags().Lookup("full")
	require.NotNil(t, fullFlag, "conversation messages must register --full")

	// Default table mode: chronological order (older before newer), bare UUID accepted.
	_ = fullFlag.Value.Set("false")
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	defaultOut := captureStdout(t, func() {
		require.NoError(t, runConversationMessages(cmd, []string{convID}))
	})
	idxOlder := strings.Index(defaultOut, "FIRST_MSG_START:")
	idxNewer := strings.Index(defaultOut, "SECOND_MSG_NEWER:")
	require.NotEqual(t, -1, idxOlder, "older message missing from output: %q", defaultOut)
	require.NotEqual(t, -1, idxNewer, "newer message missing from output: %q", defaultOut)
	assert.Less(t, idxOlder, idxNewer, "messages must be printed in chronological order (older before newer)")

	// --full mode: chronological order AND full untruncated multi-line body + full sender + message ID.
	require.NoError(t, fullFlag.Value.Set("true"))
	fullOut := captureStdout(t, func() {
		require.NoError(t, runConversationMessages(cmd, []string{convID}))
	})
	assert.Contains(t, fullOut, longOlderBody, "--full must include the complete untruncated multi-line message")
	assert.Contains(t, fullOut, "TAIL_MARKER_OLDER")
	assert.Contains(t, fullOut, "agent:reviewer-with-very-long-name", "--full must not truncate sender")
	assert.Contains(t, fullOut, "msg-older-1111", "--full should include message ID")
	assert.Less(t, strings.Index(fullOut, "FIRST_MSG_START:"), strings.Index(fullOut, "SECOND_MSG_NEWER:"),
		"--full output must be in chronological order")
}

// TestConversationCatchUp_ChronologicalOrderAndFullFlag covers
// ptone/scion#8469 item 2 on `scion conversation catch-up`.
func TestConversationCatchUp_ChronologicalOrderAndFullFlag(t *testing.T) {
	orig := saveConversationListTestState()
	defer orig.restore()
	origCatchUpJSON, origCatchUpSince := convCatchUpJSON, convCatchUpSince
	defer func() {
		convCatchUpJSON, convCatchUpSince = origCatchUpJSON, origCatchUpSince
		if f := conversationCatchUpCmd.Flags().Lookup("full"); f != nil {
			_ = f.Value.Set("false")
		}
	}()

	const convID = "66666666-6666-6666-6666-666666666666"
	olderTime := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	newerTime := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	longOlderBody := "CATCHUP_FIRST: full body exceeding sixty characters without any ellipsis truncation at the end C1_END"
	newerBody := "CATCHUP_SECOND: newer message"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/conversations/"+convID+"/messages" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []store.Message{
					{ID: "c-msg-2", ConversationID: convID, Sender: "agent:two", Msg: newerBody, CreatedAt: newerTime},
					{ID: "c-msg-1", ConversationID: convID, Sender: "agent:one", Msg: longOlderBody, CreatedAt: olderTime},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	isolateHubEnvForTest(t, server.URL, "proj-8469")
	t.Setenv("SCION_AUTH_TOKEN", "test-agent-token")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	projectPath = setupProjectWithoutHubEnabled(t, tmpHome)
	convCatchUpJSON = false
	convCatchUpSince = "1h"
	outputFormat = ""

	fullFlag := conversationCatchUpCmd.Flags().Lookup("full")
	require.NotNil(t, fullFlag, "conversation catch-up must register --full")
	require.NoError(t, fullFlag.Value.Set("true"))

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	fullOut := captureStdout(t, func() {
		require.NoError(t, runConversationCatchUp(cmd, []string{convID}))
	})
	assert.Contains(t, fullOut, longOlderBody)
	assert.Contains(t, fullOut, "C1_END")
	assert.Less(t, strings.Index(fullOut, "CATCHUP_FIRST:"), strings.Index(fullOut, "CATCHUP_SECOND:"),
		"catch-up --full must print messages in chronological order")
}
