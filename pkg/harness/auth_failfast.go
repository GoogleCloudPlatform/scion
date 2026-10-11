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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// ErrNoAuthSatisfied is wrapped by the error CheckStagedAuth returns when
// the staged auth candidates cannot satisfy the harness.
var ErrNoAuthSatisfied = errors.New("no auth method satisfied")

// stagedAuthCandidates is the subset of auth-candidates.json (written by
// ApplyAuthSettings) that the check reads.
type stagedAuthCandidates struct {
	ExplicitType   string            `json:"explicit_type"`
	EnvVars        []string          `json:"env_vars"`
	EnvSecretFiles map[string]string `json:"env_secret_files"`
}

// AuthCheckInputs is what CheckStagedAuth reads.
type AuthCheckInputs struct {
	// AgentHome is the broker-side agent home holding the staged
	// auth-candidates.json.
	AgentHome string
	// Env is the start env. The provisioner does not read it (see
	// CheckStagedAuth); counting it only makes the check more permissive.
	Env map[string]string
}

// CheckStagedAuth reports, before a container is started, a start that the
// container-side provisioner is certain to reject because no auth method is
// satisfied. It reads the auth-candidates.json staged in the agent home, the
// same file the provisioner reads.
//
// The provisioner's rule lives in Python (harnesses/scion_harness.py,
// ProvisionContext.select_auth, with each harness's AuthSpec in its
// provision.py), so the two cannot share code. The check is one-sided: it
// only rejects starts the provisioner also rejects, and
// harnesses/auth_failfast_pin_test.go pins that against the real Python
// selection for every shipped harness.
//
// It returns an error only when the harness declares auth types and either:
//
//  1. an explicit auth type is staged, the harness-config declares it, and
//     it is not satisfied (the provisioner never falls back to no-auth for
//     an explicit type); or
//  2. no explicit type is staged, the harness has no no_auth behaviour, and
//     no declared type is satisfied. All shipped harnesses declare no_auth,
//     so this branch applies only to custom harness-configs.
//
// Whether a type is satisfied is decided by stagedSatisfiesType.
//
// The check trusts the harness-config auth.types as the provisioner's
// contract. The broker rejects a start when an env-only auth type declared
// there is unsatisfied, so a harness-config must declare every method its
// provisioner accepts (a method its provision.py accepts but the config does
// not declare, or declares as env-only while the provisioner also accepts a
// file, can be rejected here). TestCheckStagedAuthCoversProvisionerSpec
// enforces this for the shipped harnesses only.
//
// The error names the harness, the auth types it accepts and the credential
// names it looks for. It never includes values.
func CheckStagedAuth(harnessName string, authMeta *config.HarnessAuthMetadata, noAuth *config.HarnessNoAuthConfig, in AuthCheckInputs) error {
	if authMeta == nil || len(authMeta.Types) == 0 {
		return nil
	}
	staged, err := readStagedAuthCandidates(in.AgentHome)
	if err != nil {
		// A missing or unreadable candidates file is not this check's
		// concern (ApplyAuthSettings writes it just before the check); the
		// provisioner reports it with its own error.
		return nil
	}

	explicit := strings.TrimSpace(staged.ExplicitType)
	if explicit != "" {
		t, ok := authMeta.Types[explicit]
		if !ok || stagedSatisfiesType(t, staged, in.Env) {
			return nil
		}
		return fmt.Errorf("%w: %s harness: auth type %q is selected but none of its credentials are available (%s); accepted auth types: %s",
			ErrNoAuthSatisfied, harnessName, explicit, describeAuthType(t), strings.Join(sortedTypeNames(authMeta), ", "))
	}

	if noAuth != nil && strings.TrimSpace(noAuth.Behavior) != "" {
		return nil
	}
	for _, t := range authMeta.Types {
		if stagedSatisfiesType(t, staged, in.Env) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s harness: no auth method is satisfied and the harness does not allow starting without credentials; provide credentials for one of the accepted auth types: %s",
		ErrNoAuthSatisfied, harnessName, strings.Join(sortedTypeNames(authMeta), ", "))
}

// CheckStagedAuth applies the package-level CheckStagedAuth to this
// harness-config's auth metadata and no_auth behaviour.
func (c *ContainerScriptHarness) CheckStagedAuth(in AuthCheckInputs) error {
	return CheckStagedAuth(c.entry.Harness, c.entry.Auth, c.entry.NoAuthConfig, in)
}

// stagedSatisfiesType is the satisfaction rule of the broker auth check.
//
//   - A type that declares required_files is always satisfied. The broker
//     never rejects it: a credential file can reach the container by routes
//     the broker cannot see (volumes, file secrets, a persistent or image
//     home, symlinks into shared directories, pre-start hooks that run
//     before the provisioner).
//   - A type with no requirements is satisfied.
//   - An env-only type is satisfied when any key of any of its required_env
//     groups is staged: listed in env_vars or env_secret_files of
//     auth-candidates.json, or set in the start env. The provisioner selects
//     env methods from the staged candidates and secret files, plus, for
//     env_fallback methods, its own environment. That environment is
//     minimal (HOME, PATH, LANG, TZ and every SCION_* variable of the
//     container), so the only container env it can see is SCION_*, which
//     the broker cannot fully see either; a type with any SCION_* key is
//     therefore always satisfied. "Any key" and the start env only make the
//     check more permissive than the provisioner.
func stagedSatisfiesType(t api.HarnessAuthTypeMetadata, staged stagedAuthCandidates, env map[string]string) bool {
	if len(t.RequiredFiles) > 0 || len(t.RequiredEnv) == 0 {
		return true
	}
	for _, group := range t.RequiredEnv {
		for _, k := range group.AnyOf {
			if k == "" {
				continue
			}
			if strings.HasPrefix(k, "SCION_") {
				return true
			}
			if _, ok := staged.EnvSecretFiles[k]; ok {
				return true
			}
			for _, name := range staged.EnvVars {
				if name == k {
					return true
				}
			}
			if env[k] != "" {
				return true
			}
		}
	}
	return false
}

func readStagedAuthCandidates(agentHome string) (stagedAuthCandidates, error) {
	var staged stagedAuthCandidates
	data, err := os.ReadFile(filepath.Join(agentHome, ".scion", "harness", "inputs", "auth-candidates.json"))
	if err != nil {
		return staged, err
	}
	if err := json.Unmarshal(data, &staged); err != nil {
		return staged, err
	}
	return staged, nil
}

// describeAuthType lists the env keys an env-only auth type t accepts.
func describeAuthType(t api.HarnessAuthTypeMetadata) string {
	var parts []string
	for _, group := range t.RequiredEnv {
		if len(group.AnyOf) > 0 {
			parts = append(parts, "env "+strings.Join(group.AnyOf, " or "))
		}
	}
	return "expects " + strings.Join(parts, ", ")
}

func sortedTypeNames(authMeta *config.HarnessAuthMetadata) []string {
	names := make([]string, 0, len(authMeta.Types))
	for name := range authMeta.Types {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
