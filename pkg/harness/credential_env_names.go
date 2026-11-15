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
	"slices"
	"sort"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/config"

	harnessesEmbed "github.com/GoogleCloudPlatform/scion/harnesses"
)

// nonSecretAuthEnvNames are the names harness auth metadata lists that are
// not credentials: the cloud project and region settings a Vertex-style auth
// type requires (and autodetects by). CredentialEnvNames leaves them out of
// the required_env and autodetect names, so an agent started without
// credentials keeps a plain project or region setting. A credential file's
// alternative env names (alternative_env_keys) are never left out. Every
// name here names a project, region or location, and none a key, token,
// secret, credential, password or auth value (enforced by a test).
var nonSecretAuthEnvNames = [...]string{
	"GOOGLE_CLOUD_PROJECT",
	"GOOGLE_CLOUD_REGION",
	"GOOGLE_CLOUD_LOCATION",
	"CLOUD_ML_REGION",
	"VERTEX_PROJECT_ID",
	"VERTEXAI_PROJECT",
	"VERTEX_LOCATION",
	"SCION_METADATA_PROJECT_ID",
}

func isNonSecretAuthEnvName(name string) bool {
	for _, n := range nonSecretAuthEnvNames {
		if n == name {
			return true
		}
	}
	return false
}

// CredentialEnvNames returns the env names authMeta declares as harness
// credentials, sorted and without duplicates: every auth type's required
// env names (required_env any_of) and the autodetect env names, except the
// project and region settings in nonSecretAuthEnvNames, plus every required
// file's alternative env names (alternative_env_keys), always. Names are
// returned exactly as declared. A nil authMeta declares none.
func CredentialEnvNames(authMeta *config.HarnessAuthMetadata) []string {
	if authMeta == nil {
		return nil
	}
	seen := make(map[string]bool)
	var names []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	for _, t := range authMeta.Types {
		for _, req := range t.RequiredEnv {
			for _, name := range req.AnyOf {
				if !isNonSecretAuthEnvName(name) {
					add(name)
				}
			}
		}
		for _, f := range t.RequiredFiles {
			for _, name := range f.AlternativeEnvKeys {
				add(name)
			}
		}
	}
	for name := range authMeta.Autodetect.Env {
		if !isNonSecretAuthEnvName(name) {
			add(name)
		}
	}
	sort.Strings(names)
	return names
}

// builtinCredentialEnvNames is computed once: the embedded harness
// configs do not change while the process runs.
var builtinCredentialEnvNames = sync.OnceValue(func() []string {
	seen := make(map[string]bool)
	var names []string
	for _, harnessName := range AllHarnessNames() {
		data, err := fs.ReadFile(harnessesEmbed.FS, harnessName+"/config.yaml")
		if err != nil {
			continue
		}
		entry, err := config.ParseHarnessConfigYAML(data)
		if err != nil {
			continue
		}
		for _, name := range CredentialEnvNames(entry.Auth) {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
})

// BuiltinCredentialEnvNames returns CredentialEnvNames over every built-in
// harness config (the embedded harnesses/<name>/config.yaml files), sorted
// and without duplicates. The caller may modify the returned slice.
func BuiltinCredentialEnvNames() []string {
	return slices.Clone(builtinCredentialEnvNames())
}
