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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#5664 (interim): telemetry.cloud.cloud_logging is read only at
// startup, so a database-backed server-config PUT refuses a changed value
// with a structured 422 and writes nothing, while an unchanged echo of GET
// is accepted and not written.

const storedTelemetryCloudLogging = `{"enabled":true,"cloud":{"enabled":true,"endpoint":"otel.example.com:4317","cloud_logging":true}}`

// cloudLoggingRefusal decodes the structured 422 of a refused write.
func cloudLoggingRefusal(t *testing.T, rr *httptest.ResponseRecorder) (code string, keys []string) {
	t.Helper()
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	var body struct {
		Error   string   `json:"error"`
		Message string   `json:"message"`
		Keys    []string `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), rr.Body.String())
	assert.NotEmpty(t, body.Message)
	return body.Error, body.Keys
}

func newCloudLoggingDBServer(t *testing.T) (*Server, *fakeHubSettingStore, *OperationalSettings) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	srv, fake, ops := newTestDBServer(t)
	fake.seedWithOrigin("telemetry", json.RawMessage(storedTelemetryCloudLogging), "managed")
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	return srv, fake, ops
}

func TestPutServerConfigDB_CloudLoggingChangeRefused(t *testing.T) {
	for name, body := range map[string]string{
		"changed":         `{"telemetry":{"cloud":{"cloud_logging":false}}}`,
		"null clears":     `{"telemetry":{"cloud":{"cloud_logging":null}}}`,
		"flat dotted":     `{"telemetry.cloud.cloud_logging":false}`,
		"partly dotted":   `{"telemetry.cloud":{"cloud_logging":false}}`,
		"case folded":     `{"Telemetry":{"CLOUD":{"Cloud_Logging":false}}}`,
		"with other keys": `{"default_model":"m1","telemetry":{"enabled":true,"cloud":{"cloud_logging":false}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv, fake, ops := newCloudLoggingDBServer(t)
			before := storedRow(fake, "telemetry").Revision
			rr := putServerConfigDB(t, srv, ops, body)
			code, keys := cloudLoggingRefusal(t, rr)
			assert.Equal(t, startupOnlyKeyRejectedCode, code)
			assert.Equal(t, []string{telemetryCloudLoggingKey}, keys)
			row := storedRow(fake, "telemetry")
			assert.Equal(t, before, row.Revision, "a refused PUT writes no telemetry row")
			assert.JSONEq(t, storedTelemetryCloudLogging, string(row.Value))
			assert.Nil(t, storedRow(fake, "general"), "a refused PUT writes no other row")
		})
	}
}

func TestPutServerConfigDB_CloudLoggingEchoAccepted(t *testing.T) {
	srv, fake, ops := newCloudLoggingDBServer(t)
	// GET shows the stored value, so sending it back unchanged is a 200.
	got := getServerConfigDB(t, srv, ops)
	require.NotNil(t, got.Telemetry)
	require.NotNil(t, got.Telemetry.Cloud)
	require.NotNil(t, got.Telemetry.Cloud.CloudLogging)
	require.True(t, *got.Telemetry.Cloud.CloudLogging)

	before := storedRow(fake, "telemetry").Revision
	rr := putServerConfigDB(t, srv, ops, `{"telemetry":{"cloud":{"cloud_logging":true}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	row := storedRow(fake, "telemetry")
	assert.Equal(t, before, row.Revision, "an echo alone writes nothing")
	assert.JSONEq(t, storedTelemetryCloudLogging, string(row.Value))

	// An echo next to a real change: the change is saved, cloud_logging
	// stays as stored.
	rr = putServerConfigDB(t, srv, ops, `{"telemetry":{"cloud":{"endpoint":"other.example.com:4317","cloud_logging":true}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	cloud, _ := valueAtPath(decodeJSONValue(t, storedRow(fake, "telemetry").Value), []string{"cloud"})
	assert.Equal(t, "other.example.com:4317", cloud.(map[string]any)["endpoint"])
	assert.Equal(t, true, cloud.(map[string]any)["cloud_logging"])
}

// A whole GET body sent back is not refused for cloud_logging.
func TestPutServerConfigDB_CloudLoggingGetRoundTrip(t *testing.T) {
	srv, _, ops := newCloudLoggingDBServer(t)
	getRR := httptest.NewRecorder()
	srv.handleGetServerConfigDB(getRR, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, getRR.Code, getRR.Body.String())
	rr := putServerConfigDB(t, srv, ops, getRR.Body.String())
	assert.NotContains(t, rr.Body.String(), startupOnlyKeyRejectedCode, rr.Body.String())
}

// Where GET shows no value, false and null are echoes of the omitted
// member; true is a change.
func TestPutServerConfigDB_CloudLoggingUnsetEchoes(t *testing.T) {
	for _, tc := range []struct {
		body     string
		wantCode int
	}{
		{`{"telemetry":{"cloud":{"cloud_logging":false}}}`, http.StatusOK},
		{`{"telemetry":{"cloud":{"cloud_logging":null}}}`, http.StatusOK},
		{`{"telemetry":{"cloud":{"cloud_logging":true}}}`, http.StatusUnprocessableEntity},
	} {
		t.Run(tc.body, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv, fake, ops := newTestDBServer(t)
			rr := putServerConfigDB(t, srv, ops, tc.body)
			require.Equal(t, tc.wantCode, rr.Code, rr.Body.String())
			if row := storedRow(fake, "telemetry"); row != nil {
				assert.NotContains(t, string(row.Value), "cloud_logging", "nothing is stored for the key")
			}
		})
	}
}
