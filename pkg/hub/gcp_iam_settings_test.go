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
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/policytroubleshooter/iam/apiv3/iampb"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reloadable GCP permission-check settings (gcp_iam section): validation on
// save, reload resolution, transition rule, audit and apply.

var (
	iamOffOpen       = gcpIAMSettings{CheckMode: SAAssignCheckOff, DenyUnknownFailOpen: true}
	iamEnforceOpen   = gcpIAMSettings{CheckMode: SAAssignCheckEnforce, DenyUnknownFailOpen: true}
	iamEnforceClosed = gcpIAMSettings{CheckMode: SAAssignCheckEnforce, DenyUnknownFailOpen: false}
)

// iamHubSettingStore is the fake hub settings store with read failures on
// demand.
type iamHubSettingStore struct {
	*fakeHubSettingStore
	failGet  atomic.Bool
	failList atomic.Bool
	// onUpsert and onDelete run before a write; a non-nil error from
	// onUpsert fails it.
	onUpsert func() error
	onDelete func()
}

func (f *iamHubSettingStore) UpsertHubSetting(ctx context.Context, section string, value json.RawMessage, updatedBy string, expectedRevision int64, origin string) (*store.HubSetting, error) {
	if f.onUpsert != nil {
		if err := f.onUpsert(); err != nil {
			return nil, err
		}
	}
	return f.fakeHubSettingStore.UpsertHubSetting(ctx, section, value, updatedBy, expectedRevision, origin)
}

func (f *iamHubSettingStore) DeleteHubSetting(ctx context.Context, section string) error {
	if f.onDelete != nil {
		f.onDelete()
	}
	return f.fakeHubSettingStore.DeleteHubSetting(ctx, section)
}

var errIAMTestRead = errors.New("injected read failure")

func (f *iamHubSettingStore) GetHubSetting(ctx context.Context, section string) (*store.HubSetting, error) {
	if f.failGet.Load() {
		return nil, errIAMTestRead
	}
	return f.fakeHubSettingStore.GetHubSetting(ctx, section)
}

func (f *iamHubSettingStore) ListHubSettings(ctx context.Context) ([]store.HubSetting, error) {
	if f.failList.Load() {
		return nil, errIAMTestRead
	}
	return f.fakeHubSettingStore.ListHubSettings(ctx)
}

// iamAuditStore fails mutation audit writes on demand.
type iamAuditStore struct {
	store.Store
	failAudit atomic.Bool
	// onAudit runs before each audit write.
	onAudit func(*store.MutationAuditRecord)
}

func (s *iamAuditStore) CreateMutationAudit(ctx context.Context, rec *store.MutationAuditRecord) error {
	if s.onAudit != nil {
		s.onAudit(rec)
	}
	if s.failAudit.Load() {
		return errors.New("injected audit failure")
	}
	return s.Store.CreateMutationAudit(ctx, rec)
}

type iamFixture struct {
	srv *Server
	ops *OperationalSettings
	hs  *iamHubSettingStore
	st  *iamAuditStore
}

// newIAMFixture returns a DB-mode server whose deploy-time (and applied)
// pair is startup, wired for self-apply on write.
func newIAMFixture(t *testing.T, startup gcpIAMSettings) *iamFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	srv, fake, ops := newTestDBServer(t)
	hs := &iamHubSettingStore{fakeHubSettingStore: fake}
	ops.store = hs
	ops.server = srv
	st := &iamAuditStore{Store: newGCPIdentitySettingsStore(t)}
	srv.store = st
	srv.gcpIAMStartup = startup
	srv.mu.Lock()
	srv.applyGCPIAMSettingsLocked(startup)
	srv.mu.Unlock()
	return &iamFixture{srv: srv, ops: ops, hs: hs, st: st}
}

func (f *iamFixture) put(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	f.srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), f.ops)
	return rr
}

func (f *iamFixture) reset(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	f.srv.handleAdminServerConfigSectionReset(rr,
		adminRequest(http.MethodDelete, "/api/v1/admin/server-config/sections/"+gcpIAMSection, ""))
	return rr
}

// applied returns the applied pair, checking that both surfaces agree.
func (f *iamFixture) applied(t *testing.T) gcpIAMSettings {
	t.Helper()
	f.srv.mu.RLock()
	defer f.srv.mu.RUnlock()
	require.Equal(t, f.srv.saAssignCheckMode, f.srv.hookIdentityCheckMode, "both surfaces carry one mode")
	return f.srv.gcpIAMSettingsLocked()
}

func (f *iamFixture) audits(t *testing.T) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := f.st.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: gcpIAMAuditMutation})
	require.NoError(t, err)
	return recs
}

func (f *iamFixture) applyAudits(t *testing.T) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := f.st.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: gcpIAMApplyMutation})
	require.NoError(t, err)
	return recs
}

// shareStore makes g use f's main store (one database, two replicas).
func (g *iamFixture) shareStore(f *iamFixture) {
	g.st = f.st
	g.srv.store = f.st
}

func (f *iamFixture) refusals(t *testing.T) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := f.st.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: gcpIAMRefusedMutation})
	require.NoError(t, err)
	return recs
}

func (f *iamFixture) hasRow() bool {
	_, err := f.hs.fakeHubSettingStore.GetHubSetting(context.Background(), gcpIAMSection)
	return err == nil
}

func (f *iamFixture) addHubSA(t *testing.T) {
	t.Helper()
	gcpIdentitySettingsSA(t, f.st.Store, store.ScopeHub, "hub", true)
}

func iamBody(checkMode, denyPolicy string) string {
	hub := map[string]any{}
	if checkMode != "" {
		hub["gcp_iam_check_mode"] = checkMode
	}
	if denyPolicy != "" {
		hub["gcp_iam_deny_unknown_policy"] = denyPolicy
	}
	b, _ := json.Marshal(map[string]any{"server": map[string]any{"hub": hub}})
	return string(b)
}

