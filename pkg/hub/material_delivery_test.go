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

// Tests for launch material selection under the delivery credential
// (ptone/scion#2129): the association relationship rules
// (authz_material_association.go), the start/restart selector and the
// availability probe (material_delivery.go).

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ldFixture is a golden fixture plus one delivery-eligible agent in project
// alpha: ancestry [project owner], placed on its own runtime broker, with
// a recorded principal edge from the project owner (unless the caller
// changes or omits it), a local secret backend and a dispatcher wired to the
// golden authorization service.
type ldFixture struct {
	*goldenFixture
	hubID   string
	broker  *store.RuntimeBroker
	agent   *store.Agent
	backend *secret.LocalBackend
	client  *mockRuntimeBrokerClient
	d       *HTTPAgentDispatcher
}

type ldOptions struct {
	// noEdge leaves the agent without a delegation edge.
	noEdge bool
	// edge adjusts the agent's edge before it is written.
	edge func(e *store.DelegationEdge)
	// rootMemberNone makes the golden hub member with no project
	// membership the agent's root user instead of the project owner.
	rootMemberNone bool
}

func newLDFixture(t *testing.T, name string, opts ldOptions) *ldFixture {
	t.Helper()
	f := newGoldenFixture(t)
	ctx := context.Background()
	m := &ldFixture{goldenFixture: f, hubID: tid("md-hub-" + name)}

	m.broker = &store.RuntimeBroker{
		ID: tid("md-broker-" + name), Name: "md-broker-" + name, Slug: "md-broker-" + name,
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, m.broker))

	root := f.projectOwnerID
	if opts.rootMemberNone {
		root = f.memberNoneID
	}
	id := tid("md-agent-" + name)
	m.agent = &store.Agent{
		ID: id, Slug: "md-" + id[:8], Name: "md-" + id[:8],
		ProjectID: f.projectAlpha.ID, Phase: string(state.PhaseRunning),
		OwnerID: root, CreatedBy: root, Ancestry: []string{root},
		RuntimeBrokerID: m.broker.ID,
		AppliedConfig:   &store.AgentAppliedConfig{},
	}
	require.NoError(t, f.store.CreateAgent(ctx, m.agent))
	if !opts.noEdge {
		hdEdge(t, f.store, store.DelegationPrincipalUser, root, id, f.projectAlpha.ID, opts.edge)
	}

	m.backend = secret.NewLocalBackend(f.store, m.hubID, "test-secret")
	m.client = &mockRuntimeBrokerClient{}
	m.d = NewHTTPAgentDispatcherWithClient(f.store, m.client, false, slog.Default())
	m.d.SetAuthzService(f.authz)
	m.d.SetHubID(m.hubID)
	m.d.SetSecretBackend(m.backend)
	return m
}

// scopeID returns the scope ID an item of scope is stored under for the
// fixture agent.
func (m *ldFixture) scopeID(scope string) string {
	switch scope {
	case store.ScopeProject:
		return m.agent.ProjectID
	case store.ScopeHub:
		return m.hubID
	case store.ScopeRuntimeBroker:
		return m.broker.ID
	case store.ScopeUser:
		return m.agent.OwnerID
	}
	return ""
}

// env stores an always-mode env var row and returns it.
func (m *ldFixture) env(t *testing.T, key, value, scope, scopeID string, secretFlag bool) store.EnvVar {
	t.Helper()
	v := store.EnvVar{
		ID: api.NewUUID(), Key: key, Value: value, Scope: scope, ScopeID: scopeID,
		InjectionMode: store.InjectionModeAlways, Secret: secretFlag, CreatedBy: m.projectOwnerID,
	}
	require.NoError(t, m.store.CreateEnvVar(context.Background(), &v))
	return v
}

// secret stores an always-mode secret through the local backend and returns
// its metadata.
func (m *ldFixture) secret(t *testing.T, name, value, secretType, scope, scopeID string) secret.SecretMeta {
	t.Helper()
	_, meta, err := m.backend.Set(context.Background(), &secret.SetSecretInput{
		Name: name, Value: value, SecretType: secretType, Target: name,
		Scope: scope, ScopeID: scopeID, InjectionMode: store.InjectionModeAlways,
		CreatedBy: m.projectOwnerID,
	})
	require.NoError(t, err)
	require.NotNil(t, meta)
	return *meta
}

// decideItem runs one association deliver decision for the fixture agent
// with explain on, through the production resource builder.
func (m *ldFixture) decideItem(t *testing.T, a *AuthzService, h *hubDeliveryIdentity, kind MaterialKind, record materialRecordKind, id, key, scope, scopeID string) Decision {
	t.Helper()
	grant, ok := grantForDeliveryScope(scope)
	require.True(t, ok)
	res := materialDeliveryResource(kind, record, id, key, scope, scopeID, m.agent.ID, grant, PurposeRestart)
	return decidePerm(a, h, res, ActionDeliver, deliverPermissionForKind(kind), true)
}

// ldFaultStore injects single store faults for the association and the
// admission walk.
type ldFaultStore struct {
	store.Store
	getSecretErr      error
	getEnvVarErr      error
	getBrokerErr      error
	getProjectErr     error
	holdErr           error
	holdErrID         string
	getSecretOverride *store.Secret
}

func (s *ldFaultStore) GetSecret(ctx context.Context, key, scope, scopeID string) (*store.Secret, error) {
	if s.getSecretErr != nil {
		return nil, s.getSecretErr
	}
	if s.getSecretOverride != nil {
		cp := *s.getSecretOverride
		return &cp, nil
	}
	return s.Store.GetSecret(ctx, key, scope, scopeID)
}

func (s *ldFaultStore) GetEnvVar(ctx context.Context, key, scope, scopeID string) (*store.EnvVar, error) {
	if s.getEnvVarErr != nil {
		return nil, s.getEnvVarErr
	}
	return s.Store.GetEnvVar(ctx, key, scope, scopeID)
}

func (s *ldFaultStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	if s.getBrokerErr != nil {
		return nil, s.getBrokerErr
	}
	return s.Store.GetRuntimeBroker(ctx, id)
}

