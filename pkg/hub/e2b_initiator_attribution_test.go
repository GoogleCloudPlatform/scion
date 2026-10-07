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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/go-jose/go-jose/v4/jwt"
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
// (c): a row written before E.2b (AttributionVersion 0/NULL) must read
// InitiatorCredentialKind as the explicit
// store.InitiatorCredentialKindLegacyUnknown at the STORE level (not merely
// through the scheduledInitiator helper), and scheduledInitiator must in
// turn treat it as LegacyUnknown with every other field cleared, never as an
// interactive credential.
func TestScheduledInitiator_LegacyRowReadsAsLegacyUnknown(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("e2b-legacy-p"), Name: "p", Slug: "e2b-legacy-p", CreatedBy: DevUserID, OwnerID: DevUserID}
	require.NoError(t, s.CreateProject(ctx, project))

	t.Run("scheduled event", func(t *testing.T) {
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
		// The adapter itself, not just scheduledInitiator, must surface
		// legacy_unknown rather than "".
		assert.Equal(t, store.InitiatorCredentialKindLegacyUnknown, got.InitiatorCredentialKind)
		assert.Empty(t, got.InitiatorPrincipalKind)
		assert.Empty(t, got.InitiatorPrincipalID)

		initiator := srv.scheduledInitiator(got.InitiatorAttribution)
		assert.True(t, initiator.LegacyUnknown)
		assert.Equal(t, store.InitiatorCredentialKindLegacyUnknown, initiator.CredentialKind)
		assert.Empty(t, initiator.PrincipalKind)
		assert.Empty(t, initiator.CredentialID)
		assert.NotEqual(t, string(CredentialKindInteractive), initiator.CredentialKind,
			"a legacy row must never be reported as an interactive credential")
	})

	t.Run("schedule", func(t *testing.T) {
		sched := &store.Schedule{
			ID:        tid("e2b-legacy-sched"),
			ProjectID: project.ID,
			Name:      "legacy-sched",
			CronExpr:  "0 9 * * *",
			EventType: "message",
			CreatedBy: DevUserID,
		}
		require.NoError(t, s.CreateSchedule(ctx, sched))

		got, err := s.GetSchedule(ctx, sched.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, got.AttributionVersion)
		assert.Equal(t, store.InitiatorCredentialKindLegacyUnknown, got.InitiatorCredentialKind)

		initiator := srv.scheduledInitiator(got.InitiatorAttribution)
		assert.True(t, initiator.LegacyUnknown)
	})

	// A bare zero-value InitiatorAttribution (no store round-trip at all)
	// must also read as legacy_unknown.
	assert.True(t, srv.scheduledInitiator(store.InitiatorAttribution{}).LegacyUnknown)
}

// TestScheduledInitiator_CredentialKindLegacyUnknownClearsEvenWithVersion
// covers the case where a row has a real AttributionVersion (it was
// genuinely captured by E.2b) yet still has InitiatorCredentialKind
// legacy_unknown (no recordable provenance at capture time, e.g. a dev or
// federated credential). scheduledInitiator must treat this exactly like a
// version-0 row: LegacyUnknown, every other field cleared.
func TestScheduledInitiator_CredentialKindLegacyUnknownClearsEvenWithVersion(t *testing.T) {
	srv := &Server{}
	attr := store.InitiatorAttribution{
		InitiatorPrincipalKind:  "dev",
		InitiatorPrincipalID:    "some-dev-user",
		InitiatorCredentialKind: store.InitiatorCredentialKindLegacyUnknown,
		AttributionVersion:      1,
		AuthorizationRevision:   1,
	}
	initiator := srv.scheduledInitiator(attr)
	assert.True(t, initiator.LegacyUnknown)
	assert.Equal(t, store.InitiatorCredentialKindLegacyUnknown, initiator.CredentialKind)
	assert.Empty(t, initiator.PrincipalKind, "no field may leak when the credential kind is legacy_unknown")
	assert.Empty(t, initiator.PrincipalID)
}

// TestCreateScheduledEvent_CapturesInitiatorAttribution covers plan §3.5's
// one-shot create row: the authoring request's identity/credential are
// captured atomically with the event row (design check (a): a single
// CreateScheduledEvent insert). Assertions read the row from the store: the
// wire response never carries these fields.
func TestCreateScheduledEvent_CapturesInitiatorAttribution(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)

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
	require.NotEmpty(t, created.ID)

	stored, err := s.GetScheduledEvent(context.Background(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, stored.AttributionVersion)
	assert.Equal(t, 1, stored.AuthorizationRevision)
	assert.Equal(t, "dev", stored.InitiatorPrincipalKind) // doRequest authenticates via the dev-auth identity
	assert.Equal(t, DevUserID, stored.InitiatorPrincipalID)
	// doRequest authenticates through the real DevAuthMiddleware/
	// UnifiedAuthMiddleware dev-token arm, producing the concrete trusted
	// *DevUser with ID()==DevUserID (ptone/scion#2342): it is recorded as
	// dev_local, never legacy_unknown or session.
	assert.Equal(t, store.InitiatorCredentialKindDevLocal, stored.InitiatorCredentialKind)
}

