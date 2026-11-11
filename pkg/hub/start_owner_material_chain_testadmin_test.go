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
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	omcTAShared      = "OMC_TA_SHARED"       // owner unshared user row and a project row
	omcTAUserOnly    = "OMC_TA_USER_ONLY"    // owner unshared user row only
	omcTAProjectOnly = "OMC_TA_PROJECT_ONLY" // project row only
	omcTAAsNeeded    = "OMC_TA_AS_NEEDED"    // as_needed: owner unshared user row and a project row
)

// omcTASeed writes the rows of these tests: in the project, the shared-name
// row, a project-only row and an as_needed row; for the owner, unshared
// rows under the shared name, an owner-only name and the as_needed name.
// It returns the owner rows' metadata.
func omcTASeed(t *testing.T, backend secret.SecretBackend, projectID, ownerID string) []secret.SecretMeta {
	t.Helper()
	set := func(name, value, scope, scopeID, mode string) secret.SecretMeta {
		_, meta, err := backend.Set(context.Background(), &secret.SetSecretInput{
			Name: name, Value: value, SecretType: store.SecretTypeEnvironment, Target: name,
			Scope: scope, ScopeID: scopeID, InjectionMode: mode, CreatedBy: scopeID,
		})
		require.NoError(t, err)
		return *meta
	}
	set(omcTAShared, "from-project", store.ScopeProject, projectID, store.InjectionModeAlways)
	set(omcTAProjectOnly, "project-only", store.ScopeProject, projectID, store.InjectionModeAlways)
	set(omcTAAsNeeded, "project-as-needed", store.ScopeProject, projectID, store.InjectionModeAsNeeded)
	return []secret.SecretMeta{
		set(omcTAShared, "owner-shared-name", store.ScopeUser, ownerID, store.InjectionModeAlways),
		set(omcTAUserOnly, "owner-user-only", store.ScopeUser, ownerID, store.InjectionModeAlways),
		set(omcTAAsNeeded, "owner-as-needed", store.ScopeUser, ownerID, store.InjectionModeAsNeeded),
	}
}

// omcTAValues is name -> value of the delivered secrets, with the source
// scope of each.
func omcTAValues(got []ResolvedSecret) (map[string]string, map[string]string) {
	values, sources := map[string]string{}, map[string]string{}
	for _, r := range got {
		values[r.Name] = r.Value
		sources[r.Name] = r.Source
	}
	return values, sources
}

// assertNothingOfOwner fails if any delivered secret is a user-scope row or
// carries one of the owner's values.
func assertNothingOfOwner(t *testing.T, site string, got []ResolvedSecret) {
	t.Helper()
	for _, r := range got {
		assert.NotEqual(t, store.ScopeUser, r.Source, "%s: owner row %s delivered", site, r.Name)
		assert.False(t, strings.HasPrefix(r.Value, "owner-"), "%s: an owner value was delivered for %s", site, r.Name)
	}
}

