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
	"go/ast"
	"go/token"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The subject read check of a review grant (ptone/scion#4538):
// artifactHost.ReviewSubjectAuthorize.

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

// TestReviewSubjectIdentityNeverEntersAContext pins that the identity
// built for the subject read check stays a policy subject: nothing in
// artifacts_review_subject.go puts any identity into a context, so the
// artifact service, which learns its caller only from ctx, can never take
// a review subject for the caller.
func TestReviewSubjectIdentityNeverEntersAContext(t *testing.T) {
	forbidden := map[string]bool{
		"WithValue":                    true,
		"contextWithIdentity":          true,
		"contextWithCredentialContext": true,
		"WithIdentity":                 true,
		"withIdentity":                 true,
		"ContextWithIdentity":          true,
	}
	calls := 0
	forEachHubFile(t, func(name string, _ *token.FileSet, f *ast.File) {
		if name != "artifacts_review_subject.go" {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			calls++
			if sym := extractCallSymbol(call); forbidden[sym] {
				t.Errorf("artifacts_review_subject.go calls %s: a review subject identity must never enter a context", sym)
			}
			return true
		})
	})
	if calls == 0 {
		t.Fatal("artifacts_review_subject.go not found or has no calls: scanner broken")
	}
}

// TestReviewSubjectIdentityOnlyASubject pins the one use of
// reviewSubjectIdentity's result: it is assigned in ReviewSubjectAuthorize
// and passed only as the identity argument of CheckAccess, and the helper
// is called nowhere else in pkg/hub.
func TestReviewSubjectIdentityOnlyASubject(t *testing.T) {
	callers := 0
	forEachHubFile(t, func(name string, _ *token.FileSet, f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				return true
			}
			ast.Inspect(fd.Body, func(m ast.Node) bool {
				call, ok := m.(*ast.CallExpr)
				if ok && extractCallSymbol(call) == "reviewSubjectIdentity" {
					callers++
					if name != "artifacts_review_subject.go" || fd.Name.Name != "ReviewSubjectAuthorize" {
						t.Errorf("%s:%s calls reviewSubjectIdentity; only ReviewSubjectAuthorize may", name, fd.Name.Name)
					}
				}
				return true
			})
			if fd.Name.Name != "ReviewSubjectAuthorize" || name != "artifacts_review_subject.go" {
				return false
			}
			// Every use of the identity variable is either its assignment,
			// the nil check, or the identity argument of CheckAccess.
			uses := 0
			ast.Inspect(fd.Body, func(m ast.Node) bool {
				call, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				for i, arg := range call.Args {
					if id, ok := arg.(*ast.Ident); ok && id.Name == "identity" {
						uses++
						if extractCallSymbol(call) != "CheckAccess" || i != 1 {
							t.Errorf("identity passed to %s (arg %d); only CheckAccess's subject argument is allowed", extractCallSymbol(call), i)
						}
					}
				}
				return true
			})
			if uses != 1 {
				t.Errorf("identity passed to %d calls in ReviewSubjectAuthorize, want exactly 1 (CheckAccess)", uses)
			}
			return false
		})
	})
	if callers != 1 {
		t.Errorf("reviewSubjectIdentity has %d callers, want 1", callers)
	}
}
