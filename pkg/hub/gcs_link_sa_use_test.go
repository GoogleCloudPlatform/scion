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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A gs:// link opens only while the sending agent may use its account the
// way the GCP token mint requires (authorizeGCSLinkServiceAccountUse): the
// use decision for the stored sender, whose step 10 denies an unrecorded hop
// on the agent's chain and is otherwise the account's parent ceiling, then
// actAs on the recorded source in enforce mode. A refusal answers the same
// not-found response as every other unavailable object, before any token is
// minted; the audit event names sa_use_not_authorized.

// gcsUseScenario is an agent-sent message in a project topic, read by a
// project member, with the agent assigned a verified project account and
// the assignment recorded from gcsAssignerID(agent).
type gcsUseScenario struct {
	gcsSAScenario
	agent *store.Agent
}

func newGCSUseScenario(t *testing.T, f *gcsFixture, seed string) gcsUseScenario {
	t.Helper()
	project := gcsTestProject(t, f, seed+"-project")
	sa := gcsTestSA(t, f.store, project, seed+"-sa", "sa-"+seed+"@test.iam.gserviceaccount.com")
	agent := gcsTestAgent(t, f.store, project, seed+"-agent", gcsAssignedIdentity(sa))
	conv := gcsTestTopic(t, f.store, project, seed+"-topic")
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: seed + "-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
		body: "see gs://bkt/o.txt", agentID: agent.ID, conversationID: conv.ID,
	})
	viewer := gcsTestUser(t, f.store, seed+"-viewer")
	addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 2, Generation: 1}, []byte("hi"))
	return gcsUseScenario{gcsSAScenario: gcsSAScenario{viewer: viewer, msg: msg, sa: sa}, agent: agent}
}

// useDecision is the stored-agent use decision the proxy makes for the
// scenario's sender and account.
func (sc gcsUseScenario) useDecision(t *testing.T, f *gcsFixture) Decision {
	t.Helper()
	agent, err := f.store.GetAgent(context.Background(), sc.agent.ID)
	require.NoError(t, err)
	return f.srv.authzService.decideStoredAgentServiceAccountUse(context.Background(), agent, sc.sa)
}

// gcsEnforceActAs turns on the enforce check mode with checker, keeping the
// fixture's fake token generator (enforceSAAssign would replace it).
func gcsEnforceActAs(srv *Server, checker store.CallerPermissionChecker) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.saAssignCheckMode = SAAssignCheckEnforce
	srv.saAssignChecker = checker
}

func requireGCSFetchAllowed(t *testing.T, f *gcsFixture, rec *httptest.ResponseRecorder, sa *store.GCPServiceAccount) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	call, ok := f.gen.lastCall()
	require.True(t, ok, "expected a mint call")
	require.Equal(t, sa.Email, call.email)
}

// TestGCSLink_SAUse_RecordedSourceWithAssignAdmitted: a live recorded source
// holding assign on the exact account admits the fetch, and the use decision
// names the parent proof.
func TestGCSLink_SAUse_RecordedSourceWithAssignAdmitted(t *testing.T) {
	f := newGCSFixture(t)
	sc := newGCSUseScenario(t, f, "use-admit")

	requireGCSFetchAllowed(t, f, sc.fetch(t, f), sc.sa)
	d := sc.useDecision(t, f)
	require.True(t, d.Allowed, "cause=%q reason=%q", d.DenyCause, d.Reason)
	require.NotNil(t, d.parentProof)
	assert.Equal(t, gcsAssignerID(sc.agent), d.parentProof.SourcePrincipal.ID)
	assert.Contains(t, d.Reason, "parent proof: gcp_service_account.assign")
}

