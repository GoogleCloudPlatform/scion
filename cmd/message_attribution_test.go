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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Messages sent with an agent delegated credential carry the issuer and
// the grant (.design/agent-delegation.md §14.5). The CLI shows the agent
// as the sender and the issuer as "authorized by"; every other message
// renders exactly as before.

const attributionConvID = "33333333-3333-3333-3333-333333333333"

// attributionCreatedAt is recent, so catch-up's --since window covers it.
var attributionCreatedAt = time.Now().UTC().Add(-time.Minute).Truncate(time.Second)

func ordinaryAttributionMessage() store.Message {
	return store.Message{
		ID: "msg-ordinary-0001", Sender: "user:carol", SenderID: "u-carol",
		Recipient: "agent:reviewer", RecipientID: "agent-reviewer-id",
		Msg: "please review", Type: "instruction", AgentID: "agent-reviewer-id",
		ConversationID: attributionConvID, CreatedAt: attributionCreatedAt,
	}
}

func delegatedAttributionMessage(id, issuer, grant string) store.Message {
	return store.Message{
		ID: id, Sender: "agent:builder", SenderID: "agent-builder-id",
		Recipient: "agent:reviewer", RecipientID: "agent-reviewer-id",
		Msg: "build is green", Type: "instruction", AgentID: "agent-builder-id",
		ConversationID: attributionConvID, CreatedAt: attributionCreatedAt,
		AuthorizingUserID: issuer, DelegationGrantID: grant,
	}
}

// attributionHub is a stand-in hub serving one conversation's messages,
// the inbox and user lookups. It counts user lookups by escaped path.
type attributionHub struct {
	server *httptest.Server

	mu        sync.Mutex
	userPaths []string
}

func (h *attributionHub) userLookups() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.userPaths...)
}

