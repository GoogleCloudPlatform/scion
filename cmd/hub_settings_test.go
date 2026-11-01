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

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSettingsHub serves the admin settings endpoints from fixed bodies and
// records every write.
type fakeSettingsHub struct {
	mu     sync.Mutex
	writes []fakeSettingsWrite

	// putStatus and putBody answer writes (PUT and DELETE); 0 means 200
	// with a default body.
	putStatus int
	putBody   string
}

type fakeSettingsWrite struct {
	Method string
	Path   string
	Body   map[string]json.RawMessage
}

func newFakeSettingsHub(t *testing.T) (*fakeSettingsHub, hubclient.AdminSettingsService) {
	t.Helper()
	f := &fakeSettingsHub{}
	schema, err := json.Marshal(map[string]interface{}{"sections": testSettingsSchema()})
	require.NoError(t, err)
	cfg, err := json.Marshal(testServerConfig(t))
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			switch r.URL.Path {
			case "/api/v1/admin/server-config/schema":
				_, _ = w.Write(schema)
			case "/api/v1/admin/server-config":
				_, _ = w.Write(cfg)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		rec := fakeSettingsWrite{Method: r.Method, Path: r.URL.Path}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &rec.Body)
		}
		f.mu.Lock()
		f.writes = append(f.writes, rec)
		f.mu.Unlock()
		if f.putStatus != 0 {
			w.WriteHeader(f.putStatus)
			_, _ = io.WriteString(w, f.putBody)
			return
		}
		switch r.URL.Path {
		case "/api/v1/admin/server-config":
			_, _ = io.WriteString(w, `{"status":"saved","reload":{"applied":["agent_defaults"],"requires_restart":[],"applied_keys":["default_max_turns"],"pending_restart":["server.hub.public_url"],"skipped":[{"key":"image_registry","reason":"not used by this hub"}],"file_applied":[]},"unchanged":["endpoints"],"warnings":["check the value"]}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return f, c.AdminSettings()
}

// withTextOutput runs the test with the global --format unset.
func withTextOutput(t *testing.T) {
	t.Helper()
	prev := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = prev })
}

func TestHubSettingsSet_SendsPlanAndPrintsReport(t *testing.T) {
	withTextOutput(t)
	f, svc := newFakeSettingsHub(t)
	var out, errw bytes.Buffer
	err := runHubSettingsWrite(context.Background(), svc, &out, &errw,
		[]settingsChange{{Key: "default_max_turns", Value: 200}}, srvcfgWriteOptions{})
	require.NoError(t, err)

	require.Len(t, f.writes, 1)
	w := f.writes[0]
	assert.Equal(t, http.MethodPut, w.Method)
	assert.JSONEq(t, `200`, string(w.Body["default_max_turns"]))
	assert.JSONEq(t, `{"agent_defaults":7}`, string(w.Body["expected_revisions"]))

	assert.Contains(t, out.String(), "Saved: agent_defaults.")
	assert.Contains(t, out.String(), "Unchanged: endpoints.")
	assert.Contains(t, out.String(), "In effect now: default_max_turns")
	assert.Contains(t, out.String(), "Takes effect after a Hub restart: server.hub.public_url")
	assert.Contains(t, out.String(), "Saved but not in use: image_registry (not used by this hub)")
	assert.Contains(t, errw.String(), "Warning: check the value")
}

// TestHubSettingsSet_BelowSchemaKeySendsWholeValueWithRevision pins the
// read-modify-write for a key below a schema key: the PUT carries the whole
// map read with GET (other entries kept), changed at the one key, and the
// section's revision from that GET guards it.
func TestHubSettingsSet_BelowSchemaKeySendsWholeValueWithRevision(t *testing.T) {
	withTextOutput(t)
	f, svc := newFakeSettingsHub(t)
	var out, errw bytes.Buffer
	err := runHubSettingsWrite(context.Background(), svc, &out, &errw,
		[]settingsChange{{Key: "runtimes.k8s.namespace", Value: "agents"}}, srvcfgWriteOptions{})
	require.NoError(t, err)
	require.Len(t, f.writes, 1)
	w := f.writes[0]
	assert.Equal(t, http.MethodPut, w.Method)
	assert.JSONEq(t, `{"docker":{"type":"docker","image":"old"},"k8s":{"type":"kubernetes","namespace":"agents"}}`, string(w.Body["runtimes"]))
	assert.JSONEq(t, `{"runtimes":9}`, string(w.Body["expected_revisions"]))
	assert.Len(t, w.Body, 2, "only the runtimes value and expected_revisions")

	// Removing an entry sends the map without it, under the same guard.
	err = runHubSettingsWrite(context.Background(), svc, &out, &errw,
		[]settingsChange{{Key: "runtimes.docker", Unset: true}}, srvcfgWriteOptions{})
	require.NoError(t, err)
	require.Len(t, f.writes, 2)
	assert.JSONEq(t, `{"k8s":{"type":"kubernetes"}}`, string(f.writes[1].Body["runtimes"]))
	assert.JSONEq(t, `{"runtimes":9}`, string(f.writes[1].Body["expected_revisions"]))
}

func TestHubSettingsSet_DryRunSendsNothing(t *testing.T) {
	withTextOutput(t)
	f, svc := newFakeSettingsHub(t)
	var out, errw bytes.Buffer
	err := runHubSettingsWrite(context.Background(), svc, &out, &errw,
		[]settingsChange{{Key: "runtimes.docker.image", Value: "new"}}, srvcfgWriteOptions{DryRun: true})
	require.NoError(t, err)
	assert.Empty(t, f.writes)
	assert.JSONEq(t, `{"runtimes":{"docker":{"type":"docker","image":"new"},"k8s":{"type":"kubernetes"}},"expected_revisions":{"runtimes":9}}`, out.String())
}

func TestHubSettingsSet_FromFile(t *testing.T) {
	withTextOutput(t)
	f, svc := newFakeSettingsHub(t)
	path := filepath.Join(t.TempDir(), "settings.yaml")
	require.NoError(t, os.WriteFile(path, []byte("default_max_turns: 9\nserver:\n  hub:\n    public_url: null\n"), 0o644))
	var out, errw bytes.Buffer
	err := runHubSettingsWrite(context.Background(), svc, &out, &errw, nil, srvcfgWriteOptions{File: path})
	require.NoError(t, err)
	require.Len(t, f.writes, 1)
	assert.JSONEq(t, `9`, string(f.writes[0].Body["default_max_turns"]))
	assert.JSONEq(t, `{"hub":{"public_url":null}}`, string(f.writes[0].Body["server"]))
	assert.JSONEq(t, `{"agent_defaults":7,"endpoints":3}`, string(f.writes[0].Body["expected_revisions"]))
}

func TestHubSettingsSet_StructuredConflictIsReported(t *testing.T) {
	withTextOutput(t)
	f, svc := newFakeSettingsHub(t)
	f.putStatus = http.StatusConflict
	f.putBody = `{"error":{"code":"revision_conflict","message":"One or more sections have been modified since the expected revision.","details":{"conflicted":[{"section":"endpoints","expected_revision":3,"current_revision":4}],"applied":{"agent_defaults":8}}}}`
	var out, errw bytes.Buffer
	err := runHubSettingsWrite(context.Background(), svc, &out, &errw,
		[]settingsChange{{Key: "default_max_turns", Value: 1}, {Key: "server.hub.public_url", Value: "https://x"}}, srvcfgWriteOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "revision_conflict, HTTP 409")
	assert.Contains(t, errw.String(), "section endpoints: expected revision 3, current revision 4")
	assert.Contains(t, errw.String(), "already saved before the conflict: agent_defaults (revision 8)")
	assert.Contains(t, errw.String(), "--revision SECTION=N")
}

func TestHubSettingsSet_NonRevision409PrintsHubMessageOnly(t *testing.T) {
	withTextOutput(t)
	for code, msg := range map[string]string{
		"gcp_iam_change_refused": "relaxing the GCP permission check is refused",
		"legacy_server_yaml":     "The server configuration is still read from the deprecated server.yaml.",
	} {
		f, svc := newFakeSettingsHub(t)
		f.putStatus = http.StatusConflict
		f.putBody = `{"error":{"code":"` + code + `","message":"` + msg + `"}}`
		var out, errw bytes.Buffer
		err := runHubSettingsWrite(context.Background(), svc, &out, &errw,
			[]settingsChange{{Key: "default_max_turns", Value: 1}}, srvcfgWriteOptions{})
		require.Error(t, err, code)
		assert.Contains(t, err.Error(), msg, code)
		assert.Contains(t, err.Error(), code+", HTTP 409", code)
		assert.NotContains(t, errw.String(), "changed since they were read", code)
		assert.NotContains(t, errw.String(), "--revision", code)
	}
}

// TestHubSettingsSet_DryRunMasksSecretValues pins the dry-run masking: the
// values of secret-named keys and of the positions the Hub masks itself are
// replaced, and everything else is printed as given.
func TestHubSettingsSet_DryRunMasksSecretValues(t *testing.T) {
	withTextOutput(t)
	f, svc := newFakeSettingsHub(t)
	var out, errw bytes.Buffer
	err := runHubSettingsWrite(context.Background(), svc, &out, &errw, []settingsChange{
		{Key: "server.github_app.webhook_secret", Value: "s3cr3t-value"},
		{Key: "telemetry.cloud.headers", Value: map[string]interface{}{"Authorization": "Bearer abc"}},
		{Key: "default_max_turns", Value: 5},
	}, srvcfgWriteOptions{DryRun: true, Plan: settingsPlanOptions{AllowUnknown: true}})
	require.NoError(t, err)
	assert.Empty(t, f.writes)
	assert.NotContains(t, out.String(), "s3cr3t-value")
	assert.NotContains(t, out.String(), "Bearer abc")
	assert.JSONEq(t, `{"server":{"github_app":{"webhook_secret":"********"}},"telemetry":{"cloud":{"headers":{"Authorization":"********"}}},"default_max_turns":5,"expected_revisions":{"agent_defaults":7,"github_app":1,"telemetry":0}}`, out.String())
}

func TestHubSettingsSet_FlatValidationErrorIsReported(t *testing.T) {
	withTextOutput(t)
	f, svc := newFakeSettingsHub(t)
	f.putStatus = http.StatusBadRequest
	f.putBody = `{"error":"validation_failed","errors":{"agent_defaults":[{"Path":"/default_max_turns","Message":"must be >= 0"}]}}`
	var out, errw bytes.Buffer
	err := runHubSettingsWrite(context.Background(), svc, &out, &errw,
		[]settingsChange{{Key: "default_max_turns", Value: -1}}, srvcfgWriteOptions{})
	require.Error(t, err)
	assert.Contains(t, errw.String(), "agent_defaults:default_max_turns: must be >= 0")
}

func TestHubSettingsSet_Structured422IsReported(t *testing.T) {
	withTextOutput(t)
	f, svc := newFakeSettingsHub(t)
	f.putStatus = http.StatusUnprocessableEntity
	f.putBody = `{"error":{"code":"validation_error","message":"server.auth.default_user_role: must be one of","details":{"field":"server.auth.default_user_role","errors":[{"section":"access","field":"server.auth.default_user_role","message":"must be one of"}]}}}`
	var out, errw bytes.Buffer
	err := runHubSettingsWrite(context.Background(), svc, &out, &errw,
		[]settingsChange{{Key: "server.auth.default_user_role", Value: "boss"}}, srvcfgWriteOptions{})
	require.Error(t, err)
	assert.Contains(t, errw.String(), "server.auth.default_user_role (access): must be one of")
	assert.Contains(t, err.Error(), "HTTP 422")
}

func TestHubSettingsSet_ForbiddenNamesScope(t *testing.T) {
	withTextOutput(t)
	f, svc := newFakeSettingsHub(t)
	f.putStatus = http.StatusForbidden
	f.putBody = `{"error":{"code":"forbidden","message":"Forbidden"}}`
	var out, errw bytes.Buffer
	err := runHubSettingsWrite(context.Background(), svc, &out, &errw,
		[]settingsChange{{Key: "default_max_turns", Value: 1}}, srvcfgWriteOptions{})
	require.Error(t, err)
	assert.Contains(t, errw.String(), "hub_config:update")
}

func TestHubSettingsSet_JSONErrorOutput(t *testing.T) {
	prev := outputFormat
	outputFormat = "json"
	t.Cleanup(func() { outputFormat = prev })
	f, svc := newFakeSettingsHub(t)
	f.putStatus = http.StatusConflict
	f.putBody = `{"error":"revision_conflict","message":"m","conflicted":[{"section":"agent_defaults","expected_revision":7,"current_revision":8}]}`
	var out, errw bytes.Buffer
	err := runHubSettingsWrite(context.Background(), svc, &out, &errw,
		[]settingsChange{{Key: "default_max_turns", Value: 1}}, srvcfgWriteOptions{})
	require.Error(t, err)
	var v struct {
		Error struct {
			Status     int                                  `json:"status"`
			Code       string                               `json:"code"`
			Conflicted []hubclient.SettingsRevisionConflict `json:"conflicted"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &v), out.String())
	assert.Equal(t, 409, v.Error.Status)
	assert.Equal(t, "revision_conflict", v.Error.Code)
	assert.Equal(t, int64(8), v.Error.Conflicted[0].CurrentRevision)
}

