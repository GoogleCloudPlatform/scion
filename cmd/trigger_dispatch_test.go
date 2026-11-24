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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetTriggerDispatchTestFlags(t *testing.T) {
	t.Helper()
	origFormat := outputFormat
	origJSON := triggerOutputJSON
	origName := triggerName
	origDesc := triggerDescription
	origWebhook := triggerWebhookRef
	origGitHub := triggerGitHubSource
	origTypes := triggerMatchTypes
	origWhere := triggerWhereRules
	origWhereFile := triggerWhereFile
	origAction := triggerAction
	origAgent := triggerAgent
	origMsg := triggerMessage
	origInterrupt := triggerInterrupt
	origWake := triggerWake
	origPlain := triggerPlain
	origDispatch := triggerDispatch
	origSubject := triggerSubjectExpr
	origOnSubject := triggerOnSubject
	origRenderEvent := triggerRenderEventRef

	t.Cleanup(func() {
		outputFormat = origFormat
		triggerOutputJSON = origJSON
		triggerName = origName
		triggerDescription = origDesc
		triggerWebhookRef = origWebhook
		triggerGitHubSource = origGitHub
		triggerMatchTypes = origTypes
		triggerWhereRules = origWhere
		triggerWhereFile = origWhereFile
		triggerAction = origAction
		triggerAgent = origAgent
		triggerMessage = origMsg
		triggerInterrupt = origInterrupt
		triggerWake = origWake
		triggerPlain = origPlain
		triggerDispatch = origDispatch
		triggerSubjectExpr = origSubject
		triggerOnSubject = origOnSubject
		triggerRenderEventRef = origRenderEvent
	})

	outputFormat = ""
	triggerOutputJSON = false
	triggerName = ""
	triggerDescription = ""
	triggerWebhookRef = ""
	triggerGitHubSource = false
	triggerMatchTypes = nil
	triggerWhereRules = nil
	triggerWhereFile = ""
	triggerAction = "message"
	triggerAgent = ""
	triggerMessage = ""
	triggerInterrupt = false
	triggerWake = false
	triggerPlain = false
	triggerDispatch = dispatchAgentFlagValues{}
	triggerSubjectExpr = ""
	triggerOnSubject = ""
	triggerRenderEventRef = ""
}

func TestTriggerCreate_DispatchAgentDerivedAllowAndFlags(t *testing.T) {
	resetTriggerDispatchTestFlags(t)

	taskFilePath := filepath.Join(t.TempDir(), "task.txt")
	require.NoError(t, os.WriteFile(taskFilePath, []byte("Fix ${event.payload.title} for ${trigger.name}"), 0o600))

	var gotReq hubclient.CreateTriggerRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/v1/projects/proj-1/triggers", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(hubclient.Trigger{
			ID:        "tr-disp-1",
			ProjectID: "proj-1",
			Name:      gotReq.Name,
			Source:    gotReq.Source,
			Match:     hubclient.TriggerMatchSpec{Types: []string{"webhook.received"}, Rules: []hubclient.TriggerMatchRuleSpec{}},
			Action:    gotReq.Action,
			Status:    "active",
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		})
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	origFn := triggerHubContextFn
	t.Cleanup(func() { triggerHubContextFn = origFn })
	triggerHubContextFn = func() (*HubContext, string, context.Context, context.CancelFunc, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		return &HubContext{Client: client, Endpoint: srv.URL}, "proj-1", ctx, cancel, nil
	}

	triggerName = "pr-fix"
	triggerWebhookRef = "ep-1"
	triggerAction = "dispatch-agent"
	triggerDispatch = dispatchAgentFlagValues{
		template:       "default",
		taskFile:       taskFilePath,
		agentName:      "pr-${event.attributes.number}",
		onNameConflict: "skip",
		branch:         "fix/${event.attributes.number}",
		labels:         []string{"pr=${event.attributes.number}", "env=${event.payload.env}"},
		ephemeral:      true,
		reuseMessage:   "Update ${event.attributes.sha}",
		githubToken:    "user_preferred",
		agentRole:      "baseline",
	}

	out := captureStdout(t, func() {
		require.NoError(t, runTriggerCreate(triggerCreateCmd, nil))
	})

	assert.Equal(t, "dispatch_agent", gotReq.Action.Type)
	assert.Equal(t, "default", gotReq.Action.Template)
	assert.Equal(t, "Fix ${event.payload.title} for ${trigger.name}", gotReq.Action.Task)
	assert.Equal(t, "pr-${event.attributes.number}", gotReq.Action.AgentName)
	assert.Equal(t, "skip", gotReq.Action.NameConflict)
	assert.Equal(t, "fix/${event.attributes.number}", gotReq.Action.Branch)
	assert.Equal(t, map[string]string{
		"pr":  "${event.attributes.number}",
		"env": "${event.payload.env}",
	}, gotReq.Action.Labels)
	assert.True(t, gotReq.Action.Ephemeral)
	assert.Equal(t, "Update ${event.attributes.sha}", gotReq.Action.ReuseMessage)
	assert.Equal(t, "user_preferred", gotReq.Action.GithubToken)
	assert.Equal(t, "baseline", gotReq.Action.AgentRole)

	// Derived untrusted vars are deduplicated in deterministic slot order and printed.
	assert.Equal(t, []string{
		"event.payload.title",
		"event.attributes.number",
		"event.payload.env",
		"event.attributes.sha",
	}, gotReq.Action.UntrustedVars)
	assert.Contains(t, out, "Event values that will be inserted as data: event.payload.title, event.attributes.number, event.payload.env, event.attributes.sha")
	assert.Contains(t, out, "Action:  dispatch_agent (template: default)")
}

