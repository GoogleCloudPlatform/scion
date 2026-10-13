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
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// startOutcomeLookupTimeout bounds the by-name lookup made after a start
// request whose outcome is unknown. A variable so tests can shorten it.
var startOutcomeLookupTimeout = 10 * time.Second

// envSubmitError marks an error returned by the env submit request itself,
// as opposed to an env-gather failure before anything was submitted.
type envSubmitError struct {
	err error
}

func (e *envSubmitError) Error() string { return "failed to submit gathered env: " + e.err.Error() }
func (e *envSubmitError) Unwrap() error { return e.err }

// envGatherCleanupSafe reports whether the CLI may delete the provisioning
// agent after env-gather failed with err. The env submit is where the Hub
// launches the agent, and the Hub runs it detached from this request
// (ptone/scion#1961), so after the submit is sent a missing or unclear
// answer can hide a launch that was accepted or has started. Deleting is
// safe only when:
//   - nothing was submitted (err is not an envSubmitError): the env could
//     not be satisfied, the user declined, or input could not be read; or
//   - the Hub definitely refused the submit: a 4xx other than 408 and 429
//     (not definite answers) and 409 (another operation owns the agent:
//     it has left provisioning, a start or launch holds it, or a delete
//     is in progress).
//
// A transport error or timeout, and any 5xx, leave the agent alone
// (ptone/scion#4007, ptone/scion#4817).
func envGatherCleanupSafe(err error) bool {
	var submitErr *envSubmitError
	if !errors.As(err, &submitErr) {
		return true
	}
	var apiErr *apiclient.APIError
	if !errors.As(submitErr.err, &apiErr) {
		return false
	}
	code := apiErr.StatusCode
	if code < 400 || code >= 500 {
		return false
	}
	switch code {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return false
	}
	return true
}

// envGatherCleanup deletes the provisioning agent of an env-gather that
// failed or was interrupted, but only while the env has not been
// submitted: once the submit is sent, the Hub may be launching the agent.
type envGatherCleanup struct {
	submitted atomic.Bool
	once      sync.Once
	del       func()
}

// markSubmitted records that the env submit is about to be sent. It is
// called before the request, so an interrupt that races the send leaves
// the agent alone.
func (c *envGatherCleanup) markSubmitted() {
	if c != nil {
		c.submitted.Store(true)
	}
}

// cleanupIfUnsubmitted is the interrupt path: it deletes the agent unless
// the env was submitted. It reports whether the delete ran.
func (c *envGatherCleanup) cleanupIfUnsubmitted() bool {
	if c == nil || c.submitted.Load() {
		return false
	}
	return c.run()
}

// cleanupAfterFailure is the error path: it deletes the agent when
// envGatherCleanupSafe(err) allows it. It reports whether the delete ran.
func (c *envGatherCleanup) cleanupAfterFailure(err error) bool {
	if c == nil || !envGatherCleanupSafe(err) {
		return false
	}
	return c.run()
}

// run deletes the agent at most once, so the interrupt and error paths
// cannot both delete.
func (c *envGatherCleanup) run() bool {
	ran := false
	c.once.Do(func() {
		if c.del != nil {
			c.del()
		}
		ran = true
	})
	return ran
}

// describeAgentAfterUnknownOutcome reads the agent by name after a start
// request whose outcome is unknown. found is true when the agent was read;
// desc then names its phase (and an active launch). When it was not read,
// desc says why, and gone is true for a definite not-found (a user caller's
// 404: an agent caller gets the same 404 for an agent it may not read).
func describeAgentAfterUnknownOutcome(get func(context.Context) (*hubclient.Agent, error)) (desc string, found, gone bool) {
	ctx, cancel := context.WithTimeout(context.Background(), startOutcomeLookupTimeout)
	defer cancel()
	a, err := get(ctx)
	switch {
	case err == nil && a != nil:
		phase, _ := hubAgentPhaseActivity(a.Phase, a.Activity, a.Status)
		if phase == "" {
			phase = "unknown"
		}
		desc = "phase " + phase
		if launchActive(a) {
			desc += ", launch active"
		}
		return desc, true, false
	case err == nil:
		return "its state could not be read", false, false
	case apiclient.IsNotFoundError(err) && !config.IsHubManagedAgent():
		return "it no longer exists", false, true
	default:
		return fmt.Sprintf("its state could not be read (%v)", err), false, false
	}
}

// envSubmitOutcomeUnknownError is the error for an env submit whose outcome
// is unknown: the agent was not deleted, and may have started.
func envSubmitOutcomeUnknownError(agentName string, err error, get func(context.Context) (*hubclient.Agent, error)) error {
	desc, found, gone := describeAgentAfterUnknownOutcome(get)
	if gone {
		return fmt.Errorf("env submit for agent '%s' did not complete (%w); %s", agentName, err, desc)
	}
	state := desc
	if found {
		state = "it is in " + desc
	}
	return fmt.Errorf("env submit for agent '%s' did not complete (%w); the agent may have started and was not deleted: %s. "+
		"Check it with 'scion list' or 'scion look %s'; delete it with 'scion delete %s' if you do not want it",
		agentName, err, state, agentName, agentName)
}

// createOutcomeUnknownError returns the error for a create request that
// failed in transport (for example a client timeout) while the Hub may
// still be creating the agent (ptone/scion#4007). It looks the agent up by
// name; when it exists, the error says so. Otherwise, or when err is a Hub
// answer (an *apiclient.APIError, which is definite), it returns nil and
// the caller reports err as before.
func createOutcomeUnknownError(agentName string, err error, get func(context.Context) (*hubclient.Agent, error)) error {
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		return nil
	}
	desc, found, _ := describeAgentAfterUnknownOutcome(get)
	if !found {
		return nil
	}
	return fmt.Errorf("create request for agent '%s' did not complete (%w); the Hub may still be creating it: %s. "+
		"Check it with 'scion list' or 'scion look %s' before retrying",
		agentName, err, desc, agentName)
}
