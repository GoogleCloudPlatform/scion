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
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// E.2b (async initiator attribution, ptone/scion#2127, plan §3.5).
// ---------------------------------------------------------------------------

// initiatorAttributionColumnFields lists the field names InitiatorAttributionMixin
// contributes to both ent.Schedule and ent.ScheduledEvent. Kept as a literal
// list (not derived from store.InitiatorAttribution's field names) so a
// rename on one side that the mixin doesn't actually share still fails this
// test.
var initiatorAttributionColumnFields = []string{
	"InitiatorPrincipalKind",
	"InitiatorPrincipalID",
	"InitiatorCredentialKind",
	"InitiatorCredentialID",
	"InitiatorCredentialSnapshot",
	"AttributionVersion",
	"AuthorizationRevision",
}

// TestInitiatorAttributionMixin_IdenticalColumns pins plan §3.5's requirement
// that Schedule and ScheduledEvent expose an identical attribution column
// set (one ent mixin), so B.3 can read the same shape from either row and
// recurrence propagation is a single struct assignment.
func TestInitiatorAttributionMixin_IdenticalColumns(t *testing.T) {
	scheduleType := reflect.TypeOf(ent.Schedule{})
	eventType := reflect.TypeOf(ent.ScheduledEvent{})

	for _, name := range initiatorAttributionColumnFields {
		sf, ok := scheduleType.FieldByName(name)
		require.True(t, ok, "ent.Schedule missing mixin field %s", name)
		ef, ok := eventType.FieldByName(name)
		require.True(t, ok, "ent.ScheduledEvent missing mixin field %s", name)
		assert.Equal(t, sf.Type, ef.Type, "field %s: type differs between Schedule and ScheduledEvent", name)
		assert.Equal(t, sf.Tag.Get("json"), ef.Tag.Get("json"), "field %s: json tag (column name) differs", name)
	}
}

// TestScheduledInitiator_LegacyRowReadsAsLegacyUnknown covers design check
// (c): a row written before E.2b (AttributionVersion 0/NULL) must read as an
// explicit legacy_unknown, never as an interactive credential, even if some
// individual columns happen to carry stale/partial data.
func TestScheduledInitiator_LegacyRowReadsAsLegacyUnknown(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("e2b-legacy-p"), Name: "p", Slug: "e2b-legacy-p", CreatedBy: DevUserID, OwnerID: DevUserID}
	require.NoError(t, s.CreateProject(ctx, project))

	// A pre-E.2b row: created directly via the store, with no
	// InitiatorAttribution set at all (mirrors a row written before this
	// migration).
	evt := &store.ScheduledEvent{
		ID:        tid("e2b-legacy-evt"),
		ProjectID: project.ID,
		EventType: "message",
		FireAt:    time.Now().Add(time.Hour),
		Payload:   `{"agentName":"a","message":"hi"}`,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))

	got, err := s.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, got.AttributionVersion, "a row created without InitiatorAttribution must have no attribution_version")

	initiator, err := srv.scheduledInitiator(ctx, *got)
	require.NoError(t, err)
	assert.True(t, initiator.LegacyUnknown)
	assert.Equal(t, store.InitiatorCredentialKindLegacyUnknown, initiator.CredentialKind)
	assert.Empty(t, initiator.PrincipalKind)
	assert.Empty(t, initiator.PrincipalID)
	assert.Empty(t, initiator.CredentialID)
	assert.NotEqual(t, string(CredentialKindInteractive), initiator.CredentialKind,
		"a legacy row must never be reported as an interactive credential")

	// Also cover the bare-struct entry point scheduledInitiator accepts.
	bare, err := srv.scheduledInitiator(ctx, store.InitiatorAttribution{})
	require.NoError(t, err)
	assert.True(t, bare.LegacyUnknown)
}

// TestScheduledInitiator_UnsupportedRowType covers the defensive default
// branch.
func TestScheduledInitiator_UnsupportedRowType(t *testing.T) {
	srv := &Server{}
	_, err := srv.scheduledInitiator(context.Background(), 42)
	assert.Error(t, err)
}