func (s *ldFaultStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if s.getProjectErr != nil {
		return nil, s.getProjectErr
	}
	return s.Store.GetProject(ctx, id)
}

func (s *ldFaultStore) HasActiveAgentHold(ctx context.Context, agentID string) (bool, error) {
	if s.holdErr != nil && (s.holdErrID == "" || s.holdErrID == agentID) {
		return false, s.holdErr
	}
	return s.Store.HasActiveAgentHold(ctx, agentID)
}

// =============================================================================
// Association rules through Decide
// =============================================================================

// TestMaterialAssociation_DecideAdmitted: every associated scope and kind is
// admitted for the delivery credential through its association rule, with
// no sharing source (stage 4 does not apply), and the step-10 admission walk
// ran and admitted the root user.
func TestMaterialAssociation_DecideAdmitted(t *testing.T) {
	m := newLDFixture(t, "admit", ldOptions{})
	h := hdIdentity(t, m.authz, m.agent.ID)

	type item struct {
		name   string
		kind   MaterialKind
		record materialRecordKind
		scope  string
		id     string
		key    string
	}
	var items []item
	for _, scope := range []string{store.ScopeProject, store.ScopeHub, store.ScopeRuntimeBroker} {
		sm := m.secret(t, "MD_SECRET_"+scope, "secret-"+scope, store.SecretTypeEnvironment, scope, m.scopeID(scope))
		items = append(items, item{scope + "/secret", MaterialKindSecret, materialRecordSecret, scope, sm.ID, sm.Name})
		ev := m.env(t, "MD_ENV_"+scope, "env-"+scope, scope, m.scopeID(scope), false)
		items = append(items, item{scope + "/env_var", MaterialKindEnvVar, materialRecordEnvVar, scope, ev.ID, ev.Key})
		lv := m.env(t, "MD_LEGACY_"+scope, "legacy-"+scope, scope, m.scopeID(scope), true)
		items = append(items, item{scope + "/legacy_secret_env", MaterialKindSecret, materialRecordEnvVar, scope, lv.ID, lv.Key})
	}

	for _, it := range items {
		t.Run(it.name, func(t *testing.T) {
			rule, ok := materialAssociationRule(it.scope)
			require.True(t, ok)
			d := m.decideItem(t, m.authz, h, it.kind, it.record, it.id, it.key, it.scope, m.scopeID(it.scope))
			require.True(t, d.Allowed, "reason %q", d.Reason)
			assert.Equal(t, "relationship grant: "+string(rule), d.Reason)
			assert.Equal(t, string(rule), d.MatchedGrant)
			assert.Equal(t, ScopeTypeRelationship, d.Scope)
			rr := relationshipResult(t, d, rule)
			assert.True(t, rr.Accepted)
			assert.Nil(t, rr.Source, "an association candidate has no sharing source, so stage 4 never runs for it")
			assert.Equal(t, 1, countDecisionSteps(d.ExplainTrace, "delegation_ceiling_delivery_hop"), "trace: %+v", d.ExplainTrace)
			assert.True(t, hasDecisionStep(d.ExplainTrace, "delegation_ceiling_delivery_root"), "trace: %+v", d.ExplainTrace)
		})
	}
}

