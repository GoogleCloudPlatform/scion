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
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetTriggerGitHubCommentTestFlags(t *testing.T) {
	t.Helper()
	resetTriggerDispatchTestFlags(t)
	origComment := triggerComment
	origReplyInThread := triggerReplyInThread
	origOffline := triggerOffline
	origQueueTTL := triggerQueueTTL
	origCoalesce := triggerCoalesceWindow
	t.Cleanup(func() {
		triggerComment = origComment
		triggerReplyInThread = origReplyInThread
		triggerOffline = origOffline
		triggerQueueTTL = origQueueTTL
		triggerCoalesceWindow = origCoalesce
	})
	triggerComment = ""
	triggerReplyInThread = false
	triggerOffline = ""
	triggerQueueTTL = ""
	triggerCoalesceWindow = ""
}

func TestTriggerCreate_GitHubCommentAndReplyInThread(t *testing.T) {
	t.Run("basic comment with auto-derived untrusted vars", func(t *testing.T) {
		resetTriggerGitHubCommentTestFlags(t)

		triggerName = "gh-ack"
		triggerGitHubSource = true
		triggerMatchTypes = []string{"github.issue_comment.created"}
		triggerComment = "Thanks @${event.attributes.sender} — `/${event.attributes.command}` received for ${event.subject}."

		req, derived, err := buildCreateTriggerRequestWithFlags(false, false, true, false)
		require.NoError(t, err)
		assert.Equal(t, hubclient.TriggerSourceSpec{Kind: "github", ID: ""}, req.Source)
		assert.Equal(t, "github.comment", req.Action.Type)
		assert.Equal(t, triggerComment, req.Action.Body)
		assert.Equal(t, "event", req.Action.Target)
		assert.False(t, req.Action.ReplyInThread)
		assert.Equal(t, []string{"event.attributes.sender", "event.attributes.command", "event.subject"}, req.Action.UntrustedVars)
		assert.Equal(t, req.Action.UntrustedVars, derived)
	})

	t.Run("reply-in-thread with explicit --allow", func(t *testing.T) {
		resetTriggerGitHubCommentTestFlags(t)

		triggerName = "gh-review-reply"
		triggerGitHubSource = true
		triggerMatchTypes = []string{"github.pull_request_review_comment.created"}
		triggerComment = "Ack @${event.attributes.sender}"
		triggerReplyInThread = true
		triggerDispatch.allow = []string{"event.attributes.*"}

		req, derived, err := buildCreateTriggerRequestWithFlags(true, false, true, false)
		require.NoError(t, err)
		assert.Equal(t, "github.comment", req.Action.Type)
		assert.Equal(t, "Ack @${event.attributes.sender}", req.Action.Body)
		assert.Equal(t, "event", req.Action.Target)
		assert.True(t, req.Action.ReplyInThread)
		assert.Equal(t, []string{"event.attributes.*"}, req.Action.UntrustedVars)
		assert.Empty(t, derived)
	})

	t.Run("explicit --action github.comment with --comment", func(t *testing.T) {
		resetTriggerGitHubCommentTestFlags(t)

		triggerName = "gh-explicit-action"
		triggerGitHubSource = true
		triggerMatchTypes = []string{"github.issues.opened"}
		triggerAction = "github.comment"
		triggerComment = "Welcome!"

		req, derived, err := buildCreateTriggerRequestWithFlags(false, true, true, false)
		require.NoError(t, err)
		assert.Equal(t, "github.comment", req.Action.Type)
		assert.Equal(t, "Welcome!", req.Action.Body)
		assert.Equal(t, "event", req.Action.Target)
		assert.Empty(t, req.Action.UntrustedVars)
		assert.Empty(t, derived)
	})

	t.Run("end-to-end runTriggerCreate sends expected JSON and prints summary", func(t *testing.T) {
		resetTriggerGitHubCommentTestFlags(t)

		var gotBody map[string]any
		withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/triggers" {
				require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(hubclient.Trigger{
					ID:     "tr-comment-1",
					Name:   "gh-thread-reply",
					Source: hubclient.TriggerSourceSpec{Kind: "github"},
					Match:  hubclient.TriggerMatchSpec{Types: []string{"github.pull_request_review_comment.created"}},
					Action: hubclient.TriggerActionSpec{
						Type:          "github.comment",
						Body:          "Noted @${event.attributes.sender}",
						Target:        "event",
						ReplyInThread: true,
						UntrustedVars: []string{"event.attributes.sender"},
					},
					Status: "active",
				})
				return
			}
			http.NotFound(w, r)
		}, hubsync.CredentialKindOAuth)

		triggerName = "gh-thread-reply"
		triggerGitHubSource = true
		triggerMatchTypes = []string{"github.pull_request_review_comment.created"}
		triggerComment = "Noted @${event.attributes.sender}"
		triggerReplyInThread = true

		out := captureStdout(t, func() {
			require.NoError(t, runTriggerCreate(nil, nil))
		})
		actionObj, ok := gotBody["action"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "github.comment", actionObj["type"])
		assert.Equal(t, "Noted @${event.attributes.sender}", actionObj["body"])
		assert.Equal(t, "event", actionObj["target"])
		assert.Equal(t, true, actionObj["replyInThread"])
		assert.Equal(t, []any{"event.attributes.sender"}, actionObj["untrustedVars"])
		assert.Contains(t, out, "Action:  github.comment (reply-in-thread)")
		assert.Contains(t, out, "Comment: Noted @${event.attributes.sender}")
	})
}