// TestOwnerMaterialChain_TestAdminMaskDeliversNothingOfOwner: for an agent
// rooted at an admin-kind test identity whose owner is not the root, the
// test-admin restriction withholds the user scope. The owner's unshared rows
// are kept for that restriction to drop (one shared evaluation), so the
// withheld row still masks the project row of the same name, and nothing of
// the owner's is delivered or fetched, at resolveSecrets and at the start
// and restart launch selection. The project row no owner row shadows is
// delivered.
func TestOwnerMaterialChain_TestAdminMaskDeliversNothingOfOwner(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "omc-ta-iss", store.SystemRoleSuperAdmin)
	root := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin).Identity.ID
	projectID := tid("omc-ta-project")
	taProject(t, s, projectID, root)

	const hubID = "omc-ta-hub"
	backend := secret.NewLocalBackend(s, hubID, "test-secret")
	rec := &ldRecordingSecretBackend{SecretBackend: backend}
	d := NewHTTPAgentDispatcherWithClient(s, &mockRuntimeBrokerClient{}, false, slog.Default())
	d.SetHubID(hubID)
	d.SetAuthzService(srv.authzService)
	d.SetSecretBackend(rec)

	id := tid("omc-ta-agent")
	agent := &store.Agent{
		ID: id, Slug: "omc-ta-" + id[:8], Name: "omc-ta-" + id[:8], ProjectID: projectID,
		Phase: string(state.PhaseRunning), OwnerID: issuer.ID, CreatedBy: root, Ancestry: []string{root},
		AppliedConfig: &store.AgentAppliedConfig{},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	hdEdge(t, s, store.DelegationPrincipalUser, root, id, projectID, nil)

	owner := omcTASeed(t, backend, projectID, issuer.ID)

	ta := d.testAdminScopesFor(ctx, agent)
	require.True(t, ta.withholdsUser(), "the test-admin restriction withholds the user scope for this agent")

	want := map[string]string{omcTAProjectOnly: "project-only"}
	resolved, _, err := d.resolveSecrets(ctx, agent)
	require.NoError(t, err)
	values, _ := omcTAValues(resolved)
	assert.Equal(t, want, values, "resolveSecrets: the withheld owner row masks the project row of the same name")
	assertNothingOfOwner(t, "resolveSecrets", resolved)

	// The as_needed pass: the kept owner row masks the project row of the
	// same name and is dropped; nothing is returned for the key.
	asNeeded, err := d.resolveAsNeededForKeys(ctx, agent, []string{omcTAAsNeeded, omcTAUserOnly}, nil)
	require.NoError(t, err)
	assert.Empty(t, asNeeded, "as_needed pass: nothing of the owner's, and the masked project row is not used")

	for _, purpose := range []MaterialPurpose{PurposeDispatch, PurposeRestart} {
		rec.fetched = nil
		got, _, err := d.selectDeliverySecrets(ctx, newDeliverySelector(srv.authzService, agent, purpose), agent)
		require.NoError(t, err)
		values, _ := omcTAValues(got)
		assert.Equal(t, want, values, "launch selection (%s)", purpose)
		assertNothingOfOwner(t, "launch selection ("+string(purpose)+")", got)
		for _, meta := range rec.fetched {
			for _, o := range owner {
				assert.NotEqual(t, o.ID, meta.ID, "an owner row's value was fetched (%s)", purpose)
			}
		}
	}
}

// TestOwnerMaterialChain_RedelegatedSameRowsDeliverNothingOfOwner: the same
// rows for a re-delegated agent with no test-admin restriction: the chain
// rule withholds the owner's unshared rows, so the project row of the same
// name stays in effect, and nothing of the owner's is delivered.
func TestOwnerMaterialChain_RedelegatedSameRowsDeliverNothingOfOwner(t *testing.T) {
	f := newSDFixture(t, "omc-ta-redeleg")
	ctx := context.Background()
	id, _ := omcRedelegated(t, f, "ta-redeleg")
	agent, err := f.Store.GetAgent(ctx, id)
	require.NoError(t, err)

	omcTASeed(t, f.Server.secretBackend, f.ProjectID, f.UserID)

	ta := f.disp.testAdminScopesFor(ctx, agent)
	require.False(t, ta.restricted, "no test-admin restriction here")

	resolved, _, err := f.disp.resolveSecrets(ctx, agent)
	require.NoError(t, err)
	values, sources := omcTAValues(resolved)
	assert.Equal(t, "from-project", values[omcTAShared], "the project row of the same name stays in effect")
	assert.Equal(t, store.ScopeProject, sources[omcTAShared])
	assert.Equal(t, "project-only", values[omcTAProjectOnly])
	assert.NotContains(t, values, omcTAUserOnly)
	assertNothingOfOwner(t, "resolveSecrets", resolved)

	asNeeded, err := f.disp.resolveAsNeededForKeys(ctx, agent, []string{omcTAAsNeeded, omcTAUserOnly}, nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{omcTAAsNeeded: "project-as-needed"}, asNeeded, "as_needed pass: the project value, never the owner's")

	for _, purpose := range []MaterialPurpose{PurposeDispatch, PurposeRestart} {
		got, _, err := f.disp.selectDeliverySecrets(ctx, newDeliverySelector(f.disp.authzService, agent, purpose), agent)
		require.NoError(t, err)
		values, sources := omcTAValues(got)
		assert.Equal(t, "from-project", values[omcTAShared], "launch selection (%s): the project value", purpose)
		assert.Equal(t, store.ScopeProject, sources[omcTAShared])
		assert.NotContains(t, values, omcTAUserOnly, "launch selection (%s)", purpose)
		assertNothingOfOwner(t, "launch selection ("+string(purpose)+")", got)
	}
}

