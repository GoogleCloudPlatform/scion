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

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
)

// ConduitRenewalOutcome is how the holder of a stream's leaf acts on a
// failed renewal re-check (design v2.21.36 §3.5, Refusal meaning).
type ConduitRenewalOutcome int

const (
	// ConduitRenewalUnavailable: a non-authorization refusal. Nothing is
	// minted or forwarded, the stream is not closed, and it ends at its
	// current deadline. Every error not classified otherwise is this.
	ConduitRenewalUnavailable ConduitRenewalOutcome = iota
	// ConduitRenewalDeny: an authorization refusal. The leaf holder closes
	// the stream with 4401 authz_expired.
	ConduitRenewalDeny
	// ConduitRenewalTargetGone: the agent no longer exists. The leaf
	// holder closes the stream with 4404 target_not_found.
	ConduitRenewalTargetGone
)

// String names the outcome for logs and metrics.
func (o ConduitRenewalOutcome) String() string {
	switch o {
	case ConduitRenewalDeny:
		return "deny"
	case ConduitRenewalTargetGone:
		return "target_gone"
	default:
		return "unavailable"
	}
}

// conduitRenewalDenyGrantErrors are the verification results of a
// presented grant that are a deny (design v2.21.36 §3.5): a bad signature,
// the wrong typ or algorithm, issuer or audience, a malformed token, a
// binding mismatch (target session, epoch or incarnation; open_jti or
// stream_id), a grant past its acceptance window (now after authz_until
// plus 30s) or not yet valid (nbf more than 30s ahead), and a kid unknown
// after one ring reload (renewConduitStream reloads once on an unknown kid;
// a failed reload is unavailable). A kid revoked for compromise leaves the
// re-check key set, so it is unknown here too: compromise and explicit
// revocation go through the deny paths, not through via or route removal.
var conduitRenewalDenyGrantErrors = []error{
	grant.ErrSignature,
	grant.ErrAlgorithm,
	grant.ErrAudience,
	grant.ErrIssuer,
	grant.ErrMalformed,
	grant.ErrTarget,
	grant.ErrStreamBinding,
	grant.ErrRecheckWindow,
	grant.ErrNotYetValid,
	grant.ErrUnknownKey,
}

// ClassifyConduitRenewal classifies an error from a renewal re-check
// (renewConduitStream) as deny, target gone or unavailable, per design
// v2.21.36 §3.5. Hub-held streams (the renewal driver) and relay-held
// streams (6.6b-3) use it alike.
//
//   - target gone: the agent no longer exists;
//   - unavailable, before any deny: authorization could not be evaluated
//     (a store or key-set read failed), even when the error also reads as a
//     refusal;
//   - deny: authorization denied (including an inactive principal and a
//     project mismatch), or a verification result in
//     conduitRenewalDenyGrantErrors;
//   - unavailable: everything else, including an inconsistent placement, a
//     validly signed grant whose via or stream is missing or outside the
//     origin table, a signing key past its not_after (reported only for an
//     otherwise valid grant), and any unclassified error.
func ClassifyConduitRenewal(err error) ConduitRenewalOutcome {
	switch {
	case err == nil:
		return ConduitRenewalUnavailable
	case errors.Is(err, errConduitAgentGone):
		return ConduitRenewalTargetGone
	case errors.Is(err, errConduitAuthzUnavailable):
		return ConduitRenewalUnavailable
	case errors.Is(err, errConduitForbidden), errors.Is(err, errConduitPrincipalInactive):
		return ConduitRenewalDeny
	}
	for _, deny := range conduitRenewalDenyGrantErrors {
		if errors.Is(err, deny) {
			return ConduitRenewalDeny
		}
	}
	return ConduitRenewalUnavailable
}
