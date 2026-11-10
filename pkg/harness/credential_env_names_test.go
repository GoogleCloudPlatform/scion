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

package harness

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"

	harnessesEmbed "github.com/GoogleCloudPlatform/scion/harnesses"
)

func TestCredentialEnvNames_AllSources(t *testing.T) {
	meta := &config.HarnessAuthMetadata{
		Types: map[string]config.HarnessAuthTypeMetadata{
			"api-key": {RequiredEnv: []config.HarnessAuthEnvRequirement{{AnyOf: []string{"ACME_KEY", "ACME_TOKEN"}}}},
			"file": {RequiredFiles: []config.HarnessAuthFileRequirement{
				{Name: "ACME_FILE", AlternativeEnvKeys: []string{"ACME_FILE_PATH"}},
			}},
		},
		Autodetect: config.HarnessAuthAutodetect{Env: map[string]string{"ACME_AUTO": "api-key", "ACME_KEY": "api-key"}},
	}
	assert.Equal(t, []string{"ACME_AUTO", "ACME_FILE_PATH", "ACME_KEY", "ACME_TOKEN"}, CredentialEnvNames(meta))
	// A project or region setting a Vertex-style type requires and
	// autodetects by is not a credential name.
	vertex := &config.HarnessAuthMetadata{
		Types: map[string]config.HarnessAuthTypeMetadata{
			"vertex-ai": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"GOOGLE_CLOUD_PROJECT"}},
					{AnyOf: []string{"GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION"}},
				},
				RequiredFiles: []config.HarnessAuthFileRequirement{
					{Name: "gcloud-adc", AlternativeEnvKeys: []string{"GOOGLE_APPLICATION_CREDENTIALS"}},
				},
			},
		},
		Autodetect: config.HarnessAuthAutodetect{Env: map[string]string{
			"GOOGLE_CLOUD_PROJECT": "vertex-ai", "GOOGLE_APPLICATION_CREDENTIALS": "vertex-ai",
		}},
	}
	assert.Equal(t, []string{"GOOGLE_APPLICATION_CREDENTIALS"}, CredentialEnvNames(vertex))
	assert.Nil(t, CredentialEnvNames(nil))
	assert.Empty(t, CredentialEnvNames(&config.HarnessAuthMetadata{}))
}

// TestBuiltinCredentialEnvNames_CoversEveryBuiltin checks the built-in set
// is not empty and holds every credential name each embedded harness
// config declares, exactly as declared.
func TestBuiltinCredentialEnvNames_CoversEveryBuiltin(t *testing.T) {
	got := BuiltinCredentialEnvNames()
	require.NotEmpty(t, got)
	set := make(map[string]bool, len(got))
	for _, name := range got {
		set[name] = true
	}
	var declared int
	for _, harnessName := range AllHarnessNames() {
		data, err := fs.ReadFile(harnessesEmbed.FS, harnessName+"/config.yaml")
		require.NoError(t, err)
		entry, err := config.ParseHarnessConfigYAML(data)
		require.NoError(t, err)
		for _, name := range CredentialEnvNames(entry.Auth) {
			declared++
			assert.True(t, set[name], "%s declares %s", harnessName, name)
		}
	}
	require.Positive(t, declared, "the built-in harness configs declare credential env names")
	// Copilot's api-key names, as declared in harnesses/copilot/config.yaml.
	for _, name := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		assert.True(t, set[name], name)
	}
	assert.False(t, set["copilot_github_token"], "names match exactly")

	// The returned slice is a copy.
	got[0] = "MUTATED"
	assert.NotEqual(t, "MUTATED", BuiltinCredentialEnvNames()[0])
}

// nonSecretNamePattern and secretNameWords define what may be on the
// non-secret list: a project, region or location name, and nothing that
// names a key, token, secret, credential, password or auth value.
var (
	nonSecretNamePattern = regexp.MustCompile(`PROJECT|REGION|LOCATION`)
	secretNameWords      = []string{"KEY", "TOKEN", "SECRET", "CREDENTIAL", "PASSWORD", "AUTH"}
)

