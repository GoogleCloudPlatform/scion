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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// hubDeletionWaitOptions tunes the poll after a 202 from DELETE. Zero values
// take the hubclient defaults (2s interval, 180s timeout). Tests override it.
var hubDeletionWaitOptions hubclient.DeletionWaitOptions

// hubDeleteOutcome is what a hub delete (and, on 202, the poll) ended in.
type hubDeleteOutcome struct {
	// Accepted is true when the hub answered 202 and the poll ran.
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
func deleteViaHubAndWait(ctx context.Context, svc hubclient.AgentService, agentName string, opts *hubclient.DeleteAgentOptions, onAccepted func()) (hubDeleteOutcome, error) {
	res, err := hubclient.DeleteWithResult(ctx, svc, agentName, opts)
	if err != nil {
		return hubDeleteOutcome{}, err
	}
	if !res.Accepted {
		return hubDeleteOutcome{}, nil
	}
	if onAccepted != nil {
		onAccepted()
	}
	pollID := res.AgentID
	if pollID == "" {
		pollID = agentName
	}
	// The poll has its own budget, independent of the DELETE request's ctx,
	// which may have little time left after the hub's ~20s wait.
	wait := hubclient.WaitForAgentDeletion(context.WithoutCancel(ctx), svc, pollID, hubDeletionWaitOptions)
	return hubDeleteOutcome{Accepted: true, Wait: wait}, nil
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
		code, msg := "unknown", ""
		if d := o.Wait.Deletion; d != nil {
			if d.Code != "" {
				code = d.Code
			}
			msg = d.Error
		}
		if msg != "" {
			msg = ": " + msg
		}
		blocked := ""
		switch code {
		case "in_doubt", "revoke_failed", "finalize_failed":
			// in_doubt: a cross-node teardown is still outstanding;
			// revoke_failed/finalize_failed: the row is stuck in finalizing.
			blocked = " Starting the agent stays blocked until a retry succeeds or force is used."
		}
		return fmt.Errorf("delete failed on the Hub (%s)%s; %s. Retry with 'scion delete %s', or force-delete it from the web UI.%s",
			code, msg, what, agentName, blocked)
	case hubclient.DeletionNotTaken:
		return fmt.Errorf("delete did not take effect (the agent is still live and no delete is running); %s. Retry with 'scion delete %s'",
			what, agentName)
	}
	return nil
}

// hubDeletePendingMessage is the notice for an accepted delete whose
// completion could not be observed (403 on the poll, or the poll timed
// out). That is not a failure: the hub owns the delete and will finish or
// fail it.
func hubDeletePendingMessage(o hubDeleteOutcome, what string) string {
	reason := "the Hub did not allow reading the agent"
	if o.Wait.Outcome == hubclient.DeletionTimedOut {
		reason = "still running when the wait ended"
	}
	return fmt.Sprintf("delete accepted; cannot observe completion; %s (%s)", what, reason)
}
