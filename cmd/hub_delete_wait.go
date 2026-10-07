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

package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// hubDeletionWaitOptions tunes the poll after a 202 from DELETE. Zero values
// take the hubclient defaults (2s interval, 180s timeout). Tests override it.
var hubDeletionWaitOptions hubclient.DeletionWaitOptions

// errDeleteResultUnknown marks an outcome where the AgentService cannot
// report 202 versus 204 (it does not implement AgentDeleteResulter).
var errDeleteResultUnknown = errors.New("this client cannot tell whether the Hub finished the delete")

// hubDeleteOutcome is what a hub delete (and, on 202, the poll) ended in.
type hubDeleteOutcome struct {
	// Accepted is true when the hub answered 202 and the poll ran, or when
	// the service could not say whether the delete finished (fail closed).
	Accepted bool
	// Wait is the poll result; meaningful only when Accepted.
	Wait hubclient.DeletionWaitResult
}

// Confirmed reports whether the delete is known to be done: a 204, or a 202
// whose poll confirmed it. Local cleanup may run only then.
func (o hubDeleteOutcome) Confirmed() bool {
	return !o.Accepted || o.Wait.Outcome == hubclient.DeletionConfirmed
}

// deleteViaHubAndWait sends DELETE for agentName through the project-scoped
// svc. On 202 it calls onAccepted (for a progress line) and polls the agent
// until the delete is confirmed, fails, turns out not to be running, cannot
// be observed, or the poll times out. A DELETE error (4xx, 502, 503, network)
// is returned unchanged.
//
// It fails closed: if svc does not implement hubclient.AgentDeleteResulter,
// a successful Delete could have been a 202, so the outcome is reported as
// accepted but unobservable, never as confirmed.
func deleteViaHubAndWait(ctx context.Context, svc hubclient.AgentService, agentName string, opts *hubclient.DeleteAgentOptions, onAccepted func()) (hubDeleteOutcome, error) {
	sent, err := sendHubDelete(ctx, svc, agentName, opts)
	if err != nil {
		return hubDeleteOutcome{}, err
	}
	if sent.NeedsPoll() && onAccepted != nil {
		onAccepted()
	}
	return sent.Wait(ctx), nil
}

// sentHubDelete is a DELETE the hub has answered. Either the outcome is
// already known (204, or a client that cannot tell 202 from 204), or the hub
// answered 202 and Wait must poll for it.
type sentHubDelete struct {
	svc       hubclient.AgentService
	needsPoll bool   // the hub answered 202
	pollID    string // ID to poll; meaningful only when needsPoll
	outcome   hubDeleteOutcome
}

// NeedsPoll reports whether the hub answered 202, so Wait will poll.
func (s sentHubDelete) NeedsPoll() bool { return s.needsPoll }

// Wait returns the outcome, polling first if the hub answered 202. The poll
// has its own budget, independent of ctx: ctx is the DELETE request's
// context, which may have little time left after the hub's ~20s wait. Wait
// is safe to call from several goroutines at once for different deletes.
func (s sentHubDelete) Wait(ctx context.Context) hubDeleteOutcome {
	if !s.NeedsPoll() {
		return s.outcome
	}
	wait := hubclient.WaitForAgentDeletion(context.WithoutCancel(ctx), s.svc, s.pollID, hubDeletionWaitOptions)
	return hubDeleteOutcome{Accepted: true, Wait: wait}
}

// sendHubDelete sends DELETE for agentName and returns without polling. A
// DELETE error is returned unchanged. See deleteViaHubAndWait for the
// fail-closed rule.
func sendHubDelete(ctx context.Context, svc hubclient.AgentService, agentName string, opts *hubclient.DeleteAgentOptions) (sentHubDelete, error) {
	resulter, ok := svc.(hubclient.AgentDeleteResulter)
	if !ok {
		if err := svc.Delete(ctx, agentName, opts); err != nil {
			return sentHubDelete{}, err
		}
		return sentHubDelete{svc: svc, outcome: hubDeleteOutcome{Accepted: true, Wait: hubclient.DeletionWaitResult{
			Outcome: hubclient.DeletionUnobservable,
			Err:     errDeleteResultUnknown,
		}}}, nil
	}
	res, err := resulter.DeleteWithResult(ctx, agentName, opts)
	if err != nil {
		return sentHubDelete{}, err
	}
	if !res.Accepted {
		return sentHubDelete{svc: svc}, nil
	}
	pollID := res.AgentID
	if pollID == "" {
		pollID = agentName
	}
	return sentHubDelete{svc: svc, needsPoll: true, pollID: pollID}, nil
}