// TestMaterialAssociation_SingleFaultDenied: each row changes exactly one
// fact of an admitted project, hub or broker item and is denied.
func TestMaterialAssociation_SingleFaultDenied(t *testing.T) {
	injected := errors.New("injected store failure")
	type row struct {
		name string
		// item returns the decision inputs; the default is the admitted
		// project secret.
		run func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, base secret.SecretMeta) Decision
		// wantDetail is the association candidate's rejection detail, when
		// the association fact rejects; empty when no association candidate
		// is built or a later step denies.
		wantDetail string
		// noCandidate: no association candidate is built at all.
		noCandidate bool
	}
	decideRes := func(t *testing.T, m *ldFixture, a *AuthzService, h *hubDeliveryIdentity, res Resource, perm string) Decision {
		t.Helper()
		return decidePerm(a, h, res, ActionDeliver, perm, true)
	}
	projectRes := func(m *ldFixture, base secret.SecretMeta) Resource {
		return materialDeliveryResource(MaterialKindSecret, materialRecordSecret, base.ID, base.Name, store.ScopeProject, m.agent.ProjectID, m.agent.ID, GrantProjectAssociation, PurposeRestart)
	}
	rows := []row{
		{
			name: "other_project_item",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, _ secret.SecretMeta) Decision {
				other := m.secret(t, "MD_OTHER_PROJECT", "v", store.SecretTypeEnvironment, store.ScopeProject, m.projectBeta.ID)
				return m.decideItem(t, m.authz, h, MaterialKindSecret, materialRecordSecret, other.ID, other.Name, store.ScopeProject, m.projectBeta.ID)
			},
			wantDetail: "delivery item is not associated with the target agent",
		},
		{
			name: "other_broker_item",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, _ secret.SecretMeta) Decision {
				otherBroker := tid("md-fault-other-broker")
				other := m.secret(t, "MD_OTHER_BROKER", "v", store.SecretTypeEnvironment, store.ScopeRuntimeBroker, otherBroker)
				return m.decideItem(t, m.authz, h, MaterialKindSecret, materialRecordSecret, other.ID, other.Name, store.ScopeRuntimeBroker, otherBroker)
			},
			wantDetail: "delivery item is not associated with the target agent",
		},
		{
			name: "broker_record_missing",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, _ secret.SecretMeta) Decision {
				b := m.secret(t, "MD_BROKER_GONE", "v", store.SecretTypeEnvironment, store.ScopeRuntimeBroker, m.broker.ID)
				a := hdWithStore(t, m.authz, &ldFaultStore{Store: m.store, getBrokerErr: store.ErrNotFound})
				return m.decideItem(t, a, h, MaterialKindSecret, materialRecordSecret, b.ID, b.Name, store.ScopeRuntimeBroker, m.broker.ID)
			},
			wantDetail: "target agent's runtime broker does not exist",
		},
		{
			name: "broker_lookup_error",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, _ secret.SecretMeta) Decision {
				b := m.secret(t, "MD_BROKER_ERR", "v", store.SecretTypeEnvironment, store.ScopeRuntimeBroker, m.broker.ID)
				a := hdWithStore(t, m.authz, &ldFaultStore{Store: m.store, getBrokerErr: injected})
				return m.decideItem(t, a, h, MaterialKindSecret, materialRecordSecret, b.ID, b.Name, store.ScopeRuntimeBroker, m.broker.ID)
			},
			wantDetail: "delivery association check failed (fail-closed): broker lookup failed",
		},
		{
			name: "project_record_missing",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, base secret.SecretMeta) Decision {
				a := hdWithStore(t, m.authz, &ldFaultStore{Store: m.store, getProjectErr: store.ErrNotFound})
				return decideRes(t, m, a, h, projectRes(m, base), "secret.deliver")
			},
			wantDetail: "target agent's project does not exist",
		},
		{
			name: "hub_id_unknown",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, _ secret.SecretMeta) Decision {
				hs := m.secret(t, "MD_HUB_NOID", "v", store.SecretTypeEnvironment, store.ScopeHub, m.hubID)
				return m.decideItem(t, m.authz, h, MaterialKindSecret, materialRecordSecret, hs.ID, hs.Name, store.ScopeHub, "")
			},
			wantDetail: "delivery item scope does not apply to the target agent",
		},
		{
			name: "hub_item_of_another_hub",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, _ secret.SecretMeta) Decision {
				otherHub := tid("md-fault-other-hub")
				hs := m.secret(t, "MD_HUB_OTHER", "v", store.SecretTypeEnvironment, store.ScopeHub, otherHub)
				return m.decideItem(t, m.authz, h, MaterialKindSecret, materialRecordSecret, hs.ID, hs.Name, store.ScopeHub, m.hubID)
			},
			wantDetail: "material record not found",
		},
		{
			name: "record_id_mismatch",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, base secret.SecretMeta) Decision {
				res := projectRes(m, base)
				res.ID = tid("md-fault-other-id")
				return decideRes(t, m, m.authz, h, res, "secret.deliver")
			},
			wantDetail: "material record does not match the delivery item",
		},
		{
			name: "record_missing",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, base secret.SecretMeta) Decision {
				res := projectRes(m, base)
				res.deliveryItem.Key = "MD_NO_SUCH_KEY"
				return decideRes(t, m, m.authz, h, res, "secret.deliver")
			},
			wantDetail: "material record not found",
		},
		{
			name: "record_lookup_error",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, base secret.SecretMeta) Decision {
				a := hdWithStore(t, m.authz, &ldFaultStore{Store: m.store, getSecretErr: injected})
				return decideRes(t, m, a, h, projectRes(m, base), "secret.deliver")
			},
			wantDetail: "delivery association check failed (fail-closed): material lookup failed",
		},
		{
			name: "internal_secret",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, base secret.SecretMeta) Decision {
				rec, err := m.store.GetSecret(context.Background(), base.Name, base.Scope, base.ScopeID)
				require.NoError(t, err)
				rec.SecretType = store.SecretTypeInternal
				a := hdWithStore(t, m.authz, &ldFaultStore{Store: m.store, getSecretOverride: rec})
				return decideRes(t, m, a, h, projectRes(m, base), "secret.deliver")
			},
			wantDetail: "material record is not deliverable",
		},
		{
			name: "plain_env_requested_as_secret",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, _ secret.SecretMeta) Decision {
				ev := m.env(t, "MD_PLAIN_AS_SECRET", "v", store.ScopeProject, m.agent.ProjectID, false)
				return m.decideItem(t, m.authz, h, MaterialKindSecret, materialRecordEnvVar, ev.ID, ev.Key, store.ScopeProject, m.agent.ProjectID)
			},
			wantDetail: "material record does not match its kind",
		},
		{
			name: "secret_env_requested_as_env_var",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, _ secret.SecretMeta) Decision {
				ev := m.env(t, "MD_SECRET_AS_PLAIN", "v", store.ScopeProject, m.agent.ProjectID, true)
				return m.decideItem(t, m.authz, h, MaterialKindEnvVar, materialRecordEnvVar, ev.ID, ev.Key, store.ScopeProject, m.agent.ProjectID)
			},
			wantDetail: "material record does not match its kind",
		},
		{
			name: "env_var_lookup_error",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, _ secret.SecretMeta) Decision {
				ev := m.env(t, "MD_ENV_ERR", "v", store.ScopeProject, m.agent.ProjectID, false)
				a := hdWithStore(t, m.authz, &ldFaultStore{Store: m.store, getEnvVarErr: injected})
				return m.decideItem(t, a, h, MaterialKindEnvVar, materialRecordEnvVar, ev.ID, ev.Key, store.ScopeProject, m.agent.ProjectID)
			},
			wantDetail: "delivery association check failed (fail-closed): material lookup failed",
		},
		{
			name: "item_for_another_agent",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, base secret.SecretMeta) Decision {
				res := projectRes(m, base)
				res.deliveryItem.TargetAgentID = m.agentAlpha.ID
				return decideRes(t, m, m.authz, h, res, "secret.deliver")
			},
			wantDetail: "delivery item is for a different agent",
		},
		{
			name: "parent_not_the_item_scope",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, base secret.SecretMeta) Decision {
				res := projectRes(m, base)
				res.ParentID = m.projectBeta.ID
				return decideRes(t, m, m.authz, h, res, "secret.deliver")
			},
			wantDetail: "delivery item parent does not match its scope",
		},
		{
			name: "no_delivery_item",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, base secret.SecretMeta) Decision {
				res := projectRes(m, base)
				res.deliveryItem = nil
				return decideRes(t, m, m.authz, h, res, "secret.deliver")
			},
			noCandidate: true,
		},
		{
			name: "user_scope_item",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, _ secret.SecretMeta) Decision {
				us := m.secret(t, "MD_USER_ITEM", "v", store.SecretTypeEnvironment, store.ScopeUser, m.agent.OwnerID)
				res := materialDeliveryResource(MaterialKindSecret, materialRecordSecret, us.ID, us.Name, store.ScopeUser, m.agent.OwnerID, m.agent.ID, GrantProjectAssociation, PurposeRestart)
				return decideRes(t, m, m.authz, h, res, "secret.deliver")
			},
			noCandidate: true,
		},
		{
			name: "env_var_permission_on_secret",
			run: func(t *testing.T, m *ldFixture, h *hubDeliveryIdentity, base secret.SecretMeta) Decision {
				return decideRes(t, m, m.authz, h, projectRes(m, base), "env_var.deliver")
			},
			noCandidate: true,
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			m := newLDFixture(t, "fault-"+r.name, ldOptions{})
			base := m.secret(t, "MD_BASE", "base-value", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
			h := hdIdentity(t, m.authz, m.agent.ID)

			// Control: the unchanged base item is admitted.
			ctl := decidePerm(m.authz, h, projectRes(m, base), ActionDeliver, "secret.deliver", true)
			require.True(t, ctl.Allowed, "control: reason %q", ctl.Reason)

			d := r.run(t, m, h, base)
			assert.False(t, d.Allowed, "reason %q", d.Reason)
			assertNoDeliveryCeilingStep(t, d)
			var assoc []RelationshipCandidateResult
			if d.Provenance != nil {
				for _, rr := range d.Provenance.Relationships {
					switch rr.Rule {
					case RelationshipRuleProjectAssociation, RelationshipRuleHubAssociation, RelationshipRuleBrokerAssociation:
						assoc = append(assoc, rr)
					}
				}
			}
			if r.noCandidate {
				assert.Empty(t, assoc, "no association candidate may be built")
				return
			}
			require.Len(t, assoc, 1, "relationships: %+v", d.Provenance)
			assert.False(t, assoc[0].Accepted)
			assert.Equal(t, RelationshipRejectFact, assoc[0].RejectedBy)
			assert.Equal(t, r.wantDetail, assoc[0].Detail)
		})
	}
}

