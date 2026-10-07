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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#3667: when the hub cannot read a staged attachment it still
// sends the message, and reports the dropped file in attachment_warnings. The
// CLI surfaces that to the user instead of reporting a clean send.

const attachWarnPath = "/scion-volumes/scratchpad/.attachments/sender/msg1/shot.png"

var attachWarnBody = []map[string]string{{
	"path":   attachWarnPath,
	"reason": "file not found on the hub host",
}}

// newAttachWarnHub serves the agent-message and outbound-message endpoints of
// projectID, answering every send as delivered with one attachment warning.
// It counts the sends it received.
func newAttachWarnHub(t *testing.T, projectID string) (*httptest.Server, *int32) {
	t.Helper()
	var sends int32
	prefix := "/api/v1/projects/" + projectID + "/agents/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, prefix) && strings.HasSuffix(r.URL.Path, "/outbound-message"):
			atomic.AddInt32(&sends, 1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id":          "msg-out",
				"status":              "sent",
				"recipient":           "user:someone@example.com",
				"attachment_warnings": attachWarnBody,
			})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, prefix) && strings.HasSuffix(r.URL.Path, "/message"):
			atomic.AddInt32(&sends, 1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message_id":          "msg-agent",
				"status":              "dispatched",
				"attachment_warnings": attachWarnBody,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &sends
}

func attachWarnHubCtx(t *testing.T, srv *httptest.Server, projectID string) *HubContext {
	t.Helper()
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}
}

// setAttachWarnState puts the package-level message flags in a known state
// for one test, with msgAttach naming an already-staged path.
func setAttachWarnState(t *testing.T, format string) {
	t.Helper()
	orig := saveMessageTestState()
	origFormat, origAttach, origCC := outputFormat, msgAttach, msgCC
	t.Cleanup(func() {
		orig.restore()
		outputFormat, msgAttach, msgCC = origFormat, origAttach, origCC
	})
	outputFormat = format
	msgAttach = []string{attachWarnPath}
	msgCC = nil
}

func TestSendMessageViaHub_AttachmentWarningPrintedToStderr(t *testing.T) {
	setAttachWarnState(t, "")
	projectID := "proj-attach-warn-agent"
	srv, _ := newAttachWarnHub(t, projectID)
	hubCtx := attachWarnHubCtx(t, srv, projectID)

	var sendErr error
	stdout, stderr := captureStdoutStderr(t, func() {
		sendErr = sendMessageViaHub(hubCtx, "builder", "see attached", false, false, false)
	})
	require.NoError(t, sendErr, "a warning must not turn the send into an error (exit code unchanged)")
	assert.Contains(t, stdout, "Message delivered to agent 'builder'.")
	assert.Contains(t, stderr, "Warning: attachment "+attachWarnPath+" was not delivered: file not found on the hub host")
}

func TestSendMessageViaHub_AttachmentWarningInJSON(t *testing.T) {
	setAttachWarnState(t, "json")
	projectID := "proj-attach-warn-agent-json"
	srv, _ := newAttachWarnHub(t, projectID)
	hubCtx := attachWarnHubCtx(t, srv, projectID)

	var sendErr error
	stdout, stderr := captureStdoutStderr(t, func() {
		sendErr = sendMessageViaHub(hubCtx, "builder", "see attached", false, false, false)
	})
	require.NoError(t, sendErr)
	assert.NotContains(t, stderr, "Warning: attachment", "under --json the warning belongs in the JSON object")

	var resp hubclient.MessageResponse
	require.NoError(t, json.Unmarshal([]byte(stdout), &resp), "stdout: %s", stdout)
	require.Len(t, resp.AttachmentWarnings, 1)
	assert.Equal(t, attachWarnPath, resp.AttachmentWarnings[0].Path)
	assert.Equal(t, "file not found on the hub host", resp.AttachmentWarnings[0].Reason)
}

func TestSendOutboundMessageViaHub_AttachmentWarningPrintedToStderr(t *testing.T) {
	setAttachWarnState(t, "")
	t.Setenv("SCION_AGENT_NAME", "sender")
	projectID := "proj-attach-warn-user"
	srv, _ := newAttachWarnHub(t, projectID)
	hubCtx := attachWarnHubCtx(t, srv, projectID)

	var sendErr error
	_, stderr := captureStdoutStderr(t, func() {
		sendErr = sendOutboundMessageViaHub(hubCtx, "user:someone@example.com", "see attached", false)
	})
	require.NoError(t, sendErr)
	assert.Contains(t, stderr, "Warning: attachment "+attachWarnPath+" was not delivered")
}

