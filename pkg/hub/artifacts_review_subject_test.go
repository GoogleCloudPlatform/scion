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

//go:build !no_sqlite

package hub

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The subject read check of a review grant (ptone/scion#4538):
// artifactHost.ReviewSubjectAuthorize. Its identity builder's C1 pin is
// TestNamedUserSubjectNeverEntersAContext (artifacts_grant_notify_test.go).

// TestArtifactHostReviewSubjectAuthorize: the subject check answers for
// the named user, from that user's own bindings, never the caller's, and
// fails closed for everything it does not serve.
func TestArtifactHostReviewSubjectAuthorize(t *testing.T) {
	f := newReviewGrantFixture(t)
	host := newArtifactHost(f.srv)
	inactive := tid("rs-inactive")
	createDCUser(t, f.s, inactive, "rs-inactive@test.com", f.home, store.ProjectRoleMember)
	u, err := f.s.GetUser(context.Background(), inactive)
	require.NoError(t, err)
	u.Status = store.UserStatusSuspended
	require.NoError(t, f.s.UpdateUser(context.Background(), u))

	// The caller is a home-project admin: its own rights must never carry
	// over to the subject.
	ctx := contextWithIdentity(context.Background(), f.user(f.admin))
	cases := []struct {
		name             string
		kind, ref, scope string
		perm             string
		want             bool
	}{
		{"home member", artifacts.PrincipalKindUser, f.member, f.home, artifacts.PermissionRead, true},
		{"home owner", artifacts.PrincipalKindUser, f.owner, f.home, artifacts.PermissionRead, true},
		{"member of another project, home scope", artifacts.PrincipalKindUser, f.outside, f.home, artifacts.PermissionRead, false},
		{"member of another project, its own scope", artifacts.PrincipalKindUser, f.outside, f.other, artifacts.PermissionRead, true},
		{"inactive member", artifacts.PrincipalKindUser, inactive, f.home, artifacts.PermissionRead, false},
		{"unknown user", artifacts.PrincipalKindUser, tid("rs-missing"), f.home, artifacts.PermissionRead, false},
		{"empty user", artifacts.PrincipalKindUser, "", f.home, artifacts.PermissionRead, false},
		{"agent subject", artifacts.PrincipalKindAgent, f.agent.ID, f.home, artifacts.PermissionRead, false},
		{"empty scope", artifacts.PrincipalKindUser, f.member, "", artifacts.PermissionRead, false},
		{"not an artifact permission", artifacts.PrincipalKindUser, f.member, f.home, "project.read", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := host.ReviewSubjectAuthorize(ctx, tc.kind, tc.ref, tc.scope, tc.perm)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestArtifactHostReviewSubjectParity: for an active user, the subject
// check gives exactly the answer Authorize gives when that user is the
// caller (a session), allow and deny alike, so the grant-time check and a
// real read decide the same way in both directions.
func TestArtifactHostReviewSubjectParity(t *testing.T) {
	f := newReviewGrantFixture(t)
	host := newArtifactHost(f.srv)
	granter := contextWithIdentity(context.Background(), f.user(f.admin))
	for _, userID := range []string{f.member, f.owner, f.admin, f.outside, f.otherAdmin, f.delegator} {
		for _, scope := range []string{f.home, f.other} {
			for _, perm := range []string{artifacts.PermissionRead, artifacts.PermissionCreate, artifacts.PermissionManage} {
				u, err := f.s.GetUser(context.Background(), userID)
				require.NoError(t, err)
				asCaller := host.Authorize(contextWithIdentity(context.Background(),
					NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "web")), scope, perm)
				asSubject, err := host.ReviewSubjectAuthorize(granter, artifacts.PrincipalKindUser, userID, scope, perm)
				require.NoError(t, err)
				assert.Equal(t, asCaller, asSubject, "user %s scope %s %s", userID, scope, perm)
			}
		}
	}
}
