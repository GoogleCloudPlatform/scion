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

package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Start and restart send the run as a runId query parameter as well as in
// the body (ptone/scion#2550), on both transports, so the broker can record
// it on the tracked start before reading the body. No run, no parameter.

func TestHTTPRuntimeBrokerClient_StartRestartSendRunIDQuery(t *testing.T) {
	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := NewHTTPRuntimeBrokerClient()
	ctx := context.Background()

	for _, runID := range []string{"run-1", ""} {
		if _, err := client.StartAgent(ctx, tid("host-1"), server.URL, "a", "p1", "", "", "", "", "", "", nil, nil, nil, nil, false, false, StartExtras{RunID: runID}); err != nil {
			t.Fatal(err)
		}
		assertRunIDQuery(t, "http start", gotQuery, runID)
		if _, err := client.RestartAgent(ctx, tid("host-1"), server.URL, "a", "p1", nil, StartExtras{RunID: runID}); err != nil {
			t.Fatal(err)
		}
		assertRunIDQuery(t, "http restart", gotQuery, runID)
	}
}

func TestControlChannelBrokerClient_StartRestartSendRunIDQuery(t *testing.T) {
	tunnel := &mockControlChannelTunnel{connected: true, status: http.StatusAccepted, body: []byte(`{}`)}
	client := &ControlChannelBrokerClient{manager: tunnel}
	ctx := context.Background()

	for _, runID := range []string{"run-1", ""} {
		if _, err := client.StartAgent(ctx, "broker-1", "unused", "a", "p1", "", "", "", "", "", "", nil, nil, nil, nil, false, false, StartExtras{RunID: runID}); err != nil {
			t.Fatal(err)
		}
		// Start carries its query on the path, as before.
		_, rawQuery, _ := strings.Cut(tunnel.lastRequest.Path, "?")
		q, err := url.ParseQuery(rawQuery + "&" + tunnel.lastRequest.Query)
		if err != nil {
			t.Fatal(err)
		}
		assertRunIDQuery(t, "control-channel start", q, runID)

		if _, err := client.RestartAgent(ctx, "broker-1", "unused", "a", "p1", nil, StartExtras{RunID: runID}); err != nil {
			t.Fatal(err)
		}
		q, err = url.ParseQuery(tunnel.lastRequest.Query)
		if err != nil {
			t.Fatal(err)
		}
		assertRunIDQuery(t, "control-channel restart", q, runID)
	}
}

func assertRunIDQuery(t *testing.T, what string, q url.Values, runID string) {
	t.Helper()
	if q.Get("projectId") != "p1" {
		t.Errorf("%s: projectId = %q, want p1 (query %v)", what, q.Get("projectId"), q)
	}
	if runID == "" {
		if q.Has("runId") {
			t.Errorf("%s: runId sent without a run: %v", what, q)
		}
		return
	}
	if got := q.Get("runId"); got != runID {
		t.Errorf("%s: runId = %q, want %q (query %v)", what, got, runID, q)
	}
}

func TestWithRunIDURLAndQuery(t *testing.T) {
	for _, tc := range []struct{ in, run, want string }{
		{"/x", "", "/x"},
		{"/x", "r 1", "/x?runId=r+1"},
		{"/x?projectId=p", "r", "/x?projectId=p&runId=r"},
	} {
		if got := withRunIDURL(tc.in, tc.run); got != tc.want {
			t.Errorf("withRunIDURL(%q, %q) = %q, want %q", tc.in, tc.run, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, run, want string }{
		{"", "", ""},
		{"", "r", "runId=r"},
		{"projectId=p", "r", "projectId=p&runId=r"},
	} {
		if got := withRunIDQuery(tc.in, tc.run); got != tc.want {
			t.Errorf("withRunIDQuery(%q, %q) = %q, want %q", tc.in, tc.run, got, tc.want)
		}
	}
}
