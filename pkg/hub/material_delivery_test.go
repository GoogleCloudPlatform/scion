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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// ancestry, when set, gives the agent's stored ancestry instead of
	// [root]. The store writes ancestry only at create (UpdateAgent leaves
	// it unchanged), so a fixture needing another ancestry must set it here.
	ancestry func(f *goldenFixture) []string
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
	if opts.ancestry != nil {
		m.agent.Ancestry = opts.ancestry(f)
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
// with a recorded ceiling that lacks the deliver permissions (a single
// fault, and not a legacy shape, so the legacy fallback does not apply)
// receives none of its project, hub or broker material; its owner-scope
// rows, whose selection this change does not touch, still arrive.
func TestDispatchDelivery_UnadmittedAgentGetsOnlyUserMaterial(t *testing.T) {
	m := newLDFixture(t, "bounded-no-deliver", ldOptions{edge: withBoundedCeiling("agent.read")})
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
	assert.Zero(t, rec.gets, "no value is read by name, including for provision credentials")
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
		m := newLDFixture(t, "probe-bounded", ldOptions{edge: withBoundedCeiling("agent.read")})
		m.env(t, "MD_PROBE_PROJECT", "p", store.ScopeProject, m.agent.ProjectID, false)
		m.secret(t, "MD_PROBE_HUB", "h", store.SecretTypeEnvironment, store.ScopeHub, m.hubID)
		s := probeServer(m)
		found, err := s.hasAnyKey(context.Background(), m.agent, []string{"MD_PROBE_PROJECT", "MD_PROBE_HUB"})
		require.NoError(t, err)
		assert.False(t, found, "a record that would not be delivered does not count")
	})
	t.Run("legacy_agent_counts_what_a_start_delivers", func(t *testing.T) {
		// A legacy agent (no edge) is served by the legacy fallback at start,
		// so the probe agrees and counts the record.
		m := newLDFixture(t, "probe-noedge", ldOptions{noEdge: true})
		m.env(t, "MD_PROBE_PROJECT", "p", store.ScopeProject, m.agent.ProjectID, false)
		found, err := probeServer(m).hasAnyKey(context.Background(), m.agent, []string{"MD_PROBE_PROJECT"})
		require.NoError(t, err)
		assert.True(t, found)
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
// Exposure of the step-10 trace
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

// =============================================================================
// Secret reference format and selection cost
// =============================================================================

// TestDispatchDelivery_GCPSecretRefMatchesResolve: with the GCP Secret
// Manager backend, start and restart selection hands the broker the same
// secrets, field for field, that the Resolve path (still used at create)
// does. In particular Ref is the bare Secret Manager resource path, which
// a Kubernetes broker mounts as <ref>/versions/latest, for a record with a
// stored gcpsm: ref, with no stored ref (legacy), and with a stored ref that
// lacks the gcpsm: prefix; the last two resolve by the computed name.
func TestDispatchDelivery_GCPSecretRefMatchesResolve(t *testing.T) {
	m := newLDFixture(t, "gcp-ref", ldOptions{})
	ctx := context.Background()
	const smProject = "md-gcp-project"
	smPrefix := "projects/" + smProject + "/secrets/"
	gcp := secret.NewGCPBackendWithClient(m.store, newCopyForwardMockSMClient(), smProject, m.hubID)
	m.d.SetSecretBackend(gcp)

	// set stores a file secret through the GCP backend and returns its
	// record as written, with the gcpsm: ref Set records.
	set := func(name, scope string) *store.Secret {
		_, meta, err := gcp.Set(ctx, &secret.SetSecretInput{
			Name: name, Value: "v-" + name, SecretType: store.SecretTypeFile, Target: "/etc/md/" + strings.ToLower(name),
			Scope: scope, ScopeID: m.scopeID(scope), InjectionMode: store.InjectionModeAlways, CreatedBy: m.projectOwnerID,
		})
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(meta.SecretRef, "gcpsm:"+smPrefix), "stored ref %q", meta.SecretRef)
		rec, err := m.store.GetSecret(ctx, name, scope, m.scopeID(scope))
		require.NoError(t, err)
		return rec
	}
	repoint := func(rec *store.Secret, ref string) {
		applied, err := m.store.UpdateSecretRefIfMatches(ctx, rec.Key, rec.Scope, rec.ScopeID, rec.SecretRef, rec.Version, ref)
		require.NoError(t, err)
		require.True(t, applied)
	}
	project := set("MD_GCP_PREFIXED", store.ScopeProject)
	hub := set("MD_GCP_LEGACY_EMPTY", store.ScopeHub)
	repoint(hub, "")
	broker := set("MD_GCP_UNPREFIXED", store.ScopeRuntimeBroker)
	repoint(broker, smPrefix+"unprefixed-ref")
	user := set("MD_GCP_USER", store.ScopeUser)

	// Set places each value under the computed (hub-prefixed) name, so the
	// expected Ref is the original stored ref without its prefix in every
	// case: directly for a gcpsm: ref, by the computed name otherwise.
	want := map[string]string{}
	for _, rec := range []*store.Secret{project, hub, broker, user} {
		want[rec.Key] = strings.TrimPrefix(rec.SecretRef, "gcpsm:")
	}

	got, err := m.d.selectDeliverySecrets(ctx, newDeliverySelector(m.authz, m.agent, PurposeRestart), m.agent)
	require.NoError(t, err)
	old, _, err := m.d.resolveSecrets(ctx, m.agent)
	require.NoError(t, err)
	require.Len(t, got, len(want))
	assert.ElementsMatch(t, old, got, "start/restart selection delivers what Resolve delivered: Name, Type, Target, Value, Source and Ref")
	for _, s := range got {
		assert.Equal(t, want[s.Name], s.Ref, "ref of %s", s.Name)
		assert.False(t, strings.HasPrefix(s.Ref, "gcpsm:"), "ref of %s is the stored form %q", s.Name, s.Ref)
		assert.Equal(t, store.SecretTypeFile, s.Type, s.Name)
		assert.Equal(t, "v-"+s.Name, s.Value, s.Name)
	}

	// End to end: the restart request to the broker carries the same refs.
	require.NoError(t, m.d.DispatchAgentRestart(ctx, m.agent))
	require.True(t, m.client.restartCalled)
	sent := map[string]string{}
	for _, s := range m.client.lastRestartExtras.ResolvedSecrets {
		sent[s.Name] = s.Ref
	}
	for name, ref := range want {
		assert.Equal(t, ref, sent[name], "restart ref of %s", name)
	}
}

// TestDeliverySelector_AdmissionWalkOncePerPermission: within one
// selection, the step-10 admission walk runs once per permission and its
// outcome is shared by every item decided under that permission; a
// decision with Explain set always runs the walk, so its trace is whole.
func TestDeliverySelector_AdmissionWalkOncePerPermission(t *testing.T) {
	m := newLDFixture(t, "memo", ldOptions{})
	ctx := context.Background()
	var metas []secret.SecretMeta
	for _, scope := range []string{store.ScopeProject, store.ScopeHub, store.ScopeRuntimeBroker} {
		metas = append(metas, m.secret(t, "MD_MEMO_"+strings.ToUpper(scope), "v", store.SecretTypeEnvironment, scope, m.scopeID(scope)))
	}
	ev := m.env(t, "MD_MEMO_ENV", "e", store.ScopeProject, m.agent.ProjectID, false)

	var log []string
	hdWithStore(t, m.authz, &ldOrderStore{Store: m.store, target: m.agent.ID, log: &log})
	edgeReads := func(run func()) int {
		log = log[:0]
		run()
		n := 0
		for _, e := range log {
			if e == "edges" {
				n++
			}
		}
		return n
	}
	resFor := func(sm secret.SecretMeta) Resource {
		grant, ok := grantForDeliveryScope(sm.Scope)
		require.True(t, ok)
		return materialDeliveryResource(MaterialKindSecret, materialRecordSecret, sm.ID, sm.Name, sm.Scope, m.scopeID(sm.Scope), m.agent.ID, grant, PurposeRestart)
	}

	// Control: a credential without a memo walks for every item.
	plain := edgeReads(func() {
		h := hdIdentity(t, m.authz, m.agent.ID)
		require.Nil(t, h.admission)
		for _, sm := range metas {
			d := decidePerm(m.authz, h, resFor(sm), ActionDeliver, "secret.deliver", false)
			require.True(t, d.Allowed, "%s: %s", sm.Name, d.Reason)
		}
	})

	sel := newDeliverySelector(m.authz, m.agent, PurposeRestart)
	memo := edgeReads(func() {
		for _, sm := range metas {
			ok, err := sel.authorize(ctx, MaterialKindSecret, materialRecordSecret, sm.ID, sm.Name, sm.Scope, m.scopeID(sm.Scope))
			require.NoError(t, err)
			require.True(t, ok, sm.Name)
		}
	})
	require.Greater(t, plain, 0, "the admission walk reads delegation edges")
	assert.Less(t, memo, plain, "the selection walked once, not once per item")

	require.NotNil(t, sel.ident)
	require.NotNil(t, sel.ident.admission, "a selection's credential carries a memo")
	o, ok := sel.ident.admission.load("secret.deliver")
	require.True(t, ok)
	assert.True(t, o.ok)
	_, ok = sel.ident.admission.load("env_var.deliver")
	assert.False(t, ok, "each permission has its own walk")
	allowed, err := sel.authorize(ctx, MaterialKindEnvVar, materialRecordEnvVar, ev.ID, ev.Key, store.ScopeProject, m.agent.ProjectID)
	require.NoError(t, err)
	assert.True(t, allowed)
	_, ok = sel.ident.admission.load("env_var.deliver")
	assert.True(t, ok)

	// Explain bypasses the memo: the trace still carries the walk's steps.
	d := decidePerm(m.authz, sel.ident, resFor(metas[0]), ActionDeliver, "secret.deliver", true)
	require.True(t, d.Allowed, d.Reason)
	assert.True(t, hasDecisionStep(d.ExplainTrace, "delegation_ceiling_delivery_root"), "trace: %+v", d.ExplainTrace)
}

// TestDispatchDelivery_TestAdminWithheldScopeNotFetched: for an agent
// rooted at an admin-kind test identity, launch selection reads no value
// for a scope the test-admin restriction withholds. Here the agent's owner
// is not its root, so its owner-scope secret is withheld: never fetched
// and never delivered. The control (owner is the root) is fetched and
// delivered.
func TestDispatchDelivery_TestAdminWithheldScopeNotFetched(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "md-ta-iss", store.SystemRoleSuperAdmin)
	root := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin).Identity.ID
	projectID := tid("md-ta-project")
	taProject(t, s, projectID, root)

	const hubID = "md-ta-hub"
	backend := secret.NewLocalBackend(s, hubID, "test-secret")
	rec := &ldRecordingSecretBackend{SecretBackend: backend}
	d := NewHTTPAgentDispatcherWithClient(s, &mockRuntimeBrokerClient{}, false, slog.Default())
	d.SetHubID(hubID)
	d.SetAuthzService(srv.authzService)
	d.SetSecretBackend(rec)

	// Owner-scope secrets need no delivery decision, so the agent need not
	// be stored and no credential is minted.
	agentOwnedBy := func(owner string) *store.Agent {
		id := generateID()
		return &store.Agent{ID: id, Slug: "md-ta-" + id[:8], Name: "md-ta-" + id[:8], ProjectID: projectID,
			OwnerID: owner, CreatedBy: root, Ancestry: []string{root}, AppliedConfig: &store.AgentAppliedConfig{}}
	}
	userSecret := func(owner, name string) secret.SecretMeta {
		_, meta, err := backend.Set(ctx, &secret.SetSecretInput{
			Name: name, Value: "v-" + name, SecretType: store.SecretTypeEnvironment, Target: name,
			Scope: store.ScopeUser, ScopeID: owner, InjectionMode: store.InjectionModeAlways, CreatedBy: owner,
		})
		require.NoError(t, err)
		return *meta
	}
	own := userSecret(root, "MD_TA_ROOT_OWNED")
	other := userSecret(issuer.ID, "MD_TA_ISSUER_OWNED")

	got, err := d.selectDeliverySecrets(ctx, newDeliverySelector(srv.authzService, agentOwnedBy(root), PurposeRestart), agentOwnedBy(root))
	require.NoError(t, err)
	require.Len(t, got, 1, "control: the root's own user scope is allowed")
	assert.Equal(t, "v-MD_TA_ROOT_OWNED", got[0].Value)
	require.Len(t, rec.fetched, 1)
	assert.Equal(t, own.ID, rec.fetched[0].ID)

	rec.fetched = nil
	withheldAgent := agentOwnedBy(issuer.ID)
	got, err = d.selectDeliverySecrets(ctx, newDeliverySelector(srv.authzService, withheldAgent, PurposeRestart), withheldAgent)
	require.NoError(t, err)
	assert.Empty(t, got, "a withheld scope delivers nothing")
	for _, meta := range rec.fetched {
		assert.NotEqual(t, other.ID, meta.ID, "a withheld secret's value was fetched")
	}
}

