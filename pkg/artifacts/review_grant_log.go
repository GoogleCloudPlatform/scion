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
	"context"
	"log/slog"
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
	// reviewRefusalSubjectUnreadable: the user cannot read the artifact.
	reviewRefusalSubjectUnreadable reviewGrantRefusal = "subject_unreadable"
	// reviewRefusalExistingAccess: the user holds a principal read, write
	// or admin grant, which a review grant would replace.
	reviewRefusalExistingAccess reviewGrantRefusal = "existing_access"
	// reviewRefusalStoreRefused: the store refused the grant under the
	// artifact's lock (the home or owner changed meanwhile).
	reviewRefusalStoreRefused reviewGrantRefusal = "store_refused"
)

// logReviewGrantRefusal records one refusal of a review grant on artifact
// a: the reason, the artifact id and, when the body named a well-formed
// user (validated by reviewGrantSubject), the grantee's principal ref. It
// is the one place these refusals are logged, so where they go can change
// here alone. It never logs content, titles, notes or tokens.
func (s *Service) logReviewGrantRefusal(ctx context.Context, a *Artifact, reason reviewGrantRefusal, grantee string) {
	attrs := []any{"reason", string(reason), "artifact_id", a.ID}
	if grantee != "" {
		attrs = append(attrs, "grantee", grantee)
	}
	slog.WarnContext(ctx, "artifacts: review grant refused", attrs...)
}