// TestCreateSchedule_CapturesInitiatorAttribution mirrors the scheduled-event
// case for the recurring-schedule create path.
func TestCreateSchedule_CapturesInitiatorAttribution(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)

	req := CreateScheduleRequest{
		Name:      "daily",
		CronExpr:  "0 9 * * *",
		EventType: "message",
		AgentName: "all",
		Message:   "status please",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	require.NotEmpty(t, created.ID)

	stored, err := s.GetSchedule(context.Background(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, stored.AttributionVersion)
	assert.Equal(t, 1, stored.AuthorizationRevision)
	assert.Equal(t, "dev", stored.InitiatorPrincipalKind)
	assert.Equal(t, DevUserID, stored.InitiatorPrincipalID)
	// See TestCreateScheduledEvent_CapturesInitiatorAttribution: the
	// dev-auth identity behind doRequest is the concrete trusted *DevUser,
	// so this records dev_local (ptone/scion#2342).
	assert.Equal(t, store.InitiatorCredentialKindDevLocal, stored.InitiatorCredentialKind)
}

// TestUpdateSchedule_AuthorityChangingEditByDevUserRecordsDevLocal covers
// ptone/scion#2342's authority-changing-edit acceptance criterion end to
// end through the real DevAuthMiddleware/UnifiedAuthMiddleware dev-token
// arm (via doRequest), not only the captureInitiatorAttribution helper: a
// payload-changing PATCH re-attributes to the concrete trusted *DevUser and
// bumps AuthorizationRevision.
func TestUpdateSchedule_AuthorityChangingEditByDevUserRecordsDevLocal(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "p1"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	// A payload change is a future-dispatch-changing edit (ruling Q2): it
	// re-attributes and bumps the revision.
	updateReq := UpdateScheduleRequest{Payload: `{"agentName":"all","message":"p2"}`}
	rec2 := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+created.ID, updateReq)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	stored, err := s.GetSchedule(context.Background(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, stored.AuthorizationRevision)
	assert.Equal(t, "dev", stored.InitiatorPrincipalKind)
	assert.Equal(t, DevUserID, stored.InitiatorPrincipalID)
	assert.Equal(t, store.InitiatorCredentialKindDevLocal, stored.InitiatorCredentialKind)
}

// TestResumeSchedule_ByDevUserRecordsDevLocal covers ptone/scion#2342's
// resume acceptance criterion end to end through the real
// DevAuthMiddleware/UnifiedAuthMiddleware dev-token arm (via doRequest):
// resuming a paused schedule re-attributes to the concrete trusted *DevUser.
func TestResumeSchedule_ByDevUserRecordsDevLocal(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	pauseRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/pause", nil)
	require.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())

	resumeRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/resume", nil)
	require.Equal(t, http.StatusOK, resumeRec.Code, resumeRec.Body.String())

	stored, err := s.GetSchedule(context.Background(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, stored.AuthorizationRevision)
	assert.Equal(t, "dev", stored.InitiatorPrincipalKind)
	assert.Equal(t, DevUserID, stored.InitiatorPrincipalID)
	assert.Equal(t, store.InitiatorCredentialKindDevLocal, stored.InitiatorCredentialKind)
}

// TestExecuteSchedule_RecurrenceCopiesInitiatorAttribution pins plan §3.5's
// recurrence row: server.go's executeSchedule copies the schedule's current
// InitiatorAttribution verbatim onto each materialized event. Table-driven
// over uat and dev_local (ptone/scion#2342 review round 1, finding 2): the
// dev_local case is the acceptance-criterion pin that a dev_local schedule's
// kind and revision round-trip to its materialized event unchanged, and
// that scheduledInitiator reads the copied row back as dev_local, never
// legacy_unknown — a regression here (e.g. a future "normalize unknown
// kinds" change in the entadapter read path or in scheduledInitiator) would
// otherwise go unnoticed.
func TestExecuteSchedule_RecurrenceCopiesInitiatorAttribution(t *testing.T) {
	cases := []struct {
		name  string
		attrs store.InitiatorAttribution
	}{
		{
			name: "uat",
			attrs: store.InitiatorAttribution{
				InitiatorPrincipalKind:      "user",
				InitiatorPrincipalID:        tid("e2b-recur-uat-user"),
				InitiatorCredentialKind:     store.InitiatorCredentialKindUAT,
				InitiatorCredentialID:       tid("e2b-recur-uat-token"),
				InitiatorCredentialSnapshot: `{"name":"recur-token"}`,
				AttributionVersion:          1,
				AuthorizationRevision:       3,
			},
		},
		{
			name: "dev_local",
			attrs: store.InitiatorAttribution{
				InitiatorPrincipalKind:  "dev",
				InitiatorPrincipalID:    DevUserID,
				InitiatorCredentialKind: store.InitiatorCredentialKindDevLocal,
				AttributionVersion:      1,
				AuthorizationRevision:   4, // a revision other than 1, per the review's fix
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, projectID := setupScheduleTest(t)
			ctx := context.Background()

			sched := &store.Schedule{
				ID:                   tid("e2b-recur-sched-" + tc.name),
				ProjectID:            projectID,
				Name:                 "recur-" + tc.name,
				CronExpr:             "0 9 * * *",
				EventType:            "message",
				Payload:              `{"agentName":"a","message":"hi"}`,
				Status:               store.ScheduleStatusActive,
				CreatedBy:            DevUserID,
				InitiatorAttribution: tc.attrs,
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

			if tc.name == "dev_local" {
				assert.Equal(t, store.InitiatorCredentialKindDevLocal, evt.InitiatorCredentialKind)
				initiator := srv.scheduledInitiator(evt.InitiatorAttribution)
				assert.False(t, initiator.LegacyUnknown, "a dev_local row must never read as legacy_unknown")
				assert.Equal(t, store.InitiatorCredentialKindDevLocal, initiator.CredentialKind)
			}
		})
	}
}

// TestSchedulerFireEvent_RestartReplayPreservesInitiatorAttribution pins plan
// §3.5's restart-replay row: firing an overdue persisted event (the
// wasExpired=true path loadPersistedTimers uses) is read-only with respect
// to attribution.
// Table-driven over uat and dev_local (ptone/scion#2342 review round 1,
// finding 2's optional restart-replay case, made mandatory by the lead's
// ruling): a restart-replayed dev_local row must survive unchanged, exactly
// like every other kind.
func TestSchedulerFireEvent_RestartReplayPreservesInitiatorAttribution(t *testing.T) {
	cases := []struct {
		name  string
		attrs store.InitiatorAttribution
	}{
		{
			name: "uat",
			attrs: store.InitiatorAttribution{
				InitiatorPrincipalKind:  "user",
				InitiatorPrincipalID:    tid("e2b-replay-uat-user"),
				InitiatorCredentialKind: store.InitiatorCredentialKindUAT,
				InitiatorCredentialID:   tid("e2b-replay-uat-token"),
				AttributionVersion:      1,
				AuthorizationRevision:   1,
			},
		},
		{
			name: "dev_local",
			attrs: store.InitiatorAttribution{
				InitiatorPrincipalKind:  "dev",
				InitiatorPrincipalID:    DevUserID,
				InitiatorCredentialKind: store.InitiatorCredentialKindDevLocal,
				AttributionVersion:      1,
				AuthorizationRevision:   1,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, projectID := setupScheduleTest(t)
			ctx := context.Background()

			evt := store.ScheduledEvent{
				ID:                   tid("e2b-replay-evt-" + tc.name),
				ProjectID:            projectID,
				EventType:            "message",
				FireAt:               time.Now().Add(-time.Hour), // overdue
				Payload:              `{"agentName":"nonexistent","message":"hi"}`,
				CreatedBy:            DevUserID,
				InitiatorAttribution: tc.attrs,
			}
			require.NoError(t, s.CreateScheduledEvent(ctx, &evt))

			// Exercise the same fireEvent(wasExpired=true) path
			// loadPersistedTimers uses for overdue events, synchronously
			// (loadPersistedTimers itself wraps this in `go`, which is a
			// concurrency detail, not a behavioral one).
			srv.scheduler.fireEvent(ctx, evt, true)

			got, err := s.GetScheduledEvent(ctx, evt.ID)
			require.NoError(t, err)
			assert.Equal(t, evt.InitiatorPrincipalKind, got.InitiatorPrincipalKind)
			assert.Equal(t, evt.InitiatorPrincipalID, got.InitiatorPrincipalID)
			assert.Equal(t, evt.InitiatorCredentialKind, got.InitiatorCredentialKind)
			assert.Equal(t, evt.InitiatorCredentialID, got.InitiatorCredentialID)
			assert.Equal(t, evt.AttributionVersion, got.AttributionVersion)
			// The target agent doesn't exist, so the handler errors and
			// fireEvent's existing (pre-E.2b) behavior downgrades the
			// wasExpired-derived "expired" status to "failed" — status
			// handling is unchanged by E.2b; this test only asserts
			// attribution survives the replay.
			assert.Equal(t, store.ScheduledEventFailed, got.Status)

			if tc.name == "dev_local" {
				assert.Equal(t, store.InitiatorCredentialKindDevLocal, got.InitiatorCredentialKind)
				assert.False(t, srv.scheduledInitiator(got.InitiatorAttribution).LegacyUnknown)
			}
		})
	}
}

// e2bFailingScheduleStore injects a store failure into CreateScheduledEvent,
// CreateSchedule, or UpdateSchedule, to prove that a failed authoring or
// mutation write leaves neither the row nor a partial attribution behind,
// and that a failed resume is reported as an error rather than a false 200.
type e2bFailingScheduleStore struct {
	store.Store
	fault                   *storeFaultSwitch // nil: always active
	createScheduledEventErr error
	createScheduleErr       error
	updateScheduleErr       error
}

func (f *e2bFailingScheduleStore) CreateScheduledEvent(ctx context.Context, evt *store.ScheduledEvent) error {
	if f.createScheduledEventErr != nil && f.fault.Active() {
		return f.createScheduledEventErr
	}
	return f.Store.CreateScheduledEvent(ctx, evt)
}

func (f *e2bFailingScheduleStore) CreateSchedule(ctx context.Context, sc *store.Schedule) error {
	if f.createScheduleErr != nil && f.fault.Active() {
		return f.createScheduleErr
	}
	return f.Store.CreateSchedule(ctx, sc)
}

func (f *e2bFailingScheduleStore) UpdateSchedule(
	ctx context.Context, sc *store.Schedule, fields store.ScheduleFieldMask,
	prevRevision int, prevRevisionKnown bool, attribution *store.InitiatorAttribution,
) error {
	if f.updateScheduleErr != nil && f.fault.Active() {
		return f.updateScheduleErr
	}
	return f.Store.UpdateSchedule(ctx, sc, fields, prevRevision, prevRevisionKnown, attribution)
}

// TestCreateScheduledEvent_StoreFailureLeavesNoPartialAttribution covers the
// transaction-guarantee test the plan requires: an injected store failure
// during authoring leaves neither the event row nor a partial attribution.
func TestCreateScheduledEvent_StoreFailureLeavesNoPartialAttribution(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	fs := &e2bFailingScheduleStore{Store: s, createScheduledEventErr: errors.New("initiator attribution test: injected scheduled event insert failure")}
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

// TestCreateSchedule_StoreFailureLeavesNoPartialAttribution is O2's schedule
// -create counterpart to the scheduled-event test above.
func TestCreateSchedule_StoreFailureLeavesNoPartialAttribution(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	fs := &e2bFailingScheduleStore{Store: s, createScheduleErr: errors.New("initiator attribution test: injected schedule insert failure")}
	srv.store = fs
	defer func() { srv.store = s }()

	req := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", req)
	assert.NotEqual(t, http.StatusCreated, rec.Code)

	result, err := s.ListSchedules(context.Background(), store.ScheduleFilter{ProjectID: projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.Items, "a failed create must leave no row, partial or otherwise")
}

// TestResumeSchedule_UpdateFailureReturnsErrorNotSuccess covers resume's
// single UpdateSchedule write (status, next_run_at and attribution
// together): when it fails, the handler must report an error, never a 200
// naming an attribution that was never persisted.
func TestResumeSchedule_UpdateFailureReturnsErrorNotSuccess(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	// Installed before the pause below, whose mutation audit goroutine reads
	// srv.store (ptone/scion#3184); armed for the resume.
	_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *e2bFailingScheduleStore {
		return &e2bFailingScheduleStore{Store: inner, fault: f, updateScheduleErr: errors.New("initiator attribution test: injected resume update failure")}
	})

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code)
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	pauseRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/pause", nil)
	require.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())

	fault.Arm()

	resumeRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/resume", nil)
	assert.NotEqual(t, http.StatusOK, resumeRec.Code, "a failed resume write must not report success")

	stored, err := s.GetSchedule(context.Background(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusPaused, stored.Status, "a failed resume write must not leave the schedule active")
	assert.Equal(t, 1, stored.AuthorizationRevision, "a failed resume write must not bump the revision")
}

// TestUpdateSchedule_StaleRevisionReturnsConflict: a write built from a
// stale read (an old authorization_revision) must be rejected rather than
// silently reverting a concurrent re-attribution, even when the write itself
// does not touch attribution (attribution == nil) — the revision check
// applies to every write, not only ones that replace attribution.
func TestUpdateSchedule_StaleRevisionReturnsConflict(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code)
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	// A concurrent actor pauses then resumes the schedule, bumping the
	// revision to 2.
	pauseRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/pause", nil)
	require.Equal(t, http.StatusOK, pauseRec.Code)
	resumeRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/resume", nil)
	require.Equal(t, http.StatusOK, resumeRec.Code)

	current, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, 2, current.AuthorizationRevision)

	// Now a write built from a STALE read (prevRevision=1, as if it had been
	// read before the pause/resume above) must be rejected, even though this
	// write carries no attribution replacement of its own.
	current.CronExpr = "0 11 * * *"
	err = s.UpdateSchedule(ctx, current, store.ScheduleFieldMask{CronExpr: true}, 1, true, nil)
	assert.ErrorIs(t, err, store.ErrRevisionConflict)

	after, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, after.AuthorizationRevision, "the concurrent re-attribution must survive the stale write attempt")
	assert.NotEqual(t, "0 11 * * *", after.CronExpr, "a rejected conditional write must not apply any of its fields")
}

