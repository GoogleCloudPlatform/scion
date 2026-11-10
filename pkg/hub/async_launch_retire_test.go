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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func assertRetiredAsyncAgentLaunch422(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rr.Code, rr.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := "server.hub.async_agent_launch is retired and ignored; use the hub.async_agent_launch experiment (on by default)"
	if resp.Error.Message != want {
		t.Errorf("message = %q, want %q", resp.Error.Message, want)
	}
	if resp.Error.Code != ErrCodeValidationError {
		t.Errorf("code = %q, want %q", resp.Error.Code, ErrCodeValidationError)
	}
}

// TestPutServerConfig_RetiredAsyncAgentLaunch422 pins that both admin PUT
// handlers refuse the retired server.hub.async_agent_launch, whatever its
// value, instead of silently dropping it (ptone/scion#4991).
func TestPutServerConfig_RetiredAsyncAgentLaunch422(t *testing.T) {
	for _, v := range []string{`true`, `false`, `null`} {
		body := `{"server":{"hub":{"async_agent_launch":` + v + `}}}`

		t.Run("db "+v, func(t *testing.T) {
			srv, st, ops := newTestDBServer(t)
			rr := httptest.NewRecorder()
			srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
			assertRetiredAsyncAgentLaunch422(t, rr)
			if rows, _ := st.ListHubSettings(context.Background()); len(rows) != 0 {
				t.Errorf("rejected PUT wrote hub_settings: %+v", rows)
			}
		})

		t.Run("file "+v, func(t *testing.T) {
			rr, settingsPath := fileModePutServerConfig(t, &Server{}, body)
			assertRetiredAsyncAgentLaunch422(t, rr)
			if data, err := os.ReadFile(settingsPath); !os.IsNotExist(err) {
				t.Errorf("rejected PUT wrote settings.yaml (err %v):\n%s", err, data)
			}
		})
	}
}

func TestRetiredAsyncAgentLaunchInBody(t *testing.T) {
	tests := []struct {
		body string
		want bool
	}{
		{body: `{"server":{"hub":{"async_agent_launch":true}}}`, want: true},
		{body: `{"server":{"hub":{"async_agent_launch":null}}}`, want: true},
		{body: `{"server":{"hub":{"launch_timeout":"10m"}}}`, want: false},
		{body: `{"server":{"hub":null}}`, want: false},
		{body: `{"server":null}`, want: false},
		{body: `{"agent_defaults":{"default_template":"claude"}}`, want: false},
		{body: `not json`, want: false},
	}
	for _, tt := range tests {
		if got := retiredAsyncAgentLaunchInBody([]byte(tt.body)); got != tt.want {
			t.Errorf("retiredAsyncAgentLaunchInBody(%s) = %v, want %v", tt.body, got, tt.want)
		}
	}
}
