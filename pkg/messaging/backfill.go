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

package messaging

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// defaultBatchSize is the page size used when scanning messages.
const defaultBatchSize = 100

// BackfillConfig controls the behaviour of a backfill run.
type BackfillConfig struct {
	// DryRun when true computes statistics without making any changes.
	DryRun bool
	// BatchSize is the page size for scanning messages (default 100).
	BatchSize int
	// Checkpoint is the ID of the last message processed in a previous run.
	// When set, only messages created after that message are processed.
	Checkpoint string
	// ProjectID scopes the backfill to a single project.
	ProjectID string
}

// BackfillResult summarises what a backfill run did (or would do in dry-run).
type BackfillResult struct {
	TotalProcessed       int      `json:"totalProcessed"`
	Attributed           int      `json:"attributed"`
	Inferred             int      `json:"inferred"`
	Skipped              int      `json:"skipped"`
	ConversationsCreated int      `json:"conversationsCreated"`
	HazardAEmailCount    int      `json:"hazardAEmailCount"`
	HazardBSlugCount     int      `json:"hazardBSlugCount"`
	LastCheckpoint       string   `json:"lastCheckpoint,omitempty"`
	Errors               []string `json:"errors,omitempty"`
}

// conversationGroup collects messages that belong to the same conversation.
type conversationGroup struct {
	key          string // canonical key used for dedup
	kind         string // "direct" or "group"
	projectID    string
	participants []participant // deduplicated participants
	agentRef     string        // agent reference for DefaultAgent resolution
	messageIDs   []string      // message IDs to stamp
	driftState   string        // computed drift state
	hazardA      bool          // Hazard (a): non-UUID sender/recipient
	hazardB      bool          // Hazard (b): slug-based agent reference
}

// participant represents a conversation participant extracted from a message.
type participant struct {
	kind string // "user" or "agent"
	id   string // UUID (or non-UUID for hazard-a cases)
}

// BackfillService stamps existing messages with Conversation records.
type BackfillService struct {
	convStore store.ConversationStore
	msgStore  store.MessageStore
	agents    AgentLookup
}

// NewBackfillService creates a new BackfillService.
func NewBackfillService(convStore store.ConversationStore, msgStore store.MessageStore, agents AgentLookup) *BackfillService {
	return &BackfillService{
		convStore: convStore,
		msgStore:  msgStore,
		agents:    agents,
	}
}

// Run executes the backfill. It is safe to call multiple times (idempotent)
// and supports resumption via BackfillConfig.Checkpoint.
func (s *BackfillService) Run(ctx context.Context, cfg BackfillConfig) (*BackfillResult, error) {
	if cfg.ProjectID == "" {
		return nil, errors.New("BackfillConfig.ProjectID is required")
	}

	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}

	result := &BackfillResult{}

	// If a checkpoint is provided, resolve it to a time boundary.
	filter := store.MessageFilter{ProjectID: cfg.ProjectID}
	if cfg.Checkpoint != "" {
		cpMsg, err := s.msgStore.GetMessage(ctx, cfg.Checkpoint)
		if err != nil {
			return nil, fmt.Errorf("resolving checkpoint message %s: %w", cfg.Checkpoint, err)
		}
		filter.After = cpMsg.CreatedAt
	}

	// Phase 1: Scan all messages and group by conversation key.
	groups := make(map[string]*conversationGroup)
	cursor := ""

	for {
		page, err := s.msgStore.ListMessages(ctx, filter, store.ListOptions{
			Limit:  cfg.BatchSize,
			Cursor: cursor,
		})
		if err != nil {
			return nil, fmt.Errorf("listing messages: %w", err)
		}

		for i := range page.Items {
			msg := &page.Items[i]
			result.TotalProcessed++

			// Skip broadcasted messages.
			if msg.Broadcasted {
				result.Skipped++
				continue
			}

			// Idempotent: skip messages that already have a conversation.
			if msg.ConversationID != "" {
				result.Skipped++
				continue
			}

			// Track the last message we looked at for checkpointing.
			result.LastCheckpoint = msg.ID

			g := s.groupForMessage(msg, cfg.ProjectID, groups)
			if g == nil {
				result.Errors = append(result.Errors, fmt.Sprintf("message %s: key derivation failed", msg.ID))
				continue
			}
			g.messageIDs = append(g.messageIDs, msg.ID)
		}

		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	// Phase 2: Resolve agent references and determine drift state for groups.
	for _, g := range groups {
		s.resolveGroup(ctx, g, cfg.ProjectID, result)
	}

	// In dry-run mode, compute statistics and return without persisting.
	if cfg.DryRun {
		for _, g := range groups {
			if g.hazardA {
				result.Inferred += len(g.messageIDs)
			} else {
				result.Attributed += len(g.messageIDs)
			}
			result.ConversationsCreated++
		}
		return result, nil
	}

	// Phase 3: Create conversations and stamp messages.
	for _, g := range groups {
		if err := s.persistGroup(ctx, g, result); err != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("group %s: %v", g.key, err))
		}
	}

	return result, nil
}

