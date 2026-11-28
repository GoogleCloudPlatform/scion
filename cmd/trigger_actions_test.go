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
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func TestTriggerCreateCLI_RepeatableActionGroups(t *testing.T) {
	defer createActionCollector.reset()

	// Parse a two-action command with per-action flags grouped after each --action.
	args := []string{
		"--name", "multi-cli",
		"--webhook", "deploys",
		"--action", "dispatch_agent",
		"--template", "dev",
		"--task", "Review ${event.attributes.repo}",
		"--branch", "main",
		"--action", "message",
		"--agent", "lead",
		"--message", "Dispatched review agent",
		"--wake",
	}
	createActionCollector.reset()
	require.NoError(t, triggerCreateCmd.Flags().Parse(args))
	t.Cleanup(func() {
		createActionCollector.reset()
		triggerCreateCmd.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
		triggerName = ""
		triggerWebhookRef = ""
		triggerAction = "message"
		triggerAgent = ""
		triggerMessage = ""
		triggerWake = false
		triggerDispatch = dispatchAgentFlagValues{}
	})

	req, _, err := buildCreateTriggerRequestWithAllow(false)
	require.NoError(t, err)
	assert.Empty(t, req.Action.Type, "multi-action request must populate Actions, not scalar Action")
	require.Len(t, req.Actions, 2)

	assert.Equal(t, "dispatch_agent", req.Actions[0].Type)
	assert.Equal(t, "dev", req.Actions[0].Template)
	assert.Equal(t, "Review ${event.attributes.repo}", req.Actions[0].Task)
	assert.Equal(t, "main", req.Actions[0].Branch)
	assert.Nil(t, req.Actions[0].UntrustedVars, "untrustedVars should be omitted when --allow is not passed so the Hub derives it")

	assert.Equal(t, "message", req.Actions[1].Type)
	assert.Equal(t, "lead", req.Actions[1].AgentName)
	assert.Equal(t, "Dispatched review agent", req.Actions[1].Message)
	assert.True(t, req.Actions[1].Wake)
}

func TestTriggerCreateCLI_RepeatableActionGroups_GitHubCommentAndAckComment(t *testing.T) {
	defer createActionCollector.reset()

	args := []string{
		"--name", "gh-multi-cli",
		"--github",
		"--type", "github.pull_request_review_comment.created",
		"--action", "dispatch-agent",
		"--template", "dev",
		"--task", "Review PR",
		"--ack-comment", "Started **${result.agent_name}**",
		"--action", "github.comment",
		"--comment", "Noted @${event.attributes.sender}",
		"--reply-in-thread",
	}
	createActionCollector.reset()
	require.NoError(t, triggerCreateCmd.Flags().Parse(args))
	t.Cleanup(func() {
		createActionCollector.reset()
		triggerCreateCmd.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
		triggerName = ""
		triggerGitHubSource = false
		triggerMatchTypes = nil
		triggerAction = "message"
		triggerComment = ""
		triggerReplyInThread = false
		triggerDispatch = dispatchAgentFlagValues{}
	})

	req, _, err := buildCreateTriggerRequestWithFlags(false, true, true, true)
	require.NoError(t, err)
	assert.Empty(t, req.Action.Type)
	require.Len(t, req.Actions, 2)

	assert.Equal(t, "dispatch_agent", req.Actions[0].Type)
	require.NotNil(t, req.Actions[0].AckComment)
	assert.Equal(t, "Started **${result.agent_name}**", req.Actions[0].AckComment.Body)

	assert.Equal(t, "github.comment", req.Actions[1].Type)
	assert.Equal(t, "Noted @${event.attributes.sender}", req.Actions[1].Body)
	assert.True(t, req.Actions[1].ReplyInThread)
	assert.Equal(t, []string{"event.attributes.sender"}, req.Actions[1].UntrustedVars)
}

func TestTriggerGetCLI_NumbersMultipleActions(t *testing.T) {
	now := time.Now().UTC()
	tr := &hubclient.Trigger{
		ID:                    "11111111-2222-4333-8444-555555555555",
		ProjectID:             "proj-1",
		Name:                  "multi-get",
		Status:                "active",
		AuthorizationRevision: 2,
		Source:                hubclient.TriggerSourceSpec{Kind: "webhook", ID: "ep-1"},
		Match:                 hubclient.TriggerMatchSpec{Types: []string{"webhook.received"}},
		Actions: []hubclient.TriggerActionSpec{
			{Type: "dispatch_agent", Template: "dev", Task: "run checks"},
			{Type: "message", AgentName: "lead", Message: "checks started"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	out := captureStdout(t, func() {
		printTriggerDetail(tr)
	})
	assert.Contains(t, out, "Actions:     2")
	assert.Contains(t, out, "1/2 [dispatch_agent]")
	assert.Contains(t, out, "Template:    dev")
	assert.Contains(t, out, "2/2 [message]")
	assert.Contains(t, out, "Agent:       lead")
	assert.Contains(t, out, "Message:     checks started")
}
