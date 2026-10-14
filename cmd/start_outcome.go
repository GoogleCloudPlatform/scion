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
	"net"
	"net/http"
	"net/url"
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

// errEnvGatherInterrupted is returned, before anything is submitted, when
// an interrupt began deleting the agent first.
var errEnvGatherInterrupted = errors.New("env-gather interrupted")

// envGatherCleanup states. Every transition is a compare-and-swap, so the
// delete and the env submit are mutually exclusive: an interrupt that
// begins the delete stops the submit from being sent, and a submit that is
// marked first stops the interrupt from deleting.
const (
	envCleanupIdle int32 = iota
	envCleanupSubmitted
	envCleanupDeleting
)

// envGatherCleanup deletes the provisioning agent of an env-gather that
// failed or was interrupted, but only while the env has not been
// submitted (once the submit is sent, the Hub may be launching the agent),
// or after the Hub definitely refused the submit. At most one delete runs.
type envGatherCleanup struct {
	state atomic.Int32
	done  chan struct{} // closed when a started delete has finished
	del   func()
}

func newEnvGatherCleanup(del func()) *envGatherCleanup {
	return &envGatherCleanup{done: make(chan struct{}), del: del}
}

// markSubmitted moves idle to submitted just before the env submit is
// sent. It reports false when a delete has already begun; the submit must
// then not be sent. A nil cleanup always allows the submit.
func (c *envGatherCleanup) markSubmitted() bool {
	if c == nil {
		return true
	}
	return c.state.CompareAndSwap(envCleanupIdle, envCleanupSubmitted)
}

// cleanupIfUnsubmitted is the interrupt path: it deletes the agent only if
// the env has not been submitted (idle to deleting). It reports whether it
// ran the delete.
func (c *envGatherCleanup) cleanupIfUnsubmitted() bool {
	if c == nil || !c.state.CompareAndSwap(envCleanupIdle, envCleanupDeleting) {
		return false
	}
	c.runDelete()
	return true
}

// cleanupAfterFailure is the error path for a failure err that
// envGatherCleanupSafe allows: before the submit (idle to deleting) or
// after a definite refusal (submitted to deleting). When the interrupt
// path is already deleting, it waits for that delete to finish so the
// process does not exit under it. It reports whether it ran the delete.
func (c *envGatherCleanup) cleanupAfterFailure(err error) bool {
	if c == nil || !envGatherCleanupSafe(err) {
		return false
	}
	if c.state.CompareAndSwap(envCleanupIdle, envCleanupDeleting) ||
		c.state.CompareAndSwap(envCleanupSubmitted, envCleanupDeleting) {
		c.runDelete()
		return true
	}
	if c.state.Load() == envCleanupDeleting && c.done != nil {
		<-c.done
	}
	return false
}

func (c *envGatherCleanup) runDelete() {
	if c.del != nil {
		c.del()
	}
	if c.done != nil {
		close(c.done)
	}
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
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) && apiErr.Code == apiclient.ErrCodeDeleteInProgress {
		// A delete already owns the agent: do not suggest another.
		return fmt.Errorf("env submit for agent '%s' did not complete (%w); a delete of the agent is in progress: %s",
			agentName, err, state)
	}
	return fmt.Errorf("env submit for agent '%s' did not complete (%w); the agent may have started and was not deleted: %s. "+
		"Check it with 'scion list' or 'scion look %s'; delete it with 'scion delete %s' if you do not want it",
		agentName, err, state, agentName, agentName)
}

// isTransportOutcomeUnknown reports whether err is a request that may have
// reached the Hub without an answer: a transport error or timeout (the
// apiclient wraps the *url.Error), a context deadline, or a 2xx answer
// with no body. Local failures (a cancelled prompt, unreadable input) are
// not: nothing was sent.
func isTransportOutcomeUnknown(err error) bool {
	var urlErr *url.Error
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &urlErr) ||
		errors.As(err, &netErr) || errors.Is(err, apiclient.ErrNoContent)
}

// createOutcomeUnknownError returns the error for a create request that
// failed in transport (for example a client timeout) while the Hub may
// still be creating, or for an existing agent starting, it
// (ptone/scion#4007). It looks the agent up by name; when it exists, the
// error says so. Otherwise, or when err is not a transport failure (a Hub
// answer is definite, and a local failure sent nothing), it returns nil
// and the caller reports err as before. existing reports that the agent
// existed before this start (a resume or restart).
func createOutcomeUnknownError(agentName string, err error, existing bool, get func(context.Context) (*hubclient.Agent, error)) error {
	if !isTransportOutcomeUnknown(err) {
		return nil
	}
	desc, found, _ := describeAgentAfterUnknownOutcome(get)
	if !found {
		return nil
	}
	verb := "creating"
	if existing {
		verb = "starting"
	}
	return fmt.Errorf("create request for agent '%s' did not complete (%w); the Hub may still be %s it: %s. "+
		"Check it with 'scion list' or 'scion look %s' before retrying",
		agentName, err, verb, desc, agentName)
}