func TestTriggerCreate_DispatchAckComment(t *testing.T) {
	t.Run("ack-comment lands in dispatch payload and derives untrusted vars with PhasePost result vars", func(t *testing.T) {
		resetTriggerGitHubCommentTestFlags(t)

		triggerName = "gh-review-dispatch"
		triggerGitHubSource = true
		triggerMatchTypes = []string{"github.issue_comment.created"}
		triggerAction = "dispatch-agent"
		triggerDispatch.template = "reviewer"
		triggerDispatch.task = "Review PR #${event.attributes.number}"
		triggerDispatch.ackComment = "Started **${result.agent_name}** — [open](${result.agent_url}). Requested by @${event.attributes.sender}."

		req, derived, err := buildCreateTriggerRequestWithFlags(false, true, false, true)
		require.NoError(t, err)
		assert.Equal(t, "dispatch_agent", req.Action.Type)
		require.NotNil(t, req.Action.AckComment)
		assert.Equal(t, triggerDispatch.ackComment, req.Action.AckComment.Body)
		assert.Nil(t, req.Action.UntrustedVars)
		assert.Nil(t, derived)
	})

	t.Run("ack-comment rejected on non-github webhook source", func(t *testing.T) {
		resetTriggerGitHubCommentTestFlags(t)

		triggerName = "wh-dispatch"
		triggerWebhookRef = "ep-1"
		triggerAction = "dispatch-agent"
		triggerDispatch.template = "reviewer"
		triggerDispatch.task = "Handle event"
		triggerDispatch.ackComment = "Started ${result.agent_name}"

		_, _, err := buildCreateTriggerRequestWithFlags(false, true, false, true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--ack-comment requires --github")
	})

	t.Run("empty explicit ack-comment rejected", func(t *testing.T) {
		resetTriggerGitHubCommentTestFlags(t)

		triggerName = "gh-dispatch"
		triggerGitHubSource = true
		triggerMatchTypes = []string{"github.issue_comment.created"}
		triggerAction = "dispatch-agent"
		triggerDispatch.template = "reviewer"
		triggerDispatch.task = "Handle event"
		triggerDispatch.ackComment = "   "

		_, _, err := buildCreateTriggerRequestWithFlags(false, true, false, true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--ack-comment cannot be empty")
	})
}

func TestTriggerCreate_GitHubCommentInvalidFlagCombinations(t *testing.T) {
	tests := []struct {
		name               string
		setup              func()
		actionExplicit     bool
		commentExplicit    bool
		ackCommentExplicit bool
		wantErrContains    string
	}{
		{
			name: "reply-in-thread without comment",
			setup: func() {
				triggerName = "bad-1"
				triggerGitHubSource = true
				triggerMatchTypes = []string{"github.pull_request_review_comment.created"}
				triggerAgent = "lead"
				triggerMessage = "hi"
				triggerReplyInThread = true
			},
			wantErrContains: "--reply-in-thread requires --comment",
		},
		{
			name: "comment without github source",
			setup: func() {
				triggerName = "bad-2"
				triggerWebhookRef = "ep-1"
				triggerComment = "hello"
			},
			commentExplicit: true,
			wantErrContains: "--comment requires --github",
		},
		{
			name: "action github.comment without comment body",
			setup: func() {
				triggerName = "bad-3"
				triggerGitHubSource = true
				triggerMatchTypes = []string{"github.issue_comment.created"}
				triggerAction = "github.comment"
			},
			actionExplicit:  true,
			wantErrContains: "--comment is required",
		},
		{
			name: "comment with explicit action message",
			setup: func() {
				triggerName = "bad-4"
				triggerGitHubSource = true
				triggerMatchTypes = []string{"github.issue_comment.created"}
				triggerAction = "message"
				triggerComment = "hello"
			},
			actionExplicit:  true,
			commentExplicit: true,
			wantErrContains: "--comment cannot be used with --action message",
		},
		{
			name: "comment with explicit action dispatch-agent",
			setup: func() {
				triggerName = "bad-5"
				triggerGitHubSource = true
				triggerMatchTypes = []string{"github.issue_comment.created"}
				triggerAction = "dispatch-agent"
				triggerComment = "hello"
			},
			actionExplicit:  true,
			commentExplicit: true,
			wantErrContains: "--comment cannot be used with --action dispatch-agent",
		},
		{
			name: "comment combined with --agent",
			setup: func() {
				triggerName = "bad-6"
				triggerGitHubSource = true
				triggerMatchTypes = []string{"github.issue_comment.created"}
				triggerComment = "hello"
				triggerAgent = "lead"
			},
			commentExplicit: true,
			wantErrContains: "--agent cannot be used with --action github.comment",
		},
		{
			name: "comment combined with --template",
			setup: func() {
				triggerName = "bad-7"
				triggerGitHubSource = true
				triggerMatchTypes = []string{"github.issue_comment.created"}
				triggerComment = "hello"
				triggerDispatch.template = "reviewer"
			},
			commentExplicit: true,
			wantErrContains: "--template cannot be used with --action github.comment",
		},
		{
			name: "ack-comment combined with --action message",
			setup: func() {
				triggerName = "bad-8"
				triggerGitHubSource = true
				triggerMatchTypes = []string{"github.issue_comment.created"}
				triggerAction = "message"
				triggerAgent = "lead"
				triggerMessage = "hi"
				triggerDispatch.ackComment = "ack"
			},
			actionExplicit:     true,
			ackCommentExplicit: true,
			wantErrContains:    "--ack-comment cannot be used with --action message",
		},
		{
			name: "ack-comment combined with --comment",
			setup: func() {
				triggerName = "bad-9"
				triggerGitHubSource = true
				triggerMatchTypes = []string{"github.issue_comment.created"}
				triggerComment = "hello"
				triggerDispatch.ackComment = "ack"
			},
			commentExplicit:    true,
			ackCommentExplicit: true,
			wantErrContains:    "--ack-comment cannot be used with --action github.comment",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resetTriggerGitHubCommentTestFlags(t)
			tc.setup()
			_, _, err := buildCreateTriggerRequestWithFlags(false, tc.actionExplicit, tc.commentExplicit, tc.ackCommentExplicit)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrContains)
		})
	}
}