// TestUpdateSchedule_MetadataOnlyStaleWriteAfterReattributionReturnsConflict
// reproduces the scenario where a name-only edit is built from a read taken
// BEFORE a payload-changing re-attribution lands. The stale write must be
// rejected outright: it must never apply the old payload under the new
// attribution, and it must not apply its own field (the rename) either, once
// rejected.
func TestUpdateSchedule_MetadataOnlyStaleWriteAfterReattributionReturnsConflict(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "p1"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code)
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	// The stale read: taken before any re-attribution.
	staleRead, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	originalPayload := staleRead.Payload

	// A payload-changing update lands, re-attributing to revision 2.
	updateReq := UpdateScheduleRequest{Payload: `{"agentName":"all","message":"p2"}`}
	rec2 := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+created.ID, updateReq)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	afterReattribution, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, 2, afterReattribution.AuthorizationRevision)
	require.NotEqual(t, originalPayload, afterReattribution.Payload)

	// The stale name-only write, built from staleRead (revision 1), must be
	// rejected rather than silently applied.
	staleRead.Name = "renamed-from-stale-read"
	err = s.UpdateSchedule(ctx, staleRead, store.ScheduleFieldMask{Name: true},
		staleRead.AuthorizationRevision, staleRead.AuthorizationRevision != 0, nil)
	assert.ErrorIs(t, err, store.ErrRevisionConflict)

	final, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, afterReattribution.Payload, final.Payload, "the payload must never revert to the stale value")
	assert.Equal(t, afterReattribution.InitiatorPrincipalID, final.InitiatorPrincipalID,
		"the attribution must never end up paired with the stale payload")
	assert.NotEqual(t, "renamed-from-stale-read", final.Name, "a rejected conditional write must not apply any field")

	// A stale status-only write, built from the same pre-re-attribution read,
	// must be rejected the same way — status is protected by both its own
	// field-mask flag and the revision check, and nothing exercises that
	// combination above.
	staleRead.Status = store.ScheduleStatusPaused
	err = s.UpdateSchedule(ctx, staleRead, store.ScheduleFieldMask{Status: true},
		staleRead.AuthorizationRevision, staleRead.AuthorizationRevision != 0, nil)
	assert.ErrorIs(t, err, store.ErrRevisionConflict)

	afterStaleStatus, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, afterReattribution.Status, afterStaleStatus.Status, "a rejected stale status write must leave status unchanged")
}

// TestUpdateSchedule_RevisionConflictMapsTo409 covers the handler side: the
// store's ErrRevisionConflict must map to an HTTP 409, not a generic 500.
func TestUpdateSchedule_RevisionConflictMapsTo409(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code)
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	fs := &e2bFailingScheduleStore{Store: s, updateScheduleErr: store.ErrRevisionConflict}
	srv.store = fs
	defer func() { srv.store = s }()

	updateReq := UpdateScheduleRequest{CronExpr: "0 12 * * *"}
	rec2 := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+created.ID, updateReq)
	assert.Equal(t, http.StatusConflict, rec2.Code, rec2.Body.String())
}

// setupTwoScheduleUsers creates two real, project-owner-privileged users so
// R7's tests can exercise "creator A, mutator B" with genuine, distinct
// session identities (via doRequestAsUser) rather than the single shared
// dev-auth identity every other test in this file uses.
func setupTwoScheduleUsers(t *testing.T, srv *Server, s store.Store, projectID string) (userA, userB *store.User) {
	t.Helper()
	ctx := context.Background()

	userA = &store.User{ID: tid("e2b-r7-user-a"), Email: "e2b-r7-a@test.com", DisplayName: "A", Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}
	userB = &store.User{ID: tid("e2b-r7-user-b"), Email: "e2b-r7-b@test.com", DisplayName: "B", Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now()}
	require.NoError(t, s.CreateUser(ctx, userA))
	require.NoError(t, s.CreateUser(ctx, userB))

	project, err := s.GetProject(ctx, projectID)
	require.NoError(t, err)
	srv.seedProjectCreatorMembership(ctx, project)
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, projectID, userA.ID))
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, projectID, userB.ID))
	return userA, userB
}

// TestUpdateSchedule_MetadataOnlyDoesNotReattribute uses two distinct real
// users as creator and mutator, so the assertion that the initiator is
// unchanged is not trivially true (a shared identity would make the "before"
// and "after" initiator look the same regardless of whether re-attribution
// happened).
func TestUpdateSchedule_MetadataOnlyDoesNotReattribute(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	userA, userB := setupTwoScheduleUsers(t, srv, s, projectID)
	ctx := context.Background()

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	rec := doRequestAsUser(t, srv, userA, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	before, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, userA.ID, before.InitiatorPrincipalID)
	require.Equal(t, 1, before.AuthorizationRevision)

	updateReq := UpdateScheduleRequest{Name: "n2"}
	rec2 := doRequestAsUser(t, srv, userB, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+created.ID, updateReq)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	after, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "n2", after.Name)
	assert.Equal(t, 1, after.AuthorizationRevision, "a metadata-only edit must not bump the revision")
	assert.Equal(t, userA.ID, after.InitiatorPrincipalID,
		"a metadata-only edit by a DIFFERENT user must not change the initiator")
}

// TestUpdateSchedule_ReattributesToMutatorNotCreator: a future-dispatch-
// changing update replaces the FULL InitiatorAttribution with the mutator's,
// not the creator's — and CreatedBy stays the creator.
func TestUpdateSchedule_ReattributesToMutatorNotCreator(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	userA, userB := setupTwoScheduleUsers(t, srv, s, projectID)
	ctx := context.Background()

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	rec := doRequestAsUser(t, srv, userA, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	before, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "user", before.InitiatorPrincipalKind)
	assert.Equal(t, userA.ID, before.InitiatorPrincipalID)
	assert.Equal(t, store.InitiatorCredentialKindSession, before.InitiatorCredentialKind)
	assert.Equal(t, userA.ID, before.CreatedBy)

	updateReq := UpdateScheduleRequest{CronExpr: "0 10 * * *"}
	rec2 := doRequestAsUser(t, srv, userB, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+created.ID, updateReq)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	after, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "0 10 * * *", after.CronExpr)
	assert.Equal(t, "user", after.InitiatorPrincipalKind)
	assert.Equal(t, userB.ID, after.InitiatorPrincipalID, "attribution must move to the mutator, not stay with the creator")
	assert.Equal(t, store.InitiatorCredentialKindSession, after.InitiatorCredentialKind)
	assert.Equal(t, 2, after.AuthorizationRevision, "a timing change must bump the revision")
	assert.Equal(t, userA.ID, after.CreatedBy, "CreatedBy must never be touched by a re-attribution")
}

// TestUpdateSchedule_UnchangedDispatchFieldsDoNotReattribute is T2's
// regression test: resending the CronExpr/EventType/Payload the schedule
// already has (e.g. a client round-tripping the full resource on every
// PATCH) is metadata-only. It must not re-attribute or bump the revision,
// even though all three "changes future dispatch" fields are present in the
// request — presence alone is not a change.
func TestUpdateSchedule_UnchangedDispatchFieldsDoNotReattribute(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	userA, userB := setupTwoScheduleUsers(t, srv, s, projectID)
	ctx := context.Background()

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	rec := doRequestAsUser(t, srv, userA, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	before, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, userA.ID, before.InitiatorPrincipalID)
	require.Equal(t, 1, before.AuthorizationRevision)

	// Resend the same CronExpr, EventType, and Payload the schedule already
	// has — all present, none actually different.
	updateReq := UpdateScheduleRequest{
		CronExpr:  before.CronExpr,
		EventType: before.EventType,
		Payload:   before.Payload,
	}
	rec2 := doRequestAsUser(t, srv, userB, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+created.ID, updateReq)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	after, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, before.CronExpr, after.CronExpr)
	assert.Equal(t, before.EventType, after.EventType)
	assert.Equal(t, before.Payload, after.Payload)
	assert.Equal(t, 1, after.AuthorizationRevision, "resending unchanged dispatch fields must not bump the revision")
	assert.Equal(t, userA.ID, after.InitiatorPrincipalID,
		"resending unchanged dispatch fields by a DIFFERENT user must not change the initiator")
}

// TestUpdateSchedule_PayloadChangeReattributes is the payload counterpart to
// TestUpdateSchedule_ReattributesToMutatorNotCreator (which covers CronExpr):
// an actual payload change still re-attributes to the mutator and bumps the
// revision, even though the field-mask/reattribution decision is now driven
// by an equality check rather than mere presence.
func TestUpdateSchedule_PayloadChangeReattributes(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	userA, userB := setupTwoScheduleUsers(t, srv, s, projectID)
	ctx := context.Background()

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	rec := doRequestAsUser(t, srv, userA, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	before, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, 1, before.AuthorizationRevision)

	newPayloadBytes, err := json.Marshal(MessageEventPayload{AgentName: "all", Message: "bye"})
	require.NoError(t, err)
	updateReq := UpdateScheduleRequest{Payload: string(newPayloadBytes)}
	rec2 := doRequestAsUser(t, srv, userB, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+created.ID, updateReq)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	after, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, string(newPayloadBytes), after.Payload)
	assert.Equal(t, userB.ID, after.InitiatorPrincipalID, "an actual payload change must re-attribute to the mutator")
	assert.Equal(t, 2, after.AuthorizationRevision, "an actual payload change must bump the revision")
}