// omcFlakyKindStore fails the first user read of userID after each arm
// (the kind read of the test-admin check), then reads normally. faults
// counts the failed reads.
type omcFlakyKindStore struct {
	store.Store
	userID string
	armed  *bool
	faults *int
}

func (s omcFlakyKindStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == s.userID && *s.armed {
		*s.armed = false
		*s.faults++
		return nil, errors.New("omc: transient user read fault")
	}
	return s.Store.GetUser(ctx, id)
}

// TestOwnerMaterialChain_TestAdminDecisionOncePerSite: the test-admin check
// is decided once per site and that one value is shared by the keep decision
// and the restriction. With a user read that fails once and then succeeds,
// a re-delegated agent rooted at a human user never receives the owner's
// unshared row at any site: a failed decision keeps the row and also drops
// it (fail closed); a successful one withholds it by the chain rule.
func TestOwnerMaterialChain_TestAdminDecisionOncePerSite(t *testing.T) {
	f := newSDFixture(t, "omc-ta-flaky")
	ctx := context.Background()
	id, _ := omcRedelegated(t, f, "ta-flaky")
	agent, err := f.Store.GetAgent(ctx, id)
	require.NoError(t, err)
	omcTASeed(t, f.Server.secretBackend, f.ProjectID, f.UserID)
	armed, faults := false, 0
	f.Server.authzService.store = omcFlakyKindStore{Store: f.Server.authzService.store, userID: f.UserID, armed: &armed, faults: &faults}

	// The armed fault lands on the test-admin decision: it reads the root
	// user's kind first, and a failed read decides restricted with nothing
	// allowed (the user scope withheld).
	armed = true
	direct := f.disp.testAdminScopesFor(ctx, agent)
	require.False(t, armed, "the test-admin decision consumed the fault")
	require.True(t, direct.restricted)
	require.True(t, direct.withholdsUser(), "a failed read withholds the user scope")
	// Unarmed, the same decision finds a human root: no restriction.
	require.False(t, f.disp.testAdminScopesFor(ctx, agent).restricted)

	// consumed asserts that a site's first read of the root user, the
	// test-admin decision, took the fault.
	consumed := func(site string, before int) {
		t.Helper()
		require.False(t, armed, "%s: the armed fault fired", site)
		require.Equal(t, before+1, faults, "%s: exactly one faulted read", site)
	}

	armed = true
	before := faults
	resolved, _, err := f.disp.resolveSecrets(ctx, agent)
	require.NoError(t, err)
	consumed("resolveSecrets", before)
	assertNothingOfOwner(t, "resolveSecrets", resolved)

	armed = true
	before = faults
	asNeeded, err := f.disp.resolveAsNeededForKeys(ctx, agent, []string{omcTAAsNeeded, omcTAUserOnly}, nil)
	require.NoError(t, err)
	consumed("as_needed", before)
	assert.NotEqual(t, "owner-as-needed", asNeeded[omcTAAsNeeded], "as_needed pass")
	assert.NotContains(t, asNeeded, omcTAUserOnly, "as_needed pass")

	for _, purpose := range []MaterialPurpose{PurposeDispatch, PurposeRestart} {
		armed = true
		before = faults
		got, _, err := f.disp.selectDeliverySecrets(ctx, newDeliverySelector(f.disp.authzService, agent, purpose), agent)
		require.NoError(t, err)
		consumed("launch selection ("+string(purpose)+")", before)
		assertNothingOfOwner(t, "launch selection ("+string(purpose)+")", got)
	}
}
