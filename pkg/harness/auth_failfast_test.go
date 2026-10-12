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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func failfastAuthMeta() *config.HarnessAuthMetadata {
	return &config.HarnessAuthMetadata{
		Types: map[string]api.HarnessAuthTypeMetadata{
			"api-key": {RequiredEnv: []api.HarnessAuthEnvRequirement{{AnyOf: []string{"EXAMPLE_API_KEY", "EXAMPLE_ALT_KEY"}}}},
			"auth-file": {RequiredFiles: []api.HarnessAuthFileRequirement{{
				Name: "EXAMPLE_AUTH", TargetSuffix: "/.example/auth.json", Field: "ExampleAuthFile",
			}}},
			"vertex-ai": {
				RequiredEnv: []api.HarnessAuthEnvRequirement{
					{AnyOf: []string{"GOOGLE_CLOUD_PROJECT"}},
					{AnyOf: []string{"GOOGLE_CLOUD_REGION"}},
				},
				RequiredFiles: []api.HarnessAuthFileRequirement{{
					Name: "gcloud-adc", Field: "GoogleAppCredentials", AlternativeEnvKeys: []string{"GOOGLE_APPLICATION_CREDENTIALS"},
				}},
			},
		},
	}
}

// envOnlyAuthMeta has no type with required files, so branch 2 (no explicit
// type, no no_auth) can reject.
func envOnlyAuthMeta() *config.HarnessAuthMetadata {
	return &config.HarnessAuthMetadata{
		Types: map[string]api.HarnessAuthTypeMetadata{
			"api-key":     failfastAuthMeta().Types["api-key"],
			"oauth-token": {RequiredEnv: []api.HarnessAuthEnvRequirement{{AnyOf: []string{"EXAMPLE_OAUTH_TOKEN"}}}},
		},
	}
}