func TestSendMessageViaConversation_ConvRef_AttachmentWarningPrintedToStderr(t *testing.T) {
	setAttachWarnState(t, "")
	t.Setenv("SCION_AGENT_NAME", "sender")
	projectID := "proj-attach-warn-conv"
	srv, _ := newAttachWarnHub(t, projectID)
	hubCtx := attachWarnHubCtx(t, srv, projectID)

	ref := &messaging.Reference{Kind: messaging.RefConversation, Value: "c1", Raw: "conv:c1"}
	var sendErr error
	_, stderr := captureStdoutStderr(t, func() {
		sendErr = sendMessageViaConversation(hubCtx, ref, "see attached", false, false, msgAttach)
	})
	require.NoError(t, sendErr)
	assert.Contains(t, stderr, "Warning: attachment "+attachWarnPath+" was not delivered")
}

// The human (non-agent) @agent path used to discard the response entirely.
func TestSendMessageViaConversation_HumanAgentRef_AttachmentWarning(t *testing.T) {
	t.Setenv("SCION_AGENT_NAME", "")
	projectID := "proj-attach-warn-human"
	ref := &messaging.Reference{Kind: messaging.RefAgent, Value: "builder", Raw: "@builder"}

	t.Run("text", func(t *testing.T) {
		setAttachWarnState(t, "")
		srv, _ := newAttachWarnHub(t, projectID)
		hubCtx := attachWarnHubCtx(t, srv, projectID)
		var sendErr error
		_, stderr := captureStdoutStderr(t, func() {
			sendErr = sendMessageViaConversation(hubCtx, ref, "see attached", false, false, msgAttach)
		})
		require.NoError(t, sendErr)
		assert.Contains(t, stderr, "Warning: attachment "+attachWarnPath+" was not delivered")
	})

	t.Run("json", func(t *testing.T) {
		setAttachWarnState(t, "json")
		srv, _ := newAttachWarnHub(t, projectID)
		hubCtx := attachWarnHubCtx(t, srv, projectID)
		var sendErr error
		stdout, _ := captureStdoutStderr(t, func() {
			sendErr = sendMessageViaConversation(hubCtx, ref, "see attached", false, false, msgAttach)
		})
		require.NoError(t, sendErr)
		var resp hubclient.MessageResponse
		require.NoError(t, json.Unmarshal([]byte(stdout), &resp), "stdout: %s", stdout)
		require.Len(t, resp.AttachmentWarnings, 1)
	})
}

func TestSendGroupMessage_AttachmentWarningReportedOnce(t *testing.T) {
	setAttachWarnState(t, "")
	projectID := "proj-attach-warn-group"
	srv, sends := newAttachWarnHub(t, projectID)
	hubCtx := attachWarnHubCtx(t, srv, projectID)

	recipients := []messages.GroupRecipient{
		{Kind: messages.RecipientAgent, Name: "alpha"},
		{Kind: messages.RecipientAgent, Name: "beta"},
	}
	var sendErr error
	_, stderr := captureStdoutStderr(t, func() {
		sendErr = sendGroupMessageViaHub(hubCtx, recipients, "see attached", false)
	})
	require.NoError(t, sendErr, "every recipient was delivered; the warning does not change the exit code")
	assert.EqualValues(t, 2, atomic.LoadInt32(sends))
	assert.Equal(t, 1, strings.Count(stderr, "Warning: attachment "+attachWarnPath),
		"the same dropped file is reported once, not per recipient: %s", stderr)
}

func TestSendGroupMessage_AttachmentWarningInJSONResults(t *testing.T) {
	setAttachWarnState(t, "json")
	projectID := "proj-attach-warn-group-json"
	srv, _ := newAttachWarnHub(t, projectID)
	hubCtx := attachWarnHubCtx(t, srv, projectID)

	recipients := []messages.GroupRecipient{
		{Kind: messages.RecipientAgent, Name: "alpha"},
		{Kind: messages.RecipientAgent, Name: "beta"},
	}
	var sendErr error
	stdout, stderr := captureStdoutStderr(t, func() {
		sendErr = sendGroupMessageViaHub(hubCtx, recipients, "see attached", false)
	})
	require.NoError(t, sendErr)
	assert.NotContains(t, stderr, "Warning: attachment")

	var summary groupSendResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &summary), "stdout: %s", stdout)
	require.Len(t, summary.Results, 2)
	for _, r := range summary.Results {
		assert.Equal(t, groupStatusDelivered, r.Status)
		require.Len(t, r.AttachmentWarnings, 1, "recipient %s", r.Recipient)
		assert.Equal(t, attachWarnPath, r.AttachmentWarnings[0].Path)
	}
}

func TestPrintAttachmentWarnings_SilentUnderJSON(t *testing.T) {
	origFormat := outputFormat
	outputFormat = "json"
	defer func() { outputFormat = origFormat }()

	out := captureStderr(t, func() {
		printAttachmentWarnings([]hubclient.AttachmentWarning{{Path: "p", Reason: "r"}})
	})
	assert.Empty(t, out)
}