// TestCreateScheduledEvent_CapturesInitiatorAttribution covers plan §3.5's
// one-shot create row: the authoring request's identity/credential are
// captured atomically with the event row (design check (a): a single
// CreateScheduledEvent insert).
func TestCreateScheduledEvent_CapturesInitiatorAttribution(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	_ = srv

	req := CreateScheduledEventRequest{
		EventType: "message",
		FireIn:    "1h",
		AgentName: "nonexistent",
		Message:   "hello",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var created store.ScheduledEvent
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	assert.Equal(t, 1, created.AttributionVersion)
	assert.Equal(t, 1, created.AuthorizationRevision)
	assert.Equal(t, "dev", created.InitiatorPrincipalKind) // doRequest authenticates via the dev-auth identity
	assert.Equal(t, DevUserID, created.InitiatorPrincipalID)
	assert.NotEqual(t, string(CredentialKindInteractive), created.InitiatorCredentialKind)

	// Round-trip through the store directly, not just the HTTP response.
	stored, err := s.GetScheduledEvent(context.Background(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.InitiatorCredentialKind, stored.InitiatorCredentialKind)
	assert.Equal(t, created.InitiatorPrincipalID, stored.InitiatorPrincipalID)
}

// TestCreateSchedule_CapturesInitiatorAttribution mirrors the scheduled-event
// case for the recurring-schedule create path.
func TestCreateSchedule_CapturesInitiatorAttribution(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	req := CreateScheduleRequest{
		Name:      "daily",
		CronExpr:  "0 9 * * *",
		EventType: "message",
		AgentName: "all",
		Message:   "status please",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var sched store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sched))
	assert.Equal(t, 1, sched.AttributionVersion)
	assert.Equal(t, 1, sched.AuthorizationRevision)
	assert.Equal(t, "dev", sched.InitiatorPrincipalKind) // doRequest authenticates via the dev-auth identity
	assert.Equal(t, DevUserID, sched.InitiatorPrincipalID)
}

// TestExecuteSchedule_RecurrenceCopiesInitiatorAttribution pins plan §3.5's
// recurrence row: server.go's executeSchedule copies the schedule's current
// InitiatorAttribution verbatim onto each materialized event.
func TestExecuteSchedule_RecurrenceCopiesInitiatorAttribution(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	sched := &store.Schedule{
		ID:        tid("e2b-recur-sched"),
		ProjectID: projectID,
		Name:      "recur",
		CronExpr:  "0 9 * * *",
		EventType: "message",
		Payload:   `{"agentName":"a","message":"hi"}`,
		Status:    store.ScheduleStatusActive,
		CreatedBy: DevUserID,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:      "user",
			InitiatorPrincipalID:        tid("e2b-recur-user"),
			InitiatorCredentialKind:     store.InitiatorCredentialKindUAT,
			InitiatorCredentialID:       tid("e2b-recur-token"),
			InitiatorCredentialSnapshot: `{"name":"recur-token"}`,
			AttributionVersion:          1,
			AuthorizationRevision:       3,
		},
	}
	require.NoError(t, s.CreateSchedule(ctx, sched))

	srv.executeSchedule(ctx, *sched, time.Now())

	result, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ScheduleID: sched.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	evt := result.Items[0]

	assert.Equal(t, sched.InitiatorPrincipalKind, evt.InitiatorPrincipalKind)
	assert.Equal(t, sched.InitiatorPrincipalID, evt.InitiatorPrincipalID)
	assert.Equal(t, sched.InitiatorCredentialKind, evt.InitiatorCredentialKind)
	assert.Equal(t, sched.InitiatorCredentialID, evt.InitiatorCredentialID)
	assert.Equal(t, sched.InitiatorCredentialSnapshot, evt.InitiatorCredentialSnapshot)
	assert.Equal(t, sched.AttributionVersion, evt.AttributionVersion)
	assert.Equal(t, sched.AuthorizationRevision, evt.AuthorizationRevision,
		"the materialized event keeps the schedule's revision snapshot")
}

