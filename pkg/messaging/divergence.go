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
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// Divergence logging (Phase 5 gate for S4 read-switch)
// ---------------------------------------------------------------------------

// DivergenceEntry records a comparison between old-model routing (thread_id,
// sender/recipient pairs) and new-model routing (conversation_id resolved via
// UpsertConversationByExternalRef).
type DivergenceEntry struct {
	MessageID  string `json:"message_id"`
	OldRouting string `json:"old_routing"` // e.g. "thread:dm:abc123" or "sender:X->recipient:Y"
	NewRouting string `json:"new_routing"` // e.g. "conv:uuid-of-conversation"
	Match      bool   `json:"match"`       // true when old and new agree
	Reason     string `json:"reason"`      // human-readable explanation
}

// DivergenceCounter tracks the total number of divergence entries logged,
// partitioned into matches and mismatches. Safe for concurrent use.
type DivergenceCounter struct {
	matches    atomic.Int64
	mismatches atomic.Int64
}

// Inc increments the appropriate counter.
func (c *DivergenceCounter) Inc(match bool) {
	if match {
		c.matches.Add(1)
	} else {
		c.mismatches.Add(1)
	}
}

// Matches returns the total number of matching entries logged.
func (c *DivergenceCounter) Matches() int64 { return c.matches.Load() }

// Mismatches returns the total number of mismatching entries logged.
func (c *DivergenceCounter) Mismatches() int64 { return c.mismatches.Load() }

// Total returns the total number of divergence entries logged.
func (c *DivergenceCounter) Total() int64 { return c.matches.Load() + c.mismatches.Load() }

// DivergenceMetrics is the package-level counter for divergence events.
// Exported so that metrics collectors can read it.
var DivergenceMetrics = &DivergenceCounter{}

// LogDivergence logs a DivergenceEntry to the provided logger and increments
// the global divergence counter. Matching entries are logged at INFO;
// mismatches are logged at WARN for easy grep.
func LogDivergence(log *slog.Logger, entry DivergenceEntry) {
	DivergenceMetrics.Inc(entry.Match)

	attrs := []any{
		"message_id", entry.MessageID,
		"old_routing", entry.OldRouting,
		"new_routing", entry.NewRouting,
		"match", entry.Match,
		"divergence_count", DivergenceMetrics.Total(),
	}
	if entry.Reason != "" {
		attrs = append(attrs, "reason", entry.Reason)
	}

	if entry.Match {
		log.Info("conversation routing check: match", attrs...)
	} else {
		log.Warn("conversation routing check: DIVERGENCE", attrs...)
	}
}

// NewRoutingStr formats a conversation ID for the divergence log's NewRouting
// field. Returns "conv:{id}" when convID is non-empty, "none" otherwise.
func NewRoutingStr(convID string) string {
	if convID == "" {
		return "none"
	}
	return "conv:" + convID
}

// ---------------------------------------------------------------------------
// Deterministic external reference helpers
// ---------------------------------------------------------------------------

// directMessageExternalRef builds a deterministic, order-independent external
// reference for a direct-message conversation between two principals.
// Format: dm:{sorted(idA, idB)}
//
// UNEXPORTED: This is the legacy format that predates kind-safe convergence
// (DEF-8). Production code must use DeriveConversationKey / DMConversationKey
// instead. This function is retained only for divergence tests that need the
// old shape for comparison.
//
// Both IDs are required; an empty ID produces a ref that makes the divergence
// visible rather than silently swallowing the error.
func directMessageExternalRef(idA, idB string) string {
	pair := []string{idA, idB}
	sort.Strings(pair)
	return fmt.Sprintf("dm:%s:%s", pair[0], pair[1])
}

// ComputeDivergenceMatch compares old-model routing against the ACTUAL
// external_ref of the conversation the new model resolved. The comparison
// is non-tautological: actualExternalRef comes from the database, not from
// reconstructing inputs.
//
// Parameters:
//   - oldRouting: the old-model routing key (from OldRoutingFromMessage)
//   - actualExternalRef: the external_ref read from the DB via ConversationResult
//   - convID: the conversation ID resolved by the new model (empty if resolution failed)
func ComputeDivergenceMatch(oldRouting, actualExternalRef, convID string) (match bool, reason string) {
	// New model failed
	if convID == "" {
		return false, "no-new-routing"
	}
	// Old model has no routing
	if oldRouting == "" {
		return false, "unknown/no-old-routing"
	}

	// DM comparison: old="sender-recipient:{sortedID}:{sortedID}",
	// new="dm:{kind}:{id}:{kind}:{id}" (kind-prefixed canonical key from
	// DMConversationKey). Extract the raw IDs from the kind-prefixed key
	// and sort them to compare against the old model's raw sorted pair.
	//
	// Case (c) ruling: a DM that carries a thread_id enters the thread
	// branch above via OldRoutingFromMessage, producing "thread:{threadID}".
	// The new model routes it by DM key, so it hits the routing-type-mismatch
	// fallback. This is the correct signal — the old model really did route
	// those by thread while the new model routes by DM key.
	//
	// Case (d) note: non-canonical raw IDs (e.g. uppercase UUIDs) in the old
	// routing will not match canonical IDs in the new key. Treated as latent;
	// not observed in production.
	if strings.HasPrefix(oldRouting, "sender-recipient:") && strings.HasPrefix(actualExternalRef, "dm:") {
		oldPair := strings.TrimPrefix(oldRouting, "sender-recipient:")
		// New format: "dm:kindA:idA:kindB:idB" → extract raw IDs, sort, join.
		newPair := strings.TrimPrefix(actualExternalRef, "dm:")
		if parts := strings.Split(actualExternalRef, ":"); len(parts) == 5 && parts[0] == "dm" {
			pair := []string{parts[2], parts[4]}
			sort.Strings(pair)
			newPair = strings.Join(pair, ":")
		}
		if oldPair == newPair {
			return true, "dm-routing-agreement"
		}
		return false, fmt.Sprintf("dm-routing-mismatch: old=%s new=%s", oldPair, newPair)
	}

	// Thread comparison: old="thread:{threadID}", new="thread:{projectID}:{threadID}"
	if strings.HasPrefix(oldRouting, "thread:") && strings.HasPrefix(actualExternalRef, "thread:") {
		oldThreadID := strings.TrimPrefix(oldRouting, "thread:")
		// New format is "thread:{projectID}:{threadID}" — extract the threadID part
		newAfterPrefix := strings.TrimPrefix(actualExternalRef, "thread:")
		// Split on first ":" to separate projectID from threadID
		if idx := strings.Index(newAfterPrefix, ":"); idx >= 0 {
			newThreadID := newAfterPrefix[idx+1:]
			if oldThreadID == newThreadID {
				return true, "thread-routing-agreement"
			}
			return false, fmt.Sprintf("thread-routing-mismatch: old=%s new=%s", oldThreadID, newThreadID)
		}
		// Unexpected format — no projectID separator
		return false, fmt.Sprintf("thread-format-unexpected: %s", actualExternalRef)
	}

	// Routing type mismatch (e.g., old says DM but new resolved a thread, or vice versa)
	return false, fmt.Sprintf("routing-type-mismatch: old=%s new=%s", oldRouting, actualExternalRef)
}

// OldRoutingFromMessage builds the old-model routing key from a message's
// sender/recipient pair and optional thread_id.
func OldRoutingFromMessage(senderID, recipientID, threadID string) string {
	if threadID != "" {
		return "thread:" + threadID
	}
	parts := []string{senderID, recipientID}
	sort.Strings(parts)
	return fmt.Sprintf("sender-recipient:%s", strings.Join(parts, ":"))
}
