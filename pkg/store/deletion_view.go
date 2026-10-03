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

package store

import (
	"encoding/json"
	"time"
)

// Agent.DeletionState values (design ptone/scion#2483 §2.1).
const (
	DeletionStateNone       = ""
	DeletionStateDeleting   = "deleting"
	DeletionStateFinalizing = "finalizing"
	DeletionStateFailed     = "failed"
)

// Deletion failure codes (design §2.2). DeletionCodeAbandoned is never
// stored by a live engine; it is what the view reports for a lease-expired
// row that has no stored code (a dead engine).
const (
	DeletionCodeRuntimeError   = "runtime_error"
	DeletionCodeConflict       = "conflict"
	DeletionCodeInDoubt        = "in_doubt"
	DeletionCodeAbandoned      = "abandoned"
	DeletionCodeRevokeFailed   = "revoke_failed"
	DeletionCodeFinalizeFailed = "finalize_failed"
)

// DeletionDisplayTTL is how long a failed delete stays visible in the view
// (design §2.1 "Display lifetime"), measured from DeletionFailedAt, or from
// DeletionLeaseAt for a lease-expired deleting row.
const DeletionDisplayTTL = 15 * time.Minute

// DeletionInfo is the client-facing view of an agent's active or failed
// delete (design §2.2). It appears on the REST agent (Agent.Deletion) and on
// every AgentStatusEvent, as an explicit null when no delete is active or
// failed.
type DeletionInfo struct {
	State          string     `json:"state"`          // "deleting" (incl. finalizing) | "failed"
	Code           string     `json:"code,omitempty"` // runtime_error | conflict | in_doubt | abandoned | revoke_failed | finalize_failed
	Error          string     `json:"error,omitempty"`
	Soft           bool       `json:"soft"`
	Claim          int64      `json:"claim"` // named to avoid confusion with Agent.Generation (reincarnation)
	StartedAt      time.Time  `json:"startedAt"`
	LeaseExpiresAt *time.Time `json:"leaseExpiresAt,omitempty"` // deleting
	ExpiresAt      *time.Time `json:"expiresAt,omitempty"`      // failed, except in_doubt and finalizing
}

// DeletionPriorState is the JSON stored in Agent.DeletionPrior: the state a
// failed delete restores (design §2.1 claim).
type DeletionPriorState struct {
	Phase    string `json:"phase,omitempty"`
	Activity string `json:"activity,omitempty"`
	LaunchID string `json:"launchId,omitempty"`
}

// DeletionRequestInfo is the JSON stored in Agent.DeletionRequest: the
// parameters of the delete request that holds the current claim.
type DeletionRequestInfo struct {
	DeleteFiles  bool   `json:"deleteFiles,omitempty"`
	RemoveBranch bool   `json:"removeBranch,omitempty"`
	Soft         bool   `json:"soft,omitempty"`
	Force        bool   `json:"force,omitempty"`
	RequestedBy  string `json:"requestedBy,omitempty"`
}

// ParseDeletionRequest decodes Agent.DeletionRequest. An empty or malformed
// value yields the zero request.
func (a *Agent) ParseDeletionRequest() DeletionRequestInfo {
	var r DeletionRequestInfo
	if a == nil || a.DeletionRequest == "" {
		return r
	}
	_ = json.Unmarshal([]byte(a.DeletionRequest), &r)
	return r
}

// ParseDeletionPrior decodes Agent.DeletionPrior. An empty or malformed value
// yields the zero prior.
func (a *Agent) ParseDeletionPrior() DeletionPriorState {
	var p DeletionPriorState
	if a == nil || a.DeletionPrior == "" {
		return p
	}
	_ = json.Unmarshal([]byte(a.DeletionPrior), &p)
	return p
}

// deletionInProgressState reports whether s is one of the two states a live
// engine holds (deleting, finalizing).
func deletionInProgressState(s string) bool {
	return s == DeletionStateDeleting || s == DeletionStateFinalizing
}

// DeletionActive is the lease-aware active predicate (design §2.1):
// state ∈ {deleting, finalizing} && lease_at > now. A row whose lease has
// passed is not active: it reads as failed everywhere.
func (a *Agent) DeletionActive(now time.Time) bool {
	if a == nil || !deletionInProgressState(a.DeletionState) {
		return false
	}
	return a.DeletionLeaseAt != nil && a.DeletionLeaseAt.After(now)
}

// DeletionEffectiveCode returns the code a failed or lease-expired delete
// reads as: the stored code if one is set, otherwise "abandoned" for a
// lease-expired deleting/finalizing row (a dead engine). Empty for a row
// whose delete is live or absent.
func (a *Agent) DeletionEffectiveCode(now time.Time) string {
	if a == nil {
		return ""
	}
	switch {
	case a.DeletionState == DeletionStateFailed:
		return a.DeletionCode
	case deletionInProgressState(a.DeletionState) && !a.DeletionActive(now):
		if a.DeletionCode != "" {
			return a.DeletionCode
		}
		return DeletionCodeAbandoned
	}
	return ""
}

// ComputeAgentDeletion builds the client-facing DeletionInfo view from an
// Agent's deletion_* columns (design §2.1, §2.2), or returns nil when no
// delete is active or visible. now is the answering node's wall clock.
//
// Display rules:
//   - a lease-live deleting/finalizing row reads "deleting", with
//     leaseExpiresAt and no expiresAt;
//   - a lease-expired deleting/finalizing row reads "failed", with its stored
//     code or else "abandoned";
//   - expiresAt = (failedAt ?? leaseAt) + 15m for failed rows and
//     lease-expired deleting rows; past it the view is nil (no banner). If
//     both are nil it falls back to startedAt + 15m, so a malformed row
//     still ages out (the engine always sets failedAt on a failed row and
//     leaseAt on a claim, so this fallback is defensive). A row with none of
//     the three has no expiresAt;
//   - lease-expired finalizing rows and in_doubt rows have no expiresAt and
//     never expire from view.
//
// A soft-deleted row (DeletedAt set) has no view.
func ComputeAgentDeletion(a *Agent, now time.Time) *DeletionInfo {
	if a == nil || a.DeletionState == DeletionStateNone || !a.DeletedAt.IsZero() {
		return nil
	}
	info := &DeletionInfo{
		Error: a.DeletionError,
		Soft:  a.ParseDeletionRequest().Soft,
		Claim: a.DeletionClaim,
	}
	if a.DeletionStartedAt != nil {
		info.StartedAt = *a.DeletionStartedAt
	}
	if a.DeletionActive(now) {
		info.State = DeletionStateDeleting
		lease := *a.DeletionLeaseAt
		info.LeaseExpiresAt = &lease
		return info
	}
	info.State = DeletionStateFailed
	info.Code = a.DeletionEffectiveCode(now)
	if a.DeletionState == DeletionStateFinalizing || info.Code == DeletionCodeInDoubt {
		return info // never expires from view
	}
	var base *time.Time
	switch {
	case a.DeletionFailedAt != nil:
		base = a.DeletionFailedAt
	case a.DeletionLeaseAt != nil:
		base = a.DeletionLeaseAt
	case a.DeletionStartedAt != nil:
		base = a.DeletionStartedAt
	}
	if base != nil {
		exp := base.Add(DeletionDisplayTTL)
		if !now.Before(exp) {
			return nil
		}
		info.ExpiresAt = &exp
	}
	return info
}
