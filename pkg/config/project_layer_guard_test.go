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

package config

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectTierRootsClassifyEveryField is a CI tripwire that forces every
// exported top-level field of VersionedSettings to be explicitly classified as
// classAllow or classDeny in projectTierRoots, and catches stale entries.
func TestProjectTierRootsClassifyEveryField(t *testing.T) {
	vt := reflect.TypeOf(VersionedSettings{})
	structTags := make(map[string]string, vt.NumField())

	for i := 0; i < vt.NumField(); i++ {
		f := vt.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("koanf")
		if tag == "-" {
			continue
		}
		// Strip any tag options (e.g. ",omitempty").
		if idx := strings.IndexByte(tag, ','); idx >= 0 {
			tag = tag[:idx]
		}
		require.NotEmptyf(t, tag, "exported VersionedSettings.%s is missing a koanf tag", f.Name)
		structTags[tag] = f.Name

		if _, ok := projectTierRoots[tag]; !ok {
			t.Errorf("VersionedSettings.%s (koanf:%q) is not classified in projectTierRoots (pkg/config/project_layer_guard.go): "+
				"decide classAllow or classDeny and document where the key is read", f.Name, tag)
		}
	}

	for root := range projectTierRoots {
		if _, ok := structTags[root]; !ok {
			t.Errorf("projectTierRoots contains %q which does not match any exported koanf field on VersionedSettings; remove stale entry", root)
		}
	}
}

func TestProjectTierAllowed_UnknownRootIsDenied(t *testing.T) {
	assert.False(t, projectTierAllowed("unknown_future_root"))
	assert.False(t, projectTierAllowed("agent_policy"))
	assert.False(t, projectTierAllowed("server"))
	assert.True(t, projectTierAllowed("hub"))
	assert.True(t, projectTierAllowed("default_template"))
}

