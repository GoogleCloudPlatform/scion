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

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// DMMigrationConfig controls the behaviour of a DM migration run.
type DMMigrationConfig struct {
	// DryRun when true computes statistics without making any changes.
	DryRun bool
	// BatchSize is the page size for scanning conversations (default 100).
	BatchSize int
}

// DMMigrationResult summarises what a DM migration run did (or would do).
type DMMigrationResult struct {
	TotalScanned      int      // total direct conversations examined
	ParticipantsAdded int      // step 2: participants derived from key
	EmptyRefMerged    int      // step 3a: empty-ref rows merged with existing
	EmptyRefRekeyed   int      // step 3a: empty-ref rows re-keyed in place
	OldFormatRekeyed  int      // step 3b: old dm:X:Y rows re-keyed
	Unparseable       int      // rows that could not be processed
	Ambiguous         int      // IDs found in neither or both tables
	Errors            []string // non-fatal errors encountered
}

// DMMigrationStore is the interface required by the DM migration service.
// It is satisfied by store.Store which embeds all sub-stores.
type DMMigrationStore interface {
	// ConversationStore methods
	GetConversation(ctx context.Context, id string) (*store.Conversation, error)
	UpdateConversation(ctx context.Context, conv *store.Conversation) error
	DeleteConversation(ctx context.Context, id string) error
	ListConversations(ctx context.Context, filter store.ConversationFilter, opts store.ListOptions) (*store.ListResult[store.Conversation], error)
	GetConversationByExternalRef(ctx context.Context, surface, externalRef string) (*store.Conversation, error)
	AddParticipant(ctx context.Context, p *store.ConversationParticipant) error
	ListParticipants(ctx context.Context, conversationID string) ([]store.ConversationParticipant, error)

	// User/Agent lookup methods
	GetUser(ctx context.Context, id string) (*store.User, error)
	GetAgent(ctx context.Context, id string) (*store.Agent, error)

	// Message re-stamping
	ListMessages(ctx context.Context, filter store.MessageFilter, opts store.ListOptions) (*store.ListResult[store.Message], error)
	SetMessageConversationID(ctx context.Context, messageID, conversationID string) error
}

// DMMigrationService migrates historical DM conversation rows to use
// kind-encoded keys (dm:<kind>:<uuid>:<kind>:<uuid>). It handles three
// categories of old rows:
//
//  1. Kind-encoded rows that may lack participants (listing-index rebuild)
//  2. Empty external_ref rows (merge with existing or re-key in place)
//  3. Old-format dm:{sorted(id1,id2)} rows without kind encoding (re-key)
type DMMigrationService struct {
	store DMMigrationStore
}

// NewDMMigrationService creates a new DMMigrationService.
func NewDMMigrationService(s DMMigrationStore) *DMMigrationService {
	return &DMMigrationService{store: s}
}

// Run executes the DM migration. It is safe to call multiple times (idempotent).
func (s *DMMigrationService) Run(ctx context.Context, cfg DMMigrationConfig) (*DMMigrationResult, error) {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}

	result := &DMMigrationResult{}

	// Collect all direct conversations first, then process.
	convs, err := s.collectDirectConversations(ctx, cfg.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("collecting direct conversations: %w", err)
	}

	for i := range convs {
		conv := &convs[i]
		result.TotalScanned++

		switch s.classifyConversation(conv) {
		case convClassKindEncoded:
			s.stepRebuildParticipants(ctx, conv, cfg.DryRun, result)
		case convClassEmptyRef:
			s.stepMergeOrRekeyEmptyRef(ctx, conv, cfg.DryRun, result)
		case convClassOldFormat:
			s.stepRekeyOldFormat(ctx, conv, cfg.DryRun, result)
		default:
			// Unknown format — record but skip.
			result.Unparseable++
		}
	}

	return result, nil
}

// convClass represents the classification of a conversation row.
type convClass int

