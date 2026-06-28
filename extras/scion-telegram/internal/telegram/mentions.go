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

package telegram

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// resolveTargetAgents determines which agents a message should be routed to.
// Returns a deduplicated list of agent slugs and whether @all was used.
//
// Tier 1: Bot @-mention (@ScionHubBot) → routes to group's default agent
// Tier 2: Direct agent @-mention (@coder) → routes to named agent(s)
// Tier 3: @all → routes to ALL agents in the linked project
//
// If no agent is resolved, returns nil, false (message should be silently ignored).
func resolveTargetAgents(msg *TGMessage, botUsername string, defaultAgent string, knownAgents []string) ([]string, bool) {
	if msg == nil {
		return nil, false
	}

	botMentioned := isBotMentioned(msg, botUsername)
	agentMentions, hasAll := extractAgentMentions(msg.Text, knownAgents)

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

// utf16Extract extracts a substring from s using UTF-16 code unit offset and length,
// as provided by the Telegram Bot API entity fields. BMP characters (< U+10000) count
// as 1 UTF-16 code unit; supplementary-plane characters (>= U+10000, e.g. most emoji)
// count as 2 (a surrogate pair). Returns the extracted substring and true, or ("", false)
// if the offset+length falls outside the string.
func utf16Extract(s string, offset, length int) (string, bool) {
	if offset < 0 || length < 0 {
		return "", false
	}

	var (
		u16pos    int // current position in UTF-16 code units
		byteStart = -1
	)

	for i, r := range s {
		if u16pos == offset {
			byteStart = i
		}
		if byteStart >= 0 && u16pos == offset+length {
			return s[byteStart:i], true
		}
		u16pos += utf16CodeUnits(r)
	}

	// Handle end-of-string: the target range ends exactly at the string boundary.
	if byteStart >= 0 && u16pos == offset+length {
		return s[byteStart:], true
	}
	// Handle zero-length at end-of-string.
	if byteStart < 0 && u16pos == offset && length == 0 {
		return "", true
	}

	return "", false
}

// utf16ByteRange returns the byte start (inclusive) and byte end (exclusive)
// positions in s that correspond to a UTF-16 offset and length.
func utf16ByteRange(s string, offset, length int) (int, int, bool) {
	if offset < 0 || length < 0 {
		return 0, 0, false
	}

	var (
		u16pos    int
		byteStart = -1
	)

	for i, r := range s {
		if u16pos == offset {
			byteStart = i
		}
		if byteStart >= 0 && u16pos == offset+length {
			return byteStart, i, true
		}
		u16pos += utf16CodeUnits(r)
	}

	if byteStart >= 0 && u16pos == offset+length {
		return byteStart, len(s), true
	}

	return 0, 0, false
}

// utf16CodeUnits returns the number of UTF-16 code units needed to represent rune r.
func utf16CodeUnits(r rune) int {
	if r >= 0x10000 {
		return 2 // supplementary plane → surrogate pair
	}
	return 1
}

// isPartialMentionEntity checks whether a Telegram mention entity covers only
// part of a longer token in the message text. This happens when agent names
// contain characters not valid in Telegram usernames (e.g., hyphens): Telegram
// creates a mention entity for just the valid-username prefix (e.g., "@agent"
// for "@agent-dev"). Returns true if the character immediately after the entity
// in the text is a valid agent-slug character (letter, digit, hyphen, or
// underscore), indicating the entity is a partial match. For dots, a lookahead
// is used: "@agent.dev" is partial, but "@agent." at end-of-token is not (the
// dot is sentence punctuation).
func isPartialMentionEntity(text string, entOffset, entLength int) bool {
	_, end, ok := utf16ByteRange(text, entOffset, entLength)
	if !ok || end >= len(text) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(text[end:])
	if r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
		return true
	}
	// Dot followed by a letter or digit indicates a dotted slug (e.g., "agent.dev").
	// A trailing dot (end of string, before space, or before punctuation) is
	// treated as sentence punctuation, not part of the name.
	if r == '.' && end+1 < len(text) {
		nextR, _ := utf8.DecodeRuneInString(text[end+1:])
		return unicode.IsLetter(nextR) || unicode.IsDigit(nextR)
	}
	return false
}