// ---- C1: validation and reload resolution ----

func TestGCPIAMStartupSettings(t *testing.T) {
	cases := []struct {
		mode, policy string
		want         gcpIAMSettings
	}{
		{"", "", iamOffOpen},
		{"off", "fail-open", iamOffOpen},
		{"enforce", "fail-closed", iamEnforceClosed},
		{"Enforce", "", iamEnforceOpen},
		{"bogus", "", iamEnforceOpen},
		{"off", "fail_closed", gcpIAMSettings{CheckMode: SAAssignCheckOff, DenyUnknownFailOpen: false}},
	}
	for _, tc := range cases {
		t.Run(tc.mode+"/"+tc.policy, func(t *testing.T) {
			assert.Equal(t, tc.want, startupGCPIAMSettings(tc.mode, tc.policy))
		})
	}
}

func TestGCPIAMPut_RejectsEmptyOrUnrecognisedValues(t *testing.T) {
	bodies := []string{
		`{"server":{"hub":{"gcp_iam_check_mode":""}}}`,
		`{"server":{"hub":{"gcp_iam_check_mode":null}}}`,
		`{"server":{"hub":{"gcp_iam_check_mode":"bogus"}}}`,
		`{"server":{"hub":{"gcp_iam_check_mode":"OFF"}}}`,
		`{"server":{"hub":{"gcp_iam_deny_unknown_policy":""}}}`,
		`{"server":{"hub":{"gcp_iam_deny_unknown_policy":null}}}`,
		`{"server":{"hub":{"gcp_iam_deny_unknown_policy":"open"}}}`,
		`{"server":{"hub":{"gcp_iam_check_mode":"enforce","gcp_iam_deny_unknown_policy":""}}}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			f := newIAMFixture(t, iamOffOpen)
			rr := f.put(t, body)
			require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), "must be one of")
			assert.False(t, f.hasRow(), "nothing is stored")
			assert.Equal(t, iamOffOpen, f.applied(t))
			assert.Empty(t, f.audits(t))
		})
	}
}

func TestGCPIAMPut_SavesAppliesAndReportsEffective(t *testing.T) {
	f := newIAMFixture(t, iamOffOpen)
	rr := f.put(t, iamBody("enforce", "fail-closed"))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.True(t, f.hasRow())
	assert.Equal(t, iamEnforceClosed, f.applied(t))
	assert.False(t, f.srv.DenyUnknownFailOpen())

	rr = httptest.NewRecorder()
	f.srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), f.ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp ServerConfigResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotNil(t, resp.Server)
	require.NotNil(t, resp.Server.Hub)
	assert.Equal(t, "enforce", resp.Server.Hub.GCPIAMCheckMode)
	assert.Equal(t, "fail-closed", resp.Server.Hub.GCPIAMDenyUnknownPolicy)

	// A key the body omits keeps its stored value.
	rr = f.put(t, iamBody("", "fail-open"))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, iamEnforceOpen, f.applied(t))
}

func TestGCPIAMPut_OtherKeysUnchanged(t *testing.T) {
	f := newIAMFixture(t, iamOffOpen)
	rr := f.put(t, `{"server":{"hub":{"stalled_threshold":"7m","gcp_iam_check_mode":"enforce"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "7m", f.ops.Snapshot().StalledThreshold)
	assert.Equal(t, SAAssignCheckEnforce, f.applied(t).CheckMode)

	// A body without the keys writes no gcp_iam row and no record.
	g := newIAMFixture(t, iamOffOpen)
	rr = g.put(t, `{"server":{"hub":{"stalled_threshold":"9m"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "9m", g.ops.Snapshot().StalledThreshold)
	assert.False(t, g.hasRow())
	assert.Empty(t, g.audits(t))
	assert.Equal(t, iamOffOpen, g.applied(t))
}

func TestGCPIAMPut_EchoOfAppliedValuesIsNoOp(t *testing.T) {
	f := newIAMFixture(t, iamOffOpen)
	rr := f.put(t, iamBody("off", "fail-open"))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.False(t, f.hasRow(), "an unchanged value is not pinned into the database")
	assert.Empty(t, f.audits(t))
}

func TestGCPIAMReload_UnusableStoredValueKeepsAppliedValue(t *testing.T) {
	// Deploy-time enforce/fail-closed; an admin override applied off/fail-open.
	f := newIAMFixture(t, iamEnforceClosed)
	require.Equal(t, http.StatusOK, f.put(t, iamBody("off", "fail-open")).Code)
	require.Equal(t, iamOffOpen, f.applied(t))

	for _, doc := range []string{
		`{"gcp_iam_check_mode":"bogus","gcp_iam_deny_unknown_policy":"nope"}`,
		`{"gcp_iam_check_mode":"bogus"}`,
		`{"gcp_iam_check_mode":7}`,
	} {
		t.Run(doc, func(t *testing.T) {
			ApplySnapshot(f.srv, snapshotWithRow(t, f, doc))
			assert.Equal(t, iamOffOpen, f.applied(t), "an unusable stored value changes nothing")
		})
	}
	assert.Empty(t, f.applyAudits(t))
	// Every distinct refusal is recorded once.
	assert.Len(t, f.refusals(t), 3)
	ApplySnapshot(f.srv, snapshotWithRow(t, f, `{"gcp_iam_check_mode":7}`))
	assert.Len(t, f.refusals(t), 3, "a repeated refusal is not recorded again")
}

// removeRow deletes the stored row without the guarded reset.
func (f *iamFixture) removeRow(t *testing.T) {
	t.Helper()
	require.NoError(t, f.hs.DeleteHubSetting(context.Background(), gcpIAMSection))
	f.ops.mu.Lock()
	delete(f.ops.cache, gcpIAMSection)
	f.ops.mu.Unlock()
}

func TestGCPIAMReload_VanishedRowKeepsAppliedValueAndIsRecorded(t *testing.T) {
	// Deploy-time off/fail-open; an admin override applied enforce/fail-closed.
	f := newIAMFixture(t, iamOffOpen)
	require.Equal(t, http.StatusOK, f.put(t, iamBody("enforce", "fail-closed")).Code)
	f.removeRow(t)
	ApplySnapshot(f.srv, f.ops.Snapshot())
	assert.Equal(t, iamEnforceClosed, f.applied(t))
	ApplySnapshot(f.srv, snapshotWithRow(t, f, `{}`))
	assert.Equal(t, iamEnforceClosed, f.applied(t))

	// {} and a missing row both leave no stored value: one refusal.
	recs := f.refusals(t)
	require.Len(t, recs, 1)
	assert.Equal(t, gcpIAMSection, recs[0].TargetID)
	assert.Equal(t, "enforce,fail-closed", recs[0].BeforeSummary)
	assert.Equal(t, gcpIAMSystemActorID, recs[0].ActorPrincipalID)
	assert.Equal(t, gcpIAMSurfaceReload, recs[0].ExecutorID)
}

func TestGCPIAMReload_VanishedRowRevertsToStricterDeployValue(t *testing.T) {
	for _, doc := range []string{"", `{}`, `not json`} {
		t.Run(doc, func(t *testing.T) {
			// Deploy-time enforce/fail-closed; an admin override applied off/fail-open.
			f := newIAMFixture(t, iamEnforceClosed)
			require.Equal(t, http.StatusOK, f.put(t, iamBody("off", "fail-open")).Code)
			f.removeRow(t)
			snap := f.ops.Snapshot()
			if doc != "" {
				snap = snapshotWithRow(t, f, doc)
			}
			ApplySnapshot(f.srv, snap)
			assert.Equal(t, iamEnforceClosed, f.applied(t))
			assert.Empty(t, f.refusals(t))
			assert.Len(t, f.applyAudits(t), 2)
		})
	}
}

// snapshotWithRow returns the snapshot for a gcp_iam row holding doc.
func snapshotWithRow(t *testing.T, f *iamFixture, doc string) Layer1Snapshot {
	t.Helper()
	f.ops.mu.Lock()
	f.ops.cache[gcpIAMSection] = sectionState{Value: json.RawMessage(doc), Revision: time.Now().UnixNano()}
	f.ops.mu.Unlock()
	return f.ops.Snapshot()
}

// ---- C3: transition rule ----

func TestGCPIAMPut_RelaxingRefusedWhileHubScopedAssignmentConfigured(t *testing.T) {
	cases := []struct {
		name    string
		startup gcpIAMSettings
		body    string
		setup   func(t *testing.T, f *iamFixture)
	}{
		{"check mode off, hub-scoped account registered", iamEnforceClosed, iamBody("off", ""),
			func(t *testing.T, f *iamFixture) { f.addHubSA(t) }},
		{"deny policy fail-open, hub-scoped account registered", iamEnforceClosed, iamBody("", "fail-open"),
			func(t *testing.T, f *iamFixture) { f.addHubSA(t) }},
		{"check mode off, hub default assign", iamEnforceClosed, iamBody("off", ""),
			func(t *testing.T, f *iamFixture) {
				f.srv.mu.Lock()
				f.srv.config.AgentDefaults.DefaultGCPIdentityMode = store.GCPMetadataModeAssign
				f.srv.mu.Unlock()
			}},
		{"check mode off, hub default assign in the same request", iamEnforceClosed,
			`{"default_gcp_identity_mode":"assign","default_gcp_identity_service_account_id":"x","server":{"hub":{"gcp_iam_check_mode":"off"}}}`,
			func(t *testing.T, f *iamFixture) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newIAMFixture(t, tc.startup)
			tc.setup(t, f)
			rr := f.put(t, tc.body)
			require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), "gcp_iam_change_refused")
			assert.Equal(t, tc.startup, f.applied(t))
			assert.False(t, f.hasRow())
			assert.Empty(t, f.audits(t))
		})
	}
}

func TestGCPIAMPut_RelaxingAllowedWithoutHubScopedAssignment(t *testing.T) {
	f := newIAMFixture(t, iamEnforceClosed)
	gcpIdentitySettingsSA(t, f.st.Store, store.ScopeProject, "p1", true) // project scope does not count
	rr := f.put(t, iamBody("off", "fail-open"))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, iamOffOpen, f.applied(t))
}

func TestGCPIAMPut_TighteningAllowedWithHubScopedAssignment(t *testing.T) {
	f := newIAMFixture(t, iamOffOpen)
	f.addHubSA(t)
	rr := f.put(t, iamBody("enforce", "fail-closed"))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, iamEnforceClosed, f.applied(t))
}

func TestGCPIAMReset_RelaxingRefusedWhileHubScopedAssignmentConfigured(t *testing.T) {
	f := newIAMFixture(t, iamOffOpen)
	require.Equal(t, http.StatusOK, f.put(t, iamBody("enforce", "")).Code)
	f.addHubSA(t)

	rr := f.reset(t)
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.True(t, f.hasRow())
	assert.Equal(t, iamEnforceOpen, f.applied(t))
}

// ---- C4: audit ----

func TestGCPIAMPut_EveryChangeIsAudited(t *testing.T) {
	f := newIAMFixture(t, iamOffOpen)
	before := time.Now().Add(-time.Second)
	rr := f.put(t, iamBody("enforce", "fail-closed"))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	recs := f.audits(t)
	require.Len(t, recs, 2)
	got := map[string]*store.MutationAuditRecord{}
	for _, r := range recs {
		got[r.TargetID] = r
	}
	for key, want := range map[string][2]string{
		gcpIAMCheckModeKey:   {"off", "enforce"},
		gcpIAMDenyUnknownKey: {"fail-open", "fail-closed"},
	} {
		r := got[key]
		require.NotNil(t, r, key)
		assert.Equal(t, gcpIAMAuditTarget, r.TargetType)
		assert.Equal(t, want[0], r.BeforeSummary, key)
		assert.Equal(t, want[1], r.AfterSummary, key)
		assert.Equal(t, "u1", r.ActorPrincipalID, key)
		assert.NotEmpty(t, r.ActorPrincipalKind, key)
		assert.True(t, r.Timestamp.After(before), key)
		assert.Equal(t, gcpIAMSurfaceKind, r.ExecutorKind, key)
		assert.Equal(t, gcpIAMSurfaceSave, r.ExecutorID, key)
	}

	// Changing one key records one row.
	require.Equal(t, http.StatusOK, f.put(t, iamBody("", "fail-open")).Code)
	assert.Len(t, f.audits(t), 3)
}

func TestGCPIAMReset_IsAudited(t *testing.T) {
	f := newIAMFixture(t, iamOffOpen)
	require.Equal(t, http.StatusOK, f.put(t, iamBody("enforce", "")).Code)
	rr := f.reset(t)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, iamOffOpen, f.applied(t))

	recs := f.audits(t)
	require.Len(t, recs, 2)
	var last *store.MutationAuditRecord
	for _, r := range recs {
		if last == nil || !r.Timestamp.Before(last.Timestamp) {
			last = r
		}
	}
	assert.Equal(t, gcpIAMCheckModeKey, last.TargetID)
	assert.Equal(t, "enforce", last.BeforeSummary)
	assert.Equal(t, "off", last.AfterSummary)
	assert.Equal(t, gcpIAMSurfaceReset, last.ExecutorID)
	assert.Equal(t, "u1", last.ActorPrincipalID)
}

func TestGCPIAMPut_ChangeNotMadeWhenAuditFails(t *testing.T) {
	f := newIAMFixture(t, iamOffOpen)
	f.st.failAudit.Store(true)
	rr := f.put(t, iamBody("enforce", ""))
	require.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())
	assert.False(t, f.hasRow())
	assert.Equal(t, iamOffOpen, f.applied(t))
}

// ---- C5: atomic apply, read failures keep the applied value ----

func TestGCPIAMPut_ReadFailureKeepsAppliedValue(t *testing.T) {
	f := newIAMFixture(t, iamOffOpen)
	f.hs.failGet.Store(true)
	rr := f.put(t, iamBody("enforce", ""))
	require.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())
	assert.Equal(t, iamOffOpen, f.applied(t))
	assert.Empty(t, f.audits(t))
}

func TestGCPIAMReload_RefreshFailureKeepsAppliedValue(t *testing.T) {
	// Replica f writes; replica g shares the database and reloads.
	f := newIAMFixture(t, iamOffOpen)
	g := newIAMFixture(t, iamOffOpen)
	g.shareStore(f)
	g.hs.fakeHubSettingStore = f.hs.fakeHubSettingStore
	require.Equal(t, http.StatusOK, f.put(t, iamBody("enforce", "fail-closed")).Code)
	g.ops.refreshAndApply(context.Background(), g.srv)
	require.Equal(t, iamEnforceClosed, g.applied(t))

	g.hs.failList.Store(true)
	g.ops.refreshAndApply(context.Background(), g.srv)
	assert.Equal(t, iamEnforceClosed, g.applied(t), "a failed read must not revert to the deploy-time value")
	assert.Empty(t, g.refusals(t), "a failed read is not a missing row")

	g.hs.failList.Store(false)
	require.Equal(t, http.StatusOK, f.put(t, iamBody("", "fail-open")).Code)
	g.ops.refreshAndApply(context.Background(), g.srv)
	assert.Equal(t, iamEnforceOpen, g.applied(t))
}

func TestGCPIAMApply_HappensUnderServerMutex(t *testing.T) {
	f := newIAMFixture(t, iamOffOpen)
	snap := snapshotWithRow(t, f, `{"gcp_iam_check_mode":"enforce","gcp_iam_deny_unknown_policy":"fail-closed"}`)
	f.srv.approveGCPIAMTransition(gcpIAMTransition{From: iamOffOpen, To: iamEnforceClosed})

	f.srv.mu.RLock()
	done := make(chan struct{})
	go func() {
		ApplySnapshot(f.srv, snap)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, SAAssignCheckOff, f.srv.saAssignCheckMode, "not applied while a reader holds s.mu")
	assert.Equal(t, SAAssignCheckOff, f.srv.hookIdentityCheckMode)
	assert.True(t, f.srv.denyUnknownFailOpen.Load())
	f.srv.mu.RUnlock()
	<-done
	assert.Equal(t, iamEnforceClosed, f.applied(t))
}

// iamCountingChecker counts CanActAs calls.
type iamCountingChecker struct{ n atomic.Int32 }

func (c *iamCountingChecker) CanActAs(context.Context, store.Principal, *store.GCPServiceAccount) (store.ActAsResult, error) {
	c.n.Add(1)
	return store.ActAsResult{Outcome: store.ActAsAllowed}, nil
}

func TestGCPIAMApply_InvalidatesCachedDecisions(t *testing.T) {
	f := newIAMFixture(t, iamEnforceOpen)
	inner := &iamCountingChecker{}
	cached := NewCachedCallerPermissionChecker(inner, time.Minute, time.Minute)
	f.srv.SetSAAssignChecker(cached)
	f.srv.SetHookIdentityChecker(cached)
	_, _ = cached.CanActAs(context.Background(), testCaller, testTargetSA)
	_, _ = cached.CanActAs(context.Background(), testCaller, testTargetSA)
	require.EqualValues(t, 1, inner.n.Load(), "second call is cached")

	f.srv.approveGCPIAMTransition(gcpIAMTransition{From: iamEnforceOpen, To: iamEnforceClosed})
	ApplySnapshot(f.srv, snapshotWithRow(t, f, `{"gcp_iam_deny_unknown_policy":"fail-closed"}`))
	_, _ = cached.CanActAs(context.Background(), testCaller, testTargetSA)
	assert.EqualValues(t, 2, inner.n.Load(), "a settings change drops cached decisions")
}

func TestPolicyTroubleshooterChecker_FollowsPolicySource(t *testing.T) {
	fake := &fakePTClient{resp: &iampb.TroubleshootIamPolicyResponse{
		OverallAccessState: iampb.TroubleshootIamPolicyResponse_UNKNOWN_INFO,
		AllowPolicyExplanation: &iampb.AllowPolicyExplanation{
			AllowAccessState: iampb.AllowAccessState_ALLOW_ACCESS_STATE_GRANTED,
		},
		DenyPolicyExplanation: &iampb.DenyPolicyExplanation{
			DenyAccessState: iampb.DenyAccessState_DENY_ACCESS_STATE_UNKNOWN_INFO,
		},
	}}
	f := newIAMFixture(t, iamEnforceOpen)
	checker := NewPolicyTroubleshooterChecker(fake, testHubSAEmail, true)
	checker.SetDenyUnknownPolicySource(f.srv.DenyUnknownFailOpen)

	res, err := checker.CanActAs(context.Background(), testCaller, testTargetSA)
	require.NoError(t, err)
	assert.Equal(t, store.ActAsAllowed, res.Outcome)

	f.srv.approveGCPIAMTransition(gcpIAMTransition{From: iamEnforceOpen, To: iamEnforceClosed})
	ApplySnapshot(f.srv, snapshotWithRow(t, f, `{"gcp_iam_deny_unknown_policy":"fail-closed"}`))
	res, err = checker.CanActAs(context.Background(), testCaller, testTargetSA)
	require.NoError(t, err)
	assert.NotEqual(t, store.ActAsAllowed, res.Outcome, fmt.Sprintf("reloaded policy applies: %+v", res))
}

// ---- C2: a hub admin's stored value overrides the deploy-time value ----

// memberRequest is adminRequest for a signed-in non-admin user.
func memberRequest(method, url, body string) *http.Request {
	r := adminRequest(method, url, body)
	member := NewAuthenticatedUser("u2", "member@example.com", "Member", store.UserRoleMember, "cli")
	ctx := contextWithCredentialContext(contextWithIdentity(r.Context(), member), CredentialContext{Kind: CredentialKindInteractive})
	return r.WithContext(ctx)
}

func TestGCPIAMPut_NonAdminChangeRefused(t *testing.T) {
	for _, body := range []string{iamBody("enforce", ""), iamBody("", "fail-closed")} {
		t.Run(body, func(t *testing.T) {
			f := newIAMFixture(t, iamOffOpen)
			rr := httptest.NewRecorder()
			f.srv.handlePutServerConfigDB(rr, memberRequest(http.MethodPut, "/api/v1/admin/server-config", body), f.ops)
			require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
			assert.False(t, f.hasRow())
			assert.Empty(t, f.audits(t))
			assert.Equal(t, iamOffOpen, f.applied(t))
		})
	}
}

func TestGCPIAMPut_NonAdminEchoAllowed(t *testing.T) {
	f := newIAMFixture(t, iamOffOpen)
	rr := httptest.NewRecorder()
	f.srv.handlePutServerConfigDB(rr, memberRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"server":{"hub":{"stalled_threshold":"8m","gcp_iam_check_mode":"off","gcp_iam_deny_unknown_policy":"fail-open"}}}`), f.ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, "8m", f.ops.Snapshot().StalledThreshold)
	assert.False(t, f.hasRow())
}

func TestGCPIAMReset_NonAdminRefused(t *testing.T) {
	f := newIAMFixture(t, iamOffOpen)
	require.Equal(t, http.StatusOK, f.put(t, iamBody("enforce", "")).Code)
	rr := httptest.NewRecorder()
	f.srv.handleAdminServerConfigSectionReset(rr,
		memberRequest(http.MethodDelete, "/api/v1/admin/server-config/sections/"+gcpIAMSection, ""))
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
	assert.True(t, f.hasRow())
	assert.Equal(t, iamEnforceOpen, f.applied(t))
}

func TestGCPIAMOverride_AdminValueBelowDeployTimeAppliesAtReload(t *testing.T) {
	// The writing replica: deploy-time enforce/fail-closed, admin sets off/fail-open.
	f := newIAMFixture(t, iamEnforceClosed)
	rr := f.put(t, iamBody("off", "fail-open"))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, iamOffOpen, f.applied(t))

	// Another replica with the same deploy-time values picks the stored
	// value up on its next reload.
	g := newIAMFixture(t, iamEnforceClosed)
	g.shareStore(f)
	row, err := f.hs.fakeHubSettingStore.GetHubSetting(context.Background(), gcpIAMSection)
	require.NoError(t, err)
	g.hs.seed(gcpIAMSection, row.Value)
	require.Equal(t, iamEnforceClosed, g.applied(t))
	g.ops.refreshAndApply(context.Background(), g.srv)
	assert.Equal(t, iamOffOpen, g.applied(t), "the stored value overrides the deploy-time value")
}

func TestGCPIAMOverride_RelaxingStillRefusedWhileHubScopedAssignmentConfigured(t *testing.T) {
	f := newIAMFixture(t, iamEnforceClosed)
	f.addHubSA(t)
	rr := f.put(t, iamBody("off", "fail-open"))
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Equal(t, iamEnforceClosed, f.applied(t))
	assert.False(t, f.hasRow())
}

// ---- Reloads of another replica's change ----

func TestGCPIAMReload_AppliesAttributedChangeWithRecord(t *testing.T) {
	f := newIAMFixture(t, iamEnforceClosed)
	g := newIAMFixture(t, iamEnforceClosed)
	g.shareStore(f)
	require.Equal(t, http.StatusOK, f.put(t, iamBody("off", "")).Code)
	g.hs.seed(gcpIAMSection, iamStoredRow(t, f))
	g.ops.refreshAndApply(context.Background(), g.srv)
	require.Equal(t, gcpIAMSettings{CheckMode: SAAssignCheckOff}, g.applied(t))

	recs := g.applyAudits(t)
	require.Len(t, recs, 1)
	assert.Equal(t, gcpIAMCheckModeKey, recs[0].TargetID)
	assert.Equal(t, "enforce", recs[0].BeforeSummary)
	assert.Equal(t, "off", recs[0].AfterSummary)
	assert.Equal(t, "u1", recs[0].ActorPrincipalID, "the actor of the audited write")
	assert.Equal(t, gcpIAMSurfaceKind, recs[0].ExecutorKind)
	assert.Equal(t, gcpIAMSurfaceReload, recs[0].ExecutorID)
	assert.False(t, recs[0].Timestamp.IsZero())
}

func TestGCPIAMReload_UnattributedChangeRefused(t *testing.T) {
	f := newIAMFixture(t, iamEnforceClosed)
	// A row written without the guarded path (no audit record).
	f.hs.seed(gcpIAMSection, json.RawMessage(`{"gcp_iam_check_mode":"off","gcp_iam_deny_unknown_policy":"fail-open"}`))
	f.ops.refreshAndApply(context.Background(), f.srv)
	assert.Equal(t, iamEnforceClosed, f.applied(t))
	assert.Empty(t, f.applyAudits(t))
	recs := f.refusals(t)
	require.Len(t, recs, 1)
	assert.Contains(t, recs[0].AfterSummary, "not attributable to an audited write")
}

func TestGCPIAMReload_RelaxingRefusedWhileHubScopedAssignmentConfigured(t *testing.T) {
	f := newIAMFixture(t, iamEnforceClosed)
	g := newIAMFixture(t, iamEnforceClosed)
	g.shareStore(f)
	require.Equal(t, http.StatusOK, f.put(t, iamBody("off", "")).Code)
	g.addHubSA(t) // registered after f's save, before g reloads
	g.hs.seed(gcpIAMSection, iamStoredRow(t, f))
	g.ops.refreshAndApply(context.Background(), g.srv)
	assert.Equal(t, iamEnforceClosed, g.applied(t))
	assert.Empty(t, g.applyAudits(t))
}

func TestGCPIAMReload_RefusedWhenAuditFails(t *testing.T) {
	f := newIAMFixture(t, iamEnforceClosed)
	g := newIAMFixture(t, iamEnforceClosed)
	g.shareStore(f)
	require.Equal(t, http.StatusOK, f.put(t, iamBody("off", "")).Code)
	g.hs.seed(gcpIAMSection, iamStoredRow(t, f))
	g.st.failAudit.Store(true)
	g.ops.refreshAndApply(context.Background(), g.srv)
	g.st.failAudit.Store(false)
	assert.Equal(t, iamEnforceClosed, g.applied(t))
	assert.Empty(t, g.applyAudits(t))
}

func TestGCPIAMReload_StartupRefusesUnattributedOverride(t *testing.T) {
	// ApplySnapshot at startup goes through the same decision.
	f := newIAMFixture(t, iamEnforceClosed)
	ApplySnapshot(f.srv, snapshotWithRow(t, f, `{"gcp_iam_check_mode":"off"}`))
	assert.Equal(t, iamEnforceClosed, f.applied(t))
}

func iamStoredRow(t *testing.T, f *iamFixture) json.RawMessage {
	t.Helper()
	row, err := f.hs.fakeHubSettingStore.GetHubSetting(context.Background(), gcpIAMSection)
	require.NoError(t, err)
	return row.Value
}

// ---- C2: hub-admin authorization by credential kind ----

func TestGCPIAMHubAdmin_CredentialKinds(t *testing.T) {
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", store.UserRoleAdmin, "cli")
	member := NewAuthenticatedUser("u2", "member@example.com", "Member", store.UserRoleMember, "cli")
	cases := []struct {
		name     string
		identity Identity
		kind     CredentialKind
		want     bool
	}{
		{"admin, interactive session", admin, CredentialKindInteractive, true},
		{"dev user, dev credential", NewDevUser(DevUserConfig{Username: "dev", Email: "dev@example.com"}), CredentialKindDev, true},
		{"admin, user access token", admin, CredentialKindUAT, false},
		{"admin, scoped token identity", NewScopedUserIdentity(admin, "p1", []string{"hub:config"}), CredentialKindUAT, false},
		{"admin, no credential context", admin, "", false},
		{"member, interactive session", member, CredentialKindInteractive, false},
		{"federated admin", NewFederatedUserIdentity("https://issuer.example", "sub", "fed@example.com", "Fed", store.UserRoleAdmin, nil), CredentialKindFederation, false},
		{"federated admin, interactive kind", NewFederatedUserIdentity("https://issuer.example", "sub", "fed@example.com", "Fed", store.UserRoleAdmin, nil), CredentialKindInteractive, false},
		{"agent", &agentIdentityWrapper{&AgentTokenClaims{ProjectID: "p1"}}, CredentialKindAgentJWT, false},
		{"broker", NewBrokerIdentity(tid("b1")), CredentialKindBroker, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := contextWithIdentity(context.Background(), tc.identity)
			if tc.kind != "" {
				ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: tc.kind})
			}
			assert.Equal(t, tc.want, gcpIAMHubAdmin(ctx))

			// The same decision on both write routes.
			f := newIAMFixture(t, iamOffOpen)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/server-config",
				strings.NewReader(iamBody("enforce", ""))).WithContext(ctx)
			rr := httptest.NewRecorder()
			f.srv.handlePutServerConfigDB(rr, req, f.ops)
			if tc.want {
				require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
				return
			}
			require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
			assert.Equal(t, iamOffOpen, f.applied(t))
			assert.False(t, f.hasRow())

			g := newIAMFixture(t, iamOffOpen)
			require.Equal(t, http.StatusOK, g.put(t, iamBody("enforce", "")).Code)
			req = httptest.NewRequest(http.MethodDelete, "/api/v1/admin/server-config/sections/"+gcpIAMSection, nil).WithContext(ctx)
			rr = httptest.NewRecorder()
			g.srv.handleAdminServerConfigSectionReset(rr, req)
			require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
			assert.True(t, g.hasRow())
		})
	}
}

