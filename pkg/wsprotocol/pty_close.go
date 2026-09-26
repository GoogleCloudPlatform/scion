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

package wsprotocol

import "strings"

// PTY WebSocket close-code contract.
//
// These codes are sent in the WebSocket close frame that ends a PTY attach
// (Hub -> web/CLI client), and in StreamCloseMessage.Code on the Hub <-> broker
// control channel. The rule is that the hop that knows the cause chooses the
// code, and later hops pass it through unchanged; a specific code is never
// downgraded to 1000.
//
// Application codes use the RFC 6455 private range 4000-4999 as 4000 + the
// nearest HTTP status. Reasons are machine-readable snake_case strings of at
// most MaxCloseReasonBytes bytes.
//
// The client-side mirror of ClassifyPTYClose lives in
// web/src/client/terminal-close-codes.ts. Both are checked against
// testdata/pty_close_codes.json; change all three together.
const (
	// ClosePTYNormal (1000): clean detach. The tmux client detached, or the
	// client closed deliberately. The tmux session still exists. Do not retry.
	ClosePTYNormal = 1000
	// ClosePTYGoingAway (1001): server process shutting down. Not emitted
	// today: neither the Hub nor the broker has a base context that is
	// cancelled on shutdown, so a restart is observed by clients as 1006.
	// Retry.
	ClosePTYGoingAway = 1001
	// ClosePTYAbnormal (1006): TCP dropped without a close frame. Never sent;
	// synthesized by the client library. Retry.
	ClosePTYAbnormal = 1006
	// ClosePTYInternalError (1011): unexpected server error, or a broker close
	// code the Hub does not recognise. Retry.
	ClosePTYInternalError = 1011
	// ClosePTYServiceRestart (1012): reserved for a future graceful drain.
	// Retry.
	ClosePTYServiceRestart = 1012
	// ClosePTYTryAgainLater (1013): overload. Not emitted today. Retry.
	ClosePTYTryAgainLater = 1013

	// ClosePTYAuthRequired (4401): credentials no longer valid. Reserved;
	// auth failures surface as HTTP 401 on the handshake or preflight today.
	// Terminal.
	ClosePTYAuthRequired = 4401
	// ClosePTYForbidden (4403): attach permission revoked. Reserved; surfaces
	// as HTTP 403 today. Terminal.
	ClosePTYForbidden = 4403
	// ClosePTYAgentNotFound (4404): the broker cannot find the agent or its
	// container. The Hub maps the legacy broker code 404 to this. Terminal.
	ClosePTYAgentNotFound = 4404
	// ClosePTYSessionGone (4410): the tmux session no longer exists (agent
	// exited, container stopped or removed). Terminal.
	ClosePTYSessionGone = 4410
	// ClosePTYUpstreamUnavailable (4503): the hop behind this one is
	// temporarily gone (Hub <-> broker control channel dropped, stream open
	// failed, tmux session not ready yet). Retry.
	ClosePTYUpstreamUnavailable = 4503
	// ClosePTYUpstreamTimeout (4504): the broker did not produce first output
	// within the open deadline. Reserved. Retry.
	ClosePTYUpstreamTimeout = 4504
)

// Close reasons emitted by the Hub.
const (
	CloseReasonBrokerDisconnected = "broker_disconnected"
	CloseReasonStreamOpenFailed   = "stream_open_failed"
	CloseReasonBrokerWriteFailed  = "broker_write_failed"
	CloseReasonClientReadFailed   = "client_read_failed"
	CloseReasonClientWriteFailed  = "client_write_failed"
	CloseReasonInternalError      = "internal_error"
)

// Close reasons emitted by the broker's attach-end classifier
// (pkg/runtimebroker classifyAttachEnd).
const (
	// CloseReasonRuntimeStreamDropped (4503): the tmux session is still
	// alive, but the exec transport between the broker and the container
	// ended abnormally (killed process, docker/podman exec transport drop,
	// k8s apiserver stream error).
	CloseReasonRuntimeStreamDropped = "runtime_stream_dropped"
	// CloseReasonSessionEnded (4410): the tmux session is gone but the
	// container/pod that hosted it still resolves.
	CloseReasonSessionEnded = "session_ended"
	// CloseReasonContainerRemoved (4410): the tmux session is gone and the
	// agent's container/pod no longer resolves either.
	CloseReasonContainerRemoved = "container_removed"
	// CloseReasonSessionNotReady (4503): the attach exec never started
	// because the tmux session was not ready yet (e.g. the agent is still
	// starting up), and the container itself still resolves. Retry.
	CloseReasonSessionNotReady = "session_not_ready"
	// CloseReasonLookupUnavailable (4503): the broker could not determine
	// whether the agent's container still exists because the runtime's list
	// call itself failed. This must never be reported as 4404 or 4410.
	CloseReasonLookupUnavailable = "lookup_unavailable"
	// CloseReasonProbeFailed (1011): the post-exit tmux has-session probe
	// itself failed or timed out (runtime unreachable), so the broker cannot
	// tell whether the session survived. Retry.
	CloseReasonProbeFailed = "probe_failed"
	// CloseReasonAgentNotFound (4404): the broker could not resolve the
	// agent to a container at stream-open time.
	CloseReasonAgentNotFound = "agent_not_found"
	// CloseReasonRuntimeUnavailable (4503): the broker's agent lookup itself
	// failed (the container runtime could not be listed) at stream-open
	// time. Retry.
	CloseReasonRuntimeUnavailable = "runtime_unavailable"
)