// =============================================================================
// Defensive branches of launch selection
// =============================================================================

// ldListFaultStore injects single store faults for launch selection's
// list reads and the probe's stored-agent lookup.
type ldListFaultStore struct {
	store.Store
	listEnvErr            error
	listProgenyEnvErr     error
	listProgenySecretsErr error
	getAgentErr           error
}

func (s *ldListFaultStore) ListEnvVars(ctx context.Context, filter store.EnvVarFilter) ([]store.EnvVar, error) {
	if s.listEnvErr != nil {
		return nil, s.listEnvErr
	}
	return s.Store.ListEnvVars(ctx, filter)
}

func (s *ldListFaultStore) ListProgenyEnvVars(ctx context.Context, ancestorIDs []string) ([]store.EnvVar, error) {
	if s.listProgenyEnvErr != nil {
		return nil, s.listProgenyEnvErr
	}
	return s.Store.ListProgenyEnvVars(ctx, ancestorIDs)
}

func (s *ldListFaultStore) ListProgenySecrets(ctx context.Context, ancestorIDs []string) ([]store.Secret, error) {
	if s.listProgenySecretsErr != nil {
		return nil, s.listProgenySecretsErr
	}
	return s.Store.ListProgenySecrets(ctx, ancestorIDs)
}

func (s *ldListFaultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.getAgentErr != nil {
		return nil, s.getAgentErr
	}
	return s.Store.GetAgent(ctx, id)
}