func newAttributionHub(t *testing.T, msgs []store.Message) *attributionHub {
	t.Helper()
	h := &attributionHub{}
	users := map[string]map[string]string{
		"u-alice": {"id": "u-alice", "displayName": "Alice Example", "email": "alice@example.com"},
		"u-bob":   {"id": "u-bob", "email": "bob@example.com"},
	}
	writeJSONBody := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	convMessages := "/api/v1/conversations/" + attributionConvID + "/messages"
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.EscapedPath(), "/api/v1/users/"):
			h.mu.Lock()
			h.userPaths = append(h.userPaths, r.URL.EscapedPath())
			h.mu.Unlock()
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/users/")
			if u, ok := users[id]; ok {
				writeJSONBody(w, http.StatusOK, u)
				return
			}
			switch id {
			case "u-denied":
				writeJSONBody(w, http.StatusForbidden, map[string]any{
					"error": map[string]any{"code": "forbidden", "message": "forbidden"},
				})
				return
			case "u-broken":
				writeJSONBody(w, http.StatusInternalServerError, map[string]any{
					"error": map[string]any{"code": "internal_error", "message": "internal error"},
				})
				return
			}
			writeJSONBody(w, http.StatusNotFound, map[string]any{
				"error": map[string]any{"code": "not_found", "message": "user not found"},
			})
		case r.URL.Path == convMessages, r.URL.Path == "/api/v1/messages":
			writeJSONBody(w, http.StatusOK, map[string]any{"items": msgs})
		case strings.HasPrefix(r.URL.Path, convMessages+"/"):
			id := strings.TrimPrefix(r.URL.Path, convMessages+"/")
			for _, m := range msgs {
				if m.ID == id {
					writeJSONBody(w, http.StatusOK, m)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(h.server.Close)
	return h
}

// useAttributionHub points the CLI at hub and snapshots the package-level
// flag state the message commands read.
func useAttributionHub(t *testing.T, h *attributionHub) {
	t.Helper()
	origProjectPath, origOutputFormat := projectPath, outputFormat
	origMsgJSON, origMsgLimit, origMsgBefore, origMsgAfter := convMsgJSON, convMsgLimit, convMsgBefore, convMsgAfter
	origGetJSON, origGetBody := convGetMessageJSON, convGetMessageBody
	origCatchUpJSON, origCatchUpSince := convCatchUpJSON, convCatchUpSince
	origInboxJSON, origInboxAll, origInboxAgent := messagesJSON, messagesShowAll, messagesAgent
	t.Cleanup(func() {
		projectPath, outputFormat = origProjectPath, origOutputFormat
		convMsgJSON, convMsgLimit, convMsgBefore, convMsgAfter = origMsgJSON, origMsgLimit, origMsgBefore, origMsgAfter
		convGetMessageJSON, convGetMessageBody = origGetJSON, origGetBody
		convCatchUpJSON, convCatchUpSince = origCatchUpJSON, origCatchUpSince
		messagesJSON, messagesShowAll, messagesAgent = origInboxJSON, origInboxAll, origInboxAgent
	})

	isolateHubEnvForTest(t, h.server.URL, "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	projectPath = setupConversationCreateProject(t, tmpHome, h.server.URL, "")
	outputFormat = ""
	convMsgJSON, convMsgLimit, convMsgBefore, convMsgAfter = false, 0, "", ""
	convGetMessageJSON, convGetMessageBody = false, false
	convCatchUpJSON, convCatchUpSince = false, "1h"
	messagesJSON, messagesShowAll, messagesAgent = false, true, ""
}

func newAttributionCmd() *cobra.Command {
	c := &cobra.Command{}
	c.SetContext(context.Background())
	return c
}

// renderTabTable lays rows out as the conversation tables do.
func renderTabTable(rows ...string) string {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	for _, row := range rows {
		_, _ = fmt.Fprintln(tw, row)
	}
	_ = tw.Flush()
	return buf.String()
}

func TestMessageAuthorizedBy(t *testing.T) {
	labels := func(id string) string { return map[string]string{"u-alice": "Alice Example"}[id] }
	ordinary := ordinaryAttributionMessage()
	delegated := delegatedAttributionMessage("m1", "u-alice", "grant-1")
	noGrant := delegatedAttributionMessage("m2", "u-alice", "")

	assert.Equal(t, "", messageAuthorizedBy(&ordinary, labels), "an ordinary message has no attribution line")
	assert.Equal(t, "", messageAuthorizedBy(nil, labels))
	assert.Equal(t, "authorized by Alice Example via delegation grant-1", messageAuthorizedBy(&delegated, labels))
	assert.Equal(t, "authorized by u-alice via delegation grant-1", messageAuthorizedBy(&delegated, nil),
		"with no labeler the issuer is shown by ID")
	assert.Equal(t, "authorized by Alice Example", messageAuthorizedBy(&noGrant, labels))
}

func TestNewUserLabeler_DisplayNameEmailThenID(t *testing.T) {
	h := newAttributionHub(t, nil)
	client, err := hubclient.New(h.server.URL)
	require.NoError(t, err)
	label := newUserLabeler(context.Background(), client)

	assert.Equal(t, "Alice Example", label("u-alice"), "display name first")
	assert.Equal(t, "bob@example.com", label("u-bob"), "email when there is no display name")
	assert.Equal(t, "u-gone", label("u-gone"), "the ID when the user is not found")
	assert.Equal(t, "u-denied", label("u-denied"), "the ID when the lookup is forbidden")
	assert.Equal(t, "u-broken", label("u-broken"), "the ID when the lookup fails")
	assert.Equal(t, "Alice Example", label("u-alice"))
	assert.Equal(t, "u-gone", label("u-gone"))
	assert.Equal(t, "u-denied", label("u-denied"))
	assert.Equal(t, "u-broken", label("u-broken"))
	assert.Equal(t, []string{
		"/api/v1/users/u-alice", "/api/v1/users/u-bob", "/api/v1/users/u-gone",
		"/api/v1/users/u-denied", "/api/v1/users/u-broken",
	}, h.userLookups(), "each user is looked up once, failed lookups included")

	// The ID is one escaped path segment.
	assert.Equal(t, "a/b?c", label("a/b?c"))
	assert.Equal(t, "/api/v1/users/a%2Fb%3Fc", h.userLookups()[5])
}

func TestConversationMessages_DelegatedShowsAuthorizedBy(t *testing.T) {
	ordinary := ordinaryAttributionMessage()
	h := newAttributionHub(t, []store.Message{
		ordinary,
		delegatedAttributionMessage("msg-delegated-001", "u-alice", "grant-1"),
		delegatedAttributionMessage("msg-delegated-002", "u-alice", "grant-2"),
		delegatedAttributionMessage("msg-delegated-003", "u-gone", "grant-3"),
	})
	useAttributionHub(t, h)

	out := captureStdout(t, func() {
		require.NoError(t, runConversationMessages(newAttributionCmd(), []string{"conv:" + attributionConvID}))
	})
	ts := clitime.Format(attributionCreatedAt, clitime.Clock)
	assert.Equal(t, renderTabTable(
		"TIME\tFROM\tMESSAGE",
		ts+"\tuser:carol\tplease review",
		ts+"\tagent:builder\tbuild is green",
		"\t\tauthorized by Alice Example via delegation grant-1",
		ts+"\tagent:builder\tbuild is green",
		"\t\tauthorized by Alice Example via delegation grant-2",
		ts+"\tagent:builder\tbuild is green",
		"\t\tauthorized by u-gone via delegation grant-3",
	), out)
	assert.Equal(t, []string{"/api/v1/users/u-alice", "/api/v1/users/u-gone"}, h.userLookups(),
		"one lookup per issuer")
}

func TestConversationMessages_OrdinaryRendersUnchanged(t *testing.T) {
	h := newAttributionHub(t, []store.Message{ordinaryAttributionMessage()})
	useAttributionHub(t, h)

	out := captureStdout(t, func() {
		require.NoError(t, runConversationMessages(newAttributionCmd(), []string{"conv:" + attributionConvID}))
	})
	ts := clitime.Format(attributionCreatedAt, clitime.Clock)
	assert.Equal(t, renderTabTable("TIME\tFROM\tMESSAGE", ts+"\tuser:carol\tplease review"), out)
	assert.Empty(t, h.userLookups(), "a listing with no delegated message looks up no user")
}

func TestConversationMessages_JSONCarriesAttributionFields(t *testing.T) {
	h := newAttributionHub(t, []store.Message{
		ordinaryAttributionMessage(),
		delegatedAttributionMessage("msg-delegated-001", "u-alice", "grant-1"),
	})
	useAttributionHub(t, h)
	convMsgJSON = true

	out := captureStdout(t, func() {
		require.NoError(t, runConversationMessages(newAttributionCmd(), []string{"conv:" + attributionConvID}))
	})
	var got struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Items, 2)
	assert.NotContains(t, got.Items[0], "authorizingUserId")
	assert.NotContains(t, got.Items[0], "delegationGrantId")
	assert.Equal(t, "user:carol", got.Items[0]["sender"])
	assert.Equal(t, "agent:builder", got.Items[1]["sender"], "the agent stays the sender")
	assert.Equal(t, "agent-builder-id", got.Items[1]["senderId"])
	assert.Equal(t, "u-alice", got.Items[1]["authorizingUserId"])
	assert.Equal(t, "grant-1", got.Items[1]["delegationGrantId"])
	assert.Empty(t, h.userLookups(), "JSON output carries the hub's fields as returned")
}

func TestConversationCatchUp_DelegatedShowsAuthorizedBy(t *testing.T) {
	h := newAttributionHub(t, []store.Message{
		ordinaryAttributionMessage(),
		delegatedAttributionMessage("msg-delegated-001", "u-alice", "grant-1"),
	})
	useAttributionHub(t, h)

	out := captureStdout(t, func() {
		require.NoError(t, runConversationCatchUp(newAttributionCmd(), []string{"conv:" + attributionConvID}))
	})
	ts := clitime.Format(attributionCreatedAt, clitime.Clock)
	assert.Equal(t, renderTabTable(
		"TIME\tFROM\tMESSAGE",
		ts+"\tuser:carol\tplease review",
		ts+"\tagent:builder\tbuild is green",
		"\t\tauthorized by Alice Example via delegation grant-1",
	), out)
}

// getMessageRows parses get-message's two-column output into label/value
// pairs, in order.
func getMessageRows(t *testing.T, out string) [][2]string {
	t.Helper()
	split := regexp.MustCompile(`^(\S+(?: \S+)*)(?:\s{2,}(.*))?$`)
	var rows [][2]string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		m := split.FindStringSubmatch(strings.TrimRight(line, " "))
		require.NotNil(t, m, "row: %q", line)
		rows = append(rows, [2]string{m[1], m[2]})
	}
	return rows
}

