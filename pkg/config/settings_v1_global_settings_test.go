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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestNativeChatConfig_Tristate pins the default-on contract: native chat
// shipped enabled, so only an explicit "enabled: false" may turn it off. A
// missing section or a missing key must not silently disable the feature.
func TestNativeChatConfig_Tristate(t *testing.T) {
	enabled, disabled := true, false

	tests := []struct {
		name string
		cfg  *V1NativeChatConfig
		want *bool
	}{
		{"absent section", nil, nil},
		{"absent key", &V1NativeChatConfig{}, nil},
		{"explicit true", &V1NativeChatConfig{Enabled: &enabled}, &enabled},
		{"explicit false", &V1NativeChatConfig{Enabled: &disabled}, &disabled},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.cfg.EnabledSetting())
		})
	}
}

// TestNativeChatConfig_YAMLRoundTrip verifies that an explicit disable
// survives a marshal/unmarshal cycle. With a plain bool the "false" would be
// dropped by omitempty and read back as enabled — the pointer prevents that.
func TestNativeChatConfig_YAMLRoundTrip(t *testing.T) {
	disabled := false
	in := &V1ServerConfig{NativeChat: &V1NativeChatConfig{Enabled: &disabled}}

	data, err := yaml.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(data), "native_chat")

	var out V1ServerConfig
	require.NoError(t, yaml.Unmarshal(data, &out))
	require.NotNil(t, out.NativeChat)
	require.NotNil(t, out.NativeChat.Enabled)
	assert.False(t, *out.NativeChat.Enabled)
}

// TestNativeChatConfig_ThreadedToGlobalConfig ensures the hub can actually
// read the toggle: it is carried from the versioned settings into GlobalConfig,
// which is what the hub server config is built from.
func TestNativeChatConfig_ThreadedToGlobalConfig(t *testing.T) {
	disabled := false

	gc := ConvertV1ServerToGlobalConfig(&V1ServerConfig{
		NativeChat: &V1NativeChatConfig{Enabled: &disabled},
	})
	require.NotNil(t, gc.NativeChat)
	require.NotNil(t, gc.NativeChat.EnabledSetting())
	assert.False(t, *gc.NativeChat.EnabledSetting())

	// No section configured — the hub sees "no preference" and defaults on.
	gcDefault := ConvertV1ServerToGlobalConfig(&V1ServerConfig{})
	assert.Nil(t, gcDefault.NativeChat.EnabledSetting())
}

// TestGlobalSettingsMentions covers round 3 review disposition 6': the raw
// substring check pkg/agent.Start uses to decide whether a malformed global
// settings.yaml plausibly configured server.shared_dir_storage.
func TestGlobalSettingsMentions(t *testing.T) {
	t.Run("mentions the substring", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		globalScionDir := filepath.Join(tmpDir, ".scion")
		require.NoError(t, os.MkdirAll(globalScionDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"),
			[]byte("schema_version: \"1\"\nserver: {shared_dir_storage: [\n"), 0644))

		assert.True(t, GlobalSettingsMentions("shared_dir_storage"))
	})

	t.Run("does not mention the substring", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		globalScionDir := filepath.Join(tmpDir, ".scion")
		require.NoError(t, os.MkdirAll(globalScionDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"),
			[]byte("schema_version: \"1\"\nsomething: [\n"), 0644))

		assert.False(t, GlobalSettingsMentions("shared_dir_storage"))
	})

	t.Run("no global settings file at all", func(t *testing.T) {
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0755))

		assert.False(t, GlobalSettingsMentions("shared_dir_storage"))
	})

	// Round 4 review nit T3 (optional, done since trivial here): a file
	// that exists but can't be read returns false ("assume not configured")
	// rather than erroring — skipped when running as root, since root can
	// read a 000-mode file regardless.
	t.Run("settings file exists but is unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root can read a 0000-mode file")
		}
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		globalScionDir := filepath.Join(tmpDir, ".scion")
		require.NoError(t, os.MkdirAll(globalScionDir, 0755))
		path := filepath.Join(globalScionDir, "settings.yaml")
		require.NoError(t, os.WriteFile(path, []byte("server: {shared_dir_storage: {}}\n"), 0644))
		require.NoError(t, os.Chmod(path, 0000))
		defer func() { _ = os.Chmod(path, 0644) }()

		assert.False(t, GlobalSettingsMentions("shared_dir_storage"))
	})
}

