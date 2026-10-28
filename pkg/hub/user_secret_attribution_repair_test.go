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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Legacy user secret attribution repair (RepairLegacyUserSecretAttribution)
// and the Warn logging of user secrets withheld by the user secret parent
// ceiling at start, restart and runtime read.

// usaSetUserSecret writes a user-scope environment secret of ownerID with
// the given created_by, as the pre-#1557 capture handler recorded it.
func usaSetUserSecret(t *testing.T, backend secret.SecretBackend, ownerID, key, value, createdBy string, allowProgeny bool, mode string) {
	t.Helper()
	_, _, err := backend.Set(context.Background(), &secret.SetSecretInput{
		Name: key, Value: value, SecretType: store.SecretTypeEnvironment, Target: key,
		Scope: store.ScopeUser, ScopeID: ownerID, InjectionMode: mode, AllowProgeny: allowProgeny,
		CreatedBy: createdBy, UpdatedBy: createdBy,
	})
	require.NoError(t, err)
}

// newUSAFixture is the start delivery fixture with the owner's two shared
// secrets attributed to the capturing agent ("agent:<capturer>") and the
// unshared secret attributed to the owner. The capturer is the owner's
// direct agent with a recorded edge.
func newUSAFixture(t *testing.T, name string) (f *sdFixture, capturer, capturerToken string) {
	t.Helper()
	base := &userScopeCeilingFixture{materialFixture: newMaterialFixture(t, name)}
	setBackfillCompleted(t, base.Store)
	f = &sdFixture{userScopeCeilingFixture: base, disp: sdDispatcher(base.Server, base.Store)}
	capturer, capturerToken = f.direct(t, name+"-capturer", true)
	legacy := userSecretAttrAgentRefPrefix + capturer
	backend := f.Server.secretBackend
	usaSetUserSecret(t, backend, f.UserID, sdAlwaysKey, sdAlwaysValue, legacy, true, store.InjectionModeAlways)
	usaSetUserSecret(t, backend, f.UserID, sdAsNeededKey, sdAsNeededValue, legacy, true, store.InjectionModeAsNeeded)
	usaSetUserSecret(t, backend, f.UserID, sdPrivateKey, sdPrivateValue, f.UserID, false, store.InjectionModeAlways)
	return f, capturer, capturerToken
}

// usaDispatched is what a real DispatchAgentStart and DispatchAgentRestart
// send to the broker of the shared always-injected owner secret, and the
// dispatcher's log.
type usaDispatched struct {
	start, restart bool
	logs           string
}

func usaDispatch(t *testing.T, f *sdFixture, agentID string) usaDispatched {
	t.Helper()
	ctx := context.Background()
	brokerID := tid("usa-broker-" + agentID[:8])
	if _, err := f.Store.GetRuntimeBroker(ctx, brokerID); err != nil {
		require.NoError(t, f.Store.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID: brokerID, Name: "usa-broker-" + agentID[:8], Slug: "usa-broker-" + agentID[:8],
			Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
		}))
		require.NoError(t, f.Store.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: f.ProjectID, BrokerID: brokerID, BrokerName: "usa-broker-" + agentID[:8],
			LocalPath: "/home/user/projects/usa/.scion", Status: store.BrokerStatusOnline,
		}))
	}
	agent, err := f.Store.GetAgent(ctx, agentID)
	require.NoError(t, err)
	agent.RuntimeBrokerID = brokerID
	agent.AppliedConfig.HarnessConfig = "claude"

	logs := &bytes.Buffer{}
	svc, err := NewAgentTokenService(AgentTokenConfig{})
	require.NoError(t, err)
	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(f.Store, client, false, slog.New(slog.NewTextHandler(logs, nil)))
	d.SetTokenGenerator(runTokenGenerator{svc: svc})
	d.SetSecretBackend(f.Server.secretBackend)
	d.SetAuthzService(f.Server.authzService)

	has := func(rs []ResolvedSecret) bool {
		for _, r := range rs {
			if r.Source == store.ScopeUser && r.Name == sdAlwaysKey && r.Value == sdAlwaysValue {
				return true
			}
		}
		return false
	}
	var out usaDispatched
	require.NoError(t, d.DispatchAgentStart(ctx, agent, "", false))
	require.True(t, client.startCalled)
	out.start = has(client.lastStartResolvedSecrets)
	require.NoError(t, d.DispatchAgentRestart(ctx, agent))
	require.True(t, client.restartCalled)
	out.restart = has(client.lastRestartExtras.ResolvedSecrets)
	out.logs = logs.String()
	return out
}

