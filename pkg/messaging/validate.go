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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// AgentProjectLookup is the minimal interface needed to check which project
// an agent belongs to. Implementations include store.Store.
type AgentProjectLookup interface {
	GetAgent(ctx context.Context, id string) (*store.Agent, error)
}

// ValidateMessage checks that a Message is internally consistent.
// It delegates to msg.Validate() for structural checks (ID, From, Kind,
// Visibility, kind/intent/event mutual-exclusivity), then adds domain-level
// checks that only the standalone validator knows about.
// Every rule has a corresponding test that fails when the rule is removed.
func ValidateMessage(msg *Message) error {
	if msg == nil {
		return fmt.Errorf("message must not be nil")
	}
	// Delegate structural checks to the type's own Validate.
	if err := msg.Validate(); err != nil {
		return err
	}
	// Domain-level checks not on the type itself:
	// 1. ConversationID is required.
	if msg.ConversationID == "" {
		return fmt.Errorf("conversation_id is required")
	}
	// 2. Body size limits (reuse constants from messages package).
	if len([]rune(msg.Body)) > messages.MaxMessageLength {
		return fmt.Errorf("body exceeds %d character limit (current: %d chars)",
			messages.MaxMessageLength, len([]rune(msg.Body)))
	}
	if len(msg.Body) > messages.MaxMsgSize {
		return fmt.Errorf("body exceeds maximum size of %d bytes", messages.MaxMsgSize)
	}
	// 3. Attachment count limit.
	if len(msg.Attachments) > messages.MaxAttachments {
		return fmt.Errorf("too many attachments: %d (max %d)",
			len(msg.Attachments), messages.MaxAttachments)
	}
	// 4. ReplyToID, if present, must not be empty string.
	if msg.ReplyToID != nil && *msg.ReplyToID == "" {
		return fmt.Errorf("reply_to_id must not be empty when set")
	}
	return nil
}

// ValidateAddressees checks the addressee list for a message.
func ValidateAddressees(addrs []Addressee, msg *Message) error {
	seen := make(map[string]bool, len(addrs))
	for i, a := range addrs {
		// 1. PrincipalKind must be "user" or "agent".
		if a.PrincipalKind != "user" && a.PrincipalKind != "agent" {
			return fmt.Errorf("addressee[%d]: principal_kind must be user or agent, got %q", i, a.PrincipalKind)
		}
		// 2. PrincipalID must not be empty.
		if a.PrincipalID == "" {
			return fmt.Errorf("addressee[%d]: principal_id is required", i)
		}
		// 3. Via must be valid.
		if err := ValidateAddressedVia(a.Via); err != nil {
			return fmt.Errorf("addressee[%d]: %w", i, err)
		}
		// 4. DeliveryState must be valid.
		if err := ValidateDeliveryState(a.DeliveryState); err != nil {
			return fmt.Errorf("addressee[%d]: %w", i, err)
		}
		// 5. No duplicate addressees (same PrincipalKind + PrincipalID).
		key := a.PrincipalKind + ":" + a.PrincipalID
		if seen[key] {
			return fmt.Errorf("addressee[%d]: duplicate addressee %s", i, key)
		}
		seen[key] = true
	}
	return nil
}

// ValidateCrossProjectAddressees checks that all agent addressees belong to
// the same project. This prevents a single message from addressing agents
// across project boundaries, which would violate project isolation (AC-33 / DEF-2).
//
// User addressees are ignored — the rule applies to agent addressees only.
func ValidateCrossProjectAddressees(
	ctx context.Context,
	agentStore AgentProjectLookup,
	addrs []Addressee,
) error {
	var projectID string
	var projectAgents []string // for error reporting

	for _, a := range addrs {
		if a.PrincipalKind != "agent" {
			continue
		}
		agent, err := agentStore.GetAgent(ctx, a.PrincipalID)
		if err != nil {
			return fmt.Errorf("failed to look up agent %q: %w", a.PrincipalID, err)
		}
		if projectID == "" {
			projectID = agent.ProjectID
			projectAgents = append(projectAgents, a.PrincipalID)
			continue
		}
		if agent.ProjectID != projectID {
			projectAgents = append(projectAgents, a.PrincipalID)
			return fmt.Errorf(
				"message addresses agents in multiple projects; " +
					"a single message may only target agents within one project",
			)
		}
		projectAgents = append(projectAgents, a.PrincipalID)
	}
	return nil
}

// ValidateMessageAddressees performs the full addressee validation including
// the cross-project check (AC-33). Callers that have a store and addressees
// should call this after ValidateMessage/ValidateLegacyMessage.
func ValidateMessageAddressees(
	ctx context.Context,
	agentStore AgentProjectLookup,
	msg *Message,
	addrs []Addressee,
) error {
	if err := ValidateAddressees(addrs, msg); err != nil {
		return err
	}
	return ValidateCrossProjectAddressees(ctx, agentStore, addrs)
}