// TestSchedulerFireEvent_RestartReplayPreservesInitiatorAttribution pins plan
// §3.5's restart-replay row: firing an overdue persisted event (the
// wasExpired=true path loadPersistedTimers uses) is read-only with respect
// to attribution.
func TestSchedulerFireEvent_RestartReplayPreservesInitiatorAttribution(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	evt := store.ScheduledEvent{
		ID:        tid("e2b-replay-evt"),
		ProjectID: projectID,
		EventType: "message",
		FireAt:    time.Now().Add(-time.Hour), // overdue
		Payload:   `{"agentName":"nonexistent","message":"hi"}`,
		CreatedBy: DevUserID,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  "user",
			InitiatorPrincipalID:    tid("e2b-replay-user"),
			InitiatorCredentialKind: store.InitiatorCredentialKindUAT,
			InitiatorCredentialID:   tid("e2b-replay-token"),
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, &evt))

	// Exercise the same fireEvent(wasExpired=true) path loadPersistedTimers
	// uses for overdue events, synchronously (loadPersistedTimers itself
	// wraps this in `go`, which is a concurrency detail, not a behavioral
	// one).
	srv.scheduler.fireEvent(ctx, evt, true)

	got, err := s.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	assert.Equal(t, evt.InitiatorPrincipalKind, got.InitiatorPrincipalKind)
	assert.Equal(t, evt.InitiatorPrincipalID, got.InitiatorPrincipalID)
	assert.Equal(t, evt.InitiatorCredentialKind, got.InitiatorCredentialKind)
	assert.Equal(t, evt.InitiatorCredentialID, got.InitiatorCredentialID)
	assert.Equal(t, evt.AttributionVersion, got.AttributionVersion)
	// The target agent doesn't exist, so the handler errors and fireEvent's
	// existing (pre-E.2b) behavior downgrades the wasExpired-derived
	// "expired" status to "failed" — status handling is unchanged by E.2b;
	// this test only asserts attribution survives the replay.
	assert.Equal(t, store.ScheduledEventFailed, got.Status)
}

// e2bFailingScheduleStore injects a store failure into CreateScheduledEvent,
// to prove that a failed authoring write leaves neither the row nor a
// partial attribution behind.
type e2bFailingScheduleStore struct {
	store.Store
	createScheduledEventErr error
}

func (f *e2bFailingScheduleStore) CreateScheduledEvent(ctx context.Context, evt *store.ScheduledEvent) error {
	if f.createScheduledEventErr != nil {
		return f.createScheduledEventErr
	}
	return f.Store.CreateScheduledEvent(ctx, evt)
}

// TestCreateScheduledEvent_StoreFailureLeavesNoPartialAttribution covers the
// transaction-guarantee test the plan requires: an injected store failure
// during authoring leaves neither the event row nor a partial attribution.
func TestCreateScheduledEvent_StoreFailureLeavesNoPartialAttribution(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	fs := &e2bFailingScheduleStore{Store: s, createScheduledEventErr: errors.New("initiator attribution E2E test: injected scheduled event insert failure")}
	srv.scheduler.store = fs

	req := CreateScheduledEventRequest{
		EventType: "message",
		FireIn:    "1h",
		AgentName: "nonexistent",
		Message:   "hello",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", req)
	assert.NotEqual(t, http.StatusCreated, rec.Code)

	result, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.Items, "a failed create must leave no row, partial or otherwise")
}

// TestUpdateSchedule_MetadataOnlyDoesNotReattribute and
// TestUpdateSchedule_FutureDispatchChangeReattributes cover ruling Q2: a
// fully reauthorized mutation that changes future dispatch replaces
// attribution and bumps authorization_revision atomically; a metadata-only
// edit does not.
func TestUpdateSchedule_MetadataOnlyDoesNotReattribute(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code)
	var sched store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sched))
	require.Equal(t, 1, sched.AuthorizationRevision)

	updateReq := UpdateScheduleRequest{Name: "n2"}
	rec2 := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+sched.ID, updateReq)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	var updated store.Schedule
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&updated))

	assert.Equal(t, "n2", updated.Name)
	assert.Equal(t, 1, updated.AuthorizationRevision, "a metadata-only edit must not bump the revision")
	assert.Equal(t, sched.InitiatorCredentialID, updated.InitiatorCredentialID)
}