// usaCaptureDefaultLog routes slog.Default to a buffer for the test.
func usaCaptureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// TestUserSecretAttributionRepair_LegacyRowWithheldThenDelivered: an
// owner's shared secret captured by an agent before #1557 is withheld from
// the owner's recorded agents at start, restart and runtime read, with a
// Warn naming the secret and never its value. After the repair it is
// delivered on all three, the repair is audited with the original
// created_by, and a second pass changes nothing.
func TestUserSecretAttributionRepair_LegacyRowWithheldThenDelivered(t *testing.T) {
	f, capturer, capturerToken := newUSAFixture(t, "usa-legacy")
	other, otherToken := f.direct(t, "usa-legacy-other", true)
	child, childToken, _ := f.progeny(t, "usa-legacy-progeny", true)
	defaultLog := usaCaptureDefaultLog(t)

	assert.Equal(t, sdDelivery{}, sdStartDelivery(t, f.disp, f.Store, child),
		"progeny path: the legacy rows are not in the lineage's progeny set before the repair")
	assert.False(t, f.read(t, child, childToken, sdAlwaysKey))
	_, keys := f.listKeys(t, other, otherToken, "user")
	assert.NotContains(t, keys, sdAlwaysKey, "the agent secret list omits the legacy row before the repair")

	for _, a := range []struct{ id, token string }{{capturer, capturerToken}, {other, otherToken}} {
		d := f.ceiling(t, a.id, sdAlwaysKey)
		require.False(t, d.Allowed, "the ceiling withholds the legacy row before the repair")
		assert.Equal(t, sdDelivery{always: false, asNeeded: false, private: true}, sdStartDelivery(t, f.disp, f.Store, a.id),
			"before the repair only the unshared row is delivered")
		got := usaDispatch(t, f, a.id)
		assert.False(t, got.start, "start does not deliver the legacy row")
		assert.False(t, got.restart, "restart does not deliver the legacy row")
		assert.Contains(t, got.logs, "level=WARN")
		assert.Contains(t, got.logs, "start-time user secret withheld by authorization")
		assert.Contains(t, got.logs, "secret="+sdAlwaysKey)
		assert.NotContains(t, got.logs, sdAlwaysValue, "the Warn never carries the value")
		assert.False(t, f.read(t, a.id, a.token, sdAlwaysKey), "the runtime read refuses the legacy row")
		assert.False(t, f.read(t, a.id, a.token, sdAsNeededKey), "the runtime read refuses the legacy row")
	}
	assert.Contains(t, defaultLog.String(), "runtime user secret read withheld by authorization")
	assert.Contains(t, defaultLog.String(), "secret="+sdAsNeededKey)
	assert.NotContains(t, defaultLog.String(), sdAsNeededValue)

	ctx := context.Background()
	before, err := f.Store.GetSecret(ctx, sdAlwaysKey, store.ScopeUser, f.UserID)
	require.NoError(t, err)

	res, err := RepairLegacyUserSecretAttribution(ctx, f.Store)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Examined)
	assert.Empty(t, res.Skipped)
	require.Len(t, res.Repaired, 2)
	for _, r := range res.Repaired {
		assert.Equal(t, userSecretAttrAgentRefPrefix+capturer, r.OriginalCreatedBy)
		assert.Equal(t, userSecretAttrBasisAncestryRoot, r.Basis)
		assert.Equal(t, f.UserID, r.ScopeID)
	}

	after, err := f.Store.GetSecret(ctx, sdAlwaysKey, store.ScopeUser, f.UserID)
	require.NoError(t, err)
	assert.Equal(t, f.UserID, after.CreatedBy)
	assert.Equal(t, before.Version, after.Version, "the repair is not a credential change")
	assert.Equal(t, before.EncryptedValue, after.EncryptedValue)

	audits, _, err := f.Store.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: mutationTypeUserSecretAttributionRepair, TargetID: after.ID,
	})
	require.NoError(t, err)
	require.Len(t, audits, 1)
	assert.Contains(t, audits[0].BeforeSummary, userSecretAttrAgentRefPrefix+capturer, "the audit keeps the original created_by")
	assert.Contains(t, audits[0].AfterSummary, f.UserID)
	assert.Equal(t, "system", audits[0].ActorPrincipalKind)

	for _, a := range []struct{ id, token string }{{capturer, capturerToken}, {other, otherToken}} {
		d := f.ceiling(t, a.id, sdAlwaysKey)
		require.True(t, d.Allowed, "the ceiling allows the repaired row; reason %q", d.Reason)
		assert.Equal(t, sdDelivery{always: true, asNeeded: true, private: true}, sdStartDelivery(t, f.disp, f.Store, a.id))
		got := usaDispatch(t, f, a.id)
		assert.True(t, got.start, "start delivers the repaired row")
		assert.True(t, got.restart, "restart delivers the repaired row")
		assert.NotContains(t, got.logs, "withheld by authorization")
		assert.True(t, f.read(t, a.id, a.token, sdAlwaysKey), "the runtime read returns the repaired row")
		assert.True(t, f.read(t, a.id, a.token, sdAsNeededKey), "the runtime read returns the repaired row")
	}

	assert.Equal(t, sdDelivery{always: true, asNeeded: true, private: false}, sdStartDelivery(t, f.disp, f.Store, child),
		"progeny path: the repaired shared rows are delivered, the unshared row is not")
	assert.True(t, f.read(t, child, childToken, sdAlwaysKey))
	code, keys := f.listKeys(t, other, otherToken, "user")
	require.Equal(t, 200, code)
	assert.Contains(t, keys, sdAlwaysKey, "the agent secret list includes the repaired row")
	assert.Contains(t, keys, sdAsNeededKey, "the agent secret list includes the repaired row")

	again, err := RepairLegacyUserSecretAttribution(ctx, f.Store)
	require.NoError(t, err)
	assert.Zero(t, again.Examined, "a second pass finds nothing to repair")
	assert.Empty(t, again.Repaired)
	assert.Empty(t, again.Skipped)
	audits, _, err = f.Store.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: mutationTypeUserSecretAttributionRepair})
	require.NoError(t, err)
	assert.Len(t, audits, 2, "a second pass writes no audit record")
	final, err := f.Store.GetSecret(ctx, sdAlwaysKey, store.ScopeUser, f.UserID)
	require.NoError(t, err)
	assert.Equal(t, after.Version, final.Version)
	assert.True(t, after.Updated.Equal(final.Updated))
}

