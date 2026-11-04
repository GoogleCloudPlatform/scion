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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCombinedServer_RequestLoggedOnce (ptone/scion#7367): on the combined
// web and hub server, a Hub API request passes through the web chain's
// request logger and then the hub chain's. It is logged once, by the hub
// logger, with the auth type the hub's auth set, and the response's
// X-Request-ID names that line. A request the hub rejects is logged once
// too.
func TestCombinedServer_RequestLoggedOnce(t *testing.T) {
	srv, s := testServer(t)
	projectID, ownerID := setupUATProjectAndOwner(t, s, "reqlog-combined")
	uatKey := mintScopedUAT(t, srv, ownerID, projectID, []string{"project:read"})

	// Both servers share one request logger, as cmd/server_foreground.go
	// wires them. The hub chain reads its logger when Handler builds it.
	capture := &capturingHandler{}
	logger := slog.New(capture)
	srv.SetRequestLogger(logger)
	ws := newTestWebServer(t, WebServerConfig{})
	ws.SetRequestLogger(logger)
	ws.MountHubAPI(srv.Handler(), func(context.Context) error { return nil })

	for _, tc := range []struct {
		name         string
		bearer       string
		wantStatus   int
		wantAuthType string
	}{
		{name: "authenticated", bearer: uatKey, wantStatus: http.StatusOK, wantAuthType: "uat"},
		{name: "rejected", bearer: "scion_pat_definitely-not-a-real-token", wantStatus: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(capture.all())
			req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+projectID, nil)
			req.Header.Set("Authorization", "Bearer "+tc.bearer)
			rec := httptest.NewRecorder()
			ws.Handler().ServeHTTP(rec, req)
			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())

			records := capture.all()[before:]
			require.Len(t, records, 1, "want exactly one request-log line")
			line := recordAttrs(records[0])
			require.Equal(t, "hub", line["component"])
			require.Equal(t, projectID, line["project_id"])
			require.EqualValues(t, tc.wantStatus, line["httpRequest.status"])
			if tc.wantAuthType != "" {
				require.Equal(t, tc.wantAuthType, line["auth_type"])
			}
			require.NotEmpty(t, rec.Header().Get("X-Request-ID"))
			require.Equal(t, rec.Header().Get("X-Request-ID"), line["request_id"])
		})
	}
}
