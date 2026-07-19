package discord

import (
	"strings"
	"unicode"

	"github.com/bwmarrin/discordgo"
)

// resolveTargetAgents determines which agents a message should be routed to.
// Returns a deduplicated list of agent slugs and whether @all was used.
//
// Three-tier routing:
//
//	Tier 1: Bot @-mention → routes to group's default agent
//	Tier 2: Direct agent @-mention (@coder) → routes to named agent(s)
//	Tier 3: @all → routes to ALL agents in the linked project
//
// If no agent is resolved, returns (nil, false) — the message should be
// silently ignored.
func resolveTargetAgents(msg *discordgo.MessageCreate, botUserID string, defaultAgent string, knownAgents []string) ([]string, bool) {
	if msg == nil || msg.Message == nil {
		return nil, false
	}

	botMentioned := isBotMentioned(msg, botUserID)
	agentMentions, hasAll := extractAgentMentions(msg.Content, knownAgents)

	if hasAll {
		return knownAgents, true
	}

	seen := make(map[string]bool)
	var result []string

	if botMentioned && defaultAgent != "" {
		seen[defaultAgent] = true
		result = append(result, defaultAgent)
	}

	for _, agent := range agentMentions {
		if !seen[agent] {
			seen[agent] = true
			result = append(result, agent)
		}
	}

	if len(result) == 0 {
		return nil, false
	}
	return result, false
}

// isBotMentioned checks if the bot user is in the message's Mentions slice.
// Uses Discord's structured mention data rather than text parsing.
func isBotMentioned(msg *discordgo.MessageCreate, botUserID string) bool {
	if msg == nil || msg.Message == nil || botUserID == "" {
		return false
	}
	for _, mention := range msg.Mentions {
		if mention.ID == botUserID {
			return true
		}
	}
	return false
}

// extractAgentMentions scans message text for @name tokens matching known agents.
// Returns matched agents and whether @all was found.
func extractAgentMentions(text string, knownAgents []string) (agents []string, hasAll bool) {
	known := make(map[string]bool, len(knownAgents))
	for _, a := range knownAgents {
		known[strings.ToLower(a)] = true
	}

	seen := make(map[string]bool)
	for _, word := range strings.Fields(text) {
		if !strings.HasPrefix(word, "@") {
			continue
		}
		name := strings.TrimPrefix(word, "@")
		name = strings.TrimRightFunc(name, func(r rune) bool {
			return unicode.IsPunct(r) && r != '_' && r != '-'
		})
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if lower == "all" {
			return nil, true
		}
		if known[lower] && !seen[lower] {
			seen[lower] = true
			// Use the original-case slug from knownAgents.
			for _, a := range knownAgents {
				if strings.ToLower(a) == lower {
					agents = append(agents, a)
					break
				}
			}
		}
	}
	return agents, false
}

// stripMentions removes bot mentions (<@BOT_ID> and <@!BOT_ID>) and agent
// @mentions from text, returning clean content for delivery to agents.
func stripMentions(text string, botUserID string, agentSlugs []string) string {
	// Remove Discord-format bot mentions: <@BOT_ID> and <@!BOT_ID>
	if botUserID != "" {
		text = strings.ReplaceAll(text, "<@"+botUserID+">", "")
		text = strings.ReplaceAll(text, "<@!"+botUserID+">", "")
	}

	remove := make(map[string]bool)
	for _, slug := range agentSlugs {
		remove[strings.ToLower(slug)] = true
	}
	remove["all"] = true

	var parts []string
	for _, word := range strings.Fields(text) {
		if !strings.HasPrefix(word, "@") {
			parts = append(parts, word)
			continue
		}
		name := strings.TrimPrefix(word, "@")
		cleaned := strings.TrimRightFunc(name, func(r rune) bool {
			return unicode.IsPunct(r) && r != '_' && r != '-'
		})
		if remove[strings.ToLower(cleaned)] {
			trailing := name[len(cleaned):]
			if trailing != "" {
				parts = append(parts, trailing)
			}
			continue
		}
		parts = append(parts, word)
	}
	return strings.Join(parts, " ")
}

// extractUnresolvedMentions finds @tokens in text that don't match known agents,
// the bot mention format (<@ID>), or @all. Used for error feedback when a user
// misspells an agent name.
func extractUnresolvedMentions(text string, botUserID string, knownAgents []string) []string {
	known := make(map[string]bool, len(knownAgents)+1)
	for _, a := range knownAgents {
		known[strings.ToLower(a)] = true
	}
	known["all"] = true

	var unresolved []string
	seen := make(map[string]bool)
	for _, word := range strings.Fields(text) {
		if !strings.HasPrefix(word, "@") {
			continue
		}
		// Skip Discord-format bot mentions: <@BOT_ID> or <@!BOT_ID>
		if strings.HasPrefix(word, "<@") && strings.HasSuffix(word, ">") {
			continue
		}
		name := strings.TrimPrefix(word, "@")
		name = strings.TrimRightFunc(name, func(r rune) bool {
			return unicode.IsPunct(r) && r != '_' && r != '-'
		})
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if !known[lower] && !seen[lower] {
			seen[lower] = true
			unresolved = append(unresolved, name)
		}
	}
	return unresolved
}

// Mention represents a classified @mention in a message.
type Mention struct {
	Name     string // agent slug or username (without @)
	Kind     string // "agent", "user", "unknown"
	Identity string // resolved: "agent:agent-a", "user:email@example.com", "unknown"
}

// ClassifiedMentions holds the result of position-aware mention parsing.
type ClassifiedMentions struct {
	StartMentions []Mention // @mentions at the beginning of the message (primary recipients)
	BodyMentions  []Mention // @mentions in the body of the message (mention notifications)
	StrippedBody  string    // message text with start mentions removed, body mentions left in
}

