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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tlsRaw returns the raw cloud.tls value of a telemetry section document,
// and whether it is present.
func tlsRaw(t *testing.T, doc []byte) (json.RawMessage, bool) {
	t.Helper()
	var sec map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(doc, &sec))
	var cloud map[string]json.RawMessage
	if c, ok := sec["cloud"]; ok {
		require.NoError(t, json.Unmarshal(c, &cloud))
	}
	v, ok := cloud["tls"]
	return v, ok
}

// seedTelemetry stores doc as the managed telemetry row and refreshes ops.
func seedTelemetry(t *testing.T, fake *fakeHubSettingStore, ops *OperationalSettings, doc string) {
	t.Helper()
	fake.seedWithOrigin("telemetry", json.RawMessage(doc), "managed")
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
}

func TestTelemetryTLSTouched(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"no telemetry", `{"default_model":"m1"}`, false},
		{"telemetry without cloud", `{"telemetry":{"enabled":true}}`, false},
		{"cloud without tls", `{"telemetry":{"cloud":{"endpoint":"e:4317"}}}`, false},
		{"tls object", `{"telemetry":{"cloud":{"tls":{"ca_file":"/certs/ca.pem"}}}}`, true},
		{"empty tls object", `{"telemetry":{"cloud":{"tls":{}}}}`, true},
		{"tls null", `{"telemetry":{"cloud":{"tls":null}}}`, true},
		{"case-variant TLS", `{"telemetry":{"cloud":{"TLS":{"cert_file":"/certs/c.pem"}}}}`, true},
		{"case-variant Cloud", `{"telemetry":{"Cloud":{"tls":{}}}}`, true},
		{"cloud null", `{"telemetry":{"cloud":null}}`, true},
		{"cloud not an object", `{"telemetry":{"cloud":"x"}}`, false},
		{"telemetry null", `{"telemetry":null}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, telemetryTLSTouched([]byte(tc.body)))
		})
	}
}

// TestTelemetryTLSSaveErrors checks the save-time rule with an injected
// logger (no process-global slog default is swapped): a touching save gets
// the error; an untouched invalid stored tls is not an error and is logged.
func TestTelemetryTLSSaveErrors(t *testing.T) {
	const halfPair = `{"cloud":{"tls":{"cert_file":"/certs/c.pem"}}}`
	const valid = `{"cloud":{"tls":{"cert_file":"/certs/c.pem","key_file":"/certs/k.pem"}}}`
	for _, tc := range []struct {
		name     string
		doc      string
		touched  bool
		wantErrs int
		wantLog  bool
	}{
		{"touched invalid", halfPair, true, 1, false},
		{"untouched invalid", halfPair, false, 0, true},
		{"touched valid", valid, true, 0, false},
		{"untouched valid", valid, false, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			errs := telemetryTLSSaveErrors(json.RawMessage(tc.doc), tc.touched, logger)
			require.Len(t, errs, tc.wantErrs, "errors: %v", errs)
			if tc.wantErrs > 0 {
				assert.Equal(t, "cloud/tls", errs[0].Path)
			}
			logged := buf.String()
			if tc.wantLog {
				assert.Contains(t, logged, "telemetry.cloud.tls", "the warning must name the setting")
				assert.Contains(t, logged, "cert_file and key_file must be set together", "the warning must name the rule")
			} else {
				assert.Empty(t, logged)
			}
		})
	}
}

// TestPutServerConfigDB_StoredInvalidTLSUnrelatedSaveKept: a telemetry save
// that does not touch cloud.tls succeeds while the stored tls is invalid,
// applies its change, and keeps the stored tls byte for byte.
func TestPutServerConfigDB_StoredInvalidTLSUnrelatedSaveKept(t *testing.T) {
	for _, tc := range []struct{ name, stored string }{
		{"half pair", `{"cloud":{"endpoint":"old.example.com:4317","tls":{"cert_file":"/certs/client.pem"}}}`},
		{"plaintext conflict", `{"cloud":{"endpoint":"old.example.com:4317","tls":{"enabled":false,"ca_file":"/certs/ca.pem"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv, fake, ops := newTestDBServer(t)
			seedTelemetry(t, fake, ops, tc.stored)
			seededTLS, ok := tlsRaw(t, []byte(tc.stored))
			require.True(t, ok)

			rr := putServerConfigDB(t, srv, ops, `{"telemetry":{"cloud":{"endpoint":"new.example.com:4317"}}}`)
			require.Equal(t, http.StatusOK, rr.Code, "an unrelated save must not fail on stored tls: %s", rr.Body.String())

			row := storedRow(fake, "telemetry")
			require.NotNil(t, row)
			endpoint, _ := valueAtPath(decodeJSONValue(t, row.Value), []string{"cloud", "endpoint"})
			assert.Equal(t, "new.example.com:4317", endpoint, "the save's change must be applied")
			gotTLS, ok := tlsRaw(t, row.Value)
			require.True(t, ok, "the stored tls was removed; row %s", row.Value)
			assert.Equal(t, string(seededTLS), string(gotTLS), "the stored tls must be kept byte for byte")
		})
	}
}