// TestUpdateSchedule_UnchangedCronEnableRefreshesNextRunAt covers a PATCH
// that resends the schedule's own CronExpr alongside status:"active" on a
// paused schedule. Because the cron itself doesn't change, that PATCH only
// reaches the field mask through the enable transition — but the enable
// transition must still recompute NextRunAt the way resumeSchedule does.
// Without that, a schedule that went stale (or was already due) while
// paused reactivates carrying its stale next_run_at, and the scheduler
// treats it as immediately due.
func TestUpdateSchedule_UnchangedCronEnableRefreshesNextRunAt(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	userA, userB := setupTwoScheduleUsers(t, srv, s, projectID)
	ctx := context.Background()

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	rec := doRequestAsUser(t, srv, userA, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	pauseRec := doRequestAsUser(t, srv, userA, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/pause", nil)
	require.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())

	// pauseSchedule leaves next_run_at untouched; simulate one that has gone
	// stale (or was already due) while paused, the same way the reviewer's
	// repro did.
	paused, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	staleNextRunAt := time.Now().UTC().Add(-48 * time.Hour)
	paused.NextRunAt = &staleNextRunAt
	require.NoError(t, s.UpdateSchedule(ctx, paused, store.ScheduleFieldMask{NextRunAt: true},
		paused.AuthorizationRevision, paused.AuthorizationRevision != 0, nil))

	before, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, store.ScheduleStatusPaused, before.Status)
	require.WithinDuration(t, staleNextRunAt, *before.NextRunAt, time.Second)

	// B resends the schedule's own CronExpr and flips status back to active
	// in one PATCH — the round-trip case the fix covers.
	updateReq := UpdateScheduleRequest{CronExpr: before.CronExpr, Status: store.ScheduleStatusActive}
	rec2 := doRequestAsUser(t, srv, userB, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+created.ID, updateReq)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	after, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusActive, after.Status)
	require.NotNil(t, after.NextRunAt)
	assert.True(t, after.NextRunAt.After(time.Now().UTC()),
		"an unchanged-cron enable must recompute next_run_at, not leave the stale paused value: got %v", after.NextRunAt)
	assert.Equal(t, 2, after.AuthorizationRevision, "an enable transition must bump the revision")
	assert.Equal(t, userB.ID, after.InitiatorPrincipalID, "an enable transition must attribute to the resumer/mutator")

	due, err := s.ListDueSchedules(ctx, time.Now().UTC())
	require.NoError(t, err)
	for _, d := range due {
		assert.NotEqual(t, created.ID, d.ID, "the reactivated schedule must not be immediately due right after the fix")
	}
}

// TestResumeSchedule_ReattributesToResumer is R7's resume counterpart.
func TestResumeSchedule_ReattributesToResumer(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	userA, userB := setupTwoScheduleUsers(t, srv, s, projectID)
	ctx := context.Background()

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	rec := doRequestAsUser(t, srv, userA, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	pauseRec := doRequestAsUser(t, srv, userA, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/pause", nil)
	require.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())

	resumeRec := doRequestAsUser(t, srv, userB, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/resume", nil)
	require.Equal(t, http.StatusOK, resumeRec.Code, resumeRec.Body.String())

	after, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusActive, after.Status)
	assert.Equal(t, 2, after.AuthorizationRevision, "resume must bump the revision")
	assert.Equal(t, userB.ID, after.InitiatorPrincipalID, "resume must attribute to the resumer")
	assert.Equal(t, userA.ID, after.CreatedBy, "CreatedBy must never be touched by a re-attribution")
}

// TestResumeSchedule_RevisionSetButVersionNilSucceeds covers a schedule
// whose authorization_revision is set but attribution_version is NULL (a
// row read as legacy by version, but which nonetheless carries a revision
// value). Resume must still succeed and bump the revision: the write's
// "known previous revision" predicate must come from AuthorizationRevision
// itself, not be derived from AttributionVersion, or this combination can
// never satisfy either the EQ or the IS NULL branch and every re-attributing
// write permanently conflicts.
func TestResumeSchedule_RevisionSetButVersionNilSucceeds(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	sched := &store.Schedule{
		ID:        tid("e2b-p7-sched"),
		ProjectID: projectID,
		Name:      "p7",
		CronExpr:  "0 9 * * *",
		EventType: "message",
		Payload:   `{"agentName":"all","message":"hi"}`,
		Status:    store.ScheduleStatusPaused,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateSchedule(ctx, sched))

	// Give the row authorization_revision=1 while leaving attribution_version
	// NULL, via a direct conditional write against the freshly created
	// (legacy, revision-less) row.
	require.NoError(t, s.UpdateSchedule(ctx, sched, store.ScheduleFieldMask{}, 0, false,
		&store.InitiatorAttribution{AuthorizationRevision: 1}))

	stored, err := s.GetSchedule(ctx, sched.ID)
	require.NoError(t, err)
	require.Equal(t, 0, stored.AttributionVersion)
	require.Equal(t, 1, stored.AuthorizationRevision)

	resumeRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+sched.ID+"/resume", nil)
	require.Equal(t, http.StatusOK, resumeRec.Code, resumeRec.Body.String())

	after, err := s.GetSchedule(ctx, sched.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusActive, after.Status)
	assert.Equal(t, 2, after.AuthorizationRevision, "resume must succeed and bump the revision even when attribution_version was NULL")
}

// TestUpdateSchedule_LegacyRowMetadataOnlyEditDoesNotAcquireDevLocal covers
// ptone/scion#2342's exclusion: a pre-E.2b legacy_unknown/version-0 row must
// not acquire dev_local (or any kind) merely by being read or
// metadata-edited by the trusted dev user — no backfill, no
// reinterpretation of legacy rows. Only an authority-changing edit (ruling
// Q2) ever replaces attribution, and this edit is deliberately
// metadata-only (a rename).
func TestUpdateSchedule_LegacyRowMetadataOnlyEditDoesNotAcquireDevLocal(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	sched := &store.Schedule{
		ID:        tid("e2b-2342-legacy-sched"),
		ProjectID: projectID,
		Name:      "legacy",
		CronExpr:  "0 9 * * *",
		EventType: "message",
		Payload:   `{"agentName":"all","message":"hi"}`,
		Status:    store.ScheduleStatusActive,
		CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateSchedule(ctx, sched))

	before, err := s.GetSchedule(ctx, sched.ID)
	require.NoError(t, err)
	require.Equal(t, 0, before.AttributionVersion)
	require.Equal(t, store.InitiatorCredentialKindLegacyUnknown, before.InitiatorCredentialKind)

	updateReq := UpdateScheduleRequest{Name: "renamed"}
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+sched.ID, updateReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after, err := s.GetSchedule(ctx, sched.ID)
	require.NoError(t, err)
	assert.Equal(t, "renamed", after.Name)
	assert.Equal(t, 0, after.AttributionVersion, "a metadata-only edit must not touch a legacy row's attribution version")
	assert.Equal(t, store.InitiatorCredentialKindLegacyUnknown, after.InitiatorCredentialKind,
		"a legacy row must never acquire dev_local (or any kind) from a non-authority edit, even by the trusted dev user")
}

// TestScheduleAndScheduledEventResponses_OmitInitiatorAttribution: none of
// the initiator/attribution fields may appear on the wire, in create or list
// responses, for either resource.
func TestScheduleAndScheduledEventResponses_OmitInitiatorAttribution(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	scheduleReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	scheduleRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", scheduleReq)
	require.Equal(t, http.StatusCreated, scheduleRec.Code)

	eventReq := CreateScheduledEventRequest{EventType: "message", FireIn: "1h", AgentName: "nonexistent", Message: "hi"}
	eventRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", eventReq)
	require.Equal(t, http.StatusCreated, eventRec.Code)

	listSchedulesRec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/schedules", nil)
	require.Equal(t, http.StatusOK, listSchedulesRec.Code)

	listEventsRec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/scheduled-events", nil)
	require.Equal(t, http.StatusOK, listEventsRec.Code)

	forbidden := []string{
		"initiatorPrincipalKind", "initiatorPrincipalId",
		"initiatorCredentialKind", "initiatorCredentialId",
		"initiatorCredentialSnapshot", "attributionVersion", "authorizationRevision",
	}
	bodies := map[string]string{
		"schedule create":        scheduleRec.Body.String(),
		"scheduled-event create": eventRec.Body.String(),
		"schedule list":          listSchedulesRec.Body.String(),
		"scheduled-event list":   listEventsRec.Body.String(),
	}
	for label, body := range bodies {
		for _, key := range forbidden {
			assert.NotContains(t, body, key, "%s response must not expose %s", label, key)
		}
	}
}

// TestPauseSchedule_EmitsMutationAudit, TestDeleteSchedule_EmitsMutationAudit
// and TestCancelScheduledEvent_EmitsMutationAudit: pause, delete and cancel
// record the actor, with no re-attribution (there is no future dispatch left
// to attribute).
func TestPauseSchedule_EmitsMutationAudit(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code)
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	pauseRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+created.ID+"/pause", nil)
	require.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())

	require.Eventually(t, func() bool {
		records, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
			MutationType: "schedule_pause", TargetID: created.ID, Limit: 1,
		})
		return err == nil && len(records) == 1
	}, 2*time.Second, 10*time.Millisecond, "pause must emit a mutation audit record")
}