func TestGCPIAMReload_UnusableValueRefusedEvenWhenAttributable(t *testing.T) {
	// g applied f's override to off; f then sets enforce (audited), but the
	// row g reads is unusable. Resolving it would give the deploy-time
	// enforce, which the latest audit record names, yet the value itself
	// fails validation, so g keeps what it applied.
	f := newIAMFixture(t, iamEnforceClosed)
	g := newIAMFixture(t, iamEnforceClosed)
	g.shareStore(f)
	require.Equal(t, http.StatusOK, f.put(t, iamBody("off", "")).Code)
	g.hs.seed(gcpIAMSection, iamStoredRow(t, f))
	g.ops.refreshAndApply(context.Background(), g.srv)
	require.Equal(t, SAAssignCheckOff, g.applied(t).CheckMode)

	require.Equal(t, http.StatusOK, f.put(t, iamBody("enforce", "")).Code)
	ApplySnapshot(g.srv, snapshotWithRow(t, g, `{"gcp_iam_check_mode":"enfroce","gcp_iam_deny_unknown_policy":"fail-closed"}`))
	assert.Equal(t, SAAssignCheckOff, g.applied(t).CheckMode)
	assert.Len(t, g.refusals(t), 1)
}

// ---- Approval lifetime, attribution and apply guards ----