// ldFaultSecretBackend injects single secret backend faults: a list error,
// a whole-call fetch error, one item's fetch error (itemErr, or a generic
// error when itemErr is nil), or one item missing from the fetch result.
// It records the metadata it was asked to fetch.
type ldFaultSecretBackend struct {
	secret.SecretBackend
	listErr   error
	fetchErr  error
	itemErrID string
	itemErr   error
	dropID    string
	fetched   []secret.SecretMeta
}

func (b *ldFaultSecretBackend) List(ctx context.Context, filter secret.Filter) ([]secret.SecretMeta, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	return b.SecretBackend.List(ctx, filter)
}

func (b *ldFaultSecretBackend) FetchValues(ctx context.Context, metas []secret.SecretMeta) (map[string]secret.FetchResult, error) {
	b.fetched = append(b.fetched, metas...)
	if b.fetchErr != nil {
		return nil, b.fetchErr
	}
	out, err := b.SecretBackend.FetchValues(ctx, metas)
	if err != nil {
		return nil, err
	}
	if b.itemErrID != "" {
		itemErr := b.itemErr
		if itemErr == nil {
			itemErr = errors.New("injected item fetch failure")
		}
		out[b.itemErrID] = secret.FetchResult{Err: itemErr}
	}
	if b.dropID != "" {
		delete(out, b.dropID)
	}
	return out, nil
}

