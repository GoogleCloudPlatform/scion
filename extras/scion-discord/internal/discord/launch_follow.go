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

package discord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/bwmarrin/discordgo"
)

// createLaunchWaitCap bounds the wait for an asynchronous create, measured
// from when Discord sent the interaction. The interaction token is valid
// for 15 minutes from then; the remaining 2 minutes leave room for the
// bind, the kickoff and the follow-up that the token sends.
const createLaunchWaitCap = 13 * time.Minute

// interactionReceivedAt returns when Discord created the interaction, from
// its snowflake ID, or now when the ID is not a snowflake.
func interactionReceivedAt(i *discordgo.InteractionCreate) time.Time {
	if i != nil && i.Interaction != nil {
		if t, err := discordgo.SnowflakeTimestamp(i.ID); err == nil && t.Unix() > 0 {
			return t
		}
	}
	return time.Now()
}

// SetAsyncCreate sets whether /scion thread opts in to an asynchronous
// create (config async_create). The default is true.
func (h *CommandHandler) SetAsyncCreate(enabled bool) {
	h.asyncCreate = enabled
}

// launchAgentReader is the agent service hubclient.WaitForLaunch polls. It
// implements only Get, through the Discord hub client so each read is
// signed and made for the linked user who created the agent. WaitForLaunch
// calls no other method; the embedded interface is nil.
type launchAgentReader struct {
	hubclient.AgentService
	hub        HubClient
	projectID  string
	onBehalfOf string
}

// Get reads the agent. A hub error is returned as *apiclient.APIError so
// WaitForLaunch can tell not-found, refused and transient answers apart.
func (r launchAgentReader) Get(ctx context.Context, agentID string) (*hubclient.Agent, error) {
	a, err := r.hub.GetAgent(ctx, r.projectID, agentID, r.onBehalfOf)
	var he *HubError
	if errors.As(err, &he) {
		return nil, &apiclient.APIError{StatusCode: he.StatusCode, Code: he.Code, Message: he.Message, Details: he.Details}
	}
	return a, err
}

// followCreateLaunch follows an accepted asynchronous create until the
// agent is running, editing the thread's status message on each launch
// step. The wait ends at the latest at receivedAt plus
// createLaunchWaitCap. It returns true when the agent is running.
// Otherwise it has edited the status message and sent the follow-up, and
// the caller stops: the thread is not bound and no kickoff is sent.
func (h *CommandHandler) followCreateLaunch(s *discordgo.Session, i *discordgo.InteractionCreate, receivedAt time.Time, threadID, statusMsgID, projectID, onBehalfOf string, resp *CreateAgentResponse) bool {
	slug := resp.Slug
	editStatus := func(content string) {
		if statusMsgID == "" {
			return
		}
		if _, err := s.ChannelMessageEdit(threadID, statusMsgID, content); err != nil {
			h.log.Warn("Failed to edit status message while following launch", "error", err, "slug", slug)
		}
	}
	// OnStep reports the create answer's step at once; without one, say
	// the agent is starting.
	if resp.Agent == nil || resp.Agent.Launch == nil || resp.Agent.Launch.Step == "" {
		editStatus(fmt.Sprintf("⏳ Starting agent `%s`…", slug))
	}

	id := resp.ID
	if id == "" {
		id = slug
	}
	ctx, cancel := context.WithDeadline(context.Background(), receivedAt.Add(createLaunchWaitCap))
	defer cancel()
	reader := launchAgentReader{hub: h.hubClient, projectID: projectID, onBehalfOf: onBehalfOf}
	out, err := hubclient.WaitForLaunch(ctx, reader, id, resp.Agent, hubclient.LaunchWaitOptions{
		PollInterval: h.launchPollInterval,
		OnStep: func(step string) {
			editStatus(fmt.Sprintf("⏳ Starting agent `%s`… (`%s`)", slug, step))
		},
	})

	switch {
	case err != nil:
		reason := launchWaitErrorText(err, time.Since(receivedAt))
		h.log.Warn("Stopped following agent launch", "error", err, "agent_id", id, "slug", slug)
		editStatus(fmt.Sprintf("⚠️ Stopped waiting for agent `%s` to start: %s.", slug, reason))
		h.followup(s, i, fmt.Sprintf(
			"Agent **%s** was accepted, but I stopped waiting for it to start: %s.\n"+
				"Check it with `/scion status`. Once it is running, run `/scion default %s` in the thread to bind it.",
			slug, reason, slug))
		return false
	case !out.Running:
		failure := launchFailureText(out)
		h.log.Warn("Agent launch failed", "slug", slug, "phase", out.Phase, "code", out.Code)
		editStatus(fmt.Sprintf("❌ Agent `%s` failed to start%s", slug, failure))
		h.followup(s, i, fmt.Sprintf("Agent **%s** was created but failed to start%s", slug, failure))
		return false
	}
	return true
}

// launchWaitErrorText describes, for Discord, why a launch wait ended
// without an outcome. The raw error (with the agent ID) goes to the log
// only. waited is the time since the interaction was received.
func launchWaitErrorText(err error, waited time.Duration) string {
	var rejected *hubclient.LaunchStatusRejectedError
	var unreadable *hubclient.LaunchStatusUnreadableError
	switch {
	case errors.Is(err, hubclient.ErrLaunchWaitTimeout):
		return "its launch is still running on the hub"
	case errors.Is(err, hubclient.ErrAgentDeletedDuringLaunch):
		return "the agent was deleted while it was starting"
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("I stopped waiting after %d min", int(waited.Round(time.Minute)/time.Minute))
	case errors.As(err, &rejected), errors.As(err, &unreadable):
		return "the hub refused the status request"
	default:
		return "its status could not be read"
	}
}

// launchFailureText describes a failed launch outcome as a suffix:
// " (`code`, cause: `cause`): message".
func launchFailureText(out hubclient.LaunchOutcome) string {
	label := out.Code
	if label == "" {
		label = "phase: " + out.Phase
	}
	text := " (`" + label + "`"
	if out.Agent != nil && out.Agent.Launch != nil && out.Agent.Launch.Cause != "" {
		text += ", cause: `" + out.Agent.Launch.Cause + "`"
	}
	text += ")"
	if out.Message != "" {
		text += ": " + out.Message
	}
	return text
}