func TestProjectLayerGuard_DeniedRootsIgnoredAcrossLoaders(t *testing.T) {
	boolFalse := false
	_ = boolFalse

	globalYAML := `schema_version: "1"
use_host_credentials: false
default_runtime_broker: "global-broker-default"
server:
  broker:
    broker_id: "G"
    broker_token: "global-secret-token"
    broker_nickname: "global-nick"
    instances:
      - name: "global-inst"
        runtime: "docker"
  secrets:
    gcp_project_id: "global-gcp-proj"
  workspace_storage:
    backend: local
`

	cases := []struct {
		name           string
		projectFile    string // filename: settings.yaml or settings.json
		projectContent string
		useExternal    bool
		useCWD         bool
		expectProjTpl  string
		expectProjHub  string
	}{
		{
			name:        "in-repo v1 yaml",
			projectFile: "settings.yaml",
			projectContent: `schema_version: "1"
default_template: "from-project"
hub:
  project_id: "proj-hub-id"
runtimes:
  projrt:
    type: docker
use_host_credentials: true
auto_inject_gcloud_adc: true
default_runtime_broker: "attacker-broker"
auto_expose_ports:
  enabled: true
foo: 1
server:
  broker:
    broker_id: "P"
    broker_token: "project-secret-token"
    broker_nickname: "project-nick"
    instances:
      - name: "evil-inst"
        runtime: "docker"
  secrets:
    gcp_project_id: "evil-gcp-proj"
  project_storage:
    backend: nfs
    nfs:
      mount_root: /attacker
      shares:
        - id: x
          server: 10.9.9.9
          export: /x
`,
			expectProjTpl: "from-project",
			expectProjHub: "proj-hub-id",
		},
		{
			name:        "external split-storage dir",
			projectFile: "settings.yaml",
			useExternal: true,
			projectContent: `schema_version: "1"
default_template: "from-external"
hub:
  project_id: "ext-hub-id"
server:
  broker:
    broker_id: "P-EXT"
    broker_token: "ext-secret-token"
  secrets:
    gcp_project_id: "ext-gcp-proj"
`,
			expectProjTpl: "from-external",
			expectProjHub: "ext-proj-id",
		},
		{
			name:        "empty projectPath discovered via CWD",
			projectFile: "settings.yaml",
			useCWD:      true,
			projectContent: `schema_version: "1"
default_template: "from-cwd"
hub:
  project_id: "cwd-hub-id"
server:
  broker:
    broker_id: "P-CWD"
    broker_token: "cwd-secret-token"
`,
			expectProjTpl: "from-cwd",
			expectProjHub: "cwd-hub-id",
		},
		{
			name:        "legacy format with hub.brokerId",
			projectFile: "settings.yaml",
			projectContent: `default_template: "from-legacy"
hub:
  projectId: "legacy-hub-id"
  brokerId: "P-LEGACY"
  brokerToken: "legacy-secret-token"
  brokerNickname: "legacy-nick"
`,
			expectProjTpl: "from-legacy",
			expectProjHub: "legacy-hub-id",
		},
		{
			name:        "json project settings",
			projectFile: "settings.json",
			projectContent: `{
  "schema_version": "1",
  "default_template": "from-json",
  "hub": {"project_id": "json-hub-id"},
  "server": {"broker": {"broker_id": "P-JSON", "broker_token": "json-secret-token"}},
  "unknown_root": 42
}`,
			expectProjTpl: "from-json",
			expectProjHub: "json-hub-id",
		},
		{
			name:        "scalar server root",
			projectFile: "settings.yaml",
			projectContent: `schema_version: "1"
default_template: "from-scalar"
hub:
  project_id: "scalar-hub-id"
server: "not-a-map"
`,
			expectProjTpl: "from-scalar",
			expectProjHub: "scalar-hub-id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetWarnedProjectGuardKeysCache()
			home := t.TempDir()
			t.Setenv("HOME", home)

			globalDir := filepath.Join(home, ".scion")
			require.NoError(t, os.MkdirAll(globalDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(globalYAML), 0o600))

			repoRoot := filepath.Join(home, "repo")
			inRepoScion := filepath.Join(repoRoot, ".scion")
			require.NoError(t, os.MkdirAll(inRepoScion, 0o755))

			var targetSettingsDir string
			if tc.useExternal {
				// Mark in-repo project as split-storage via project-id file and
				// place settings in external project config dir.
				require.NoError(t, os.WriteFile(filepath.Join(inRepoScion, "project-id"), []byte("ext-proj-id\n"), 0o600))
				extDir := GetProjectConfigDir(inRepoScion)
				require.NotEqual(t, inRepoScion, extDir)
				require.NoError(t, os.MkdirAll(extDir, 0o755))
				targetSettingsDir = extDir
			} else {
				targetSettingsDir = inRepoScion
			}
			require.NoError(t, os.WriteFile(filepath.Join(targetSettingsDir, tc.projectFile), []byte(tc.projectContent), 0o600))

			loadPath := inRepoScion
			if tc.useCWD {
				t.Chdir(repoRoot)
				loadPath = ""
			}

			// 1. LoadVersionedSettings
			vs, err := LoadVersionedSettings(loadPath)
			require.NoError(t, err)
			assert.Equal(t, tc.expectProjTpl, vs.DefaultTemplate)
			require.NotNil(t, vs.Hub)
			assert.Equal(t, tc.expectProjHub, vs.Hub.ProjectID)
			require.NotNil(t, vs.Server)
			require.NotNil(t, vs.Server.Broker)
			assert.Equal(t, "G", vs.Server.Broker.BrokerID)
			assert.Equal(t, "global-secret-token", vs.Server.Broker.BrokerToken)
			assert.Equal(t, "global-nick", vs.Server.Broker.BrokerNickname)
			require.Len(t, vs.Server.Broker.Instances, 1)
			assert.Equal(t, "global-inst", vs.Server.Broker.Instances[0].Name)
			require.NotNil(t, vs.Server.Secrets)
			assert.Equal(t, "global-gcp-proj", vs.Server.Secrets.GCPProjectID)
			assert.Nil(t, vs.Server.ProjectStorage)
			require.NotNil(t, vs.Server.WorkspaceStorage)
			assert.Equal(t, "local", vs.Server.WorkspaceStorage.Backend)
			require.NotNil(t, vs.UseHostCredentials)
			assert.False(t, *vs.UseHostCredentials)
			assert.False(t, vs.AutoInjectGcloudADC)
			assert.Equal(t, "global-broker-default", vs.DefaultRuntimeBroker)
			assert.Nil(t, vs.AutoExposePorts)

			// 2. LoadEffectiveSettings
			eff, _, err := LoadEffectiveSettings(loadPath)
			require.NoError(t, err)
			assert.Equal(t, tc.expectProjTpl, eff.DefaultTemplate)
			require.NotNil(t, eff.Server)
			assert.Equal(t, "G", eff.Server.Broker.BrokerID)

			// 3. LoadSettings (legacy view)
			legacy, err := LoadSettings(loadPath)
			require.NoError(t, err)
			require.NotNil(t, legacy.Hub)
			assert.Equal(t, "G", legacy.Hub.BrokerID)
			assert.Equal(t, "global-secret-token", legacy.Hub.BrokerToken)
			assert.Equal(t, "global-nick", legacy.Hub.BrokerNickname)

			// 4. loadSettingValues (source attribution)
			_, loadRes, err := loadSettingValues(loadPath)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(loadRes.sources["server.broker.broker_id"], "global "),
				"expected global attribution for server.broker.broker_id, got %q", loadRes.sources["server.broker.broker_id"])
			assert.NotContains(t, loadRes.sources, "server.project_storage.backend")
			assert.NotContains(t, loadRes.sources, "foo")
		})
	}
}

