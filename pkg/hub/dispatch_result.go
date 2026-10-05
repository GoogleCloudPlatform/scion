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

package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A cross-node dispatch runs on the node that holds the broker connection, and
// the node that took the request reads the outcome from the broker_dispatch
// row. When the broker answers with an HTTP error status, the executing node
// writes that answer into the failed row's result column as a broker error
// envelope, so the originating node can return the same typed error a direct
// dispatch returns.

// dispatchBrokerError is the broker's HTTP error answer as carried on a
// failed broker_dispatch row. Body, Status and RetryAfter are authoritative;
// Code is the body's error.code, copied for readers of the row and never
// used to rebuild the error.
type dispatchBrokerError struct {
	Status     int    `json:"status"`
	Code       string `json:"code,omitempty"`
	Body       string `json:"body"`
	RetryAfter string `json:"retryAfter,omitempty"`
}

// dispatchFailureEnvelope is the JSON shape of a failed row's result. It
// carries the typed failures the originating node's callers act on:
//   - BrokerError is the broker's HTTP error answer;
//   - EnvStillMissing is the requirements of an *ErrEnvStillMissing (a
//     finalize that still lacks required env keys);
//   - HubErrors names the hub sentinel errors in the executing node's error
//     chain (see dispatchHubSentinels), such as a delete holding the row.
//
// They are rebuilt in that order of precedence.
type dispatchFailureEnvelope struct {
	BrokerError     *dispatchBrokerError           `json:"brokerError,omitempty"`
	EnvStillMissing *RemoteEnvRequirementsResponse `json:"envStillMissing,omitempty"`
	HubErrors       []string                       `json:"hubErrors,omitempty"`
}

// dispatchHubSentinels are the hub sentinel errors the HTTP handlers answer
// with their own status when a dispatch fails with them, keyed by the name
// recorded on a failed row. An unknown name read from a row is ignored.
var dispatchHubSentinels = []struct {
	name string
	err  error
}{
	{"delete_in_progress", store.ErrDeleteInProgress},
	{"launch_invalid_phase", ErrLaunchInvalidPhase},
}

// dispatchHubError is a failure the executing node returned with hub
// sentinel errors in its chain, rebuilt on the originating node: the
// original error text, matching the same sentinels under errors.Is.
type dispatchHubError struct {
	msg       string
	sentinels []error
}

func (e *dispatchHubError) Error() string   { return e.msg }
func (e *dispatchHubError) Unwrap() []error { return e.sentinels }

// dispatchFailureResult returns the result to record on a failed dispatch row
// for execErr: an envelope carrying the broker's HTTP error answer and/or the
// still-missing env requirements found in execErr's chain, else "". The
// broker body is cut to maxBrokerErrorBodyBytes, the same bound the HTTP
// transport applies when it reads an error body.
func dispatchFailureResult(execErr error) string {
	var env dispatchFailureEnvelope
	var se *brokerStatusError
	if errors.As(execErr, &se) && isHTTPErrorStatus(se.StatusCode) {
		env.BrokerError = &dispatchBrokerError{
			Status:     se.StatusCode,
			Code:       se.brokerErrorCode(),
			Body:       cutBrokerErrorBody(se.Body),
			RetryAfter: se.RetryAfter,
		}
	}
	var missing *ErrEnvStillMissing
	if errors.As(execErr, &missing) && hasEnvNeeds(missing.Requirements) {
		env.EnvStillMissing = missing.Requirements
	}
	for _, hs := range dispatchHubSentinels {
		if errors.Is(execErr, hs.err) {
			env.HubErrors = append(env.HubErrors, hs.name)
		}
	}
	if env.BrokerError == nil && env.EnvStillMissing == nil && len(env.HubErrors) == 0 {
		return ""
	}
	out, err := json.Marshal(env)
	if err != nil {
		return ""
	}
	return string(out)
}

// dispatchFailureError returns the error for a failed dispatch row. When the
// row carries a valid envelope the typed failure is rebuilt and wrapped, so
// errors.As and errors.Is find the same *brokerStatusError,
// *ErrEnvStillMissing or hub sentinel a direct dispatch returns. Otherwise
// the row's error text is returned, as before.
func dispatchFailureError(d *store.BrokerDispatch) error {
	env := decodeDispatchFailure(d.Result)
	if se := brokerErrorFromEnvelope(env); se != nil {
		return fmt.Errorf("dispatch %s failed: %w", d.Op, se)
	}
	if env != nil && hasEnvNeeds(env.EnvStillMissing) {
		return fmt.Errorf("dispatch %s failed: %w", d.Op, &ErrEnvStillMissing{Requirements: env.EnvStillMissing})
	}
	if sentinels := hubSentinelsFromEnvelope(env); len(sentinels) > 0 {
		return fmt.Errorf("dispatch %s failed: %w", d.Op, &dispatchHubError{msg: d.Error, sentinels: sentinels})
	}
	return fmt.Errorf("dispatch %s failed: %s", d.Op, d.Error)
}

// decodeDispatchFailure decodes a failed row's result, or returns nil when it
// is empty or not an envelope.
func decodeDispatchFailure(result string) *dispatchFailureEnvelope {
	if result == "" {
		return nil
	}
	var env dispatchFailureEnvelope
	if err := json.Unmarshal([]byte(result), &env); err != nil {
		return nil
	}
	return &env
}

// brokerErrorFromResult decodes a failed row's result into the broker's
// error, or returns nil when the result has no valid broker error.
func brokerErrorFromResult(result string) *brokerStatusError {
	return brokerErrorFromEnvelope(decodeDispatchFailure(result))
}

// brokerErrorFromEnvelope rebuilds the broker's error from status, body and
// retryAfter, or returns nil when there is none or its status is outside
// 400-599.
func brokerErrorFromEnvelope(env *dispatchFailureEnvelope) *brokerStatusError {
	if env == nil || env.BrokerError == nil {
		return nil
	}
	be := env.BrokerError
	if !isHTTPErrorStatus(be.Status) {
		return nil
	}
	return &brokerStatusError{StatusCode: be.Status, Body: be.Body, RetryAfter: be.RetryAfter}
}

// hubSentinelsFromEnvelope returns the known hub sentinel errors named by
// the envelope, ignoring unknown names.
func hubSentinelsFromEnvelope(env *dispatchFailureEnvelope) []error {
	if env == nil {
		return nil
	}
	var out []error
	for _, name := range env.HubErrors {
		for _, hs := range dispatchHubSentinels {
			if hs.name == name {
				out = append(out, hs.err)
			}
		}
	}
	return out
}

// hasEnvNeeds reports whether reqs names at least one still-missing key; a
// still-missing answer without one is not carried.
func hasEnvNeeds(reqs *RemoteEnvRequirementsResponse) bool {
	return reqs != nil && len(reqs.Needs) > 0
}

func isHTTPErrorStatus(code int) bool { return code >= 400 && code <= 599 }

// cutBrokerErrorBody returns body cut to maxBrokerErrorBodyBytes. The cut
// backs off to the start of a UTF-8 sequence by at most utf8.UTFMax-1 bytes,
// so a multi-byte character is never split.
func cutBrokerErrorBody(body string) string {
	if len(body) <= maxBrokerErrorBodyBytes {
		return body
	}
	n := maxBrokerErrorBodyBytes
	for n > maxBrokerErrorBodyBytes-(utf8.UTFMax-1) && !utf8.RuneStart(body[n]) {
		n--
	}
	return body[:n]
}
