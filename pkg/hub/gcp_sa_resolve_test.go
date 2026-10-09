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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#4016: --service-account accepts an id, email or display name,
// resolved by one hub-side resolver (resolveGCPServiceAccountRef) at every
// site that takes a caller-supplied or configured account reference.

// seedResolveSA registers a verified service account with the given scope,
// email and display name.
func seedResolveSA(t *testing.T, s store.Store, scope, scopeID, email, name, createdBy string) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:                 uuid.New().String(),
		Scope:              scope,
		ScopeID:            scopeID,
		Email:              email,
		DisplayName:        name,
		ProjectID:          "gcp-proj",
		CreatedBy:          createdBy,
		Verified:           true,
		VerificationStatus: store.GCPVerificationVerified,
		VerifiedAt:         time.Now(),
		CreatedAt:          time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

func uniqueSAEmail(prefix string) string {
	return fmt.Sprintf("%s-%s@example.com", prefix, uuid.New().String()[:8])
}

// ============================================================================
// The resolver
// ============================================================================

func TestResolveGCPServiceAccountRef(t *testing.T) {
	ctx := context.Background()
	const proj, other = "resolve-proj", "resolve-other"

	setup := func(t *testing.T) (*Server, store.Store) {
		t.Helper()
		return testServer(t)
	}

	t.Run("a UUID matches the id", func(t *testing.T) {
		srv, s := setup(t)
		sa := seedResolveSA(t, s, store.ScopeProject, proj, uniqueSAEmail("id"), "by-id", "u")
		got, err := srv.resolveGCPServiceAccountRef(ctx, proj, sa.ID)
		require.NoError(t, err)
		assert.Equal(t, sa.ID, got.ID)
	})

	t.Run("a UUID is returned whatever its scope; the caller checks reachability", func(t *testing.T) {
		srv, s := setup(t)
		sa := seedResolveSA(t, s, store.ScopeProject, other, uniqueSAEmail("id-other"), "x", "u")
		got, err := srv.resolveGCPServiceAccountRef(ctx, proj, sa.ID)
		require.NoError(t, err)
		assert.False(t, got.ReachableFromProject(proj))
	})

	t.Run("an email matches the project's account", func(t *testing.T) {
		srv, s := setup(t)
		email := uniqueSAEmail("em")
		sa := seedResolveSA(t, s, store.ScopeProject, proj, email, "em", "u")
		got, err := srv.resolveGCPServiceAccountRef(ctx, proj, email)
		require.NoError(t, err)
		assert.Equal(t, sa.ID, got.ID)
	})

	t.Run("an email matches a hub-scoped account", func(t *testing.T) {
		srv, s := setup(t)
		email := uniqueSAEmail("em-hub")
		sa := seedResolveSA(t, s, store.ScopeHub, "some-hub", email, "em-hub", "u")
		got, err := srv.resolveGCPServiceAccountRef(ctx, proj, email)
		require.NoError(t, err)
		assert.Equal(t, sa.ID, got.ID)
	})

	t.Run("on an email match the project-scoped account wins over the hub-scoped one", func(t *testing.T) {
		srv, s := setup(t)
		email := uniqueSAEmail("em-both")
		seedResolveSA(t, s, store.ScopeHub, "some-hub", email, "hub copy", "u")
		projSA := seedResolveSA(t, s, store.ScopeProject, proj, email, "project copy", "u")
		got, err := srv.resolveGCPServiceAccountRef(ctx, proj, email)
		require.NoError(t, err)
		assert.Equal(t, projSA.ID, got.ID)
	})

	t.Run("two hub-scoped accounts with one email are ambiguous", func(t *testing.T) {
		// Hub-scoped uniqueness is per hub instance id, which can change.
		srv, s := setup(t)
		email := uniqueSAEmail("em-hub2")
		a := seedResolveSA(t, s, store.ScopeHub, "hub-a", email, "a", "u")
		b := seedResolveSA(t, s, store.ScopeHub, "hub-b", email, "b", "u")
		_, err := srv.resolveGCPServiceAccountRef(ctx, proj, email)
		var amb *errGCPSAAmbiguous
		require.ErrorAs(t, err, &amb)
		assert.ElementsMatch(t, []string{a.ID, b.ID}, candidateIDs(amb))
	})

	t.Run("a display name matches", func(t *testing.T) {
		srv, s := setup(t)
		sa := seedResolveSA(t, s, store.ScopeProject, proj, uniqueSAEmail("dn"), "builder", "u")
		got, err := srv.resolveGCPServiceAccountRef(ctx, proj, "builder")
		require.NoError(t, err)
		assert.Equal(t, sa.ID, got.ID)
	})

	t.Run("a duplicate display name across scopes is ambiguous", func(t *testing.T) {
		srv, s := setup(t)
		hubSA := seedResolveSA(t, s, store.ScopeHub, "some-hub", uniqueSAEmail("dup-h"), "dup", "u")
		projSA := seedResolveSA(t, s, store.ScopeProject, proj, uniqueSAEmail("dup-p"), "dup", "u")
		_, err := srv.resolveGCPServiceAccountRef(ctx, proj, "dup")
		var amb *errGCPSAAmbiguous
		require.ErrorAs(t, err, &amb)
		// Project-scoped candidates are listed first.
		assert.Equal(t, []gcpSACandidate{
			{ID: projSA.ID, Scope: store.ScopeProject},
			{ID: hubSA.ID, Scope: store.ScopeHub},
		}, amb.candidates)
		assert.Contains(t, amb.Error(), projSA.ID)
		assert.Contains(t, amb.Error(), hubSA.ID)
	})

	t.Run("a duplicate display name within the project is ambiguous", func(t *testing.T) {
		srv, s := setup(t)
		seedResolveSA(t, s, store.ScopeProject, proj, uniqueSAEmail("dup2-a"), "twin", "u")
		seedResolveSA(t, s, store.ScopeProject, proj, uniqueSAEmail("dup2-b"), "twin", "u")
		_, err := srv.resolveGCPServiceAccountRef(ctx, proj, "twin")
		var amb *errGCPSAAmbiguous
		require.ErrorAs(t, err, &amb)
		assert.Len(t, amb.candidates, 2)
	})

	t.Run("another project's accounts are never matched by email or name", func(t *testing.T) {
		srv, s := setup(t)
		email := uniqueSAEmail("elsewhere")
		seedResolveSA(t, s, store.ScopeProject, other, email, "elsewhere", "u")
		// Nor do they make a reachable match ambiguous.
		mine := seedResolveSA(t, s, store.ScopeProject, proj, uniqueSAEmail("mine"), "shared-name", "u")
		seedResolveSA(t, s, store.ScopeProject, other, uniqueSAEmail("theirs"), "shared-name", "u")

		for _, ref := range []string{email, "elsewhere"} {
			_, err := srv.resolveGCPServiceAccountRef(ctx, proj, ref)
			assert.True(t, errors.Is(err, store.ErrNotFound), "ref %q: %v", ref, err)
		}
		got, err := srv.resolveGCPServiceAccountRef(ctx, proj, "shared-name")
		require.NoError(t, err)
		assert.Equal(t, mine.ID, got.ID)
	})

	t.Run("no match is ErrNotFound", func(t *testing.T) {
		srv, _ := setup(t)
		for _, ref := range []string{"", "  ", uuid.New().String(), "nobody@example.com", "no such name"} {
			_, err := srv.resolveGCPServiceAccountRef(ctx, proj, ref)
			assert.True(t, errors.Is(err, store.ErrNotFound), "ref %q: %v", ref, err)
		}
	})
}