// ii2HubEnvNames is the full 21-name SCION_* env var list of the running
// scion-hub process on scion-integration2 (findings/ii2-hub-env-names.txt,
// round 6 addendum Update 04:40), with plausible values: comma lists for
// AUTHORIZEDDOMAINS/ADMINEMAILS, "true" for CLOUD_LOGGING, strings
// elsewhere.
var ii2HubEnvNames = map[string]string{
	"SCION_CLOUD_LOGGING":                           "true",
	"SCION_DEV_BINARIES":                            "false",
	"SCION_GCP_PROJECT_ID":                          "deploy-demo-test",
	"SCION_HUB_ENDPOINT":                            "https://community.projects.scion-ai.dev",
	"SCION_HUB_STORAGE_BUCKET":                      "scion-hub-storage",
	"SCION_IMAGE_REGISTRY":                          "us-docker.pkg.dev/deploy-demo-test/scion",
	"SCION_MAINTENANCE_REPO_BRANCH":                 "main",
	"SCION_MAINTENANCE_REPO_PATH":                   "/srv/scion-maintenance",
	"SCION_SERVER_AUTH_AUTHORIZEDDOMAINS":           "example.com,corp.example.com",
	"SCION_SERVER_BASE_URL":                         "https://community.projects.scion-ai.dev",
	"SCION_SERVER_HUB_ADMINEMAILS":                  "admin@example.com,ops@example.com",
	"SCION_SERVER_HUB_GCPPROJECTID":                 "deploy-demo-test",
	"SCION_SERVER_LOG_LEVEL":                        "info",
	"SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTID":        "cli-client-id.apps.googleusercontent.com",
	"SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTSECRET":    "cli-client-secret",
	"SCION_SERVER_OAUTH_DEVICE_GOOGLE_CLIENTID":     "device-client-id.apps.googleusercontent.com",
	"SCION_SERVER_OAUTH_DEVICE_GOOGLE_CLIENTSECRET": "device-client-secret",
	"SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENTID":        "web-client-id.apps.googleusercontent.com",
	"SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENTSECRET":    "web-client-secret",
	"SCION_SERVER_SECRETS_BACKEND":                  "gcp",
	"SCION_SERVER_SECRETS_GCPPROJECTID":             "deploy-demo-test",
}