// isBotMentioned checks Telegram's structured entities for a mention matching the bot's username.
// Handles both "mention" entities (@username text) and "text_mention" entities (tapping a user
// from a previous message creates a text_mention with a User object instead of @username text).
func isBotMentioned(msg *TGMessage, botUsername string) bool {
	if msg == nil || botUsername == "" {
		return false
	}
	lower := strings.ToLower(botUsername)
	for _, ent := range msg.Entities {
		switch ent.Type {
		case "mention":
			mention, ok := utf16Extract(msg.Text, ent.Offset, ent.Length)
			if !ok {
				continue
			}
			mention = strings.TrimPrefix(mention, "@")
			if strings.ToLower(mention) == lower {
				return true
			}
		case "text_mention":
			// text_mention is used when tapping a user's name from a previous message.
			// The mention text may not contain @username; identity is in ent.User.
			if ent.User != nil && strings.ToLower(ent.User.Username) == lower {
				return true
			}
		}
	}
	return false
}

// extractAgentMentions scans message text for @<name> tokens matching known agents.
// Returns the list of matched agent slugs and whether @all was found.
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

// stripMentionRx matches @mention tokens for removal by stripMentions.
var stripMentionRx = regexp.MustCompile(`@\S+`)

// stripMentions removes @botUsername and @agentSlug mentions from text, returning clean content.
// Uses regex-based matching to preserve all whitespace and indentation (important for code blocks).
func stripMentions(text string, botUsername string, agentSlugs []string) string {
	remove := make(map[string]bool)
	if botUsername != "" {
		remove[strings.ToLower(botUsername)] = true
	}
	for _, slug := range agentSlugs {
		remove[strings.ToLower(slug)] = true
	}
	remove["all"] = true

	matches := stripMentionRx.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return text
	}

	type span struct{ start, end int }
	var removals []span

	for _, loc := range matches {
		start, end := loc[0], loc[1]
		if start > 0 {
			r, _ := utf8.DecodeLastRuneInString(text[:start])
			if !unicode.IsSpace(r) {
				continue
			}
		}
		word := text[start:end]
		name := word[1:]
		cleaned := strings.TrimRightFunc(name, func(r rune) bool {
			return unicode.IsPunct(r) && r != '_' && r != '-'
		})
		if !remove[strings.ToLower(cleaned)] {
			continue
		}
		trailing := name[len(cleaned):]
		removeStart := start
		removeEnd := end - len(trailing)

		if removeEnd < len(text) {
			r, size := utf8.DecodeRuneInString(text[removeEnd:])
			if unicode.IsSpace(r) && r != '\n' && r != '\r' {
				removeEnd += size
			}
		} else if removeStart > 0 {
			r, size := utf8.DecodeLastRuneInString(text[:removeStart])
			if unicode.IsSpace(r) && r != '\n' && r != '\r' {
				removeStart -= size
			}
		}

		removals = append(removals, span{removeStart, removeEnd})
	}

	if len(removals) == 0 {
		return text
	}

	var b strings.Builder
	pos := 0
	for _, r := range removals {
		if r.start < pos {
			if r.end > pos {
				pos = r.end
			}
			continue
		}
		b.WriteString(text[pos:r.start])
		pos = r.end
	}
	b.WriteString(text[pos:])

	return strings.TrimSpace(b.String())
}