// TestUserSecretAttributionRepair_Bases: each provable attribution is
// repaired with its basis.
func TestUserSecretAttributionRepair_Bases(t *testing.T) {
	f, _, _ := newUSAFixture(t, "usa-bases")
	ctx := context.Background()
	backend := f.Server.secretBackend

	mkAgent := func(name string, mutate func(a *store.Agent)) string {
		a := &store.Agent{
			ID: tid("usa-bases-" + name), Slug: "usa-bases-" + name, Name: "usa-bases-" + name,
			ProjectID: f.ProjectID, Phase: "running", StateVersion: 1,
		}
		mutate(a)
		require.NoError(t, f.Store.CreateAgent(ctx, a))
		return a.ID
	}
	otherUser := tid("usa-bases-other-user")
	createDCUser(t, f.Store, otherUser, "usa-bases-other@test.com", f.ProjectID, store.ProjectRoleMember)

	ownerAgent := mkAgent("owner", func(a *store.Agent) { a.OwnerID = f.UserID; a.CreatedBy = otherUser })
	creatorAgent := mkAgent("creator", func(a *store.Agent) { a.OwnerID = otherUser; a.CreatedBy = f.UserID })
	subAgent := mkAgent("sub", func(a *store.Agent) {
		a.Ancestry = []string{f.UserID, ownerAgent}
		a.OwnerID = ownerAgent
		a.CreatedBy = ownerAgent
	})
	softDeleted := mkAgent("deleted", func(a *store.Agent) {
		a.OwnerID = f.UserID
		a.CreatedBy = otherUser
		a.DeletedAt = time.Now()
	})
	sd, err := f.Store.GetAgent(ctx, softDeleted)
	require.NoError(t, err)
	require.False(t, sd.DeletedAt.IsZero(), "the agent is soft-deleted")
	gone := uuid.NewString()

	want := map[string]string{
		"USA_OWNER":    userSecretAttrBasisAgentOwner,
		"USA_CREATOR":  userSecretAttrBasisAgentCreator,
		"USA_SUB":      userSecretAttrBasisAncestryRoot,
		"USA_SOFTDEL":  userSecretAttrBasisAgentOwner,
		"USA_GONE":     userSecretAttrBasisAgentGone,
		"USA_UNSHARED": userSecretAttrBasisAgentGone,
	}
	refs := map[string]string{
		"USA_OWNER": ownerAgent, "USA_CREATOR": creatorAgent, "USA_SUB": subAgent,
		"USA_SOFTDEL": softDeleted, "USA_GONE": gone, "USA_UNSHARED": gone,
	}
	for key, ref := range refs {
		usaSetUserSecret(t, backend, f.UserID, key, "v", userSecretAttrAgentRefPrefix+ref, key != "USA_UNSHARED", store.InjectionModeAsNeeded)
	}

	res, err := RepairLegacyUserSecretAttribution(ctx, f.Store)
	require.NoError(t, err)
	assert.Empty(t, res.Skipped)
	got := map[string]string{}
	for _, r := range res.Repaired {
		got[r.Name] = r.Basis
	}
	for key, basis := range want {
		assert.Equal(t, basis, got[key], "basis for %s", key)
		row, err := f.Store.GetSecret(ctx, key, store.ScopeUser, f.UserID)
		require.NoError(t, err)
		assert.Equal(t, f.UserID, row.CreatedBy, "%s is attributed to its owner", key)
	}
}