func TestProjectLayerGuard_ProjectOnlyDeniedRootsZeroed(t *testing.T) {
	resetWarnedProjectGuardKeysCache()
	home := t.TempDir()
	t.Setenv("HOME", home)

	projectDir := filepath.Join(home, "proj", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(`schema_version: "1"
default_template: "my-tpl"
runtimes:
  custom:
    type: docker
hub:
  project_id: "proj-1"
server:
  broker:
    broker_id: "evil-broker"
use_host_credentials: true
default_runtime_broker: "evil-default-broker"
quotas:
  enforce_broker_quotas: false
unknown_future_key: "ignored"
`), 0o600))

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)
	assert.Equal(t, "my-tpl", vs.DefaultTemplate)
	assert.Contains(t, vs.Runtimes, "custom")
	require.NotNil(t, vs.Hub)
	assert.Equal(t, "proj-1", vs.Hub.ProjectID)
	if vs.Server != nil && vs.Server.Broker != nil {
		assert.Empty(t, vs.Server.Broker.BrokerID)
	}
	assert.Nil(t, vs.UseHostCredentials)
	assert.Empty(t, vs.DefaultRuntimeBroker)
	assert.Nil(t, vs.Quotas)
}

func TestProjectLayerGuard_EnvStillOverridesDeniedKeys(t *testing.T) {
	resetWarnedProjectGuardKeysCache()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_SERVER_BROKER_BROKER_ID", "env-broker-id")

	projectDir := filepath.Join(home, "proj", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(`schema_version: "1"
server:
  broker:
    broker_id: "project-broker-id"
`), 0o600))

	vs, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)
	require.NotNil(t, vs.Server)
	require.NotNil(t, vs.Server.Broker)
	assert.Equal(t, "env-broker-id", vs.Server.Broker.BrokerID)
}

func TestProjectLayerGuard_WarningDedupedAndNeverLogsValues(t *testing.T) {
	resetWarnedProjectGuardKeysCache()
	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(oldLogger)

	home := t.TempDir()
	t.Setenv("HOME", home)
	projectDir := filepath.Join(home, "proj", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	const secretVal = "SUPER_SECRET_BROKER_TOKEN_987654321"
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(`schema_version: "1"
server:
  broker:
    broker_id: "proj-broker"
    broker_token: "`+secretVal+`"
use_host_credentials: true
`), 0o600))

	_, err := LoadVersionedSettings(projectDir)
	require.NoError(t, err)
	firstLog := buf.String()
	assert.Equal(t, 1, strings.Count(firstLog, projectGuardWarnMessage), "expected exactly one guard warning")
	assert.Contains(t, firstLog, "server.broker.broker_id")
	assert.Contains(t, firstLog, "server.broker.broker_token")
	assert.Contains(t, firstLog, "use_host_credentials")
	assert.NotContains(t, firstLog, secretVal, "guard warning must never log setting values")
	assert.NotContains(t, firstLog, "proj-broker", "guard warning must never log setting values")

	// Second load of the same file with the same keys must be deduped.
	_, err = LoadVersionedSettings(projectDir)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(buf.String(), projectGuardWarnMessage), "expected warning to be deduped on second load")
}

func TestProjectLayerGuard_RestoreFailureFailsClosed(t *testing.T) {
	k := koanf.New(".")
	require.NoError(t, defaultRestoreProjectRoot(k, "server", map[string]any{"broker": map[string]any{"broker_id": "G"}}))
	g := guardProjectLayers(k, nil)
	g.restoreRootFn = func(*koanf.Koanf, string, any) error {
		return errors.New("injected restore error")
	}
	err := g.restore(k, nil, nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "restoring server")
}

func TestLoadProjectSettingsFromDir_StripsDeniedRoots(t *testing.T) {
	resetWarnedProjectGuardKeysCache()
	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(oldLogger)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(`schema_version: "1"
hub:
  endpoint: "https://hub.example.com"
  project_id: "proj-abc"
server:
  broker:
    broker_id: "broker-from-file"
    broker_token: "secret-from-file"
`), 0o600))

	// Project-tier single-dir loader strips denied roots and warns.
	ps, err := LoadProjectSettingsFromDir(dir)
	require.NoError(t, err)
	require.NotNil(t, ps.Hub)
	assert.Equal(t, "https://hub.example.com", ps.Hub.Endpoint)
	assert.Equal(t, "proj-abc", ps.Hub.ProjectID)
	assert.Empty(t, ps.Hub.BrokerID, "LoadProjectSettingsFromDir must strip server.broker")
	assert.Empty(t, ps.Hub.BrokerToken, "LoadProjectSettingsFromDir must strip server.broker")
	assert.Contains(t, buf.String(), projectGuardWarnMessage)
	assert.Contains(t, buf.String(), "server.broker.broker_id")
	assert.NotContains(t, buf.String(), "secret-from-file")

	// Global-tier single-dir loader does NOT strip denied roots.
	gs, err := LoadSettingsFromDir(dir)
	require.NoError(t, err)
	require.NotNil(t, gs.Hub)
	assert.Equal(t, "broker-from-file", gs.Hub.BrokerID)
	assert.Equal(t, "secret-from-file", gs.Hub.BrokerToken)
}