func getMessageLabels(rows [][2]string) []string {
	labels := make([]string, 0, len(rows))
	for _, r := range rows {
		labels = append(labels, r[0])
	}
	return labels
}

var ordinaryGetMessageLabels = []string{
	"ID", "CONVERSATION ID", "PROJECT ID", "AGENT ID", "SENDER", "SENDER ID", "SENDER PROJECT ID",
	"RECIPIENT", "RECIPIENT ID", "RECIPIENT PROJECT ID", "MESSAGE", "TYPE", "URGENT", "BROADCASTED",
	"READ", "GROUP ID", "CHANNEL", "THREAD ID", "CREATED", "DISPATCH STATE", "DISPATCHED AT",
	"DISPATCH FAILURE REASON",
}

func TestConversationGetMessage_DelegatedShowsAuthorizedBy(t *testing.T) {
	h := newAttributionHub(t, []store.Message{delegatedAttributionMessage("msg-delegated-001", "u-alice", "grant-1")})
	useAttributionHub(t, h)

	out := captureStdout(t, func() {
		require.NoError(t, runConversationGetMessage(newAttributionCmd(),
			[]string{"conv:" + attributionConvID, "msg-delegated-001"}))
	})
	rows := getMessageRows(t, out)
	want := append([]string{}, ordinaryGetMessageLabels[:7]...)
	want = append(want, "AUTHORIZED BY", "AUTHORIZING USER ID", "DELEGATION GRANT ID")
	want = append(want, ordinaryGetMessageLabels[7:]...)
	require.Equal(t, want, getMessageLabels(rows))
	assert.Equal(t, [2]string{"SENDER", "agent:builder"}, rows[4], "the agent stays the sender")
	assert.Equal(t, [2]string{"AUTHORIZED BY", "Alice Example"}, rows[7])
	assert.Equal(t, [2]string{"AUTHORIZING USER ID", "u-alice"}, rows[8])
	assert.Equal(t, [2]string{"DELEGATION GRANT ID", "grant-1"}, rows[9])
}

