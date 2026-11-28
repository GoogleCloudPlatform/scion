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

package artifacts

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Trigger identifiers recorded as Actor.Ref on automatic ancestor succession
// transfers (ptone/scion#9846).
const (
	SuccessionTriggerAgentDelete = "agent_delete"
	SuccessionTriggerReconciler  = "reconciler"
)

const successionPage = 100

// Succession describes an automatic ownership transfer of a deleted agent's
// live artifacts in its home project to a live, eligible ancestor agent.
type Succession struct {
	From               Owner  // {agent, childID}
	ProjectID          string // child's project; only artifacts homed here move
	ToAgentID          string
	Sponsor            string
	SponsorReviewGrant bool
	Trigger            string
	// OnTransferred, when non-nil, is invoked after each committed transfer.
	OnTransferred func(a *Artifact, res TransferResult)
}

// SuccessionResult summarizes one TransferToSuccessor run.
type SuccessionResult struct {
	Transferred int
	KeyCleared  int
	Skipped     int
}

func successionActor(trigger, projectID, fromAgentID string) *Actor {
	return &Actor{
		Kind:      ActorKindSystem,
		Ref:       trigger,
		Authority: AuthorityAncestorSuccession,
		Expect: Expect{
			ScopeRef:  projectID,
			OwnerKind: PrincipalKindAgent,
			OwnerRef:  fromAgentID,
		},
	}
}

// TransferToSuccessor transfers every live project-scoped artifact owned by
// s.From and homed in s.ProjectID to s.ToAgentID, one transaction per artifact.
// Key collisions clear the transferred artifact's key into former_key (with a
// single retry on concurrent ErrKeyConflict). Artifacts that were moved out of
// s.ProjectID, changed owner, or deleted concurrently are skipped and logged.
// Any other store error stops the loop and is returned; already-committed
// transfers remain committed and a subsequent run is idempotent.
func TransferToSuccessor(ctx context.Context, st Store, s Succession, now time.Time, log *slog.Logger) (SuccessionResult, error) {
	var result SuccessionResult
	if st == nil {
		return result, errors.New("artifacts: succession needs a store")
	}
	if s.From.Kind == "" || s.From.Ref == "" || s.ProjectID == "" || s.ToAgentID == "" || s.Sponsor == "" || s.Trigger == "" {
		return result, errors.New("artifacts: incomplete succession parameters")
	}
	if log == nil {
		log = discardLogger
	}
	actor := successionActor(s.Trigger, s.ProjectID, s.From.Ref)
	opts := TransferOptions{
		ExpectOwner:        s.From,
		To:                 Owner{Kind: PrincipalKindAgent, Ref: s.ToAgentID},
		Sponsor:            s.Sponsor,
		ClearKey:           true,
		SponsorReviewGrant: s.SponsorReviewGrant,
		FormerOwnerLive:    false,
	}

	after := ""
	for {
		page, err := st.ListLiveOwnedArtifacts(ctx, s.From, ScopeKindProject, s.ProjectID, after, successionPage)
		if err != nil {
			return result, err
		}
		for _, ref := range page {
			after = ref.ID
			a, res, err := st.TransferOwner(ctx, ref.ID, opts, actor, now)
			if errors.Is(err, ErrKeyConflict) {
				// Lost a race with a concurrent same-key publish after the
				// clash count and before the UPDATE; retry once so the clash
				// is now visible and cleared.
				a, res, err = st.TransferOwner(ctx, ref.ID, opts, actor, now)
			}
			switch {
			case err == nil:
				keyCleared := res.KeyCleared
				writeSystemDecision(ctx, log, *actor, logEntry{
					action:         logActionTransfer,
					outcome:        logOutcomeAllowed,
					artifact:       a,
					subjectKind:    PrincipalKindAgent,
					subjectRef:     s.ToAgentID,
					formerOwnerRef: s.From.Ref,
					keyCleared:     &keyCleared,
				})
				result.Transferred++
				if res.KeyCleared {
					result.KeyCleared++
				}
				if s.OnTransferred != nil {
					s.OnTransferred(a, res)
				}
			case errors.Is(err, ErrOwnerChanged):
				writeSystemDecision(ctx, log, *actor, logEntry{
					action:         logActionTransfer,
					outcome:        logOutcomeRefused,
					reason:         logReasonOwnerChanged,
					artifact:       &Artifact{ID: ref.ID, ScopeRef: ref.ScopeRef, OwnerKind: s.From.Kind},
					subjectKind:    PrincipalKindAgent,
					subjectRef:     s.ToAgentID,
					formerOwnerRef: s.From.Ref,
				})
				result.Skipped++
			case errors.Is(err, ErrStewardPrecondition):
				writeSystemDecision(ctx, log, *actor, logEntry{
					action:         logActionTransfer,
					outcome:        logOutcomeRefused,
					reason:         logReasonMovedOutOfProject,
					artifact:       &Artifact{ID: ref.ID, ScopeRef: ref.ScopeRef, OwnerKind: s.From.Kind},
					subjectKind:    PrincipalKindAgent,
					subjectRef:     s.ToAgentID,
					formerOwnerRef: s.From.Ref,
				})
				result.Skipped++
			case errors.Is(err, ErrNotFound), errors.Is(err, ErrAttachmentNotShareable):
				writeSystemDecision(ctx, log, *actor, logEntry{
					action:         logActionTransfer,
					outcome:        logOutcomeRefused,
					reason:         logReasonNotFound,
					artifact:       &Artifact{ID: ref.ID, ScopeRef: ref.ScopeRef, OwnerKind: s.From.Kind},
					subjectKind:    PrincipalKindAgent,
					subjectRef:     s.ToAgentID,
					formerOwnerRef: s.From.Ref,
				})
				result.Skipped++
			default:
				writeSystemDecision(ctx, log, *actor, logEntry{
					action:         logActionTransfer,
					outcome:        logOutcomeError,
					reason:         logReasonStoreError,
					artifact:       &Artifact{ID: ref.ID, ScopeRef: ref.ScopeRef, OwnerKind: s.From.Kind},
					subjectKind:    PrincipalKindAgent,
					subjectRef:     s.ToAgentID,
					formerOwnerRef: s.From.Ref,
				})
				return result, err
			}
		}
		if len(page) < successionPage {
			return result, nil
		}
	}
}
