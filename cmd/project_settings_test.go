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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProjectSettingsHub serves a project's settings and records each PUT.
type fakeProjectSettingsHub struct {
	mu       sync.Mutex
	settings string // GET /settings body
	resolved string // GET /settings/resolved body
	puts     []map[string]json.RawMessage
	putPaths []string

	putStatus int
	putBody   string
}

func newFakeProjectSettingsHub(t *testing.T) (*fakeProjectSettingsHub, hubclient.Client) {
	t.Helper()
	f := &fakeProjectSettingsHub{
		settings: `{"defaultTemplate":"claude","defaultMaxTurns":100,"telemetryEnabled":true,"defaultResources":{"requests":{"cpu":"1","memory":"2Gi"},"limits":{"cpu":"2"}},"defaultGCPIdentityServiceAccountIDByProfile":{}}`,
		resolved: `{"project":{"defaultTemplate":"claude"},"settings":{"scion.dev/default-template":{"projectSet":true,"projectValue":"claude","hubDefault":"present"},"scion.dev/default-model":{"projectSet":false,"projectValue":null,"hubDefault":"unknown"}}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/settings/resolved"):
			_, _ = io.WriteString(w, f.resolved)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/settings"):
			_, _ = io.WriteString(w, f.settings)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/settings"):
			var body map[string]json.RawMessage
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			f.mu.Lock()
			f.puts = append(f.puts, body)
			f.putPaths = append(f.putPaths, r.URL.Path)
			f.mu.Unlock()
			if f.putStatus != 0 {
				w.WriteHeader(f.putStatus)
				_, _ = io.WriteString(w, f.putBody)
				return
			}
			_, _ = io.WriteString(w, `{"defaultTemplate":"gemini","defaultMaxTurns":0,"warnings":["a profile default account is not verified"]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return f, c
}

// useFakeProject points the settings and identity resolvers at the fake Hub.
func useFakeProject(t *testing.T, c hubclient.Client) {
	t.Helper()
	prevS, prevI := resolveProjectForSettings, resolveProjectForIdentityDefault
	fake := func() (hubclient.Client, string, error) { return c, "proj-1", nil }
	resolveProjectForSettings = fake
	resolveProjectForIdentityDefault = fake
	t.Cleanup(func() {
		resolveProjectForSettings = prevS
		resolveProjectForIdentityDefault = prevI
	})
}

func TestProjectSettingsClearForms_CoverEveryField(t *testing.T) {
	typ := reflect.TypeOf(hubclient.ProjectSettings{})
	n := 0
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		n++
		_, ok := projectSettingsClearForms[name]
		assert.True(t, ok, "hubclient.ProjectSettings field %q has no clear form in projectSettingsClearForms; classify it", name)
	}
	assert.Equal(t, n, len(projectSettingsClearForms), "projectSettingsClearForms names a field hubclient.ProjectSettings no longer has")
	require.Greater(t, n, 10, "control: the struct was read")
}

func TestResolveProjectSettingKey(t *testing.T) {
	for _, k := range []string{"defaultTemplate", "default_template", "default-template", "DEFAULTTEMPLATE"} {
		f, sub, err := resolveProjectSettingKey(k)
		require.NoError(t, err, k)
		assert.Equal(t, "defaultTemplate", f.Name, k)
		assert.Empty(t, sub, k)
	}
	f, sub, err := resolveProjectSettingKey("default_resources.limits.cpu")
	require.NoError(t, err)
	assert.Equal(t, "defaultResources", f.Name)
	assert.Equal(t, []string{"limits", "cpu"}, sub)

	_, _, err = resolveProjectSettingKey("defaultTemplat")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did you mean defaultTemplate")
	for _, k := range []string{"bucket", "runtimes", "harnesses", "profiles"} {
		_, _, err = resolveProjectSettingKey(k)
		require.Error(t, err, k)
		assert.Contains(t, err.Error(), "server and Runtime Broker settings", k)
	}
	_, _, err = resolveProjectSettingKey("defaultGCPIdentityMode")
	require.Error(t, err)
	assert.Contains(t, err.Error(), projSettingsIdentityKey)
	_, _, err = resolveProjectSettingKey("defaultTemplate.x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no keys below it")
}

func TestParseProjectSettingValue(t *testing.T) {
	field := func(name string) projectSettingField {
		f, _, err := resolveProjectSettingKey(name)
		require.NoError(t, err)
		return f
	}
	v, err := parseProjectSettingValue(field("defaultMaxTurns"), "unlimited")
	require.NoError(t, err)
	assert.Equal(t, 0, v)
	v, err = parseProjectSettingValue(field("defaultThinkingLevel"), "42")
	require.NoError(t, err)
	assert.Equal(t, 42, v)
	_, err = parseProjectSettingValue(field("defaultThinkingLevel"), "unlimited")
	require.Error(t, err, "unlimited applies to the count limits only")
	v, err = parseProjectSettingValue(field("telemetryEnabled"), "false")
	require.NoError(t, err)
	assert.Equal(t, false, v)
	_, err = parseProjectSettingValue(field("telemetryEnabled"), "maybe")
	require.Error(t, err)
	v, err = parseProjectSettingValue(field("defaultModel"), "claude-opus")
	require.NoError(t, err)
	assert.Equal(t, "claude-opus", v)
	_, err = parseProjectSettingValue(field("defaultModel"), "")
	require.Error(t, err)
	v, err = parseProjectSettingValue(field("defaultResources"), `{limits: {cpu: "2"}}`)
	require.NoError(t, err)
	assert.Equal(t, map[string]interface{}{"limits": map[string]interface{}{"cpu": "2"}}, v)
	_, err = parseProjectSettingValue(field("defaultResources"), "2")
	require.Error(t, err)
}

func TestBuildProjectSettingsBody_ClearFormsAndSparse(t *testing.T) {
	body, err := buildProjectSettingsBody(nil, []settingsChange{
		{Key: "defaultModel", Value: "m"},
		{Key: "defaultThinkingLevel", Unset: true},
		{Key: "default_template", Unset: true},
		{Key: "activeProfile", Unset: true},
		{Key: "defaultResources", Unset: true},
		{Key: "defaultMaxTurns", Unset: true},
	})
	require.NoError(t, err)
	b, err := json.Marshal(body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"defaultModel":"m","defaultThinkingLevel":null,"defaultTemplate":"","activeProfile":"","defaultResources":{},"defaultMaxTurns":null}`, string(b))

	_, err = buildProjectSettingsBody(nil, []settingsChange{{Key: "defaultModel", Value: "a"}, {Key: "default_model", Value: "b"}})
	require.Error(t, err, "given twice")
}

func TestBuildProjectSettingsBody_ResourcesReadModifyWrite(t *testing.T) {
	var cur hubclient.ProjectSettings
	require.NoError(t, json.Unmarshal([]byte(`{"defaultResources":{"requests":{"cpu":"1","memory":"2Gi"},"limits":{"cpu":"2"}}}`), &cur))

	body, err := buildProjectSettingsBody(&cur, []settingsChange{{Key: "defaultResources.limits.memory", Value: "8Gi"}})
	require.NoError(t, err)
	b, _ := json.Marshal(body)
	assert.JSONEq(t, `{"defaultResources":{"requests":{"cpu":"1","memory":"2Gi"},"limits":{"cpu":"2","memory":"8Gi"}}}`, string(b), "the whole value is sent, other parts kept")

	body, err = buildProjectSettingsBody(&cur, []settingsChange{{Key: "defaultResources.requests.memory", Unset: true}})
	require.NoError(t, err)
	b, _ = json.Marshal(body)
	assert.JSONEq(t, `{"defaultResources":{"requests":{"cpu":"1"},"limits":{"cpu":"2"}}}`, string(b))

	_, err = buildProjectSettingsBody(&cur, []settingsChange{{Key: "defaultResources.disk", Unset: true}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not set")

	_, err = buildProjectSettingsBody(&cur, []settingsChange{{Key: "defaultResources", Unset: true}, {Key: "defaultResources.limits.cpu", Value: "4"}})
	require.Error(t, err, "whole and below")
}

func TestProjectSettingsSet_SendsOnlyGivenKeysAndPrintsWarnings(t *testing.T) {
	withTextOutput(t)
	f, c := newFakeProjectSettingsHub(t)
	var out, errw bytes.Buffer
	err := runProjectSettingsWrite(context.Background(), c.Projects(), "proj-1", &out, &errw,
		[]settingsChange{{Key: "defaultTemplate", Value: "gemini"}, {Key: "default_max_turns", Value: "unlimited"}})
	require.NoError(t, err)
	require.Len(t, f.puts, 1)
	assert.Equal(t, "/api/v1/projects/proj-1/settings", f.putPaths[0])
	assert.Len(t, f.puts[0], 2, "only the given keys")
	assert.JSONEq(t, `"gemini"`, string(f.puts[0]["defaultTemplate"]))
	assert.JSONEq(t, `0`, string(f.puts[0]["defaultMaxTurns"]))
	assert.Contains(t, out.String(), "Saved.")
	assert.Regexp(t, `defaultTemplate\s+gemini`, out.String())
	assert.Contains(t, errw.String(), "Warning: a profile default account is not verified")
}

func TestProjectSettingsUnset_SendsClearForms(t *testing.T) {
	withTextOutput(t)
	f, c := newFakeProjectSettingsHub(t)
	var out, errw bytes.Buffer
	err := runProjectSettingsWrite(context.Background(), c.Projects(), "proj-1", &out, &errw,
		[]settingsChange{{Key: "telemetryEnabled", Unset: true}, {Key: "defaultTemplate", Unset: true}, {Key: "defaultResources.limits", Unset: true}})
	require.NoError(t, err)
	require.Len(t, f.puts, 1)
	assert.JSONEq(t, `null`, string(f.puts[0]["telemetryEnabled"]))
	assert.JSONEq(t, `""`, string(f.puts[0]["defaultTemplate"]))
	assert.JSONEq(t, `{"requests":{"cpu":"1","memory":"2Gi"}}`, string(f.puts[0]["defaultResources"]), "read from GET, limits removed, requests kept")
}

func TestProjectSettingsWrite_UnknownAndNotStoredKeysSendNothing(t *testing.T) {
	withTextOutput(t)
	f, c := newFakeProjectSettingsHub(t)
	var out, errw bytes.Buffer
	for _, key := range []string{"defaultTemplat", "bucket", "runtimes", "harnesses", "profiles"} {
		for _, unset := range []bool{false, true} {
			err := runProjectSettingsWrite(context.Background(), c.Projects(), "proj-1", &out, &errw,
				[]settingsChange{{Key: "defaultModel", Value: "m"}, {Key: key, Value: "x", Unset: unset}})
			require.Error(t, err, key)
		}
	}
	assert.Empty(t, f.puts, "an unknown or not-stored key stops the whole command before any request")
}

func TestProjectSettingsWrite_ErrorsAreReported(t *testing.T) {
	withTextOutput(t)
	f, c := newFakeProjectSettingsHub(t)
	f.putStatus = http.StatusUnprocessableEntity
	f.putBody = `{"error":{"code":"validation_error","message":"invalid defaultMaxTurns -1: must be 0 (Unlimited) or more; send null to clear it","details":{"field":"defaultMaxTurns"}}}`
	var out, errw bytes.Buffer
	err := runProjectSettingsWrite(context.Background(), c.Projects(), "proj-1", &out, &errw, []settingsChange{{Key: "defaultMaxTurns", Value: "-1"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 422")
	assert.Contains(t, errw.String(), "defaultMaxTurns: invalid defaultMaxTurns -1")

	f.putStatus = http.StatusBadRequest
	f.putBody = `{"error":{"code":"invalid_request","message":"maxAgentRole must be one of none, readonly, baseline, full"}}`
	errw.Reset()
	err = runProjectSettingsWrite(context.Background(), c.Projects(), "proj-1", &out, &errw, []settingsChange{{Key: "maxAgentRole", Value: "boss"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maxAgentRole must be one of")

	f.putStatus = http.StatusForbidden
	f.putBody = `{"error":{"code":"forbidden","message":"Forbidden"}}`
	errw.Reset()
	err = runProjectSettingsWrite(context.Background(), c.Projects(), "proj-1", &out, &errw, []settingsChange{{Key: "defaultModel", Value: "m"}})
	require.Error(t, err)
	assert.Contains(t, errw.String(), "project:update")
}

func TestProjectSettingsGet(t *testing.T) {
	withTextOutput(t)
	_, c := newFakeProjectSettingsHub(t)
	var out, errw bytes.Buffer
	require.NoError(t, runProjectSettingsGet(context.Background(), c.Projects(), "proj-1", &out, &errw, "", false))
	assert.Regexp(t, `defaultTemplate\s+claude`, out.String())
	assert.Regexp(t, `defaultModel\s+-`, out.String(), "every setting is listed; unset ones show -")

	out.Reset()
	require.NoError(t, runProjectSettingsGet(context.Background(), c.Projects(), "proj-1", &out, &errw, "default_max_turns", false))
	assert.Equal(t, "100\n", out.String())

	out.Reset()
	require.NoError(t, runProjectSettingsGet(context.Background(), c.Projects(), "proj-1", &out, &errw, "defaultResources.requests.memory", false))
	assert.Equal(t, "2Gi\n", out.String())

	err := runProjectSettingsGet(context.Background(), c.Projects(), "proj-1", &out, &errw, "defaultModel", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not set")

	out.Reset()
	require.NoError(t, runProjectSettingsGet(context.Background(), c.Projects(), "proj-1", &out, &errw, "", true))
	assert.Regexp(t, `scion.dev/default-template\s+claude\s+present`, out.String())
	assert.Regexp(t, `scion.dev/default-model\s+-\s+unknown`, out.String(), "the Hub's tri-state is shown as given")
}

func TestProjectSettings_IdentityDelegatesToIdentityDefault(t *testing.T) {
	withTextOutput(t)
	f, c := newFakeProjectSettingsHub(t)
	useFakeProject(t, c)
	prevProfile := projSettingsProfile
	t.Cleanup(func() { projSettingsProfile = prevProfile })
	// The run path is called directly, not through cobra's Execute, so the
	// commands need a context (the identity code derives its timeout from it).
	for _, c := range []*cobra.Command{projectSettingsSetCmd, projectSettingsUnsetCmd} {
		prevCtx := c.Context()
		c.SetContext(context.Background())
		t.Cleanup(func() { c.SetContext(prevCtx) })
	}

	projSettingsProfile = ""
	require.NoError(t, runProjectSettingsCommand(projectSettingsSetCmd, []settingsChange{{Key: "default_gcp_identity", Value: "block"}}))
	require.Len(t, f.puts, 1)
	assert.JSONEq(t, `"block"`, string(f.puts[0]["defaultGCPIdentityMode"]), "the identity-default request: mode set, account cleared")
	assert.JSONEq(t, `""`, string(f.puts[0]["defaultGCPIdentityServiceAccountID"]))

	require.NoError(t, runProjectSettingsCommand(projectSettingsUnsetCmd, []settingsChange{{Key: "defaultGCPIdentity", Unset: true}}))
	require.Len(t, f.puts, 2)
	assert.JSONEq(t, `""`, string(f.puts[1]["defaultGCPIdentityMode"]))

	// The identity key goes alone, and --profile needs it.
	err := runProjectSettingsCommand(projectSettingsSetCmd, []settingsChange{{Key: "defaultGCPIdentity", Value: "block"}, {Key: "defaultModel", Value: "m"}})
	require.Error(t, err)
	projSettingsProfile = "small"
	err = runProjectSettingsCommand(projectSettingsSetCmd, []settingsChange{{Key: "defaultModel", Value: "m"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--profile applies only to defaultGCPIdentity")
	assert.Len(t, f.puts, 2, "the refused commands sent nothing")
}

// cobraRunE pairs a command with its RunE, to call it directly.
type cobraRunE struct {
	run func(*cobra.Command, []string) error
	cmd *cobra.Command
}

// TestSettingsSetCommands_ArgumentCount pins that 'set' with no arguments, or
// an odd count, is refused before any request (ptone/scion#7016). The check
// runs before the Hub client is built; a resolver that fails the test shows
// none is built for project settings.
func TestSettingsSetCommands_ArgumentCount(t *testing.T) {
	prev := resolveProjectForSettings
	resolveProjectForSettings = func() (hubclient.Client, string, error) {
		t.Fatal("the Hub client must not be built for a refused argument count")
		return nil, "", errors.New("unreachable")
	}
	t.Cleanup(func() { resolveProjectForSettings = prev })

	for name, c := range map[string]*cobraRunE{
		"project-defaults set": {hubProjectDefaultsSetCmd.RunE, hubProjectDefaultsSetCmd},
		"project settings set": {projectSettingsSetCmd.RunE, projectSettingsSetCmd},
	} {
		for _, args := range [][]string{nil, {"k"}, {"k", "v", "k2"}} {
			err := c.run(c.cmd, args)
			require.Error(t, err, "%s %v", name, args)
			assert.Contains(t, err.Error(), "want KEY VALUE pairs", "%s %v", name, args)
		}
	}
}

func TestProjectSettingsCommands_ModeAndPlatformGates(t *testing.T) {
	assert.True(t, agentAllowed["project.settings"])
	assert.True(t, agentAllowed["project.settings.get"], "an agent reads its own project's settings")
	assert.False(t, agentAllowed["project.settings.set"], "the Hub refuses agents on PUT")
	assert.False(t, agentAllowed["project.settings.unset"])
	for _, p := range []string{"project.settings", "project.settings.get", "project.settings.set", "project.settings.unset"} {
		assert.True(t, windowsAllowed[p], "%s must be listed for Windows", p)
	}
	assert.False(t, windowsAllowed["project"], "the parent stays gated, as for project.use")
}

// TestProjectSettingsGet_PrintsHubResponseUnchanged pins that get prints what
// the Hub returns: a value the Hub masks stays masked in every output form,
// the JSON and YAML forms are the Hub's response body as received (no member
// added, dropped, reordered or changed), and get makes no request beyond the
// one settings read (no client-side unmasking or extra lookups). get is the
// agent-mode command.
func TestProjectSettingsGet_PrintsHubResponseUnchanged(t *testing.T) {
	f, c := newFakeProjectSettingsHub(t)
	// Not in field order, with a member the typed decode does not know
	// (holding a number as the Hub wrote it, which the typed decode would
	// not keep), and a null. The text forms below decode the same response
	// into the typed settings, so the known members hold values it accepts.
	f.settings = `{"telemetryEnabled":null,"defaultTemplate":"claude","defaultHarnessAuth":"********","defaultMaxTurns":100,"futureSetting":{"b":1,"a":"x","n":1e2}}`
	var calls int
	counting := &countingProjects{ProjectService: c.Projects(), calls: &calls}
	var out, errw bytes.Buffer

	withTextOutput(t)
	require.NoError(t, runProjectSettingsGet(context.Background(), counting, "proj-1", &out, &errw, "", false))
	assert.Regexp(t, `defaultHarnessAuth\s+\*\*\*\*\*\*\*\*`, out.String())
	out.Reset()
	require.NoError(t, runProjectSettingsGet(context.Background(), counting, "proj-1", &out, &errw, "defaultHarnessAuth", false))
	assert.Equal(t, "********\n", out.String())

	prev := outputFormat
	t.Cleanup(func() { outputFormat = prev })

	outputFormat = "json"
	out.Reset()
	require.NoError(t, runProjectSettingsGet(context.Background(), counting, "proj-1", &out, &errw, "", false))
	assert.Equal(t, compactJSON(t, f.settings), compactJSON(t, out.String()), "the JSON form is the Hub's response body, byte for byte apart from whitespace")
	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	assert.Equal(t, "********", got["defaultHarnessAuth"], "the masked value stays masked")

	outputFormat = "yaml"
	out.Reset()
	require.NoError(t, runProjectSettingsGet(context.Background(), counting, "proj-1", &out, &errw, "", false))
	assert.Equal(t, "telemetryEnabled: null\ndefaultTemplate: claude\ndefaultHarnessAuth: '********'\ndefaultMaxTurns: 100\nfutureSetting:\n  b: 1\n  a: x\n  n: 1e2\n", out.String(),
		"the YAML form has the Hub's members, in its order, with its values")

	outputFormat = "json"
	out.Reset()
	require.NoError(t, runProjectSettingsGet(context.Background(), counting, "proj-1", &out, &errw, "", true))
	assert.Equal(t, compactJSON(t, f.resolved), compactJSON(t, out.String()), "the --resolved JSON form is the Hub's response body")

	assert.Equal(t, 5, calls, "exactly one settings read per get, nothing else")
	assert.Empty(t, f.puts)
}

// compactJSON returns s with insignificant whitespace removed.
func compactJSON(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, json.Compact(&b, []byte(s)))
	return b.String()
}

// countingProjects counts the ProjectService reads get makes.
type countingProjects struct {
	hubclient.ProjectService
	calls *int
}

func (p *countingProjects) GetSettings(ctx context.Context, id string) (*hubclient.ProjectSettings, error) {
	*p.calls++
	return p.ProjectService.GetSettings(ctx, id)
}

func (p *countingProjects) GetResolvedSettings(ctx context.Context, id string) (*hubclient.ResolvedProjectSettings, error) {
	*p.calls++
	return p.ProjectService.GetResolvedSettings(ctx, id)
}

func (p *countingProjects) GetSettingsBody(ctx context.Context, id string) (json.RawMessage, error) {
	*p.calls++
	return p.ProjectService.GetSettingsBody(ctx, id)
}

func (p *countingProjects) GetResolvedSettingsBody(ctx context.Context, id string) (json.RawMessage, error) {
	*p.calls++
	return p.ProjectService.GetResolvedSettingsBody(ctx, id)
}

// TestProjectSettingsWrite_UnknownResourceKeysSendNothing pins that keys below
// defaultResources, and members of an object value, are checked against
// hubclient.ProjectResourceSpec: the Hub drops an unknown member silently, so
// the CLI refuses it before any request.
func TestProjectSettingsWrite_UnknownResourceKeysSendNothing(t *testing.T) {
	withTextOutput(t)
	f, c := newFakeProjectSettingsHub(t)
	var out, errw bytes.Buffer
	cases := []struct {
		ch   settingsChange
		want string
	}{
		{settingsChange{Key: "defaultResources.limits.memroy", Value: "8Gi"}, "did you mean memory"},
		{settingsChange{Key: "defaultResources.limits.gpu", Value: "1"}, `"gpu" is not a key`},
		{settingsChange{Key: "defaultResources.limit.cpu", Value: "1"}, "did you mean limits"},
		{settingsChange{Key: "defaultResources.limits", Value: "2"}, "is an object"},
		{settingsChange{Key: "defaultResources.disk.size", Value: "1"}, "has no keys below it"},
		{settingsChange{Key: "defaultResources", Value: `{limits: {memroy: 8Gi}}`}, "memroy"},
		{settingsChange{Key: "defaultResources.limits.memroy", Unset: true}, "did you mean memory"},
	}
	for _, tc := range cases {
		err := runProjectSettingsWrite(context.Background(), c.Projects(), "proj-1", &out, &errw, []settingsChange{tc.ch})
		require.Error(t, err, tc.ch.Key)
		assert.Contains(t, err.Error(), tc.want, tc.ch.Key)
	}
	assert.Empty(t, f.puts, "nothing is sent for an unknown key below defaultResources")

	// Control: valid keys and a valid object are sent.
	require.NoError(t, runProjectSettingsWrite(context.Background(), c.Projects(), "proj-1", &out, &errw,
		[]settingsChange{{Key: "defaultResources", Value: `{limits: {memory: 8Gi}, disk: 20Gi}`}}))
	require.Len(t, f.puts, 1)

	err := runProjectSettingsGet(context.Background(), c.Projects(), "proj-1", &out, &errw, "defaultResources.limits.memroy", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a key")
}

func TestBuildProjectSettingsBody_UnsetPrunesEmptyParents(t *testing.T) {
	var cur hubclient.ProjectSettings
	require.NoError(t, json.Unmarshal([]byte(`{"defaultResources":{"requests":{"cpu":"1","memory":"2Gi"},"limits":{"cpu":"2"}}}`), &cur))
	body, err := buildProjectSettingsBody(&cur, []settingsChange{
		{Key: "defaultResources.requests.cpu", Unset: true},
		{Key: "defaultResources.requests.memory", Unset: true},
	})
	require.NoError(t, err)
	b, _ := json.Marshal(body)
	assert.JSONEq(t, `{"defaultResources":{"limits":{"cpu":"2"}}}`, string(b), "an emptied requests object is removed")
}

func TestProjectSettings_IdentityKeyHasNoKeysBelow(t *testing.T) {
	withTextOutput(t)
	f, c := newFakeProjectSettingsHub(t)
	useFakeProject(t, c)
	for _, ch := range []settingsChange{{Key: "defaultGCPIdentity.x", Value: "block"}, {Key: "defaultGCPIdentity.x", Unset: true}} {
		err := runProjectSettingsCommand(projectSettingsSetCmd, []settingsChange{ch})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "has no keys below it")
	}
	assert.Empty(t, f.puts)
	var out, errw bytes.Buffer
	err := runProjectSettingsGet(context.Background(), c.Projects(), "proj-1", &out, &errw, "defaultGCPIdentity.x", false)
	require.Error(t, err)
}

// TestProjectSettings_IdentityProfileUnsetDelegates is the positive --profile
// case: unset with --profile removes only that profile's account, through the
// identity-default code (read the map, send it back without the entry).
func TestProjectSettings_IdentityProfileUnsetDelegates(t *testing.T) {
	withTextOutput(t)
	f, c := newFakeProjectSettingsHub(t)
	f.settings = `{"defaultGCPIdentityServiceAccountIDByProfile":{"small":"sa-1","big":"sa-2"}}`
	useFakeProject(t, c)
	prevProfile := projSettingsProfile
	t.Cleanup(func() { projSettingsProfile = prevProfile })
	prevCtx := projectSettingsUnsetCmd.Context()
	projectSettingsUnsetCmd.SetContext(context.Background())
	t.Cleanup(func() { projectSettingsUnsetCmd.SetContext(prevCtx) })

	projSettingsProfile = "small"
	require.NoError(t, runProjectSettingsCommand(projectSettingsUnsetCmd, []settingsChange{{Key: "defaultGCPIdentity", Unset: true}}))
	require.Len(t, f.puts, 1)
	assert.JSONEq(t, `{"big":"sa-2"}`, string(f.puts[0]["defaultGCPIdentityServiceAccountIDByProfile"]))
	_, modeSent := f.puts[0]["defaultGCPIdentityMode"]
	assert.False(t, modeSent, "a per-profile unset leaves the project default alone")
}

// TestParseProjectSettingValue_ResourceNumbersAreText pins that an unquoted
// number in a defaultResources object is sent as text, as the API expects.
func TestParseProjectSettingValue_ResourceNumbersAreText(t *testing.T) {
	f, _, err := resolveProjectSettingKey("defaultResources")
	require.NoError(t, err)
	v, err := parseProjectSettingValue(f, `{limits: {cpu: 2, memory: 8Gi}, requests: {cpu: 0.5}}`)
	require.NoError(t, err)
	b, err := json.Marshal(v)
	require.NoError(t, err)
	assert.JSONEq(t, `{"limits":{"cpu":"2","memory":"8Gi"},"requests":{"cpu":"0.5"}}`, string(b))
}

// TestBuildProjectSettingsBody_ResourceKeysMatchAnyCase pins that keys below
// defaultResources match in any letter case, as the Hub decodes them, and
// are sent in their canonical spelling.
func TestBuildProjectSettingsBody_ResourceKeysMatchAnyCase(t *testing.T) {
	body, err := buildProjectSettingsBody(&hubclient.ProjectSettings{}, []settingsChange{{Key: "defaultResources.Limits.CPU", Value: "2"}})
	require.NoError(t, err)
	b, _ := json.Marshal(body)
	assert.JSONEq(t, `{"defaultResources":{"limits":{"cpu":"2"}}}`, string(b))
}
