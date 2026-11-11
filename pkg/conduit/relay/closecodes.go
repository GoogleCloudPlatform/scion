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

package relay

import (
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
)

// Relay-side close codes and reason tokens (design v2.5 §3.3.1). The codes
// pkg/conduit does not define yet (it is in upstream review) live here.
// A reason is a snake_case token, optionally followed by ": <detail>", and
// at most MaxReasonBytes long (it must fit a WebSocket close frame).
//
// Classification rule: an admission infrastructure fault (registry read or
// insert error, grant keys unavailable) is 4504, never 4403/4409, so a
// database blip cannot become a terminal client error. 4503 is reserved
// for deliberate, planned closes and is sent with the reconnect window
// (Config.ReconnectWindow; the dialer draws its delay from it).
const (
	// CloseTargetNotFound (4404, stream scope): the target is unknown or
	// deleted.
	CloseTargetNotFound uint32 = 4404
	// CloseInternalError (1011): an unexpected internal error.
	CloseInternalError uint32 = 1011

	// MaxReasonBytes bounds a close reason.
	MaxReasonBytes = 100
)

// Reason tokens.
const (
	// 4400 protocol errors.
	ReasonBadHello           = "bad_hello"
	ReasonBadFrame           = "bad_frame"
	ReasonUnsupportedVersion = "unsupported_version"
	ReasonFrameTooLarge      = "frame_too_large"
	// 4401 credential.
	ReasonUnauthenticated = "unauthenticated"
	// 4403 refusals.
	ReasonForbidden    = "forbidden"
	ReasonGrantInvalid = "grant_invalid"
	// 4404 (stream).
	ReasonTargetNotFound = "target_not_found"
	// 4409: ReasonSupersededIncarnation and ReasonLegacyHelloSuperseded
	// (incarnation.go).
	// 4499.
	ReasonCancelled = "cancelled"
	// 4503 planned or deliberate.
	ReasonRelayRestart = "relay_restart"
	ReasonDraining     = "draining"
	ReasonSuperseded   = "superseded"
	ReasonNotServing   = "not_serving"
	// 4504 transient failures.
	ReasonRegistryUnavailable  = "registry_unavailable"
	ReasonGrantKeysUnavailable = "grant_keys_unavailable"
	ReasonOpenTimeout          = "open_timeout"
	ReasonUpstreamUnreachable  = "upstream_unreachable"
	// 1011.
	ReasonInternal = "internal"
)

// reason builds "<token>: <detail>" (just token when detail is empty),
// truncated to MaxReasonBytes.
func reason(token, detail string) string {
	s := token
	if detail != "" {
		s += ": " + detail
	}
	if len(s) > MaxReasonBytes {
		s = strings.ToValidUTF8(s[:MaxReasonBytes], "")
	}
	return s
}

// reject is conduit.Reject with a §3.3.1 reason.
func reject(code uint32, token, detail string) error {
	return conduit.Reject(code, reason(token, detail))
}

// closeErr is a *conduit.CloseError with a §3.3.1 reason.
func closeErr(code uint32, token, detail string) *conduit.CloseError {
	return &conduit.CloseError{Code: code, Reason: reason(token, detail)}
}
