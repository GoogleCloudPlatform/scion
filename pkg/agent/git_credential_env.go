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

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Least-privilege container env: GitHub credential keys.
//
// A hub-dispatched agent (opts.BrokerMode) receives GitHub credential env
// keys only when the hub says its template allows them
// (api.StartOptions.AllowGitCredentials, carried in the dispatch request).
// The value is read from the dispatch request only, never from broker-local
// config. Absent or false means the keys are removed. Local (non-broker)
// starts are unchanged. The keys that turn on the in-container GitHub App
// credential path (gitCredentialHelperEnvKeys) are removed with them; the
// hub refuses that path's token refresh for such an agent too.
//
// One decision, gitCredentialsStripped, is made per start. Keys are removed
// in one place, the final runtime.RunConfig (stripGitCredentialRunConfig),
// from Env, ResolvedAuth.EnvVars, and the environment-type and variable-type
// ResolvedSecrets whose env name matches. File-type secrets are not covered:
// they are projected to a path, which carries no env name to match. Two
// earlier steps keep values from being read or written where that final
// filter cannot see them:
//   - the config layer: an empty-value passthrough marker or a ${VAR}
//     reference never reads a matching name from the broker host env
//     (buildAgentEnvWithPolicy);
//   - auth gathering: matching keys never enter the auth pipeline, so they
//     are neither auto-detected nor staged as files in the agent home, and
//     files an earlier start staged are deleted
//     (stripGitCredentialAuthEnv, removeStagedGitCredentialFiles).

// gitCredentialEnvExact lists the GitHub credential keys matched by name.
var gitCredentialEnvExact = map[string]struct{}{
	"GITHUB_TOKEN": {},
}

// gitCredentialEnvPrefix matches GH_TOKEN, GH_ENTERPRISE_TOKEN and the
// GH_<OWNER> / GH_<OWNER>__<REPO> per-owner credential convention.
const gitCredentialEnvPrefix = "GH_"

// IsGitCredentialEnvKey reports whether key names a GitHub credential env
// var: GITHUB_TOKEN, or any key starting with GH_. Matching ignores ASCII
// case. Every GH_ key matches, including gh CLI settings such as GH_HOST or
// GH_REPO, because an owner credential can use any owner name and so cannot
// be told apart from a setting by name alone.
func IsGitCredentialEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	if _, ok := gitCredentialEnvExact[upper]; ok {
		return true
	}
	return strings.HasPrefix(upper, gitCredentialEnvPrefix)
}

// gitCredentialHelperEnvKeys are not credentials themselves, but they turn
// on the in-container GitHub App credential path, which fetches a token on
// demand. They are removed together with the credential keys so that path
// stays off for an agent without the allowance.
var gitCredentialHelperEnvKeys = map[string]struct{}{
	"SCION_GITHUB_APP_ENABLED":  {},
	"SCION_GITHUB_TOKEN_PATH":   {},
	"SCION_GITHUB_TOKEN_EXPIRY": {},
}

// isStrippedGitEnvKey reports whether key is removed from the container env
// of an agent without the allowance: a credential key or a helper key. Both
// match ignoring ASCII case, like IsGitCredentialEnvKey.
func isStrippedGitEnvKey(key string) bool {
	if IsGitCredentialEnvKey(key) {
		return true
	}
	_, ok := gitCredentialHelperEnvKeys[strings.ToUpper(key)]
	return ok
}

// gitCredentialsStripped reports whether GitHub credential keys must be
// removed from the container env for this start: a hub-dispatched start
// whose dispatch request did not allow them.
func gitCredentialsStripped(opts api.StartOptions) bool {
	return opts.BrokerMode && !opts.AllowGitCredentials
}

// stripGitCredentialAuthEnv removes matching keys from the gathered auth env.
func stripGitCredentialAuthEnv(auth *api.AuthConfig) {
	if auth == nil {
		return
	}
	for k := range auth.EnvVars {
		if IsGitCredentialEnvKey(k) {
			delete(auth.EnvVars, k)
		}
	}
}

// removeStagedGitCredentialFiles deletes auth secret files with a matching
// name that an earlier start staged in the agent home, so they are not
// carried into the container again.
func removeStagedGitCredentialFiles(agentHome string) {
	if agentHome == "" {
		return
	}
	dir := filepath.Join(agentHome, ".scion", "harness", "secrets")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !IsGitCredentialEnvKey(e.Name()) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// stripGitCredentialRunConfig removes every matching key, and every
// gitCredentialHelperEnvKeys key, from cfg's Env and ResolvedAuth.EnvVars,
// and every environment-type or variable-type ResolvedSecret whose env name
// (secretEnvTarget) matches. File-type secrets are left alone. Maps and
// slices are copied, never edited in place, and the removed key names are
// returned.
func stripGitCredentialRunConfig(cfg *runtime.RunConfig) (removed []string) {
	if cfg == nil {
		return nil
	}
	seen := map[string]struct{}{}
	note := func(k string) {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			removed = append(removed, k)
		}
	}

	env := make([]string, 0, len(cfg.Env))
	for _, kv := range cfg.Env {
		k, _, _ := strings.Cut(kv, "=")
		if isStrippedGitEnvKey(k) {
			note(k)
			continue
		}
		env = append(env, kv)
	}
	cfg.Env = env

	if cfg.ResolvedAuth != nil && len(cfg.ResolvedAuth.EnvVars) > 0 {
		ra := *cfg.ResolvedAuth
		ra.EnvVars = make(map[string]string, len(cfg.ResolvedAuth.EnvVars))
		for k, v := range cfg.ResolvedAuth.EnvVars {
			if isStrippedGitEnvKey(k) {
				note(k)
				continue
			}
			ra.EnvVars[k] = v
		}
		cfg.ResolvedAuth = &ra
	}

	if len(cfg.ResolvedSecrets) > 0 {
		secrets := make([]api.ResolvedSecret, 0, len(cfg.ResolvedSecrets))
		for _, s := range cfg.ResolvedSecrets {
			if (s.Type == "environment" || s.Type == "variable" || s.Type == "") && isStrippedGitEnvKey(secretEnvTarget(s)) {
				note(secretEnvTarget(s))
				continue
			}
			secrets = append(secrets, s)
		}
		cfg.ResolvedSecrets = secrets
	}
	sort.Strings(removed)
	return removed
}

// expandEnvWithoutGitCredentials is util.ExpandEnv except that a reference
// to a matching name never reads the host env: it expands to "" and is
// reported like an unset variable (warned is true), so the caller skips the
// entry instead of falling back to the empty-value passthrough.
func expandEnvWithoutGitCredentials(s string) (string, bool) {
	warned := false
	expanded := os.Expand(s, func(key string) string {
		if IsGitCredentialEnvKey(key) {
			warned = true
			return ""
		}
		val, ok := os.LookupEnv(key)
		if !ok {
			fmt.Fprintf(os.Stderr, "Warning: environment variable %q is not set\n", key)
			warned = true
			return ""
		}
		return val
	})
	return expanded, warned
}
