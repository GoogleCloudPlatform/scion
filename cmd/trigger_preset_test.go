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

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withMockTriggerPresetHub(t *testing.T, handler http.HandlerFunc, credKind hubsync.CredentialKind) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	client, err := hubclient.New(ts.URL)
	require.NoError(t, err)

	origTriggerFn := triggerHubContextFn
	origPresetFn := triggerPresetHubContextFn
	t.Cleanup(func() {
		triggerHubContextFn = origTriggerFn
		triggerPresetHubContextFn = origPresetFn
	})

	hubCtx := &HubContext{
		Client:         client,
		Endpoint:       ts.URL,
		CredentialKind: credKind,
	}
	triggerHubContextFn = func() (*HubContext, string, context.Context, context.CancelFunc, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		return hubCtx, "proj-1", ctx, cancel, nil
	}
	triggerPresetHubContextFn = func() (*HubContext, context.Context, context.CancelFunc, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		return hubCtx, ctx, cancel, nil
	}
}

func resetTriggerPresetFlags(t *testing.T) {
	t.Helper()
	origFormat := outputFormat
	origJSON := triggerOutputJSON
	origListSource := triggerPresetListSource
	origName := triggerPresetApplyName
	origDesc := triggerPresetApplyDescription
	origFields := triggerPresetApplyFields
	origAction := triggerPresetApplyAction
	origAgent := triggerPresetApplyAgent
	origMsg := triggerPresetApplyMessage
	origInterrupt := triggerPresetApplyInterrupt
	origWake := triggerPresetApplyWake
	origPlain := triggerPresetApplyPlain
	origDispatch := triggerPresetApplyDispatch
	origComment := triggerPresetApplyComment
	origDryRun := triggerPresetApplyDryRun
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerOutputJSON = origJSON
		triggerPresetListSource = origListSource
		triggerPresetApplyName = origName
		triggerPresetApplyDescription = origDesc
		triggerPresetApplyFields = origFields
		triggerPresetApplyAction = origAction
		triggerPresetApplyAgent = origAgent
		triggerPresetApplyMessage = origMsg
		triggerPresetApplyInterrupt = origInterrupt
		triggerPresetApplyWake = origWake
		triggerPresetApplyPlain = origPlain
		triggerPresetApplyDispatch = origDispatch
		triggerPresetApplyComment = origComment
		triggerPresetApplyDryRun = origDryRun
	})
	outputFormat = ""
	triggerOutputJSON = false
	triggerPresetListSource = ""
	triggerPresetApplyName = ""
	triggerPresetApplyDescription = ""
	triggerPresetApplyFields = nil
	triggerPresetApplyAction = "message"
	triggerPresetApplyAgent = ""
	triggerPresetApplyMessage = ""
	triggerPresetApplyInterrupt = false
	triggerPresetApplyWake = false
	triggerPresetApplyPlain = false
	triggerPresetApplyDispatch = false
	triggerPresetApplyComment = ""
	triggerPresetApplyDryRun = false
}

