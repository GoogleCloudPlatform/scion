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

package messages

import (
	"strings"
	"unicode"
)

// MaxMentionRecipients caps the number of mention recipients to avoid spam.
const MaxMentionRecipients = 10

// MentionResult represents the outcome of a single mention fan-out attempt.
type MentionResult struct {
	Slug   string `json:"slug"`
	Status string `json:"status"`          // "delivered", "not_found", "error"
	Error  string `json:"error,omitempty"` // human-readable reason on failure
}

// ExtractMentions scans message text for @name tokens and returns a deduplicated
// list of mentioned names (without the @ prefix). Trailing punctuation (except
// underscores and hyphens) is stripped from each token.
//
// An @ preceded by a non-whitespace character (e.g. inside an email address like
// user@example.com) is not treated as a mention trigger.
func ExtractMentions(text string) []string {
	var mentions []string
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
		if !seen[lower] {
			seen[lower] = true
			mentions = append(mentions, name)
		}
	}
	return mentions
}

// ParseCCFlag parses a --cc flag value into a slice of agent names.
// Names are comma-separated and whitespace-trimmed.
func ParseCCFlag(cc string) []string {
	if cc == "" {
		return nil
	}
	parts := strings.Split(cc, ",")
	var names []string
	seen := make(map[string]bool)
	for _, p := range parts {
		name := strings.TrimSpace(p)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if !seen[lower] {
			seen[lower] = true
			names = append(names, name)
		}
	}
	return names
}

// AgentInfo holds the minimal agent data needed for mention resolution.
type AgentInfo struct {
	Slug string
	Name string
}

// ResolveMentions validates mention slugs against a set of known agents,
// deduplicates, excludes the primary recipient, and caps at MaxMentionRecipients.
// It returns a MentionResult for each input slug.
func ResolveMentions(mentionNames []string, knownAgents []AgentInfo, primaryRecipientSlug string) []MentionResult {
	if len(mentionNames) == 0 {
		return nil
	}

	// Build lookup map: lowercase slug -> original slug
	lookup := make(map[string]string, len(knownAgents))
	for _, a := range knownAgents {
		lookup[strings.ToLower(a.Slug)] = a.Slug
	}

	primaryLower := strings.ToLower(strings.TrimPrefix(primaryRecipientSlug, "agent:"))

	var results []MentionResult
	seen := make(map[string]bool)
	seen[primaryLower] = true // skip primary recipient
	deliveredCount := 0

	for _, name := range mentionNames {
		lower := strings.ToLower(name)
		if seen[lower] {
			continue
		}
		seen[lower] = true

		slug, ok := lookup[lower]
		if !ok {
			results = append(results, MentionResult{
				Slug:   name,
				Status: "not_found",
				Error:  "no matching agent in this project",
			})
			continue
		}

		if deliveredCount >= MaxMentionRecipients {
			results = append(results, MentionResult{
				Slug:   slug,
				Status: "error",
				Error:  "mention recipient cap reached",
			})
			continue
		}

		results = append(results, MentionResult{
			Slug:   slug,
			Status: "delivered",
		})
		deliveredCount++
	}

	return results
}

// DeliveredSlugs returns only the slugs from results that were successfully delivered.
func DeliveredSlugs(results []MentionResult) []string {
	var slugs []string
	for _, r := range results {
		if r.Status == "delivered" {
			slugs = append(slugs, r.Slug)
		}
	}
	return slugs
}