// TestNonSecretAuthEnvNames_OnlyProjectAndRegion: every name on the
// non-secret list names a project, region or location and none names a
// credential.
func TestNonSecretAuthEnvNames_OnlyProjectAndRegion(t *testing.T) {
	require.Len(t, nonSecretAuthEnvNames, 8)
	for _, name := range nonSecretAuthEnvNames {
		assert.Regexp(t, nonSecretNamePattern, name, "%s must name a project, region or location", name)
		for _, word := range secretNameWords {
			assert.NotContains(t, name, word, "%s must not name a credential", name)
		}
	}
}

// TestCredentialEnvNames_AlternativeAndAutodetectWithheld: every
// alternative_env_keys entry of every built-in config is a credential name
// and is never on the non-secret list; every autodetect key is a credential
// name unless it is on the non-secret list (which TestNonSecretAuthEnvNames_OnlyProjectAndRegion bounds).
func TestCredentialEnvNames_AlternativeAndAutodetectWithheld(t *testing.T) {
	set := map[string]bool{}
	for _, name := range BuiltinCredentialEnvNames() {
		set[name] = true
	}
	var alternatives, autodetect int
	for _, harnessName := range AllHarnessNames() {
		data, err := fs.ReadFile(harnessesEmbed.FS, harnessName+"/config.yaml")
		require.NoError(t, err)
		entry, err := config.ParseHarnessConfigYAML(data)
		require.NoError(t, err)
		if entry.Auth == nil {
			continue
		}
		for _, typ := range entry.Auth.Types {
			for _, f := range typ.RequiredFiles {
				for _, name := range f.AlternativeEnvKeys {
					alternatives++
					assert.False(t, isNonSecretAuthEnvName(name), "%s: alternative env name %s is on the non-secret list", harnessName, name)
					assert.True(t, set[name], "%s: alternative env name %s is withheld", harnessName, name)
				}
			}
		}
		for name := range entry.Auth.Autodetect.Env {
			autodetect++
			if isNonSecretAuthEnvName(name) {
				continue
			}
			assert.True(t, set[name], "%s: autodetect name %s is withheld", harnessName, name)
		}
	}
	require.Positive(t, alternatives, "the built-in configs declare alternative env names")
	require.Positive(t, autodetect, "the built-in configs declare autodetect env names")
	assert.True(t, set["GOOGLE_APPLICATION_CREDENTIALS"])
}

// TestCredentialEnvNames_NewRequiredNameWithheld: a name a config
// adds to required_env is a credential name without any list change.
func TestCredentialEnvNames_NewRequiredNameWithheld(t *testing.T) {
	meta := &config.HarnessAuthMetadata{Types: map[string]config.HarnessAuthTypeMetadata{
		"api-key": {RequiredEnv: []config.HarnessAuthEnvRequirement{{AnyOf: []string{"EXAMPLE_NEW_TOKEN"}}}},
	}}
	assert.Equal(t, []string{"EXAMPLE_NEW_TOKEN"}, CredentialEnvNames(meta))
}

// TestBuiltinCredentialEnvNames_APIKeyNamesWithheld guards the built-in set
// against a harness config change that drops a sign-in name: every built-in
// required_env name that is not on the non-secret list is in the set, and
// the well-known API-key and token names are.
func TestBuiltinCredentialEnvNames_APIKeyNamesWithheld(t *testing.T) {
	set := map[string]bool{}
	for _, name := range BuiltinCredentialEnvNames() {
		set[name] = true
	}
	for _, name := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "GEMINI_API_KEY", "GOOGLE_API_KEY",
		"OPENAI_API_KEY", "CODEX_API_KEY", "XAI_API_KEY", "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		assert.True(t, set[name], name)
	}
	for name := range set {
		assert.False(t, isNonSecretAuthEnvName(name), "%s is on the non-secret list", name)
		assert.False(t, strings.TrimSpace(name) != name, "%q is exact", name)
	}
}