// setApplied sets the applied pair directly, as another reload would.
func (f *iamFixture) setApplied(p gcpIAMSettings) {
	f.srv.mu.Lock()
	f.srv.applyGCPIAMSettingsLocked(p)
	f.srv.mu.Unlock()
}

func (f *iamFixture) approval() *gcpIAMTransition {
	f.srv.mu.RLock()
	defer f.srv.mu.RUnlock()
	return f.srv.gcpIAMApproved
}

func TestGCPIAMPut_ApprovalClearedWhenSelfApplyDoesNotUseIt(t *testing.T) {
	f := newIAMFixture(t, iamEnforceClosed)
	// The applied pair moves during the write, so the self-apply decides
	// a different transition than the one the save approved.
	f.hs.onUpsert = func() error {
		f.setApplied(iamOffOpen)
		return nil
	}
	require.Equal(t, http.StatusOK, f.put(t, iamBody("off", "fail-open")).Code)
	f.hs.onUpsert = nil
	assert.Nil(t, f.approval(), "the approval does not outlive its write")

	// A later reload of the same transition goes through the full rule.
	f.setApplied(iamEnforceClosed)
	f.addHubSA(t)
	ApplySnapshot(f.srv, f.ops.Snapshot())
	assert.Equal(t, iamEnforceClosed, f.applied(t))
	assert.Empty(t, f.applyAudits(t))
}