// TestUserSecretAttributionRepair_SkipReasons: every row whose attribution
// is not provably the owner's is left unchanged with its reason, and a
// correctly attributed row, or one whose created_by is not an agent
// reference, is not examined.
func TestUserSecretAttributionRepair_SkipReasons(t *testing.T) {
	base := &userScopeCeilingFixture{materialFixture: newMaterialFixture(t, "usa-skips")}
	ctx := context.Background()
	s := base.Store
	backend := base.Server.secretBackend
	owner := base.UserID

	otherUser := tid("usa-skips-other-user")
	createDCUser(t, s, otherUser, "usa-skips-other@test.com", base.ProjectID, store.ProjectRoleMember)
	foreignAgent := tid("usa-skips-foreign")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: foreignAgent, Slug: "usa-skips-foreign", Name: "usa-skips-foreign", ProjectID: base.ProjectID,
		Phase: "running", StateVersion: 1, OwnerID: otherUser, CreatedBy: otherUser, Ancestry: []string{otherUser},
	}))
	missingUser := uuid.NewString()
	// Owned by the scope user, but its ancestry root is another user: the
	// capture handler would have written into that user's scope.
	rootedElsewhere := tid("usa-skips-rooted-elsewhere")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: rootedElsewhere, Slug: "usa-skips-rooted", Name: "usa-skips-rooted", ProjectID: base.ProjectID,
		Phase: "running", StateVersion: 1, OwnerID: owner, CreatedBy: owner, Ancestry: []string{otherUser},
	}))

	usaSetUserSecret(t, backend, owner, "USA_FOREIGN", "v", userSecretAttrAgentRefPrefix+foreignAgent, true, store.InjectionModeAsNeeded)
	usaSetUserSecret(t, backend, owner, "USA_BADREF", "v", userSecretAttrAgentRefPrefix+"not-a-uuid", true, store.InjectionModeAsNeeded)
	usaSetUserSecret(t, backend, owner, "USA_ROOTED", "v", userSecretAttrAgentRefPrefix+rootedElsewhere, true, store.InjectionModeAsNeeded)
	usaSetUserSecret(t, backend, owner, "USA_FEDERATED", "v", userSecretAttrAgentRefPrefix+"https://issuer.example:"+uuid.NewString(), true, store.InjectionModeAsNeeded)
	usaSetUserSecret(t, backend, missingUser, "USA_NOUSER", "v", userSecretAttrAgentRefPrefix+uuid.NewString(), true, store.InjectionModeAsNeeded)
	// Not examined: already attributed to its owner, or not an agent reference.
	usaSetUserSecret(t, backend, owner, "USA_CORRECT", "v", owner, true, store.InjectionModeAsNeeded)
	usaSetUserSecret(t, backend, owner, "USA_OTHERREF", "v", otherUser, true, store.InjectionModeAsNeeded)
	untouched := map[string]*store.Secret{}
	for _, key := range []string{"USA_FOREIGN", "USA_BADREF", "USA_ROOTED", "USA_FEDERATED", "USA_CORRECT", "USA_OTHERREF"} {
		row, err := s.GetSecret(ctx, key, store.ScopeUser, owner)
		require.NoError(t, err)
		untouched[key] = row
	}

	res, err := RepairLegacyUserSecretAttribution(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, 5, res.Examined)
	assert.Empty(t, res.Repaired)
	reasons := map[string]string{}
	for _, sk := range res.Skipped {
		reasons[sk.Name] = sk.Reason
	}
	assert.Equal(t, map[string]string{
		"USA_FOREIGN":   userSecretAttrSkipAgentNotOwnedBy,
		"USA_BADREF":    userSecretAttrSkipAgentRefInvalid,
		"USA_ROOTED":    userSecretAttrSkipAgentNotOwnedBy,
		"USA_FEDERATED": userSecretAttrSkipFederatedRef,
		"USA_NOUSER":    userSecretAttrSkipScopeUserMissing,
	}, reasons)
	assert.Zero(t, res.RowChanged())
	for key, before := range untouched {
		after, err := s.GetSecret(ctx, key, store.ScopeUser, owner)
		require.NoError(t, err)
		assert.Equal(t, before.CreatedBy, after.CreatedBy, "%s is unchanged", key)
		assert.Equal(t, before.Version, after.Version, "%s is unchanged", key)
		assert.True(t, before.Updated.Equal(after.Updated), "%s is unchanged", key)
	}
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: mutationTypeUserSecretAttributionRepair})
	require.NoError(t, err)
	assert.Empty(t, audits, "a skip writes no audit record")

	// A row with no scope owner cannot be stored through the backend; the
	// decision is checked on the row itself.
	_, reason, err := legacyUserSecretAttributionBasis(ctx, s, &store.Secret{
		ID: uuid.NewString(), Key: "USA_EMPTY", Scope: store.ScopeUser, CreatedBy: userSecretAttrAgentRefPrefix + uuid.NewString(),
	})
	require.NoError(t, err)
	assert.Equal(t, userSecretAttrSkipScopeOwnerEmpty, reason)
}

