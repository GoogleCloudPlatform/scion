package slack

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
)

// OpenAskUserModal opens a Slack modal for free-text response to an ask-user request.
func OpenAskUserModal(client *slackapi.Client, triggerID, requestID, prompt string) {
	title := "Reply to agent"
	if len(title) > 24 {
		title = title[:24]
	}

	modalRequest := slackapi.ModalViewRequest{
		Type:       slackapi.VTModal,
		CallbackID: fmt.Sprintf("ask:modal:%s", requestID),
		Title:      slackapi.NewTextBlockObject("plain_text", title, false, false),
		Submit:     slackapi.NewTextBlockObject("plain_text", "Submit", false, false),
		Close:      slackapi.NewTextBlockObject("plain_text", "Cancel", false, false),
		Blocks: slackapi.Blocks{
			BlockSet: []slackapi.Block{
				slackapi.NewInputBlock(
					"response_block",
					slackapi.NewTextBlockObject("plain_text", "Your response", false, false),
					slackapi.NewTextBlockObject("plain_text", "Type your response...", false, false),
					slackapi.NewPlainTextInputBlockElement(
						slackapi.NewTextBlockObject("plain_text", "Type your response...", false, false),
						"response",
					),
				),
			},
		},
	}

	if _, err := client.OpenView(triggerID, modalRequest); err != nil {
		slog.Error("Failed to open ask-user modal", "request_id", requestID, "error", err)
	}
}

// HandleAskModalSubmit processes a modal submission for an ask-user request.
func HandleAskModalSubmit(
	ctx context.Context,
	client *slackapi.Client,
	store Store,
	deliverInbound func(topic string, msg *messages.StructuredMessage) *hubError,
	callback slackapi.InteractionCallback,
	log *slog.Logger,
) {
	if log == nil {
		log = slog.Default()
	}

	callbackID := callback.View.CallbackID
	parts := strings.SplitN(callbackID, ":", 3)
	if len(parts) < 3 || parts[1] != "modal" {
		log.Warn("Unexpected modal callback_id format", "callback_id", callbackID)
		return
	}
	requestID := parts[2]

	responseText := extractViewStateValue(callback.View.State, "response_block", "response")
	if responseText == "" {
		log.Debug("Empty modal response, ignoring", "request_id", requestID)
		return
	}

	pending, err := store.GetPendingAskUser(ctx, requestID)
	if err != nil {
		log.Error("Failed to get pending ask-user for modal", "request_id", requestID, "error", err)
		return
	}
	if pending == nil {
		log.Warn("Ask-user request not found for modal submit", "request_id", requestID)
		return
	}
	if pending.Responded {
		log.Debug("Ask-user already responded", "request_id", requestID)
		return
	}
	if time.Now().After(pending.ExpiresAt) {
		log.Debug("Ask-user request expired", "request_id", requestID)
		return
	}

	if deliverInbound != nil {
		userID := callback.User.ID
		sender := "slack:" + userID
		if mapping, mapErr := store.GetUserMapping(ctx, userID); mapErr == nil && mapping != nil && mapping.ScionEmail != "" {
			sender = "user:" + mapping.ScionEmail
		}

		topic := projectcompat.AgentTopic(pending.ProjectID, pending.AgentSlug)
		msg := &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "slack",
			ThreadID:  pending.ChannelID,
			Sender:    sender,
			SenderID:  userID,
			Recipient: "agent:" + pending.AgentSlug,
			Msg:       responseText,
			Type:      messages.TypeInstruction,
			Metadata: map[string]string{
				"slack_channel_id": pending.ChannelID,
				"project_id":       pending.ProjectID,
				"ask_request_id":   pending.RequestID,
			},
		}

		if he := deliverInbound(topic, msg); he != nil {
			log.Error("Failed to deliver ask-user modal response",
				"request_id", requestID, "error", he)
			return
		}
	}

	if err := store.MarkAskUserResponded(ctx, requestID); err != nil {
		log.Error("Failed to mark ask-user as responded after modal", "request_id", requestID, "error", err)
	}

	log.Info("Ask-user modal response submitted",
		"request_id", requestID,
		"user", callback.User.ID,
	)
}

func extractViewStateValue(state *slackapi.ViewState, blockID, actionID string) string {
	if state == nil || state.Values == nil {
		return ""
	}
	block, ok := state.Values[blockID]
	if !ok {
		return ""
	}
	action, ok := block[actionID]
	if !ok {
		return ""
	}
	return action.Value
}
