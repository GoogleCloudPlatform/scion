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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fake-hub tests for `scion project identity default` (ptone/scion#4020).

const (
	idDefProject = "proj-1"
	idDefSAOne   = "11111111-1111-4111-8111-111111111111"
	idDefSATwo   = "22222222-2222-4222-8222-222222222222"
)

var identityDefaultFields = []string{
	"defaultGCPIdentityMode",
	"defaultGCPIdentityServiceAccountID",
	"defaultGCPIdentityServiceAccountIDByProfile",
}

// idDefFakeHub is a fake Hub that stores project settings as a JSON object
// and merges a PUT key by key, as the real settings endpoint does.
type idDefFakeHub struct {
	mu        sync.Mutex
	stored    map[string]any
	resolved  map[string]any // the "settings" part of /settings/resolved
	putBodies []map[string]any
	statusRef []string
	gets      int    // GETs of /settings
	putStatus int    // when non-zero, PUT fails with this status
	putError  string // error body for a failing PUT
}

func (f *idDefFakeHub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		settingsPath := "/api/v1/projects/" + idDefProject + "/settings"
		statusPrefix := "/api/v1/projects/" + idDefProject + "/gcp-service-accounts/"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == settingsPath:
			f.gets++
			_ = json.NewEncoder(w).Encode(f.stored)
		case r.Method == http.MethodGet && r.URL.Path == settingsPath+"/resolved":
			_ = json.NewEncoder(w).Encode(map[string]any{"project": f.stored, "settings": f.resolved})
		case r.Method == http.MethodPut && r.URL.Path == settingsPath:
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			var put map[string]any
			require.NoError(t, json.Unmarshal(body, &put))
			f.putBodies = append(f.putBodies, put)
			if f.putStatus != 0 {
				w.WriteHeader(f.putStatus)
				_, _ = w.Write([]byte(f.putError))
				return
			}
			for k, v := range put {
				if v == nil {
					continue
				}
				f.stored[k] = v
			}
			_ = json.NewEncoder(w).Encode(f.stored)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, statusPrefix) && strings.HasSuffix(r.URL.Path, "/status"):
			ref := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, statusPrefix), "/status")
			f.statusRef = append(f.statusRef, ref)
			switch ref {
			case "worker@example.com", "Worker SA", idDefSAOne:
				_, _ = w.Write([]byte(`{"account":{"id":"` + idDefSAOne + `","displayName":"Worker SA","scope":"project","email":"worker@example.com"}}`))
			case "Shared":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":"identity_ambiguous","message":"GCP service account \"Shared\" matches 2 registered accounts: ` +
					idDefSAOne + ` (project), ` + idDefSATwo + ` (hub); use the account id"}}`))
			default:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"GCP Service Account not found"}}`))
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/gcp-service-accounts":
			_, _ = w.Write([]byte(`{"items":[{"id":"` + idDefSAOne + `","scope":"project","displayName":"Worker SA"}]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// newIdDefFakeHub starts the fake Hub with stored settings that include
// unrelated fields, which every write must leave untouched.
func newIdDefFakeHub(t *testing.T) *idDefFakeHub {
	t.Helper()
	f := &idDefFakeHub{
		stored: map[string]any{
			"defaultTemplate":    "claude",
			"defaultModel":       "model-x",
			"maxAgentRole":       "baseline",
			"telemetryEnabled":   false,
			"activeProfile":      "gke",
			"defaultMaxTurns":    float64(50),
			"defaultResources":   map[string]any{"disk": "10Gi"},
			"defaultHarnessAuth": "api-key",
			"defaultGCPIdentityServiceAccountIDByProfile": map[string]any{"other": idDefSATwo},
		},
		resolved: map[string]any{},
	}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	prevResolve, prevFormat, prevJSON, prevProfile := resolveProjectForIdentityDefault, outputFormat, identityDefaultJSON, identityDefaultProfile
	t.Cleanup(func() {
		resolveProjectForIdentityDefault, outputFormat, identityDefaultJSON, identityDefaultProfile = prevResolve, prevFormat, prevJSON, prevProfile
	})
	resolveProjectForIdentityDefault = func() (hubclient.Client, string, error) { return client, idDefProject, nil }
	outputFormat, identityDefaultJSON, identityDefaultProfile = "", false, ""
	return f
}

// assertOnlyIdentityFieldsSent checks that every PUT carried only identity
// fields and that the unrelated stored settings survived.
func assertOnlyIdentityFieldsSent(t *testing.T, f *idDefFakeHub) {
	t.Helper()
	for _, body := range f.putBodies {
		for k := range body {
			assert.Contains(t, identityDefaultFields, k, "PUT body must carry only identity fields")
		}
	}
	assert.Equal(t, "claude", f.stored["defaultTemplate"])
	assert.Equal(t, "model-x", f.stored["defaultModel"])
	assert.Equal(t, "baseline", f.stored["maxAgentRole"])
	assert.Equal(t, false, f.stored["telemetryEnabled"])
	assert.Equal(t, "gke", f.stored["activeProfile"])
	assert.Equal(t, float64(50), f.stored["defaultMaxTurns"])
	assert.Equal(t, map[string]any{"disk": "10Gi"}, f.stored["defaultResources"])
	assert.Equal(t, "api-key", f.stored["defaultHarnessAuth"])
}

func TestIdentityDefaultSet_ProjectAccountByEmailStoresID(t *testing.T) {
	f := newIdDefFakeHub(t)

	var runErr error
	stdout, _ := captureStdoutStderr(t, func() { runErr = runIdentityDefaultSet(idDefCmd(t), []string{"worker@example.com"}) })
	require.NoError(t, runErr)

	assert.Equal(t, []string{"worker@example.com"}, f.statusRef, "the reference is resolved through the Hub")
	require.Len(t, f.putBodies, 1)
	assert.Equal(t, map[string]any{
		"defaultGCPIdentityMode":             "assign",
		"defaultGCPIdentityServiceAccountID": idDefSAOne,
	}, f.putBodies[0], "the resolved id is stored, not the reference as typed")
	assertOnlyIdentityFieldsSent(t, f)
	assert.Equal(t, map[string]any{"other": idDefSATwo}, f.stored["defaultGCPIdentityServiceAccountIDByProfile"],
		"setting the project default keeps the per-profile defaults")
	assert.Contains(t, stdout, "Project default: assign "+idDefSAOne)
	assert.Zero(t, f.gets, "a project-default write does not read the settings first")
}

func TestIdentityDefaultSet_ModeClearsAccount(t *testing.T) {
	for _, arg := range []string{"passthrough", "Block"} {
		t.Run(arg, func(t *testing.T) {
			f := newIdDefFakeHub(t)
			f.stored["defaultGCPIdentityMode"] = "assign"
			f.stored["defaultGCPIdentityServiceAccountID"] = idDefSAOne

			var runErr error
			_, _ = captureStdoutStderr(t, func() { runErr = runIdentityDefaultSet(idDefCmd(t), []string{arg}) })
			require.NoError(t, runErr)

			assert.Empty(t, f.statusRef, "a mode is not resolved as an account")
			assert.Zero(t, f.gets, "a project-default write does not read the settings first")
			require.Len(t, f.putBodies, 1)
			assert.Equal(t, map[string]any{
				"defaultGCPIdentityMode":             strings.ToLower(arg),
				"defaultGCPIdentityServiceAccountID": "",
			}, f.putBodies[0])
			assertOnlyIdentityFieldsSent(t, f)
		})
	}
}

func TestIdentityDefaultSet_ProfileKeepsOtherProfiles(t *testing.T) {
	f := newIdDefFakeHub(t)
	identityDefaultProfile = "gke"

	var runErr error
	stdout, _ := captureStdoutStderr(t, func() { runErr = runIdentityDefaultSet(idDefCmd(t), []string{"Worker SA"}) })
	require.NoError(t, runErr)

	require.Len(t, f.putBodies, 1)
	assert.Equal(t, map[string]any{
		"defaultGCPIdentityServiceAccountIDByProfile": map[string]any{"gke": idDefSAOne, "other": idDefSATwo},
	}, f.putBodies[0], "only the per-profile map is sent, with the other profile's entry kept")
	assert.Equal(t, 1, f.gets, "a per-profile write reads the stored map first")
	assertOnlyIdentityFieldsSent(t, f)
	assert.Contains(t, stdout, "gke: "+idDefSAOne)
	assert.Contains(t, stdout, "other: "+idDefSATwo)
}

func TestIdentityDefaultSet_ProfileRejectsMode(t *testing.T) {
	f := newIdDefFakeHub(t)
	identityDefaultProfile = "gke"

	err := runIdentityDefaultSet(idDefCmd(t), []string{"passthrough"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "per-profile default is an account")
	assert.Empty(t, f.putBodies)
}

func TestIdentityDefaultSet_AssignWordRejected(t *testing.T) {
	f := newIdDefFakeHub(t)
	err := runIdentityDefaultSet(idDefCmd(t), []string{"assign"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "assign needs an account")
	assert.Empty(t, f.putBodies)
}

func TestIdentityDefaultSet_AmbiguousReferenceNotWritten(t *testing.T) {
	f := newIdDefFakeHub(t)

	err := runIdentityDefaultSet(idDefCmd(t), []string{"Shared"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "identity_ambiguous")
	assert.Contains(t, err.Error(), idDefSAOne+" (project)")
	assert.Contains(t, err.Error(), idDefSATwo+" (hub)")
	assert.Contains(t, err.Error(), "nothing was changed")
	assert.Empty(t, f.putBodies, "an ambiguous reference must be rejected before writing")
}

func TestIdentityDefaultSet_UnknownReferenceNotWritten(t *testing.T) {
	f := newIdDefFakeHub(t)

	err := runIdentityDefaultSet(idDefCmd(t), []string{"nobody@example.com"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no GCP service account \"nobody@example.com\" is available")
	assert.Empty(t, f.putBodies)
}

func TestIdentityDefaultSet_ForbiddenNamesRole(t *testing.T) {
	f := newIdDefFakeHub(t)
	f.putStatus = http.StatusForbidden
	f.putError = `{"error":{"code":"forbidden","message":"Insufficient permissions"}}`

	err := runIdentityDefaultSet(idDefCmd(t), []string{"passthrough"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Insufficient permissions", "the Hub's message is shown")
	assert.Contains(t, err.Error(), "project.update")
	assert.Contains(t, err.Error(), "project-admin")
}

func TestIdentityDefaultSet_HubRejectionShownAsIs(t *testing.T) {
	f := newIdDefFakeHub(t)
	f.putStatus = http.StatusBadRequest
	f.putError = `{"error":{"code":"identity_mode_unsupported","message":"default GCP identity mode 'block' is not available for a Kubernetes-bound project; choose 'passthrough' or 'assign' instead"}}`

	err := runIdentityDefaultSet(idDefCmd(t), []string{"block"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not available for a Kubernetes-bound project")
	assert.NotContains(t, err.Error(), "project.update")
}

func TestIdentityDefaultUnset_Project(t *testing.T) {
	f := newIdDefFakeHub(t)
	f.stored["defaultGCPIdentityMode"] = "assign"
	f.stored["defaultGCPIdentityServiceAccountID"] = idDefSAOne

	var runErr error
	stdout, _ := captureStdoutStderr(t, func() { runErr = runIdentityDefaultUnset(idDefCmd(t), nil) })
	require.NoError(t, runErr)

	require.Len(t, f.putBodies, 1)
	assert.Equal(t, map[string]any{
		"defaultGCPIdentityMode":             "",
		"defaultGCPIdentityServiceAccountID": "",
	}, f.putBodies[0])
	assertOnlyIdentityFieldsSent(t, f)
	assert.Equal(t, map[string]any{"other": idDefSATwo}, f.stored["defaultGCPIdentityServiceAccountIDByProfile"])
	assert.Contains(t, stdout, "Project default: not set")
	assert.Zero(t, f.gets, "a project-default unset does not read the settings first")
}

func TestIdentityDefaultUnset_ProfileKeepsOthers(t *testing.T) {
	f := newIdDefFakeHub(t)
	f.stored["defaultGCPIdentityServiceAccountIDByProfile"] = map[string]any{"gke": idDefSAOne, "other": idDefSATwo}
	identityDefaultProfile = "gke"

	var runErr error
	_, _ = captureStdoutStderr(t, func() { runErr = runIdentityDefaultUnset(idDefCmd(t), nil) })
	require.NoError(t, runErr)

	require.Len(t, f.putBodies, 1)
	assert.Equal(t, map[string]any{
		"defaultGCPIdentityServiceAccountIDByProfile": map[string]any{"other": idDefSATwo},
	}, f.putBodies[0])
	assertOnlyIdentityFieldsSent(t, f)
}

func TestIdentityDefaultUnset_LastProfileSendsEmptyMap(t *testing.T) {
	f := newIdDefFakeHub(t)
	identityDefaultProfile = "other"

	var runErr error
	_, _ = captureStdoutStderr(t, func() { runErr = runIdentityDefaultUnset(idDefCmd(t), nil) })
	require.NoError(t, runErr)

	require.Len(t, f.putBodies, 1)
	assert.Equal(t, map[string]any{
		"defaultGCPIdentityServiceAccountIDByProfile": map[string]any{},
	}, f.putBodies[0], "removing the last entry sends {} so the Hub clears the map")
}

func TestIdentityDefaultUnset_MissingProfileWritesNothing(t *testing.T) {
	f := newIdDefFakeHub(t)
	identityDefaultProfile = "absent"

	var runErr error
	_, _ = captureStdoutStderr(t, func() { runErr = runIdentityDefaultUnset(idDefCmd(t), nil) })
	require.NoError(t, runErr)
	assert.Empty(t, f.putBodies)
}

func TestIdentityDefaultShow_LadderWithSources(t *testing.T) {
	f := newIdDefFakeHub(t)
	f.stored["defaultGCPIdentityServiceAccountIDByProfile"] = map[string]any{"gke": idDefSAOne}
	f.stored["defaultGCPIdentityMode"] = "passthrough"
	f.resolved = map[string]any{
		identityDefaultModeKey: map[string]any{"projectSet": true, "projectValue": "passthrough", "hubDefault": "present", "hubValue": "assign"},
		identityDefaultSAIDKey: map[string]any{"projectSet": false, "projectValue": nil, "hubDefault": "present", "hubValue": idDefSATwo},
	}

	var runErr error
	stdout, _ := captureStdoutStderr(t, func() { runErr = runIdentityDefaultShow(idDefCmd(t), nil) })
	require.NoError(t, runErr)
	assert.Empty(t, f.putBodies, "show never writes")

	for _, want := range []string{
		"1. request",
		`2. per-profile "gke"`,
		"assign " + idDefSAOne + " (Worker SA)",
		"3. project",
		"passthrough",
		"4. hub",
		"assign " + idDefSATwo,
		"not reached while the project default is set",
		"5. runtime",
		"source: hub setting agent_defaults.default_gcp_identity_mode",
	} {
		assert.Contains(t, stdout, want)
	}
}

func TestIdentityDefaultShow_JSON(t *testing.T) {
	f := newIdDefFakeHub(t)
	f.stored["defaultGCPIdentityServiceAccountIDByProfile"] = map[string]any{}
	f.resolved = map[string]any{
		identityDefaultModeKey: map[string]any{"projectSet": false, "projectValue": nil, "hubDefault": "unknown", "hubValue": nil},
	}
	identityDefaultJSON = true

	var runErr error
	stdout, _ := captureStdoutStderr(t, func() { runErr = runIdentityDefaultShow(idDefCmd(t), nil) })
	require.NoError(t, runErr)

	var ladder identityDefaultLadder
	require.NoError(t, json.Unmarshal([]byte(stdout), &ladder))
	assert.Equal(t, idDefProject, ladder.ProjectID)
	rungs := make([]string, 0, len(ladder.Rungs))
	states := map[string]string{}
	for _, r := range ladder.Rungs {
		rungs = append(rungs, r.Rung)
		states[r.Rung] = r.State
		assert.NotEmpty(t, r.Source, "every rung names its source")
	}
	assert.Equal(t, []string{"request", "per-profile", "project", "hub", "runtime"}, rungs)
	assert.Equal(t, "not_set", states["per-profile"])
	assert.Equal(t, "not_set", states["project"])
	assert.Equal(t, "unknown", states["hub"], "an unknown hub default is not reported as absent")
}

func TestProjectIdentityDefault_NotInAgentMode(t *testing.T) {
	for _, path := range []string{
		"project.identity",
		"project.identity.default",
		"project.identity.default.show",
		"project.identity.default.set",
		"project.identity.default.unset",
	} {
		assert.False(t, agentAllowed[path], "%s must not be available in agent mode", path)
	}
	names := []string{}
	for _, c := range projectIdentityDefaultCmd.Commands() {
		names = append(names, c.Name())
	}
	assert.True(t, slices.Contains(names, "show") && slices.Contains(names, "set") && slices.Contains(names, "unset"))
}

// idDefCmd returns a command carrying a context, as cobra sets one when the
// command is executed; the run functions derive their timeout from it.
func idDefCmd(t *testing.T) *cobra.Command {
	t.Helper()
	c := &cobra.Command{}
	c.SetContext(context.Background())
	return c
}

// idDefNilClient is a Hub client whose settings read and account status
// return (nil, nil), to check the nil guards.
type idDefNilClient struct{ hubclient.Client }

func (idDefNilClient) Projects() hubclient.ProjectService { return idDefNilProjects{} }
func (idDefNilClient) GCPServiceAccounts() hubclient.GCPServiceAccountService {
	return idDefNilSAs{}
}

type idDefNilProjects struct{ hubclient.ProjectService }

func (idDefNilProjects) GetSettings(context.Context, string) (*hubclient.ProjectSettings, error) {
	return nil, nil
}

type idDefNilSAs struct {
	hubclient.GCPServiceAccountService
}

func (idDefNilSAs) Status(context.Context, string, string) (*hubclient.GCPServiceAccountStatus, error) {
	return nil, nil
}

func TestIdentityDefault_NilSettingsIsAnError(t *testing.T) {
	_, err := readIdentityDefaults(context.Background(), idDefNilClient{}, idDefProject)
	require.Error(t, err, "a missing settings body must not read as an empty per-profile map")
	assert.Contains(t, err.Error(), "returned no project settings")
	assert.Contains(t, err.Error(), "nothing was changed")
}

func TestIdentityDefault_NilAccountStatusIsAnError(t *testing.T) {
	_, err := resolveIdentityDefaultAccount(context.Background(), idDefNilClient{}, idDefProject, "worker@example.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "returned no id")
	assert.Contains(t, err.Error(), "nothing was changed")
}
