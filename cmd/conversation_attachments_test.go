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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strSHA256(s string) string {
	return sha256Hex([]byte(s))
}

func TestGetMessageTextAndBodyDisplay(t *testing.T) {
	const attID = "11111111-1111-1111-1111-111111111111"
	msg := &store.Message{
		ID:             "msg-1",
		ConversationID: "conv-1",
		Sender:         "user:alice",
		Recipient:      "agent:bot",
		Msg:            "raw body\nline2",
		Type:           "instruction",
		CreatedAt:      time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC),
		Attachments: []store.MessageAttachment{
			{
				ID:          attID,
				Name:        "notes.txt",
				Size:        2048,
				ContentType: "text/plain",
				SHA256:      strSHA256("x"),
				Ref:         artifacts.FormatAttachmentRef(attID),
			},
		},
	}

	// Text display includes Attachment: line after body.
	var textBuf bytes.Buffer
	require.NoError(t, writeGetMessageText(context.Background(), nil, &textBuf, msg))
	out := textBuf.String()
	assert.Contains(t, out, "Attachment: notes.txt (2.0 KiB, text/plain) scion://attachment/"+attID)

	// --body is byte-identical raw msg.Msg only.
	var bodyBuf bytes.Buffer
	require.NoError(t, writeMessageBody(&bodyBuf, msg))
	assert.Equal(t, "raw body\nline2", bodyBuf.String())
}

func TestSaveMessageAttachments_AllFilterForceAndJSON(t *testing.T) {
	const (
		convID = "conv-1"
		msgID  = "msg-1"
		att1ID = "aaaaaaaa-1111-1111-1111-111111111111"
		att2ID = "bbbbbbbb-2222-2222-2222-222222222222"
	)
	content1 := "hello attachment one"
	content2 := "hello attachment two"

	var downloads []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads = append(downloads, r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/conversations/" + convID + "/messages/" + msgID + "/attachments/" + att1ID:
			_, _ = w.Write([]byte(content1))
		case "/api/v1/conversations/" + convID + "/messages/" + msgID + "/attachments/" + att2ID:
			_, _ = w.Write([]byte(content2))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	msg := &store.Message{
		ID:             msgID,
		ConversationID: convID,
		Attachments: []store.MessageAttachment{
			{ID: att1ID, Name: "one.txt", Size: int64(len(content1)), ContentType: "text/plain", SHA256: strSHA256(content1), Ref: artifacts.FormatAttachmentRef(att1ID)},
			{ID: att2ID, Name: "two.txt", Size: int64(len(content2)), ContentType: "text/plain", SHA256: strSHA256(content2), Ref: artifacts.FormatAttachmentRef(att2ID)},
		},
	}

	dir := t.TempDir()
	var stderr bytes.Buffer

	// 1. Save filtered by name ("two.txt") only.
	anns, err := saveMessageAttachments(context.Background(), client.Conversations(), &stderr, convID, msg, dir, []string{"two.txt"}, false)
	require.NoError(t, err)
	assert.Len(t, downloads, 1)
	assert.Empty(t, anns[0].SavedPath)
	assert.Equal(t, filepath.Join(dir, "two.txt"), anns[1].SavedPath)
	got2, err := os.ReadFile(filepath.Join(dir, "two.txt"))
	require.NoError(t, err)
	assert.Equal(t, content2, string(got2))

	// 2. Save all without --force: one.txt succeeds, two.txt is refused and annotated skipped: "exists".
	anns, err = saveMessageAttachments(context.Background(), client.Conversations(), &stderr, convID, msg, dir, nil, false)
	require.Error(t, err)
	assert.Equal(t, filepath.Join(dir, "one.txt"), anns[0].SavedPath)
	assert.Equal(t, "exists", anns[1].Skipped)

	// Verify JSON output format carries savedPath and skipped.
	var jsonBuf bytes.Buffer
	require.NoError(t, outputMessageWithSaveJSON(&jsonBuf, msg, anns))
	var decoded struct {
		Attachments []struct {
			ID        string `json:"id"`
			SavedPath string `json:"savedPath"`
			Skipped   string `json:"skipped"`
		} `json:"attachments"`
	}
	require.NoError(t, json.Unmarshal(jsonBuf.Bytes(), &decoded))
	require.Len(t, decoded.Attachments, 2)
	assert.Equal(t, filepath.Join(dir, "one.txt"), decoded.Attachments[0].SavedPath)
	assert.Equal(t, "exists", decoded.Attachments[1].Skipped)

	// 3. Save with --force overwrites existing file.
	anns, err = saveMessageAttachments(context.Background(), client.Conversations(), &stderr, convID, msg, dir, []string{att2ID}, true)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "two.txt"), anns[1].SavedPath)
	assert.Empty(t, anns[1].Skipped)

	// 4. Unknown --attachment filter returns an error without downloading.
	prevDownloads := len(downloads)
	_, err = saveMessageAttachments(context.Background(), client.Conversations(), &stderr, convID, msg, dir, []string{"nonexistent.txt"}, false)
	require.Error(t, err)
	assert.Equal(t, prevDownloads, len(downloads))
}

