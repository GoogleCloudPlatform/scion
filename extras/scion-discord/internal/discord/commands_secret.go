package discord

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// getSubSubcommandOption extracts a named option value from a sub-subcommand
// interaction (i.e. options nested two levels deep under a subcommand group).
func getSubSubcommandOption(i *discordgo.InteractionCreate, name string) string {
	data := i.ApplicationCommandData()
	if len(data.Options) == 0 || len(data.Options[0].Options) == 0 {
		return ""
	}
	sub := data.Options[0].Options[0] // sub-subcommand
	for _, opt := range sub.Options {
		if opt.Name == name {
			return opt.StringValue()
		}
	}
	return ""
}

// validateSecretKey checks that a key is non-empty and contains no spaces,
// newlines, or equals signs.
func validateSecretKey(key string) error {
	if key == "" {
		return fmt.Errorf("secret key cannot be empty")
	}
	if strings.ContainsAny(key, " \t\n\r=:") {
		return fmt.Errorf("secret key must not contain spaces, tabs, newlines, '=' or ':'")
	}
	return nil
}

// HandleSecretSet opens a modal for the user to enter a secret value.
// This MUST be the first interaction response (no defer) because Discord
// requires InteractionResponseModal as the initial response.
func (h *CommandHandler) HandleSecretSet(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Check user registration.
	discordUserID := interactionUserID(i)
	if discordUserID == "" {
		h.respondModalError(s, i, "Could not identify your user.")
		return
	}
	if _, ok := requirePrincipal(ctx, h.store, h.log, discordUserID, func(msg string) { h.respondModalError(s, i, msg) }); !ok {
		return
	}

	// Resolve channel link for project context.
	link, err := resolveChannelLink(ctx, s, h.store, i.ChannelID)
	if err != nil {
		h.log.Error("Failed to resolve channel link for secret set", "error", err)
		h.respondModalError(s, i, "Something went wrong. Please try again.")
		return
	}
	if link == nil {
		h.respondModalError(s, i, "This channel is not linked to a project. Use `/scion setup` first.")
		return
	}

	// Extract and validate key.
	key := getSubSubcommandOption(i, "key")
	if err := validateSecretKey(key); err != nil {
		h.respondModalError(s, i, fmt.Sprintf("Invalid key: %s", err))
		return
	}

	// Build the modal custom ID. Discord limits custom IDs to 100 chars.
	// Format: "secret:set:<key>:<projectID>"
	customID := fmt.Sprintf("secret:set:%s:%s", key, link.ProjectID)
	if len(customID) > 100 {
		// Key or project ID too long — truncate is not safe. Use ephemeral error.
		h.respondModalError(s, i, "The secret key is too long for this operation. Please use a shorter key name.")
		return
	}

	// Modal title is limited to 45 chars by Discord.
	// Use runes to avoid splitting multi-byte UTF-8 characters.
	title := "Set Secret: " + key
	if runes := []rune(title); len(runes) > 45 {
		title = string(runes[:45])
	}

	err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: customID,
			Title:    title,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.TextInput{
							CustomID:    "secret_value",
							Label:       "Secret Value",
							Style:       discordgo.TextInputParagraph,
							Placeholder: "Enter the secret value...",
							Required:    true,
						},
					},
				},
			},
		},
	})
	if err != nil {
		h.log.Error("Failed to open secret set modal", "error", err, "key", key)
	}
}

// respondModalError sends an immediate ephemeral message for cases where
// the modal cannot be opened (e.g. validation failures before the modal).
func (h *CommandHandler) respondModalError(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: msg,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
	if err != nil {
		h.log.Error("Failed to send modal error response", "error", err)
	}
}