func TestDeleteSchedule_EmitsMutationAudit(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)

	createReq := CreateScheduleRequest{Name: "n1", CronExpr: "0 9 * * *", EventType: "message", AgentName: "all", Message: "hi"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", createReq)
	require.Equal(t, http.StatusCreated, rec.Code)
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	deleteRec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+projectID+"/schedules/"+created.ID, nil)
	require.Equal(t, http.StatusNoContent, deleteRec.Code)

	require.Eventually(t, func() bool {
		records, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
			MutationType: "schedule_delete", TargetID: created.ID, Limit: 1,
		})
		return err == nil && len(records) == 1
	}, 2*time.Second, 10*time.Millisecond, "delete must emit a mutation audit record")
}

func TestCancelScheduledEvent_EmitsMutationAudit(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)

	eventReq := CreateScheduledEventRequest{EventType: "message", FireIn: "1h", AgentName: "nonexistent", Message: "hi"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", eventReq)
	require.Equal(t, http.StatusCreated, rec.Code)
	var created store.ScheduledEvent
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	cancelRec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+projectID+"/scheduled-events/"+created.ID, nil)
	require.Equal(t, http.StatusNoContent, cancelRec.Code)

	require.Eventually(t, func() bool {
		records, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
			MutationType: "scheduled_event_cancel", TargetID: created.ID, Limit: 1,
		})
		return err == nil && len(records) == 1
	}, 2*time.Second, 10*time.Millisecond, "cancel must emit a mutation audit record")
}

// TestDispatchAgentFire_SuccessAuditCarriesExecutorAndPairedCredential:
// firing a dispatch_agent event via the scheduler test hook must produce a
// success mutation-audit record AND a log line, both carrying
// executor_kind=scheduler/executor_id=scheduled_event:<id>. The initiator's
// credential is copied onto the audit only when the initiator is the same
// principal as the creator — mapped back into hub.CredentialKind's
// vocabulary.
func TestDispatchAgentFire_SuccessAuditCarriesExecutorAndPairedCredential(t *testing.T) {
	f := bypassAgentsSetup(t)
	ctx := context.Background()
	f.srv.seedProjectCreatorMembership(ctx, f.proj)
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, f.proj.ID, f.owner.ID))
	f.srv.scheduler = NewScheduler(f.store, slog.Default())
	f.srv.scheduler.RegisterEventHandler("dispatch_agent", f.srv.dispatchAgentEventHandler())

	capture := &capturingHandler{}
	restoreLog := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(restoreLog) })

	waitForAudit := func(t *testing.T, agentID string) *store.MutationAuditRecord {
		t.Helper()
		var records []*store.MutationAuditRecord
		require.Eventually(t, func() bool {
			var err error
			records, _, err = f.store.ListMutationAudits(context.Background(), store.MutationAuditFilter{
				MutationType: "agent_delegation", TargetType: "agent", TargetID: agentID, Limit: 1,
			})
			return err == nil && len(records) == 1
		}, 2*time.Second, 10*time.Millisecond, "scheduled dispatch success audit not recorded")
		return records[0]
	}

	t.Run("same principal: credential is copied and mapped to hub.CredentialKind", func(t *testing.T) {
		evt := store.ScheduledEvent{
			ID:        tid("e2b-r4-same-evt"),
			ProjectID: f.proj.ID,
			EventType: "dispatch_agent",
			FireAt:    time.Now(),
			Payload:   `{"agentName":"r4-same-agent"}`,
			CreatedBy: f.owner.ID,
			InitiatorAttribution: store.InitiatorAttribution{
				InitiatorPrincipalKind:  "user",
				InitiatorPrincipalID:    f.owner.ID,
				InitiatorCredentialKind: store.InitiatorCredentialKindUAT,
				InitiatorCredentialID:   tid("e2b-r4-tok-a"),
				AttributionVersion:      1,
				AuthorizationRevision:   1,
			},
		}
		require.NoError(t, f.store.CreateScheduledEvent(ctx, &evt))
		f.srv.scheduler.fireEvent(ctx, evt, false)

		agent, err := f.store.GetAgentBySlug(ctx, f.proj.ID, "r4-same-agent")
		require.NoError(t, err)

		rec := waitForAudit(t, agent.ID)
		assert.Equal(t, "user", rec.ActorPrincipalKind)
		assert.Equal(t, f.owner.ID, rec.ActorPrincipalID)
		assert.Equal(t, string(CredentialKindUAT), rec.ActorCredentialType, "must use hub.CredentialKind's vocabulary, not the attribution domain")
		assert.Equal(t, tid("e2b-r4-tok-a"), rec.ActorCredentialID)
		assert.Equal(t, "scheduler", rec.ExecutorKind)
		assert.Equal(t, "scheduled_event:"+evt.ID, rec.ExecutorID)
		assert.Equal(t, "allow", rec.CanDelegateResult)

		// bypassAgentsSetup configures no dispatcher, so dispatchAgentEventHandler
		// takes the "no dispatcher available" branch rather than the success
		// log — both carry the same executor fields, from the same
		// ExecutorContext read.
		logRec, ok := findRecord(capture.all(), "Scheduler: no dispatcher available, agent created but not started")
		require.True(t, ok, "expected a dispatch log line")
		attrs := recordAttrs(logRec)
		assert.Equal(t, "scheduler", attrs["executor_kind"])
		assert.Equal(t, "scheduled_event:"+evt.ID, attrs["executor_id"])
	})

	t.Run("different principal: credential fields left empty", func(t *testing.T) {
		evt := store.ScheduledEvent{
			ID:        tid("e2b-r4-diff-evt"),
			ProjectID: f.proj.ID,
			EventType: "dispatch_agent",
			FireAt:    time.Now(),
			Payload:   `{"agentName":"r4-diff-agent"}`,
			CreatedBy: f.owner.ID,
			InitiatorAttribution: store.InitiatorAttribution{
				InitiatorPrincipalKind:  "user",
				InitiatorPrincipalID:    tid("e2b-r4-other-user"),
				InitiatorCredentialKind: store.InitiatorCredentialKindUAT,
				InitiatorCredentialID:   tid("e2b-r4-tok-b"),
				AttributionVersion:      1,
				AuthorizationRevision:   2,
			},
		}
		require.NoError(t, f.store.CreateScheduledEvent(ctx, &evt))
		f.srv.scheduler.fireEvent(ctx, evt, false)

		agent, err := f.store.GetAgentBySlug(ctx, f.proj.ID, "r4-diff-agent")
		require.NoError(t, err)

		rec := waitForAudit(t, agent.ID)
		assert.Equal(t, "user", rec.ActorPrincipalKind)
		assert.Equal(t, f.owner.ID, rec.ActorPrincipalID, "the actor is still the creator/execution identity")
		assert.Empty(t, rec.ActorCredentialType, "initiator differs from creator: no credential pairing")
		assert.Empty(t, rec.ActorCredentialID)
		assert.Equal(t, "scheduler", rec.ExecutorKind)
		assert.Equal(t, "scheduled_event:"+evt.ID, rec.ExecutorID)
	})

	t.Run("legacy_unknown initiator: credential fields left empty", func(t *testing.T) {
		evt := store.ScheduledEvent{
			ID:        tid("e2b-r4-legacy-evt"),
			ProjectID: f.proj.ID,
			EventType: "dispatch_agent",
			FireAt:    time.Now(),
			Payload:   `{"agentName":"r4-legacy-agent"}`,
			CreatedBy: f.owner.ID,
			// No InitiatorAttribution set at all (legacy row).
		}
		require.NoError(t, f.store.CreateScheduledEvent(ctx, &evt))
		f.srv.scheduler.fireEvent(ctx, evt, false)

		agent, err := f.store.GetAgentBySlug(ctx, f.proj.ID, "r4-legacy-agent")
		require.NoError(t, err)

		rec := waitForAudit(t, agent.ID)
		assert.Equal(t, f.owner.ID, rec.ActorPrincipalID)
		assert.Empty(t, rec.ActorCredentialType)
		assert.Empty(t, rec.ActorCredentialID)
	})

	// The next two subtests cover ptone/scion#2342 review round 1, finding
	// 1: a dev_local initiator's PrincipalKind is "dev"
	// (hub.DevUser.Type()), but scheduledCreatorIdentity resolves CreatedBy
	// generically as a "user" identity — never "dev" — so the general
	// same-kind/same-ID rule alone could never pair a genuine dev_local
	// self-fire. initiatorMatchesExecutor's dev_local addition closes that
	// gap; these subtests pin both its positive and negative sides.

	t.Run("dev_local initiator, same principal as the executed dev user: credential is copied and mapped to the dev kind", func(t *testing.T) {
		// bypassAgentsServer (bypassAgentsSetup) configures cfg.DevAuthToken,
		// so New() already seeded a User row at DevUserID (seedDevUser) —
		// scheduledCreatorIdentity can resolve CreatedBy: DevUserID via
		// GetUser without any extra setup here.
		require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, f.proj.ID, DevUserID))

		evt := store.ScheduledEvent{
			ID:        tid("e2b-r4-devlocal-same-evt"),
			ProjectID: f.proj.ID,
			EventType: "dispatch_agent",
			FireAt:    time.Now(),
			Payload:   `{"agentName":"r4-devlocal-same-agent"}`,
			CreatedBy: DevUserID,
			InitiatorAttribution: store.InitiatorAttribution{
				InitiatorPrincipalKind:  "dev",
				InitiatorPrincipalID:    DevUserID,
				InitiatorCredentialKind: store.InitiatorCredentialKindDevLocal,
				AttributionVersion:      1,
				AuthorizationRevision:   1,
			},
		}
		require.NoError(t, f.store.CreateScheduledEvent(ctx, &evt))
		f.srv.scheduler.fireEvent(ctx, evt, false)

		agent, err := f.store.GetAgentBySlug(ctx, f.proj.ID, "r4-devlocal-same-agent")
		require.NoError(t, err)

		rec := waitForAudit(t, agent.ID)
		assert.Equal(t, "user", rec.ActorPrincipalKind, "scheduledCreatorIdentity resolves the seeded dev user as a generic user identity")
		assert.Equal(t, DevUserID, rec.ActorPrincipalID)
		assert.Equal(t, string(CredentialKindDev), rec.ActorCredentialType, "must show the dev kind, distinct from an ordinary session")
		assert.NotEqual(t, string(CredentialKindInteractive), rec.ActorCredentialType)
	})

	t.Run("dev_local initiator, executor is not DevUserID: credential fields left empty", func(t *testing.T) {
		// The initiator's PrincipalID IS DevUserID; what varies here is the
		// executor (CreatedBy is the ordinary owner, so exec.ID() !=
		// DevUserID). Neither the general rule (PrincipalKind "dev" !=
		// exec.Type() "user") nor the dev_local addition (requires
		// exec.ID()==DevUserID) can match — this is the negative mirror of
		// the subtest above, and it pins initiatorMatchesExecutor's
		// exec.ID()==DevUserID clause specifically (ptone/scion#2342 review
		// round 2, R1 — the direct table test TestInitiatorMatchesExecutor
		// below is the primary pin for every clause of the dev_local arm;
		// this subtest additionally proves the helper is wired correctly
		// into the real fire/audit path).
		evt := store.ScheduledEvent{
			ID:        tid("e2b-r4-devlocal-wrongid-evt"),
			ProjectID: f.proj.ID,
			EventType: "dispatch_agent",
			FireAt:    time.Now(),
			Payload:   `{"agentName":"r4-devlocal-wrongid-agent"}`,
			CreatedBy: f.owner.ID,
			InitiatorAttribution: store.InitiatorAttribution{
				InitiatorPrincipalKind:  "dev",
				InitiatorPrincipalID:    DevUserID,
				InitiatorCredentialKind: store.InitiatorCredentialKindDevLocal,
				AttributionVersion:      1,
				AuthorizationRevision:   1,
			},
		}
		require.NoError(t, f.store.CreateScheduledEvent(ctx, &evt))
		f.srv.scheduler.fireEvent(ctx, evt, false)

		agent, err := f.store.GetAgentBySlug(ctx, f.proj.ID, "r4-devlocal-wrongid-agent")
		require.NoError(t, err)

		rec := waitForAudit(t, agent.ID)
		assert.Equal(t, "user", rec.ActorPrincipalKind)
		assert.Equal(t, f.owner.ID, rec.ActorPrincipalID, "the actor is still the creator/execution identity")
		assert.Empty(t, rec.ActorCredentialType, "a dev_local initiator paired with a non-DevUserID executor must not pair")
		assert.Empty(t, rec.ActorCredentialID)
	})
}

