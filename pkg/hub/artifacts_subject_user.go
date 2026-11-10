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
	"context"
	"errors"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The named-user subject (ptone/scion#5145): the one builder of a
// check-only identity for a user other than the caller of a request. The
// review-grant subject check (ReviewSubjectAuthorize), the Ask-for-review
// send check (MayNotifyGrant) and the notice label's email rule
// (grantNoticeLabel) ask policy a question ABOUT a named user; none of them
// acts AS that user.
//
// The identity built here is passed only as the subject of one
// AuthzService.CheckAccess under maskAllAuthzMemo. It never enters a
// context, so it can never be taken for the caller: every handler learns
// its caller from ctx. TestNamedUserSubjectNeverEntersAContext pins the
// allowlist of callers and that, in each, the identity reaches at most one
// CheckAccess and nothing else.

// namedUserSubject loads user userID, a NAMED user and never the caller of
// ctx, and builds a CHECK-ONLY identity for it: refused (nil, nil, nil) for a
// missing user, a status other than active (O4: invited, pending and
// suspended), or a test identity that can no longer sign in; an error only for
// a failed lookup. The identity may only be passed as the subject of
// AuthzService.CheckAccess under maskAllAuthzMemo, and never enters a context.
func (s *Server) namedUserSubject(ctx context.Context, userID string) (*store.User, Identity, error) {
	if s == nil || s.store == nil || userID == "" {
		return nil, nil, nil
	}
	user, err := s.store.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if user == nil || user.Status != store.UserStatusActive {
		return nil, nil, nil
	}
	if testFixtureRejection(user, s.testIdentities.enabled, s.testIdentities.clock()) != "" {
		return nil, nil, nil
	}
	return user, NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, ""), nil
}