// TestGCSLink_SAUse_RefusedAboveCeiling: a recorded source without assign,
// from the start or after it lost it, refuses the fetch.
func TestGCSLink_SAUse_RefusedAboveCeiling(t *testing.T) {
	t.Run("source loses assign", func(t *testing.T) {
		f := newGCSFixture(t)
		sc := newGCSUseScenario(t, f, "use-lost")
		d := sc.useDecision(t, f)
		require.True(t, d.Allowed, "control: the source holds assign: cause=%q reason=%q", d.DenyCause, d.Reason)

		_, err := f.store.DeleteRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, gcsAssignerID(sc.agent))
		require.NoError(t, err)
		requireGCSUniformDeny(t, f, sc.fetch(t, f), GCSLinkReasonSAUseNotAuthorized)
		requireUseDeniedWithCause(t, sc.useDecision(t, f), DenyCauseCeilingDelegatorLacksPermission)
	})
	t.Run("source never held assign", func(t *testing.T) {
		f := newGCSFixture(t)
		sc := newGCSUseScenario(t, f, "use-noassign")
		noAssign := tid("use-noassign-source")
		scaCreateDelegatorWithoutAssign(t, f.store, noAssign, "use-noassign-source@test.com", sc.agent.ProjectID)
		seedAssignment(t, f.store, sc.agent, noAssign, sc.sa.ID)

		requireGCSUniformDeny(t, f, sc.fetch(t, f), GCSLinkReasonSAUseNotAuthorized)
		requireUseDeniedWithCause(t, sc.useDecision(t, f), DenyCauseCeilingDelegatorLacksPermission)
	})
}

// TestGCSLink_SAUse_RefusedUnrecorded: no recorded assignment, or an
// unrecorded edge on the sender's own chain, refuses the fetch. The same
// sender with a recorded edge is admitted, so only the edge differs.
func TestGCSLink_SAUse_RefusedUnrecorded(t *testing.T) {
	t.Run("assignment not recorded", func(t *testing.T) {
		f := newGCSFixture(t)
		sc := newGCSUseScenario(t, f, "use-noprov")
		_, err := f.store.DeactivateAgentServiceAccountAssignments(context.Background(), sc.agent.ID,
			store.Deactivation{Cause: store.EdgeDeactivationSACleared, OpID: "op-gcs-clear"})
		require.NoError(t, err)

		requireGCSUniformDeny(t, f, sc.fetch(t, f), GCSLinkReasonSAUseNotAuthorized)
		requireUseDeniedWithCause(t, sc.useDecision(t, f), DenyCauseCeilingProvenanceMissing)
	})
	for _, tc := range []struct {
		name     string
		edge     func(t *testing.T, s store.Store, delegatorType, delegatorID, delegateType, delegateID, scopeType, scopeID, role string)
		admitted bool
	}{
		{"unrecorded edge", createDCEdge, false},
		{"recorded edge", seedRecordedDelegationEdge, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGCSFixture(t)
			seed := "use-edge-recorded"
			if !tc.admitted {
				seed = "use-edge-unrecorded"
			}
			sc := newGCSUseScenario(t, f, seed)
			setBackfillCompleted(t, f.store)
			root := tid(seed + "-root")
			createDCUser(t, f.store, root, seed+"-root@test.com", sc.agent.ProjectID, store.ProjectRoleOwner)
			tc.edge(t, f.store, store.DelegationPrincipalUser, root, store.DelegationPrincipalAgent, sc.agent.ID,
				store.RoleScopeProject, sc.agent.ProjectID, store.ProjectRoleOwner)

			if tc.admitted {
				requireGCSFetchAllowed(t, f, sc.fetch(t, f), sc.sa)
				return
			}
			requireGCSUniformDeny(t, f, sc.fetch(t, f), GCSLinkReasonSAUseNotAuthorized)
			requireUseDeniedWithCause(t, sc.useDecision(t, f), DenyCauseCeilingUnrecorded)
		})
	}
}

