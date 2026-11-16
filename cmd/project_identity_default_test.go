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
	"net/url"
	"slices"
	"strconv"
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
	idDefProject   = "proj-1"
	idDefSAOne     = "11111111-1111-4111-8111-111111111111"
	idDefSATwo     = "22222222-2222-4222-8222-222222222222"
	idDefBrokerOne = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	idDefBrokerTwo = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

var identityDefaultFields = []string{
	"defaultGCPIdentityMode",
	"defaultGCPIdentityServiceAccountID",
	"defaultGCPIdentityServiceAccountIDByBroker",
	"defaultGCPIdentityServiceAccountIDByProfile",
}

// idDefFakeHub is a fake Hub that stores project settings as a JSON object
// and merges a PUT key by key, as the real settings endpoint does.
type idDefFakeHub struct {
	mu                sync.Mutex
	stored            map[string]any
	resolved          map[string]any // the "settings" part of /settings/resolved
	brokers           []hubclient.RuntimeBroker
	brokerPageSize    int // when >0, paginates /api/v1/runtime-brokers via nextCursor
	brokerListQueries []url.Values
	putBodies         []map[string]any
	statusRef         []string
	gets              int    // GETs of /settings
	putStatus         int    // when non-zero, PUT fails with this status
	putError          string // error body for a failing PUT
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
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runtime-brokers":
			q := r.URL.Query()
			f.brokerListQueries = append(f.brokerListQueries, q)
			filtered := make([]hubclient.RuntimeBroker, 0, len(f.brokers))
			wantName := q.Get("name")
			for _, b := range f.brokers {
				if wantName != "" && !strings.EqualFold(b.Name, wantName) {
					continue
				}
				filtered = append(filtered, b)
			}
			nextCursor := ""
			if f.brokerPageSize > 0 {
				start := 0
				if c := q.Get("cursor"); c != "" {
					parsed, err := strconv.Atoi(c)
					require.NoError(t, err)
					start = parsed
				}
				if start > len(filtered) {
					start = len(filtered)
				}
				end := start + f.brokerPageSize
				if end < len(filtered) {
					nextCursor = strconv.Itoa(end)
				} else {
					end = len(filtered)
				}
				filtered = filtered[start:end]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"brokers":    filtered,
				"nextCursor": nextCursor,
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/runtime-brokers/"):
			wantID := strings.TrimPrefix(r.URL.Path, "/api/v1/runtime-brokers/")
			for _, b := range f.brokers {
				if b.ID == wantID {
					_ = json.NewEncoder(w).Encode(b)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"Runtime Broker not found"}}`))
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
		brokers: []hubclient.RuntimeBroker{
			{ID: idDefBrokerOne, Name: "CR East", Slug: "cr-east"},
			{ID: idDefBrokerTwo, Name: "Docker West", Slug: "docker-west"},
		},
	}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	prevResolve, prevFormat, prevJSON, prevProfile, prevBroker := resolveProjectForIdentityDefault, outputFormat, identityDefaultJSON, identityDefaultProfile, identityDefaultBroker
	t.Cleanup(func() {
		resolveProjectForIdentityDefault, outputFormat, identityDefaultJSON, identityDefaultProfile, identityDefaultBroker = prevResolve, prevFormat, prevJSON, prevProfile, prevBroker
	})
	resolveProjectForIdentityDefault = func() (hubclient.Client, string, error) { return client, idDefProject, nil }
	outputFormat, identityDefaultJSON, identityDefaultProfile, identityDefaultBroker = "", false, "", ""
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
	f.stored["defaultGCPIdentityServiceAccountIDByBroker"] = map[string]any{idDefBrokerOne: idDefSAOne}
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
		`2. per-broker "CR East" (` + idDefBrokerOne + `)`,
		`3. per-profile "gke"`,
		"assign " + idDefSAOne + " (Worker SA)",
		"4. project",
		"passthrough",
		"5. hub",
		"assign " + idDefSATwo,
		"not reached while the project default is set",
		"6. runtime",
		"source: hub setting agent_defaults.default_gcp_identity_mode",
	} {
		assert.Contains(t, stdout, want)
	}
}

func TestIdentityDefaultShow_JSON(t *testing.T) {
	f := newIdDefFakeHub(t)
	f.stored["defaultGCPIdentityServiceAccountIDByBroker"] = map[string]any{}
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
	assert.Equal(t, []string{"request", "per-broker", "per-profile", "project", "hub", "runtime"}, rungs)
	assert.Equal(t, "not_set", states["per-broker"])
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

func TestIdentityDefaultSet_BrokerByNameSlugAndIDStoresCanonicalID(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  string
	}{
		{name: "by name", ref: "CR East"},
		{name: "by slug", ref: "cr-east"},
		{name: "by id", ref: idDefBrokerOne},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIdDefFakeHub(t)
			f.stored["defaultGCPIdentityServiceAccountIDByBroker"] = map[string]any{idDefBrokerTwo: idDefSATwo}
			identityDefaultBroker = tc.ref

			var runErr error
			stdout, _ := captureStdoutStderr(t, func() { runErr = runIdentityDefaultSet(idDefCmd(t), []string{"Worker SA"}) })
			require.NoError(t, runErr)

			require.Len(t, f.putBodies, 1)
			assert.Equal(t, map[string]any{
				"defaultGCPIdentityServiceAccountIDByBroker": map[string]any{
					idDefBrokerOne: idDefSAOne,
					idDefBrokerTwo: idDefSATwo,
				},
			}, f.putBodies[0], "only the per-broker map is sent, keyed by canonical broker UUID, preserving other brokers")
			assertOnlyIdentityFieldsSent(t, f)
			assert.Contains(t, stdout, idDefBrokerOne+": "+idDefSAOne)
			assert.Contains(t, stdout, idDefBrokerTwo+": "+idDefSATwo)
		})
	}
}

func TestIdentityDefaultSet_BrokerRejectsModeAndProfileConflict(t *testing.T) {
	f := newIdDefFakeHub(t)
	identityDefaultBroker = "cr-east"
	err := runIdentityDefaultSet(idDefCmd(t), []string{"passthrough"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "per-broker default is an account")
	assert.Empty(t, f.putBodies)

	identityDefaultProfile = "gke"
	err = runIdentityDefaultSet(idDefCmd(t), []string{"Worker SA"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--broker and --profile cannot be used together")
	assert.Empty(t, f.putBodies)

	err = runIdentityDefaultUnset(idDefCmd(t), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--broker and --profile cannot be used together")
	assert.Empty(t, f.putBodies)
}

func TestIdentityDefaultSet_UnknownAndAmbiguousBrokerNotWritten(t *testing.T) {
	f := newIdDefFakeHub(t)
	identityDefaultBroker = "no-such-broker"
	err := runIdentityDefaultSet(idDefCmd(t), []string{"Worker SA"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no Runtime Broker "no-such-broker" was found`)
	assert.Empty(t, f.putBodies)

	f.brokers = append(f.brokers, hubclient.RuntimeBroker{ID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Name: "CR East", Slug: "cr-east-2"})
	identityDefaultBroker = "CR East"
	err = runIdentityDefaultSet(idDefCmd(t), []string{"Worker SA"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "matches 2 brokers")
	assert.Contains(t, err.Error(), idDefBrokerOne)
	assert.Empty(t, f.putBodies)
}

func TestIdentityDefaultUnset_BrokerByNameAndStaleDeletedID(t *testing.T) {
	const deletedBrokerID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	f := newIdDefFakeHub(t)
	f.stored["defaultGCPIdentityServiceAccountIDByBroker"] = map[string]any{
		idDefBrokerOne:  idDefSAOne,
		deletedBrokerID: idDefSATwo,
	}

	// Unset live broker by slug keeps the other entry.
	identityDefaultBroker = "cr-east"
	var runErr error
	_, _ = captureStdoutStderr(t, func() { runErr = runIdentityDefaultUnset(idDefCmd(t), nil) })
	require.NoError(t, runErr)
	require.Len(t, f.putBodies, 1)
	assert.Equal(t, map[string]any{
		"defaultGCPIdentityServiceAccountIDByBroker": map[string]any{deletedBrokerID: idDefSATwo},
	}, f.putBodies[0])

	// Unset since-deleted broker directly by stored UUID key sends {} to clear the map.
	identityDefaultBroker = deletedBrokerID
	_, _ = captureStdoutStderr(t, func() { runErr = runIdentityDefaultUnset(idDefCmd(t), nil) })
	require.NoError(t, runErr)
	require.Len(t, f.putBodies, 2)
	assert.Equal(t, map[string]any{
		"defaultGCPIdentityServiceAccountIDByBroker": map[string]any{},
	}, f.putBodies[1])

	// Unset an existing broker with no entry writes nothing.
	identityDefaultBroker = "docker-west"
	_, _ = captureStdoutStderr(t, func() { runErr = runIdentityDefaultUnset(idDefCmd(t), nil) })
	require.NoError(t, runErr)
	assert.Len(t, f.putBodies, 2)
}

func TestIdentityDefaultSet_BrokerUsesServerNameFilterAndIgnoresPlugins(t *testing.T) {
	const (
		brokerPage3ID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
		pluginID      = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	)
	f := newIdDefFakeHub(t)
	f.brokerPageSize = 1
	f.brokers = []hubclient.RuntimeBroker{
		{ID: idDefBrokerOne, Name: "CR East", Slug: "cr-east"},
		{ID: idDefBrokerTwo, Name: "Docker West", Slug: "docker-west"},
		{ID: brokerPage3ID, Name: "CR Page 3", Slug: "cr-page-3"},
		{ID: pluginID, Name: "Telegram Plugin", Slug: "telegram-plugin", Labels: map[string]string{pluginBrokerLabelKey: "telegram"}},
	}

	// Resolving by Name passes server-side ?name=CR+Page+3 and succeeds in a single filtered page.
	identityDefaultBroker = "CR Page 3"
	var runErr error
	_, _ = captureStdoutStderr(t, func() { runErr = runIdentityDefaultSet(idDefCmd(t), []string{"Worker SA"}) })
	require.NoError(t, runErr)
	require.Len(t, f.brokerListQueries, 1, "server-side Name filter must resolve without scanning unfiltered pages")
	assert.Equal(t, "CR Page 3", f.brokerListQueries[0].Get("name"))
	require.Len(t, f.putBodies, 1)
	assert.Equal(t, map[string]any{
		"defaultGCPIdentityServiceAccountIDByBroker": map[string]any{brokerPage3ID: idDefSAOne},
	}, f.putBodies[0])

	// Message-broker plugin records are excluded on ID Get, Name List, and Slug List.
	for _, pluginRef := range []string{pluginID, "Telegram Plugin", "telegram-plugin"} {
		identityDefaultBroker = pluginRef
		err := runIdentityDefaultSet(idDefCmd(t), []string{"Worker SA"})
		require.Error(t, err, "plugin ref %q must not resolve as a Runtime Broker", pluginRef)
		assert.Contains(t, err.Error(), "no Runtime Broker")
		assert.Len(t, f.putBodies, 1, "nothing written for plugin ref %q", pluginRef)
	}
}

func TestIdentityDefault_BrokerPaginatedSlugAndShow(t *testing.T) {
	const brokerPage3ID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	f := newIdDefFakeHub(t)
	f.brokerPageSize = 1
	f.brokers = []hubclient.RuntimeBroker{
		{ID: idDefBrokerOne, Name: "CR East", Slug: "cr-east"},
		{ID: idDefBrokerTwo, Name: "Docker West", Slug: "docker-west"},
		{ID: brokerPage3ID, Name: "CR Page 3", Slug: "cr-page-3"},
	}

	// Slug fallback pages through NextCursor across all 3 pages to find cr-page-3.
	identityDefaultBroker = "cr-page-3"
	var runErr error
	_, _ = captureStdoutStderr(t, func() { runErr = runIdentityDefaultSet(idDefCmd(t), []string{"Worker SA"}) })
	require.NoError(t, runErr)
	require.Len(t, f.putBodies, 1)
	assert.Equal(t, map[string]any{
		"defaultGCPIdentityServiceAccountIDByBroker": map[string]any{brokerPage3ID: idDefSAOne},
	}, f.putBodies[0])
	// First query is server-side ?name=cr-page-3 (0 results), followed by 3 paginated unfiltered slug queries.
	require.Len(t, f.brokerListQueries, 4)
	assert.Equal(t, "cr-page-3", f.brokerListQueries[0].Get("name"))
	assert.Equal(t, "", f.brokerListQueries[1].Get("cursor"))
	assert.Equal(t, "1", f.brokerListQueries[2].Get("cursor"))
	assert.Equal(t, "2", f.brokerListQueries[3].Get("cursor"))

	// `show` pages through NextCursor to decorate CR Page 3's display name on page 3.
	f.brokerListQueries = nil
	stdout, _ := captureStdoutStderr(t, func() { runErr = runIdentityDefaultShow(idDefCmd(t), nil) })
	require.NoError(t, runErr)
	assert.Contains(t, stdout, `per-broker "CR Page 3" (`+brokerPage3ID+`) assign `+idDefSAOne+` (Worker SA)`)
	assert.Len(t, f.brokerListQueries, 3, "show must page through NextCursor until all stored broker IDs are decorated")
}

func TestIdentityDefaultSet_BrokerCrossPageAmbiguityRejected(t *testing.T) {
	const (
		dupTwo   = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		dupThree = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	)
	f := newIdDefFakeHub(t)
	f.brokerPageSize = 1
	f.brokers = []hubclient.RuntimeBroker{
		{ID: idDefBrokerOne, Name: "CR Shared", Slug: "cr-shared-slug"},
		{ID: idDefBrokerTwo, Name: "Docker Middle", Slug: "docker-middle"},
		{ID: dupTwo, Name: "CR Shared", Slug: "cr-other-slug"},
		{ID: dupThree, Name: "Other Name", Slug: "cr-shared-slug"},
	}

	// Cross-page duplicate Name (brokerPageSize=1 splits the two "CR Shared" matches across pages 1 and 2).
	identityDefaultBroker = "CR Shared"
	err := runIdentityDefaultSet(idDefCmd(t), []string{"Worker SA"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "matches 2 brokers")
	assert.Contains(t, err.Error(), idDefBrokerOne)
	assert.Contains(t, err.Error(), dupTwo)
	assert.Empty(t, f.putBodies)

	// Cross-page duplicate Slug across pages 1 and 4.
	identityDefaultBroker = "cr-shared-slug"
	err = runIdentityDefaultSet(idDefCmd(t), []string{"Worker SA"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "matches 2 brokers")
	assert.Contains(t, err.Error(), idDefBrokerOne)
	assert.Contains(t, err.Error(), dupThree)
	assert.Empty(t, f.putBodies)
}