// groupForMessage finds or creates the conversation group for a message.
// Returns nil when key derivation fails (e.g. malformed dm: key); the caller
// MUST check for nil before appending message IDs.
func (s *BackfillService) groupForMessage(msg *store.Message, projectID string, groups map[string]*conversationGroup) *conversationGroup {
	senderKind, senderID := parsePrincipal(msg.Sender, msg.SenderID)
	recipientKind, recipientID := parsePrincipal(msg.Recipient, msg.RecipientID)

	extRef, derivedKind, _, deriveErr := DeriveConversationKey(KeyInputs{
		ThreadID:      msg.ThreadID,
		ProjectID:     projectID,
		SenderKind:    senderKind,
		SenderID:      senderID,
		RecipientKind: recipientKind,
		RecipientID:   recipientID,
	})
	if deriveErr != nil {
		// Key derivation refused — return nil so the caller skips this message.
		return nil
	}
	key := extRef
	kind := derivedKind

	g, ok := groups[key]
	if !ok {
		g = &conversationGroup{
			key:        key,
			kind:       kind,
			projectID:  projectID,
			driftState: DriftStateActive,
		}
		groups[key] = g
	}

	// Collect participants (deduplicated in addParticipant).
	addParticipant(g, senderKind, senderID)
	addParticipant(g, recipientKind, recipientID)

	// Track the agent reference for DefaultAgent resolution.
	// Prefer the AgentID field, then fall back to a participant that is an agent.
	if msg.AgentID != "" && g.agentRef == "" {
		g.agentRef = msg.AgentID
	} else if senderKind == "agent" && g.agentRef == "" {
		g.agentRef = senderID
	} else if recipientKind == "agent" && g.agentRef == "" {
		g.agentRef = recipientID
	}

	// Hazard (a): check for non-UUID sender/recipient IDs.
	if !isValidUUID(senderID) || !isValidUUID(recipientID) {
		g.hazardA = true
	}

	return g
}

// resolveGroup resolves the default agent reference and sets the drift state.
func (s *BackfillService) resolveGroup(ctx context.Context, g *conversationGroup, projectID string, result *BackfillResult) {
	if g.hazardA {
		// Hazard (a): flag the entire group as inferred.
		g.driftState = DriftStateActive // still create the record
		result.HazardAEmailCount += len(g.messageIDs)
		return
	}

	if g.agentRef == "" {
		return
	}

	// Hazard (b): resolve agent ref using NormalizeAgentRef (D2).
	resolvedID, err := NormalizeAgentRef(ctx, s.agents, projectID, g.agentRef)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Agent was deleted — mark orphaned.
			g.driftState = DriftStateOrphaned
			result.HazardBSlugCount += len(g.messageIDs)
			g.hazardB = true
		} else if errors.Is(err, store.ErrInvalidInput) {
			// Ref is neither UUID nor valid slug — flag as inferred.
			g.hazardA = true // treat same as hazard A for drift
			result.HazardBSlugCount += len(g.messageIDs)
			g.hazardB = true
		} else {
			result.Errors = append(result.Errors,
				fmt.Sprintf("resolving agent ref %q: %v", g.agentRef, err))
		}
		return
	}

	// If the resolved ID differs from the original ref, it was a slug.
	if resolvedID != g.agentRef {
		result.HazardBSlugCount += len(g.messageIDs)
		g.hazardB = true
	}

	g.agentRef = resolvedID
}