// TestGCSLink_SAUse_AccessConstraint: an access constraint on the sending
// agent applies to the proxy's use decision as it does to the agent's own
// token request (Decide's step 7c). A constraint whose maximum permissions
// keep gcp_service_account.use admits the fetch; one that leaves it out
// refuses the fetch before any token is minted.
func TestGCSLink_SAUse_AccessConstraint(t *testing.T) {
	for _, tc := range []struct {
		name     string
		maximum  []string
		admitted bool
	}{
		{"constraint keeps use", []string{"agent.read", "gcp_service_account.use"}, true},
		{"constraint removes use", []string{"agent.read"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGCSFixture(t)
			seed := "use-ac-keep"
			if !tc.admitted {
				seed = "use-ac-remove"
			}
			sc := newGCSUseScenario(t, f, seed)
			subjectType, subjectID := "agent", sc.agent.ID
			_, err := f.store.CreateAccessConstraint(context.Background(), &store.AccessConstraint{
				Name:                 seed + "-boundary",
				SubjectKind:          store.ConstraintSubjectPrincipal,
				SubjectPrincipalType: &subjectType,
				SubjectPrincipalID:   &subjectID,
				ScopeType:            "system",
				MaximumPermissions:   tc.maximum,
				Purpose:              "stored-agent use decision test",
				CreatedBy:            sc.viewer.ID,
			})
			require.NoError(t, err)

			if tc.admitted {
				requireGCSFetchAllowed(t, f, sc.fetch(t, f), sc.sa)
				d := sc.useDecision(t, f)
				require.True(t, d.Allowed, "cause=%q reason=%q", d.DenyCause, d.Reason)
				return
			}
			requireGCSUniformDeny(t, f, sc.fetch(t, f), GCSLinkReasonSAUseNotAuthorized)
			d := sc.useDecision(t, f)
			require.False(t, d.Allowed, "reason %q", d.Reason)
			assert.Empty(t, d.DenyCause, "reason %q", d.Reason)
			assert.Equal(t, "restriction removed permission: access_constraint - permission removed by access_constraint", d.Reason)
			assert.Nil(t, d.parentProof, "the constraint denies before step 10")
		})
	}
}

// switchedConstraintsStore fails ListAccessConstraints with err once its
// fault switch is armed, and delegates until then.
type switchedConstraintsStore struct {
	store.Store
	fault *storeFaultSwitch
	err   error
}

func (s *switchedConstraintsStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	if s.fault.Active() {
		return nil, s.err
	}
	return s.Store.ListAccessConstraints(ctx, limit, offset)
}

// TestGCSLink_SAUse_AccessConstraintFailuresDeny: the proxy's use decision
// fails closed when step 7c cannot be evaluated. A failure to resolve the
// sending agent's principal closure, or to load the access constraints,
// denies with resolution_error before step 10, as Decide tags them. Each
// case is the admitted scenario with one store read failing: the wrapper
// is installed on the authorization service right after the fixture is
// built and delegates until its switch is armed.
func TestGCSLink_SAUse_AccessConstraintFailuresDeny(t *testing.T) {
	require.Equal(t, "access constraint load error (fail-closed)", reasonStoredAgentConstraintLoadError,
		"the constraint-load deny names the failure")
	injected := errors.New("injected store failure")
	for _, tc := range []struct {
		name   string
		faulty func(inner store.Store, fault *storeFaultSwitch) store.Store
		reason string
	}{
		{
			"principal resolution error",
			func(inner store.Store, fault *storeFaultSwitch) store.Store {
				return &errorInjectingStore{Store: inner, fault: fault, getEffectiveGroupsForAgentErr: injected}
			},
			"principal resolution error (fail-closed)",
		},
		{
			"constraint load error",
			func(inner store.Store, fault *storeFaultSwitch) store.Store {
				return &switchedConstraintsStore{Store: inner, fault: fault, err: injected}
			},
			reasonStoredAgentConstraintLoadError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGCSFixture(t)
			fault := &storeFaultSwitch{}
			f.srv.authzService.store = tc.faulty(f.srv.authzService.store, fault)
			sc := newGCSUseScenario(t, f, "use-ac-fault")
			control := sc.useDecision(t, f)
			require.True(t, control.Allowed, "control: cause=%q reason=%q", control.DenyCause, control.Reason)

			fault.Arm()
			d := sc.useDecision(t, f)
			require.False(t, d.Allowed, "reason %q", d.Reason)
			assert.Equal(t, DenyCauseResolutionError, d.DenyCause, "reason %q", d.Reason)
			assert.Equal(t, tc.reason, d.Reason)
			assert.Nil(t, d.parentProof, "the failure denies before step 10")
		})
	}
}

