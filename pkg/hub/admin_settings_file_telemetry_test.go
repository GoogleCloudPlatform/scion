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
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"
)

// fileTelemetrySeed is a settings.yaml telemetry section with the fields
// the settings page does not show (filter, resource, cloud headers, tls,
// batch) next to the ones it does.
const fileTelemetrySeed = `schema_version: "1"
telemetry:
  enabled: true
  cloud:
    endpoint: otel.example.com:4317
    protocol: grpc
    headers:
      x-api-key: real-key
    tls:
      enabled: true
      ca_file: /etc/ssl/ca.pem
    batch:
      max_size: 512
      timeout: 5s
  filter:
    events:
      include:
        - agent.start
    sampling:
      rates:
        agent.tool: 0.5
  resource:
    team: core
`

// putFileTelemetry seeds settings.yaml, sends body to the file-mode
// server-config PUT and returns the stored telemetry section.
func putFileTelemetry(t *testing.T, seed, body string) *config.V1TelemetryConfig {
	t.Helper()
	settingsPath := setTempScionHome(t)
	require.NoError(t, os.WriteFile(settingsPath, []byte(seed), 0600))
	rr := httptest.NewRecorder()
	(&Server{}).handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	var vs config.VersionedSettings
	require.NoError(t, yamlv3.Unmarshal(data, &vs))
	require.NotNil(t, vs.Telemetry)
	return vs.Telemetry
}

// A file-mode telemetry save keeps the nested keys it omits, as the DB
// path does (ptone/scion#4047).
func TestPutServerConfig_FileMode_TelemetrySaveKeepsOmittedNestedKeys(t *testing.T) {
	tel := putFileTelemetry(t, fileTelemetrySeed,
		`{"telemetry":{"enabled":false,"cloud":{"endpoint":"otel.example.com:4318","protocol":"http"}}}`)

	require.NotNil(t, tel.Enabled)
	assert.False(t, *tel.Enabled)
	require.NotNil(t, tel.Cloud)
	assert.Equal(t, "otel.example.com:4318", tel.Cloud.Endpoint)
	assert.Equal(t, "http", tel.Cloud.Protocol)
	assert.Equal(t, map[string]string{"x-api-key": "real-key"}, tel.Cloud.Headers)
	require.NotNil(t, tel.Cloud.TLS)
	require.NotNil(t, tel.Cloud.TLS.Enabled)
	assert.True(t, *tel.Cloud.TLS.Enabled)
	assert.Equal(t, "/etc/ssl/ca.pem", tel.Cloud.TLS.CAFile)
	require.NotNil(t, tel.Cloud.Batch)
	assert.Equal(t, 512, tel.Cloud.Batch.MaxSize)
	assert.Equal(t, "5s", tel.Cloud.Batch.Timeout)
	require.NotNil(t, tel.Filter)
	require.NotNil(t, tel.Filter.Events)
	assert.Equal(t, []string{"agent.start"}, tel.Filter.Events.Include)
	require.NotNil(t, tel.Filter.Sampling)
	assert.Equal(t, map[string]float64{"agent.tool": 0.5}, tel.Filter.Sampling.Rates)
	assert.Equal(t, map[string]string{"team": "core"}, tel.Resource)
}

// A nested key merged key by key: a sent tls member changes, the tls
// members the body omits stay.
func TestPutServerConfig_FileMode_TelemetryNestedObjectMergesKeyByKey(t *testing.T) {
	tel := putFileTelemetry(t, fileTelemetrySeed,
		`{"telemetry":{"cloud":{"tls":{"ca_file":"/etc/ssl/other.pem"}}}}`)

	require.NotNil(t, tel.Cloud)
	assert.Equal(t, "otel.example.com:4317", tel.Cloud.Endpoint)
	require.NotNil(t, tel.Cloud.TLS)
	require.NotNil(t, tel.Cloud.TLS.Enabled)
	assert.True(t, *tel.Cloud.TLS.Enabled)
	assert.Equal(t, "/etc/ssl/other.pem", tel.Cloud.TLS.CAFile)
	require.NotNil(t, tel.Cloud.Batch)
	assert.Equal(t, 512, tel.Cloud.Batch.MaxSize)
}

// An explicit null clears a nested key and leaves its siblings.
func TestPutServerConfig_FileMode_TelemetryExplicitNullClearsNestedKey(t *testing.T) {
	tel := putFileTelemetry(t, fileTelemetrySeed,
		`{"telemetry":{"cloud":{"tls":null},"filter":{"sampling":null}}}`)

	require.NotNil(t, tel.Cloud)
	assert.Nil(t, tel.Cloud.TLS)
	assert.Equal(t, "otel.example.com:4317", tel.Cloud.Endpoint)
	require.NotNil(t, tel.Cloud.Batch)
	assert.Equal(t, 512, tel.Cloud.Batch.MaxSize)
	require.NotNil(t, tel.Filter)
	assert.Nil(t, tel.Filter.Sampling)
	require.NotNil(t, tel.Filter.Events)
	assert.Equal(t, []string{"agent.start"}, tel.Filter.Events.Include)
	assert.Equal(t, map[string]string{"team": "core"}, tel.Resource)
}

// Free-form maps and arrays are replaced as a whole, not merged.
func TestPutServerConfig_FileMode_TelemetryFreeFormMapsAndArraysReplace(t *testing.T) {
	tel := putFileTelemetry(t, fileTelemetrySeed,
		`{"telemetry":{"resource":{"env":"prod"},"cloud":{"headers":{"x-other":"v"}},"filter":{"events":{"include":["agent.stop"]}}}}`)

	assert.Equal(t, map[string]string{"env": "prod"}, tel.Resource)
	require.NotNil(t, tel.Cloud)
	assert.Equal(t, map[string]string{"x-other": "v"}, tel.Cloud.Headers)
	require.NotNil(t, tel.Cloud.TLS)
	assert.Equal(t, "/etc/ssl/ca.pem", tel.Cloud.TLS.CAFile)
	require.NotNil(t, tel.Filter)
	require.NotNil(t, tel.Filter.Events)
	assert.Equal(t, []string{"agent.stop"}, tel.Filter.Events.Include)
	require.NotNil(t, tel.Filter.Sampling)
	assert.Equal(t, map[string]float64{"agent.tool": 0.5}, tel.Filter.Sampling.Rates)
}

// A save with no telemetry stored creates the section from the sent keys.
func TestPutServerConfig_FileMode_TelemetrySaveWithNoStoredSection(t *testing.T) {
	tel := putFileTelemetry(t, "schema_version: \"1\"\n",
		`{"telemetry":{"enabled":true,"cloud":{"endpoint":"otel.example.com:4317"}}}`)

	require.NotNil(t, tel.Enabled)
	assert.True(t, *tel.Enabled)
	require.NotNil(t, tel.Cloud)
	assert.Equal(t, "otel.example.com:4317", tel.Cloud.Endpoint)
	assert.Nil(t, tel.Filter)
	assert.Nil(t, tel.Resource)
}