func TestTriggerPresetCLI_ListShowAndSanitization(t *testing.T) {
	resetTriggerPresetFlags(t)

	var seenSource string
	mockPresets := []hubclient.TriggerPreset{
		{
			ID:          "github.command",
			Source:      "github",
			Version:     1,
			Title:       "Slash command \x1b[31mevil\x1b[0m",
			Description: "Matches @app /command\u202e spoofing",
			ActionKinds: []string{"dispatch_agent", "message", "github.comment"},
			Fields: []hubclient.TriggerPresetField{
				{Name: "command", Type: "string", Required: true, Help: "Slash command name"},
				{Name: "on", Type: "enum", Enum: []string{"pull_request", "issue", "any"}, Default: "pull_request", Help: "Target type"},
				{Name: "allow_bots", Type: "bool", Default: false, Help: "Allow bot senders"},
			},
		},
	}

	withMockTriggerPresetHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/trigger-presets" {
			seenSource = r.URL.Query().Get("source")
			_ = json.NewEncoder(w).Encode(mockPresets)
			return
		}
		http.NotFound(w, r)
	}, hubsync.CredentialKindOAuth)

	// 1. list with --source github
	triggerPresetListSource = "github"
	listOut := captureStdout(t, func() {
		require.NoError(t, runTriggerPresetList(triggerPresetListCmd, nil))
	})
	assert.Equal(t, "github", seenSource)
	assert.Contains(t, listOut, "github.command")
	assert.NotContains(t, listOut, "\x1b[31m", "ANSI escape sequences must be sanitized")

	// 2. list --json
	triggerOutputJSON = true
	listJSON := captureStdout(t, func() {
		require.NoError(t, runTriggerPresetList(triggerPresetListCmd, nil))
	})
	assert.NotContains(t, listJSON, "\x1b", "JSON output must escape control chars")
	assert.NotContains(t, listJSON, "\u202e", "JSON output must escape bidi override")
	var decodedList []hubclient.TriggerPreset
	require.NoError(t, json.Unmarshal([]byte(listJSON), &decodedList))
	require.Len(t, decodedList, 1)

	// 3. show text
	triggerOutputJSON = false
	showOut := captureStdout(t, func() {
		require.NoError(t, runTriggerPresetShow(triggerPresetShowCmd, []string{"github.command"}))
	})
	assert.Contains(t, showOut, "Preset: github.command")
	assert.Contains(t, showOut, "pull_request|issue|any")
	assert.NotContains(t, showOut, "\x1b[31m")
	assert.NotContains(t, showOut, "\u202e")

	// 4. show unknown preset
	err := runTriggerPresetShow(triggerPresetShowCmd, []string{"github.nope"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestTriggerPresetCLI_ApplyDryRunFieldCoercionAndActionValidation(t *testing.T) {
	resetTriggerPresetFlags(t)

	var seenCompileReq hubclient.CompileTriggerPresetRequest
	var createCalled bool

	schema := hubclient.TriggerPreset{
		ID:      "github.command",
		Source:  "github",
		Version: 1,
		Title:   "Slash command",
		Fields: []hubclient.TriggerPresetField{
			{Name: "command", Type: "string", Required: true},
			{Name: "on", Type: "enum", Enum: []string{"pull_request", "issue", "any"}},
			{Name: "allowed_associations", Type: "list"},
			{Name: "allow_bots", Type: "bool"},
		},
	}
	compiledResp := hubclient.CompiledTriggerPreset{
		Match: hubclient.CompiledTriggerMatch{
			Types: []string{"github.issue_comment.created"},
			Rules: []hubclient.CompiledTriggerRule{
				{Dialect: "cel", Expr: "event.attributes.command == 'review'"},
			},
		},
		Subject: hubclient.CompiledTriggerSubject{Mode: "new"},
		Form: hubclient.CompiledTriggerForm{
			Preset:  "github.command",
			Version: 1,
			Fields: map[string]any{
				"command":              "review",
				"on":                   "pull_request",
				"allowed_associations": []string{"OWNER", "MEMBER"},
				"allow_bots":           true,
			},
		},
	}

	withMockTriggerPresetHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/trigger-presets":
			_ = json.NewEncoder(w).Encode([]hubclient.TriggerPreset{schema})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/trigger-presets/compile":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&seenCompileReq))
			_ = json.NewEncoder(w).Encode(compiledResp)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/triggers":
			createCalled = true
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.Trigger{ID: "tr-1"})
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindOAuth)

	// 1. --dry-run coerces bool and list fields, prints compiled rule, and does not create.
	triggerPresetApplyDryRun = true
	triggerPresetApplyFields = []string{
		"command=review",
		"on=pull_request",
		"allowed_associations=OWNER,MEMBER",
		"allow_bots=true",
	}
	dryOut := captureStdout(t, func() {
		require.NoError(t, runTriggerPresetApply(triggerPresetApplyCmd, []string{"github.command"}))
	})
	assert.False(t, createCalled, "--dry-run must not call Create")
	assert.Equal(t, "github.command", seenCompileReq.Preset)
	assert.Equal(t, "review", seenCompileReq.Fields["command"])
	assert.Equal(t, true, seenCompileReq.Fields["allow_bots"])
	assert.Equal(t, []any{"OWNER", "MEMBER"}, seenCompileReq.Fields["allowed_associations"])
	assert.Contains(t, dryOut, "Compiled preset: github.command (v1)")
	assert.Contains(t, dryOut, `1/1 [cel] "event.attributes.command == 'review'"`)

	// 2. Reject unavailable actions clearly.
	triggerPresetApplyDryRun = false
	triggerPresetApplyDispatch = true
	err := runTriggerPresetApply(triggerPresetApplyCmd, []string{"github.command"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dispatch_agent")
	assert.Contains(t, err.Error(), "not yet available")

	triggerPresetApplyDispatch = false
	triggerPresetApplyComment = "hello"
	err = runTriggerPresetApply(triggerPresetApplyCmd, []string{"github.command"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "github.comment")
	assert.Contains(t, err.Error(), "not yet available")

	triggerPresetApplyComment = ""
	triggerPresetApplyAction = "dispatch_agent"
	err = runTriggerPresetApply(triggerPresetApplyCmd, []string{"github.command"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not yet available")

	// 3. Invalid --field syntax and invalid bool value return usage errors.
	triggerPresetApplyAction = "message"
	triggerPresetApplyDryRun = true
	triggerPresetApplyFields = []string{"not-a-pair"}
	err = runTriggerPresetApply(triggerPresetApplyCmd, []string{"github.command"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected key=value")

	triggerPresetApplyFields = []string{"allow_bots=maybe"}
	err = runTriggerPresetApply(triggerPresetApplyCmd, []string{"github.command"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid boolean value")
}

func TestTriggerPresetCLI_ApplyRefusesReuseAndCoalesceWindow(t *testing.T) {
	resetTriggerPresetFlags(t)

	var createCalled bool
	schema := hubclient.TriggerPreset{
		ID:      "github.pr_updated",
		Source:  "github",
		Version: 1,
		Title:   "PR updated",
	}
	compiledResp := hubclient.CompiledTriggerPreset{
		Match: hubclient.CompiledTriggerMatch{
			Types: []string{"github.pull_request.synchronize"},
			Rules: []hubclient.CompiledTriggerRule{
				{Dialect: "cel", Expr: "event.attributes.sender_type != 'Bot'"},
			},
		},
		Subject: hubclient.CompiledTriggerSubject{
			Mode:           "reuse",
			CoalesceWindow: "30s",
		},
		Form: hubclient.CompiledTriggerForm{
			Preset:  "github.pr_updated",
			Version: 1,
			Fields:  map[string]any{"coalesce_window": "30s"},
		},
	}

	withMockTriggerPresetHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/trigger-presets":
			_ = json.NewEncoder(w).Encode([]hubclient.TriggerPreset{schema})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/trigger-presets/compile":
			_ = json.NewEncoder(w).Encode(compiledResp)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/triggers":
			createCalled = true
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.Trigger{ID: "tr-1"})
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindOAuth)

	// --dry-run still works to preview the compiled rule.
	triggerPresetApplyDryRun = true
	dryOut := captureStdout(t, func() {
		require.NoError(t, runTriggerPresetApply(triggerPresetApplyCmd, []string{"github.pr_updated"}))
	})
	assert.Contains(t, dryOut, "mode=reuse, coalesce_window=30s")
	assert.False(t, createCalled)

	// Applying without --dry-run refuses client-side before calling Create.
	triggerPresetApplyDryRun = false
	triggerPresetApplyName = "pr-updated-trigger"
	triggerPresetApplyAgent = "lead"
	triggerPresetApplyMessage = "Handle PR update"
	err := runTriggerPresetApply(triggerPresetApplyCmd, []string{"github.pr_updated"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not yet supported")
	assert.Contains(t, err.Error(), "reuse")
	assert.Contains(t, err.Error(), "30s")
	assert.False(t, createCalled, "apply must refuse client-side before calling Create")
}