func stageCandidates(t *testing.T, payload map[string]interface{}) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".scion", "harness", "inputs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	full := map[string]interface{}{
		"schema_version": 1, "explicit_type": "", "env_vars": []string{},
		"env_secret_files": map[string]string{}, "file_secret_files": map[string]string{}, "files": []interface{}{},
	}
	for k, v := range payload {
		full[k] = v
	}
	data, _ := json.Marshal(full)
	if err := os.WriteFile(filepath.Join(dir, "auth-candidates.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestCheckStagedAuth(t *testing.T) {
	dropToShell := &config.HarnessNoAuthConfig{Behavior: "drop-to-shell"}
	cases := []struct {
		name    string
		meta    *config.HarnessAuthMetadata
		staged  map[string]interface{}
		noAuth  *config.HarnessNoAuthConfig
		env     map[string]string
		wantErr string // substring; "" means no error
	}{
		// Env-only types are checked.
		{name: "explicit env-only type missing", staged: map[string]interface{}{"explicit_type": "api-key"},
			noAuth: dropToShell, wantErr: `auth type "api-key" is selected`},
		{name: "explicit env-only type missing names keys and accepted types", staged: map[string]interface{}{"explicit_type": "api-key"},
			wantErr: "(expects env EXAMPLE_API_KEY or EXAMPLE_ALT_KEY); accepted auth types: api-key, auth-file, vertex-ai"},
		{name: "explicit env-only type missing, only an unrelated key staged", staged: map[string]interface{}{"explicit_type": "api-key",
			"env_vars": []string{"GOOGLE_CLOUD_REGION"}}, wantErr: `auth type "api-key" is selected`},
		{name: "explicit env-only type present in env_vars", staged: map[string]interface{}{"explicit_type": "api-key",
			"env_vars": []string{"EXAMPLE_API_KEY"}}},
		{name: "explicit env-only type present as alternative key", staged: map[string]interface{}{"explicit_type": "api-key",
			"env_vars": []string{"EXAMPLE_ALT_KEY"}}},
		{name: "explicit env-only type present in env_secret_files", staged: map[string]interface{}{"explicit_type": "api-key",
			"env_secret_files": map[string]string{"EXAMPLE_API_KEY": "$HOME/.scion/harness/secrets/EXAMPLE_API_KEY"}}},
		{name: "explicit env-only type present in start env", staged: map[string]interface{}{"explicit_type": "api-key"},
			env: map[string]string{"EXAMPLE_API_KEY": "placeholder"}},
		// Types with required files are never rejected.
		{name: "explicit file type, nothing staged", staged: map[string]interface{}{"explicit_type": "auth-file"}},
		{name: "explicit env-and-file type, nothing staged", staged: map[string]interface{}{"explicit_type": "vertex-ai"}},
		{name: "no explicit, no no_auth, a file type declared, nothing staged"},
		// Other branches.
		{name: "explicit type unknown to Go is allowed", staged: map[string]interface{}{"explicit_type": "custom"}},
		{name: "no explicit, no_auth allows", meta: envOnlyAuthMeta(), noAuth: dropToShell},
		{name: "no explicit, no no_auth, env-only harness, nothing staged", meta: envOnlyAuthMeta(),
			wantErr: "does not allow starting without credentials; provide credentials for one of the accepted auth types: api-key, oauth-token"},
		{name: "no explicit, blank no_auth behaviour, env-only harness", meta: envOnlyAuthMeta(), noAuth: &config.HarnessNoAuthConfig{Behavior: "  "},
			wantErr: "does not allow starting without credentials"},
		{name: "no explicit, no no_auth, env-only harness, one type satisfied", meta: envOnlyAuthMeta(),
			staged: map[string]interface{}{"env_vars": []string{"EXAMPLE_OAUTH_TOKEN"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := tc.meta
			if meta == nil {
				meta = failfastAuthMeta()
			}
			home := stageCandidates(t, tc.staged)
			err := CheckStagedAuth("example", meta, tc.noAuth, AuthCheckInputs{AgentHome: home, Env: tc.env})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckStagedAuth = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrNoAuthSatisfied) {
				t.Fatalf("CheckStagedAuth = %v, want ErrNoAuthSatisfied", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "example harness") {
				t.Errorf("error %q, want it to name the harness and contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestCheckStagedAuthNoTypesNeverFails(t *testing.T) {
	home := stageCandidates(t, map[string]interface{}{"explicit_type": "api-key"})
	if err := CheckStagedAuth("example", nil, nil, AuthCheckInputs{AgentHome: home}); err != nil {
		t.Errorf("nil auth metadata: %v", err)
	}
	if err := CheckStagedAuth("example", &config.HarnessAuthMetadata{}, nil, AuthCheckInputs{AgentHome: home}); err != nil {
		t.Errorf("no auth types: %v", err)
	}
}

func TestCheckStagedAuthUnreadableCandidatesAllowed(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".scion", "harness", "inputs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth-candidates.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckStagedAuth("example", envOnlyAuthMeta(), nil, AuthCheckInputs{AgentHome: home}); err != nil {
		t.Errorf("malformed candidates file: %v, want nil", err)
	}
}

func TestCheckStagedAuthErrorHasNoValues(t *testing.T) {
	home := stageCandidates(t, map[string]interface{}{"explicit_type": "api-key",
		"env_vars": []string{"OTHER"}})
	err := CheckStagedAuth("example", failfastAuthMeta(), nil, AuthCheckInputs{AgentHome: home, Env: map[string]string{"OTHER": "placeholder-secret-value"}})
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "placeholder-secret-value") {
		t.Errorf("error leaks an env value: %q", err)
	}
}

// A missing candidates file fails open, like an unreadable one.
func TestCheckStagedAuthMissingCandidatesAllowed(t *testing.T) {
	if err := CheckStagedAuth("example", envOnlyAuthMeta(), nil, AuthCheckInputs{AgentHome: t.TempDir()}); err != nil {
		t.Errorf("missing candidates file: %v, want nil", err)
	}
}

// The provisioner's environment passes every SCION_* variable of the
// container through, which an env_fallback method can read and the broker
// cannot fully see, so a type with a SCION_* key is always satisfied. A type
// without one is still checked.
func TestCheckStagedAuthScionKeysFailOpen(t *testing.T) {
	meta := &config.HarnessAuthMetadata{Types: map[string]api.HarnessAuthTypeMetadata{
		"metadata": {RequiredEnv: []api.HarnessAuthEnvRequirement{{AnyOf: []string{"EXAMPLE_PROJECT", "SCION_EXAMPLE_PROJECT_ID"}}}},
		"api-key":  failfastAuthMeta().Types["api-key"],
	}}
	home := stageCandidates(t, map[string]interface{}{"explicit_type": "metadata"})
	if err := CheckStagedAuth("example", meta, nil, AuthCheckInputs{AgentHome: home}); err != nil {
		t.Errorf("explicit type with a SCION_* key, nothing staged: %v, want nil", err)
	}
	home = stageCandidates(t, nil)
	if err := CheckStagedAuth("example", meta, nil, AuthCheckInputs{AgentHome: home}); err != nil {
		t.Errorf("no explicit type, no no_auth, a type with a SCION_* key: %v, want nil", err)
	}
	home = stageCandidates(t, map[string]interface{}{"explicit_type": "api-key"})
	if err := CheckStagedAuth("example", meta, nil, AuthCheckInputs{AgentHome: home}); !errors.Is(err, ErrNoAuthSatisfied) {
		t.Errorf("explicit type without a SCION_* key, nothing staged: %v, want ErrNoAuthSatisfied", err)
	}
}