// hubDeleteFailure returns the error for an accepted delete that FAILED or
// did NOT take effect, or nil for any other outcome. what names the local
// state that was kept, e.g. "local worktree kept".
func hubDeleteFailure(agentName string, o hubDeleteOutcome, what string) error {
	if !o.Accepted {
		return nil
	}
	switch o.Wait.Outcome {
	case hubclient.DeletionFailed:
		code, msg := "", ""
		if d := o.Wait.Deletion; d != nil {
			code = d.Code
			msg = d.Error
		}
		if code == "" {
			// The hub sends the failure code and error text to platform
			// admins only (ptone/scion#3122). Without them the outcome is
			// still a failure, but which kind is unknown, so start may be
			// blocked.
			return fmt.Errorf("delete failed on the Hub; %s. Retry with 'scion delete %s', or force-delete it from the web UI. Starting the agent may stay blocked until a retry succeeds or force is used",
				what, agentName)
		}
		if msg != "" {
			msg = ": " + msg
		}
		blocked := ""
		switch code {
		case "in_doubt", "revoke_failed", "finalize_failed":
			// in_doubt: a cross-node teardown is still outstanding;
			// revoke_failed/finalize_failed: the row is stuck in finalizing.
			blocked = ". Starting the agent stays blocked until a retry succeeds or force is used"
		case "abandoned":
			// A lease-expired finalizing row with no stored code also reads
			// as abandoned and blocks start; the client cannot tell.
			blocked = ". Starting the agent may stay blocked until a retry succeeds or force is used"
		}
		return fmt.Errorf("delete failed on the Hub (%s)%s; %s. Retry with 'scion delete %s', or force it with 'scion delete --force %s'%s",
			code, msg, what, agentName, agentName, blocked)
	case hubclient.DeletionNotTaken:
		return fmt.Errorf("delete did not take effect (the agent is still live and no delete is running); %s. Retry with 'scion delete %s'",
			what, agentName)
	}
	return nil
}

// hubDeletePendingReason explains why an accepted delete's completion could
// not be observed. That is not a failure: the hub owns the delete and will
// finish or fail it.
func hubDeletePendingReason(o hubDeleteOutcome) string {
	err := o.Wait.Err
	switch o.Wait.Outcome {
	case hubclient.DeletionTimedOut:
		if err != nil {
			return fmt.Sprintf("could not read the agent when the wait ended: %v", err)
		}
		return "still running when the wait ended"
	case hubclient.DeletionUnobservable:
		switch {
		case errors.Is(err, errDeleteResultUnknown):
			return err.Error()
		case err == nil, apiclient.IsForbiddenError(err), apiclient.IsUnauthorizedError(err):
			return "the Hub did not allow reading the agent"
		default:
			return fmt.Sprintf("the Hub rejected reading the agent: %v", err)
		}
	}
	return "outcome unknown"
}

// hubDeletePendingMessage is scion delete's notice for an accepted delete
// whose completion could not be observed.
func hubDeletePendingMessage(o hubDeleteOutcome) string {
	return fmt.Sprintf("delete accepted; cannot observe completion; local worktree kept (%s)", hubDeletePendingReason(o))
}

// hubRemovalPendingMessage is scion stop --rm's notice for the same case.
func hubRemovalPendingMessage(o hubDeleteOutcome) string {
	return fmt.Sprintf("removal accepted; cannot observe completion (%s)", hubDeletePendingReason(o))
}
