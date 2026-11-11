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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMessagesAndConversationJSON_NoLegacyGroveKey covers the CLI, which
// prints store.Message verbatim (via outputJSON) in `scion messages
// --json`, `scion conversation messages --json`, `scion conversation
// get-message --json` and `scion conversation catch-up --json`.
// store.Message JSON-encodes with only its canonical "projectId" key, so
// these four command surfaces must emit only "projectId" and never
// "groveId" — this is a golden, wire-level check of that, using a fake Hub
// server exactly as the existing httptest-hub cmd tests do (mirrors
// newConversationListServer / setupConversationCreateProject in
// conversation_test.go).
//
// Each subtest also asserts the canonical projectId value round-trips
// correctly, so a naive fix that dropped the field entirely (rather than
// keeping it canonical) would also fail.
func TestMessagesAndConversationJSON_NoLegacyGroveKey(t *testing.T) {
	const projectID = "proj-json-test"
	const conversationID = "2bf9904b-7f3d-4f25-94e9-9b6a25e93783"
	wantMsg := store.Message{
		ID:        "msg-1",
		ProjectID: projectID,
		Sender:    "user:alice",
		SenderID:  "alice",
		Recipient: "agent:bob",
		Msg:       "hello",
		Type:      "instruction",
		CreatedAt: time.Now().UTC(),
	}

	t.Run("scion messages --json", func(t *testing.T) {
		origProjectPath := projectPath
		origJSON := messagesJSON
		origShowAll := messagesShowAll
		origAgent := messagesAgent
		origFormat := outputFormat
		t.Cleanup(func() {
			projectPath = origProjectPath
			messagesJSON = origJSON
			messagesShowAll = origShowAll
			messagesAgent = origAgent
			outputFormat = origFormat
		})

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/messages" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(store.ListResult[store.Message]{
				Items: []store.Message{wantMsg},
			})
		}))
		defer server.Close()

		isolateHubEnvForTest(t, server.URL, "")
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		projectDir := setupConversationCreateProject(t, tmpHome, server.URL, "")
		projectPath = projectDir
		messagesJSON = true
		messagesShowAll = true
		messagesAgent = ""

		cmd := &cobra.Command{}
		out := captureStdout(t, func() {
			require.NoError(t, runMessagesList(cmd, nil))
		})

		assert.Contains(t, out, `"projectId": "`+projectID+`"`)
		assert.NotContains(t, out, "groveId")
	})

	t.Run("scion conversation messages --json", func(t *testing.T) {
		origProjectPath := projectPath
		origJSON := convMsgJSON
		origFormat := outputFormat
		t.Cleanup(func() {
			projectPath = origProjectPath
			convMsgJSON = origJSON
			outputFormat = origFormat
		})

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/conversations/"+conversationID+"/messages" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(store.ListResult[store.Message]{
				Items: []store.Message{wantMsg},
			})
		}))
		defer server.Close()

		isolateHubEnvForTest(t, server.URL, "")
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		projectDir := setupConversationCreateProject(t, tmpHome, server.URL, "")
		projectPath = projectDir
		convMsgJSON = true

		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		out := captureStdout(t, func() {
			require.NoError(t, runConversationMessages(cmd, []string{"conv:" + conversationID}))
		})

		assert.Contains(t, out, `"projectId": "`+projectID+`"`)
		assert.NotContains(t, out, "groveId")
	})

	t.Run("scion conversation get-message --json", func(t *testing.T) {
		origProjectPath := projectPath
		origJSON := convGetMessageJSON
		origFormat := outputFormat
		t.Cleanup(func() {
			projectPath = origProjectPath
			convGetMessageJSON = origJSON
			outputFormat = origFormat
		})

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/conversations/"+conversationID+"/messages/"+wantMsg.ID {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(wantMsg)
		}))
		defer server.Close()

		isolateHubEnvForTest(t, server.URL, "")
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		projectDir := setupConversationCreateProject(t, tmpHome, server.URL, "")
		projectPath = projectDir
		convGetMessageJSON = true

		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		out := captureStdout(t, func() {
			require.NoError(t, runConversationGetMessage(cmd, []string{"conv:" + conversationID, wantMsg.ID}))
		})

		assert.Contains(t, out, `"projectId": "`+projectID+`"`)
		assert.NotContains(t, out, "groveId")
	})

	t.Run("scion conversation catch-up --json", func(t *testing.T) {
		origProjectPath := projectPath
		origJSON := convCatchUpJSON
		origSince := convCatchUpSince
		origFormat := outputFormat
		t.Cleanup(func() {
			projectPath = origProjectPath
			convCatchUpJSON = origJSON
			convCatchUpSince = origSince
			outputFormat = origFormat
		})

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/conversations/"+conversationID+"/messages" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(store.ListResult[store.Message]{
				Items: []store.Message{wantMsg},
			})
		}))
		defer server.Close()

		isolateHubEnvForTest(t, server.URL, "")
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		projectDir := setupConversationCreateProject(t, tmpHome, server.URL, "")
		projectPath = projectDir
		convCatchUpJSON = true
		convCatchUpSince = "1h"

		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		out := captureStdout(t, func() {
			require.NoError(t, runConversationCatchUp(cmd, []string{"conv:" + conversationID}))
		})

		assert.Contains(t, out, `"projectId": "`+projectID+`"`)
		assert.NotContains(t, out, "groveId")
	})
}