// TestPutServerConfigDB_TelemetryTLSInvalidPairRejected: a save that sets or
// changes cloud.tls to an invalid merged value gets 400 naming cloud/tls,
// and the stored row is not written.
func TestPutServerConfigDB_TelemetryTLSInvalidPairRejected(t *testing.T) {
	for _, tc := range []struct{ name, stored, body string }{
		{"body sends cert only",
			`{"cloud":{"endpoint":"e.example.com:4317","tls":{"ca_file":"/certs/ca.pem"}}}`,
			`{"telemetry":{"cloud":{"tls":{"cert_file":"/certs/client.pem"}}}}`},
		{"body sets plaintext over a stored ca_file",
			`{"cloud":{"endpoint":"e.example.com:4317","tls":{"ca_file":"/certs/ca.pem"}}}`,
			`{"telemetry":{"cloud":{"tls":{"enabled":false}}}}`},
		{"body clears key_file of a stored pair",
			`{"cloud":{"endpoint":"e.example.com:4317","tls":{"cert_file":"/certs/client.pem","key_file":"/certs/client-key.pem"}}}`,
			`{"telemetry":{"cloud":{"tls":{"key_file":null}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv, fake, ops := newTestDBServer(t)
			seedTelemetry(t, fake, ops, tc.stored)
			before := storedRow(fake, "telemetry")
			require.NotNil(t, before)

			rr := putServerConfigDB(t, srv, ops, tc.body)
			require.Equal(t, http.StatusBadRequest, rr.Code, "an invalid tls save must be refused: %s", rr.Body.String())
			var resp struct {
				Errors map[string][]struct{ Path, Message string } `json:"errors"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp), rr.Body.String())
			require.NotEmpty(t, resp.Errors["telemetry"], rr.Body.String())
			assert.Equal(t, "cloud/tls", resp.Errors["telemetry"][0].Path)
			assert.Contains(t, resp.Errors["telemetry"][0].Message, "telemetry.cloud.tls")

			after := storedRow(fake, "telemetry")
			require.NotNil(t, after)
			assert.Equal(t, string(before.Value), string(after.Value), "a refused save must not write the row")
			assert.Equal(t, before.Revision, after.Revision, "a refused save must not write the row")
		})
	}
}