func TestTriggerCreate_DispatchAgentExplicitAllowNotReDerived(t *testing.T) {
	resetTriggerDispatchTestFlags(t)

	triggerName = "explicit-allow"
	triggerWebhookRef = "ep-1"
	triggerAction = "dispatch-agent"
	triggerDispatch = dispatchAgentFlagValues{
		template: "default",
		task:     "Fix ${event.payload.title}",
		allow:    []string{"event.payload.*"},
	}

	req, derived, err := buildCreateTriggerRequestWithAllow(true)
	require.NoError(t, err)
	assert.Nil(t, derived)
	assert.Equal(t, []string{"event.payload.*"}, req.Action.UntrustedVars)
}

func TestTriggerCreate_DispatchAgentFlagValidation(t *testing.T) {
	t.Run("missing template", func(t *testing.T) {
		resetTriggerDispatchTestFlags(t)
		triggerName = "bad"
		triggerWebhookRef = "ep-1"
		triggerAction = "dispatch-agent"
		_, _, err := buildCreateTriggerRequestWithAllow(false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--template is required")
	})

	t.Run("task and task-file mutually exclusive", func(t *testing.T) {
		resetTriggerDispatchTestFlags(t)
		triggerName = "bad"
		triggerWebhookRef = "ep-1"
		triggerAction = "dispatch-agent"
		triggerDispatch = dispatchAgentFlagValues{
			template: "default",
			task:     "inline",
			taskFile: "/tmp/file",
		}
		_, _, err := buildCreateTriggerRequestWithAllow(false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--task and --task-file cannot be used together")
	})

	t.Run("invalid label syntax", func(t *testing.T) {
		resetTriggerDispatchTestFlags(t)
		triggerName = "bad"
		triggerWebhookRef = "ep-1"
		triggerAction = "dispatch-agent"
		triggerDispatch = dispatchAgentFlagValues{
			template: "default",
			labels:   []string{"missing-equals"},
		}
		_, _, err := buildCreateTriggerRequestWithAllow(false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expected key=value")
	})

	t.Run("invalid on-name-conflict", func(t *testing.T) {
		resetTriggerDispatchTestFlags(t)
		triggerName = "bad"
		triggerWebhookRef = "ep-1"
		triggerAction = "dispatch-agent"
		triggerDispatch = dispatchAgentFlagValues{
			template:       "default",
			onNameConflict: "overwrite",
		}
		_, _, err := buildCreateTriggerRequestWithAllow(false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--on-name-conflict")
	})

	t.Run("invalid github-token", func(t *testing.T) {
		resetTriggerDispatchTestFlags(t)
		triggerName = "bad"
		triggerWebhookRef = "ep-1"
		triggerAction = "dispatch-agent"
		triggerDispatch = dispatchAgentFlagValues{
			template:    "default",
			githubToken: "pat",
		}
		_, _, err := buildCreateTriggerRequestWithAllow(false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--github-token")
	})

	t.Run("invalid template syntax caught during derivation", func(t *testing.T) {
		resetTriggerDispatchTestFlags(t)
		triggerName = "bad"
		triggerWebhookRef = "ep-1"
		triggerAction = "dispatch-agent"
		triggerDispatch = dispatchAgentFlagValues{
			template: "default",
			task:     "hello ${unclosed",
		}
		_, _, err := buildCreateTriggerRequestWithAllow(false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `slot "task"`)
		assert.Contains(t, err.Error(), "byte offset 6")
	})
}

func TestFormatTriggerAPIError(t *testing.T) {
	cases := []struct {
		name     string
		apiErr   *apiclient.APIError
		contains []string
	}{
		{
			name: "invalid_template with slot and offset",
			apiErr: &apiclient.APIError{
				StatusCode: 400,
				Code:       "invalid_template",
				Message:    "bad",
				Details:    map[string]any{"slot": "task", "offset": float64(12), "message": "unclosed placeholder"},
			},
			contains: []string{`slot "task"`, "byte offset 12", "unclosed placeholder"},
		},
		{
			name: "untrusted_var_not_allowed",
			apiErr: &apiclient.APIError{
				StatusCode: 400,
				Code:       "untrusted_var_not_allowed",
				Message:    "not allowed",
				Details:    map[string]any{"slot": "branch", "path": "event.attributes.ref"},
			},
			contains: []string{`"event.attributes.ref"`, `slot "branch"`, "--allow event.attributes.ref"},
		},
		{
			name: "invalid_untrusted_var",
			apiErr: &apiclient.APIError{
				StatusCode: 400,
				Code:       "invalid_untrusted_var",
				Message:    "bad var",
				Details:    map[string]any{"path": "trigger.id"},
			},
			contains: []string{`--allow path "trigger.id"`},
		},
		{
			name: "reserved_label_key",
			apiErr: &apiclient.APIError{
				StatusCode: 400,
				Code:       "reserved_label_key",
				Message:    "reserved",
				Details:    map[string]any{"key": "trigger_event"},
			},
			contains: []string{`"trigger_event"`, "reserved"},
		},
		{
			name: "source_not_replay_protected",
			apiErr: &apiclient.APIError{
				StatusCode: 400,
				Code:       "source_not_replay_protected",
				Message:    "no replay",
			},
			contains: []string{"replay protection", "dispatch-agent"},
		},
		{
			name: "ephemeral_unsupported",
			apiErr: &apiclient.APIError{
				StatusCode: 400,
				Code:       "ephemeral_unsupported",
				Message:    "ephemeral agents are not yet supported",
			},
			contains: []string{"--ephemeral", "not yet supported"},
		},
		{
			name: "github_token_required",
			apiErr: &apiclient.APIError{
				StatusCode: 400,
				Code:       "github_token_required",
				Message:    "missing installation",
			},
			contains: []string{"--github-token", "missing installation"},
		},
		{
			name: "invalid_value with field",
			apiErr: &apiclient.APIError{
				StatusCode: 400,
				Code:       "invalid_value",
				Message:    "unsupported agentRole",
				Details:    map[string]any{"field": "agentRole"},
			},
			contains: []string{"agentRole", "unsupported agentRole"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			formatted := formatTriggerAPIError(tc.apiErr)
			require.Error(t, formatted)
			for _, sub := range tc.contains {
				assert.Contains(t, formatted.Error(), sub)
			}
		})
	}
}

func TestTriggerRenderCLI_EscapesControlAndBidiAndHighlightsUntrusted(t *testing.T) {
	resetTriggerDispatchTestFlags(t)

	const trigID = "11111111-1111-1111-1111-111111111111"
	const eventID = "22222222-2222-2222-2222-222222222222"

	renderResp := hubclient.TriggerRenderResponse{
		Slots: hubclient.TriggerRenderSlots{
			Task: &hubclient.TriggerRenderSlotResult{
				Rendered: "Inspect \x1b[31mred\x1b[0m and bidi \u202eevil\u202c\nline2",
				Used: []hubclient.TriggerRenderUsedVar{
					{Path: "event.payload.title", Class: "untrusted"},
					{Path: "trigger.name", Class: "trusted"},
				},
			},
			Name: &hubclient.TriggerRenderSlotResult{
				Rendered: "pr-42",
				Used: []hubclient.TriggerRenderUsedVar{
					{Path: "event.attributes.number", Class: "untrusted"},
				},
			},
			Labels: map[string]hubclient.TriggerRenderSlotResult{
				"pr": {
					Rendered: "42\x07",
					Used:     []hubclient.TriggerRenderUsedVar{{Path: "event.attributes.number", Class: "untrusted"}},
				},
			},
		},
		Errors: []hubclient.TriggerRenderError{
			{Slot: "branch", Path: "event.attributes.branch", Kind: "template value missing"},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/v1/projects/proj-1/triggers/"+trigID+"/render", r.URL.Path)
		assert.Equal(t, eventID, r.URL.Query().Get("eventId"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(renderResp)
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	origFn := triggerHubContextFn
	t.Cleanup(func() { triggerHubContextFn = origFn })
	triggerHubContextFn = func() (*HubContext, string, context.Context, context.CancelFunc, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		return &HubContext{Client: client, Endpoint: srv.URL}, "proj-1", ctx, cancel, nil
	}

	// Human mode: escapes ANSI ESC, BEL, and bidi overrides; highlights [UNTRUSTED].
	triggerRenderEventRef = eventID
	humanOut := captureStdout(t, func() {
		require.NoError(t, runTriggerRender(triggerRenderCmd, []string{trigID}))
	})
	assert.NotContains(t, humanOut, "\x1b[31m", "ANSI escape must be escaped in terminal output")
	assert.NotContains(t, humanOut, "\u202e", "bidi RLO override must be escaped in terminal output")
	assert.NotContains(t, humanOut, "\x07", "BEL control char must be escaped in terminal output")
	assert.Contains(t, humanOut, "event.payload.title [UNTRUSTED]")
	assert.Contains(t, humanOut, "trigger.name [trusted]")
	assert.Contains(t, humanOut, "Slot: label.pr")
	assert.Contains(t, humanOut, "[branch] template value missing (path: event.attributes.branch)")

	// JSON mode: valid JSON DTO with raw structure preserved (control/bidi JSON-escaped).
	triggerOutputJSON = true
	jsonOut := captureStdout(t, func() {
		require.NoError(t, runTriggerRender(triggerRenderCmd, []string{trigID}))
	})
	var decoded hubclient.TriggerRenderResponse
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &decoded))
	require.NotNil(t, decoded.Slots.Task)
	assert.Equal(t, renderResp.Slots.Task.Rendered, decoded.Slots.Task.Rendered)
	require.Len(t, decoded.Errors, 1)
	assert.Equal(t, "branch", decoded.Errors[0].Slot)
}

func TestTriggerPresetApply_DispatchAgentSucceeds(t *testing.T) {
	resetTriggerPresetFlags(t)

	var gotCreate hubclient.CreateTriggerRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/trigger-presets":
			_ = json.NewEncoder(w).Encode([]hubclient.TriggerPreset{
				{ID: "github.command", Source: "github", Version: 1, Title: "Slash command"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/trigger-presets/compile":
			_ = json.NewEncoder(w).Encode(hubclient.CompiledTriggerPreset{
				Match: hubclient.CompiledTriggerMatch{
					Types: []string{"github.issue_comment.created"},
					Rules: []hubclient.CompiledTriggerRule{{Dialect: "cel", Expr: "event.attributes.command == 'review'"}},
				},
				Subject: hubclient.CompiledTriggerSubject{Mode: "skip", Expr: "event.subject"},
				Form:    hubclient.CompiledTriggerForm{Preset: "github.command", Version: 1},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/triggers":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotCreate))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.Trigger{
				ID:        "tr-preset-disp-1",
				ProjectID: "proj-1",
				Name:      gotCreate.Name,
				Source:    gotCreate.Source,
				Match:     *gotCreate.Match,
				Action:    gotCreate.Action,
				Status:    "active",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	origFn := triggerHubContextFn
	t.Cleanup(func() { triggerHubContextFn = origFn })
	triggerHubContextFn = func() (*HubContext, string, context.Context, context.CancelFunc, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		return &HubContext{Client: client, Endpoint: srv.URL}, "proj-1", ctx, cancel, nil
	}

	triggerPresetApplyName = "gh-review-dispatch"
	triggerPresetApplyAction = "dispatch-agent"
	triggerPresetApplyDispatchFlags = dispatchAgentFlagValues{
		template:    "default",
		task:        "Review PR #${event.attributes.number}",
		githubToken: "installation",
	}

	out := captureStdout(t, func() {
		require.NoError(t, runTriggerPresetApply(triggerPresetApplyCmd, []string{"github.command"}))
	})
	assert.Equal(t, "dispatch_agent", gotCreate.Action.Type)
	assert.Equal(t, "default", gotCreate.Action.Template)
	assert.Equal(t, "installation", gotCreate.Action.GithubToken)
	assert.Equal(t, []string{"event.attributes.number"}, gotCreate.Action.UntrustedVars)
	assert.Contains(t, out, "Event values that will be inserted as data: event.attributes.number")
	assert.Contains(t, out, "Action:  dispatch_agent (template: default)")
}

func TestTriggerRenderAndUpdate_AgentAndWindowsGates(t *testing.T) {
	assert.True(t, agentAllowed["trigger.render"], "trigger.render is read-only and should be allowed in agent mode")
	assert.False(t, agentAllowed["trigger.update"], "trigger.update must remain absent from agentAllowed (agents cannot PATCH triggers)")
	assert.True(t, agentAllowed["trigger.create"], "trigger.create remains allowed in agent mode")
	assert.True(t, windowsAllowed["trigger.render"], "trigger.render must be listed in windowsAllowed")
	assert.True(t, windowsAllowed["trigger.update"], "trigger.update must be listed in windowsAllowed")
}

func TestTriggerUpdate_DispatchAgentPartialUpdateAndDerivedAllow(t *testing.T) {
	resetTriggerDispatchTestFlags(t)

	const trigID = "33333333-3333-3333-3333-333333333333"
	var gotPatch hubclient.UpdateTriggerRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(hubclient.Trigger{
				ID:                    trigID,
				ProjectID:             "proj-1",
				Name:                  "pr-fix",
				AuthorizationRevision: 2,
				Action: hubclient.TriggerActionSpec{
					Type:     "dispatch_agent",
					Template: "default",
					Task:     "old task",
					Branch:   "fix/${event.attributes.number}",
				},
				Status: "active",
			})
		case http.MethodPatch:
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotPatch))
			_ = json.NewEncoder(w).Encode(hubclient.Trigger{
				ID:                    trigID,
				ProjectID:             "proj-1",
				Name:                  "pr-fix",
				AuthorizationRevision: 3,
				Action:                *gotPatch.Action,
				Status:                "active",
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	origFn := triggerHubContextFn
	t.Cleanup(func() { triggerHubContextFn = origFn })
	triggerHubContextFn = func() (*HubContext, string, context.Context, context.CancelFunc, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		return &HubContext{Client: client, Endpoint: srv.URL}, "proj-1", ctx, cancel, nil
	}

	// Build a fresh cobra command with the same flags so Changed() state is clean and isolated.
	origTask := triggerUpdateDispatch.task
	origRev := triggerUpdateRevision
	t.Cleanup(func() {
		triggerUpdateDispatch.task = origTask
		triggerUpdateRevision = origRev
	})

	require.NoError(t, triggerUpdateCmd.Flags().Set("task", "Fix ${event.payload.title}"))
	require.NoError(t, triggerUpdateCmd.Flags().Set("revision", "2"))
	t.Cleanup(func() {
		_ = triggerUpdateCmd.Flags().Set("task", "")
		_ = triggerUpdateCmd.Flags().Set("revision", "0")
		triggerUpdateCmd.Flags().Lookup("task").Changed = false
		triggerUpdateCmd.Flags().Lookup("revision").Changed = false
	})

	out := captureStdout(t, func() {
		require.NoError(t, runTriggerUpdate(triggerUpdateCmd, []string{trigID}))
	})

	require.NotNil(t, gotPatch.ExpectedRevision)
	assert.Equal(t, 2, *gotPatch.ExpectedRevision)
	require.NotNil(t, gotPatch.Action)
	assert.Equal(t, "dispatch_agent", gotPatch.Action.Type)
	assert.Equal(t, "default", gotPatch.Action.Template, "existing template should be preserved on partial update")
	assert.Equal(t, "Fix ${event.payload.title}", gotPatch.Action.Task)
	assert.Equal(t, "fix/${event.attributes.number}", gotPatch.Action.Branch, "existing branch should be preserved")
	assert.Equal(t, []string{"event.payload.title", "event.attributes.number"}, gotPatch.Action.UntrustedVars)
	assert.Contains(t, out, "Event values that will be inserted as data: event.payload.title, event.attributes.number")
	assert.Contains(t, out, "Trigger updated: "+trigID+" (revision 3)")
}

func TestTriggerUpdate_PinsRevisionWhenActionChangedAndMaps409(t *testing.T) {
	resetTriggerDispatchTestFlags(t)

	const trigID = "44444444-4444-4444-4444-444444444444"
	var gotPatch hubclient.UpdateTriggerRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(hubclient.Trigger{
				ID:                    trigID,
				ProjectID:             "proj-1",
				Name:                  "pr-fix",
				AuthorizationRevision: 7,
				Action: hubclient.TriggerActionSpec{
					Type:     "dispatch_agent",
					Template: "default",
					Task:     "old task",
				},
				Status: "active",
			})
		case http.MethodPatch:
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotPatch))
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    "revision_conflict",
					"message": "trigger revision mismatch; refresh and retry",
				},
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	origFn := triggerHubContextFn
	t.Cleanup(func() { triggerHubContextFn = origFn })
	triggerHubContextFn = func() (*HubContext, string, context.Context, context.CancelFunc, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		return &HubContext{Client: client, Endpoint: srv.URL}, "proj-1", ctx, cancel, nil
	}

	origTask := triggerUpdateDispatch.task
	t.Cleanup(func() { triggerUpdateDispatch.task = origTask })

	// Set --task without setting --revision: ExpectedRevision must be pinned to existing.AuthorizationRevision (7).
	require.NoError(t, triggerUpdateCmd.Flags().Set("task", "New task"))
	t.Cleanup(func() {
		_ = triggerUpdateCmd.Flags().Set("task", "")
		triggerUpdateCmd.Flags().Lookup("task").Changed = false
	})

	err = runTriggerUpdate(triggerUpdateCmd, []string{trigID})
	require.Error(t, err)
	require.NotNil(t, gotPatch.ExpectedRevision, "ExpectedRevision must be pinned from fetched trigger when --revision is omitted")
	assert.Equal(t, 7, *gotPatch.ExpectedRevision)
	assert.Contains(t, err.Error(), "revision conflict")
}

func TestTriggerCLI_RefusesCrossActionFlags(t *testing.T) {
	t.Run("create_message_refuses_dispatch_flags", func(t *testing.T) {
		resetTriggerDispatchTestFlags(t)
		triggerName = "bad-msg"
		triggerWebhookRef = "ep-1"
		triggerAction = "message"
		triggerAgent = "lead"
		triggerMessage = "hi"
		triggerDispatch.task = "stray task"

		_, _, err := buildCreateTriggerRequestWithAllow(false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--task cannot be used with --action message")
	})

	t.Run("create_dispatch_refuses_message_flags", func(t *testing.T) {
		resetTriggerDispatchTestFlags(t)
		triggerName = "bad-dispatch"
		triggerWebhookRef = "ep-1"
		triggerAction = "dispatch-agent"
		triggerDispatch.template = "default"
		triggerDispatch.task = "run"
		triggerMessage = "stray message"

		_, _, err := buildCreateTriggerRequestWithAllow(false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--message cannot be used with --action dispatch-agent")
	})

	t.Run("update_refuses_cross_action_flags", func(t *testing.T) {
		resetTriggerDispatchTestFlags(t)
		const trigID = "55555555-5555-5555-5555-555555555555"

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(hubclient.Trigger{
					ID:                    trigID,
					ProjectID:             "proj-1",
					Name:                  "msg-trig",
					AuthorizationRevision: 1,
					Action: hubclient.TriggerActionSpec{
						Type:      "message",
						AgentName: "lead",
						Message:   "hello",
					},
					Status: "active",
				})
				return
			}
			t.Errorf("unexpected %s request when cross-action flags should have failed client-side", r.Method)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)

		client, err := hubclient.New(srv.URL)
		require.NoError(t, err)
		origFn := triggerHubContextFn
		t.Cleanup(func() { triggerHubContextFn = origFn })
		triggerHubContextFn = func() (*HubContext, string, context.Context, context.CancelFunc, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			return &HubContext{Client: client, Endpoint: srv.URL}, "proj-1", ctx, cancel, nil
		}

		require.NoError(t, triggerUpdateCmd.Flags().Set("task", "not-for-message"))
		t.Cleanup(func() {
			_ = triggerUpdateCmd.Flags().Set("task", "")
			triggerUpdateCmd.Flags().Lookup("task").Changed = false
		})

		err = runTriggerUpdate(triggerUpdateCmd, []string{trigID})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--task cannot be used with action type \"message\"")
	})
}