// TestMaterialAssociation_OnlyDeliveryCredential: the association rows
// admit nothing for any other principal, and nothing but deliver for the
// delivery credential.
func TestMaterialAssociation_OnlyDeliveryCredential(t *testing.T) {
	m := newLDFixture(t, "only-hd", ldOptions{})
	base := m.secret(t, "MD_ONLY_HD", "v", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
	res := materialDeliveryResource(MaterialKindSecret, materialRecordSecret, base.ID, base.Name, store.ScopeProject, m.agent.ProjectID, m.agent.ID, GrantProjectAssociation, PurposeRestart)

	stored, err := m.store.GetAgent(context.Background(), m.agent.ID)
	require.NoError(t, err)
	others := map[string]Identity{
		"agent_jwt_all_scopes": progenyPairAgent(m.agent.ID, m.agent.ProjectID, m.agent.Ancestry, allRegisteredAgentScopes()),
		"stored_agent":         &storedAgentIdentity{agent: stored},
		"project_owner":        NewAuthenticatedUser(m.projectOwnerID, "proj-owner@golden.test", "Owner", "member", "web"),
		"super_admin":          NewAuthenticatedUser(m.superAdminID, "superadmin@golden.test", "Super", "admin", "web"),
	}
	for name, ident := range others {
		t.Run(name, func(t *testing.T) {
			d := decidePerm(m.authz, ident, res, ActionDeliver, "secret.deliver", false)
			assert.False(t, d.Allowed, "reason %q", d.Reason)
			assert.Equal(t, deliveryGateReason, d.Reason)
		})
	}

	h := hdIdentity(t, m.authz, m.agent.ID)
	for _, tc := range []struct {
		action Action
		perm   string
	}{
		{ActionUse, "secret.use"},
		{ActionRead, "project.secret_read"},
		{ActionRead, "secret.deliver"},
	} {
		t.Run("delivery_credential/"+string(tc.action)+"/"+tc.perm, func(t *testing.T) {
			d := decidePerm(m.authz, h, res, tc.action, tc.perm, false)
			assert.False(t, d.Allowed, "reason %q", d.Reason)
		})
	}
}

// TestMaterialAssociation_AdmissionWalkDenies: an associated item still
// needs the step-10 admission walk. Each row changes one recorded or live
// fact behind the target and is denied at step 10, with the association
// accepted.
func TestMaterialAssociation_AdmissionWalkDenies(t *testing.T) {
	rows := []struct {
		name      string
		opts      ldOptions
		after     func(t *testing.T, m *ldFixture)
		authz     func(m *ldFixture) *AuthzService
		wantCause DenyCause
	}{
		{
			name:      "edge_without_deliver_effect",
			opts:      ldOptions{edge: withBoundedCeiling("agent.read")},
			wantCause: DenyCauseCeilingEffectExceeded,
		},
		{
			// A pre-provenance edge, as a legacy writer left it.
			name: "unrecorded_provenance",
			opts: ldOptions{noEdge: true},
			after: func(t *testing.T, m *ldFixture) {
				createDCEdge(t, m.store, store.DelegationPrincipalUser, m.projectOwnerID, store.DelegationPrincipalAgent, m.agent.ID,
					store.RoleScopeProject, m.agent.ProjectID, string(AgentRoleFull))
			},
			wantCause: DenyCauseCeilingUnrecorded,
		},
		{
			name: "root_user_suspended",
			opts: ldOptions{},
			after: func(t *testing.T, m *ldFixture) {
				setUserStatus(t, m.store, m.projectOwnerID, store.UserStatusSuspended)
			},
			wantCause: DenyCauseCeilingDelegatorLacksPermission,
		},
		{
			// A hub member with no membership in alpha: catalog grants do
			// not admit the root to the project.
			name:      "root_not_admitted",
			opts:      ldOptions{rootMemberNone: true},
			wantCause: DenyCauseCeilingDelegatorLacksPermission,
		},
		{
			name: "target_held",
			opts: ldOptions{},
			after: func(t *testing.T, m *ldFixture) {
				holdAgentFor(t, m.store, m.agent.ID, m.agent.ProjectID, m.projectOwnerID)
			},
			wantCause: DenyCauseCeilingOrphaned,
		},
		{
			name: "hold_lookup_error",
			opts: ldOptions{},
			authz: func(m *ldFixture) *AuthzService {
				return NewAuthzService(&ldFaultStore{Store: m.store, holdErr: errors.New("injected hold failure")}, slog.Default())
			},
			wantCause: DenyCauseCeilingError,
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			m := newLDFixture(t, "walk-"+r.name, r.opts)
			base := m.secret(t, "MD_WALK", "v", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
			h := hdIdentity(t, m.authz, m.agent.ID)
			if r.after != nil {
				r.after(t, m)
			}
			a := m.authz
			if r.authz != nil {
				a = r.authz(m)
			}
			d := m.decideItem(t, a, h, MaterialKindSecret, materialRecordSecret, base.ID, base.Name, store.ScopeProject, m.agent.ProjectID)
			assert.False(t, d.Allowed, "reason %q", d.Reason)
			assert.Equal(t, r.wantCause, d.DenyCause, "reason %q", d.Reason)
			assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy)
			assert.True(t, relationshipResult(t, d, RelationshipRuleProjectAssociation).Accepted, "the association passed; step 10 denied")
		})
	}
}