// TestPutServerConfigDB_StoredInvalidTLSFixedBySave: a save that touches tls
// with a valid merged result succeeds and replaces the invalid stored tls.
func TestPutServerConfigDB_StoredInvalidTLSFixedBySave(t *testing.T) {
	const stored = `{"cloud":{"endpoint":"e.example.com:4317","tls":{"cert_file":"/certs/client.pem"}}}`
	for _, tc := range []struct {
		name    string
		body    string
		wantTLS map[string]any // nil: tls removed
	}{
		{"completes the pair", `{"telemetry":{"cloud":{"tls":{"key_file":"/certs/client-key.pem"}}}}`,
			map[string]any{"cert_file": "/certs/client.pem", "key_file": "/certs/client-key.pem"}},
		{"clears tls", `{"telemetry":{"cloud":{"tls":null}}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv, fake, ops := newTestDBServer(t)
			seedTelemetry(t, fake, ops, stored)

			rr := putServerConfigDB(t, srv, ops, tc.body)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			row := storedRow(fake, "telemetry")
			require.NotNil(t, row)
			got, ok := valueAtPath(decodeJSONValue(t, row.Value), []string{"cloud", "tls"})
			if tc.wantTLS == nil {
				assert.False(t, ok, "tls must be cleared; row %s", row.Value)
				return
			}
			require.True(t, ok, "row %s", row.Value)
			assert.Equal(t, tc.wantTLS, got)
		})
	}
}

// TestPutServerConfigDB_StoredInvalidTLSOtherSectionSaveUnaffected: a save of
// another section neither fails on nor touches an invalid stored tls.
func TestPutServerConfigDB_StoredInvalidTLSOtherSectionSaveUnaffected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, fake, ops := newTestDBServer(t)
	seedTelemetry(t, fake, ops, `{"cloud":{"endpoint":"e.example.com:4317","tls":{"cert_file":"/certs/client.pem"}}}`)
	before := storedRow(fake, "telemetry")
	require.NotNil(t, before)

	rr := putServerConfigDB(t, srv, ops, `{"default_model":"m1"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	after := storedRow(fake, "telemetry")
	require.NotNil(t, after)
	assert.Equal(t, string(before.Value), string(after.Value))
	assert.Equal(t, before.Revision, after.Revision)
}

// putServerConfigFile sends a file-mode server-config PUT.
func putServerConfigFile(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	(&Server{}).handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
	return rr
}

const fileModeTelemetrySettings = `schema_version: "1"
telemetry:
  cloud:
    endpoint: otel.example.com:4317
`

func TestPutServerConfig_FileMode_TelemetryTLSInvalidPairRejected(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"cert only", `{"telemetry":{"cloud":{"endpoint":"otel.example.com:4317","tls":{"cert_file":"/certs/client.pem"}}}}`},
		{"plaintext with CA", `{"telemetry":{"cloud":{"endpoint":"otel.example.com:4317","tls":{"enabled":false,"ca_file":"/certs/ca.pem"}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settingsPath := setTempScionHome(t)
			require.NoError(t, os.WriteFile(settingsPath, []byte(fileModeTelemetrySettings), 0o600))

			rr := putServerConfigFile(t, tc.body)
			require.Equal(t, http.StatusBadRequest, rr.Code, "an invalid tls save must be refused: %s", rr.Body.String())
			assert.Contains(t, rr.Body.String(), "telemetry.cloud.tls")
			data, err := os.ReadFile(settingsPath)
			require.NoError(t, err)
			assert.Equal(t, fileModeTelemetrySettings, string(data), "a refused save must not write the file")
		})
	}
}

func TestPutServerConfig_FileMode_TelemetryTLSValidPairSaved(t *testing.T) {
	settingsPath := setTempScionHome(t)
	require.NoError(t, os.WriteFile(settingsPath, []byte(fileModeTelemetrySettings), 0o600))

	rr := putServerConfigFile(t, `{"telemetry":{"cloud":{"endpoint":"otel.example.com:4317","tls":{"cert_file":"/certs/client.pem","key_file":"/certs/client-key.pem"}}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(data), "cert_file: /certs/client.pem") && strings.Contains(string(data), "key_file: /certs/client-key.pem"),
		"the pair must be written; file:\n%s", data)
}