// TestDeliverySelection_DefensiveBranches: each row injects exactly one
// fault into launch selection and checks it fails closed: a read error is
// the selection's error, a missing item, one gone since listing or one
// Secret Manager cannot return is not delivered, any other failed item is
// the selection's error, an
// internal secret is neither delivered nor fetched, and the credential is
// not minted for a missing target, a changed project or a user-scope item.
func TestDeliverySelection_DefensiveBranches(t *testing.T) {
	ctx := context.Background()
	injected := errors.New("injected fault")
	restartSel := func(m *ldFixture, a *store.Agent) *deliverySelector {
		return newDeliverySelector(m.authz, a, PurposeRestart)
	}
	withProgenyAncestry := func(m *ldFixture) *store.Agent {
		a := *m.agent
		a.Ancestry = []string{m.agent.OwnerID, tid("md-df-parent-agent")}
		return &a
	}

	t.Run("env_list_error", func(t *testing.T) {
		m := newLDFixture(t, "df-envlist", ldOptions{})
		m.d.store = &ldListFaultStore{Store: m.store, listEnvErr: injected}
		_, _, err := m.d.selectDeliveryEnv(ctx, restartSel(m, m.agent), m.agent)
		assert.ErrorIs(t, err, injected)
	})
	t.Run("progeny_env_list_error", func(t *testing.T) {
		m := newLDFixture(t, "df-progenyenv", ldOptions{})
		m.d.store = &ldListFaultStore{Store: m.store, listProgenyEnvErr: injected}
		a := withProgenyAncestry(m)
		_, _, err := m.d.selectDeliveryEnv(ctx, restartSel(m, a), a)
		assert.ErrorIs(t, err, injected)
	})
	t.Run("nil_agent_selects_nothing", func(t *testing.T) {
		m := newLDFixture(t, "df-nilagent", ldOptions{})
		env, plain, err := m.d.selectDeliveryEnv(ctx, restartSel(m, nil), nil)
		require.NoError(t, err)
		assert.Empty(t, env)
		assert.Empty(t, plain)
		secrets, err := m.d.selectDeliverySecrets(ctx, restartSel(m, nil), nil)
		require.NoError(t, err)
		assert.Empty(t, secrets)
	})
	t.Run("no_secret_backend_selects_nothing", func(t *testing.T) {
		m := newLDFixture(t, "df-nobackend", ldOptions{})
		m.secret(t, "MD_DF_NOBACKEND", "v", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
		m.d.secretBackend = nil
		secrets, err := m.d.selectDeliverySecrets(ctx, restartSel(m, m.agent), m.agent)
		require.NoError(t, err)
		assert.Empty(t, secrets)
	})
	t.Run("secret_list_error", func(t *testing.T) {
		m := newLDFixture(t, "df-secretlist", ldOptions{})
		m.d.SetSecretBackend(&ldFaultSecretBackend{SecretBackend: m.backend, listErr: injected})
		_, err := m.d.selectDeliverySecrets(ctx, restartSel(m, m.agent), m.agent)
		assert.ErrorIs(t, err, injected)
	})
	t.Run("progeny_secret_list_error", func(t *testing.T) {
		m := newLDFixture(t, "df-progenysecret", ldOptions{})
		m.d.store = &ldListFaultStore{Store: m.store, listProgenySecretsErr: injected}
		a := withProgenyAncestry(m)
		_, err := m.d.selectDeliverySecrets(ctx, restartSel(m, a), a)
		assert.ErrorIs(t, err, injected)
	})
	t.Run("fetch_error_fails_selection", func(t *testing.T) {
		m := newLDFixture(t, "df-fetch", ldOptions{})
		sm := m.secret(t, "MD_DF_FETCH", "v", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
		fb := &ldFaultSecretBackend{SecretBackend: m.backend, fetchErr: injected}
		m.d.SetSecretBackend(fb)
		_, err := m.d.selectDeliverySecrets(ctx, restartSel(m, m.agent), m.agent)
		assert.ErrorIs(t, err, injected)
		var ids []string
		for _, meta := range fb.fetched {
			ids = append(ids, meta.ID)
		}
		assert.Contains(t, ids, sm.ID, "control: the admitted item reached the fetch")
	})
	for _, tc := range []struct {
		name    string
		itemErr error
	}{
		{"gone_item_not_delivered", store.ErrNotFound},
		{"secret_manager_access_item_not_delivered", fmt.Errorf("%w: %w", secret.ErrValueAccess, injected)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newLDFixture(t, "df-"+tc.name[:8], ldOptions{})
			failed := m.secret(t, "MD_DF_ITEM_FAILED", "f", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
			missing := m.secret(t, "MD_DF_ITEM_MISSING", "m", store.SecretTypeEnvironment, store.ScopeHub, m.hubID)
			m.secret(t, "MD_DF_ITEM_OK", "ok", store.SecretTypeEnvironment, store.ScopeRuntimeBroker, m.broker.ID)
			m.d.SetSecretBackend(&ldFaultSecretBackend{SecretBackend: m.backend, itemErrID: failed.ID, itemErr: tc.itemErr, dropID: missing.ID})
			secrets, err := m.d.selectDeliverySecrets(ctx, restartSel(m, m.agent), m.agent)
			require.NoError(t, err)
			require.Len(t, secrets, 1)
			assert.Equal(t, "MD_DF_ITEM_OK", secrets[0].Name)
			assert.Equal(t, "ok", secrets[0].Value)
		})
	}
	t.Run("failed_item_fails_selection", func(t *testing.T) {
		m := newLDFixture(t, "df-itemfail", ldOptions{})
		failed := m.secret(t, "MD_DF_ITEM_FAILED", "f", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
		m.secret(t, "MD_DF_ITEM_OK", "ok", store.SecretTypeEnvironment, store.ScopeRuntimeBroker, m.broker.ID)
		m.d.SetSecretBackend(&ldFaultSecretBackend{SecretBackend: m.backend, itemErrID: failed.ID, itemErr: injected})
		secrets, err := m.d.selectDeliverySecrets(ctx, restartSel(m, m.agent), m.agent)
		assert.ErrorIs(t, err, injected)
		assert.NotErrorIs(t, err, secret.ErrResolveDecrypt)
		assert.Nil(t, secrets)
	})
	t.Run("internal_secret_neither_delivered_nor_fetched", func(t *testing.T) {
		m := newLDFixture(t, "df-internal", ldOptions{})
		internalID := api.NewUUID()
		require.NoError(t, m.store.CreateSecret(ctx, &store.Secret{
			ID: internalID, Key: "MD_DF_INTERNAL", EncryptedValue: "internal-value", SecretType: store.SecretTypeInternal,
			Target: "MD_DF_INTERNAL", Scope: store.ScopeProject, ScopeID: m.agent.ProjectID, InjectionMode: store.InjectionModeAlways,
		}))
		m.secret(t, "MD_DF_PLAIN", "p", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
		fb := &ldFaultSecretBackend{SecretBackend: m.backend}
		m.d.SetSecretBackend(fb)
		secrets, err := m.d.selectDeliverySecrets(ctx, restartSel(m, m.agent), m.agent)
		require.NoError(t, err)
		require.Len(t, secrets, 1, "control: the plain project secret is delivered")
		assert.Equal(t, "MD_DF_PLAIN", secrets[0].Name)
		for _, meta := range fb.fetched {
			assert.NotEqual(t, internalID, meta.ID, "an internal secret was fetched")
		}
	})
	t.Run("mint_refused_for_changed_project", func(t *testing.T) {
		m := newLDFixture(t, "df-project", ldOptions{})
		moved := *m.agent
		moved.ProjectID = tid("md-df-other-project")
		sel := restartSel(m, &moved)
		ok, err := sel.authorize(ctx, MaterialKindEnvVar, materialRecordEnvVar, "id", "MD_DF_KEY", store.ScopeProject, moved.ProjectID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "project changed")
		assert.False(t, ok)
		assert.Nil(t, sel.ident)
	})
	t.Run("mint_refused_without_target", func(t *testing.T) {
		m := newLDFixture(t, "df-notarget", ldOptions{})
		require.Error(t, restartSel(m, nil).mint(ctx))
		noID := *m.agent
		noID.ID = ""
		require.Error(t, restartSel(m, &noID).mint(ctx))
	})
	t.Run("user_scope_item_needs_no_credential", func(t *testing.T) {
		m := newLDFixture(t, "df-userscope", ldOptions{})
		sel := restartSel(m, m.agent)
		ok, err := sel.authorize(ctx, MaterialKindEnvVar, materialRecordEnvVar, "id", "MD_DF_KEY", store.ScopeUser, m.agent.OwnerID)
		require.NoError(t, err)
		assert.False(t, ok, "user scope has no association grant")
		assert.Nil(t, sel.ident, "nothing was minted")
	})
	t.Run("probe_selector_lookup_error", func(t *testing.T) {
		m := newLDFixture(t, "df-probe", ldOptions{})
		srv := &Server{store: &ldListFaultStore{Store: m.store, getAgentErr: injected}, authzService: m.authz, hubID: m.hubID}
		sel, err := srv.newProbeDeliverySelector(ctx, m.agent)
		assert.ErrorIs(t, err, injected)
		assert.Nil(t, sel)

		srv = &Server{store: &ldListFaultStore{Store: m.store, getAgentErr: store.ErrNotFound}, authzService: m.authz, hubID: m.hubID}
		sel, err = srv.newProbeDeliverySelector(ctx, m.agent)
		require.NoError(t, err, "a missing record is the create-time probe")
		assert.Nil(t, sel)
	})
}

// TestDispatchDelivery_LocalSelectionMatchesResolve: with the local
// backend, start and restart selection of an admitted agent returns the
// same secrets, field for field (Name, Type, Target, Value, Source, Ref),
// as the Resolve path, across scopes, types and custom targets.
func TestDispatchDelivery_LocalSelectionMatchesResolve(t *testing.T) {
	m := newLDFixture(t, "local-parity", ldOptions{})
	ctx := context.Background()
	for _, scope := range []string{store.ScopeProject, store.ScopeHub, store.ScopeRuntimeBroker, store.ScopeUser} {
		up := strings.ToUpper(scope)
		m.secret(t, "MD_LP_ENV_"+up, "env-"+scope, store.SecretTypeEnvironment, scope, m.scopeID(scope))
		_, _, err := m.backend.Set(ctx, &secret.SetSecretInput{
			Name: "md-lp-file-" + scope, Value: "file-" + scope, SecretType: store.SecretTypeFile, Target: "/etc/md-lp/" + scope,
			Scope: scope, ScopeID: m.scopeID(scope), InjectionMode: store.InjectionModeAlways, CreatedBy: m.projectOwnerID,
		})
		require.NoError(t, err)
	}
	got, err := m.d.selectDeliverySecrets(ctx, newDeliverySelector(m.authz, m.agent, PurposeRestart), m.agent)
	require.NoError(t, err)
	old, _, err := m.d.resolveSecrets(ctx, m.agent)
	require.NoError(t, err)
	require.Len(t, got, 8)
	assert.ElementsMatch(t, old, got)
}

// TestMaterialAssociation_FactDefensiveBranches: the association fact's
// defensive branches, each reached by one fault against an admitted
// control. Most cannot be reached through newHubDeliveryIdentity and
// materialAssociationCandidate, so the fact is called directly.
func TestMaterialAssociation_FactDefensiveBranches(t *testing.T) {
	m := newLDFixture(t, "fact-df", ldOptions{})
	ctx := context.Background()
	sm := m.secret(t, "MD_FACT_DF", "v", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)
	res := materialDeliveryResource(MaterialKindSecret, materialRecordSecret, sm.ID, sm.Name, store.ScopeProject, m.agent.ProjectID, m.agent.ID, GrantProjectAssociation, PurposeRestart)
	h := hdIdentity(t, m.authz, m.agent.ID)
	rule := RelationshipRuleProjectAssociation

	holds, detail := m.authz.materialAssociationFact(ctx, h, rule, res)
	require.True(t, holds, "control: %s", detail)

	t.Run("nil_evidence", func(t *testing.T) {
		bad := *h
		bad.evidence = nil
		holds, detail := m.authz.materialAssociationFact(ctx, &bad, rule, res)
		assert.False(t, holds)
		assert.Equal(t, "delivery credential has no stored-agent evidence", detail)
	})
	t.Run("bound_agent_differs", func(t *testing.T) {
		bad := *h
		bad.boundAgentID = tid("md-fact-df-other")
		holds, detail := m.authz.materialAssociationFact(ctx, &bad, rule, res)
		assert.False(t, holds)
		assert.Equal(t, "delivery credential is bound to a different agent", detail)
	})
	t.Run("evidence_agent_differs", func(t *testing.T) {
		bad := *h
		other := *m.agent
		other.ID = tid("md-fact-df-other")
		bad.evidence = &storedAgentIdentity{agent: &other}
		holds, detail := m.authz.materialAssociationFact(ctx, &bad, rule, res)
		assert.False(t, holds)
		assert.Equal(t, "delivery credential is bound to a different agent", detail)
	})
	t.Run("rule_does_not_match_scope", func(t *testing.T) {
		holds, detail := m.authz.materialAssociationFact(ctx, h, RelationshipRuleHubAssociation, res)
		assert.False(t, holds)
		assert.Equal(t, "delivery item scope does not match the association", detail)
	})
	t.Run("store_unavailable", func(t *testing.T) {
		holds, detail := (&AuthzService{}).materialAssociationFact(ctx, h, rule, res)
		assert.False(t, holds)
		assert.Contains(t, detail, "store not available")
	})
	t.Run("project_lookup_error", func(t *testing.T) {
		a := hdWithStore(t, m.authz, &ldFaultStore{Store: m.store, getProjectErr: errors.New("injected project lookup failure")})
		holds, detail := a.materialAssociationFact(ctx, h, rule, res)
		assert.False(t, holds)
		assert.Contains(t, detail, "project lookup failed")
	})
}

// TestDeliverySelector_AdmissionMemoKeepsDeny: a denied admission walk is
// memoized too. The first item stores the deny with its cause, and every
// later item under the same permission in that selection is denied with the
// same cause without walking the chain again.
func TestDeliverySelector_AdmissionMemoKeepsDeny(t *testing.T) {
	m := newLDFixture(t, "memo-deny", ldOptions{edge: withBoundedCeiling("agent.read")})
	ctx := context.Background()
	var metas []secret.SecretMeta
	for _, scope := range []string{store.ScopeProject, store.ScopeHub, store.ScopeRuntimeBroker} {
		metas = append(metas, m.secret(t, "MD_MEMO_DENY_"+strings.ToUpper(scope), "v", store.SecretTypeEnvironment, scope, m.scopeID(scope)))
	}
	var log []string
	hdWithStore(t, m.authz, &ldOrderStore{Store: m.store, target: m.agent.ID, log: &log})
	edgeReads := func(run func()) int {
		log = log[:0]
		run()
		n := 0
		for _, e := range log {
			if e == "edges" {
				n++
			}
		}
		return n
	}

	sel := newDeliverySelector(m.authz, m.agent, PurposeRestart)
	first := edgeReads(func() {
		ok, err := sel.authorize(ctx, MaterialKindSecret, materialRecordSecret, metas[0].ID, metas[0].Name, store.ScopeProject, m.agent.ProjectID)
		require.NoError(t, err)
		assert.False(t, ok, "control: the chain does not allow secret.deliver")
	})
	require.Greater(t, first, 0, "the first item walked the chain")
	require.NotNil(t, sel.ident)
	require.NotNil(t, sel.ident.admission)
	o, ok := sel.ident.admission.load("secret.deliver")
	require.True(t, ok, "the deny is memoized")
	assert.False(t, o.ok)
	assert.Equal(t, DenyCauseCeilingEffectExceeded, o.cause)
	assert.NotEmpty(t, o.reason)

	rest := edgeReads(func() {
		for _, sm := range metas[1:] {
			ok, err := sel.authorize(ctx, MaterialKindSecret, materialRecordSecret, sm.ID, sm.Name, sm.Scope, m.scopeID(sm.Scope))
			require.NoError(t, err)
			assert.False(t, ok, "%s stays denied", sm.Name)
		}
	})
	assert.Less(t, rest, 2*first, "the later items did not walk the chain again")

	grant, ok := grantForDeliveryScope(store.ScopeHub)
	require.True(t, ok)
	res := materialDeliveryResource(MaterialKindSecret, materialRecordSecret, metas[1].ID, metas[1].Name, store.ScopeHub, m.hubID, m.agent.ID, grant, PurposeRestart)
	d := decidePerm(m.authz, sel.ident, res, ActionDeliver, "secret.deliver", false)
	assert.False(t, d.Allowed)
	assert.Equal(t, DenyCauseCeilingEffectExceeded, d.DenyCause, "a memoized deny keeps its cause: %s", d.Reason)
	assert.Equal(t, 3, sel.denied, "every item was refused")
}

// TestDispatchDelivery_TestAdminPlaceholderMasksLowerScope: for an agent
// rooted at an admin-kind test identity whose owner is not the root, the
// owner's user-scope secret is withheld, yet it still outranks an allowed
// project secret of the same name, as on the Resolve path: neither is
// delivered. The withheld secret is never fetched, no item is delivered
// without a value, and a project secret no withheld item masks is delivered.
func TestDispatchDelivery_TestAdminPlaceholderMasksLowerScope(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "md-tam-iss", store.SystemRoleSuperAdmin)
	root := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin).Identity.ID
	projectID := tid("md-tam-project")
	taProject(t, s, projectID, root)

	const hubID = "md-tam-hub"
	backend := secret.NewLocalBackend(s, hubID, "test-secret")
	rec := &ldRecordingSecretBackend{SecretBackend: backend}
	d := NewHTTPAgentDispatcherWithClient(s, &mockRuntimeBrokerClient{}, false, slog.Default())
	d.SetHubID(hubID)
	d.SetAuthzService(srv.authzService)
	d.SetSecretBackend(rec)

	id := tid("md-tam-agent")
	agent := &store.Agent{
		ID: id, Slug: "md-tam-" + id[:8], Name: "md-tam-" + id[:8], ProjectID: projectID,
		Phase: string(state.PhaseRunning), OwnerID: issuer.ID, CreatedBy: root, Ancestry: []string{root},
		AppliedConfig: &store.AgentAppliedConfig{},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	hdEdge(t, s, store.DelegationPrincipalUser, root, id, projectID, nil)

	set := func(name, value, scope, scopeID string) secret.SecretMeta {
		_, meta, err := backend.Set(ctx, &secret.SetSecretInput{
			Name: name, Value: value, SecretType: store.SecretTypeEnvironment, Target: name,
			Scope: scope, ScopeID: scopeID, InjectionMode: store.InjectionModeAlways, CreatedBy: root,
		})
		require.NoError(t, err)
		return *meta
	}
	set("MD_TAM_SHARED", "from-project", store.ScopeProject, projectID)
	set("MD_TAM_PROJECT_ONLY", "project-only", store.ScopeProject, projectID)

	values := func(got []ResolvedSecret) map[string]string {
		out := map[string]string{}
		for _, r := range got {
			out[r.Name] = r.Value
		}
		return out
	}
	got, err := d.selectDeliverySecrets(ctx, newDeliverySelector(srv.authzService, agent, PurposeRestart), agent)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"MD_TAM_SHARED": "from-project", "MD_TAM_PROJECT_ONLY": "project-only"}, values(got),
		"control: the root is a member of the project, so its project secrets are authorized and allowed")

	withheld := set("MD_TAM_SHARED", "from-user", store.ScopeUser, issuer.ID)
	rec.fetched = nil
	got, err = d.selectDeliverySecrets(ctx, newDeliverySelector(srv.authzService, agent, PurposeRestart), agent)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"MD_TAM_PROJECT_ONLY": "project-only"}, values(got),
		"the withheld user secret masks the project secret of the same name, and is itself dropped")
	for _, r := range got {
		assert.NotEmpty(t, r.Value, "%s delivered without a value", r.Name)
	}
	for _, meta := range rec.fetched {
		assert.NotEqual(t, withheld.ID, meta.ID, "the withheld secret's value was fetched")
	}
	old, _, err := d.resolveSecrets(ctx, agent)
	require.NoError(t, err)
	assert.ElementsMatch(t, old, got, "the same result as the Resolve path")
}