// usaListHookStore runs after() once ListSecrets returns, on every listed
// legacy row, as a concurrent writer between the list and the repair would.
type usaListHookStore struct {
	store.Store
	after func(ctx context.Context, inner store.Store, row store.Secret) error
}

func (r *usaListHookStore) ListSecrets(ctx context.Context, filter store.SecretFilter) ([]store.Secret, error) {
	rows, err := r.Store.ListSecrets(ctx, filter)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Scope != store.ScopeUser || !strings.HasPrefix(row.CreatedBy, userSecretAttrAgentRefPrefix) {
			continue
		}
		if err := r.after(ctx, r.Store, row); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// usaTxStore wraps the transaction store a WithTx callback receives.
type usaTxStore struct {
	store.Store
	wrap func(tx store.Store) store.Store
}

func (w *usaTxStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return w.Store.WithTx(ctx, func(tx store.Store) error { return fn(w.wrap(tx)) })
}

type usaAuditFailTx struct{ store.Store }

func (usaAuditFailTx) CreateMutationAudit(context.Context, *store.MutationAuditRecord) error {
	return errors.New("fake audit write failure")
}

type usaSwapMissTx struct{ store.Store }

func (usaSwapMissTx) AttributeUserSecretToOwner(context.Context, string, string) (bool, error) {
	return false, nil
}

// TestUserSecretAttributionRepair_VersionBumpDuringRepair: a write between
// the list and the repair bumps the version (here a metadata write; the
// value-write case is TestAttributeUserSecretToOwner in entadapter); the
// repair still applies, because the swap does not depend on the version.
func TestUserSecretAttributionRepair_VersionBumpDuringRepair(t *testing.T) {
	f, capturer, _ := newUSAFixture(t, "usa-refresh")
	ctx := context.Background()
	s := &usaListHookStore{Store: f.Store, after: func(ctx context.Context, inner store.Store, row store.Secret) error {
		desc := "refreshed " + time.Now().String()
		_, err := inner.UpdateSecretMeta(ctx, row.Key, row.Scope, row.ScopeID, &store.SecretMetaUpdate{Description: &desc})
		return err
	}}
	res, err := RepairLegacyUserSecretAttribution(ctx, s)
	require.NoError(t, err)
	assert.Empty(t, res.Skipped)
	require.Len(t, res.Repaired, 2)
	for _, r := range res.Repaired {
		assert.Equal(t, userSecretAttrBasisAncestryRoot, r.Basis)
		assert.Equal(t, userSecretAttrAgentRefPrefix+capturer, r.OriginalCreatedBy)
	}
	row, err := f.Store.GetSecret(ctx, sdAlwaysKey, store.ScopeUser, f.UserID)
	require.NoError(t, err)
	assert.Equal(t, f.UserID, row.CreatedBy)
}

// TestUserSecretAttributionRepair_ConcurrentAttribution: another writer (a
// second booting replica) attributes the row to its owner first; the missed
// swap re-reads the row and counts it as repaired, not as a skip, and this
// pass writes no audit record for it.
func TestUserSecretAttributionRepair_ConcurrentAttribution(t *testing.T) {
	f, capturer, _ := newUSAFixture(t, "usa-concurrent")
	ctx := context.Background()
	s := &usaListHookStore{Store: f.Store, after: func(ctx context.Context, inner store.Store, row store.Secret) error {
		_, err := inner.AttributeUserSecretToOwner(ctx, row.ID, row.CreatedBy)
		return err
	}}
	res, err := RepairLegacyUserSecretAttribution(ctx, s)
	require.NoError(t, err)
	assert.Empty(t, res.Skipped)
	require.Len(t, res.Repaired, 2)
	for _, r := range res.Repaired {
		assert.Equal(t, userSecretAttrBasisConcurrent, r.Basis)
		assert.Equal(t, userSecretAttrAgentRefPrefix+capturer, r.OriginalCreatedBy)
	}
	audits, _, err := f.Store.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: mutationTypeUserSecretAttributionRepair})
	require.NoError(t, err)
	assert.Empty(t, audits)
}

