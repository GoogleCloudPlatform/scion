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

// DM diagnostic: inventory existing noncanonical direct conversation rows
// and extraneous participant rows.
//
// This diagnostic provides a read-only report mechanism for:
//  1. Direct conversations with unparseable or malformed DM keys
//  2. Direct conversations with extraneous participant rows (participants
//     not named in the canonical key)
//  3. Direct conversations with missing participant rows (key participants
//     without listing rows)
//
// Rules:
//   - Only migrate where historical principal IDs prove the pair unambiguously.
//   - Never guess from display names or delete historical conversations.
//   - Unresolvable rows fail closed and have a documented operator recovery path.

import (
	"context"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// dmDiagnosticPageSize is the number of conversations fetched per page
// during the DM diagnostic scan.
const dmDiagnosticPageSize = 200

// DMDiagnosticReport represents the diagnostic output for DM conversations.
type DMDiagnosticReport struct {
	TotalDMConversations int
	CanonicalCount       int
	NoncanonicalCount    int

	// Details of issues found.
	UnparseableKeys         []DMDiagnosticIssue
	ExtraneousParticipants  []DMDiagnosticIssue
	MissingParticipants     []DMDiagnosticIssue
	MigratableConversations []DMDiagnosticMigration
}

// DMDiagnosticIssue describes a single noncanonical DM row.
type DMDiagnosticIssue struct {
	ConversationID string
	ExternalRef    string
	Detail         string
}

// DMDiagnosticMigration describes a DM that can be deterministically migrated
// because historical principal IDs prove the pair unambiguously.
type DMDiagnosticMigration struct {
	ConversationID     string
	CurrentExternalRef string
	CanonicalKey       string
	Reason             string
}

// RunDMDiagnostic inspects all direct conversations in the store and reports
// noncanonical rows, extraneous participants, and missing participants.
// This is a read-only operation — it never modifies data.
//
// Conversations are fetched using cursor-based pagination to avoid loading
// the entire table into memory at once.
func RunDMDiagnostic(ctx context.Context, s store.Store) (*DMDiagnosticReport, error) {
	report := &DMDiagnosticReport{}

	filter := store.ConversationFilter{Kind: "direct"}
	opts := store.ListOptions{
		Limit:          dmDiagnosticPageSize,
		SkipTotalCount: true,
	}

	for {
		result, err := s.ListConversations(ctx, filter, opts)
		if err != nil {
			return nil, fmt.Errorf("listing conversations: %w", err)
		}

		for _, conv := range result.Items {
			diagnoseDMConversation(ctx, s, conv, report)
		}

		if result.NextCursor == "" {
			break
		}
		opts.Cursor = result.NextCursor
	}

	return report, nil
}

// diagnoseDMConversation checks a single direct conversation and updates the
// diagnostic report. It verifies the DM key is parseable and that participant
// rows match the canonical key members.
func diagnoseDMConversation(ctx context.Context, s store.Store, conv store.Conversation, report *DMDiagnosticReport) {
	report.TotalDMConversations++

	// Check 1: Can we parse the DM key?
	kindA, idA, kindB, idB, parseErr := messages.ParseDMKey(conv.ExternalRef)
	if parseErr != nil {
		report.NoncanonicalCount++
		report.UnparseableKeys = append(report.UnparseableKeys, DMDiagnosticIssue{
			ConversationID: conv.ID,
			ExternalRef:    conv.ExternalRef,
			Detail:         fmt.Sprintf("unparseable DM key: %v", parseErr),
		})
		return
	}

	// Key is parseable — check participants.
	participants, partErr := s.ListParticipants(ctx, conv.ID)
	if partErr != nil {
		report.NoncanonicalCount++
		report.UnparseableKeys = append(report.UnparseableKeys, DMDiagnosticIssue{
			ConversationID: conv.ID,
			ExternalRef:    conv.ExternalRef,
			Detail:         fmt.Sprintf("error listing participants: %v", partErr),
		})
		return
	}

	isCanonical := true

	// Check 2: Extraneous participants (not named in the key).
	for _, p := range participants {
		if (p.PrincipalKind == kindA && p.PrincipalID == idA) ||
			(p.PrincipalKind == kindB && p.PrincipalID == idB) {
			continue
		}
		isCanonical = false
		report.ExtraneousParticipants = append(report.ExtraneousParticipants, DMDiagnosticIssue{
			ConversationID: conv.ID,
			ExternalRef:    conv.ExternalRef,
			Detail: fmt.Sprintf("extraneous participant: kind=%s id=%s (not in DM key)",
				p.PrincipalKind, p.PrincipalID),
		})
	}

	// Check 3: Missing participants (key members without rows).
	hasA, hasB := false, false
	for _, p := range participants {
		if p.PrincipalKind == kindA && p.PrincipalID == idA {
			hasA = true
		}
		if p.PrincipalKind == kindB && p.PrincipalID == idB {
			hasB = true
		}
	}
	if !hasA {
		// Missing participant is a listing gap, not necessarily a data
		// integrity issue (the key is still the ACL). Report it.
		report.MissingParticipants = append(report.MissingParticipants, DMDiagnosticIssue{
			ConversationID: conv.ID,
			ExternalRef:    conv.ExternalRef,
			Detail:         fmt.Sprintf("missing participant row: kind=%s id=%s", kindA, idA),
		})
	}
	if !hasB {
		report.MissingParticipants = append(report.MissingParticipants, DMDiagnosticIssue{
			ConversationID: conv.ID,
			ExternalRef:    conv.ExternalRef,
			Detail:         fmt.Sprintf("missing participant row: kind=%s id=%s", kindB, idB),
		})
	}

	if isCanonical && hasA && hasB {
		report.CanonicalCount++
	} else {
		report.NoncanonicalCount++
	}
}