func TestSaveMessageAttachments_MaliciousNamesAndCollisions(t *testing.T) {
	const (
		convID = "conv-sec"
		msgID  = "msg-sec"
	)
	cases := []struct {
		id   string
		name string
		body string
	}{
		{"11111111-0000-0000-0000-000000000001", "../../x", "escape1"},
		{"22222222-0000-0000-0000-000000000002", `C:\x`, "escape2"},
		{"33333333-0000-0000-0000-000000000003", ".bashrc", "dotfile"},
		{"44444444-0000-0000-0000-000000000004", "CON", "winreserved1"},
		{"55555555-0000-0000-0000-000000000005", "con.txt", "winreserved2"},
		{"aaaaaaaa-0000-0000-0000-000000000006", "Report.txt", "first report"},
		{"bbbbbbbb-0000-0000-0000-000000000007", "report.txt", "second report"},
	}

	byID := make(map[string]string, len(cases))
	atts := make([]store.MessageAttachment, 0, len(cases))
	for _, tc := range cases {
		byID[tc.id] = tc.body
		atts = append(atts, store.MessageAttachment{
			ID:          tc.id,
			Name:        tc.name,
			Size:        int64(len(tc.body)),
			ContentType: "text/plain",
			SHA256:      strSHA256(tc.body),
			Ref:         artifacts.FormatAttachmentRef(tc.id),
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		id := parts[len(parts)-1]
		body, ok := byID[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	parent := t.TempDir()
	dir := filepath.Join(parent, "out")
	msg := &store.Message{ID: msgID, ConversationID: convID, Attachments: atts}

	var stderr bytes.Buffer
	anns, err := saveMessageAttachments(context.Background(), client.Conversations(), &stderr, convID, msg, dir, nil, false)
	require.NoError(t, err)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, len(cases), "every attachment including case-fold collision must be saved as its own file")

	for i, tc := range cases {
		saved := anns[i].SavedPath
		require.NotEmpty(t, saved)
		// Must stay directly inside dir and never start with '.'.
		assert.Equal(t, dir, filepath.Dir(saved))
		base := filepath.Base(saved)
		assert.False(t, strings.HasPrefix(base, "."), "saved filename %q must not start with '.'", base)
		assert.NotEqual(t, "CON", strings.ToUpper(strings.TrimSuffix(base, filepath.Ext(base))), "reserved Windows device name must be prefixed")
		got, rerr := os.ReadFile(saved)
		require.NoError(t, rerr)
		assert.Equal(t, tc.body, string(got))
	}

	// Verify case-fold collision got -<first 8 of id> before extension.
	assert.Equal(t, "Report.txt", filepath.Base(anns[5].SavedPath))
	assert.Equal(t, "report-bbbbbbbb.txt", filepath.Base(anns[6].SavedPath))
}

func TestSaveMessageAttachments_DigestAndSizeMismatchLeaveNoPartialFile(t *testing.T) {
	const (
		convID = "conv-verify"
		msgID  = "msg-verify"
		attID  = "cccccccc-1111-1111-1111-111111111111"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("actual bytes"))
	}))
	defer srv.Close()

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	tests := []struct {
		name string
		att  store.MessageAttachment
	}{
		{
			name: "sha256 mismatch",
			att: store.MessageAttachment{
				ID: attID, Name: "bad-hash.txt", Size: int64(len("actual bytes")),
				ContentType: "text/plain", SHA256: strSHA256("other bytes"),
			},
		},
		{
			name: "size mismatch",
			att: store.MessageAttachment{
				ID: attID, Name: "bad-size.txt", Size: int64(len("actual bytes") + 5),
				ContentType: "text/plain", SHA256: strSHA256("actual bytes"),
			},
		},
		{
			name: "missing digest",
			att: store.MessageAttachment{
				ID: attID, Name: "no-hash.txt", Size: int64(len("actual bytes")),
				ContentType: "text/plain", SHA256: "",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			msg := &store.Message{ID: msgID, ConversationID: convID, Attachments: []store.MessageAttachment{tc.att}}
			var stderr bytes.Buffer
			_, err := saveMessageAttachments(context.Background(), client.Conversations(), &stderr, convID, msg, dir, nil, false)
			require.Error(t, err)
			entries, rerr := os.ReadDir(dir)
			require.NoError(t, rerr)
			assert.Empty(t, entries, "failed verification must leave no partial or temp file")
		})
	}
}

