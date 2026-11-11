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
	"log/slog"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// relayableStreamCloseReasons are the close reasons the hub passes from a
// Runtime Broker or a conduit relay or target on to a client close frame:
// the fixed protocol tokens only. A reason of the form "<token>: <detail>"
// whose token is one of these is passed on as the token alone. Any other
// reason is free text from the far end and is replaced by "".
var relayableStreamCloseReasons = map[string]bool{
	// Hub reasons (wsprotocol and pkg/hub).
	wsprotocol.CloseReasonBrokerDisconnected: true,
	wsprotocol.CloseReasonStreamOpenFailed:   true,
	wsprotocol.CloseReasonBrokerWriteFailed:  true,
	wsprotocol.CloseReasonClientReadFailed:   true,
	wsprotocol.CloseReasonClientWriteFailed:  true,
	wsprotocol.CloseReasonInternalError:      true,
	ptyCloseReasonUpstreamUnreachable:        true,
	ptyCloseReasonForbidden:                  true,
	closeReasonSlowConsumer:                  true,
	conduitReasonStreamRenewalUnavailable:    true,
	conduitReasonStreamRenewalUnsupported:    true,
	conduitReasonAuthzExpired:                true,
	// Runtime Broker attach reasons (wsprotocol).
	wsprotocol.CloseReasonRuntimeStreamDropped: true,
	wsprotocol.CloseReasonSessionEnded:         true,
	wsprotocol.CloseReasonContainerRemoved:     true,
	wsprotocol.CloseReasonAgentStopped:         true,
	wsprotocol.CloseReasonSessionNotReady:      true,
	wsprotocol.CloseReasonLookupUnavailable:    true,
	wsprotocol.CloseReasonProbeFailed:          true,
	wsprotocol.CloseReasonAgentNotFound:        true,
	wsprotocol.CloseReasonRuntimeUnavailable:   true,
	wsprotocol.CloseReasonAttachUnsupported:    true,
	wsprotocol.CloseReasonInputOverflow:        true,
	// Conduit relay reasons.
	relay.ReasonBadHello:              true,
	relay.ReasonBadFrame:              true,
	relay.ReasonUnsupportedVersion:    true,
	relay.ReasonFrameTooLarge:         true,
	relay.ReasonUnauthenticated:       true,
	relay.ReasonGrantInvalid:          true,
	relay.ReasonTargetNotFound:        true,
	relay.ReasonCancelled:             true,
	relay.ReasonRelayRestart:          true,
	relay.ReasonDraining:              true,
	relay.ReasonSuperseded:            true,
	relay.ReasonNotServing:            true,
	relay.ReasonRegistryUnavailable:   true,
	relay.ReasonGrantKeysUnavailable:  true,
	relay.ReasonOpenTimeout:           true,
	relay.ReasonInternal:              true,
	relay.ReasonSupersededIncarnation: true,
	relay.ReasonLegacyHelloSuperseded: true,
	relay.ReasonRelayNotAdmitting:     true,
	// Conduit session reasons.
	conduit.CloseReasonDrainDeadline: true,
}

// relayableStreamCloseReason returns reason when it is one of the fixed
// close reasons (relayableStreamCloseReasons); for a reason of the form
// "<token>: <detail>" whose token is one of them, it returns the token;
// else "". A dropped detail or a replaced reason is logged at warn level,
// capped and rendered by brokerBodyForLog, with the close code and source,
// so the far end's text stays in the hub log only.
func relayableStreamCloseReason(source string, code int, reason string) string {
	if reason == "" || relayableStreamCloseReasons[reason] {
		return reason
	}
	// A reason of the form "<token>: <detail>", from a broker or a relay:
	// split once on the first ": "; when the token is exactly an allowlisted
	// token, pass on the token only and log the detail.
	if token, detail, found := strings.Cut(reason, ": "); found && detail != "" && relayableStreamCloseReasons[token] {
		slog.Warn("stream close reason detail dropped",
			"source", source,
			"close_code", code,
			"token", token,
			"detail_len", len(detail),
			"detail", brokerBodyForLog(detail))
		return token
	}
	slog.Warn("stream close reason replaced",
		"source", source,
		"close_code", code,
		"reason_len", len(reason),
		"reason", brokerBodyForLog(reason))
	return ""
}