func TestGCPIAMReset_ApprovalClearedWhenSelfApplyDoesNotUseIt(t *testing.T) {
	f := newIAMFixture(t, iamOffOpen)
	require.Equal(t, http.StatusOK, f.put(t, iamBody("enforce", "fail-closed")).Code)
	require.Equal(t, iamEnforceClosed, f.applied(t))
	f.hs.onDelete = func() { f.setApplied(iamOffOpen) }
	require.Equal(t, http.StatusOK, f.reset(t).Code)
	f.hs.onDelete = nil
	assert.Nil(t, f.approval(), "the approval does not outlive its reset")

	f.setApplied(iamEnforceClosed)
	f.addHubSA(t)
	ApplySnapshot(f.srv, f.ops.Snapshot())
	assert.Equal(t, iamEnforceClosed, f.applied(t))
	assert.Empty(t, f.applyAudits(t))
}

func TestGCPIAMReload_AttributionRequiredOnlyForLessStrictChange(t *testing.T) {
	t.Run("stricter change applies without an audit record", func(t *testing.T) {
		f := newIAMFixture(t, iamOffOpen)
		f.hs.seed(gcpIAMSection, json.RawMessage(`{"gcp_iam_check_mode":"enforce","gcp_iam_deny_unknown_policy":"fail-closed"}`))
		f.ops.refreshAndApply(context.Background(), f.srv)
		assert.Equal(t, iamEnforceClosed, f.applied(t))
		assert.Empty(t, f.refusals(t))
		recs := f.applyAudits(t)
		require.Len(t, recs, 2)
		for _, r := range recs {
			assert.Equal(t, gcpIAMSystemActorKind, r.ActorPrincipalKind)
			assert.Equal(t, gcpIAMSystemActorID, r.ActorPrincipalID)
		}
	})
	for name, tc := range map[string]struct {
		startup gcpIAMSettings
		row     string
	}{
		"less strict change": {iamEnforceClosed, `{"gcp_iam_check_mode":"off","gcp_iam_deny_unknown_policy":"fail-closed"}`},
		"mixed change":       {gcpIAMSettings{CheckMode: SAAssignCheckOff}, `{"gcp_iam_check_mode":"enforce","gcp_iam_deny_unknown_policy":"fail-open"}`},
	} {
		t.Run(name+" is refused without an audit record", func(t *testing.T) {
			f := newIAMFixture(t, tc.startup)
			f.hs.seed(gcpIAMSection, json.RawMessage(tc.row))
			f.ops.refreshAndApply(context.Background(), f.srv)
			assert.Equal(t, tc.startup, f.applied(t))
			assert.Empty(t, f.applyAudits(t))
			recs := f.refusals(t)
			require.Len(t, recs, 1)
			assert.Contains(t, recs[0].AfterSummary, "not attributable to an audited write")
		})
	}
}