// =============================================================================
// Start and restart selection
// =============================================================================

// ldRecordingSecretBackend wraps a backend and records the calls launch
// selection makes, in order, into a shared log.
type ldRecordingSecretBackend struct {
	secret.SecretBackend
	mu       sync.Mutex
	log      *[]string
	fetched  []secret.SecretMeta
	resolves int
	gets     int
}

func (b *ldRecordingSecretBackend) note(s string) {
	if b.log != nil {
		*b.log = append(*b.log, s)
	}
}

func (b *ldRecordingSecretBackend) FetchValues(ctx context.Context, metas []secret.SecretMeta) (map[string]secret.FetchResult, error) {
	b.mu.Lock()
	b.note("fetch")
	b.fetched = append(b.fetched, metas...)
	b.mu.Unlock()
	return b.SecretBackend.FetchValues(ctx, metas)
}

func (b *ldRecordingSecretBackend) Resolve(ctx context.Context, userID, projectID, brokerID string, opts *secret.ResolveOpts) ([]secret.SecretWithValue, error) {
	b.mu.Lock()
	b.resolves++
	b.mu.Unlock()
	return b.SecretBackend.Resolve(ctx, userID, projectID, brokerID, opts)
}

func (b *ldRecordingSecretBackend) Get(ctx context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	b.mu.Lock()
	b.gets++
	b.mu.Unlock()
	return b.SecretBackend.Get(ctx, name, scope, scopeID)
}

// ldOrderStore records, into the shared log, the target agent read that
// mints the delivery credential and every delegation edge read of the
// admission walk.
type ldOrderStore struct {
	store.Store
	target string
	log    *[]string
}

func (s *ldOrderStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if id == s.target {
		*s.log = append(*s.log, "agent")
	}
	return s.Store.GetAgent(ctx, id)
}

func (s *ldOrderStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	*s.log = append(*s.log, "edges")
	return s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
}

// TestDispatchDelivery_RestartDeliversAssociatedMaterial: a restart
// delivers project, hub and broker env vars and environment secrets, plus
// the owner's user-scope rows, through the delivery selector.
func TestDispatchDelivery_RestartDeliversAssociatedMaterial(t *testing.T) {
	m := newLDFixture(t, "restart", ldOptions{})
	for _, scope := range []string{store.ScopeProject, store.ScopeHub, store.ScopeRuntimeBroker, store.ScopeUser} {
		m.env(t, "MD_ENV_"+strings.ToUpper(scope), "env-"+scope, scope, m.scopeID(scope), false)
		m.secret(t, "MD_SECRET_"+strings.ToUpper(scope), "secret-"+scope, store.SecretTypeEnvironment, scope, m.scopeID(scope))
	}
	m.env(t, "MD_LEGACY_SECRET_ENV", "legacy-value", store.ScopeProject, m.agent.ProjectID, true)

	require.NoError(t, m.d.DispatchAgentRestart(context.Background(), m.agent))
	require.True(t, m.client.restartCalled)
	env := m.client.lastRestartResolvedEnv
	for _, scope := range []string{store.ScopeProject, store.ScopeHub, store.ScopeRuntimeBroker, store.ScopeUser} {
		assert.Equal(t, "env-"+scope, env["MD_ENV_"+strings.ToUpper(scope)], "env %s", scope)
		assert.Equal(t, "secret-"+scope, env["MD_SECRET_"+strings.ToUpper(scope)], "secret %s", scope)
	}
	assert.Equal(t, "legacy-value", env["MD_LEGACY_SECRET_ENV"])
}

// TestDispatchDelivery_UnadmittedAgentGetsOnlyUserMaterial: the same agent
// with no delegation edge (a single fault) receives none of its project,
// hub or broker material; its owner-scope rows, whose selection this change
// does not touch, still arrive.
func TestDispatchDelivery_UnadmittedAgentGetsOnlyUserMaterial(t *testing.T) {
	m := newLDFixture(t, "no-edge", ldOptions{noEdge: true})
	rec := &ldRecordingSecretBackend{SecretBackend: m.backend}
	m.d.SetSecretBackend(rec)
	var denied []string
	for _, scope := range []string{store.ScopeProject, store.ScopeHub, store.ScopeRuntimeBroker} {
		m.env(t, "MD_ENV_"+strings.ToUpper(scope), "env-"+scope, scope, m.scopeID(scope), false)
		sm := m.secret(t, "MD_SECRET_"+strings.ToUpper(scope), "secret-"+scope, store.SecretTypeEnvironment, scope, m.scopeID(scope))
		denied = append(denied, sm.ID)
	}
	m.env(t, "MD_ENV_USER", "env-user", store.ScopeUser, m.agent.OwnerID, false)
	m.secret(t, "MD_SECRET_USER", "secret-user", store.SecretTypeEnvironment, store.ScopeUser, m.agent.OwnerID)

	require.NoError(t, m.d.DispatchAgentRestart(context.Background(), m.agent))
	env := m.client.lastRestartResolvedEnv
	for _, scope := range []string{store.ScopeProject, store.ScopeHub, store.ScopeRuntimeBroker} {
		assert.NotContains(t, env, "MD_ENV_"+strings.ToUpper(scope))
		assert.NotContains(t, env, "MD_SECRET_"+strings.ToUpper(scope))
	}
	assert.Equal(t, "env-user", env["MD_ENV_USER"])
	assert.Equal(t, "secret-user", env["MD_SECRET_USER"])

	// No value of a refused item was ever read.
	for _, meta := range rec.fetched {
		assert.NotContains(t, denied, meta.ID, "refused secret %s was fetched", meta.Name)
	}
	assert.Zero(t, rec.resolves, "launch selection never calls Resolve")
	// rec.gets is not asserted: the provision-credential pass of a start
	// still reads project secrets by name for the broker's skill fetch,
	// outside launch selection and unchanged here.
}