func candidateIDs(amb *errGCPSAAmbiguous) []string {
	out := make([]string, 0, len(amb.candidates))
	for _, c := range amb.candidates {
		out = append(out, c.ID)
	}
	return out
}

// ============================================================================
// The four sites
// ============================================================================

// resolveSite is one site that takes a service-account reference. setup
// builds a fresh world and returns its project id, a store, the user who
// creates seeded accounts, and run, which submits ref at the site and
// returns the status, body and (on success) the assigned account id.
type resolveSite struct {
	name  string
	setup func(t *testing.T) (projectID string, s store.Store, createdBy string, run func(t *testing.T, ref string) (int, string, string))
}

func resolveSites() []resolveSite {
	return []resolveSite{
		{name: "create", setup: func(t *testing.T) (string, store.Store, string, func(*testing.T, string) (int, string, string)) {
			f := bypassAgentsSetup(t)
			n := 0
			return f.proj.ID, f.store, f.owner.ID, func(t *testing.T, ref string) (int, string, string) {
				n++
				rec := createAgentAsOwner(t, f, CreateAgentRequest{
					Name: fmt.Sprintf("resolve-create-%d", n),
					GCPIdentity: &GCPIdentityAssignment{
						MetadataMode:     store.GCPMetadataModeAssign,
						ServiceAccountID: ref,
					},
				})
				if rec.Code != http.StatusCreated {
					return rec.Code, rec.Body.String(), ""
				}
				var resp CreateAgentResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				require.NotNil(t, resp.Agent)
				return rec.Code, rec.Body.String(), assignedSAID(t, f.store, resp.Agent.ID)
			}
		}},
		{name: "patch", setup: func(t *testing.T) (string, store.Store, string, func(*testing.T, string) (int, string, string)) {
			f := bypassAgentsSetup(t)
			n := 0
			return f.proj.ID, f.store, f.owner.ID, func(t *testing.T, ref string) (int, string, string) {
				n++
				a := pendingAgentForPatch(t, f, fmt.Sprintf("resolve-patch-%d", n))
				rec := patchAgentSAAsOwner(t, f, a.ID, ref)
				if rec.Code != http.StatusOK {
					return rec.Code, rec.Body.String(), ""
				}
				return rec.Code, rec.Body.String(), assignedSAID(t, f.store, a.ID)
			}
		}},
		{name: "reincarnate", setup: func(t *testing.T) (string, store.Store, string, func(*testing.T, string) (int, string, string)) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			n := 0
			return project.ID, s, "someone", func(t *testing.T, ref string) (int, string, string) {
				n++
				agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
					a.ID = uuid.New().String()
					a.Slug = fmt.Sprintf("resolve-reincarnate-%d", n)
				})
				rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{ServiceAccount: ref})
				if rec.Code != http.StatusAccepted {
					return rec.Code, rec.Body.String(), ""
				}
				waitForReincarnationSettled(t, s, agent.ID)
				return rec.Code, rec.Body.String(), assignedSAID(t, s, agent.ID)
			}
		}},
		{name: "project default", setup: func(t *testing.T) (string, store.Store, string, func(*testing.T, string) (int, string, string)) {
			f := bypassAgentsSetup(t)
			checker := store.NewFakeCallerPermissionChecker()
			enforceSAAssign(f.srv, checker)
			pf := &profileDefaultFixture{bypassAgentsFixture: f}
			n := 0
			return f.proj.ID, f.store, f.owner.ID, func(t *testing.T, ref string) (int, string, string) {
				n++
				// Allow actAs on whatever the reference resolves to, so a
				// refusal below is the resolver's and not the gate's.
				all, err := f.store.ListGCPServiceAccounts(context.Background(), store.GCPServiceAccountFilter{})
				require.NoError(t, err)
				for _, sa := range all {
					checker.AllowTarget(sa.Email)
				}
				pf.setAnnotations(t, map[string]string{
					projectSettingDefaultGCPIdentityMode: store.GCPMetadataModeAssign,
					projectSettingDefaultGCPIdentitySAID: ref,
				})
				rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: fmt.Sprintf("resolve-default-%d", n)})
				if rec.Code != http.StatusCreated {
					return rec.Code, rec.Body.String(), ""
				}
				var resp CreateAgentResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				require.NotNil(t, resp.Agent)
				return rec.Code, rec.Body.String(), assignedSAID(t, f.store, resp.Agent.ID)
			}
		}},
	}
}