func TestUpdateSchedule_FutureDispatchChangeReattributes(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code)
	var sched store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sched))
	require.Equal(t, 1, sched.AuthorizationRevision)

	updateReq := UpdateScheduleRequest{CronExpr: "0 10 * * *"}
	rec2 := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+sched.ID, updateReq)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	var updated store.Schedule
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&updated))

	assert.Equal(t, "0 10 * * *", updated.CronExpr)
	assert.Equal(t, 2, updated.AuthorizationRevision, "a timing change must bump the revision")
	assert.Equal(t, sched.CreatedBy, updated.CreatedBy, "CreatedBy must never be touched by a re-attribution")
}

// TestResumeSchedule_Reattributes covers the resume/enable half of ruling
// Q2: resuming re-arms future dispatch, so it re-attributes even though
// nothing about the schedule's payload/timing changed.
func TestResumeSchedule_Reattributes(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code)
	var sched store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&sched))

	pauseRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+sched.ID+"/pause", nil)
	require.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())

	resumeRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+sched.ID+"/resume", nil)
	require.Equal(t, http.StatusOK, resumeRec.Code, resumeRec.Body.String())
	var resumed store.Schedule
	require.NoError(t, json.NewDecoder(resumeRec.Body).Decode(&resumed))

	assert.Equal(t, store.ScheduleStatusActive, resumed.Status)
	assert.Equal(t, 2, resumed.AuthorizationRevision, "resume must bump the revision")
}

// newScopedUATInitiatorContext builds a live-request context carrying a
// scoped UAT identity/credential (kind=uat), for tests that exercise
// InitiatorAttribution capture directly rather than through the
// scheduled-event/schedule HTTP authoring path — which denies every scoped
// UAT today (B's interim dispatch_agent authoring gate; plan correction
// (a): "Supported-UAT scheduled execution is a B.3 integration fixture, not
// an E-only admission"). B.3's tests can reuse this fixture and the
// assertions in TestCaptureInitiatorAttribution_ScopedUATFixture below.
func newScopedUATInitiatorContext(userID, tokenID, projectID string, scopes []string) context.Context {
	user := NewAuthenticatedUser(userID, userID+"@example.com", "Test User", "member", "api")
	scoped := NewScopedUserIdentityWithCredentialID(user, projectID, scopes, tokenID)
	ctx := contextWithIdentity(context.Background(), scoped)
	return contextWithCredentialContext(ctx, credentialContextForIdentity(scoped))
}

func TestCaptureInitiatorAttribution_ScopedUATFixture(t *testing.T) {
	userID := tid("e2b-uat-user")
	tokenID := tid("e2b-uat-token")
	projectID := tid("e2b-uat-project")
	ctx := newScopedUATInitiatorContext(userID, tokenID, projectID, []string{"scheduled_event:create"})

	attr := newInitiatorAttribution(ctx)
	assert.Equal(t, "user", attr.InitiatorPrincipalKind)
	assert.Equal(t, userID, attr.InitiatorPrincipalID)
	assert.Equal(t, store.InitiatorCredentialKindUAT, attr.InitiatorCredentialKind)
	assert.Equal(t, tokenID, attr.InitiatorCredentialID)
	assert.Equal(t, 1, attr.AuthorizationRevision)
	assert.Equal(t, initiatorAttributionVersion, attr.AttributionVersion)
}

// TestCaptureInitiatorAttribution_NoIdentityReadsAsLegacyUnknown covers the
// defensive branch: a context with no ambient identity (should not happen on
// an authenticated path) still produces a zero attribution, which
// scheduledInitiator reads back as legacy_unknown.
func TestCaptureInitiatorAttribution_NoIdentityReadsAsLegacyUnknown(t *testing.T) {
	srv := &Server{}
	attr := captureInitiatorAttribution(context.Background())
	assert.Equal(t, store.InitiatorAttribution{}, attr)

	initiator, err := srv.scheduledInitiator(context.Background(), attr)
	require.NoError(t, err)
	assert.True(t, initiator.LegacyUnknown)
}