// TestLoadGlobalSettings_EnvFree is the round 6 addendum's headline fix
// (hy-em/hy-rev-6/hy-aud-6, nfs-gke UAT blocker): LoadGlobalSettings must
// not consult any SCION_ environment variable at all, at the loader level.
func TestLoadGlobalSettings_EnvFree(t *testing.T) {
	setGlobal := func(t *testing.T, yamlBody string) {
		t.Helper()
		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		globalScionDir := filepath.Join(tmpDir, ".scion")
		require.NoError(t, os.MkdirAll(globalScionDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(yamlBody), 0644))
	}

	const nfsGlobalYAML = `schema_version: "1"
active_profile: local
server:
  shared_dir_storage:
    backend: nfs
    nfs:
      mount_root: /srv
      shares:
        - id: scion-shared
`
	const unsetGlobalYAML = `schema_version: "1"
active_profile: local
`

	setIi2Env := func(t *testing.T) {
		t.Helper()
		for k, v := range ii2HubEnvNames {
			t.Setenv(k, v)
		}
	}

	t.Run("ii2 21-var set alone does not break the loader", func(t *testing.T) {
		setGlobal(t, nfsGlobalYAML)
		setIi2Env(t)

		vs, warnings, err := LoadGlobalSettings()
		require.NoError(t, err)
		assert.Empty(t, warnings)
		require.NotNil(t, vs.Server)
		require.NotNil(t, vs.Server.SharedDirStorage)
		assert.Equal(t, "nfs", vs.Server.SharedDirStorage.Backend)
	})

	// This is the exact regression: SCION_AUTO_EXPOSE_PORTS maps to the
	// bare key "auto_expose_ports", which collides with a struct-typed
	// field and made koanf's Unmarshal fail outright pre-fix.
	collidingVars := map[string]string{
		"SCION_AUTO_EXPOSE_PORTS": "true",
		"SCION_SERVER":            "x",
		"SCION_TELEMETRY":         "x",
	}
	for name, value := range collidingVars {
		t.Run("ii2 set plus colliding var "+name+" still loads the nfs block", func(t *testing.T) {
			setGlobal(t, nfsGlobalYAML)
			setIi2Env(t)
			t.Setenv(name, value)

			vs, warnings, err := LoadGlobalSettings()
			require.NoError(t, err, "an unrelated SCION_* var must never break the global-only, env-free shared_dir_storage read")
			assert.Empty(t, warnings)
			require.NotNil(t, vs.Server)
			require.NotNil(t, vs.Server.SharedDirStorage)
			assert.Equal(t, "nfs", vs.Server.SharedDirStorage.Backend)
			require.NotNil(t, vs.Server.SharedDirStorage.NFS)
			assert.Equal(t, "/srv", vs.Server.SharedDirStorage.NFS.MountRoot)
		})

		t.Run("ii2 set plus colliding var "+name+" with an unset block behaves like main", func(t *testing.T) {
			setGlobal(t, unsetGlobalYAML)
			setIi2Env(t)
			t.Setenv(name, value)

			vs, _, err := LoadGlobalSettings()
			require.NoError(t, err)
			assert.Nil(t, vs.Server, "an unset shared_dir_storage block must stay unset, not be produced by env leakage")
			assert.False(t, GlobalSettingsIsLegacyFormat())
		})
	}

	t.Run("mount_root env override has no effect (no env mapping exists for this setting)", func(t *testing.T) {
		setGlobal(t, nfsGlobalYAML)
		t.Setenv("SCION_SERVER_SHARED_DIR_STORAGE_NFS_MOUNT_ROOT", "/evil")

		vs, _, err := LoadGlobalSettings()
		require.NoError(t, err)
		require.NotNil(t, vs.Server)
		require.NotNil(t, vs.Server.SharedDirStorage)
		require.NotNil(t, vs.Server.SharedDirStorage.NFS)
		assert.Equal(t, "/srv", vs.Server.SharedDirStorage.NFS.MountRoot, "there is no env mapping for this Layer-0 setting; the env-free read makes this doubly true")
	})
}