func TestTriggerPresetCLI_ApplyGitHubCommentAndAckComment(t *testing.T) {
	schema := hubclient.TriggerPreset{
		ID:          "github.command",
		Source:      "github",
		Version:     1,
		Title:       "Slash command",
		ActionKinds: []string{"dispatch_agent", "message", "github.comment"},
		Fields: []hubclient.TriggerPresetField{
			{Name: "command", Type: "string", Required: true},
		},
	}
	compiledResp := hubclient.CompiledTriggerPreset{
		Match: hubclient.CompiledTriggerMatch{
			Types: []string{"github.pull_request_review_comment.created"},
			Rules: []hubclient.CompiledTriggerRule{
				{Dialect: "cel", Expr: "event.attributes.command == 'help'"},
			},
		},
		Subject: hubclient.CompiledTriggerSubject{Mode: "new"},
		Form: hubclient.CompiledTriggerForm{
			Preset:  "github.command",
			Version: 1,
			Fields:  map[string]any{"command": "help"},
		},
	}

	t.Run("preset apply with --comment and --reply-in-thread", func(t *testing.T) {
		resetTriggerPresetFlags(t)

		var seenCreate hubclient.CreateTriggerRequest
		withMockTriggerPresetHub(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/trigger-presets":
				_ = json.NewEncoder(w).Encode([]hubclient.TriggerPreset{schema})
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/trigger-presets/compile":
				_ = json.NewEncoder(w).Encode(compiledResp)
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/triggers":
				require.NoError(t, json.NewDecoder(r.Body).Decode(&seenCreate))
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(hubclient.Trigger{
					ID:      "tr-preset-comment",
					Name:    seenCreate.Name,
					Source:  seenCreate.Source,
					Match:   *seenCreate.Match,
					Action:  seenCreate.Action,
					Subject: *seenCreate.Subject,
					Status:  "active",
				})
			default:
				http.NotFound(w, r)
			}
		}, hubsync.CredentialKindOAuth)

		triggerPresetApplyName = "help-comment"
		triggerPresetApplyFields = []string{"command=help"}
		triggerPresetApplyComment = "Hi @${event.attributes.sender}, commands: /review, /fix"
		triggerPresetApplyReplyInThread = true

		out := captureStdout(t, func() {
			require.NoError(t, runTriggerPresetApply(triggerPresetApplyCmd, []string{"github.command"}))
		})
		assert.Equal(t, "github", seenCreate.Source.Kind)
		assert.Equal(t, "github.comment", seenCreate.Action.Type)
		assert.Equal(t, triggerPresetApplyComment, seenCreate.Action.Body)
		assert.Equal(t, "event", seenCreate.Action.Target)
		assert.True(t, seenCreate.Action.ReplyInThread)
		assert.Equal(t, []string{"event.attributes.sender"}, seenCreate.Action.UntrustedVars)
		assert.Contains(t, out, "Trigger created: tr-preset-comment")
		assert.Contains(t, out, "Action:  github.comment (reply-in-thread)")
	})

	t.Run("preset apply with --dispatch and --ack-comment", func(t *testing.T) {
		resetTriggerPresetFlags(t)

		var seenCreate hubclient.CreateTriggerRequest
		withMockTriggerPresetHub(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/trigger-presets":
				_ = json.NewEncoder(w).Encode([]hubclient.TriggerPreset{schema})
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/trigger-presets/compile":
				_ = json.NewEncoder(w).Encode(compiledResp)
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/triggers":
				require.NoError(t, json.NewDecoder(r.Body).Decode(&seenCreate))
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(hubclient.Trigger{
					ID:      "tr-preset-ack",
					Name:    seenCreate.Name,
					Source:  seenCreate.Source,
					Match:   *seenCreate.Match,
					Action:  seenCreate.Action,
					Subject: *seenCreate.Subject,
					Status:  "active",
				})
			default:
				http.NotFound(w, r)
			}
		}, hubsync.CredentialKindOAuth)

		triggerPresetApplyName = "review-dispatch"
		triggerPresetApplyFields = []string{"command=review"}
		triggerPresetApplyDispatch = true
		triggerPresetApplyDispatchFlags.template = "reviewer"
		triggerPresetApplyDispatchFlags.task = "Review PR"
		triggerPresetApplyDispatchFlags.ackComment = "Started **${result.agent_name}** — [open](${result.agent_url}). Requested by @${event.attributes.sender}."

		out := captureStdout(t, func() {
			require.NoError(t, runTriggerPresetApply(triggerPresetApplyCmd, []string{"github.command"}))
		})
		assert.Equal(t, "dispatch_agent", seenCreate.Action.Type)
		require.NotNil(t, seenCreate.Action.AckComment)
		assert.Equal(t, triggerPresetApplyDispatchFlags.ackComment, seenCreate.Action.AckComment.Body)
		assert.Nil(t, seenCreate.Action.UntrustedVars)
		assert.Contains(t, out, "Trigger created: tr-preset-ack")
		assert.Contains(t, out, "Ack:     Started **${result.agent_name}**")
	})

	t.Run("preset apply rejects --comment on non-github preset", func(t *testing.T) {
		resetTriggerPresetFlags(t)

		whSchema := hubclient.TriggerPreset{
			ID:      "webhook.generic",
			Source:  "webhook",
			Version: 1,
			Title:   "Generic webhook",
		}
		withMockTriggerPresetHub(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/trigger-presets":
				_ = json.NewEncoder(w).Encode([]hubclient.TriggerPreset{whSchema})
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/trigger-presets/compile":
				_ = json.NewEncoder(w).Encode(compiledResp)
			default:
				http.NotFound(w, r)
			}
		}, hubsync.CredentialKindOAuth)

		triggerPresetApplyName = "wh-comment"
		triggerPresetApplyComment = "hello"
		err := runTriggerPresetApply(triggerPresetApplyCmd, []string{"webhook.generic"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--comment requires a github preset")
	})
}