// TestGCSLink_SAUse_AccessConstraintRestrictionsHaveCheck pins why the
// nil-Check deny in storedAgentAccessConstraintDeny is unreachable from
// store data: every restriction accessConstraintRestrictions builds for
// the sending agent carries a Check (ConstraintsToRestrictions sets one on
// each), so the stored constraint is applied by its maximum permissions.
// The deny branch stays as a fail-closed guard against a future source of
// restrictions without one.
func TestGCSLink_SAUse_AccessConstraintRestrictionsHaveCheck(t *testing.T) {
	f := newGCSFixture(t)
	sc := newGCSUseScenario(t, f, "use-ac-check")
	subjectType, subjectID := "agent", sc.agent.ID
	_, err := f.store.CreateAccessConstraint(context.Background(), &store.AccessConstraint{
		Name:                 "use-ac-check-boundary",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &subjectType,
		SubjectPrincipalID:   &subjectID,
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read", "gcp_service_account.use"},
		Purpose:              "stored-agent use decision test",
		CreatedBy:            sc.viewer.ID,
	})
	require.NoError(t, err)

	r := gcpServiceAccountResource(sc.sa)
	restrictions, err := f.srv.authzService.accessConstraintRestrictions(context.Background(),
		map[string]struct{}{"agent:" + sc.agent.ID: {}},
		ResourceContext{ResourceType: r.Type, ResourceID: r.ID, OwnerID: r.OwnerID, ProjectID: projectIDForResource(r), Ancestry: r.Ancestry})
	require.NoError(t, err)
	require.Len(t, restrictions, 1, "the constraint on the agent applies")
	for _, rr := range restrictions {
		require.NotNil(t, rr.Check, "restriction %s", rr.Kind)
		assert.True(t, rr.Check("gcp_service_account.use"))
		assert.False(t, rr.Check("agent.delete"))
	}
	d := sc.useDecision(t, f)
	require.True(t, d.Allowed, "cause=%q reason=%q", d.DenyCause, d.Reason)
}

// TestGCSLink_SAUse_SenderStanding: the proxy refuses a sending agent that
// is not in good standing, as the GCP token handlers refuse its own token
// request (agentStandingForbidden): a held agent, or an agent whose root
// user is inactive. The refusal comes before the use decision and answers
// the uniform not-found response before any token is minted; the use
// decision itself still admits, so standing is the only cause. The clean
// agent is the admitted control.
func TestGCSLink_SAUse_SenderStanding(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(t *testing.T, f *gcsFixture, sc gcsUseScenario)
	}{
		{"in good standing", nil},
		{"held agent", func(t *testing.T, f *gcsFixture, sc gcsUseScenario) {
			holdAgentFor(t, f.store, sc.agent.ID, sc.agent.ProjectID, gcsStandingRootID(sc.agent.Slug))
		}},
		{"inactive root", func(t *testing.T, f *gcsFixture, sc gcsUseScenario) {
			ctx := context.Background()
			u, err := f.store.GetUser(ctx, gcsStandingRootID(sc.agent.Slug))
			require.NoError(t, err)
			u.Status = store.UserStatusSuspended
			require.NoError(t, f.store.UpdateUser(ctx, u))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGCSFixture(t)
			sc := newGCSUseScenario(t, f, "use-standing")
			require.NoError(t, f.srv.agentStanding(context.Background(), sc.agent.ID), "precondition: the sender is in good standing")
			if tc.fault == nil {
				requireGCSFetchAllowed(t, f, sc.fetch(t, f), sc.sa)
				return
			}
			tc.fault(t, f, sc)
			require.Error(t, f.srv.agentStanding(context.Background(), sc.agent.ID), "precondition: the sender is not in good standing")

			requireGCSUniformDeny(t, f, sc.fetch(t, f), GCSLinkReasonSAUseNotAuthorized)
			d := sc.useDecision(t, f)
			require.True(t, d.Allowed, "the use decision admits; standing refused: cause=%q reason=%q", d.DenyCause, d.Reason)
		})
	}
}