// TestDispatchDelivery_MintBeforeFetch: during a restart, the delivery
// credential is minted from the stored agent and every associated item is
// decided before any secret value is fetched.
func TestDispatchDelivery_MintBeforeFetch(t *testing.T) {
	m := newLDFixture(t, "order", ldOptions{})
	var log []string
	m.d.SetSecretBackend(&ldRecordingSecretBackend{SecretBackend: m.backend, log: &log})
	m.secret(t, "MD_ORDER_PROJECT", "p", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
	m.secret(t, "MD_ORDER_BROKER", "b", store.SecretTypeEnvironment, store.ScopeRuntimeBroker, m.broker.ID)
	hdWithStore(t, m.authz, &ldOrderStore{Store: m.store, target: m.agent.ID, log: &log})

	_, err := m.d.selectDeliverySecrets(context.Background(), newDeliverySelector(m.authz, m.agent, PurposeRestart), m.agent)
	require.NoError(t, err)

	firstAgent, lastEdges, firstFetch := -1, -1, -1
	for i, e := range log {
		switch e {
		case "agent":
			if firstAgent < 0 {
				firstAgent = i
			}
		case "edges":
			lastEdges = i
		case "fetch":
			if firstFetch < 0 {
				firstFetch = i
			}
		}
	}
	require.GreaterOrEqual(t, firstAgent, 0, "the credential was minted from the stored agent: %v", log)
	require.GreaterOrEqual(t, lastEdges, 0, "the admission walk ran: %v", log)
	require.GreaterOrEqual(t, firstFetch, 0, "values were fetched: %v", log)
	assert.Less(t, firstAgent, firstFetch, "mint precedes fetch: %v", log)
	assert.Less(t, lastEdges, firstFetch, "every admission precedes fetch: %v", log)
}

// TestDispatchDelivery_MintFailureFailsStart: an associated item that needs
// the delivery credential fails the restart, before anything reaches the
// broker and before any value is fetched, when the credential cannot be
// minted.
func TestDispatchDelivery_MintFailureFailsStart(t *testing.T) {
	t.Run("no_authorization_service", func(t *testing.T) {
		m := newLDFixture(t, "mint-noauthz", ldOptions{})
		rec := &ldRecordingSecretBackend{SecretBackend: m.backend}
		m.d.SetSecretBackend(rec)
		m.d.SetAuthzService(nil)
		m.secret(t, "MD_MINT_PROJECT", "p", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)

		err := m.d.DispatchAgentRestart(context.Background(), m.agent)
		require.Error(t, err)
		var sre *secretResolutionError
		require.ErrorAs(t, err, &sre)
		assert.ErrorIs(t, err, errDeliveryAuthzUnavailable)
		assert.False(t, m.client.restartCalled)
		assert.Empty(t, rec.fetched)
	})
	t.Run("agent_deleted_in_store", func(t *testing.T) {
		m := newLDFixture(t, "mint-deleted", ldOptions{})
		m.env(t, "MD_MINT_ENV", "v", store.ScopeProject, m.agent.ProjectID, false)
		softDeleteStoredAgent(t, m.store, m.agent.ID)

		_, _, err := m.d.selectDeliveryEnv(context.Background(), newDeliverySelector(m.authz, m.agent, PurposeRestart), m.agent)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "agent is deleted")
	})
	t.Run("no_associated_item_needs_no_credential", func(t *testing.T) {
		m := newLDFixture(t, "mint-none", ldOptions{})
		m.d.SetAuthzService(nil)
		m.env(t, "MD_USER_ONLY", "u", store.ScopeUser, m.agent.OwnerID, false)
		sel := newDeliverySelector(nil, m.agent, PurposeRestart)
		env, _, err := m.d.selectDeliveryEnv(context.Background(), sel, m.agent)
		require.NoError(t, err)
		assert.Equal(t, "u", env["MD_USER_ONLY"])
		assert.Nil(t, sel.ident, "nothing needed the credential")
	})
}

// TestDispatchDelivery_DeniedHigherScopeDoesNotMaskLower: precedence runs
// after authorization, so a refused project item leaves an admitted broker
// item for the same key in place.
func TestDispatchDelivery_DeniedHigherScopeDoesNotMaskLower(t *testing.T) {
	m := newLDFixture(t, "mask", ldOptions{})
	// The project row is admitted only through a matching record; a fault
	// store refuses the project scope by reporting its record missing.
	m.env(t, "MD_SHARED_KEY", "from-broker", store.ScopeRuntimeBroker, m.broker.ID, false)
	m.env(t, "MD_SHARED_KEY", "from-project", store.ScopeProject, m.agent.ProjectID, false)

	sel := newDeliverySelector(m.authz, m.agent, PurposeRestart)
	env, _, err := m.d.selectDeliveryEnv(context.Background(), sel, m.agent)
	require.NoError(t, err)
	assert.Equal(t, "from-project", env["MD_SHARED_KEY"], "control: project outranks broker")

	hdWithStore(t, m.authz, &ldProjectEnvHiddenStore{Store: m.store})
	sel = newDeliverySelector(m.authz, m.agent, PurposeRestart)
	env, _, err = m.d.selectDeliveryEnv(context.Background(), sel, m.agent)
	require.NoError(t, err)
	assert.Equal(t, "from-broker", env["MD_SHARED_KEY"], "the refused project row does not mask the broker row")
}

