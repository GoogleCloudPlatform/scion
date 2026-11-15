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

import (
	"context"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func mentionDisplaySlug(displayName string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(displayName))), "-")
}

// translateMentionsOutbound translates human-readable @firstname-lastname
// mentions into @email format for agent consumption. This is called when a
// human sends a message that is routed to an agent, so the agent sees
// canonical email identifiers instead of display-name slugs.
func translateMentionsOutbound(content string, members []chatMemberEntry) string {
	if !strings.Contains(content, "@") {
		return content
	}
	for _, m := range members {
		if m.Kind != "user" || m.Email == "" || m.DisplayName == "" {
			continue
		}
		slug := mentionDisplaySlug(m.DisplayName)
		if slug == "" {
			continue
		}
		content = replaceMention(content, slug, m.Email)
	}
	return content
}

// translateMentionsInbound translates @email mentions into human-readable
// @firstname-lastname format for known human members.
func translateMentionsInbound(content string, members []chatMemberEntry) string {
	if !strings.Contains(content, "@") {
		return content
	}
	for _, m := range members {
		if m.Kind != "user" || m.Email == "" || m.DisplayName == "" {
			continue
		}
		slug := mentionDisplaySlug(m.DisplayName)
		if slug == "" {
			continue
		}
		content = replaceMention(content, m.Email, slug)
	}
	return content
}

// translateKnownEmailMentionsInbound rewrites standalone @<email> tokens in
// prose to @<display-slug> when <email> resolves to a known Scion user — first
// checking pre-resolved project members and falling back to GetUserByEmail for
// any remaining @<email> tokens (such as Hub users without an explicit project
// binding, or hub-scoped / user-DM threads without a project). Unknown emails,
// plain prose emails without a leading '@', and code spans / fenced blocks /
// '>' blockquotes are left unchanged (ptone/scion#8133).
func (s *Server) translateKnownEmailMentionsInbound(ctx context.Context, content string, members []chatMemberEntry) string {
	if !strings.Contains(content, "@") {
		return content
	}
	if len(members) > 0 {
		content = translateMentionsInbound(content, members)
	}
	if s == nil || s.store == nil {
		return content
	}
	checked := make(map[string]bool, len(members))
	for _, m := range members {
		if m.Kind == "user" && m.Email != "" {
			checked[strings.ToLower(strings.TrimSpace(m.Email))] = true
		}
	}
	var extra []chatMemberEntry
	for _, token := range messages.ExtractProseWrappedMentions(content) {
		if !strings.Contains(token, "@") {
			continue
		}
		lower := strings.ToLower(strings.TrimSpace(token))
		if lower == "" || checked[lower] {
			continue
		}
		checked[lower] = true
		u, err := s.store.GetUserByEmail(ctx, token)
		if err != nil || u == nil || u.DisplayName == "" {
			continue
		}
		extra = append(extra, chatMemberEntry{
			ID:          u.ID,
			Kind:        "user",
			DisplayName: u.DisplayName,
			Email:       u.Email,
		})
	}
	if len(extra) > 0 {
		content = translateMentionsInbound(content, extra)
	}
	return content
}

// replaceMention replaces standalone @oldName mention tokens with @newName in
// prose, skipping fenced code blocks, inline backtick code spans, and '>'
// blockquote lines.
func replaceMention(content, oldName, newName string) string {
	return messages.ReplaceProseMention(content, oldName, newName)
}
