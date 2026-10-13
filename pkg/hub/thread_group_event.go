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
	"errors"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// This file holds the thread group JSON payload, the chat.thread_group event
// and the builder that turns store rows into the payload (ptone/scion#4424,
// design sections 2 and 5a). The REST handlers and the migration of personal
// groups share it, so both publish identical events.

// Thread group event actions.
const (
	ThreadGroupEventCreated = "created"
	ThreadGroupEventUpdated = "updated"
	ThreadGroupEventDeleted = "deleted"
)

// ThreadGroupEvent is published on project.<projectID>.chat.thread_group when
// a thread group is created, updated (renamed, or a thread added or removed)
// or deleted. It carries the full group snapshot so subscribers need no REST
// fetch. A deleted event carries the group as it was just before the delete.
type ThreadGroupEvent struct {
	Action string              `json:"action"` // "created", "updated", "deleted"
	Group  ThreadGroupResponse `json:"group"`
}

// ThreadGroupResponse is the JSON shape of a thread group in the
// /api/v1/thread-groups API and in ThreadGroupEvent.
type ThreadGroupResponse struct {
	ID            string    `json:"id"`
	ProjectID     string    `json:"projectId"`
	Name          string    `json:"name"`
	CreatedBy     string    `json:"createdBy"`
	CreatedByKind string    `json:"createdByKind"` // user | agent
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	// Threads is never nil; a group with no live threads has [].
	Threads []ThreadGroupThread `json:"threads"`
}

// ThreadGroupThread is one member thread of a ThreadGroupResponse. ThreadID
// (the webchat topic ID) and Name are resolved at read time. A group
// conversation with no topic (an external surface) has an empty ThreadID and
// takes its Name from the conversation.
type ThreadGroupThread struct {
	ConversationID string    `json:"conversationId"`
	ThreadID       string    `json:"threadId"`
	Name           string    `json:"name"`
	AddedBy        string    `json:"addedBy"`
	AddedAt        time.Time `json:"addedAt"`
}

// PublishChatThreadGroupEvent is a no-op.
func (noopEventPublisher) PublishChatThreadGroupEvent(_ context.Context, _ string, _ string, _ ThreadGroupResponse) {
}

// PublishChatThreadGroupEvent publishes a ThreadGroupEvent on
// project.<projectID>.chat.thread_group. An empty projectID publishes nothing.
func (p *eventBuilder) PublishChatThreadGroupEvent(_ context.Context, projectID string, action string, group ThreadGroupResponse) {
	if projectID == "" {
		return
	}
	p.sink("project."+projectID+".chat.thread_group", ThreadGroupEvent{
		Action: action,
		Group:  group,
	})
}

// threadGroupResponse builds the payload of one thread group. See
// threadGroupResponses.
func (s *Server) threadGroupResponse(ctx context.Context, g *store.ThreadGroup) (ThreadGroupResponse, error) {
	out, err := s.threadGroupResponses(ctx, g.ProjectID, []store.ThreadGroup{*g})
	if err != nil {
		return ThreadGroupResponse{}, err
	}
	return out[0], nil
}

// threadGroupResponses builds the payload of thread groups that all belong to
// projectID. It never returns a nil slice.
//
// Member threads are resolved in a batch: one ListTopics call maps the
// project's live topics by conversation ID. A member with no live topic in
// that map is looked up by conversation ID:
//   - a missing or deleted conversation is skipped;
//   - a thread-backed conversation ("thread:" external_ref) whose topic is
//     gone (deleted, or no longer linked back) is skipped, so a missed
//     cleanup after a topic delete never shows up (design section 7a);
//   - any other conversation (an external surface, section 7h) is returned
//     with an empty ThreadID and its display name.
//
// Archived conversations are returned (section 7a2).
func (s *Server) threadGroupResponses(ctx context.Context, projectID string, groups []store.ThreadGroup) ([]ThreadGroupResponse, error) {
	out := make([]ThreadGroupResponse, 0, len(groups))
	if len(groups) == 0 {
		return out, nil
	}

	topicsByConv := map[string]WebChatTopic{}
	hasMembers := false
	for i := range groups {
		if len(groups[i].Members) > 0 {
			hasMembers = true
			break
		}
	}
	if hasMembers {
		s.mu.RLock()
		wcs := s.webChatStore
		s.mu.RUnlock()
		if wcs != nil {
			topics, err := wcs.ListTopics(ctx, projectID)
			if err != nil {
				return nil, fmt.Errorf("thread groups: list topics: %w", err)
			}
			for _, t := range topics {
				if t.ConversationID != "" {
					topicsByConv[t.ConversationID] = t
				}
			}
		}
	}

	for i := range groups {
		g := &groups[i]
		resp := ThreadGroupResponse{
			ID:            g.ID,
			ProjectID:     g.ProjectID,
			Name:          g.Name,
			CreatedBy:     g.CreatedBy,
			CreatedByKind: g.CreatedByKind,
			CreatedAt:     g.CreatedAt,
			UpdatedAt:     g.UpdatedAt,
			Threads:       make([]ThreadGroupThread, 0, len(g.Members)),
		}
		for _, m := range g.Members {
			th := ThreadGroupThread{
				ConversationID: m.ConversationID,
				AddedBy:        m.AddedBy,
				AddedAt:        m.AddedAt,
			}
			if t, ok := topicsByConv[m.ConversationID]; ok {
				th.ThreadID = t.ID
				th.Name = t.Name
				resp.Threads = append(resp.Threads, th)
				continue
			}
			conv, err := s.store.GetConversation(ctx, m.ConversationID)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("thread groups: get conversation %s: %w", m.ConversationID, err)
			}
			if conv.DeletedAt != nil {
				continue
			}
			if _, _, perr := messaging.ParseThreadConversationExternalRef(conv.ExternalRef); perr == nil {
				// Thread-backed, but its topic is deleted or unlinked.
				continue
			}
			th.Name = conv.DisplayName
			resp.Threads = append(resp.Threads, th)
		}
		out = append(out, resp)
	}
	return out, nil
}