func TestGCPIAMReload_NotAppliedWriteIsNotAttributable(t *testing.T) {
	f := newIAMFixture(t, iamEnforceClosed)
	g := newIAMFixture(t, iamEnforceClosed)
	g.shareStore(f)
	f.hs.onUpsert = func() error { return errors.New("injected write failure") }
	require.Equal(t, http.StatusInternalServerError, f.put(t, iamBody("off", "")).Code)
	f.hs.onUpsert = nil
	require.Len(t, f.audits(t), 1, "the change was recorded before the write")
	require.Eventually(t, func() bool {
		recs, _, err := f.st.ListMutationAudits(context.Background(),
			store.MutationAuditFilter{MutationType: gcpIAMNotAppliedMutation})
		return err == nil && len(recs) == 1
	}, 5*time.Second, 10*time.Millisecond)

	// The same value later appears without an audited write.
	g.hs.seed(gcpIAMSection, json.RawMessage(`{"gcp_iam_check_mode":"off"}`))
	g.ops.refreshAndApply(context.Background(), g.srv)
	assert.Equal(t, iamEnforceClosed, g.applied(t))
	assert.Empty(t, g.applyAudits(t))
	recs := g.refusals(t)
	require.Len(t, recs, 1)
	assert.Contains(t, recs[0].AfterSummary, "not attributable to an audited write")
}

