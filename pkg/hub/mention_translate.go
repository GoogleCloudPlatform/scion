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

package hub

import "strings"

// translateMentionsOutbound translates human-readable @firstname-lastname
// mentions into @email format for agent consumption. This is called when a
// human sends a message that is routed to an agent, so the agent sees
// canonical email identifiers instead of display-name slugs.
func translateMentionsOutbound(content string, members []chatMemberEntry) string {
	for _, m := range members {
		if m.Kind != "user" || m.Email == "" || m.DisplayName == "" {
			continue
		}
		slug := strings.ToLower(strings.ReplaceAll(m.DisplayName, " ", "-"))
		if slug == "" {
			continue
		}
		content = replaceMention(content, slug, m.Email)
	}
	return content
}

// translateMentionsInbound translates @email mentions from agent messages into
// human-readable @firstname-lastname format. This is called when an agent
// sends a message to a human thread, so the stored message renders correctly
// in the native chat UI.
func translateMentionsInbound(content string, members []chatMemberEntry) string {
	for _, m := range members {
		if m.Kind != "user" || m.Email == "" || m.DisplayName == "" {
			continue
		}
		slug := strings.ToLower(strings.ReplaceAll(m.DisplayName, " ", "-"))
		if slug == "" {
			continue
		}
		content = replaceMention(content, m.Email, slug)
	}
	return content
}

// replaceMention replaces @oldName with @newName at word boundaries in content.
// The match is case-insensitive. A word boundary is defined as start-of-string
// or whitespace before the @, and end-of-string, whitespace, or common
// punctuation after the mention name. Hyphens and @ are NOT treated as word
// boundaries after the mention, preventing partial matches like @john-smith
// matching inside @john-smith-jones, and @user matching inside @user@example.com.
func replaceMention(content, oldName, newName string) string {
	target := "@" + strings.ToLower(oldName)
	replacement := "@" + newName
	lower := strings.ToLower(content)

	var result strings.Builder
	result.Grow(len(content))
	pos := 0

	for {
		idx := strings.Index(lower[pos:], target)
		if idx < 0 {
			result.WriteString(content[pos:])
			break
		}
		absIdx := pos + idx
		end := absIdx + len(target)

		// Check word boundary before the mention: must be at start of
		// string or preceded by whitespace.
		boundaryBefore := absIdx == 0
		if !boundaryBefore {
			prev := content[absIdx-1]
			boundaryBefore = prev == ' ' || prev == '\n' || prev == '\t' || prev == '\r'
		}

		// Check word boundary after the mention: must be at end of string
		// or followed by whitespace / common punctuation. Hyphens and @
		// are intentionally excluded so @john-smith does not match inside
		// @john-smith-jones and @user does not match inside @user@example.com.
		boundaryAfter := end >= len(content)
		if !boundaryAfter {
			next := content[end]
			boundaryAfter = next == ' ' || next == '\n' || next == '\t' || next == '\r' ||
				next == ',' || next == '.' || next == '!' || next == '?' ||
				next == ')' || next == '(' || next == ':' || next == ';' ||
				next == '"' || next == '\''
		}

		if boundaryBefore && boundaryAfter {
			result.WriteString(content[pos:absIdx])
			result.WriteString(replacement)
		} else {
			result.WriteString(content[pos:end])
		}
		pos = end
	}

	return result.String()
}