func jsonFieldTags(t *testing.T, typ reflect.Type) map[string]string {
	t.Helper()
	out := make(map[string]string, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out[name] = tag
	}
	return out
}

// TestTriggerActionSpec_ClientServerJSONRoundTripAndParity pins the JSON wire
// contract between hubclient and hub for github.comment and dispatch_agent
// ackComment so field names and tags cannot drift.
func TestTriggerActionSpec_ClientServerJSONRoundTripAndParity(t *testing.T) {
	// 1. Struct tag parity between hubclient and hub.
	assert.Equal(t,
		jsonFieldTags(t, reflect.TypeOf(hub.TriggerActionSpec{})),
		jsonFieldTags(t, reflect.TypeOf(hubclient.TriggerActionSpec{})),
		"hubclient.TriggerActionSpec JSON tags must match hub.TriggerActionSpec",
	)
	assert.Equal(t,
		jsonFieldTags(t, reflect.TypeOf(hub.DispatchAgentAckComment{})),
		jsonFieldTags(t, reflect.TypeOf(hubclient.DispatchAgentAckComment{})),
		"hubclient.DispatchAgentAckComment JSON tags must match hub.DispatchAgentAckComment",
	)

	// 2. Client -> Server strict round-trip for github.comment CreateTriggerRequest
	// and hub.GitHubCommentActionPayload.
	clientCommentReq := hubclient.CreateTriggerRequest{
		Name:   "gh-comment-rt",
		Source: hubclient.TriggerSourceSpec{Kind: "github"},
		Match: &hubclient.TriggerMatchSpec{
			Types: []string{"github.pull_request_review_comment.created"},
			Rules: []hubclient.TriggerMatchRuleSpec{{Dialect: "cel", Expr: "event.attributes.sender_type != 'Bot'"}},
		},
		Action: hubclient.TriggerActionSpec{
			Type:          "github.comment",
			Body:          "Thanks @${event.attributes.sender}!",
			Target:        "event",
			ReplyInThread: true,
			UntrustedVars: []string{"event.attributes.sender"},
		},
	}
	rawCommentReq, err := json.Marshal(clientCommentReq)
	require.NoError(t, err)

	var srvCommentReq hub.CreateTriggerRequest
	dec := json.NewDecoder(bytes.NewReader(rawCommentReq))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&srvCommentReq))
	assert.Equal(t, "github.comment", srvCommentReq.Action.Type)
	assert.Equal(t, clientCommentReq.Action.Body, srvCommentReq.Action.Body)
	assert.Equal(t, "event", srvCommentReq.Action.Target)
	assert.True(t, srvCommentReq.Action.ReplyInThread)
	assert.Equal(t, []string{"event.attributes.sender"}, srvCommentReq.Action.UntrustedVars)

	// Verify the stored GitHubCommentActionPayload matches the action fields.
	storedCommentRaw, err := json.Marshal(hub.GitHubCommentActionPayload{
		Body:          srvCommentReq.Action.Body,
		Target:        srvCommentReq.Action.Target,
		ReplyInThread: srvCommentReq.Action.ReplyInThread,
		UntrustedVars: srvCommentReq.Action.UntrustedVars,
	})
	require.NoError(t, err)
	var clientDecodedComment hubclient.TriggerActionSpec
	decStored := json.NewDecoder(bytes.NewReader(storedCommentRaw))
	decStored.DisallowUnknownFields()
	require.NoError(t, decStored.Decode(&clientDecodedComment))
	assert.Equal(t, clientCommentReq.Action.Body, clientDecodedComment.Body)
	assert.Equal(t, clientCommentReq.Action.Target, clientDecodedComment.Target)
	assert.Equal(t, clientCommentReq.Action.ReplyInThread, clientDecodedComment.ReplyInThread)
	assert.Equal(t, clientCommentReq.Action.UntrustedVars, clientDecodedComment.UntrustedVars)

	// 3. Client -> Server strict round-trip for dispatch_agent with ackComment
	// and Server -> Client TriggerResponse round-trip.
	clientDispatchReq := hubclient.CreateTriggerRequest{
		Name:   "gh-dispatch-ack-rt",
		Source: hubclient.TriggerSourceSpec{Kind: "github"},
		Match: &hubclient.TriggerMatchSpec{
			Types: []string{"github.issue_comment.created"},
			Rules: []hubclient.TriggerMatchRuleSpec{},
		},
		Action: hubclient.TriggerActionSpec{
			Type:          "dispatch_agent",
			Template:      "reviewer",
			Task:          "Review PR #${event.attributes.number}",
			AckComment:    &hubclient.DispatchAgentAckComment{Body: "Started **${result.agent_name}** — [open](${result.agent_url})."},
			UntrustedVars: []string{"event.attributes.number"},
		},
	}
	rawDispatchReq, err := json.Marshal(clientDispatchReq)
	require.NoError(t, err)

	var srvDispatchReq hub.CreateTriggerRequest
	decDispatch := json.NewDecoder(bytes.NewReader(rawDispatchReq))
	decDispatch.DisallowUnknownFields()
	require.NoError(t, decDispatch.Decode(&srvDispatchReq))
	require.NotNil(t, srvDispatchReq.Action.AckComment)
	assert.Equal(t, clientDispatchReq.Action.AckComment.Body, srvDispatchReq.Action.AckComment.Body)

	// Server TriggerResponse -> Client Trigger round-trip.
	srvResp := hub.TriggerResponse{
		ID:        "tr-1",
		ProjectID: "proj-1",
		Name:      "gh-dispatch-ack-rt",
		Source:    hub.TriggerSourceSpec{Kind: "github"},
		Match:     hub.TriggerMatchSpec{Types: []string{"github.issue_comment.created"}, Rules: []hub.TriggerMatchRuleSpec{}},
		Action:    srvDispatchReq.Action,
		Status:    "active",
	}
	rawResp, err := json.Marshal(srvResp)
	require.NoError(t, err)

	var clientTrig hubclient.Trigger
	decResp := json.NewDecoder(bytes.NewReader(rawResp))
	decResp.DisallowUnknownFields()
	require.NoError(t, decResp.Decode(&clientTrig))
	require.NotNil(t, clientTrig.Action.AckComment)
	assert.Equal(t, clientDispatchReq.Action.AckComment.Body, clientTrig.Action.AckComment.Body)
}

