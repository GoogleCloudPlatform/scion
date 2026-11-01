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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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

// isBrokerReloadUnconfirmed reports whether err is a runtime broker's 502
// answer with error code reload_unconfirmed: a reset-auth wrote the new
// credential into the agent container, but the running agent did not
// confirm the reload (ptone/scion#5460).
func isBrokerReloadUnconfirmed(err error) bool {
	var se *brokerStatusError
	return errors.As(err, &se) && se.StatusCode == http.StatusBadGateway &&
		se.brokerErrorCode() == api.BrokerErrCodeReloadUnconfirmed
}

// writeBrokerReloadUnconfirmed writes the hub's 502 reload_unconfirmed answer
// for err when it is a broker's reload_unconfirmed answer, and reports
// whether it did. The message is the fixed shared text; the broker's body is
// not echoed.
func writeBrokerReloadUnconfirmed(w http.ResponseWriter, err error) bool {
	if !isBrokerReloadUnconfirmed(err) {
		return false
	}
	writeError(w, http.StatusBadGateway, api.BrokerErrCodeReloadUnconfirmed, api.ReloadUnconfirmedMessage, nil)
	return true
}

// isBrokerRuntimeUnavailable reports whether err is a runtime broker's 503
// answer with error code runtime_unavailable.
func isBrokerRuntimeUnavailable(err error) bool {
	var se *brokerStatusError
	return errors.As(err, &se) && se.StatusCode == http.StatusServiceUnavailable &&
		se.brokerErrorCode() == brokerCodeRuntimeUnavailable
}

// isBrokerDraining reports whether err is a runtime broker's 503 answer with
// error code broker_draining (api.BrokerErrCodeBrokerDraining): the broker
// is shutting down and refused the request before any start tracker,
// registration or file work, so it did not act. A start that gets it is
// released like one refused with runtime_unavailable (startOutcomeOf), so
// a retry after the broker's Retry-After is not blocked by an unconfirmed
// claim (design wave 3, Optional-2). The start-failure revoke stays
// disarmed for it (isConfirmedStartNotActedOnError is not changed).
func isBrokerDraining(err error) bool {
	var se *brokerStatusError
	return errors.As(err, &se) && se.StatusCode == http.StatusServiceUnavailable &&
		se.brokerErrorCode() == api.BrokerErrCodeBrokerDraining
}

// defaultBrokerDrainingRetryAfter is the Retry-After the hub sends with a
// relayed broker_draining 503 when the broker gave no usable value (the
// broker sends 5).
const defaultBrokerDrainingRetryAfter = "5"

// brokerDrainingMessage is the hub's client-facing text for a broker's
// broker_draining answer.
const brokerDrainingMessage = "The agent's runtime broker is shutting down and did not act on the request; retry shortly"

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

// brokerRuntimeCauseDetails returns {"cause": code} when err is a broker
// runtime_error answer whose details.cause is one of the known codes
// (api.IsBrokerRuntimeCause) and well-formed (brokerCodePattern), else nil.
// Only the fixed code is copied, never the broker's message or any other
// detail (ptone/scion#4555).
func brokerRuntimeCauseDetails(err error) map[string]interface{} {
	var se *brokerStatusError
	if !errors.As(err, &se) || se.brokerErrorCode() != ErrCodeRuntimeError {
		return nil
	}
	code, _ := se.brokerErrorDetails()["cause"].(string)
	if code = relayableRuntimeCause(code); code == "" {
		return nil
	}
	return map[string]interface{}{"cause": code}
}

