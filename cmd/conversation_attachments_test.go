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
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