const (
	convClassUnknown     convClass = iota
	convClassKindEncoded           // ParseDMKey succeeds
	convClassEmptyRef              // external_ref == ""
	convClassOldFormat             // starts with "dm:" but ParseDMKey fails
)

// classifyConversation determines which migration step applies.
func (s *DMMigrationService) classifyConversation(conv *store.Conversation) convClass {
	if conv.ExternalRef == "" {
		return convClassEmptyRef
	}

	if strings.HasPrefix(conv.ExternalRef, "dm:") {
		_, _, _, _, err := messages.ParseDMKey(conv.ExternalRef)
		if err == nil {
			return convClassKindEncoded
		}
		return convClassOldFormat
	}

	return convClassUnknown
}

// collectDirectConversations paginates through all direct conversations.
func (s *DMMigrationService) collectDirectConversations(ctx context.Context, batchSize int) ([]store.Conversation, error) {
	var all []store.Conversation
	cursor := ""

	for {
		page, err := s.store.ListConversations(ctx, store.ConversationFilter{
			Kind: "direct",
		}, store.ListOptions{
			Limit:  batchSize,
			Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}

		all = append(all, page.Items...)

		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	return all, nil
}

// ---------------------------------------------------------------------------
// Step 2: Listing-index rebuild (kind-encoded rows missing participants)
// ---------------------------------------------------------------------------

func (s *DMMigrationService) stepRebuildParticipants(
	ctx context.Context,
	conv *store.Conversation,
	dryRun bool,
	result *DMMigrationResult,
) {
	kindA, idA, kindB, idB, err := messages.ParseDMKey(conv.ExternalRef)
	if err != nil {
		result.Unparseable++
		result.Errors = append(result.Errors,
			fmt.Sprintf("step2: parse key %q: %v", conv.ExternalRef, err))
		return
	}

	// Verify both principals exist in their claimed tables.
	if !s.verifyPrincipal(ctx, kindA, idA) || !s.verifyPrincipal(ctx, kindB, idB) {
		result.Unparseable++
		return
	}

	if dryRun {
		// In dry-run, count what would be added.
		existing, _ := s.store.ListParticipants(ctx, conv.ID)
		needed := s.countMissingParticipants(existing, kindA, idA, kindB, idB)
		result.ParticipantsAdded += needed
		return
	}

	// Add participants (idempotent — ErrAlreadyExists is expected for existing).
	added := 0
	for _, p := range []struct{ kind, id string }{{kindA, idA}, {kindB, idB}} {
		err := s.store.AddParticipant(ctx, &store.ConversationParticipant{
			ID:             uuid.NewString(),
			ConversationID: conv.ID,
			PrincipalKind:  p.kind,
			PrincipalID:    p.id,
			Role:           "member",
		})
		if err == nil {
			added++
		} else if !errors.Is(err, store.ErrAlreadyExists) {
			result.Errors = append(result.Errors,
				fmt.Sprintf("step2: add participant %s:%s to %s: %v", p.kind, p.id, conv.ID, err))
		}
	}
	result.ParticipantsAdded += added
}

// verifyPrincipal checks that the given ID exists in the table claimed by kind.
func (s *DMMigrationService) verifyPrincipal(ctx context.Context, kind, id string) bool {
	switch kind {
	case "user":
		_, err := s.store.GetUser(ctx, id)
		return err == nil
	case "agent":
		_, err := s.store.GetAgent(ctx, id)
		return err == nil
	default:
		return false
	}
}

// countMissingParticipants returns how many of the two principals are missing.
func (s *DMMigrationService) countMissingParticipants(
	existing []store.ConversationParticipant,
	kindA, idA, kindB, idB string,
) int {
	missing := 2
	for _, p := range existing {
		if (p.PrincipalKind == kindA && p.PrincipalID == idA) ||
			(p.PrincipalKind == kindB && p.PrincipalID == idB) {
			missing--
		}
	}
	if missing < 0 {
		missing = 0
	}
	return missing
}

// ---------------------------------------------------------------------------
// Step 3a: Merge or re-key empty-ref rows
// ---------------------------------------------------------------------------

func (s *DMMigrationService) stepMergeOrRekeyEmptyRef(
	ctx context.Context,
	conv *store.Conversation,
	dryRun bool,
	result *DMMigrationResult,
) {
	// Read participants to determine the two principals.
	parts, err := s.store.ListParticipants(ctx, conv.ID)
	if err != nil {
		result.Unparseable++
		result.Errors = append(result.Errors,
			fmt.Sprintf("step3a: list participants for %s: %v", conv.ID, err))
		return
	}

	// Filter to active participants (no LeftAt).
	var active []store.ConversationParticipant
	for _, p := range parts {
		if p.LeftAt == nil {
			active = append(active, p)
		}
	}

	if len(active) != 2 {
		result.Unparseable++
		return
	}

	// Validate kinds.
	if !isValidDMKind(active[0].PrincipalKind) || !isValidDMKind(active[1].PrincipalKind) {
		result.Unparseable++
		return
	}

	// Compute the kind-encoded key.
	newKey, err := messages.DMConversationKey(
		active[0].PrincipalKind, active[0].PrincipalID,
		active[1].PrincipalKind, active[1].PrincipalID,
	)
	if err != nil {
		result.Unparseable++
		result.Errors = append(result.Errors,
			fmt.Sprintf("step3a: compute key for %s: %v", conv.ID, err))
		return
	}

	// Check if a row with this key already exists.
	existing, err := s.store.GetConversationByExternalRef(ctx, "native", newKey)
	if err == nil && existing != nil && existing.ID != conv.ID {
		// Merge case: re-stamp messages and soft-delete old row.
		if dryRun {
			result.EmptyRefMerged++
			return
		}
		if mergeErr := s.mergeConversation(ctx, conv.ID, existing.ID, active, result); mergeErr != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("step3a: merge %s → %s: %v", conv.ID, existing.ID, mergeErr))
			return
		}
		result.EmptyRefMerged++
	} else {
		// Re-key in place.
		if dryRun {
			result.EmptyRefRekeyed++
			return
		}
		conv.ExternalRef = newKey
		conv.ProjectID = nil // DMs are global (DEF-10)
		if err := s.store.UpdateConversation(ctx, conv); err != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("step3a: re-key %s: %v", conv.ID, err))
			return
		}
		result.EmptyRefRekeyed++
	}
}