func TestConversationGetMessage_OrdinaryRendersUnchanged(t *testing.T) {
	h := newAttributionHub(t, []store.Message{ordinaryAttributionMessage()})
	useAttributionHub(t, h)

	out := captureStdout(t, func() {
		require.NoError(t, runConversationGetMessage(newAttributionCmd(),
			[]string{"conv:" + attributionConvID, "msg-ordinary-0001"}))
	})
	assert.Equal(t, ordinaryGetMessageLabels, getMessageLabels(getMessageRows(t, out)))
	assert.Empty(t, h.userLookups())
}

func TestConversationGetMessage_JSONCarriesAttributionFields(t *testing.T) {
	h := newAttributionHub(t, []store.Message{
		ordinaryAttributionMessage(),
		delegatedAttributionMessage("msg-delegated-001", "u-alice", "grant-1"),
	})
	useAttributionHub(t, h)
	convGetMessageJSON = true

	get := func(id string) map[string]any {
		out := captureStdout(t, func() {
			require.NoError(t, runConversationGetMessage(newAttributionCmd(), []string{"conv:" + attributionConvID, id}))
		})
		var got map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		return got
	}
	delegated := get("msg-delegated-001")
	assert.Equal(t, "agent:builder", delegated["sender"])
	assert.Equal(t, "u-alice", delegated["authorizingUserId"])
	assert.Equal(t, "grant-1", delegated["delegationGrantId"])
	ordinary := get("msg-ordinary-0001")
	assert.NotContains(t, ordinary, "authorizingUserId")
	assert.NotContains(t, ordinary, "delegationGrantId")
	assert.Empty(t, h.userLookups())
}

// inboxRow is one row of the inbox table, as runMessagesList prints it.
func inboxRow(id, agent, msgType, msg, suffix string) string {
	return fmt.Sprintf("%-12s  %-14s  %-14s  %-22s  %s%s\n",
		id, agent, msgType, clitime.Format(attributionCreatedAt, clitime.Minute), msg, suffix)
}

var inboxHeader = fmt.Sprintf("%-12s  %-14s  %-14s  %-22s  %s\n", "ID", "AGENT", "TYPE", "TIME", "MESSAGE") +
	fmt.Sprintf("%-12s  %-14s  %-14s  %-22s  %s\n",
		"------------", "--------------", "--------------", "----------------------", "-------")

func TestMessagesInbox_DelegatedShowsAuthorizedBy(t *testing.T) {
	h := newAttributionHub(t, []store.Message{
		ordinaryAttributionMessage(),
		delegatedAttributionMessage("msg-delegated-001", "u-bob", "grant-1"),
	})
	useAttributionHub(t, h)

	out := captureStdout(t, func() { require.NoError(t, runMessagesList(newAttributionCmd(), nil)) })
	assert.Equal(t, inboxHeader+
		inboxRow("msg-ordinary", "agent-revie...", "instruction", "please review", "")+
		inboxRow("msg-delegate", "agent-build...", "instruction", "build is green",
			" [authorized by bob@example.com via delegation grant-1]"), out)
}

func TestMessagesInbox_OrdinaryRendersUnchanged(t *testing.T) {
	h := newAttributionHub(t, []store.Message{ordinaryAttributionMessage()})
	useAttributionHub(t, h)

	out := captureStdout(t, func() { require.NoError(t, runMessagesList(newAttributionCmd(), nil)) })
	assert.Equal(t, inboxHeader+inboxRow("msg-ordinary", "agent-revie...", "instruction", "please review", ""), out)
	assert.Empty(t, h.userLookups())
}

func TestMessagesInbox_JSONCarriesAttributionFields(t *testing.T) {
	h := newAttributionHub(t, []store.Message{
		ordinaryAttributionMessage(),
		delegatedAttributionMessage("msg-delegated-001", "u-alice", "grant-1"),
	})
	useAttributionHub(t, h)
	messagesJSON = true

	out := captureStdout(t, func() { require.NoError(t, runMessagesList(newAttributionCmd(), nil)) })
	var got struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Items, 2)
	assert.NotContains(t, got.Items[0], "authorizingUserId")
	assert.Equal(t, "agent:builder", got.Items[1]["sender"])
	assert.Equal(t, "u-alice", got.Items[1]["authorizingUserId"])
	assert.Equal(t, "grant-1", got.Items[1]["delegationGrantId"])
	assert.Empty(t, h.userLookups())
}