// hasNonBotUserMention returns true if the message starts with (offset=0) a
// mention or text_mention entity pointing to a Telegram user who is neither
// the bot nor a known agent. Mentions embedded later in the message (offset>0)
// are ignored so the message can still route to the default agent.
func hasNonBotUserMention(msg *TGMessage, botUsername string, knownAgents []string) bool {
	if msg == nil || len(msg.Entities) == 0 {
		return false
	}
	lowerBot := strings.ToLower(botUsername)
	agentSet := make(map[string]bool, len(knownAgents))
	for _, a := range knownAgents {
		agentSet[strings.ToLower(a)] = true
	}
	for _, ent := range msg.Entities {
		if ent.Offset != 0 {
			continue
		}
		switch ent.Type {
		case "mention":
			mention, ok := utf16Extract(msg.Text, ent.Offset, ent.Length)
			if !ok {
				continue
			}
			// Skip if the entity is a partial match — Telegram may truncate
			// at a hyphen because hyphens are not valid in Telegram usernames.
			// For example, "@agent-dev" produces an entity covering only "@agent".
			if isPartialMentionEntity(msg.Text, ent.Offset, ent.Length) {
				continue
			}
			username := strings.ToLower(strings.TrimPrefix(mention, "@"))
			if username == "" || username == lowerBot || agentSet[username] {
				continue
			}
			return true
		case "text_mention":
			if ent.User == nil {
				continue
			}
			if botUsername != "" && strings.ToLower(ent.User.Username) == lowerBot {
				continue
			}
			if ent.User.Username != "" && agentSet[strings.ToLower(ent.User.Username)] {
				continue
			}
			return true
		}
	}
	return false
}

// Precompiled patterns for extractAgentFromBotMessage.
var (
	// agentToAgentRe matches the observer format: "👀 🤖 sender → 🤖 recipient 👀"
	agentToAgentRe = regexp.MustCompile(`^(?:\[URGENT\] )?(?:\[Broadcast\] )?👀 🤖 \S+ → 🤖 (\S+) 👀`)
	// standardAgentRe matches the standard format: "🤖 agent-slug" optionally followed by
	// " → @recipient", " [status]", newline, etc. Uses greedy \S+ which stops at first
	// whitespace — correctly handles "🤖 blue → @ptone805 message body".
	standardAgentRe = regexp.MustCompile(`^(?:\[URGENT\] )?(?:\[Broadcast\] )?🤖 (\S+)`)
	// stateChangeCardRe matches state change card headers: "✅ agent-slug — Completed"
	stateChangeCardRe = regexp.MustCompile(`^(?:\[URGENT\] )?(?:\[Broadcast\] )?\S+ (\S+?) — \S`)
)

// extractAgentFromBotMessage parses a FormatMessageV2 output and extracts the
// target agent slug. For agent-to-agent observer messages it returns the
// recipient; for standard messages it returns the agent slug. Returns "" if
// the text does not match any known format.
func extractAgentFromBotMessage(text string) string {
	if text == "" {
		return ""
	}

	// Try agent-to-agent format first (more specific).
	if m := agentToAgentRe.FindStringSubmatch(text); m != nil {
		return m[1]
	}

	// Try standard agent format.
	if m := standardAgentRe.FindStringSubmatch(text); m != nil {
		return m[1]
	}

	// Try state change card format (e.g. "✅ coordinator — Completed").
	if m := stateChangeCardRe.FindStringSubmatch(text); m != nil {
		return m[1]
	}

	return ""
}

// entityMentionSet returns the set of lowercase usernames that appear in
// msg.Entities as "mention" type — these are real Telegram users confirmed by
// the Bot API. Tokens typed as @something that do NOT appear in this set were
// not recognized by Telegram as valid usernames.
func entityMentionSet(msg *TGMessage) map[string]bool {
	set := make(map[string]bool)
	if msg == nil {
		return set
	}
	for _, ent := range msg.Entities {
		if ent.Type != "mention" {
			continue
		}
		mention, ok := utf16Extract(msg.Text, ent.Offset, ent.Length)
		if !ok {
			continue
		}
		username := strings.TrimPrefix(mention, "@")
		if username != "" {
			set[strings.ToLower(username)] = true
		}
	}
	return set
}

// extractUnresolvedMentions returns @mention tokens from the message that do not
// match the bot username or any known agent slug. Used to detect when a user
// explicitly @mentioned something that isn't a known integration target.
func extractUnresolvedMentions(text, botUsername string, knownAgents []string) []string {
	known := make(map[string]bool, len(knownAgents)+1)
	if botUsername != "" {
		known[strings.ToLower(botUsername)] = true
	}
	for _, a := range knownAgents {
		known[strings.ToLower(a)] = true
		known["all"] = true
	}

	var unresolved []string
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
		if !known[lower] && !seen[lower] {
			seen[lower] = true
			unresolved = append(unresolved, name)
		}
	}
	return unresolved
}