func TestSaveMessageAttachments_404ContinuesRemaining(t *testing.T) {
	const (
		convID    = "conv-404"
		msgID     = "msg-404"
		missingID = "dddddddd-1111-1111-1111-111111111111"
		presentID = "eeeeeeee-2222-2222-2222-222222222222"
	)
	const presentBody = "still here"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/"+missingID) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"Attachment not found"}}`))
			return
		}
		_, _ = w.Write([]byte(presentBody))
	}))
	defer srv.Close()

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	dir := t.TempDir()
	msg := &store.Message{
		ID:             msgID,
		ConversationID: convID,
		Attachments: []store.MessageAttachment{
			{ID: missingID, Name: "gone.txt", Size: 10, ContentType: "text/plain", SHA256: strSHA256("0123456789")},
			{ID: presentID, Name: "ok.txt", Size: int64(len(presentBody)), ContentType: "text/plain", SHA256: strSHA256(presentBody)},
		},
	}

	var stderr bytes.Buffer
	anns, err := saveMessageAttachments(context.Background(), client.Conversations(), &stderr, convID, msg, dir, nil, false)
	require.NoError(t, err)
	assert.Contains(t, stderr.String(), "attachment gone.txt no longer available")
	assert.Empty(t, anns[0].SavedPath)
	assert.Equal(t, filepath.Join(dir, "ok.txt"), anns[1].SavedPath)
	got, err := os.ReadFile(filepath.Join(dir, "ok.txt"))
	require.NoError(t, err)
	assert.Equal(t, presentBody, string(got))
}