func TestHubSettingsUnset_RefusedKeySendsNothing(t *testing.T) {
	withTextOutput(t)
	f, svc := newFakeSettingsHub(t)
	var out, errw bytes.Buffer
	err := runHubSettingsWrite(context.Background(), svc, &out, &errw,
		[]settingsChange{{Key: "server.hub.gcp_iam_check_mode", Unset: true}}, srvcfgWriteOptions{})
	require.Error(t, err)
	assert.Empty(t, f.writes)
}

func TestHubSettingsGet(t *testing.T) {
	withTextOutput(t)
	_, svc := newFakeSettingsHub(t)

	var out bytes.Buffer
	require.NoError(t, runHubSettingsGet(context.Background(), svc, &out, "server.hub.public_url", ""))
	assert.Equal(t, "https://hub.example\n", out.String())

	out.Reset()
	require.NoError(t, runHubSettingsGet(context.Background(), svc, &out, "federation.enabled", ""))
	assert.Equal(t, "false\n", out.String())

	out.Reset()
	require.NoError(t, runHubSettingsGet(context.Background(), svc, &out, "runtimes.k8s", ""))
	assert.Equal(t, "type: kubernetes\n", out.String())

	out.Reset()
	require.NoError(t, runHubSettingsGet(context.Background(), svc, &out, "", "endpoints"))
	assert.Contains(t, out.String(), "endpoints (source db, revision 3)")
	assert.Contains(t, out.String(), "server.hub.public_url: https://hub.example")
	assert.Contains(t, out.String(), "image_registry: (not set)")

	out.Reset()
	require.NoError(t, runHubSettingsGet(context.Background(), svc, &out, "", ""))
	assert.Contains(t, out.String(), "SECTION")
	assert.Regexp(t, `agent_defaults\s+db\s+7`, out.String())

	err := runHubSettingsGet(context.Background(), svc, &out, "default_max_turn", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did you mean default_max_turns")

	err = runHubSettingsGet(context.Background(), svc, &out, "default_model", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not set")
}

func TestHubSettingsGet_JSONKey(t *testing.T) {
	prev := outputFormat
	outputFormat = "json"
	t.Cleanup(func() { outputFormat = prev })
	_, svc := newFakeSettingsHub(t)
	var out bytes.Buffer
	require.NoError(t, runHubSettingsGet(context.Background(), svc, &out, "default_max_turns", ""))
	assert.JSONEq(t, `{"key":"default_max_turns","section":"agent_defaults","revision":7,"source":"db","value":100}`, out.String())
}

func TestHubSettingsSchema(t *testing.T) {
	withTextOutput(t)
	_, svc := newFakeSettingsHub(t)
	var out bytes.Buffer
	require.NoError(t, runHubSettingsSchema(context.Background(), svc, &out, ""))
	assert.Contains(t, out.String(), "  server.hub.public_url  (restart)")
	assert.Contains(t, out.String(), "project_defaults  (project defaults: not part of the server configuration)")
	assert.Contains(t, out.String(), "experiments  (not in the server configuration")
	require.Error(t, runHubSettingsSchema(context.Background(), svc, &out, "nope"))
}

func TestHubSettingsReset(t *testing.T) {
	withTextOutput(t)
	f, svc := newFakeSettingsHub(t)
	var out, errw bytes.Buffer
	require.NoError(t, runHubSettingsReset(context.Background(), svc, &out, &errw, "telemetry", true))
	require.Len(t, f.writes, 1)
	assert.Equal(t, http.MethodDelete, f.writes[0].Method)
	assert.Equal(t, "/api/v1/admin/server-config/sections/telemetry", f.writes[0].Path)
}

// TestHubAdminSettingsCommands_ModeAndPlatformGates pins the gate decisions
// for these commands: none is available in agent mode (agents are refused
// by the Hub on every one of these endpoints), and all run on Windows (Hub
// API calls only).
func TestHubAdminSettingsCommands_ModeAndPlatformGates(t *testing.T) {
	paths := []string{
		"hub.settings", "hub.settings.get", "hub.settings.set", "hub.settings.unset", "hub.settings.schema",
	}
	for _, p := range paths {
		assert.False(t, agentAllowed[p], "%s must not be available in agent mode", p)
		assert.True(t, windowsAllowed[p], "%s must be listed for Windows", p)
	}
}

// TestHubSettingsReset_ConflictNamesNoRevision pins the reset guidance: a
// runtimes or profiles reset refused because the other section changed
// meanwhile (the Hub's flat 409 with a conflicted list) is a revision
// conflict, and its hint names no --revision, since reset takes none.
func TestHubSettingsReset_ConflictNamesNoRevision(t *testing.T) {
	withTextOutput(t)
	f, svc := newFakeSettingsHub(t)
	f.putStatus = http.StatusConflict
	f.putBody = `{"error":"revision_conflict","message":"The section was modified concurrently.","conflicted":[{"section":"profiles","expected_revision":4,"current_revision":5,"read":true}]}`
	var out, errw bytes.Buffer
	err := runHubSettingsReset(context.Background(), svc, &out, &errw, "runtimes", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "revision_conflict, HTTP 409")
	assert.Contains(t, errw.String(), "section profiles: expected revision 4, current revision 5")
	assert.Contains(t, errw.String(), "The section changed while it was reset. Re-run the command.")
	assert.NotContains(t, errw.String(), "--revision")
}

// TestHubSettingsGet_UpdatedFollowsDisplayZone pins the UPDATED column to the
// CLI's display zone (--tz, --utc), through pkg/clitime, instead of a fixed
// UTC layout.
func TestHubSettingsGet_UpdatedFollowsDisplayZone(t *testing.T) {
	withTextOutput(t)
	zone := time.FixedZone("UTC+9", 9*60*60)
	prev := clitime.Zone()
	clitime.SetZone(zone)
	t.Cleanup(func() { clitime.SetZone(prev) })

	updated := time.Date(2026, 10, 30, 23, 30, 0, 0, time.UTC)
	cfg := map[string]interface{}{
		"settings_tier": "db",
		"section_metadata": map[string]interface{}{
			"agent_defaults": map[string]interface{}{"source": "db", "revision": 7, "updated_at": updated.Format(time.RFC3339), "updated_by": "admin"},
		},
	}
	body, err := json.Marshal(cfg)
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, runHubSettingsGet(context.Background(), c.AdminSettings(), &out, "", ""))
	want := clitime.Format(updated, clitime.Minute)
	assert.Contains(t, want, "2026-10-31 08:30", "the display zone moves the clock")
	assert.Contains(t, out.String(), want)
	assert.NotContains(t, out.String(), "2026-10-30T23:30:00Z", "no fixed UTC layout")
}
