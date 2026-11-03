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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/internal/writesonread"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerconvert"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const convertTestBrokerID = "b-legacy"

const convertTestSettings = `schema_version: "1"
server:
  broker:
    broker_id: ` + convertTestBrokerID + `
runtimes:
  docker:
    type: docker
  cluster:
    type: kubernetes
    context: prod
    namespace: agents
  pod:
    type: podman
profiles:
  build:
    runtime: docker
    image_registry: reg.example/build
  local:
    runtime: docker
    image_registry: reg.example/local
  k8s:
    runtime: cluster
`

// convertTestHome makes an isolated HOME with the given global settings
// and returns the settings file path.
func convertTestHome(t *testing.T, settings string) (home, settingsPath string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	// The settings loaders read SCION_* variables; an agent sandbox can
	// leak ones that would change the broker ID or the Hub.
	for _, name := range []string{"SCION_BROKER_ID", "SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_PROJECT_ID", "SCION_HOST_UID", "SCION_SERVER_BROKER_BROKER_ID"} {
		t.Setenv(name, "")
	}
	t.Chdir(t.TempDir())
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	settingsPath = filepath.Join(globalDir, "settings.yaml")
	require.NoError(t, os.WriteFile(settingsPath, []byte(settings), 0o644))
	return home, settingsPath
}

// convertTreeSnapshot records every path under root with its mode, modification
// time and content hash, so a test can prove the command wrote (even the
// same bytes), created and removed nothing.
func convertTreeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := fmt.Sprintf("%s %d", info.Mode(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			entry += " " + hex.EncodeToString(sum[:])
		}
		snap[path] = entry
		return nil
	}))
	return snap
}

func convertFileSHA(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// setConvertFlags sets the convert flags and the output format for one
// test and restores them afterwards, and isolates the no-write switch
// the command sets. The tests name their profiles: the
// embedded default settings add profiles of their own.
func setConvertFlags(t *testing.T, plan, skipHub bool, format string, profiles ...string) {
	t.Helper()
	// convert turns the process-wide no-write switch on; keep it per test.
	writesonread.IsolateForTest(t)
	oldPlan, oldSkipHub, oldProfiles, oldChoices, oldFormat, oldShowEnv := brokerConvertPlan, brokerConvertSkipHub, brokerConvertProfiles, brokerConvertChoices, outputFormat, brokerConvertShowEnv
	t.Cleanup(func() {
		brokerConvertPlan, brokerConvertSkipHub, brokerConvertProfiles, brokerConvertChoices, outputFormat, brokerConvertShowEnv = oldPlan, oldSkipHub, oldProfiles, oldChoices, oldFormat, oldShowEnv
	})
	brokerConvertPlan, brokerConvertSkipHub, brokerConvertProfiles, brokerConvertChoices, outputFormat = plan, skipHub, profiles, "", format
	brokerConvertShowEnv = false
}

func setConvertHubClient(t *testing.T, f func(*config.Settings) (hubclient.Client, error)) {
	t.Helper()
	old := convertHubClient
	t.Cleanup(func() { convertHubClient = old })
	convertHubClient = f
}

func runConvertForTest(t *testing.T) (string, error) {
	t.Helper()
	var out bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&out)
	c.SetContext(context.Background())
	err := runBrokerConvert(c, nil)
	return out.String(), err
}

// convertGETOnlyHub is a fake Hub that answers the plan's GET endpoints
// and records any other request; the tests assert there is none.
type convertGETOnlyHub struct {
	*httptest.Server
	mu     sync.Mutex
	nonGET []string
	gets   int
}