// HandleSecretModalSubmit processes the modal submission for secret set.
// It is called after the broker defers the interaction response.
func (h *CommandHandler) HandleSecretModalSubmit(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ModalSubmitData()

	// Parse customID: "secret:set:<key>:<projectID>"
	parts := strings.SplitN(data.CustomID, ":", 4)
	if len(parts) < 4 || parts[1] != "set" {
		h.followup(s, i, "Invalid modal submission.")
		return
	}
	key := parts[2]
	projectID := parts[3]

	// Extract the secret value from modal components.
	value := extractModalTextValue(data.Components, "secret_value")
	if value == "" {
		h.followup(s, i, "Empty secret value — no action taken.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Look up user mapping for on-behalf-of header.
	discordUserID := interactionUserID(i)
	if discordUserID == "" {
		h.followup(s, i, "Could not identify your user.")
		return
	}
	onBehalfOf, ok := h.requirePrincipal(ctx, s, i)
	if !ok {
		return
	}

	// Call hub API to set the secret.
	err := h.hubClient.SetSecret(ctx, key, value, "project", projectID, onBehalfOf)
	if err != nil {
		h.log.Error("Failed to set secret via hub", "error", err, "key", key, "project_id", projectID)
		h.followup(s, i, secretErrorText(err, onBehalfOf, h.projectSlugFor(ctx, s, i.ChannelID, projectID), "set", key,
			fmt.Sprintf("Failed to set secret **%s**: %s", key, err)))
		return
	}

	h.followup(s, i, fmt.Sprintf("Secret **%s** has been set.", key))
	h.log.Info("Secret set via Discord",
		"key", key, "project_id", projectID, "discord_user", discordUserID)
}

// HandleSecretList lists all secrets in the linked project (metadata only).
func (h *CommandHandler) HandleSecretList(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Check user registration.
	if interactionUserID(i) == "" {
		h.followup(s, i, "Could not identify your user.")
		return
	}
	onBehalfOf, ok := h.requirePrincipal(ctx, s, i)
	if !ok {
		return
	}

	// Resolve channel link.
	link, err := resolveChannelLink(ctx, s, h.store, i.ChannelID)
	if err != nil {
		h.log.Error("Failed to resolve channel link for secret list", "error", err)
		h.followup(s, i, "Something went wrong. Please try again.")
		return
	}
	if link == nil {
		h.followup(s, i, "This channel is not linked to a project. Use `/scion setup` first.")
		return
	}

	secrets, err := h.hubClient.ListSecrets(ctx, "project", link.ProjectID, onBehalfOf)
	if err != nil {
		h.log.Error("Failed to list secrets", "error", err, "project_id", link.ProjectID)
		h.followup(s, i, deniedText(err, emailFromPrincipal(onBehalfOf), link.ProjectSlug, "list secrets", "Failed to list secrets. Please try again later."))
		return
	}

	if len(secrets) == 0 {
		h.followup(s, i, fmt.Sprintf("No secrets found in project **%s**.", link.ProjectSlug))
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**Secrets in %s** (%d):\n", link.ProjectSlug, len(secrets)))
	for _, sec := range secrets {
		line := fmt.Sprintf("- `%s`", sec.Key)
		if sec.Type != "" {
			line += fmt.Sprintf(" (type: %s)", sec.Type)
		}
		if sec.Description != "" {
			line += fmt.Sprintf(" — %s", sec.Description)
		}
		sb.WriteString(line + "\n")
	}

	// Discord messages are limited to 2000 characters. Truncate defensively.
	output := sb.String()
	if len(output) > 1900 {
		output = output[:1900] + "\n... (truncated)"
	}

	h.followup(s, i, output)
}

// HandleSecretGet shows metadata for a single secret (never the value).
func (h *CommandHandler) HandleSecretGet(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Check user registration.
	if interactionUserID(i) == "" {
		h.followup(s, i, "Could not identify your user.")
		return
	}
	onBehalfOf, ok := h.requirePrincipal(ctx, s, i)
	if !ok {
		return
	}

	key := getSubSubcommandOption(i, "key")
	if err := validateSecretKey(key); err != nil {
		h.followup(s, i, fmt.Sprintf("Invalid key: %s", err))
		return
	}

	// Resolve channel link.
	link, err := resolveChannelLink(ctx, s, h.store, i.ChannelID)
	if err != nil {
		h.log.Error("Failed to resolve channel link for secret get", "error", err)
		h.followup(s, i, "Something went wrong. Please try again.")
		return
	}
	if link == nil {
		h.followup(s, i, "This channel is not linked to a project. Use `/scion setup` first.")
		return
	}

	info, err := h.hubClient.GetSecret(ctx, key, "project", link.ProjectID, onBehalfOf)
	if err != nil {
		h.log.Error("Failed to get secret", "error", err, "key", key, "project_id", link.ProjectID)
		h.followup(s, i, secretErrorText(err, onBehalfOf, link.ProjectSlug, "read", key,
			fmt.Sprintf("Failed to get secret **%s**: %s", key, err)))
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**Secret: %s**\n", info.Key))
	if info.Type != "" {
		sb.WriteString(fmt.Sprintf("**Type:** %s\n", info.Type))
	}
	sb.WriteString(fmt.Sprintf("**Scope:** %s\n", info.Scope))
	if info.Description != "" {
		sb.WriteString(fmt.Sprintf("**Description:** %s\n", info.Description))
	}
	if info.Updated != "" {
		sb.WriteString(fmt.Sprintf("**Last updated:** %s\n", info.Updated))
	}
	sb.WriteString(fmt.Sprintf("**Version:** %d\n", info.Version))
	sb.WriteString("_(Secret value is never shown)_")

	h.followup(s, i, sb.String())
}

// HandleSecretDelete deletes a secret from the linked project.
func (h *CommandHandler) HandleSecretDelete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Check user registration.
	discordUserID := interactionUserID(i)
	if discordUserID == "" {
		h.followup(s, i, "Could not identify your user.")
		return
	}
	onBehalfOf, ok := h.requirePrincipal(ctx, s, i)
	if !ok {
		return
	}

	key := getSubSubcommandOption(i, "key")
	if err := validateSecretKey(key); err != nil {
		h.followup(s, i, fmt.Sprintf("Invalid key: %s", err))
		return
	}

	// Resolve channel link.
	link, err := resolveChannelLink(ctx, s, h.store, i.ChannelID)
	if err != nil {
		h.log.Error("Failed to resolve channel link for secret delete", "error", err)
		h.followup(s, i, "Something went wrong. Please try again.")
		return
	}
	if link == nil {
		h.followup(s, i, "This channel is not linked to a project. Use `/scion setup` first.")
		return
	}

	err = h.hubClient.DeleteSecret(ctx, key, "project", link.ProjectID, onBehalfOf)
	if err != nil {
		h.log.Error("Failed to delete secret", "error", err, "key", key, "project_id", link.ProjectID)
		h.followup(s, i, secretErrorText(err, onBehalfOf, link.ProjectSlug, "delete", key,
			fmt.Sprintf("Failed to delete secret **%s**: %s", key, err)))
		return
	}

	h.followup(s, i, fmt.Sprintf("Secret **%s** has been deleted.", key))
	h.log.Info("Secret deleted via Discord",
		"key", key, "project_id", link.ProjectID, "discord_user", discordUserID)
}

// secretErrorText returns the reply for a failed secret request: verb
// ("read", "set", "delete") names what was asked for the secret key. A
// denial that does not say which action was denied names the secret and
// the verb. Other errors get fallback.
func secretErrorText(err error, onBehalfOf, project, verb, key, fallback string) string {
	return deniedText(err, emailFromPrincipal(onBehalfOf), project, fmt.Sprintf("%s the secret **%s**", verb, key), fallback)
}

// projectSlugFor returns the slug of projectID when it is the project the
// channel is linked to, or "" when unknown.
func (h *CommandHandler) projectSlugFor(ctx context.Context, s *discordgo.Session, channelID, projectID string) string {
	link, err := resolveChannelLink(ctx, s, h.store, channelID)
	if err != nil || link == nil || link.ProjectID != projectID {
		return ""
	}
	return link.ProjectSlug
}