// ---------------------------------------------------------------------------
// Step 3b: Re-key old-format dm: rows
// ---------------------------------------------------------------------------

func (s *DMMigrationService) stepRekeyOldFormat(
	ctx context.Context,
	conv *store.Conversation,
	dryRun bool,
	result *DMMigrationResult,
) {
	// Old format: dm:<sorted_id1>:<sorted_id2>
	// Strip "dm:" prefix, split into exactly 2 IDs.
	body := conv.ExternalRef[3:] // strip "dm:"
	parts := strings.SplitN(body, ":", 2)
	if len(parts) != 2 {
		result.Unparseable++
		return
	}

	id1, id2 := parts[0], parts[1]

	// Validate both are UUIDs.
	if !isValidUUID(id1) || !isValidUUID(id2) {
		result.Unparseable++
		return
	}

	// Resolve kinds by looking up each ID in both tables.
	kind1, ok1 := s.resolveKind(ctx, id1)
	kind2, ok2 := s.resolveKind(ctx, id2)

	if !ok1 || !ok2 {
		result.Ambiguous++
		result.Errors = append(result.Errors,
			fmt.Sprintf("step3b: ambiguous kind resolution for conversation %s — id1=%s id2=%s (found in neither or both tables)", conv.ID, id1, id2))
		return
	}

	// Compute the kind-encoded key.
	newKey, err := messages.DMConversationKey(kind1, id1, kind2, id2)
	if err != nil {
		result.Unparseable++
		result.Errors = append(result.Errors,
			fmt.Sprintf("step3b: compute key for %s: %v", conv.ID, err))
		return
	}

	// Check if a row with this key already exists.
	existing, err := s.store.GetConversationByExternalRef(ctx, "native", newKey)
	if err == nil && existing != nil && existing.ID != conv.ID {
		// Merge case.
		if dryRun {
			result.OldFormatRekeyed++
			return
		}
		// Get participants for merge.
		convParts, _ := s.store.ListParticipants(ctx, conv.ID)
		if mergeErr := s.mergeConversation(ctx, conv.ID, existing.ID, convParts, result); mergeErr != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("step3b: merge %s → %s: %v", conv.ID, existing.ID, mergeErr))
			return
		}
		result.OldFormatRekeyed++
	} else {
		// Re-key in place.
		if dryRun {
			result.OldFormatRekeyed++
			return
		}
		conv.ExternalRef = newKey
		conv.ProjectID = nil // DMs are global (DEF-10)
		if err := s.store.UpdateConversation(ctx, conv); err != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("step3b: re-key %s: %v", conv.ID, err))
			return
		}
		result.OldFormatRekeyed++
	}
}