// TestUserSecretAttributionRepair_RowDeletedDuringRepair: a row deleted
// between the list and the repair is dropped, not skipped.
func TestUserSecretAttributionRepair_RowDeletedDuringRepair(t *testing.T) {
	f, _, _ := newUSAFixture(t, "usa-deleted")
	ctx := context.Background()
	s := &usaListHookStore{Store: f.Store, after: func(ctx context.Context, inner store.Store, row store.Secret) error {
		return inner.DeleteSecret(ctx, row.Key, row.Scope, row.ScopeID)
	}}
	res, err := RepairLegacyUserSecretAttribution(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Examined)
	assert.Equal(t, 2, res.Vanished)
	assert.Empty(t, res.Repaired)
	assert.Empty(t, res.Skipped)
}

// TestUserSecretAttributionRepair_PersistentSwapMissIsRowChanged: a row
// whose swap keeps missing while it still carries the legacy attribution is
// retried a bounded number of times, then reported as row_changed (which
// keeps the boot marker unwritten) and left unchanged.
func TestUserSecretAttributionRepair_PersistentSwapMissIsRowChanged(t *testing.T) {
	f, capturer, _ := newUSAFixture(t, "usa-rowchanged")
	ctx := context.Background()
	s := &usaTxStore{Store: f.Store, wrap: func(tx store.Store) store.Store { return usaSwapMissTx{Store: tx} }}
	res, err := RepairLegacyUserSecretAttribution(ctx, s)
	require.NoError(t, err)
	assert.Empty(t, res.Repaired)
	require.Len(t, res.Skipped, 2)
	for _, sk := range res.Skipped {
		assert.Equal(t, userSecretAttrSkipRowChanged, sk.Reason)
	}
	assert.Equal(t, 2, res.RowChanged())
	row, err := f.Store.GetSecret(ctx, sdAlwaysKey, store.ScopeUser, f.UserID)
	require.NoError(t, err)
	assert.Equal(t, userSecretAttrAgentRefPrefix+capturer, row.CreatedBy)
}

// TestUserSecretAttributionRepair_AuditFailureRollsBackSwap: when the audit
// record cannot be written, the attribution swap in the same transaction is
// rolled back and the pass returns the error.
func TestUserSecretAttributionRepair_AuditFailureRollsBackSwap(t *testing.T) {
	f, capturer, _ := newUSAFixture(t, "usa-auditfail")
	ctx := context.Background()
	s := &usaTxStore{Store: f.Store, wrap: func(tx store.Store) store.Store { return usaAuditFailTx{Store: tx} }}
	res, err := RepairLegacyUserSecretAttribution(ctx, s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fake audit write failure")
	assert.Empty(t, res.Repaired)
	for _, key := range []string{sdAlwaysKey, sdAsNeededKey} {
		row, err := f.Store.GetSecret(ctx, key, store.ScopeUser, f.UserID)
		require.NoError(t, err)
		assert.Equal(t, userSecretAttrAgentRefPrefix+capturer, row.CreatedBy, "%s: the swap is rolled back", key)
	}
	audits, _, err := f.Store.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: mutationTypeUserSecretAttributionRepair})
	require.NoError(t, err)
	assert.Empty(t, audits)
}