func newConvertGETOnlyHub(t *testing.T) *convertGETOnlyHub {
	t.Helper()
	h := &convertGETOnlyHub{}
	base := "/api/v1/runtime-brokers/" + convertTestBrokerID
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		if r.Method != http.MethodGet {
			h.nonGET = append(h.nonGET, r.Method+" "+r.URL.Path)
			h.mu.Unlock()
			t.Errorf("convert --plan sent a non-GET request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		h.gets++
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var body string
		switch r.URL.Path {
		case base:
			body = `{"id":"` + convertTestBrokerID + `","name":"build-host","profiles":[{"name":"build"},{"name":"local"},{"name":"k8s"}]}`
		case base + "/settings":
			body = `{"brokerId":"` + convertTestBrokerID + `","settings":{"maxAgents":10},"effective":{"maxAgents":{"value":10,"source":"broker","count":1,"inherited":{"value":0,"source":"unlimited"}}}}`
		case base + "/projects":
			body = `{"projects":[{"projectId":"p-1","projectName":"web"}]}`
		case "/api/v1/agents":
			body = `{"agents":[{"id":"a-1","name":"alpha","projectId":"p-1","project":"web","phase":"running","appliedConfig":{"profile":"k8s"}}]}`
		case "/api/v1/projects/p-1/settings":
			body = `{"activeProfile":"build"}`
		default:
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(h.Close)
	return h
}

func TestBrokerConvert_RequiresPlan(t *testing.T) {
	setConvertFlags(t, false, true, "")
	_, err := runConvertForTest(t)
	assert.ErrorIs(t, err, errConvertPlanOnly)
	assert.Equal(t, "convert currently supports only --plan (read-only)", err.Error())
}

// --skip-hub: the local plan, no Hub client, and not one file written,
// created or removed under HOME.
func TestBrokerConvert_SkipHubWritesNothing(t *testing.T) {
	home, settingsPath := convertTestHome(t, convertTestSettings)
	setConvertFlags(t, true, true, "", "build", "local", "k8s")
	setConvertHubClient(t, func(*config.Settings) (hubclient.Client, error) {
		t.Error("--skip-hub must not open a Hub client")
		return nil, errors.New("unexpected")
	})
	before, beforeSHA := convertTreeSnapshot(t, home), convertFileSHA(t, settingsPath)

	out, err := runConvertForTest(t)
	require.NoError(t, err, "no blocking finding, and capacity needs the Hub: exit 0")
	assert.Contains(t, out, "1. Proposed instances")
	assert.Contains(t, out, "server:\n  broker:\n    instances:")
	assert.Contains(t, out, "k8s -> ")
	assert.Contains(t, out, "(needs_hub)")
	assert.Contains(t, out, "(skipped: needs the Hub)")

	assert.Equal(t, beforeSHA, convertFileSHA(t, settingsPath), "the settings file is unchanged")
	assert.Equal(t, before, convertTreeSnapshot(t, home), "nothing under HOME was written, created or removed")
}

// Blocking findings exit 3, through the exit status the CLI reports.
func TestBrokerConvert_BlockingFindingExits3(t *testing.T) {
	convertTestHome(t, convertTestSettings+"  legacy-podman:\n    runtime: pod\n")
	setConvertFlags(t, true, true, "", "build", "local", "k8s", "legacy-podman")
	out, err := runConvertForTest(t)
	require.Error(t, err)
	assert.Equal(t, brokerconvert.ExitBlocked, exitCodeFor(err))
	assert.Contains(t, out, "legacy-podman -> (no mapping: unsupported_flat_target_type)")
}

func TestBrokerConvert_RefusesConfiguredInstances(t *testing.T) {
	convertTestHome(t, "schema_version: \"1\"\nserver:\n  broker:\n    instances:\n      - key: local-docker\n        name: local-docker\n        runtime_target:\n          type: docker\nprofiles:\n  local:\n    runtime: docker\n")
	setConvertFlags(t, true, true, "")
	_, err := runConvertForTest(t)
	assert.ErrorIs(t, err, brokerconvert.ErrInstancesConfigured)
	assert.Equal(t, 1, exitCodeFor(err))
}

// The Hub sections send GET requests only, and nothing is written locally.
func TestBrokerConvert_HubSectionsGETOnlyAndWriteNothing(t *testing.T) {
	home, settingsPath := convertTestHome(t, convertTestSettings)
	setConvertFlags(t, true, false, "json", "build", "local", "k8s")
	hub := newConvertGETOnlyHub(t)
	setConvertHubClient(t, func(*config.Settings) (hubclient.Client, error) {
		return hubclient.New(hub.URL)
	})
	before, beforeSHA := convertTreeSnapshot(t, home), convertFileSHA(t, settingsPath)

	out, err := runConvertForTest(t)
	require.NoError(t, err)

	var doc struct {
		SchemaVersion  int    `json:"schemaVersion"`
		LegacyBrokerID string `json:"legacyBrokerId"`
		Capacity       struct {
			Status      string `json:"status"`
			Outcome     string `json:"outcome"`
			Allocations []struct {
				MaxAgents int64 `json:"maxAgents"`
			} `json:"allocations"`
		} `json:"capacity"`
		Agents struct {
			Items []struct {
				ID       string `json:"id"`
				Mapping  string `json:"mapping"`
				Instance string `json:"instance"`
			} `json:"items"`
		} `json:"agents"`
		Projects struct {
			Items []struct {
				ActiveProfile struct {
					Mapping string `json:"mapping"`
				} `json:"activeProfile"`
			} `json:"items"`
		} `json:"projects"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &doc), out)
	assert.Equal(t, 1, doc.SchemaVersion)
	assert.Equal(t, convertTestBrokerID, doc.LegacyBrokerID, "the host's registered Runtime Broker ID")
	assert.Equal(t, "planned", doc.Capacity.Status)
	assert.Equal(t, "preserve_by_split", doc.Capacity.Outcome)
	require.Len(t, doc.Capacity.Allocations, 3)
	require.Len(t, doc.Agents.Items, 1)
	assert.Equal(t, "mapped", doc.Agents.Items[0].Mapping)
	require.Len(t, doc.Projects.Items, 1)
	assert.Equal(t, "mapped", doc.Projects.Items[0].ActiveProfile.Mapping)

	hub.mu.Lock()
	assert.Empty(t, hub.nonGET, "GET requests only")
	assert.Positive(t, hub.gets)
	hub.mu.Unlock()
	assert.Equal(t, beforeSHA, convertFileSHA(t, settingsPath), "the settings file is unchanged")
	assert.Equal(t, before, convertTreeSnapshot(t, home), "nothing under HOME was written, created or removed")
}

// No Hub configured: the local sections only, as with --skip-hub.
func TestBrokerConvert_NoHubConfiguredIsLocalOnly(t *testing.T) {
	convertTestHome(t, convertTestSettings)
	setConvertFlags(t, true, false, "", "build", "local", "k8s")
	setConvertHubClient(t, func(*config.Settings) (hubclient.Client, error) {
		return nil, errHubNotConfigured
	})
	out, err := runConvertForTest(t)
	require.NoError(t, err)
	assert.Contains(t, out, "(skipped: needs the Hub)")
}

// Mode availability as confirmed: human mode on unix only. Not in
// agentAllowed, not in windowsAllowed, and both gates remove or refuse it.
func TestBrokerConvert_ModeAvailability(t *testing.T) {
	assert.False(t, agentAllowed["runtime-broker.convert"], "not available in agent mode")
	assert.False(t, agentAllowed["runtime-broker"], "the parent is not agent-allowed either")
	assert.False(t, windowsAllowed["runtime-broker.convert"], "not available on Windows")

	root, _ := gateTestTree(t)
	applyAgentMode(root)
	c, _, err := root.Find([]string{"runtime-broker", "convert"})
	assert.True(t, err != nil || c == root || c.Name() != "convert", "agent mode removes runtime-broker convert")

	root, ran := gateTestTree(t)
	applyPlatformRestrictions(root, "windows")
	root.SetArgs([]string{"runtime-broker", "convert", "--plan"})
	err = root.Execute()
	require.Error(t, err)
	assert.True(t, errors.Is(err, errUnsupportedOnWindows), "got %v", err)
	assert.Empty(t, *ran)

	root, ran = gateTestTree(t)
	root.SetArgs([]string{"runtime-broker", "convert", "--plan"})
	require.NoError(t, root.Execute(), "human mode on unix runs it")
	assert.Equal(t, []string{"runtime-broker.convert"}, *ran)
}

// resetConvertParse undoes a ParseFlags of convert through the root: the
// command's own flags and the root's hidden --no-hub.
func resetConvertParse(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		brokerConvertCmd.Flags().VisitAll(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
		if f := rootCmd.PersistentFlags().Lookup("no-hub"); f != nil {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
		noHub = false
	})
}

// --skip-hub is convert's own flag; the root's hidden, removed --no-hub is
// not shadowed and still refuses inside convert, as everywhere else.
func TestBrokerConvert_SkipHubFlagAndRootNoHubRefusal(t *testing.T) {
	setupNoProjectPreRun(t)
	restoreSilenceUsage(t, brokerConvertCmd)
	setConvertFlags(t, false, false, "")
	resetConvertParse(t)

	assert.Nil(t, brokerConvertCmd.LocalNonPersistentFlags().Lookup("no-hub"), "convert does not shadow the root --no-hub")

	c, rest, err := rootCmd.Find([]string{"runtime-broker", "convert", "--plan", "--skip-hub"})
	require.NoError(t, err)
	require.Equal(t, brokerConvertCmd, c)
	require.NoError(t, c.ParseFlags(rest))
	assert.True(t, brokerConvertPlan)
	assert.True(t, brokerConvertSkipHub)
	assert.NoError(t, rootCmd.PersistentPreRunE(c, []string{}), "no project needed")

	c, rest, err = rootCmd.Find([]string{"runtime-broker", "convert", "--plan", "--no-hub"})
	require.NoError(t, err)
	require.NoError(t, c.ParseFlags(rest))
	assert.True(t, noHub, "--no-hub is the root's flag")
	err = rootCmd.PersistentPreRunE(c, []string{})
	assert.ErrorIs(t, err, errNoHubRemoved, "the removed root --no-hub refuses inside convert too")
}

// legacyHomeFixture adds what the settings loaders and the startup layout
// migration rewrite on a normal command: the legacy hub project key
// (projectkeys.LegacyConfigHubProjectIDKey) in the
// global settings and a legacy global layout entry.
func legacyHomeFixture(t *testing.T) (home, settingsPath string) {
	t.Helper()
	home, settingsPath = convertTestHome(t, convertTestSettings+"hub:\n  "+projectkeys.LegacyConfigProjectIDKey+": legacy-project-id\n")
	legacyDir := filepath.Join(home, ".scion", legacyLayoutDir, "acme")
	require.NoError(t, os.MkdirAll(legacyDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(legacyDir, "f"), []byte("x"), 0o644))
	warnRemovedLegacyEnvOnce = sync.Once{}
	t.Cleanup(func() { warnRemovedLegacyEnvOnce = sync.Once{} })
	t.Cleanup(func() { config.SetProjectMigrationReporter(config.NewSlogReporter()) })
	return home, settingsPath
}

// convert --plan loads the settings read-only: run as the CLI runs it (the
// pre-run hooks, the root hook, then the command), it leaves a legacy
// hub project key and a legacy layout entry exactly as they were, with
// the Hub skipped and with the fake Hub. The controls show the same
// fixture is rewritten by a normal settings load and a normal command.
func TestBrokerConvert_ReadOnlyLoadLeavesLegacyFilesAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"skip-hub", []string{"runtime-broker", "convert", "--plan", "--skip-hub", "--profiles=build,local,k8s"}},
		{"fake hub", []string{"runtime-broker", "convert", "--plan", "--profiles=build,local,k8s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupNoProjectPreRun(t)
			restoreSilenceUsage(t, brokerConvertCmd)
			setConvertFlags(t, false, false, "")
			resetConvertParse(t)
			hub := newConvertGETOnlyHub(t)
			setConvertHubClient(t, func(*config.Settings) (hubclient.Client, error) {
				return hubclient.New(hub.URL)
			})
			home, settingsPath := legacyHomeFixture(t)
			before, beforeSHA := convertTreeSnapshot(t, home), convertFileSHA(t, settingsPath)

			target := prepareInvocation(tc.args)
			require.Equal(t, brokerConvertCmd, target)
			assert.True(t, writesonread.Disabled(), "the no-write switch is on before any settings load")
			c, rest, err := rootCmd.Find(tc.args)
			require.NoError(t, err)
			require.NoError(t, c.ParseFlags(rest))
			require.NoError(t, rootCmd.PersistentPreRunE(c, []string{}))
			var out bytes.Buffer
			c.SetOut(&out)
			t.Cleanup(func() { c.SetOut(nil) })
			if c.Context() == nil {
				c.SetContext(context.Background())
			}
			require.NoError(t, runBrokerConvert(c, nil))
			assert.Contains(t, out.String(), "1. Proposed instances")

			assert.Equal(t, beforeSHA, convertFileSHA(t, settingsPath), "the settings file's bytes are unchanged")
			assert.Equal(t, before, convertTreeSnapshot(t, home), "no rewrite, migration, lock or backup file under HOME")
		})
	}

	t.Run("control: a normal settings load rewrites the legacy hub project key", func(t *testing.T) {
		writesonread.IsolateForTest(t)
		home, settingsPath := legacyHomeFixture(t)
		beforeSHA := convertFileSHA(t, settingsPath)
		_, err := config.LoadSettings(filepath.Join(home, ".scion"))
		require.NoError(t, err)
		assert.NotEqual(t, beforeSHA, convertFileSHA(t, settingsPath), "the fixture's legacy key is migrated by a normal load")
	})
	t.Run("control: a normal command migrates the legacy layout", func(t *testing.T) {
		writesonread.IsolateForTest(t)
		home, _ := legacyHomeFixture(t)
		prepareInvocation([]string{"list"})
		info, err := os.Lstat(filepath.Join(home, ".scion", legacyLayoutDir, "acme"))
		require.NoError(t, err)
		assert.NotZero(t, info.Mode()&os.ModeSymlink, "the fixture's legacy layout is migrated by a normal command")
	})
}

// isNoWriteCommand matches runtime-broker convert (and still server
// preflight), and nothing else under runtime-broker.
func TestBrokerConvert_IsNoWriteCommand(t *testing.T) {
	assert.True(t, isNoWriteCommand(brokerConvertCmd))
	assert.True(t, isNoWriteCommand(serverPreflightCmd))
	assert.False(t, isNoWriteCommand(brokerStatusCmd))
	assert.False(t, isNoWriteCommand(brokerRegisterCmd))
	stray := &cobra.Command{Use: "convert"}
	assert.False(t, isNoWriteCommand(stray), "a convert outside runtime-broker is not matched")
}

// A direct call of the command (no root hook) still turns the no-write
// switch on before it loads the settings: the legacy hub project key is left
// in the file.
func TestBrokerConvert_DirectCallSetsNoWriteSwitch(t *testing.T) {
	setConvertFlags(t, true, true, "", "build", "local", "k8s")
	home, settingsPath := legacyHomeFixture(t)
	before, beforeSHA := convertTreeSnapshot(t, home), convertFileSHA(t, settingsPath)
	require.False(t, writesonread.Disabled(), "the switch starts off")

	_, err := runConvertForTest(t)
	require.NoError(t, err)

	assert.True(t, writesonread.Disabled(), "RunE turned the switch on")
	assert.Equal(t, beforeSHA, convertFileSHA(t, settingsPath), "the legacy key is not migrated")
	assert.Equal(t, before, convertTreeSnapshot(t, home), "nothing under HOME was written, created or removed")
}

const convertTestEnvToken = "PLANTED-ENV-TOKEN-cmd-77"

// convertEnvSettings has a harness env value in the k8s profile.
const convertEnvSettings = convertTestSettings + `    harness_overrides:
      claude:
        env:
          API_TOKEN: ` + convertTestEnvToken + `
`

// --show-env-values is refused with JSON output, before any output.
func TestBrokerConvert_ShowEnvValuesRefusedWithJSON(t *testing.T) {
	convertTestHome(t, convertEnvSettings)
	setConvertFlags(t, true, true, "json", "build", "local", "k8s")
	brokerConvertShowEnv = true
	out, err := runConvertForTest(t)
	assert.ErrorIs(t, err, errShowEnvValuesJSON)
	assert.Equal(t, 1, exitCodeFor(err))
	assert.Empty(t, out)
}

// Env values are redacted by default in both formats; --show-env-values
// prints them in the human output only.
func TestBrokerConvert_EnvValuesRedactedByDefault(t *testing.T) {
	convertTestHome(t, convertEnvSettings)
	for _, format := range []string{"", "json"} {
		setConvertFlags(t, true, true, format, "build", "local", "k8s")
		out, err := runConvertForTest(t)
		require.NoError(t, err, format)
		assert.NotContains(t, out, convertTestEnvToken, "format %q", format)
		assert.Contains(t, out, "API_TOKEN", "format %q: the key is kept", format)
	}
	setConvertFlags(t, true, true, "", "build", "local", "k8s")
	brokerConvertShowEnv = true
	out, err := runConvertForTest(t)
	require.NoError(t, err)
	assert.Contains(t, out, convertTestEnvToken)
	assert.Contains(t, out, brokerconvert.ShowEnvValuesWarning)
}

// The runtime-broker command list and convert's Short use one wording.
func TestBrokerConvert_ShortMatchesCommandList(t *testing.T) {
	assert.Equal(t, brokerConvertShort, brokerConvertCmd.Short)
	assert.Contains(t, brokerCmd.Long, "  convert      "+brokerConvertShort)
}

// Settings warnings go to the command's error stream, not the process's.
func TestBrokerConvert_SettingsWarningsGoToCommandStderr(t *testing.T) {
	// v1 runtime fields without the schema_version line: the settings
	// loader warns.
	settings := strings.TrimPrefix(convertTestSettings, "schema_version: \"1\"\n")
	require.NotEqual(t, convertTestSettings, settings, "the fixture starts with the schema_version line")
	convertTestHome(t, settings)
	setConvertFlags(t, true, true, "", "build", "local", "k8s")
	var out, errOut bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&out)
	c.SetErr(&errOut)
	c.SetContext(context.Background())
	require.NoError(t, runBrokerConvert(c, nil))
	assert.Contains(t, errOut.String(), "Warning: ")
	assert.Contains(t, errOut.String(), "schema_version")
	assert.NotContains(t, out.String(), "Warning: ", "warnings are not on stdout")
}