// TestInitiatorCredentialKindFor pins the committed
// session|uat|agent|legacy_unknown domain (rulings "E.2b field names"): every
// hub.CredentialKind maps to one of "uat" or "agent"; anything else is
// recorded as "session".
func TestInitiatorCredentialKindFor(t *testing.T) {
	cases := []struct {
		kind CredentialKind
		want string
	}{
		{CredentialKindUAT, store.InitiatorCredentialKindUAT},
		{CredentialKindAgentJWT, store.InitiatorCredentialKindAgent},
		{CredentialKindInteractive, store.InitiatorCredentialKindSession},
		{CredentialKindDev, store.InitiatorCredentialKindSession},
		{CredentialKindFederation, store.InitiatorCredentialKindSession},
		{CredentialKindBroker, store.InitiatorCredentialKindSession},
		{CredentialKind(""), store.InitiatorCredentialKindSession},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, initiatorCredentialKindFor(tc.kind), "kind=%q", tc.kind)
	}
}

// setBrokerDispatchInitiator (path F) --------------------------------------

func TestSetBrokerDispatchInitiator(t *testing.T) {
	identity := NewAuthenticatedUser(tid("e2b-bd-user"), "bd-user@test.com", "BD User", "member", "api")
	ctx := contextWithIdentity(context.Background(), identity)
	ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(identity))
	ctx = logging.ContextWithRequestMeta(ctx, &logging.RequestMeta{RequestID: "e2b-corr-123"})

	d := &store.BrokerDispatch{}
	setBrokerDispatchInitiator(ctx, d)

	assert.Equal(t, "user", d.InitiatorPrincipalKind)
	assert.Equal(t, identity.ID(), d.InitiatorPrincipalID)
	assert.Equal(t, store.InitiatorCredentialKindSession, d.InitiatorCredentialKind)
	assert.Equal(t, "e2b-corr-123", d.CorrelationID)
}

// TestBrokerDispatch_StoreRoundTripCarriesInitiatorAndCorrelation covers the
// plan's required test: "broker dispatch rows carry the initiator plus
// correlation_id."
func TestBrokerDispatch_StoreRoundTripCarriesInitiatorAndCorrelation(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	d := &store.BrokerDispatch{
		ID:                      tid("e2b-bd-1"),
		BrokerID:                tid("e2b-bd-broker"),
		Op:                      "restart",
		InitiatorPrincipalKind:  "user",
		InitiatorPrincipalID:    tid("e2b-bd-user"),
		InitiatorCredentialKind: store.InitiatorCredentialKindUAT,
		InitiatorCredentialID:   tid("e2b-bd-token"),
		CorrelationID:           "e2b-corr-xyz",
	}
	require.NoError(t, s.InsertBrokerDispatch(ctx, d))

	got, err := s.GetBrokerDispatch(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, "user", got.InitiatorPrincipalKind)
	assert.Equal(t, tid("e2b-bd-user"), got.InitiatorPrincipalID)
	assert.Equal(t, store.InitiatorCredentialKindUAT, got.InitiatorCredentialKind)
	assert.Equal(t, tid("e2b-bd-token"), got.InitiatorCredentialID)
	assert.Equal(t, "e2b-corr-xyz", got.CorrelationID)
}

// TestInitiatorCredentialSnapshotJSON_BoundedAndSanitized pins reuse of
// E.1/E.2a's sanitization primitives for the snapshot column.
func TestInitiatorCredentialSnapshotJSON_BoundedAndSanitized(t *testing.T) {
	cred := CredentialContext{
		Kind: CredentialKindUAT,
		ID:   "tok-1",
		Decoration: &CredentialDecoration{
			Kind:      CredentialKindUAT,
			TokenID:   "tok-1",
			TokenName: "my-token\x00", // control char must be sanitized
			Purpose:   "nightly job",
			Labels:    map[string]string{"team": "infra"},
		},
	}
	snapshotJSON := initiatorCredentialSnapshotJSON(cred)
	require.NotEmpty(t, snapshotJSON)
	assert.NotContains(t, snapshotJSON, "\x00")

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(snapshotJSON), &decoded))
	assert.Equal(t, "nightly job", decoded["purpose"])

	// No decoration (e.g. session/agent credential) yields no snapshot.
	assert.Empty(t, initiatorCredentialSnapshotJSON(CredentialContext{Kind: CredentialKindInteractive}))
}