// resolveKind attempts to determine the kind ("user" or "agent") for a given ID
// by looking it up in both tables. Returns ("", false) if found in neither or both.
func (s *DMMigrationService) resolveKind(ctx context.Context, id string) (string, bool) {
	_, userErr := s.store.GetUser(ctx, id)
	_, agentErr := s.store.GetAgent(ctx, id)

	isUser := userErr == nil
	isAgent := agentErr == nil

	if isUser && isAgent {
		// Found in both — ambiguous.
		return "", false
	}
	if !isUser && !isAgent {
		// Found in neither — ambiguous.
		return "", false
	}
	if isUser {
		return "user", true
	}
	return "agent", true
}

// ---------------------------------------------------------------------------
// Merge helper
// ---------------------------------------------------------------------------

// mergeConversation moves messages from oldConvID to newConvID, copies missing
// participants, and soft-deletes the old row.
func (s *DMMigrationService) mergeConversation(
	ctx context.Context,
	oldConvID, newConvID string,
	oldParticipants []store.ConversationParticipant,
	result *DMMigrationResult,
) error {
	// Re-stamp all messages from old conversation to new.
	cursor := ""
	for {
		page, err := s.store.ListMessages(ctx, store.MessageFilter{
			ConversationID: oldConvID,
		}, store.ListOptions{
			Limit:  defaultBatchSize,
			Cursor: cursor,
		})
		if err != nil {
			return fmt.Errorf("listing messages for %s: %w", oldConvID, err)
		}

		for _, msg := range page.Items {
			if err := s.store.SetMessageConversationID(ctx, msg.ID, newConvID); err != nil {
				result.Errors = append(result.Errors,
					fmt.Sprintf("re-stamp message %s: %v", msg.ID, err))
			}
		}

		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	// Copy missing participants to the target conversation.
	for _, p := range oldParticipants {
		err := s.store.AddParticipant(ctx, &store.ConversationParticipant{
			ID:             uuid.NewString(),
			ConversationID: newConvID,
			PrincipalKind:  p.PrincipalKind,
			PrincipalID:    p.PrincipalID,
			Role:           "member",
		})
		if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			result.Errors = append(result.Errors,
				fmt.Sprintf("copy participant %s:%s to %s: %v",
					p.PrincipalKind, p.PrincipalID, newConvID, err))
		}
	}

	// Soft-delete the old row.
	if err := s.store.DeleteConversation(ctx, oldConvID); err != nil {
		return fmt.Errorf("deleting old conversation %s: %w", oldConvID, err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// isValidDMKind returns true if the kind is a valid DM principal kind.
func isValidDMKind(kind string) bool {
	return kind == "user" || kind == "agent"
}