func assignedSAID(t *testing.T, s store.Store, agentID string) string {
	t.Helper()
	got, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	require.Equal(t, store.GCPMetadataModeAssign, got.AppliedConfig.GCPIdentity.MetadataMode)
	return got.AppliedConfig.GCPIdentity.ServiceAccountID
}

func TestGCPSAResolve_EachSiteAcceptsIDEmailAndDisplayName(t *testing.T) {
	for _, site := range resolveSites() {
		t.Run(site.name, func(t *testing.T) {
			projectID, s, createdBy, run := site.setup(t)
			sa := seedResolveSA(t, s, store.ScopeProject, projectID, uniqueSAEmail("forms"), "forms-account", createdBy)
			for _, ref := range []string{sa.ID, sa.Email, sa.DisplayName} {
				code, body, assigned := run(t, ref)
				require.Less(t, code, 300, "ref %q: %s", ref, body)
				assert.Equal(t, sa.ID, assigned, "ref %q", ref)
			}
		})
	}
}

func TestGCPSAResolve_EachSitePrefersProjectScopedOnEmail(t *testing.T) {
	for _, site := range resolveSites() {
		t.Run(site.name, func(t *testing.T) {
			projectID, s, createdBy, run := site.setup(t)
			email := uniqueSAEmail("both")
			seedResolveSA(t, s, store.ScopeHub, "some-hub", email, "hub copy", createdBy)
			projSA := seedResolveSA(t, s, store.ScopeProject, projectID, email, "project copy", createdBy)
			code, body, assigned := run(t, email)
			require.Less(t, code, 300, body)
			assert.Equal(t, projSA.ID, assigned)
		})
	}
}