// TestDispatchDelivery_UndecryptableSecretFailsStart: a stored secret whose
// value cannot be decrypted fails the start and the restart with a
// *secretResolutionError (errors.Is secret.ErrResolveDecrypt), as Resolve
// fails them, instead of being skipped: a lower-scope secret of the same
// name never stands in for it, and nothing reaches the broker. Both the
// project over hub and the owner's user scope over project are covered.
func TestDispatchDelivery_UndecryptableSecretFailsStart(t *testing.T) {
	ctx := context.Background()
	const key = "MD_UNDECRYPTABLE"
	foreign, err := secret.NewLegacyValueCipher("FAKE-foreign-secret").Encrypt("fake-undecryptable-value")
	require.NoError(t, err)

	type op struct {
		name   string
		run    func(m *ldFixture) error
		called func(m *ldFixture) bool
		env    func(m *ldFixture) map[string]string
	}
	ops := []op{
		{"start", func(m *ldFixture) error { return m.d.DispatchAgentStart(ctx, m.agent, "task", false) },
			func(m *ldFixture) bool { return m.client.startCalled },
			func(m *ldFixture) map[string]string { return m.client.lastResolvedEnv }},
		{"restart", func(m *ldFixture) error { return m.d.DispatchAgentRestart(ctx, m.agent) },
			func(m *ldFixture) bool { return m.client.restartCalled },
			func(m *ldFixture) map[string]string { return m.client.lastRestartResolvedEnv }},
	}
	for _, layers := range []struct{ high, low string }{
		{store.ScopeProject, store.ScopeHub},
		{store.ScopeUser, store.ScopeProject},
	} {
		for _, o := range ops {
			t.Run(layers.high+"_over_"+layers.low+"/"+o.name, func(t *testing.T) {
				m := newLDFixture(t, "undecrypt-"+layers.high+"-"+o.name, ldOptions{})
				require.NoError(t, m.store.CreateSecret(ctx, &store.Secret{
					ID: api.NewUUID(), Key: key, EncryptedValue: foreign, SecretType: store.SecretTypeEnvironment,
					Target: key, Scope: layers.high, ScopeID: m.scopeID(layers.high), InjectionMode: store.InjectionModeAlways,
					CreatedBy: m.projectOwnerID,
				}))
				m.secret(t, key, "lower-scope-value", store.SecretTypeEnvironment, layers.low, m.scopeID(layers.low))

				// The selection itself reports the decrypt failure and
				// returns nothing.
				secrets, err := m.d.selectDeliverySecrets(ctx, newDeliverySelector(m.authz, m.agent, PurposeRestart), m.agent)
				require.ErrorIs(t, err, secret.ErrResolveDecrypt)
				assert.Nil(t, secrets)

				err = o.run(m)
				require.Error(t, err)
				var sre *secretResolutionError
				require.ErrorAs(t, err, &sre)
				assert.ErrorIs(t, err, secret.ErrResolveDecrypt)
				assert.False(t, o.called(m), "nothing reaches the broker")
				assert.NotContains(t, o.env(m), key, "neither value is delivered")
			})
		}
	}
}

