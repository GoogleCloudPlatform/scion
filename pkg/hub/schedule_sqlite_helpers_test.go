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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// authzHelperAgentSlug is the slug of the agent seedScheduleAuthorAgent
// stores; message schedules in these tests target it, so the
// scheduled-message rule resolves a real target.
const authzHelperAgentSlug = "schedule-author-agent"

// authoredRequest builds a request carrying identity and the credential
// context the auth middleware derives from it, so the authoring handlers
// record the request's attribution as they do in production.
func authoredRequest(t *testing.T, identity Identity, method, path string, body interface{}) *http.Request {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	ctx := contextWithIdentity(req.Context(), identity)
	ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(identity))
	return req.WithContext(ctx)
}

// doAuthoredScheduleRequest sends an authored request to the schedules
// route.
func doAuthoredScheduleRequest(t *testing.T, srv *Server, identity Identity, projectID, schedulePath, method string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	req := authoredRequest(t, identity, method, "/api/v1/projects/"+projectID+"/schedules/"+schedulePath, body)
	rec := httptest.NewRecorder()
	srv.handleSchedules(rec, req, projectID, schedulePath)
	return rec
}

// doAuthoredEventRequest sends an authored create to the scheduled-events
// route.
func doAuthoredEventRequest(t *testing.T, srv *Server, identity Identity, projectID string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	req := authoredRequest(t, identity, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", body)
	rec := httptest.NewRecorder()
	srv.handleScheduledEvents(rec, req, projectID, "")
	return rec
}

func loadScheduleRevision(t *testing.T, s store.Store, id string) scheduleRevision {
	t.Helper()
	sc, err := s.GetSchedule(context.Background(), id)
	require.NoError(t, err)
	return scheduleRevision{Attribution: sc.InitiatorAttribution, Ceiling: sc.AuthorityCeiling}
}

// createOwnerSchedule creates a schedule of eventType as owner through the
// handler and returns its ID.
func createOwnerSchedule(t *testing.T, srv *Server, owner Identity, projectID, name, eventType string) string {
	t.Helper()
	req := CreateScheduleRequest{Name: name, CronExpr: "0 * * * *", EventType: eventType, AgentName: "worker"}
	if eventType == "message" {
		req.AgentName = authzHelperAgentSlug
		req.Message = "ping"
	}
	rec := doAuthoredScheduleRequest(t, srv, owner, projectID, "", http.MethodPost, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	return created.ID
}

func pauseSchedule(t *testing.T, srv *Server, owner Identity, projectID, id string) {
	t.Helper()
	rec := doAuthoredScheduleRequest(t, srv, owner, projectID, id+"/pause", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// fireRevisionCeiling calls revisionAuthorityCeiling for identity and
// returns its result and the response it wrote.
func fireRevisionCeiling(srv *Server, identity Identity, projectID string) (store.EffectCeiling, bool, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), identity))
	rec := httptest.NewRecorder()
	c, ok := srv.revisionAuthorityCeiling(rec, req, projectID, ActionCreate)
	return c, ok, rec
}

// scheduleRevision is the authority part of a stored schedule: attribution,
// revision and ceiling.
type scheduleRevision struct {
	Attribution store.InitiatorAttribution
	Ceiling     store.EffectCeiling
}