func TestTriggerUpdate_DispatchAckCommentPreserveClearAndRederive(t *testing.T) {
	const (
		trigID         = "55555555-5555-5555-5555-555555555555"
		initialAckBody = "Started **${result.agent_name}**"
	)

	runUpdateCase := func(t *testing.T, setFlags func(t *testing.T)) (hubclient.UpdateTriggerRequest, map[string]any) {
		t.Helper()
		resetTriggerGitHubCommentTestFlags(t)

		var (
			gotPatch    hubclient.UpdateTriggerRequest
			gotRawPatch map[string]any
		)
		withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.Method {
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode(hubclient.Trigger{
					ID:                    trigID,
					ProjectID:             "proj-1",
					Name:                  "gh-dispatch-ack",
					AuthorizationRevision: 4,
					Source:                hubclient.TriggerSourceSpec{Kind: "github"},
					Action: hubclient.TriggerActionSpec{
						Type:       "dispatch_agent",
						Template:   "reviewer",
						Task:       "old task",
						AckComment: &hubclient.DispatchAgentAckComment{Body: initialAckBody},
					},
					Status: "active",
				})
			case http.MethodPatch:
				bodyBytes, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(bodyBytes, &gotPatch))
				require.NoError(t, json.Unmarshal(bodyBytes, &gotRawPatch))
				_ = json.NewEncoder(w).Encode(hubclient.Trigger{
					ID:                    trigID,
					ProjectID:             "proj-1",
					Name:                  "gh-dispatch-ack",
					AuthorizationRevision: 5,
					Source:                hubclient.TriggerSourceSpec{Kind: "github"},
					Action:                *gotPatch.Action,
					Status:                "active",
				})
			default:
				http.NotFound(w, r)
			}
		}, hubsync.CredentialKindOAuth)

		origUpdateDispatch := triggerUpdateDispatch
		t.Cleanup(func() {
			triggerUpdateDispatch = origUpdateDispatch
			for _, name := range []string{"task", "ack-comment"} {
				_ = triggerUpdateCmd.Flags().Set(name, "")
				triggerUpdateCmd.Flags().Lookup(name).Changed = false
			}
		})

		setFlags(t)
		_ = captureStdout(t, func() {
			require.NoError(t, runTriggerUpdate(triggerUpdateCmd, []string{trigID}))
		})
		return gotPatch, gotRawPatch
	}

	t.Run("update --task preserves existing ackComment.body", func(t *testing.T) {
		gotPatch, _ := runUpdateCase(t, func(t *testing.T) {
			require.NoError(t, triggerUpdateCmd.Flags().Set("task", "Fix PR #${event.attributes.number}"))
		})
		require.NotNil(t, gotPatch.Action)
		assert.Equal(t, "Fix PR #${event.attributes.number}", gotPatch.Action.Task)
		require.NotNil(t, gotPatch.Action.AckComment)
		assert.Equal(t, initialAckBody, gotPatch.Action.AckComment.Body)
		assert.Nil(t, gotPatch.Action.UntrustedVars)
	})

	t.Run("update --ack-comment empty clears ackComment", func(t *testing.T) {
		gotPatch, gotRawPatch := runUpdateCase(t, func(t *testing.T) {
			require.NoError(t, triggerUpdateCmd.Flags().Set("ack-comment", ""))
		})
		require.NotNil(t, gotPatch.Action)
		assert.Nil(t, gotPatch.Action.AckComment)
		actionMap, ok := gotRawPatch["action"].(map[string]any)
		require.True(t, ok)
		_, hasAckKey := actionMap["ackComment"]
		assert.False(t, hasAckKey, "cleared ackComment must be omitted from PATCH JSON")
	})

	t.Run("update --ack-comment Y updates body and omits untrustedVars for Hub re-derivation", func(t *testing.T) {
		const updatedAckBody = "Started **${result.agent_name}** ([open](${result.agent_url})) for @${event.attributes.sender} on ${event.subject}"
		gotPatch, _ := runUpdateCase(t, func(t *testing.T) {
			require.NoError(t, triggerUpdateCmd.Flags().Set("ack-comment", updatedAckBody))
		})
		require.NotNil(t, gotPatch.Action)
		require.NotNil(t, gotPatch.Action.AckComment)
		assert.Equal(t, updatedAckBody, gotPatch.Action.AckComment.Body)
		assert.Nil(t, gotPatch.Action.UntrustedVars)
	})
}