func TestGCPIAMNotAppliedRecordWrittenBeforeHandlerReturns(t *testing.T) {
	f := newIAMFixture(t, iamEnforceClosed)
	// A slow store write: the record is only present when the handler
	// returns if the handler waited for the write.
	var writes atomic.Int32
	f.st.onAudit = func(rec *store.MutationAuditRecord) {
		if rec.MutationType == gcpIAMNotAppliedMutation {
			time.Sleep(200 * time.Millisecond)
			writes.Add(1)
		}
	}
	f.hs.onUpsert = func() error { return errors.New("injected write failure") }
	require.Equal(t, http.StatusInternalServerError, f.put(t, iamBody("off", "")).Code)
	f.hs.onUpsert = nil

	require.Equal(t, int32(1), writes.Load(), "write completed before the handler returned")
	recs, _, err := f.st.ListMutationAudits(context.Background(),
		store.MutationAuditFilter{MutationType: gcpIAMNotAppliedMutation})
	require.NoError(t, err)
	require.Len(t, recs, 1)
	r := recs[0]
	assert.Equal(t, gcpIAMAuditTarget, r.TargetType)
	assert.Equal(t, gcpIAMSection, r.TargetID)
	assert.Equal(t, "enforce,fail-closed", r.BeforeSummary)
	assert.Equal(t, "off,fail-closed", r.AfterSummary)
	assert.NotEmpty(t, r.ActorPrincipalID)
}