// TestUserSecretWithheldWarn_RateLimited: repeated starts of the same agent
// log the withheld Warn for a secret once per interval, and repeated
// runtime reads likewise.
func TestUserSecretWithheldWarn_RateLimited(t *testing.T) {
	f, capturer, capturerToken := newUSAFixture(t, "usa-ratelimit")
	ctx := context.Background()
	logs := &bytes.Buffer{}
	d := NewHTTPAgentDispatcherWithClient(f.Store, &mockRuntimeBrokerClient{}, false, slog.New(slog.NewTextHandler(logs, nil)))
	d.SetSecretBackend(f.Server.secretBackend)
	d.SetAuthzService(f.Server.authzService)
	agent, err := f.Store.GetAgent(ctx, capturer)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		_, _, err := d.resolveSecrets(ctx, agent)
		require.NoError(t, err)
	}
	assert.Equal(t, 1, strings.Count(logs.String(), "secret="+sdAlwaysKey+" "),
		"three starts log the withheld Warn once for the secret")

	defaultLog := usaCaptureDefaultLog(t)
	for i := 0; i < 3; i++ {
		assert.False(t, f.read(t, capturer, capturerToken, sdAlwaysKey))
	}
	assert.Equal(t, 1, strings.Count(defaultLog.String(), "runtime user secret read withheld by authorization"),
		"three reads log the withheld Warn once")
}

// usaMissOnceTx makes the first attribution swap of secret missID miss,
// across transactions (missed is shared).
type usaMissOnceTx struct {
	store.Store
	missID string
	missed *bool
}

func (m usaMissOnceTx) AttributeUserSecretToOwner(ctx context.Context, id, expected string) (bool, error) {
	if id == m.missID && !*m.missed {
		*m.missed = true
		return false, nil
	}
	return m.Store.AttributeUserSecretToOwner(ctx, id, expected)
}

// usaRereadStore applies mutate to what GetSecret returns for key once a
// swap of that row has missed (*after), as a writer between the swap and
// the re-read would.
type usaRereadStore struct {
	store.Store
	key    string
	after  *bool
	mutate func(*store.Secret)
}

func (r *usaRereadStore) GetSecret(ctx context.Context, key, scope, scopeID string) (*store.Secret, error) {
	sec, err := r.Store.GetSecret(ctx, key, scope, scopeID)
	if err == nil && sec != nil && key == r.key && *r.after {
		r.mutate(sec)
	}
	return sec, err
}

// usaMissOnce returns a store over f whose first swap of key's row misses,
// whose re-read of key is passed through mutate (nil: unchanged), and the
// row's ID.
func usaMissOnce(t *testing.T, f *sdFixture, key string, mutate func(*store.Secret)) (store.Store, string) {
	t.Helper()
	row, err := f.Store.GetSecret(context.Background(), key, store.ScopeUser, f.UserID)
	require.NoError(t, err)
	missed := false
	tx := &usaTxStore{Store: f.Store, wrap: func(tx store.Store) store.Store {
		return usaMissOnceTx{Store: tx, missID: row.ID, missed: &missed}
	}}
	if mutate == nil {
		return tx, row.ID
	}
	return &usaRereadStore{Store: tx, key: key, after: &missed, mutate: mutate}, row.ID
}

// TestUserSecretAttributionRepair_MissThenSuccess: a swap that misses once
// while the row still carries the legacy attribution is decided again and
// applied on the second attempt, with one audit record.
func TestUserSecretAttributionRepair_MissThenSuccess(t *testing.T) {
	f, capturer, _ := newUSAFixture(t, "usa-missonce")
	ctx := context.Background()
	s, id := usaMissOnce(t, f, sdAlwaysKey, nil)
	res, err := RepairLegacyUserSecretAttribution(ctx, s)
	require.NoError(t, err)
	assert.Empty(t, res.Skipped)
	require.Len(t, res.Repaired, 2)
	for _, r := range res.Repaired {
		assert.Equal(t, userSecretAttrBasisAncestryRoot, r.Basis)
		assert.Equal(t, userSecretAttrAgentRefPrefix+capturer, r.OriginalCreatedBy)
	}
	row, err := f.Store.GetSecret(ctx, sdAlwaysKey, store.ScopeUser, f.UserID)
	require.NoError(t, err)
	assert.Equal(t, f.UserID, row.CreatedBy)
	audits, _, err := f.Store.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: mutationTypeUserSecretAttributionRepair, TargetID: id})
	require.NoError(t, err)
	assert.Len(t, audits, 1, "the missed attempt writes no audit record")
}