// TestGCSLink_SAUse_ActAsEnforceMode: in enforce mode the recorded source,
// never the agent or the viewer, must pass actAs on the account; a denial
// refuses the fetch before any token is minted.
func TestGCSLink_SAUse_ActAsEnforceMode(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		name := "denied"
		if allowed {
			name = "allowed"
		}
		t.Run(name, func(t *testing.T) {
			f := newGCSFixture(t)
			sc := newGCSUseScenario(t, f, "use-actas-"+name)
			checker := store.NewFakeCallerPermissionChecker()
			if allowed {
				checker.AllowTarget(sc.sa.Email)
			} else {
				checker.DenyTarget(sc.sa.Email, "no actAs grant")
			}
			gcsEnforceActAs(f.srv, checker)

			rec := sc.fetch(t, f)
			if allowed {
				requireGCSFetchAllowed(t, f, rec, sc.sa)
			} else {
				requireGCSUniformDeny(t, f, rec, GCSLinkReasonSAUseNotAuthorized)
			}
			calls := checker.Calls()
			require.Len(t, calls, 1, "one actAs check for the fetch")
			assert.Equal(t, store.PrincipalUser, calls[0].Caller.Kind)
			assert.Equal(t, gcsAssignerID(sc.agent), calls[0].Caller.ID, "the recorded source is the actAs caller")
			assert.Equal(t, sc.sa.ID, calls[0].TargetSAID)
		})
	}
}

// TestGCSLink_SAUse_DecisionAlwaysAudited: the stored-agent use decision is
// recorded for every fetch that reaches it, allow and deny, with the sender
// as principal and gcp_service_account.use as the permission.
func TestGCSLink_SAUse_DecisionAlwaysAudited(t *testing.T) {
	f := newGCSFixture(t)
	f.srv.authzService.DecisionAuditSampleRate = 0
	em := &recordingDecisionAuditEmitter{}
	prev := f.srv.authzService.decisionAuditEmitter
	f.srv.authzService.SetDecisionAuditEmitter(em)
	t.Cleanup(func() { f.srv.authzService.SetDecisionAuditEmitter(prev) })
	sc := newGCSUseScenario(t, f, "use-audit")

	useRecords := func() []*store.DecisionAuditRecord {
		var out []*store.DecisionAuditRecord
		for _, r := range em.records {
			if r.PermissionID == "gcp_service_account.use" && r.ResourceID == sc.sa.ID {
				out = append(out, r)
			}
		}
		return out
	}

	requireGCSFetchAllowed(t, f, sc.fetch(t, f), sc.sa)
	got := useRecords()
	require.Len(t, got, 1)
	assert.Equal(t, "allow", got[0].Result)
	assert.Equal(t, sc.agent.ID, got[0].PrincipalID)

	_, err := f.store.DeleteRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, gcsAssignerID(sc.agent))
	require.NoError(t, err)
	rec := sc.fetch(t, f)
	require.Equal(t, http.StatusNotFound, rec.Code)
	got = useRecords()
	require.Len(t, got, 2)
	assert.Equal(t, "deny", got[1].Result)
	assert.Equal(t, string(DeniedByDelegationCeiling), got[1].DeniedBy)
}