// persistGroup creates the conversation and stamps all messages in the group.
func (s *BackfillService) persistGroup(ctx context.Context, g *conversationGroup, result *BackfillResult) error {
	convID := uuid.NewString()

	conv := &store.Conversation{
		ID:          convID,
		Kind:        g.kind,
		Surface:     "native",
		ExternalRef: g.key,
		DriftState:  g.driftState,
	}

	if g.projectID != "" {
		conv.ProjectID = &g.projectID
	}

	// Set DefaultAgentID only if we have a resolved UUID.
	if g.agentRef != "" && isValidUUID(g.agentRef) && !g.hazardA {
		conv.DefaultAgentID = &g.agentRef
	}

	upserted, err := s.convStore.UpsertConversationByExternalRef(ctx, conv)
	if err != nil {
		return fmt.Errorf("upserting conversation: %w", err)
	}

	// Track whether this was a new creation or an existing one.
	if upserted.ID == convID {
		result.ConversationsCreated++
	}
	actualConvID := upserted.ID

	// Add participants (idempotent — AddParticipant handles re-joins).
	for _, p := range g.participants {
		if err := s.convStore.AddParticipant(ctx, &store.ConversationParticipant{
			ID:             uuid.NewString(),
			ConversationID: actualConvID,
			PrincipalKind:  p.kind,
			PrincipalID:    p.id,
			Role:           "member",
		}); err != nil {
			// AddParticipant may return ErrAlreadyExists for re-joins;
			// the ent adapter handles this, but guard against other impls.
			if !errors.Is(err, store.ErrAlreadyExists) {
				result.Errors = append(result.Errors,
					fmt.Sprintf("adding participant %s:%s to %s: %v", p.kind, p.id, actualConvID, err))
			}
		}
	}

	// Stamp messages.
	for _, msgID := range g.messageIDs {
		if err := s.msgStore.SetMessageConversationID(ctx, msgID, actualConvID); err != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("stamping message %s: %v", msgID, err))
			continue
		}

		if g.hazardA {
			result.Inferred++
		} else {
			result.Attributed++
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// parsePrincipal extracts the kind and ID from a message's sender/recipient
// fields. The Sender/Recipient field has format "kind:name" (e.g. "user:alice",
// "agent:code-reviewer"). The SenderID/RecipientID field holds the actual
// identifier (usually a UUID, sometimes an email for legacy data).
func parsePrincipal(label, id string) (kind, principalID string) {
	// Extract kind from the label prefix.
	if idx := strings.Index(label, ":"); idx >= 0 {
		kind = label[:idx]
	} else {
		kind = "user" // default
	}

	// Prefer the explicit ID field; fall back to the name part of the label.
	if id != "" {
		principalID = id
	} else if idx := strings.Index(label, ":"); idx >= 0 {
		principalID = label[idx+1:]
	} else {
		principalID = label
	}

	return kind, principalID
}

// addParticipant adds a participant to the group if not already present.
func addParticipant(g *conversationGroup, kind, id string) {
	for _, p := range g.participants {
		if p.kind == kind && p.id == id {
			return
		}
	}
	g.participants = append(g.participants, participant{kind: kind, id: id})
}

// isValidUUID reports whether s is a valid UUID.
func isValidUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}
