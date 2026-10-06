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

package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// Deletion states reported in DeletionInfo.State.
const (
	DeletionStateDeleting = "deleting" // includes the hub's finalizing step
	DeletionStateFailed   = "failed"
)

// DeletionInfo is the hub's view of an active or failed agent delete
// (design ptone/scion#2483 §2.2). It mirrors store.DeletionInfo field for
// field; the JSON is the contract.
type DeletionInfo struct {
	State          string     `json:"state"`          // "deleting" | "failed"
	Code           string     `json:"code,omitempty"` // runtime_error | conflict | in_doubt | abandoned | revoke_failed | finalize_failed
	Error          string     `json:"error,omitempty"`
	Soft           bool       `json:"soft"`
	Claim          int64      `json:"claim"`
	StartedAt      time.Time  `json:"startedAt"`
	LeaseExpiresAt *time.Time `json:"leaseExpiresAt,omitempty"`
	ExpiresAt      *time.Time `json:"expiresAt,omitempty"`
	// Stage is "finalizing" when the hub row is finalizing (teardown done;
	// the row never expires from view and blocks start until a retry or
	// force); omitted otherwise. Mirrors store.DeletionInfo.Stage.
	Stage string `json:"stage,omitempty"`
}

// DeleteResult is the outcome of a DELETE that the hub answered with 2xx.
type DeleteResult struct {
	// Accepted is true when the hub answered 202: the delete is still running
	// in the background, and its completion has to be observed (see
	// WaitForAgentDeletion). False means the hub answered 204, or, from the
	// DeleteWithResult package func's fallback, some 2xx that it cannot tell
	// apart from a 202. Callers that remove local state must not treat the
	// fallback as proof of completion (see AgentDeleteResulter).
	Accepted bool
	// AgentID is the hub's agent ID from the 202 body. Empty on 204.
	AgentID string
	// Deletion is the hub's deletion view from the 202 body, if any.
	Deletion *DeletionInfo
}

// AgentDeleteResulter is implemented by AgentService values that can report
// whether a delete finished (204) or was accepted (202). It is a separate
// interface, not part of AgentService, so existing AgentService
// implementations outside this package keep compiling. Callers that remove
// local state once a delete is done should type-assert this interface and
// fail closed when it is absent, rather than use the package func's fallback.
type AgentDeleteResulter interface {
	DeleteWithResult(ctx context.Context, agentID string, opts *DeleteAgentOptions) (DeleteResult, error)
}

// DeleteWithResult deletes an agent and reports whether the hub finished the
// delete (204) or accepted it to finish in the background (202). Errors
// (4xx, 502, 503) are returned as *apiclient.APIError, exactly as Delete
// returns them.
func (s *agentService) DeleteWithResult(ctx context.Context, agentID string, opts *DeleteAgentOptions) (DeleteResult, error) {
	resp, err := s.c.delete(ctx, s.deletePath(agentID, opts), nil)
	if err != nil {
		return DeleteResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return DeleteResult{}, apiclient.ParseErrorResponse(resp)
	}
	if resp.StatusCode != http.StatusAccepted {
		return DeleteResult{}, nil
	}

	res := DeleteResult{Accepted: true}
	var body struct {
		AgentID  string        `json:"agentId"`
		Deletion *DeletionInfo `json:"deletion"`
	}
	// The body is informational; a missing or malformed body still means
	// "accepted", and the caller polls by the ID it already has.
	if data, err := io.ReadAll(resp.Body); err == nil && len(data) > 0 {
		if json.Unmarshal(data, &body) == nil {
			res.AgentID = body.AgentID
			res.Deletion = body.Deletion
		}
	}
	return res, nil
}

// DeleteWithResult calls svc.DeleteWithResult when svc implements
// AgentDeleteResulter. Otherwise it falls back to svc.Delete. Plain Delete
// cannot tell a 204 from a 202, so the fallback returns Accepted=false on any
// 2xx: the caller behaves as it did before 202 existed (no poll). Every
// AgentService returned by this package implements AgentDeleteResulter; only
// third-party implementations (test doubles) take the fallback.
func DeleteWithResult(ctx context.Context, svc AgentService, agentID string, opts *DeleteAgentOptions) (DeleteResult, error) {
	if r, ok := svc.(AgentDeleteResulter); ok {
		return r.DeleteWithResult(ctx, agentID, opts)
	}
	if err := svc.Delete(ctx, agentID, opts); err != nil {
		return DeleteResult{}, err
	}
	return DeleteResult{}, nil
}

// DeletionWaitOutcome classifies how an accepted (202) delete ended, as far
// as the client could observe.
type DeletionWaitOutcome int

const (
	// DeletionConfirmed: the agent is gone (404) or soft-deleted (deletedAt
	// set). Local cleanup may proceed.
	DeletionConfirmed DeletionWaitOutcome = iota
	// DeletionFailed: the hub reports deletion.state == failed. Result.Deletion
	// carries the code and message.
	DeletionFailed
	// DeletionNotTaken: after the 202 the agent is live with no deletion
	// marker, so no delete is running. The delete did not take effect.
	DeletionNotTaken
	// DeletionUnobservable: the caller may not read the agent (401/403), or
	// the hub rejected the read with another non-retryable 4xx (anything but
	// 404, 408 and 429), so completion cannot be observed. Err says which.
	// The delete was still accepted.
	DeletionUnobservable
	// DeletionTimedOut: the wait ended before an outcome was seen: the delete
	// was still running, or every poll since the last good read failed (Err
	// is then the last poll error), or ctx was canceled (Err carries
	// ctx.Err()).
	DeletionTimedOut
)