// TestLoadGlobalSettings_GlobalOnly_IgnoresProjectConfigsLeak is round 6
// addendum Update 04:45 (hy-aud-6, folded in because the loader was being
// rewritten anyway): LoadGlobalSettings/GlobalSettingsIsLegacyFormat must
// not merge ~/.scion/project-configs/<slug>__<id>/.scion/settings.yaml just
// because ~/.scion itself happens to contain a project-id file —
// resolveEffectiveProjectPath(globalDir) treats globalDir
// AS IF it might be a project directory and would otherwise pick that
// unrelated file up as globalDir's own "project layer".
func TestLoadGlobalSettings_GlobalOnly_IgnoresProjectConfigsLeak(t *testing.T) {
	const evilProjectConfigYAML = `schema_version: "1"
server:
  shared_dir_storage:
    backend: nfs
    nfs:
      mount_root: /evil
      shares:
        - id: evil-shared
`

	setup := func(t *testing.T, globalYAML string) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		globalScionDir := filepath.Join(home, ".scion")
		require.NoError(t, os.MkdirAll(globalScionDir, 0755))

		// ~/.scion carries its own project identity — plausible on a broker
		// whose ~/.scion doubles as a hub-side project checkout.
		require.NoError(t, os.WriteFile(
			filepath.Join(globalScionDir, "project-id"),
			[]byte("11111111-2222-3333-4444-555555555555\n"), 0644))

		require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalYAML), 0644))

		// The unrelated project's split-storage settings.yaml that
		// resolveEffectiveProjectPath(globalDir) would (pre-fix) merge in
		// as if it were globalDir's own project-level file.
		externalDir, err := GetGitProjectExternalConfigDir(globalScionDir)
		require.NoError(t, err)
		require.NotEmpty(t, externalDir)
		require.NoError(t, os.MkdirAll(externalDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(externalDir, "settings.yaml"), []byte(evilProjectConfigYAML), 0644))
	}

	t.Run("global file has the block: the evil project-configs file must not override it", func(t *testing.T) {
		setup(t, `schema_version: "1"
server:
  shared_dir_storage:
    backend: nfs
    nfs:
      mount_root: /srv
      shares:
        - id: scion-shared
`)

		assert.False(t, GlobalSettingsIsLegacyFormat())

		vs, _, err := LoadGlobalSettings()
		require.NoError(t, err)
		require.NotNil(t, vs.Server)
		require.NotNil(t, vs.Server.SharedDirStorage)
		require.NotNil(t, vs.Server.SharedDirStorage.NFS)
		assert.Equal(t, "/srv", vs.Server.SharedDirStorage.NFS.MountRoot,
			"must read the GLOBAL file's mount_root, not the leaked project-configs one")
		require.Len(t, vs.Server.SharedDirStorage.NFS.Shares, 1)
		assert.Equal(t, "scion-shared", vs.Server.SharedDirStorage.NFS.Shares[0].ID)
	})

	t.Run("global file has no block: the evil project-configs file must not inject one", func(t *testing.T) {
		setup(t, `schema_version: "1"
active_profile: local
`)

		assert.False(t, GlobalSettingsIsLegacyFormat())

		vs, _, err := LoadGlobalSettings()
		require.NoError(t, err)
		if vs.Server != nil {
			assert.Nil(t, vs.Server.SharedDirStorage, "the evil project-configs block must not leak into a global-only read")
		}
	})
}