// warnRecords decodes a JSON slog buffer and returns the WARN records whose msg equals msg.
func warnRecords(t *testing.T, raw []byte, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line %q", line)
		if rec["level"] == "WARN" && rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// TestDispatchDelivery_WithheldItemsLoggedAtWarn: a restart of an agent the
// delivery decision refuses (a recorded ceiling without the deliver
// permissions, which the legacy fallback does not serve) withholds its project, hub and
// broker items and says so at Warn: one record per item with the key, scope,
// kind and deny cause, and one per-start summary with the count. No value is
// logged.
func TestDispatchDelivery_WithheldItemsLoggedAtWarn(t *testing.T) {
	m := newLDFixture(t, "warn-withheld", ldOptions{edge: withBoundedCeiling("agent.read")})
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	m.authz.logger = logger
	m.d.log = logger
	var keys []string
	for _, scope := range []string{store.ScopeProject, store.ScopeHub, store.ScopeRuntimeBroker} {
		m.env(t, "MD_WARN_ENV_"+strings.ToUpper(scope), "warn-env-value-"+scope, scope, m.scopeID(scope), false)
		m.secret(t, "MD_WARN_SECRET_"+strings.ToUpper(scope), "warn-secret-value-"+scope, store.SecretTypeEnvironment, scope, m.scopeID(scope))
		keys = append(keys, "MD_WARN_ENV_"+strings.ToUpper(scope), "MD_WARN_SECRET_"+strings.ToUpper(scope))
	}

	require.NoError(t, m.d.DispatchAgentRestart(context.Background(), m.agent))
	env := m.client.lastRestartResolvedEnv
	for _, k := range keys {
		assert.NotContains(t, env, k, "withheld item %s was delivered", k)
	}

	items := warnRecords(t, buf.Bytes(), "launch material withheld: not authorized for delivery")
	seen := map[string]map[string]any{}
	for _, r := range items {
		k, _ := r["key"].(string)
		seen[k] = r
	}
	for _, k := range keys {
		r, ok := seen[k]
		require.True(t, ok, "no Warn record for withheld item %s; records: %v", k, items)
		assert.Equal(t, m.agent.ID, r["agent_id"])
		assert.Equal(t, string(PurposeRestart), r["purpose"])
		assert.NotEmpty(t, r["scope"])
		assert.NotEmpty(t, r["kind"])
		assert.NotEmpty(t, r["deny_cause"], "item %s: the deny cause is logged", k)
	}
	summary := warnRecords(t, buf.Bytes(), "launch material withheld: items not authorized for delivery")
	require.Len(t, summary, 1)
	assert.EqualValues(t, len(keys), summary[0]["count"])
	assert.NotContains(t, buf.String(), "warn-env-value-", "a withheld env value was logged")
	assert.NotContains(t, buf.String(), "warn-secret-value-", "a withheld secret value was logged")
}
