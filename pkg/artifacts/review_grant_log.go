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
	"log/slog"
	"net/http"
)

// reviewGrantRefusal is why putReviewGrant refused a review grant with a
// 403 (ptone/scion#5119). It is logged, never sent to the caller.
type reviewGrantRefusal string

const (
	// reviewRefusalCaller: the caller does not hold review-grant authority
	// on the artifact (reviewGrantCaller or reviewGrantAuthorized).
	reviewRefusalCaller reviewGrantRefusal = "caller"
	// reviewRefusalBody: the body is not exactly a principal review grant
	// to a user.
	reviewRefusalBody reviewGrantRefusal = "body"
	// reviewRefusalSubjectUnreadable: the user cannot read the artifact, or
	// the host cannot check whether they can (no ReviewSubjectAuthority:
	// the check fails closed, and the caller gets the uniform 403).
	reviewRefusalSubjectUnreadable reviewGrantRefusal = "subject_unreadable"
	// reviewRefusalExistingAccess: the user holds a principal read, write
	// or admin grant, which a review grant would replace.
	reviewRefusalExistingAccess reviewGrantRefusal = "existing_access"
	// reviewRefusalStoreRefused: the store refused the grant under the
	// artifact's lock (the home or owner changed meanwhile).
	reviewRefusalStoreRefused reviewGrantRefusal = "store_refused"
)

// Decision log reasons of review-grant refusals: the five refusal values with
// a review_ prefix, so they stay distinct in the log's shared reason field.
const (
	logReasonReviewCaller            = "review_caller"
	logReasonReviewBody              = "review_body"
	logReasonReviewSubjectUnreadable = "review_subject_unreadable"
	logReasonReviewExistingAccess    = "review_existing_access"
	logReasonReviewStoreRefused      = "review_store_refused"
	// logReasonReviewRefused is the reason of a refusal value not listed
	// above (there is none today), so no other text can reach the log.
	logReasonReviewRefused = "review_refused"
)

// logReason is the decision log reason of the refusal reason.
func (reason reviewGrantRefusal) logReason() string {
	switch reason {
	case reviewRefusalCaller:
		return logReasonReviewCaller
	case reviewRefusalBody:
		return logReasonReviewBody
	case reviewRefusalSubjectUnreadable:
		return logReasonReviewSubjectUnreadable
	case reviewRefusalExistingAccess:
		return logReasonReviewExistingAccess
	case reviewRefusalStoreRefused:
		return logReasonReviewStoreRefused
	}
	return logReasonReviewRefused
}

// logReviewGrantRefusal writes one refusal of a review grant on artifact a
// by the caller of r to the artifact decision log (ptone/scion#5120), at
// WARN, as action share with outcome refused and the mapped reason. When
// the body named a well-formed user (validated by reviewGrantSubject), the
// entry also carries that grantee as its principal subject with permission
// review. It is the one place these refusals are logged. Like every
// decision log entry it never carries content, titles, notes or tokens.
func (s *Service) logReviewGrantRefusal(r *http.Request, a *Artifact, reason reviewGrantRefusal, grantee string) {
	e := logEntry{action: logActionShare, outcome: logOutcomeRefused, reason: reason.logReason(), artifact: a}
	if grantee != "" {
		e.subjectKind, e.subjectRef, e.permission = SubjectPrincipal, grantee, GrantReview
	}
	s.logDecision(r, slog.LevelWarn, e)
}