func TestConversationMessagesAndCatchUp_AttachmentsDisplay(t *testing.T) {
	orig := saveConversationListTestState()
	defer orig.restore()
	origMsgJSON, origMsgFull := convMsgJSON, convMsgFull
	origCatchUpJSON, origCatchUpFull, origCatchUpSince := convCatchUpJSON, convCatchUpFull, convCatchUpSince
	defer func() {
		convMsgJSON, convMsgFull = origMsgJSON, origMsgFull
		convCatchUpJSON, convCatchUpFull, convCatchUpSince = origCatchUpJSON, origCatchUpFull, origCatchUpSince
	}()

	const (
		convID = "77777777-7777-7777-7777-777777777777"
		att1ID = "11111111-aaaa-1111-1111-111111111111"
		att2ID = "22222222-bbbb-2222-2222-222222222222"
		att3ID = "33333333-cccc-3333-3333-333333333333"
	)

	t1 := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	t3 := t1.Add(2 * time.Minute)

	msgs := []store.Message{
		{
			ID:             "msg-plain",
			ConversationID: convID,
			Sender:         "user:alice",
			Msg:            "no attachments here",
			CreatedAt:      t1,
		},
		{
			ID:             "msg-two-atts",
			ConversationID: convID,
			Sender:         "user:alice",
			Msg:            "two files attached",
			CreatedAt:      t2,
			Attachments: []store.MessageAttachment{
				{
					ID:          att1ID,
					Name:        "notes.txt",
					Size:        2048,
					ContentType: "text/plain",
					SHA256:      strSHA256("notes"),
					Ref:         artifacts.FormatAttachmentRef(att1ID),
				},
				{
					ID:          att2ID,
					Name:        "chart.png",
					Size:        512,
					ContentType: "image/png",
					SHA256:      strSHA256("png"),
					Ref:         artifacts.FormatAttachmentRef(att2ID),
				},
			},
		},
		{
			ID:             "msg-deferred",
			ConversationID: convID,
			Sender:         "user:bob",
			Msg:            "sent while away",
			DispatchState:  store.MessageDispatchDeferred,
			CreatedAt:      t3,
			Attachments: []store.MessageAttachment{
				{
					ID:          att3ID,
					Name:        "My Report.pdf",
					Size:        1024,
					ContentType: "application/pdf",
					SHA256:      strSHA256("pdf"),
					Ref:         artifacts.FormatAttachmentRef(att3ID),
				},
			},
			DeferredAttachments: []store.DeferredAttachment{
				{Name: "My_Report.pdf", Size: 1024},
			},
		},
	}

	var downloadHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/attachments/") {
			downloadHits++
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/api/v1/conversations/"+convID+"/messages" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":      msgs,
				"totalCount": len(msgs),
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	isolateHubEnvForTest(t, srv.URL, "proj-9579")
	t.Setenv("SCION_AUTH_TOKEN", "test-agent-token")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	projectPath = setupProjectWithoutHubEnabled(t, tmpHome)

	newCmd := func() *cobra.Command {
		c := &cobra.Command{}
		c.SetContext(context.Background())
		return c
	}

	ts1 := clitime.Format(t1, clitime.Clock)
	ts2 := clitime.Format(t2, clitime.Clock)
	ts3 := clitime.Format(t3, clitime.Clock)

	// 1. messages table mode: ATT column blank for 0, count for >0.
	convMsgJSON, convMsgFull, outputFormat = false, false, ""
	msgTableOut := captureStdout(t, func() {
		require.NoError(t, runConversationMessages(newCmd(), []string{convID}))
	})
	assert.Equal(t, renderTabTable(
		"TIME\tFROM\tATT\tMESSAGE",
		ts1+"\tuser:alice\t\tno attachments here",
		ts2+"\tuser:alice\t2\ttwo files attached",
		ts3+"\tuser:bob\t1\tsent while away",
	), msgTableOut)

	// 2. catch-up table mode: ATT column + deferred "not delivered" line aligned under MESSAGE.
	convCatchUpJSON, convCatchUpFull, convCatchUpSince, outputFormat = false, false, "1h", ""
	catchUpTableOut := captureStdout(t, func() {
		require.NoError(t, runConversationCatchUp(newCmd(), []string{convID}))
	})
	assert.Equal(t, renderTabTable(
		"TIME\tFROM\tATT\tMESSAGE",
		ts1+"\tuser:alice\t\tno attachments here",
		ts2+"\tuser:alice\t2\ttwo files attached",
		ts3+"\tuser:bob\t1\tsent while away",
		"\t\t\tAttachment not delivered: My_Report.pdf (1.0 KiB): the message was deferred. Ask the sender to resend it.",
	), catchUpTableOut)

	// 3. messages --full and catch-up --full: identical per-attachment lines as get-message + deferred notice.
	convMsgJSON, convMsgFull, outputFormat = false, true, ""
	msgFullOut := captureStdout(t, func() {
		require.NoError(t, runConversationMessages(newCmd(), []string{convID}))
	})
	convCatchUpJSON, convCatchUpFull, outputFormat = false, true, ""
	catchUpFullOut := captureStdout(t, func() {
		require.NoError(t, runConversationCatchUp(newCmd(), []string{convID}))
	})
	for name, out := range map[string]string{"messages --full": msgFullOut, "catch-up --full": catchUpFullOut} {
		assert.Contains(t, out, "Attachment: notes.txt (2.0 KiB, text/plain) scion://attachment/"+att1ID, name)
		assert.Contains(t, out, "Attachment: chart.png (512 bytes, image/png) scion://attachment/"+att2ID, name)
		assert.Contains(t, out, "Attachment: My Report.pdf (1.0 KiB, application/pdf) scion://attachment/"+att3ID, name)
		assert.Contains(t, out, "  Attachment not delivered: My_Report.pdf (1.0 KiB): the message was deferred. Ask the sender to resend it.", name)
	}

	// 4. messages --json and catch-up --json: attachments[] and deferredAttachments preserved, zero downloads.
	convMsgJSON, convMsgFull, outputFormat = true, false, ""
	msgJSONOut := captureStdout(t, func() {
		require.NoError(t, runConversationMessages(newCmd(), []string{convID}))
	})
	convCatchUpJSON, convCatchUpFull, outputFormat = true, false, ""
	catchUpJSONOut := captureStdout(t, func() {
		require.NoError(t, runConversationCatchUp(newCmd(), []string{convID}))
	})
	for name, raw := range map[string]string{"messages --json": msgJSONOut, "catch-up --json": catchUpJSONOut} {
		var decoded struct {
			Items []store.Message `json:"items"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &decoded), name)
		require.Len(t, decoded.Items, 3, name)
		assert.Empty(t, decoded.Items[0].Attachments, name)
		require.Len(t, decoded.Items[1].Attachments, 2, name)
		assert.Equal(t, att1ID, decoded.Items[1].Attachments[0].ID, name)
		assert.Equal(t, artifacts.FormatAttachmentRef(att1ID), decoded.Items[1].Attachments[0].Ref, name)
		require.Len(t, decoded.Items[2].Attachments, 1, name)
		assert.Equal(t, att3ID, decoded.Items[2].Attachments[0].ID, name)
		require.Len(t, decoded.Items[2].DeferredAttachments, 1, name)
		assert.Equal(t, "My_Report.pdf", decoded.Items[2].DeferredAttachments[0].Name, name)
	}
	assert.Zero(t, downloadHits, "--json and text list modes must never download attachment bytes")
}