// TestInitiatorMatchesExecutor is the direct, table-driven unit test for
// initiatorMatchesExecutor (ptone/scion#2342 review round 2, R1 and O1). The
// helper is pure, so it needs no server. Each dev_local-arm row is chosen so
// that replacing any single clause of the arm with an unconditional `true`
// changes that row's expected result from false to true — the per-row
// comments below name the clause each row pins.
func TestInitiatorMatchesExecutor(t *testing.T) {
	devUserExec := NewAuthenticatedUser(DevUserID, "dev@localhost", "Development User", "admin", "api")
	otherUserExec := NewAuthenticatedUser(tid("e2b-ime-other-user"), "other@example.com", "Other User", "member", "api")
	// A non-user identity whose ID is nonetheless DevUserID, to kill the
	// exec.Type()=="user" clause without also changing exec.ID().
	nonUserDevIDExec := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: DevUserID}}}
	// Typed-nil execs (GCP#2188 review comment, ptone/scion#2342): a non-nil
	// Identity holding a nil concrete pointer. exec == nil is false for
	// both, so these pin isNilIdentity's reflect-based check: the rows below
	// require initiatorMatchesExecutor to return false without reaching
	// exec.ID()/exec.Type(), which would otherwise panic on a nil receiver.
	var typedNilDevUserExec *DevUser
	var typedNilAuthenticatedUserExec *AuthenticatedUser

	cases := []struct {
		name      string
		initiator ScheduledInitiator
		exec      Identity
		want      bool
	}{
		// 1. Positive: the dev_local arm's exact case.
		{
			name:      "dev_local, DevUserID, user executor DevUserID: pairs",
			initiator: ScheduledInitiator{PrincipalKind: "dev", PrincipalID: DevUserID, CredentialKind: store.InitiatorCredentialKindDevLocal},
			exec:      devUserExec,
			want:      true,
		},
		// 2. Same shape, other credential kinds — kills the
		// CredentialKind==dev_local clause (mutating it to `true` would let
		// any kind pair through the dev_local arm).
		{
			name:      "dev, DevUserID, session credential: does not pair",
			initiator: ScheduledInitiator{PrincipalKind: "dev", PrincipalID: DevUserID, CredentialKind: store.InitiatorCredentialKindSession},
			exec:      devUserExec,
			want:      false,
		},
		{
			name:      "dev, DevUserID, uat credential: does not pair",
			initiator: ScheduledInitiator{PrincipalKind: "dev", PrincipalID: DevUserID, CredentialKind: store.InitiatorCredentialKindUAT},
			exec:      devUserExec,
			want:      false,
		},
		{
			name:      "dev, DevUserID, agent credential: does not pair",
			initiator: ScheduledInitiator{PrincipalKind: "dev", PrincipalID: DevUserID, CredentialKind: store.InitiatorCredentialKindAgent},
			exec:      devUserExec,
			want:      false,
		},
		// 3. dev_local naming another principal — kills the
		// PrincipalID==DevUserID clause.
		{
			name:      "dev_local naming a non-DevUserID principal: does not pair",
			initiator: ScheduledInitiator{PrincipalKind: "dev", PrincipalID: tid("e2b-ime-other-dev"), CredentialKind: store.InitiatorCredentialKindDevLocal},
			exec:      devUserExec,
			want:      false,
		},
		// 4. Executor is not DevUserID — kills the exec.ID()==DevUserID
		// clause (also covered end to end by the fire-path subtest above).
		{
			name:      "dev_local/DevUserID initiator, executor is a different user: does not pair",
			initiator: ScheduledInitiator{PrincipalKind: "dev", PrincipalID: DevUserID, CredentialKind: store.InitiatorCredentialKindDevLocal},
			exec:      otherUserExec,
			want:      false,
		},
		// 5. Executor is not a user — kills the exec.Type()=="user" clause.
		{
			name:      "dev_local/DevUserID initiator, executor is DevUserID but not a user: does not pair",
			initiator: ScheduledInitiator{PrincipalKind: "dev", PrincipalID: DevUserID, CredentialKind: store.InitiatorCredentialKindDevLocal},
			exec:      nonUserDevIDExec,
			want:      false,
		},
		// O1: PrincipalKind != "dev" but every other dev_local-arm clause
		// holds, and the general rule doesn't fire either (PrincipalKind
		// "agent" != exec.Type() "user") — kills the PrincipalKind=="dev"
		// clause O1 added.
		{
			name:      "non-dev PrincipalKind, DevUserID, dev_local credential, user executor DevUserID: does not pair",
			initiator: ScheduledInitiator{PrincipalKind: "agent", PrincipalID: DevUserID, CredentialKind: store.InitiatorCredentialKindDevLocal},
			exec:      devUserExec,
			want:      false,
		},
		// O1's own suggested row: a "user"-kind dev_local row pairs via the
		// general rule alone (PrincipalKind == exec.Type() and the IDs
		// match) — the dev_local arm is neither needed nor reached for this
		// shape.
		{
			name:      "user PrincipalKind, DevUserID, dev_local credential, user executor DevUserID: pairs via the general rule",
			initiator: ScheduledInitiator{PrincipalKind: "user", PrincipalID: DevUserID, CredentialKind: store.InitiatorCredentialKindDevLocal},
			exec:      devUserExec,
			want:      true,
		},
		// 6. Legacy rows never pair, regardless of what other fields say —
		// this is the top-level guard, checked before either rule.
		{
			name:      "legacy_unknown: never pairs",
			initiator: ScheduledInitiator{LegacyUnknown: true},
			exec:      devUserExec,
			want:      false,
		},
		{
			name:      "legacy_unknown carrying dev_local/DevUserID values: never pairs",
			initiator: ScheduledInitiator{LegacyUnknown: true, PrincipalKind: "dev", PrincipalID: DevUserID, CredentialKind: store.InitiatorCredentialKindDevLocal},
			exec:      devUserExec,
			want:      false,
		},
		// 7. Nil executor never pairs — the other top-level guard.
		{
			name:      "nil executor: never pairs",
			initiator: ScheduledInitiator{PrincipalKind: "dev", PrincipalID: DevUserID, CredentialKind: store.InitiatorCredentialKindDevLocal},
			exec:      nil,
			want:      false,
		},
		// 7a. Typed-nil executors (GCP#2188 review comment, ptone/scion#2342):
		// a non-nil Identity holding a nil *DevUser or nil *AuthenticatedUser
		// makes a bare exec == nil comparison false, so each row here is
		// required to return false without panicking — one against the
		// dev_local arm's DevUserID shape, one against a general-rule row,
		// for both typed-nil concrete types.
		{
			name:      "typed-nil *DevUser executor, dev_local/DevUserID initiator: does not pair, does not panic",
			initiator: ScheduledInitiator{PrincipalKind: "dev", PrincipalID: DevUserID, CredentialKind: store.InitiatorCredentialKindDevLocal},
			exec:      typedNilDevUserExec,
			want:      false,
		},
		{
			name:      "typed-nil *DevUser executor, general-rule initiator: does not pair, does not panic",
			initiator: ScheduledInitiator{PrincipalKind: "user", PrincipalID: tid("e2b-ime-typed-nil-user"), CredentialKind: store.InitiatorCredentialKindUAT},
			exec:      typedNilDevUserExec,
			want:      false,
		},
		{
			name:      "typed-nil *AuthenticatedUser executor, dev_local/DevUserID initiator: does not pair, does not panic",
			initiator: ScheduledInitiator{PrincipalKind: "dev", PrincipalID: DevUserID, CredentialKind: store.InitiatorCredentialKindDevLocal},
			exec:      typedNilAuthenticatedUserExec,
			want:      false,
		},
		{
			name:      "typed-nil *AuthenticatedUser executor, general-rule initiator: does not pair, does not panic",
			initiator: ScheduledInitiator{PrincipalKind: "user", PrincipalID: tid("e2b-ime-typed-nil-user2"), CredentialKind: store.InitiatorCredentialKindUAT},
			exec:      typedNilAuthenticatedUserExec,
			want:      false,
		},
		// 8. The general rule is unchanged for ordinary kinds: same
		// kind/ID pairs; a kind or ID mismatch does not.
		{
			name:      "uat, kind and ID match: pairs via the general rule",
			initiator: ScheduledInitiator{PrincipalKind: "user", PrincipalID: tid("e2b-ime-uat-user"), CredentialKind: store.InitiatorCredentialKindUAT},
			exec:      NewAuthenticatedUser(tid("e2b-ime-uat-user"), "uat@example.com", "UAT User", "member", "api"),
			want:      true,
		},
		{
			name:      "uat, kind mismatch: does not pair",
			initiator: ScheduledInitiator{PrincipalKind: "agent", PrincipalID: tid("e2b-ime-uat-user2"), CredentialKind: store.InitiatorCredentialKindUAT},
			exec:      NewAuthenticatedUser(tid("e2b-ime-uat-user2"), "uat2@example.com", "UAT User 2", "member", "api"),
			want:      false,
		},
		{
			name:      "session, ID mismatch: does not pair",
			initiator: ScheduledInitiator{PrincipalKind: "user", PrincipalID: tid("e2b-ime-session-user"), CredentialKind: store.InitiatorCredentialKindSession},
			exec:      NewAuthenticatedUser(tid("e2b-ime-session-user-other"), "session@example.com", "Session User", "member", "api"),
			want:      false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, initiatorMatchesExecutor(tc.initiator, tc.exec))
		})
	}
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
// an authenticated path) still produces an explicit legacy_unknown
// credential kind (every other field stays empty), which scheduledInitiator
// reads back as legacy_unknown.
func TestCaptureInitiatorAttribution_NoIdentityReadsAsLegacyUnknown(t *testing.T) {
	srv := &Server{}
	attr := captureInitiatorAttribution(context.Background())
	assert.Equal(t, store.InitiatorAttribution{InitiatorCredentialKind: store.InitiatorCredentialKindLegacyUnknown}, attr)
	assert.True(t, srv.scheduledInitiator(attr).LegacyUnknown)
}