// relayableRuntimeCause returns code when it is a broker runtime cause the
// hub may pass to a client (a create's details.cause, or a failed launch
// report's stored cause), else "". The api.IsBrokerRuntimeCause allowlist is
// the real filter: only its fixed codes may reach the client. The
// brokerCodePattern check cannot reject a code the allowlist accepts today;
// it is an extra safeguard, not a replacement, so keep the allowlist.
func relayableRuntimeCause(code string) string {
	if !api.IsBrokerRuntimeCause(code) || !brokerCodePattern.MatchString(code) {
		return ""
	}
	return code
}

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
//
// It relays a broker's broker_draining 503 the same way, under code
// broker_draining with the hub's own message (isBrokerDraining): the broker
// is shutting down and did not act, so the request can be retried
// (ptone/scion#4669, design wave 3 Optional-2). The broker sends it only
// for creates, starts and restarts, so the other relay sites never see it.
//
// A runtime_unavailable answer whose details carry
// api.BrokerErrorReasonProfileUnresolved (a stored-profile miss on a
// co-located broker, ptone/scion#4955) keeps the code and the 503, but gets
// the hub's own text naming the profile, without the retry-later wording
// and without Retry-After: it lasts until the profiles or runtimes are
// changed. The broker's message is not relayed.
func writeBrokerRuntimeUnavailable(w http.ResponseWriter, err error, runtime string) bool {
	if isBrokerDraining(err) {
		w.Header().Set("Retry-After", brokerRetryAfter(err, defaultBrokerDrainingRetryAfter))
		writeError(w, http.StatusServiceUnavailable, api.BrokerErrCodeBrokerDraining, brokerDrainingMessage, nil)
		return true
	}
	if !isBrokerRuntimeUnavailable(err) {
		return false
	}
	if profile, ok := brokerProfileUnresolved(err); ok {
		writeError(w, http.StatusServiceUnavailable, brokerCodeRuntimeUnavailable, brokerProfileUnresolvedMessage(profile), nil)
		return true
	}
	w.Header().Set("Retry-After", brokerRuntimeRetryAfter(err))
	writeError(w, http.StatusServiceUnavailable, brokerCodeRuntimeUnavailable, brokerRuntimeUnavailableMessage(runtime), nil)
	return true
}

// brokerRuntimeRetryAfter is the Retry-After to send for a broker's
// runtime_unavailable answer: the broker's value if it is a positive number
// of seconds, otherwise defaultBrokerRuntimeRetryAfter.
func brokerRuntimeRetryAfter(err error) string {
	return brokerRetryAfter(err, defaultBrokerRuntimeRetryAfter)
}

// brokerRetryAfter is the broker's Retry-After on err if it is a positive
// number of seconds, otherwise def.
func brokerRetryAfter(err error, def string) string {
	var se *brokerStatusError
	if errors.As(err, &se) {
		if n, convErr := strconv.Atoi(strings.TrimSpace(se.RetryAfter)); convErr == nil && n > 0 {
			return strconv.Itoa(n)
		}
	}
	return def
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

// maxRelayedProfileNameLen bounds the broker-supplied profile name the hub
// puts in its own text.
const maxRelayedProfileNameLen = 128

// brokerProfileUnresolved reports whether err is a broker answer whose
// details carry api.BrokerErrorReasonProfileUnresolved, and returns the
// profile it names ("" for an unset active_profile). A profile detail that
// is not a string, or is longer than maxRelayedProfileNameLen, is dropped
// to "".
func brokerProfileUnresolved(err error) (string, bool) {
	var se *brokerStatusError
	if !errors.As(err, &se) {
		return "", false
	}
	details := se.brokerErrorDetails()
	if reason, _ := details[api.BrokerErrorDetailReason].(string); reason != api.BrokerErrorReasonProfileUnresolved {
		return "", false
	}
	profile, _ := details[api.BrokerErrorDetailProfile].(string)
	if len(profile) > maxRelayedProfileNameLen {
		profile = ""
	}
	return profile, true
}

// brokerProfileUnresolvedMessage is the hub's client-facing text for a
// broker's profile_unresolved refusal; profile is "" when the dispatch
// needed the default profile and active_profile is unset.
func brokerProfileUnresolvedMessage(profile string) string {
	if profile == "" {
		return "The agent's runtime profile cannot be resolved on its runtime broker: active_profile is unset or not one of the hub's stored profiles; set active_profile to a stored profile, or give the agent an explicit profile"
	}
	return fmt.Sprintf("Runtime profile %q cannot be resolved on the agent's runtime broker: it is not one of the hub's stored profiles, or its runtime is not one of the hub's stored runtimes; choose a stored profile, or fix the server's profiles and runtimes settings", profile)
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
