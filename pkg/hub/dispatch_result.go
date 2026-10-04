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

// dispatchFailureEnvelope is the JSON shape of a failed row's result.
type dispatchFailureEnvelope struct {
	BrokerError *dispatchBrokerError `json:"brokerError,omitempty"`
}

// dispatchFailureResult returns the result to record on a failed dispatch row
// for execErr: the broker error envelope when execErr carries the broker's
// HTTP error answer, else "". The body is cut to maxBrokerErrorBodyBytes, the
// same bound the HTTP transport applies when it reads an error body.
func dispatchFailureResult(execErr error) string {
	var se *brokerStatusError
	if !errors.As(execErr, &se) || !isHTTPErrorStatus(se.StatusCode) {
		return ""
	}
	out, err := json.Marshal(dispatchFailureEnvelope{BrokerError: &dispatchBrokerError{
		Status:     se.StatusCode,
		Code:       se.brokerErrorCode(),
		Body:       cutBrokerErrorBody(se.Body),
		RetryAfter: se.RetryAfter,
	}})
	if err != nil {
		return ""
	}
	return string(out)
}

// dispatchFailureError returns the error for a failed dispatch row. When the
// row carries a valid broker error envelope the broker's error is rebuilt and
// wrapped, so errors.As finds the same *brokerStatusError a direct dispatch
// returns. Otherwise the row's error text is returned, as before.
func dispatchFailureError(d *store.BrokerDispatch) error {
	if se := brokerErrorFromResult(d.Result); se != nil {
		return fmt.Errorf("dispatch %s failed: %w", d.Op, se)
	}
	return fmt.Errorf("dispatch %s failed: %s", d.Op, d.Error)
}

// brokerErrorFromResult decodes a failed row's result into the broker's
// error, or returns nil when the result is empty, not an envelope, or has a
// status outside 400-599.
func brokerErrorFromResult(result string) *brokerStatusError {
	if result == "" {
		return nil
	}
	var env dispatchFailureEnvelope
	if err := json.Unmarshal([]byte(result), &env); err != nil || env.BrokerError == nil {
		return nil
	}
	be := env.BrokerError
	if !isHTTPErrorStatus(be.Status) {
		return nil
	}
	return &brokerStatusError{StatusCode: be.Status, Body: be.Body, RetryAfter: be.RetryAfter}
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
