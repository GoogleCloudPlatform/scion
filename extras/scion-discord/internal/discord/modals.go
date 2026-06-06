package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// OpenAskUserModal responds to a component interaction by presenting a modal
// dialog for free-text input. The modal's custom_id encodes the request ID so
// the subsequent submit can be routed back to the correct pending request.
//
// This function MUST be used as the initial interaction response (not after a
// deferred update) because Discord requires InteractionResponseModal to be
// the first response to a component interaction.
func OpenAskUserModal(s *discordgo.Session, i *discordgo.InteractionCreate, requestID, prompt string) {
	title := "Reply to agent"
	// Discord modal title limit is 45 characters.
	if len(title) > 45 {
		title = title[:45]
	}

	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: fmt.Sprintf("ask:modal:%s", requestID),
			Title:    title,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.TextInput{
							CustomID:    "response",
							Label:       "Your response",
							Style:       discordgo.TextInputParagraph,
							Placeholder: "Type your response...",
							Required:    true,
						},
					},
				},
			},
		},
	})
	if err != nil {
		slog.Error("Failed to open ask-user modal", "request_id", requestID, "error", err)
	}
}

// HandleModalSubmit processes a modal submission routed from the broker's
// InteractionModalSubmit handler. It extracts the text value, looks up the
// pending request, delivers the response to the hub, and sends an ephemeral
// confirmation.
func HandleModalSubmit(
	s *discordgo.Session,
	i *discordgo.InteractionCreate,
	store Store,
	deliverInbound func(topic string, msg *messages.StructuredMessage),
	log *slog.Logger,
) {
	if log == nil {
		log = slog.Default()
	}

	data := i.ModalSubmitData()
	customID := data.CustomID

	// Parse custom_id: "ask:modal:<requestID>"
	parts := strings.SplitN(customID, ":", 3)
	if len(parts) < 3 || parts[1] != "modal" {
		log.Warn("Unexpected modal custom_id format", "custom_id", customID)
		respondEphemeral(s, i, "Invalid modal submission.")
		return
	}
	requestID := parts[2]

	// Extract the text value from the modal components.
	responseText := extractModalTextValue(data.Components, "response")
	if responseText == "" {
		respondEphemeral(s, i, "Empty response — no action taken.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pending, err := store.GetPendingAskUser(ctx, requestID)
	if err != nil {
		log.Error("Failed to get pending ask-user for modal", "request_id", requestID, "error", err)
		respondEphemeral(s, i, "Error looking up request. Please try again.")
		return
	}
	if pending == nil {
		respondEphemeral(s, i, "This request has expired or was not found.")
		return
	}
	if pending.Responded {
		respondEphemeral(s, i, "This request has already been answered.")
		return
	}
	if time.Now().After(pending.ExpiresAt) {
		respondEphemeral(s, i, "This request has expired.")
		return
	}

	// Deliver the response to the hub.
	if deliverInbound != nil {
		discordUserID := interactionUserID(i)
		sender := "discord:" + discordUserID
		if mapping, mapErr := store.GetUserMapping(ctx, discordUserID); mapErr == nil && mapping != nil && mapping.ScionEmail != "" {
			sender = "user:" + mapping.ScionEmail
		}

		topic := fmt.Sprintf("scion.project.%s.agent.%s.messages", pending.ProjectID, pending.AgentSlug)
		recipient := "agent:" + pending.AgentSlug

		msg := &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "discord",
			ThreadID:  pending.ChannelID,
			Sender:    sender,
			SenderID:  discordUserID,
			Recipient: recipient,
			Msg:       responseText,
			Type:      messages.TypeInstruction,
			Metadata: map[string]string{
				"discord_channel_id": pending.ChannelID,
				"project_id":        pending.ProjectID,
				"ask_request_id":    pending.RequestID,
			},
		}

		deliverInbound(topic, msg)
	}

	// Mark as responded.
	if err := store.MarkAskUserResponded(ctx, requestID); err != nil {
		log.Error("Failed to mark ask-user as responded after modal", "request_id", requestID, "error", err)
	}

	// Edit the original ask-user message to disable buttons.
	if pending.MessageID != "" && pending.ChannelID != "" {
		truncated := responseText
		runes := []rune(responseText)
		if len(runes) > 100 {
			truncated = string(runes[:97]) + "..."
		}
		editContent := fmt.Sprintf("✅ Responded: %s", truncated)
		empty := []discordgo.MessageComponent{}
		_, editErr := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
			ID:         pending.MessageID,
			Channel:    pending.ChannelID,
			Content:    &editContent,
			Components: &empty,
		})
		if editErr != nil {
			log.Warn("Failed to edit original ask-user message after modal", "error", editErr)
		}
	}

	// Send ephemeral follow-up confirming the response was sent.
	respondEphemeral(s, i, "Response sent.")

	log.Info("Ask-user modal response submitted",
		"request_id", requestID,
		"user", interactionUserID(i),
	)
}

// extractModalTextValue walks the modal's component tree (ActionsRow → TextInput)
// and returns the value of the TextInput with the given customID.
func extractModalTextValue(components []discordgo.MessageComponent, targetCustomID string) string {
	for _, row := range components {
		ar, ok := row.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, comp := range ar.Components {
			input, ok := comp.(*discordgo.TextInput)
			if !ok {
				continue
			}
			if input.CustomID == targetCustomID {
				return input.Value
			}
		}
	}
	return ""
}

// respondEphemeral sends an ephemeral follow-up message after a deferred
// interaction acknowledgment.
func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: content,
		Flags:   discordgo.MessageFlagsEphemeral,
	})
	if err != nil {
		slog.Error("Failed to send ephemeral follow-up", "error", err)
	}
}