func TestGCPIAMNotAppliedRecordStoreFailureKeepsResponse(t *testing.T) {
	f := newIAMFixture(t, iamEnforceClosed)
	f.st.onAudit = func(rec *store.MutationAuditRecord) {
		if rec.MutationType == gcpIAMNotAppliedMutation {
			f.st.failAudit.Store(true)
		}
	}
	f.hs.onUpsert = func() error { return errors.New("injected write failure") }
	require.Equal(t, http.StatusInternalServerError, f.put(t, iamBody("off", "")).Code)
	f.hs.onUpsert = nil
	f.st.failAudit.Store(false)
	recs, _, err := f.st.ListMutationAudits(context.Background(),
		store.MutationAuditFilter{MutationType: gcpIAMNotAppliedMutation})
	require.NoError(t, err)
	assert.Empty(t, recs)
	assert.Equal(t, iamEnforceClosed, f.applied(t))
}

func TestGCPIAMReload_NotAppliedRecordForOtherKeyKeepsAttribution(t *testing.T) {
	f := newIAMFixture(t, iamEnforceClosed)
	g := newIAMFixture(t, iamEnforceClosed)
	g.shareStore(f)
	require.Equal(t, http.StatusOK, f.put(t, iamBody("off", "")).Code)
	// A later failed write of the other key only.
	f.hs.onUpsert = func() error { return errors.New("injected write failure") }
	require.Equal(t, http.StatusInternalServerError, f.put(t, iamBody("", "fail-open")).Code)
	f.hs.onUpsert = nil
	require.Eventually(t, func() bool {
		recs, _, err := f.st.ListMutationAudits(context.Background(),
			store.MutationAuditFilter{MutationType: gcpIAMNotAppliedMutation})
		return err == nil && len(recs) == 1
	}, 5*time.Second, 10*time.Millisecond)

	g.hs.seed(gcpIAMSection, iamStoredRow(t, f))
	g.ops.refreshAndApply(context.Background(), g.srv)
	assert.Equal(t, gcpIAMSettings{CheckMode: SAAssignCheckOff}, g.applied(t))
}

func TestGCPIAMReload_AppliedPairChangedDuringDecisionIsKept(t *testing.T) {
	f := newIAMFixture(t, iamEnforceClosed)
	g := newIAMFixture(t, iamEnforceClosed)
	g.shareStore(f)
	require.Equal(t, http.StatusOK, f.put(t, iamBody("off", "fail-open")).Code)
	g.hs.seed(gcpIAMSection, iamStoredRow(t, f))

	// The applied pair changes after the decision reads it and before the
	// apply step takes the server mutex.
	g.st.onAudit = func(rec *store.MutationAuditRecord) {
		if rec.MutationType == gcpIAMApplyMutation {
			g.setApplied(iamEnforceOpen)
		}
	}
	g.ops.refreshAndApply(context.Background(), g.srv)
	g.st.onAudit = nil
	assert.Equal(t, iamEnforceOpen, g.applied(t), "the apply step does not overwrite a pair it did not decide from")
}