func TestGCPSAResolve_EachSiteRefusesAmbiguousDisplayName(t *testing.T) {
	for _, site := range resolveSites() {
		t.Run(site.name, func(t *testing.T) {
			projectID, s, createdBy, run := site.setup(t)
			hubSA := seedResolveSA(t, s, store.ScopeHub, "some-hub", uniqueSAEmail("amb-h"), "same name", createdBy)
			projSA := seedResolveSA(t, s, store.ScopeProject, projectID, uniqueSAEmail("amb-p"), "same name", createdBy)

			code, body, _ := run(t, "same name")
			require.Equal(t, http.StatusBadRequest, code, body)

			var resp struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
					Details struct {
						Candidates []gcpSACandidate `json:"candidates"`
					} `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &resp), body)
			assert.Equal(t, ErrCodeIdentityAmbiguous, resp.Error.Code)
			assert.Equal(t, []gcpSACandidate{
				{ID: projSA.ID, Scope: store.ScopeProject},
				{ID: hubSA.ID, Scope: store.ScopeHub},
			}, resp.Error.Details.Candidates)
			assert.Contains(t, resp.Error.Message, projSA.ID)
			assert.Contains(t, resp.Error.Message, hubSA.ID)
		})
	}
}

// A reference that matches nothing reachable gets the same answer whatever
// its form, and the same answer as a reference to another project's account:
// the not-available text must not reveal whether the account exists.
func TestGCPSAResolve_EachSiteNotFoundIsOneAnswer(t *testing.T) {
	for _, site := range resolveSites() {
		t.Run(site.name, func(t *testing.T) {
			_, s, createdBy, run := site.setup(t)
			elsewhere := seedResolveSA(t, s, store.ScopeProject, tid("resolve-elsewhere"),
				uniqueSAEmail("elsewhere"), "elsewhere-name", createdBy)

			baseCode, baseBody, _ := run(t, uuid.New().String())
			require.Equal(t, http.StatusBadRequest, baseCode, baseBody)
			assert.Contains(t, baseBody, "not available")

			for _, ref := range []string{
				"nobody@example.com",
				"no such display name",
				elsewhere.ID,
				elsewhere.Email,
				elsewhere.DisplayName,
			} {
				code, body, _ := run(t, ref)
				requireIndistinguishable(t, oracleProbe{status: baseCode, body: baseBody}, oracleProbe{status: code, body: body})
			}
		})
	}
}

// An email typed in a different case still matches; a display name stays
// exact.
func TestResolveGCPServiceAccountRef_EmailIgnoresCase(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	const proj = "resolve-case-proj"
	sa := seedResolveSA(t, s, store.ScopeProject, proj, "Mixed.Case-Agent@Example.com", "Case Name", "u")

	for _, ref := range []string{"mixed.case-agent@example.com", "MIXED.CASE-AGENT@EXAMPLE.COM", sa.Email} {
		got, err := srv.resolveGCPServiceAccountRef(ctx, proj, ref)
		require.NoError(t, err, "ref %q", ref)
		assert.Equal(t, sa.ID, got.ID, "ref %q", ref)
	}
	_, err := srv.resolveGCPServiceAccountRef(ctx, proj, "case name")
	assert.True(t, errors.Is(err, store.ErrNotFound), "display names match exactly: %v", err)

	// Case-folding does not weaken precedence: a hub-scoped account whose
	// email differs only in case still loses to the project's own.
	seedResolveSA(t, s, store.ScopeHub, "some-hub", "mixed.case-agent@example.com", "hub copy", "u")
	got, err := srv.resolveGCPServiceAccountRef(ctx, proj, "MIXED.case-agent@example.com")
	require.NoError(t, err)
	assert.Equal(t, sa.ID, got.ID)
}

func TestGCPSAResolve_EachSiteMatchesEmailIgnoringCase(t *testing.T) {
	for _, site := range resolveSites() {
		t.Run(site.name, func(t *testing.T) {
			projectID, s, createdBy, run := site.setup(t)
			sa := seedResolveSA(t, s, store.ScopeProject, projectID, uniqueSAEmail("case"), "case-account", createdBy)
			code, body, assigned := run(t, strings.ToUpper(sa.Email))
			require.Less(t, code, 300, body)
			assert.Equal(t, sa.ID, assigned)
		})
	}
}

// The assign gate runs on the resolved account whatever form named it: a
// caller refused for an account by id gets the same 403, byte for byte, when
// naming it by email or display name. Each case varies one layer of the
// gate: hub-scope mode coupling, the GCP actAs check, and Hub permission.
func TestGCPSAResolve_AssignGateRunsForEveryForm(t *testing.T) {
	forms := func(sa *store.GCPServiceAccount) []string {
		return []string{sa.ID, sa.Email, sa.DisplayName}
	}
	requireSame403 := func(t *testing.T, run func(t *testing.T, ref string) (int, string), sa *store.GCPServiceAccount) {
		t.Helper()
		var first oracleProbe
		for i, ref := range forms(sa) {
			code, body := run(t, ref)
			require.Equal(t, http.StatusForbidden, code, "ref %q: %s", ref, body)
			if i == 0 {
				first = oracleProbe{status: code, body: body}
				continue
			}
			assert.Equal(t, first.body, body, "ref %q must be refused exactly as the id is", ref)
		}
	}

	type bypassSite struct {
		name string
		run  func(f *bypassAgentsFixture) func(t *testing.T, ref string) (int, string)
	}
	bypassSites := []bypassSite{
		{"create", func(f *bypassAgentsFixture) func(*testing.T, string) (int, string) {
			n := 0
			return func(t *testing.T, ref string) (int, string) {
				n++
				rec := createAgentAsOwner(t, f, CreateAgentRequest{
					Name: fmt.Sprintf("resolve-gate-create-%d", n),
					GCPIdentity: &GCPIdentityAssignment{
						MetadataMode:     store.GCPMetadataModeAssign,
						ServiceAccountID: ref,
					},
				})
				return rec.Code, rec.Body.String()
			}
		}},
		{"patch", func(f *bypassAgentsFixture) func(*testing.T, string) (int, string) {
			n := 0
			return func(t *testing.T, ref string) (int, string) {
				n++
				a := pendingAgentForPatch(t, f, fmt.Sprintf("resolve-gate-patch-%d", n))
				rec := patchAgentSAAsOwner(t, f, a.ID, ref)
				return rec.Code, rec.Body.String()
			}
		}},
	}

	for _, site := range bypassSites {
		t.Run(site.name+"/hub-scoped account while hub-scope assign is off", func(t *testing.T) {
			f := bypassAgentsSetup(t)
			ensureHubMembership(context.Background(), f.store, f.owner.ID)
			setMode(f.srv, SAAssignCheckOff)
			sa := seedResolveSA(t, f.store, store.ScopeHub, "some-hub", uniqueSAEmail("gate-hub"), "gate hub account", f.owner.ID)
			run := site.run(f)
			requireSame403(t, run, sa)
			_, body := run(t, sa.Email)
			assert.Contains(t, body, "gcpIamCheckMode=enforce")
		})

		t.Run(site.name+"/caller without actAs", func(t *testing.T) {
			f := bypassAgentsSetup(t)
			sa := seedResolveSA(t, f.store, store.ScopeProject, f.proj.ID, uniqueSAEmail("gate-actas"), "gate actas account", f.owner.ID)
			enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().DenyTarget(sa.Email, "no actAs grant"))
			requireSame403(t, site.run(f), sa)
		})
	}

	t.Run("reincarnate/caller without permission on the account", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		user := newReincarnateAuthzUser(t, s, "resolve-gate")
		grantAgentLifecycleAtProject(t, s, user.ID, project.ID)
		grantAgentDelegationAtProject(t, s, user.ID, project.ID)
		grantPermissionViaRoleBinding(t, s, user.ID, "agent.update", store.RoleScopeProject, project.ID)
		// Created by someone else; nothing grants this user read on it.
		sa := seedResolveSA(t, s, store.ScopeProject, project.ID, uniqueSAEmail("gate-reinc"), "gate reincarnate account", "someone-else")
		identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
		n := 0
		run := func(t *testing.T, ref string) (int, string) {
			n++
			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.ID = uuid.New().String()
				a.Slug = fmt.Sprintf("resolve-gate-reinc-%d", n)
			})
			req := reincarnateRequest(t, agent.ID, identity, ReincarnateAgentRequest{ServiceAccount: ref})
			rec := httptest.NewRecorder()
			srv.handleReincarnateAgent(rec, req, agent.ID)
			return rec.Code, rec.Body.String()
		}
		requireSame403(t, run, sa)
	})
}
