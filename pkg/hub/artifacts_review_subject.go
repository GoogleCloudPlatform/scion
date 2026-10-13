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

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The subject read check of a review grant (ptone/scion#4538): the hub
// side of artifacts.ReviewSubjectAuthority.
//
// ReviewSubjectAuthorize answers for a NAMED user, the subject of a review
// grant, not for the caller of ctx. It is CHECK-ONLY: the identity it
// builds (reviewSubjectIdentity) is passed only as the subject of one
// policy read, AuthzService.CheckAccess, the same decision Authorize asks
// for a caller. It never enters a context, so it can never be taken for
// the caller: the artifact service learns its caller only from ctx
// (artifactHost.Principal), which refuses every in-process identity.
// TestReviewSubjectIdentityNeverEntersAContext pins that.
//
// The shape matches the send-time recipient check of ptone/scion#4575
// (RecipientAuthorize), so the two can later share one implementation.

var _ artifacts.ReviewSubjectAuthority = (*artifactHost)(nil)

// ReviewSubjectAuthorize implements artifacts.ReviewSubjectAuthority:
// whether policy lets the user ref exercise permission on artifacts in the
// project scopeRef, decided by AuthzService.CheckAccess with that user as
// the subject. A session user has no credential limits, so there is no
// credential step. It answers false for a kind other than user, a missing,
// inactive or no longer usable user, an empty scope or a permission that
// is not an artifact registry row, and an error only for a failed lookup.
//
// The decision runs under maskAllAuthzMemo, so no memoized input of the
// caller's request is visible to it.
func (h *artifactHost) ReviewSubjectAuthorize(ctx context.Context, kind, ref, scopeRef, permission string) (bool, error) {
	perm, ok := artifactPermission(permission)
	if !ok || scopeRef == "" || kind != artifacts.PrincipalKindUser || h.server == nil || h.server.authzService == nil {
		return false, nil
	}
	identity, err := h.reviewSubjectIdentity(ctx, ref)
	if err != nil || identity == nil {
		return false, err
	}
	decision := h.server.authzService.CheckAccess(maskAllAuthzMemo(ctx), identity, Resource{
		Type:       permissions.ResourceArtifact,
		ParentType: permissions.ResourceProject,
		ParentID:   scopeRef,
	}, Action(perm.Action))
	return decision.Allowed, nil
}

// reviewSubjectIdentity builds the subject identity of a review grant from
// the user's stored record, as the provenance and standing checks do. It
// returns nil and no error, which the caller treats as "cannot read", for
// a missing or inactive user or a test identity that can no longer sign
// in, and an error only for a failed lookup.
//
// The result must only ever be passed as the subject of a policy read and
// never be stored in a context (TestReviewSubjectIdentityNeverEntersAContext).
func (h *artifactHost) reviewSubjectIdentity(ctx context.Context, userID string) (Identity, error) {
	if h.server == nil || h.server.store == nil || userID == "" {
		return nil, nil
	}
	user, err := h.server.store.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if user == nil || user.Status != store.UserStatusActive {
		return nil, nil
	}
	if testFixtureRejection(user, h.server.testIdentities.enabled, h.server.testIdentities.clock()) != "" {
		return nil, nil
	}
	return NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, ""), nil
}