// MaxCloseReasonBytes is the largest reason RFC 6455 allows in a close frame
// (125-byte control payload minus the 2-byte code).
const MaxCloseReasonBytes = 123

// CloseDisposition is what a PTY client should do after a close code.
type CloseDisposition int

const (
	// DispositionDetached means stop; the session ended cleanly (1000).
	DispositionDetached CloseDisposition = iota
	// DispositionRetry means the cause is transient; a reconnect may succeed.
	DispositionRetry
	// DispositionTerminal means stop and report the reason; retrying will
	// not help.
	DispositionTerminal
)

// String returns the lower-case name used in testdata/pty_close_codes.json.
func (d CloseDisposition) String() string {
	switch d {
	case DispositionDetached:
		return "detached"
	case DispositionRetry:
		return "retry"
	case DispositionTerminal:
		return "terminal"
	default:
		return "unknown"
	}
}

// ClassifyPTYClose maps a PTY WebSocket close code to a client disposition.
// It is the single source of truth for clients; the TypeScript mirror must
// agree with it on every row of testdata/pty_close_codes.json.
func ClassifyPTYClose(code int) CloseDisposition {
	switch {
	case code == ClosePTYNormal:
		return DispositionDetached
	case code == ClosePTYAuthRequired, code == ClosePTYForbidden,
		code == ClosePTYAgentNotFound, code == ClosePTYSessionGone:
		return DispositionTerminal
	case code == ClosePTYUpstreamUnavailable, code == ClosePTYUpstreamTimeout:
		return DispositionRetry
	case code >= 4000 && code <= 4999:
		// Unknown application code: fail safe and do not hammer the server.
		return DispositionTerminal
	case code == 1002, code == 1003, code == 1007, code == 1008, code == 1009, code == 1010:
		// Protocol and policy errors will not fix themselves.
		return DispositionTerminal
	default:
		// 1001, 1005, 1006, 1011-1015, and anything else.
		return DispositionRetry
	}
}

// MapBrokerStreamCloseCode maps a broker StreamCloseMessage.Code to the
// WebSocket close code the Hub sends to the PTY client. Codes in 1000-1015
// and 4000-4999 pass through. Brokers that predate the close-code contract
// send 0 when the tmux client ended (mapped to 1000, as before), 404 when the
// agent lookup failed (mapped to 4404), and 500 on other errors (mapped to
// 1011); any other value is also mapped to 1011.
func MapBrokerStreamCloseCode(code int) int {
	switch {
	case code == 0:
		return ClosePTYNormal
	case code == 404:
		return ClosePTYAgentNotFound
	case code >= 1000 && code <= 1015, code >= 4000 && code <= 4999:
		return code
	default:
		return ClosePTYInternalError
	}
}

// IsSendableCloseCode reports whether code may appear in a close frame on the
// wire. RFC 6455 reserves 1004, 1005, 1006 and 1015 for local use, and codes
// below 1000 or in 1016-2999 are not assigned.
func IsSendableCloseCode(code int) bool {
	switch {
	case code == 1004, code == 1005, code == 1006, code == 1015:
		return false
	case code >= 1000 && code <= 1015:
		return true
	case code >= 3000 && code <= 4999:
		return true
	default:
		return false
	}
}

// TruncateCloseReason makes reason safe for a close frame: it drops invalid
// UTF-8 (RFC 6455 section 8.1 requires clients to fail the connection on an
// invalid reason, which would turn the real code into 1006), then shortens
// the result to at most MaxCloseReasonBytes bytes without splitting a UTF-8
// sequence.
func TruncateCloseReason(reason string) string {
	reason = strings.ToValidUTF8(reason, "")
	if len(reason) <= MaxCloseReasonBytes {
		return reason
	}
	cut := MaxCloseReasonBytes
	// Back up to the start of a UTF-8 sequence (continuation bytes are 10xxxxxx).
	for cut > 0 && reason[cut]&0xC0 == 0x80 {
		cut--
	}
	return reason[:cut]
}