// TestCaptureInitiatorAttribution_TypedNilIdentityReadsAsLegacyUnknown covers
// the same defensive branch as the test above, but for a context whose
// ambient identity is a non-nil Identity holding a nil concrete pointer (a
// "typed nil" — GCP#2188 review comment, ptone/scion#2342, raised against
// initiatorMatchesExecutor but equally applicable to any Identity call site
// that only checks identity == nil). GetIdentityFromContext's
// ctx.Value(identityContextKey{}).(Identity) type assertion returns such a
// value unchanged if one is ever stored on the context, so this exercises
// captureInitiatorAttribution's isNilIdentity guard directly rather than
// relying on no production caller ever doing so.
func TestCaptureInitiatorAttribution_TypedNilIdentityReadsAsLegacyUnknown(t *testing.T) {
	srv := &Server{}

	t.Run("typed-nil *DevUser", func(t *testing.T) {
		var nilDevUser *DevUser
		ctx := contextWithIdentity(context.Background(), nilDevUser)
		attr := captureInitiatorAttribution(ctx)
		assert.Equal(t, store.InitiatorAttribution{InitiatorCredentialKind: store.InitiatorCredentialKindLegacyUnknown}, attr)
		assert.True(t, srv.scheduledInitiator(attr).LegacyUnknown)
	})

	t.Run("typed-nil *AuthenticatedUser", func(t *testing.T) {
		var nilAuthenticatedUser *AuthenticatedUser
		ctx := contextWithIdentity(context.Background(), nilAuthenticatedUser)
		attr := captureInitiatorAttribution(ctx)
		assert.Equal(t, store.InitiatorAttribution{InitiatorCredentialKind: store.InitiatorCredentialKindLegacyUnknown}, attr)
		assert.True(t, srv.scheduledInitiator(attr).LegacyUnknown)
	})
}

// TestInitiatorCredentialKindFor pins the committed
// session|uat|agent|dev_local|legacy_unknown domain (rulings "E.2b field
// names"; dev_local added by ptone/scion#2342): only a genuine interactive
// session maps to "session"; only the concrete trusted *DevUser paired with
// CredentialKindDev maps to "dev_local". Absent, federation, and broker
// credentials all map to legacy_unknown, never to an interactive-style
// value — and CredentialKindDev without the trusted identity also falls
// back to legacy_unknown rather than session or dev_local.
func TestInitiatorCredentialKindFor(t *testing.T) {
	devUser := NewDevUser(DevUserConfig{})

	cases := []struct {
		name     string
		identity Identity
		kind     CredentialKind
		want     string
	}{
		{"uat", nil, CredentialKindUAT, store.InitiatorCredentialKindUAT},
		{"agent", nil, CredentialKindAgentJWT, store.InitiatorCredentialKindAgent},
		{"interactive session", nil, CredentialKindInteractive, store.InitiatorCredentialKindSession},
		{"trusted DevUser + dev credential kind: dev_local", devUser, CredentialKindDev, store.InitiatorCredentialKindDevLocal},
		{"dev credential kind without the trusted identity: legacy_unknown", nil, CredentialKindDev, store.InitiatorCredentialKindLegacyUnknown},
		{"federation", nil, CredentialKindFederation, store.InitiatorCredentialKindLegacyUnknown},
		{"broker", nil, CredentialKindBroker, store.InitiatorCredentialKindLegacyUnknown},
		{"absent", nil, CredentialKind(""), store.InitiatorCredentialKindLegacyUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, initiatorCredentialKindFor(tc.identity, tc.kind), "identity=%v kind=%q", tc.identity, tc.kind)
		})
	}
}

// TestIsTrustedLocalDevUser is the table test for the shared predicate
// (binding addition from pat-e-lead/pat-b-lead on ptone/scion#2342): only
// the concrete *DevUser with ID()==DevUserID is trusted. A nil identity, a
// nil *DevUser, a wrapper that embeds *DevUser, a distinct look-alike type
// reporting the same ID, and a *DevUser with the wrong ID must all be
// rejected.
func TestIsTrustedLocalDevUser(t *testing.T) {
	genuine := NewDevUser(DevUserConfig{})
	wrongID := &DevUser{id: "not-the-dev-user-id"}
	var nilDevUser *DevUser
	wrapped := &devUserEmbeddingWrapper{DevUser: genuine}
	lookAlike := &devLookAlikeIdentity{id: DevUserID}

	cases := []struct {
		name     string
		identity Identity
		want     bool
	}{
		{"genuine DevUser", genuine, true},
		{"nil Identity", nil, false},
		{"non-nil Identity holding a nil *DevUser", nilDevUser, false},
		{"DevUser with the wrong ID", wrongID, false},
		{"wrapper embedding *DevUser", wrapped, false},
		{"look-alike identity with the same ID and Type()==\"dev\"", lookAlike, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isTrustedLocalDevUser(tc.identity))
		})
	}
}

