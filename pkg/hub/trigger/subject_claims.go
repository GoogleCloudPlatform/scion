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

package trigger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// AbandonedClaimTTL is the age after which an unbound active claim
// (agent_id IS NULL) is considered abandoned and eligible for takeover
// or maintenance release (Stage 2 §5.6, §18 S16).
const AbandonedClaimTTL = store.TriggerSubjectAbandonedClaimTTL

// ErrClaimNotHeld is returned by Bind when the claim row is no longer held by
// the caller's fire (for example, it was already released or taken over after
// the abandoned-claim TTL).
var ErrClaimNotHeld = errors.New("trigger: subject claim is not held")

// Claim is a per-(trigger, subject) live-subject claim record (Stage 2 §5.6).
type Claim = store.TriggerSubjectClaim

// SubjectClaims manages atomic per-(trigger, subject) live-subject claims for
// dispatch_agent mode "skip" (Stage 2 §5.6) and Stage 3 "reuse".
type SubjectClaims interface {
	// Claim atomically takes (trigger, subject): INSERT, or UPDATE … WHERE state='released'
	// or (state='active' AND agent_id IS NULL AND claimed_at < now-5m) [abandoned claim].
	// Returns the existing active claim (with its agent) when it cannot take it.
	Claim(ctx context.Context, triggerID, subject, eventID, fireID string) (c Claim, existing *Claim, err error)
	Bind(ctx context.Context, c Claim, agentID string) error // state stays active; agent_id set
	Release(ctx context.Context, c Claim) error              // state=released (create failed, agent deleted/terminal)
	Live(ctx context.Context, triggerID, subject string) (*Claim, error)
}

// AgentLookup is the narrow read-only interface SubjectClaims uses to check
// whether a bound claim's agent still exists and is non-terminal.
type AgentLookup interface {
	GetAgent(ctx context.Context, id string) (*store.Agent, error)
}

// AgentPhaseLookup is an optional capability on SubjectClaims (implemented by
// StoreSubjectClaims) allowing the dispatcher to inspect a claimed agent's
// phase before attempting reuse wake delivery (Stage 3 §6.2 rev 1.16).
type AgentPhaseLookup interface {
	AgentPhase(ctx context.Context, agentID string) (phase string, exists bool, err error)
}

// StoreSubjectClaims implements SubjectClaims over store.TriggerStore and AgentLookup.
type StoreSubjectClaims struct {
	triggers store.TriggerStore
	agents   AgentLookup
	now      func() time.Time
}

var _ SubjectClaims = (*StoreSubjectClaims)(nil)

// NewSubjectClaims constructs a StoreSubjectClaims backed by triggers and agents.
// If now is nil, time.Now is used.
func NewSubjectClaims(triggers store.TriggerStore, agents AgentLookup, now func() time.Time) *StoreSubjectClaims {
	if now == nil {
		now = time.Now
	}
	return &StoreSubjectClaims{
		triggers: triggers,
		agents:   agents,
		now:      now,
	}
}

// Claim atomically acquires (triggerID, subject) or returns the existing active claim.
func (s *StoreSubjectClaims) Claim(ctx context.Context, triggerID, subject, eventID, fireID string) (Claim, *Claim, error) {
	if s == nil || s.triggers == nil {
		return Claim{}, nil, fmt.Errorf("%w: subject claims store is nil", store.ErrInvalidInput)
	}
	now := s.now().UTC()
	claimed, existing, err := s.triggers.ClaimTriggerSubject(ctx, triggerID, subject, eventID, fireID, now, now.Add(-AbandonedClaimTTL))
	if err != nil {
		return Claim{}, nil, err
	}
	if claimed != nil {
		return *claimed, nil, nil
	}
	return Claim{}, existing, nil
}

// Bind records agentID on an active claim held by c.FireID.
func (s *StoreSubjectClaims) Bind(ctx context.Context, c Claim, agentID string) error {
	if s == nil || s.triggers == nil {
		return fmt.Errorf("%w: subject claims store is nil", store.ErrInvalidInput)
	}
	bound, err := s.triggers.BindTriggerSubjectClaim(ctx, c.ID, c.FireID, agentID, s.now().UTC())
	if err != nil {
		return err
	}
	if !bound {
		return ErrClaimNotHeld
	}
	return nil
}

// Release transitions an active unbound claim (agent_id IS NULL) held by
// c.FireID to released. Releasing an already-released, superseded, or bound
// claim is a no-op (bound claims are released only via Live, maintenance, or
// agent deletion).
func (s *StoreSubjectClaims) Release(ctx context.Context, c Claim) error {
	if s == nil || s.triggers == nil {
		return fmt.Errorf("%w: subject claims store is nil", store.ErrInvalidInput)
	}
	_, err := s.triggers.ReleaseTriggerSubjectClaim(ctx, c.ID, c.FireID, s.now().UTC())
	return err
}

// Live returns the active claim for (triggerID, subject) whose bound agent
// exists, has deleted_at IS NULL, and is not in a terminal phase
// ({stopped, error, halted}). If the active claim's agent is missing,
// soft-deleted, or terminal, Live lazily releases the claim and returns (nil, nil).
// An active unbound claim (agent_id IS NULL) has no live agent yet and returns (nil, nil)
// without releasing it.
func (s *StoreSubjectClaims) Live(ctx context.Context, triggerID, subject string) (*Claim, error) {
	if s == nil || s.triggers == nil || s.agents == nil {
		return nil, fmt.Errorf("%w: subject claims store is nil", store.ErrInvalidInput)
	}
	claim, err := s.triggers.GetActiveTriggerSubjectClaim(ctx, triggerID, subject)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if claim.AgentID == "" {
		return nil, nil
	}

	ag, err := s.agents.GetAgent(ctx, claim.AgentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			if _, rErr := s.triggers.ReleaseTriggerSubjectClaimsForAgent(ctx, claim.AgentID, s.now().UTC()); rErr != nil {
				return nil, rErr
			}
			return nil, nil
		}
		return nil, err
	}
	if ag == nil || !ag.DeletedAt.IsZero() || isTerminalClaimAgentPhase(ag.Phase) {
		if _, rErr := s.triggers.ReleaseTriggerSubjectClaimsForAgent(ctx, claim.AgentID, s.now().UTC()); rErr != nil {
			return nil, rErr
		}
		return nil, nil
	}
	return claim, nil
}

// AgentPhase returns the current lifecycle phase of agentID, or exists=false
// when the agent is missing or soft-deleted.
func (s *StoreSubjectClaims) AgentPhase(ctx context.Context, agentID string) (string, bool, error) {
	if s == nil || s.agents == nil || agentID == "" {
		return "", false, nil
	}
	ag, err := s.agents.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	if ag == nil || !ag.DeletedAt.IsZero() {
		return "", false, nil
	}
	return ag.Phase, true, nil
}

func isTerminalClaimAgentPhase(phase string) bool {
	switch state.Phase(phase) {
	case state.PhaseStopped, state.PhaseError, state.PhaseHalted:
		return true
	default:
		return false
	}
}
