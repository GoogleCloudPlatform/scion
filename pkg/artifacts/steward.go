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

package artifacts

import (
	"errors"
	"net/http"
)

// Stewards (design D25, ptone/scion#5144). On an artifact owned by an
// agent, the owners of its home project and the user who delegated to the
// agent are its stewards (Stewardship): adminAuthorityWith admits them
// after the owner and admin grantees, so every management route serves
// them. Their authority is bounded:
//   - they may not give admin (a principal or scope admin grant) or move
//     the artifact: handlePutGrant and handlePatchArtifact answer 403
//     CodeStewardActionNotAllowed, and the store refuses both again for a
//     steward actor (ErrStewardActionNotAllowed);
//   - every store write they make carries the artifact's home and owner as
//     they were when the authority was decided (Actor.Expect, set by
//     callerActor), checked under the artifact's lock, so a write racing
//     a move or an ownership transfer answers 404 rather than commit
//     against the new state.

// CodeStewardActionNotAllowed is the error code of a steward's request for
// an admin grant or a move.
const CodeStewardActionNotAllowed = "steward_action_not_allowed"

// stewardActionNotAllowedMessage is the text of a
// CodeStewardActionNotAllowed refusal. The caller has passed the read and
// authority checks, so naming the rule reveals nothing.
const stewardActionNotAllowedMessage = "as the agent's delegating user or an owner of its home project you can share this artifact " +
	"but not give admin access or move it; a user with an admin grant on it can, or you can publish your own copy"

// refuseStewardAction logs and answers a steward's request for an admin
// grant or a move (action logActionShare or logActionMove).
func (s *Service) refuseStewardAction(w http.ResponseWriter, r *http.Request, action string, a *Artifact) {
	s.logRefusal(r, action, logReasonStewardActionNotAllowed, a)
	writeError(w, http.StatusForbidden, CodeStewardActionNotAllowed, stewardActionNotAllowedMessage)
}

// logStewardPrecondition logs a store write refused with
// ErrStewardPrecondition (the artifact moved or changed owner since the
// caller's steward authority was decided), which the route answers 404 as
// for a concurrent delete. It logs nothing for any other error.
func (s *Service) logStewardPrecondition(r *http.Request, action string, a *Artifact, err error) {
	if errors.Is(err, ErrStewardPrecondition) {
		s.logRefusal(r, action, logReasonStewardPreconditionFailed, a)
	}
}