// TestAuthzService_DevLocalAuthorityEnabled pins B.3 R6's invariant
// (binding design addition, ptone/scion#2342): devLocalAuthorityEnabled()
// tracks the exact server-wide condition that lets a request authenticate
// as the trusted dev user (ServerConfig.DevAuthToken != "", see
// devLocalAuthorityEnabled's doc comment in devauth.go for the precise
// server.go/auth.go line references) — not merely whether the seeded
// DevUserID row happens to exist in the store.
func TestAuthzService_DevLocalAuthorityEnabled(t *testing.T) {
	t.Run("dev-auth on: flag true, and a real dev-token request is trusted", func(t *testing.T) {
		srv, _ := testServer(t) // testServer enables dev auth (testDevToken).
		require.True(t, srv.authzService.devLocalAuthorityEnabled())
		// Pin the mirror to the value UnifiedAuthMiddleware actually uses
		// (srv.authConfig.DevAuthEnabled), not to a re-derivation of
		// cfg.DevAuthToken != "" (ptone/scion#2342 review round 1, finding 3).
		assert.Equal(t, srv.authConfig.DevAuthEnabled, srv.authzService.devLocalAuthorityEnabled())

		var gotIdentity Identity
		handler := UnifiedAuthMiddleware(srv.authConfig)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotIdentity = GetIdentityFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.True(t, isTrustedLocalDevUser(gotIdentity), "a real dev-token request must authenticate as the trusted *DevUser")
	})

	t.Run("dev-auth off: flag false, and the real wiring cannot produce a trusted dev identity, even though the seeded DevUserID row exists", func(t *testing.T) {
		s, err := newTestStore(":memory:")
		if err != nil {
			if strings.Contains(err.Error(), "sqlite driver not registered") {
				t.Skip("Skipping test because sqlite driver is not registered (build with -tags sqlite to enable)")
			}
			t.Fatalf("failed to create test store: %v", err)
		}
		ctx := context.Background()
		require.NoError(t, s.Migrate(ctx))
		_ = s.DeleteHubSetting(ctx, "migration_delegation_edge_backfill_v1")

		// Seed the DevUserID row directly, exactly as server.go's startup
		// path would (seedDevUser) — but this server's config disables
		// dev-auth, so the row's mere existence must not be what the flag
		// reports.
		seedDevUser(ctx, s, DevUserConfig{})

		cfg := DefaultServerConfig()
		cfg.DevAuthToken = "" // dev-auth off
		srv, err := New(cfg, s)
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = srv.Shutdown(context.Background())
			_ = s.Close()
		})

		assert.False(t, srv.authzService.devLocalAuthorityEnabled())
		// Same mirror pin as the "on" subtest — the invariant is checked
		// against the value the middleware actually uses on both sides.
		assert.Equal(t, srv.authConfig.DevAuthEnabled, srv.authzService.devLocalAuthorityEnabled())

		// The other half of binding addition (c)'s invariant
		// (isTrustedLocalDevUser(id) ⇒ devLocalAuthorityEnabled()) is that
		// this server's own request pipeline can never produce the trusted
		// identity in the first place when the flag is false — not merely
		// that the flag reads false in isolation (ptone/scion#2342 review
		// round 1, finding 3). A dev-token-shaped bearer token, sent through
		// the real UnifiedAuthMiddleware with dev-auth off, must be
		// rejected before any handler could see a trusted dev identity.
		var gotIdentity Identity
		handlerCalled := false
		handler := UnifiedAuthMiddleware(srv.authConfig)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			gotIdentity = GetIdentityFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.True(t, !handlerCalled || !isTrustedLocalDevUser(gotIdentity),
			"with dev-auth off, the inner handler must either never run, or never see a trusted dev identity")
	})
}

// devLookAlikeIdentity is a minimal Identity implementation that reports
// Type()=="dev" and the well-known DevUserID without being a *DevUser. It
// models "a look-alike identity type with the same ID and Type()==\"dev\""
// (ptone/scion#2342 AC): the dev_local decision must not be derivable from
// Type() or ID() alone.
type devLookAlikeIdentity struct{ id string }

func (d *devLookAlikeIdentity) ID() string   { return d.id }
func (d *devLookAlikeIdentity) Type() string { return "dev" }

// devTypedUserIdentity is a full UserIdentity implementation (not merely a
// minimal Identity stub) whose Type() happens to be "dev" with the
// well-known DevUserID — modeling "a real user whose Type() is dev"
// (ptone/scion#2342 AC), distinct from devLookAlikeIdentity's bare stub.
type devTypedUserIdentity struct{ id string }

func (d *devTypedUserIdentity) ID() string          { return d.id }
func (d *devTypedUserIdentity) Type() string        { return "dev" }
func (d *devTypedUserIdentity) Email() string       { return "look-alike@example.com" }
func (d *devTypedUserIdentity) DisplayName() string { return "Look-Alike Dev-Typed User" }
func (d *devTypedUserIdentity) Role() string        { return "member" }

var _ UserIdentity = (*devTypedUserIdentity)(nil)

// devUserEmbeddingWrapper embeds *DevUser (promoting its ID()/Type()
// methods) but is itself a distinct concrete type. Go type assertions do
// not see through embedding to an outer type, so isTrustedLocalDevUser must
// reject this even though it satisfies Identity identically to a genuine
// *DevUser. Models "a *DevUser-like wrapper" (ptone/scion#2342 AC).
type devUserEmbeddingWrapper struct{ *DevUser }

// TestInitiatorCredentialKindFor_NonDevLookAlikesRecordLegacyUnknown covers
// the negative acceptance criteria directly at the mapping level: every
// non-genuine dev-shaped identity records legacy_unknown, never dev_local —
// including when its ambient CredentialKind is CredentialKindDev (which
// credentialContextForIdentity assigns to ANY identity whose Type() is
// "dev", genuine or not).
func TestInitiatorCredentialKindFor_NonDevLookAlikesRecordLegacyUnknown(t *testing.T) {
	cases := []struct {
		name     string
		identity Identity
	}{
		{"look-alike identity, same ID, Type()==\"dev\"", &devLookAlikeIdentity{id: DevUserID}},
		{"full UserIdentity, same ID, Type()==\"dev\"", &devTypedUserIdentity{id: DevUserID}},
		{"wrapper embedding a genuine *DevUser", &devUserEmbeddingWrapper{DevUser: NewDevUser(DevUserConfig{})}},
		{"DevUser with the wrong ID", &DevUser{id: "not-the-dev-user-id"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := initiatorCredentialKindFor(tc.identity, CredentialKindDev)
			assert.Equal(t, store.InitiatorCredentialKindLegacyUnknown, got)
			assert.NotEqual(t, store.InitiatorCredentialKindDevLocal, got)
		})
	}
}

// TestInitiatorCredentialKindFor_NonDevIdentitiesNeverEmitDevLocal is the
// positive-domain counterpart (binding addition from
// pat-e-lead/pat-b-lead on ptone/scion#2342): every ordinary,
// non-dev-shaped identity/credential-kind pairing keeps its own mapping and
// never emits dev_local — session, UAT, agent, broker, and a "system"
// context with no ambient identity at all (a deferred/scheduler execution
// context; see TestCaptureInitiatorAttribution_NoIdentityReadsAsLegacyUnknown).
func TestInitiatorCredentialKindFor_NonDevIdentitiesNeverEmitDevLocal(t *testing.T) {
	sessionUser := NewAuthenticatedUser(tid("e2b-nondev-session"), "s@example.com", "S", "member", "api")
	uatBase := NewAuthenticatedUser(tid("e2b-nondev-uat-user"), "u@example.com", "U", "member", "api")
	scopedUAT := NewScopedUserIdentityWithCredentialID(uatBase, tid("e2b-nondev-proj"), []string{"schedule:create"}, tid("e2b-nondev-tok"))
	agent := &agentIdentityWrapper{&AgentTokenClaims{}}
	broker := NewBrokerIdentity(tid("e2b-nondev-broker"))

	cases := []struct {
		name     string
		identity Identity
		kind     CredentialKind
		want     string
	}{
		{"session", sessionUser, CredentialKindInteractive, store.InitiatorCredentialKindSession},
		{"uat", scopedUAT, CredentialKindUAT, store.InitiatorCredentialKindUAT},
		{"agent", agent, CredentialKindAgentJWT, store.InitiatorCredentialKindAgent},
		{"broker", broker, CredentialKindBroker, store.InitiatorCredentialKindLegacyUnknown},
		{"system (no ambient identity)", nil, CredentialKind(""), store.InitiatorCredentialKindLegacyUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := initiatorCredentialKindFor(tc.identity, tc.kind)
			assert.Equal(t, tc.want, got)
			assert.NotEqual(t, store.InitiatorCredentialKindDevLocal, got)
		})
	}
}

// TestHubCredentialKindForInitiator pins the reverse mapping:
// uat/agent/session/dev_local round-trip to a real hub.CredentialKind, with
// dev_local mapping to CredentialKindDev ("dev") so audit/log attribution
// stays visibly distinct from an ordinary interactive session
// (ptone/scion#2342); legacy_unknown (or anything else) means "leave the
// column unset".
func TestHubCredentialKindForInitiator(t *testing.T) {
	cases := []struct{ in, want string }{
		{store.InitiatorCredentialKindUAT, string(CredentialKindUAT)},
		{store.InitiatorCredentialKindAgent, string(CredentialKindAgentJWT)},
		{store.InitiatorCredentialKindSession, string(CredentialKindInteractive)},
		{store.InitiatorCredentialKindDevLocal, string(CredentialKindDev)},
		{store.InitiatorCredentialKindLegacyUnknown, ""},
		{"", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, hubCredentialKindForInitiator(tc.in), "in=%q", tc.in)
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
	// A plain (non-scoped, non-agent) user identity's credential context
	// defaults to CredentialKindInteractive (credentialContextForIdentity's
	// fallback branch), which maps to "session" — a genuine interactive
	// session, correctly distinct from legacy_unknown.
	assert.Equal(t, store.InitiatorCredentialKindSession, d.InitiatorCredentialKind)
	assert.Equal(t, "e2b-corr-123", d.CorrelationID)
}

// TestSetBrokerDispatchInitiator_NoIdentityRecordsLegacyUnknown covers a
// context with no ambient identity at all (e.g. a cross-node op opened
// outside a live request): the credential kind must be the explicit
// legacy_unknown, never an empty string that would look like "no value
// recorded" rather than "no recordable provenance."
func TestSetBrokerDispatchInitiator_NoIdentityRecordsLegacyUnknown(t *testing.T) {
	d := &store.BrokerDispatch{}
	setBrokerDispatchInitiator(context.Background(), d)

	assert.Empty(t, d.InitiatorPrincipalKind)
	assert.Empty(t, d.InitiatorPrincipalID)
	assert.Equal(t, store.InitiatorCredentialKindLegacyUnknown, d.InitiatorCredentialKind)
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