// TestUserSecretAttributionRepair_RereadForeignAgentNotRewritten: after a
// missed swap, a re-read that finds the row attributed to an agent of
// another user is decided on its own facts: skipped, never rewritten.
func TestUserSecretAttributionRepair_RereadForeignAgentNotRewritten(t *testing.T) {
	f, capturer, _ := newUSAFixture(t, "usa-reread-foreign")
	ctx := context.Background()
	otherUser := tid("usa-reread-foreign-user")
	createDCUser(t, f.Store, otherUser, "usa-reread-foreign-other@test.com", f.ProjectID, store.ProjectRoleMember)
	foreign := tid("usa-reread-foreign-agent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: foreign, Slug: "usa-reread-foreign", Name: "usa-reread-foreign", ProjectID: f.ProjectID,
		Phase: "running", StateVersion: 1, OwnerID: otherUser, CreatedBy: otherUser, Ancestry: []string{otherUser},
	}))
	s, _ := usaMissOnce(t, f, sdAlwaysKey, func(sec *store.Secret) { sec.CreatedBy = userSecretAttrAgentRefPrefix + foreign })
	res, err := RepairLegacyUserSecretAttribution(ctx, s)
	require.NoError(t, err)
	require.Len(t, res.Skipped, 1)
	assert.Equal(t, sdAlwaysKey, res.Skipped[0].Name)
	assert.Equal(t, userSecretAttrSkipAgentNotOwnedBy, res.Skipped[0].Reason)
	assert.Equal(t, userSecretAttrAgentRefPrefix+foreign, res.Skipped[0].CreatedBy)
	require.Len(t, res.Repaired, 1, "the other legacy row is repaired")
	assert.Equal(t, sdAsNeededKey, res.Repaired[0].Name)
	row, err := f.Store.GetSecret(ctx, sdAlwaysKey, store.ScopeUser, f.UserID)
	require.NoError(t, err)
	assert.Equal(t, userSecretAttrAgentRefPrefix+capturer, row.CreatedBy, "the skipped row is not rewritten")
}

// TestUserSecretAttributionRepair_RereadNonAgentAuthorLeftAlone: after a
// missed swap, a re-read that finds an author that is neither an agent
// reference nor the owner leaves the row alone and counts it.
func TestUserSecretAttributionRepair_RereadNonAgentAuthorLeftAlone(t *testing.T) {
	f, capturer, _ := newUSAFixture(t, "usa-reread-nonagent")
	ctx := context.Background()
	s, _ := usaMissOnce(t, f, sdAlwaysKey, func(sec *store.Secret) { sec.CreatedBy = "someone-else" })
	res, err := RepairLegacyUserSecretAttribution(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Examined)
	assert.Equal(t, 1, res.AuthorChanged)
	assert.Empty(t, res.Skipped)
	require.Len(t, res.Repaired, 1)
	assert.Equal(t, sdAsNeededKey, res.Repaired[0].Name)
	row, err := f.Store.GetSecret(ctx, sdAlwaysKey, store.ScopeUser, f.UserID)
	require.NoError(t, err)
	assert.Equal(t, userSecretAttrAgentRefPrefix+capturer, row.CreatedBy, "the row is not rewritten")
}

// TestUserSecretAttributionRepair_RecreatedRowVanishes: a row deleted and
// recreated under the same key between the list and the repair (a new ID)
// counts as vanished, and the new row is not touched by this pass.
func TestUserSecretAttributionRepair_RecreatedRowVanishes(t *testing.T) {
	f, capturer, _ := newUSAFixture(t, "usa-recreated")
	ctx := context.Background()
	legacy := userSecretAttrAgentRefPrefix + capturer
	s := &usaListHookStore{Store: f.Store, after: func(ctx context.Context, inner store.Store, row store.Secret) error {
		if row.Key != sdAlwaysKey {
			return nil
		}
		if err := inner.DeleteSecret(ctx, row.Key, row.Scope, row.ScopeID); err != nil {
			return err
		}
		return inner.CreateSecret(ctx, &store.Secret{
			ID: uuid.NewString(), Key: row.Key, EncryptedValue: "enc-recreated",
			Scope: row.Scope, ScopeID: row.ScopeID, AllowProgeny: true, CreatedBy: legacy,
		})
	}}
	res, err := RepairLegacyUserSecretAttribution(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Vanished)
	require.Len(t, res.Repaired, 1)
	assert.Equal(t, sdAsNeededKey, res.Repaired[0].Name)
	row, err := f.Store.GetSecret(ctx, sdAlwaysKey, store.ScopeUser, f.UserID)
	require.NoError(t, err)
	assert.Equal(t, legacy, row.CreatedBy, "the recreated row is not touched by this pass")
}
