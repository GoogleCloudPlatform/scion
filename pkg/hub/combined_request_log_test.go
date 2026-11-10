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
// too, and a request the web chain answers itself is logged once, by the
// web logger.
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
	// Production sets the hub as the access settings provider, and
	// contentHostGuardFor then applies srv.ContentHostGuard outermost; set
	// that guard here so the test pins the production composition. With no
	// content URL configured, the guard passes every request through.
	ws.SetContentHostGuard(srv.ContentHostGuard)

	for _, tc := range []struct {
		name          string
		path          string
		bearer        string
		wantStatus    int
		wantComponent string
		wantProjectID string
		wantAuthType  string
	}{
		{name: "authenticated", path: "/api/v1/projects/" + projectID, bearer: uatKey,
			wantStatus: http.StatusOK, wantComponent: "hub", wantProjectID: projectID, wantAuthType: "uat"},
		{name: "rejected", path: "/api/v1/projects/" + projectID, bearer: "scion_pat_definitely-not-a-real-token",
			wantStatus: http.StatusUnauthorized, wantComponent: "hub", wantProjectID: projectID},
		{name: "web answered", path: "/healthz",
			wantStatus: http.StatusOK, wantComponent: "web"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(capture.all())
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			rec := httptest.NewRecorder()
			ws.Handler().ServeHTTP(rec, req)
			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())

			records := capture.all()[before:]
			require.Len(t, records, 1, "want exactly one request-log line")
			line := recordAttrs(records[0])
			require.Equal(t, tc.wantComponent, line["component"])
			require.Equal(t, tc.wantProjectID, line["project_id"])
			require.EqualValues(t, tc.wantStatus, line["httpRequest.status"])
			// Unconditional: the rejected and web-answered rows pin an
			// empty auth_type (a rejection is logged separately, at the
			// point of rejection, and never sets the request's auth).
			require.Equal(t, tc.wantAuthType, line["auth_type"])
			require.NotEmpty(t, rec.Header().Get("X-Request-ID"))
			require.Equal(t, rec.Header().Get("X-Request-ID"), line["request_id"])
		})
	}
}

// TestCombinedServer_ContentHostRefusalNotRequestLogged (ptone/scion#7367):
// on the combined server with a content URL in effect, a request on the
// content host for a route it does not serve is refused 404 by the content
// host guard, which wraps the web request logger. The request logger writes
// no line for it; the guard's own refusal line goes to the default logger.
func TestCombinedServer_ContentHostRefusalNotRequestLogged(t *testing.T) {
	srv, _ := contentHostServer(t, loTestOrigin)

	capture := &capturingHandler{}
	logger := slog.New(capture)
	srv.SetRequestLogger(logger)
	ws := newTestWebServer(t, WebServerConfig{})
	ws.SetRequestLogger(logger)
	ws.MountHubAPI(srv.Handler(), func(context.Context) error { return nil })
	ws.SetContentHostGuard(srv.ContentHostGuard)

	// The guard logs its refusal to the default logger; capture that apart
	// from the request logger, so it cannot count as a request-log line.
	guardCapture := &capturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(guardCapture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	before := len(capture.all())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Host = loTestHost
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	require.Empty(t, capture.all()[before:], "want no request-log line for a content host refusal")
	var refusals int
	for _, r := range guardCapture.all() {
		if r.Message == "Content host: request refused (route not served on the content host)" {
			refusals++
		}
	}
	require.Equal(t, 1, refusals, "want the guard's own refusal line")
}