// String returns a short, stable name for the outcome.
func (o DeletionWaitOutcome) String() string {
	switch o {
	case DeletionConfirmed:
		return "confirmed"
	case DeletionFailed:
		return "failed"
	case DeletionNotTaken:
		return "not_taken"
	case DeletionUnobservable:
		return "unobservable"
	case DeletionTimedOut:
		return "timed_out"
	}
	return fmt.Sprintf("outcome(%d)", int(o))
}

// Defaults for WaitForAgentDeletion (design ptone/scion#2483 §2.4).
const (
	DefaultDeletionPollInterval = 2 * time.Second
	DefaultDeletionPollTimeout  = 180 * time.Second
	// DefaultDeletionNotTakenPolls is how many consecutive polls must see a
	// live row with no deletion marker before the wait reports
	// DeletionNotTaken. Two polls (one interval apart) allow for
	// read-after-write lag between hub nodes.
	DefaultDeletionNotTakenPolls = 2
)

// DeletionWaitOptions tunes WaitForAgentDeletion. Zero values take the
// defaults. Now and After exist so tests can drive the clock.
type DeletionWaitOptions struct {
	Interval      time.Duration
	Timeout       time.Duration
	NotTakenPolls int
	Now           func() time.Time
	After         func(time.Duration) <-chan time.Time
}

// DeletionWaitResult is what WaitForAgentDeletion observed.
type DeletionWaitResult struct {
	Outcome DeletionWaitOutcome
	// Deletion is the last deletion view seen (set for DeletionFailed).
	Deletion *DeletionInfo
	// Err is the error behind DeletionUnobservable, or the last transient
	// poll error before DeletionTimedOut.
	Err error
	// Polls is the number of GET requests made.
	Polls int
}

// WaitForAgentDeletion polls svc.Get(agentID) after a 202 until the delete is
// confirmed, fails, is found not to be running, cannot be observed, or the
// timeout passes. svc should be project-scoped (Client.ProjectAgents), so the
// poll reads GET /projects/{pid}/agents/{agentId}.
//
// Transient errors (network, 5xx, 408, 429) keep the poll going until the
// timeout. Each GET gets a deadline capped at the remaining budget, so the
// whole wait stays within Timeout (plus scheduling slack).
func WaitForAgentDeletion(ctx context.Context, svc AgentService, agentID string, opts DeletionWaitOptions) DeletionWaitResult {
	if opts.Interval <= 0 {
		opts.Interval = DefaultDeletionPollInterval
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultDeletionPollTimeout
	}
	if opts.NotTakenPolls <= 0 {
		opts.NotTakenPolls = DefaultDeletionNotTakenPolls
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.After == nil {
		opts.After = time.After
	}

	start := opts.Now()
	var res DeletionWaitResult
	liveWithoutMarker := 0
	for {
		res.Polls++
		// Cap this GET at the remaining budget, measured on opts.Now (so a
		// fake clock in tests still yields a positive duration).
		remaining := opts.Timeout - opts.Now().Sub(start)
		if remaining <= 0 {
			remaining = time.Millisecond
		}
		pollCtx, cancel := context.WithTimeout(ctx, remaining)
		ag, err := svc.Get(pollCtx, agentID)
		budgetSpent := err != nil && ctx.Err() == nil && errors.Is(pollCtx.Err(), context.DeadlineExceeded)
		cancel()
		if budgetSpent {
			// The wait's own deadline cut this GET short. That says nothing
			// about the hub, so keep the last observation (a good read leaves
			// Err nil); only a first poll has nothing better to report.
			if res.Polls == 1 {
				res.Err = err
			}
			res.Outcome = DeletionTimedOut
			return res
		}
		switch {
		case err != nil:
			var apiErr *apiclient.APIError
			switch {
			case errors.As(err, &apiErr) && apiErr.IsNotFound():
				res.Outcome = DeletionConfirmed
				res.Err = nil
				return res
			case errors.As(err, &apiErr) && (apiErr.IsForbidden() || apiErr.IsUnauthorized() || nonRetryable4xx(apiErr.StatusCode)):
				res.Outcome = DeletionUnobservable
				res.Err = err
				return res
			}
			res.Err = err
		case ag == nil:
			res.Err = errors.New("empty agent response")
		case !ag.DeletedAt.IsZero():
			res.Outcome = DeletionConfirmed
			res.Deletion = ag.Deletion
			res.Err = nil
			return res
		case ag.Deletion == nil:
			res.Err = nil
			res.Deletion = nil
			liveWithoutMarker++
			if liveWithoutMarker >= opts.NotTakenPolls {
				res.Outcome = DeletionNotTaken
				return res
			}
		case ag.Deletion.State == DeletionStateFailed:
			res.Outcome = DeletionFailed
			res.Deletion = ag.Deletion
			res.Err = nil
			return res
		default: // deleting (or a state this client does not know): keep polling
			res.Err = nil
			res.Deletion = ag.Deletion
			liveWithoutMarker = 0
		}

		if opts.Now().Sub(start) >= opts.Timeout {
			res.Outcome = DeletionTimedOut
			return res
		}
		select {
		case <-ctx.Done():
			res.Outcome = DeletionTimedOut
			if res.Err == nil {
				res.Err = ctx.Err()
			}
			return res
		case <-opts.After(opts.Interval):
		}
	}
}

// nonRetryable4xx reports whether a poll status means retrying cannot help.
// 404 is handled as "gone" before this is asked; 408 and 429 are retried.
func nonRetryable4xx(status int) bool {
	return status >= 400 && status < 500 &&
		status != http.StatusNotFound &&
		status != http.StatusRequestTimeout &&
		status != http.StatusTooManyRequests
}