// TestGlobalSettingsIsLegacyFormat_LegacyGlobalWithV1ProjectConfigsOverlay is
// round 6 disposition item 1 (rev L1 = tst L2, reviews/
// r6-test-probe_legacy_overlay_test.go.txt): the prior
// TestLoadGlobalSettings_GlobalOnly_IgnoresProjectConfigsLeak test above only
// used a v1 GLOBAL file, so it can't tell the difference between
// GlobalSettingsIsLegacyFormat() correctly using detectDirSettingsFormat
// (global file only) and a regression back to detectHierarchyFormat (which
// also consults the project layer) — because in that test the global file
// was already versioned either way. This is exactly the pre-fix bug at
// 9174c89c: a LEGACY (no schema_version) global file that DOES contain a
// real shared_dir_storage block, combined with a project-id file under
// ~/.scion PLUS a v1-format project-configs settings.yaml, made
// detectHierarchyFormat(globalDir) see the v1 project file and report
// hasVersioned=true — so the legacy gate in run.go never fired, and an
// operator's real nfs config silently vanished (the legacy Settings struct
// has no server.shared_dir_storage field at all) instead of failing closed.
// GlobalSettingsIsLegacyFormat must report true here regardless of what the
// project layer contains, because it must key off the global file alone.
func TestGlobalSettingsIsLegacyFormat_LegacyGlobalWithV1ProjectConfigsOverlay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Isolate from any project the test happens to run inside of —
	// resolveEffectiveProjectPath("") would otherwise also consult the
	// process's CWD, which is irrelevant to what this test is pinning.
	t.Chdir(t.TempDir())

	globalScionDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	// Legacy format (no schema_version), but WITH a real shared_dir_storage
	// block — the block the legacy Settings struct silently drops.
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"),
		[]byte("active_profile: local\nserver:\n  shared_dir_storage:\n    backend: nfs\n"), 0644))

	// ~/.scion carries its own project identity.
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "project-id"),
		[]byte("11111111-2222-3333-4444-555555555555\n"), 0644))

	// A v1-format project-configs file — the exact input that made
	// detectHierarchyFormat(globalDir) see "hasVersioned=true" pre-fix.
	externalDir, err := GetGitProjectExternalConfigDir(globalScionDir)
	require.NoError(t, err)
	require.NotEmpty(t, externalDir)
	require.NoError(t, os.MkdirAll(externalDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(externalDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nactive_profile: local\n"), 0644))

	// Pin the actual pre-fix failure mode: detectHierarchyFormat(globalDir)
	// (the function GlobalSettingsIsLegacyFormat used to call) does see the
	// project layer as versioned. This is intentional, documented behavior
	// for detectHierarchyFormat's OTHER callers (general project settings
	// loading) — the point of this test is that GlobalSettingsIsLegacyFormat
	// must NOT be one of them.
	hasVersioned, _ := detectHierarchyFormat(globalScionDir)
	assert.True(t, hasVersioned,
		"sanity check: detectHierarchyFormat does see the project-configs file as versioned — "+
			"that's why GlobalSettingsIsLegacyFormat must not use it")

	assert.True(t, GlobalSettingsIsLegacyFormat(),
		"the legacy gate must key off the global file alone, not a project-configs overlay "+
			"(mutant E3: reverting to detectHierarchyFormat must fail this assertion)")
	assert.True(t, GlobalSettingsMentions("shared_dir_storage"))

	// Pin loadGlobalSettingsOnly's OWN format decision too (mutant E4),
	// independent of GlobalSettingsIsLegacyFormat: the legacy Settings
	// struct has no server.shared_dir_storage field at all, so a correctly
	// legacy-loaded read must come back with Server == nil. If
	// loadGlobalSettingsOnly's decision were reverted to
	// detectHierarchyFormat, it would take the VERSIONED branch instead
	// (since the project-configs overlay reports hasVersioned=true) and
	// this well-formed-enough-to-parse-as-v1-too YAML would then load
	// Server.SharedDirStorage.Backend="nfs" — silently promoting a
	// legacy-dropped config into a live one instead of failing closed.
	vs, _, err := LoadGlobalSettings()
	require.NoError(t, err)
	assert.Nil(t, vs.Server, "a legacy-format global file must load via the legacy adapter (Server nil), not the versioned one")
}

// TestLoadGlobalSettingsWithOverlay_IgnoresProjectConfigsSettings checks
// that only the global settings file is read, even when the global directory
// carries a project-id file whose project-configs settings.yaml sets runtime
// values. The overlay itself is covered by TestLoadGlobalSettingsWithOverlay.
func TestLoadGlobalSettingsWithOverlay_IgnoresProjectConfigsSettings(t *testing.T) {
	prev := GetGlobalSettingsOverlay()
	t.Cleanup(func() { SetGlobalSettingsOverlay(prev) })
	SetGlobalSettingsOverlay(nil)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	globalScionDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
runtimes:
  k8s:
    type: kubernetes
    namespace: ns-global
`), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "project-id"),
		[]byte("11111111-2222-3333-4444-555555555555\n"), 0644))
	externalDir, err := GetGitProjectExternalConfigDir(globalScionDir)
	require.NoError(t, err)
	require.NotEmpty(t, externalDir)
	require.NoError(t, os.MkdirAll(externalDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(externalDir, "settings.yaml"), []byte(`schema_version: "1"
runtimes:
  k8s:
    namespace: ns-project-config
    kubernetes_service_account_mappings:
      agent-worker@my-project.iam.gserviceaccount.com: project-config-ksa
`), 0644))

	vs, _, err := LoadGlobalSettingsWithOverlay()
	require.NoError(t, err)
	assert.Equal(t, "ns-global", vs.Runtimes["k8s"].Namespace)
	_, mapped := vs.ResolveKubernetesServiceAccountMappingForSelection("", "k8s", "agent-worker@my-project.iam.gserviceaccount.com")
	assert.False(t, mapped, "the project-configs mapping must not be used")
}