// maxBodyMentions caps the number of body mentions to avoid spam.
const maxBodyMentions = 5

// classifyMentions performs position-aware classification of @mentions in text.
//
// It separates leading @mentions (start mentions used for routing) from
// @mentions embedded in the body (used for mention notifications).
// Discord bot mentions (<@ID> format) are skipped — they are handled separately.
// If @all is found at the start, returns empty ClassifiedMentions.
func classifyMentions(text string, botUserID string, knownAgents []string, userResolver func(username string) (email string, found bool)) ClassifiedMentions {
	tokens := strings.Fields(text)
	if len(tokens) == 0 {
		return ClassifiedMentions{}
	}

	known := make(map[string]string, len(knownAgents))
	for _, a := range knownAgents {
		known[strings.ToLower(a)] = a
	}

	// classifyToken resolves a single @token into a Mention.
	// Returns the Mention and true if resolved, or zero Mention and false if
	// the token should be skipped (bot mention, empty name, unknown non-agent).
	classifyToken := func(token string) (Mention, bool) {
		// Skip Discord bot mentions: <@ID> or <@!ID>
		if strings.HasPrefix(token, "<@") && strings.HasSuffix(token, ">") {
			return Mention{}, false
		}
		if !strings.HasPrefix(token, "@") {
			return Mention{}, false
		}

		name := strings.TrimPrefix(token, "@")
		name = strings.TrimRightFunc(name, func(r rune) bool {
			return unicode.IsPunct(r) && r != '_' && r != '-'
		})
		if name == "" {
			return Mention{}, false
		}

		lower := strings.ToLower(name)
		if slug, ok := known[lower]; ok {
			return Mention{Name: slug, Kind: "agent", Identity: "agent:" + slug}, true
		}
		if userResolver != nil {
			if email, found := userResolver(name); found {
				return Mention{Name: name, Kind: "user", Identity: "user:" + email}, true
			}
		}
		return Mention{Name: name, Kind: "unknown", Identity: "unknown"}, true
	}

	// Phase 1: scan leading tokens for start mentions.
	var startMentions []Mention
	var startTokenCount int

	for _, token := range tokens {
		// Skip Discord bot mentions at the start — they don't count as
		// @agent mentions and shouldn't break the leading-mention scan.
		if strings.HasPrefix(token, "<@") && strings.HasSuffix(token, ">") {
			startTokenCount++
			continue
		}
		if !strings.HasPrefix(token, "@") {
			break // first non-mention token ends the start region
		}

		name := strings.TrimPrefix(token, "@")
		name = strings.TrimRightFunc(name, func(r rune) bool {
			return unicode.IsPunct(r) && r != '_' && r != '-'
		})
		if name == "" {
			break
		}

		// @all at start → return empty, let broadcast logic handle it.
		if strings.ToLower(name) == "all" {
			return ClassifiedMentions{}
		}

		m, ok := classifyToken(token)
		if ok {
			startMentions = append(startMentions, m)
		}
		startTokenCount++
	}

	// Build StrippedBody by dropping the leading mention tokens.
	// Use byte-offset preservation to avoid destroying formatting.
	var strippedBody string
	if startTokenCount < len(tokens) {
		idx := 0
		tokenCount := 0
		runes := []rune(text)
		for idx < len(runes) {
			// Skip whitespace.
			for idx < len(runes) && unicode.IsSpace(runes[idx]) {
				idx++
			}
			if idx >= len(runes) {
				break
			}
			if tokenCount == startTokenCount {
				break
			}
			// Skip non-whitespace.
			for idx < len(runes) && !unicode.IsSpace(runes[idx]) {
				idx++
			}
			tokenCount++
		}
		if idx < len(runes) {
			byteOffset := len(string(runes[:idx]))
			strippedBody = text[byteOffset:]
		}
	}
	bodyTokens := tokens[startTokenCount:]

	// Phase 2: scan body tokens for @mentions.
	seen := make(map[string]bool, len(startMentions))
	for _, sm := range startMentions {
		seen[strings.ToLower(sm.Name)] = true
	}

	var bodyMentions []Mention
	for _, token := range bodyTokens {
		if len(bodyMentions) >= maxBodyMentions {
			break
		}
		m, ok := classifyToken(token)
		if !ok || m.Kind == "unknown" {
			continue
		}
		// Deduplicate: skip if already seen (start mention or earlier body mention).
		lower := strings.ToLower(m.Name)
		if seen[lower] {
			continue
		}
		seen[lower] = true
		bodyMentions = append(bodyMentions, m)
	}

	return ClassifiedMentions{
		StartMentions: startMentions,
		BodyMentions:  bodyMentions,
		StrippedBody:  strippedBody,
	}
}

// agentFromReply extracts the agent slug from a referenced message.
// When a user replies to a webhook message, the webhook username IS the agent
// slug (since the Discord plugin uses per-agent webhooks with the agent slug
// as the webhook username). When replying to a regular bot API message,
// returns "" because the bot's own messages don't carry agent identity in
// the username.
func agentFromReply(ref *discordgo.Message, botUserID string) string {
	if ref == nil {
		return ""
	}

	// Webhook messages have WebhookID set and the Author.Username is the
	// agent slug (set when the webhook message was sent).
	if ref.WebhookID != "" && ref.Author != nil {
		return ref.Author.Username
	}

	// Regular bot API messages — cannot determine which agent sent them
	// from the message metadata alone. The bot user's username is the bot
	// name, not the agent slug.
	return ""
}
