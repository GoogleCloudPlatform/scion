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
	"strings"
	"unicode"
)

// resolveTargetAgents determines which agents a message should be routed to.
// Returns a deduplicated list of agent slugs.
//
// Tier 1: Bot @-mention (@ScionHubBot) → routes to group's default agent
// Tier 2: Direct agent @-mention (@coder) → routes to named agent(s)
// Tier 3: @all → routes to ALL agents in the linked project
//
// If no agent is resolved, returns nil (message should be silently ignored).
func resolveTargetAgents(msg *TGMessage, botUsername string, defaultAgent string, knownAgents []string) []string {
	if msg == nil {
		return nil
	}

	botMentioned := isBotMentioned(msg, botUsername)
	agentMentions, hasAll := extractAgentMentions(msg.Text, knownAgents)

	if hasAll {
		return knownAgents
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
		return nil
	}
	return result
}

// isBotMentioned checks Telegram's structured entities for a mention matching the bot's username.
func isBotMentioned(msg *TGMessage, botUsername string) bool {
	if msg == nil || botUsername == "" {
		return false
	}
	lower := strings.ToLower(botUsername)
	for _, ent := range msg.Entities {
		if ent.Type != "mention" {
			continue
		}
		if ent.Offset < 0 || ent.Offset+ent.Length > len(msg.Text) {
			continue
		}
		mention := msg.Text[ent.Offset : ent.Offset+ent.Length]
		mention = strings.TrimPrefix(mention, "@")
		if strings.ToLower(mention) == lower {
			return true
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

// stripMentions removes @botUsername and @agentSlug mentions from text, returning clean content.
func stripMentions(text string, botUsername string, agentSlugs []string) string {
	remove := make(map[string]bool)
	if botUsername != "" {
		remove[strings.ToLower(botUsername)] = true
	}
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