// ldProjectEnvHiddenStore answers not-found for project-scope env var reads
// by key (the association's record check) only.
type ldProjectEnvHiddenStore struct{ store.Store }

func (s *ldProjectEnvHiddenStore) GetEnvVar(ctx context.Context, key, scope, scopeID string) (*store.EnvVar, error) {
	if scope == store.ScopeProject {
		return nil, store.ErrNotFound
	}
	return s.Store.GetEnvVar(ctx, key, scope, scopeID)
}

// TestDispatchDelivery_PreservedFilters: the filters launch selection kept
// from the earlier resolution still apply to admitted items: reserved
// targets are dropped, as_needed environment secrets and env rows are left
// for the second pass, file secrets are placed whatever their mode, and an
// unset hub ID delivers no hub item.
func TestDispatchDelivery_PreservedFilters(t *testing.T) {
	m := newLDFixture(t, "filters", ldOptions{})
	ctx := context.Background()
	m.secret(t, "SCION_AGENT_ID", "spoofed", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
	_, _, err := m.backend.Set(ctx, &secret.SetSecretInput{
		Name: "MD_AS_NEEDED", Value: "later", SecretType: store.SecretTypeEnvironment, Target: "MD_AS_NEEDED",
		Scope: store.ScopeProject, ScopeID: m.agent.ProjectID, InjectionMode: store.InjectionModeAsNeeded,
	})
	require.NoError(t, err)
	_, _, err = m.backend.Set(ctx, &secret.SetSecretInput{
		Name: "md-file", Value: "file-body", SecretType: store.SecretTypeFile, Target: "/tmp/md-file",
		Scope: store.ScopeProject, ScopeID: m.agent.ProjectID, InjectionMode: store.InjectionModeAsNeeded,
	})
	require.NoError(t, err)
	require.NoError(t, m.store.CreateEnvVar(ctx, &store.EnvVar{
		ID: api.NewUUID(), Key: "MD_ENV_AS_NEEDED", Value: "later", Scope: store.ScopeProject,
		ScopeID: m.agent.ProjectID, InjectionMode: store.InjectionModeAsNeeded,
	}))

	secrets, err := m.d.selectDeliverySecrets(ctx, newDeliverySelector(m.authz, m.agent, PurposeDispatch), m.agent)
	require.NoError(t, err)
	byName := map[string]ResolvedSecret{}
	for _, s := range secrets {
		byName[s.Name] = s
	}
	assert.NotContains(t, byName, "SCION_AGENT_ID", "reserved target dropped")
	assert.NotContains(t, byName, "MD_AS_NEEDED", "as_needed environment secret left for the second pass")
	require.Contains(t, byName, "md-file", "file secret placed whatever its mode")
	assert.Equal(t, "file-body", byName["md-file"].Value)
	assert.Equal(t, store.ScopeProject, byName["md-file"].Source)

	env, _, err := m.d.selectDeliveryEnv(ctx, newDeliverySelector(m.authz, m.agent, PurposeDispatch), m.agent)
	require.NoError(t, err)
	assert.NotContains(t, env, "MD_ENV_AS_NEEDED")

	t.Run("hub_id_unset", func(t *testing.T) {
		m := newLDFixture(t, "filters-nohub", ldOptions{})
		m.env(t, "MD_HUB_ENV", "h", store.ScopeHub, m.hubID, false)
		m.d.SetHubID("")
		env, _, err := m.d.selectDeliveryEnv(context.Background(), newDeliverySelector(m.authz, m.agent, PurposeRestart), m.agent)
		require.NoError(t, err)
		assert.NotContains(t, env, "MD_HUB_ENV")
	})
}

// TestDispatchDelivery_SelectionRereadsEveryStart: nothing is cached
// between starts. An item admitted at one restart is refused at the next
// once the agent is held.
func TestDispatchDelivery_SelectionRereadsEveryStart(t *testing.T) {
	m := newLDFixture(t, "reread", ldOptions{})
	m.env(t, "MD_REREAD", "v", store.ScopeProject, m.agent.ProjectID, false)
	require.NoError(t, m.d.DispatchAgentRestart(context.Background(), m.agent))
	assert.Equal(t, "v", m.client.lastRestartResolvedEnv["MD_REREAD"])

	holdAgentFor(t, m.store, m.agent.ID, m.agent.ProjectID, m.projectOwnerID)
	env, _, err := m.d.selectDeliveryEnv(context.Background(), newDeliverySelector(m.authz, m.agent, PurposeRestart), m.agent)
	require.NoError(t, err)
	assert.NotContains(t, env, "MD_REREAD", "a held agent receives no associated item")
}

// =============================================================================
// Availability probe
// =============================================================================

// TestDeliveryProbe_AuthorizesStoredAgent: once the agent's record exists,
// the auth credential probe counts a project or hub record only if it would
// be delivered; before (the create-time probe) it counts by existence.
func TestDeliveryProbe_AuthorizesStoredAgent(t *testing.T) {
	probeServer := func(m *ldFixture) *Server {
		return &Server{store: m.store, authzService: m.authz, hubID: m.hubID}
	}

	t.Run("admitted_project_and_hub", func(t *testing.T) {
		m := newLDFixture(t, "probe-ok", ldOptions{})
		m.env(t, "MD_PROBE_PROJECT", "p", store.ScopeProject, m.agent.ProjectID, false)
		m.secret(t, "MD_PROBE_HUB", "h", store.SecretTypeEnvironment, store.ScopeHub, m.hubID)
		s := probeServer(m)
		for _, key := range []string{"MD_PROBE_PROJECT", "MD_PROBE_HUB"} {
			found, err := s.hasAnyKey(context.Background(), m.agent, []string{key})
			require.NoError(t, err)
			assert.True(t, found, key)
		}
	})
	t.Run("stored_agent_not_admitted", func(t *testing.T) {
		m := newLDFixture(t, "probe-noedge", ldOptions{noEdge: true})
		m.env(t, "MD_PROBE_PROJECT", "p", store.ScopeProject, m.agent.ProjectID, false)
		m.secret(t, "MD_PROBE_HUB", "h", store.SecretTypeEnvironment, store.ScopeHub, m.hubID)
		s := probeServer(m)
		found, err := s.hasAnyKey(context.Background(), m.agent, []string{"MD_PROBE_PROJECT", "MD_PROBE_HUB"})
		require.NoError(t, err)
		assert.False(t, found, "a record that would not be delivered does not count")
	})
	t.Run("create_time_probe_counts_existence", func(t *testing.T) {
		m := newLDFixture(t, "probe-create", ldOptions{noEdge: true})
		m.env(t, "MD_PROBE_PROJECT", "p", store.ScopeProject, m.agent.ProjectID, false)
		unstored := *m.agent
		unstored.ID = tid("md-probe-create-unstored")
		found, err := probeServer(m).hasAnyKey(context.Background(), &unstored, []string{"MD_PROBE_PROJECT"})
		require.NoError(t, err)
		assert.True(t, found)
	})
	t.Run("never_fetches", func(t *testing.T) {
		m := newLDFixture(t, "probe-nofetch", ldOptions{})
		rec := &ldRecordingSecretBackend{SecretBackend: m.backend}
		m.secret(t, "MD_PROBE_SECRET", "p", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
		s := probeServer(m)
		s.secretBackend = rec
		found, err := s.hasAnyKey(context.Background(), m.agent, []string{"MD_PROBE_SECRET"})
		require.NoError(t, err)
		assert.True(t, found)
		assert.Empty(t, rec.fetched)
		assert.Zero(t, rec.gets)
		assert.Zero(t, rec.resolves)
	})
}

// =============================================================================
// Exposure of the step-10 trace (review carry-forward G6)
// =============================================================================

// TestDeliveryAuthzRequest_NeverExplains pins that launch selection never
// asks Decide for an explanation: the hub_delivery step-10 trace, which
// names edge, delegator and root user IDs, is produced only when Explain is
// set, so it never exists on the launch path. A launch decision's trace is
// empty.
func TestDeliveryAuthzRequest_NeverExplains(t *testing.T) {
	m := newLDFixture(t, "explain", ldOptions{})
	base := m.secret(t, "MD_EXPLAIN", "v", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
	h := hdIdentity(t, m.authz, m.agent.ID)
	res := materialDeliveryResource(MaterialKindSecret, materialRecordSecret, base.ID, base.Name, store.ScopeProject, m.agent.ProjectID, m.agent.ID, GrantProjectAssociation, PurposeRestart)
	req := deliveryAuthzRequest(h, MaterialKindSecret, res)
	assert.False(t, req.Explain)
	assert.Equal(t, "secret.deliver", req.Permission)
	assert.Equal(t, ActionDeliver, req.Action)
	assert.Equal(t, CredentialKindHubDelivery, req.Credential.Kind)

	d := m.authz.Decide(context.Background(), req)
	require.True(t, d.Allowed, d.Reason)
	assert.Empty(t, d.ExplainTrace)

	// A refused launch decision has no trace either; its reason, which may
	// name a chain member, is not returned by the selector.
	holdAgentFor(t, m.store, m.agent.ID, m.agent.ProjectID, m.projectOwnerID)
	d = m.authz.Decide(context.Background(), req)
	require.False(t, d.Allowed)
	assert.Empty(t, d.ExplainTrace)
	sel := newDeliverySelector(m.authz, m.agent, PurposeRestart)
	ok, err := sel.authorize(context.Background(), MaterialKindSecret, materialRecordSecret, base.ID, base.Name, store.ScopeProject, m.agent.ProjectID)
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestMaterialDeliveryResource_Shape pins the typed resource a launch
// decision is made on.
func TestMaterialDeliveryResource_Shape(t *testing.T) {
	res := materialDeliveryResource(MaterialKindEnvVar, materialRecordEnvVar, "id-1", "KEY", store.ScopeRuntimeBroker, "broker-1", "agent-1", GrantBrokerAssociation, PurposeDispatch)
	assert.Equal(t, "env_var", res.Type)
	assert.Equal(t, "id-1", res.ID)
	assert.Equal(t, "broker", res.ParentType)
	assert.Equal(t, "broker-1", res.ParentID)
	assert.Equal(t, map[string]string{"delivery_target": "agent-1", "grant": "broker_association", "purpose": "dispatch"}, res.Labels)
	require.NotNil(t, res.deliveryItem)
	assert.Equal(t, materialDeliveryItem{Record: materialRecordEnvVar, Key: "KEY", Scope: store.ScopeRuntimeBroker, ScopeID: "broker-1", TargetAgentID: "agent-1"}, *res.deliveryItem)

	assert.Equal(t, "secret.deliver", deliverPermissionForKind(MaterialKindSecret))
	assert.Equal(t, "env_var.deliver", deliverPermissionForKind(MaterialKindEnvVar))
	assert.Empty(t, deliverPermissionForKind(MaterialKind("skill_injection")))
	for scope, want := range map[string]GrantKind{
		store.ScopeProject: GrantProjectAssociation, store.ScopeHub: GrantHubAssociation, store.ScopeRuntimeBroker: GrantBrokerAssociation,
	} {
		got, ok := grantForDeliveryScope(scope)
		assert.True(t, ok, scope)
		assert.Equal(t, want, got, scope)
	}
	_, ok := grantForDeliveryScope(store.ScopeUser)
	assert.False(t, ok, "user scope has no association grant")
	assert.Equal(t, MaterialKindSecret, envVarDeliveryKind(store.EnvVar{Secret: true}))
	assert.Equal(t, MaterialKindEnvVar, envVarDeliveryKind(store.EnvVar{}))
}
