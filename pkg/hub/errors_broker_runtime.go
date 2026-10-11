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
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// MissingEnvVars writes a 422 Unprocessable Entity response when required
// environment variables cannot be resolved from available sources.
func MissingEnvVars(w http.ResponseWriter, keys []string, envInfo *EnvGatherResponse) {
	details := map[string]interface{}{
		"missingKeys": keys,
	}
	if envInfo != nil {
		details["envGather"] = envInfo
	}
	writeError(w, http.StatusUnprocessableEntity, ErrCodeMissingEnvVars,
		fmt.Sprintf("Cannot start agent: %d required environment variable(s) are missing: %s",
			len(keys), strings.Join(keys, ", ")),
		details)
}

// brokerCodeRuntimeUnavailable is the runtime broker's error code for a 503
// meaning the runtime that holds the agent is not available on the broker
// right now — for an existing-agent request, typically because no runtime of
// the agent's recorded type is registered there (ptone/scion#2748).
const brokerCodeRuntimeUnavailable = "runtime_unavailable"

// defaultBrokerRuntimeRetryAfter is the Retry-After the hub sends with a
// relayed runtime_unavailable 503 when the broker gave no usable value.
const defaultBrokerRuntimeRetryAfter = "30"

// isBrokerRuntimeUnavailable reports whether err is a runtime broker's 503
// answer with error code runtime_unavailable.
func isBrokerRuntimeUnavailable(err error) bool {
	var se *brokerStatusError
	return errors.As(err, &se) && se.StatusCode == http.StatusServiceUnavailable &&
		se.brokerErrorCode() == brokerCodeRuntimeUnavailable
}

// isRestartStopTolerable reports whether a restart's stop-leg error means the
// agent has no running instance on its broker, so the start leg may proceed.
// The current broker stop route answers 202 for an absent agent, so these
// codes are defensive, for older brokers or proxies: a 404 agent_not_found
// or a 409 agent_not_running. Any other error, including a
// runtime_unavailable 503, leaves the old instance's state unknown.
//
// A code accepted here must mean the old instance is not running: when the
// restart's start leg then fails, handleAgentLifecycle settles the
// reservation and records the agent as stopped on that assumption (the
// final else after the start leg's dispatchErr checks), with no further
// stop.
func isRestartStopTolerable(err error) bool {
	var se *brokerStatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.StatusCode {
	case http.StatusNotFound:
		return se.brokerErrorCode() == ErrCodeAgentNotFound
	case http.StatusConflict:
		return se.brokerErrorCode() == ErrCodeAgentNotRunning
	}
	return false
}

// restartStopFailedRetryAfter is the Retry-After sent when a restart is
// aborted because its stop leg failed.
const restartStopFailedRetryAfter = "30"

// brokerCodePattern bounds a broker error code copied into a hub response.
var brokerCodePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// writeRestartStopFailed writes the retryable 503 for a restart aborted
// because its stop leg failed and the start leg was not dispatched. The
// message is fixed; when stopErr is a broker answer with a well-formed error
// code, details.brokerCode carries that code (never the raw body) so the
// cause can be diagnosed.
func writeRestartStopFailed(w http.ResponseWriter, stopErr error) {
	var details map[string]interface{}
	var se *brokerStatusError
	if errors.As(stopErr, &se) {
		if code := se.brokerErrorCode(); brokerCodePattern.MatchString(code) {
			details = map[string]interface{}{"brokerCode": code}
		}
	}
	w.Header().Set("Retry-After", restartStopFailedRetryAfter)
	writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
		"Restart not performed: the agent's current instance could not be stopped; retry later", details)
}

// writeBrokerRuntimeUnavailable relays a broker's runtime_unavailable 503 for
// an existing-agent operation as a retryable 503 (with Retry-After) instead of
// the generic 502, and reports whether it did; for any other error it writes
// nothing and returns false. runtime is the agent's recorded runtime type.
// Like the logs relay, the message is the hub's own text rather than the
// broker's response body, and the broker's Retry-After is used only if it is
// a positive number of seconds.
func writeBrokerRuntimeUnavailable(w http.ResponseWriter, err error, runtime string) bool {
	if !isBrokerRuntimeUnavailable(err) {
		return false
	}
	w.Header().Set("Retry-After", brokerRuntimeRetryAfter(err))
	writeError(w, http.StatusServiceUnavailable, brokerCodeRuntimeUnavailable, brokerRuntimeUnavailableMessage(runtime), nil)
	return true
}

// brokerRuntimeRetryAfter is the Retry-After to send for a broker's
// runtime_unavailable answer: the broker's value if it is a positive number
// of seconds, otherwise defaultBrokerRuntimeRetryAfter.
func brokerRuntimeRetryAfter(err error) string {
	var se *brokerStatusError
	if errors.As(err, &se) {
		if n, convErr := strconv.Atoi(strings.TrimSpace(se.RetryAfter)); convErr == nil && n > 0 {
			return strconv.Itoa(n)
		}
	}
	return defaultBrokerRuntimeRetryAfter
}

// brokerRuntimeUnavailableMessage is the hub's client-facing text for a
// broker's runtime_unavailable answer; runtime is the agent's recorded
// runtime type.
func brokerRuntimeUnavailableMessage(runtime string) string {
	if rt := dispatchRecordedRuntime(runtime); rt != "" {
		return fmt.Sprintf("Runtime %q is not available on the agent's runtime broker; retry later or check the broker's runtime configuration", rt)
	}
	return "The agent's runtime is not available on its runtime broker; retry later or check the broker's runtime configuration"
}

// RuntimeTargetRefusal is a typed flat Runtime Broker refusal raised by the
// Hub (.design/flat-runtime-brokers-contract.md sections 7 and 9): a
// correctness or compatibility refusal, never an authorization result.
// Handlers write it with its own Status, Code and Details; it is classified
// as a confirmed not-acted-on start error.
type RuntimeTargetRefusal struct {
	Code    string
	Status  int
	Message string
	Details map[string]interface{}
}

func (e *RuntimeTargetRefusal) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}
