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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestResolveKubernetesServiceAccountMapping covers the precedence for GCP
// identity mode "assign" on Kubernetes (ptone/scion#2328): a
// profile-level entry for a given GSA wins over the runtime-level entry for
// the same GSA, mirroring ResolveImageRegistry's profile-overrides-runtime
// precedent but evaluated per map key.
func TestResolveKubernetesServiceAccountMapping(t *testing.T) {
	tests := []struct {
		name        string
		settings    *VersionedSettings
		profileName string
		gsaEmail    string
		wantKSA     string
		wantOK      bool
	}{
		{
			name: "runtime-level mapping",
			settings: &VersionedSettings{
				ActiveProfile: "prod",
				Profiles: map[string]V1ProfileConfig{
					"prod": {Runtime: "gke"},
				},
				Runtimes: map[string]V1RuntimeConfig{
					"gke": {
						Type: "kubernetes",
						KubernetesServiceAccountMappings: map[string]string{
							"agent-worker@my-project.iam.gserviceaccount.com": "agent-worker-ksa",
						},
					},
				},
			},
			profileName: "prod",
			gsaEmail:    "agent-worker@my-project.iam.gserviceaccount.com",
			wantKSA:     "agent-worker-ksa",
			wantOK:      true,
		},
		{
			name: "profile-level entry overrides runtime-level entry for the same GSA",
			settings: &VersionedSettings{
				ActiveProfile: "prod",
				Profiles: map[string]V1ProfileConfig{
					"prod": {
						Runtime: "gke",
						KubernetesServiceAccountMappings: map[string]string{
							"agent-worker@my-project.iam.gserviceaccount.com": "profile-ksa",
						},
					},
				},
				Runtimes: map[string]V1RuntimeConfig{
					"gke": {
						Type: "kubernetes",
						KubernetesServiceAccountMappings: map[string]string{
							"agent-worker@my-project.iam.gserviceaccount.com": "runtime-ksa",
						},
					},
				},
			},
			profileName: "prod",
			gsaEmail:    "agent-worker@my-project.iam.gserviceaccount.com",
			wantKSA:     "profile-ksa",
			wantOK:      true,
		},
		{
			name: "profile mapping for a different GSA does not shadow the runtime mapping",
			settings: &VersionedSettings{
				ActiveProfile: "prod",
				Profiles: map[string]V1ProfileConfig{
					"prod": {
						Runtime: "gke",
						KubernetesServiceAccountMappings: map[string]string{
							"other@my-project.iam.gserviceaccount.com": "other-ksa",
						},
					},
				},
				Runtimes: map[string]V1RuntimeConfig{
					"gke": {
						Type: "kubernetes",
						KubernetesServiceAccountMappings: map[string]string{
							"agent-worker@my-project.iam.gserviceaccount.com": "runtime-ksa",
						},
					},
				},
			},
			profileName: "prod",
			gsaEmail:    "agent-worker@my-project.iam.gserviceaccount.com",
			wantKSA:     "runtime-ksa",
			wantOK:      true,
		},
		{
			name: "empty profile name uses active profile",
			settings: &VersionedSettings{
				ActiveProfile: "prod",
				Profiles: map[string]V1ProfileConfig{
					"prod": {Runtime: "gke"},
				},
				Runtimes: map[string]V1RuntimeConfig{
					"gke": {
						Type: "kubernetes",
						KubernetesServiceAccountMappings: map[string]string{
							"agent-worker@my-project.iam.gserviceaccount.com": "agent-worker-ksa",
						},
					},
				},
			},
			profileName: "",
			gsaEmail:    "agent-worker@my-project.iam.gserviceaccount.com",
			wantKSA:     "agent-worker-ksa",
			wantOK:      true,
		},
		{
			name: "no mapping for the requested GSA",
			settings: &VersionedSettings{
				ActiveProfile: "prod",
				Profiles: map[string]V1ProfileConfig{
					"prod": {Runtime: "gke"},
				},
				Runtimes: map[string]V1RuntimeConfig{
					"gke": {Type: "kubernetes"},
				},
			},
			profileName: "prod",
			gsaEmail:    "unmapped@my-project.iam.gserviceaccount.com",
			wantKSA:     "",
			wantOK:      false,
		},
		{
			name:        "unknown profile",
			settings:    &VersionedSettings{ActiveProfile: "prod"},
			profileName: "does-not-exist",
			gsaEmail:    "agent-worker@my-project.iam.gserviceaccount.com",
			wantKSA:     "",
			wantOK:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKSA, gotOK := tt.settings.ResolveKubernetesServiceAccountMapping(tt.profileName, tt.gsaEmail)
			assert.Equal(t, tt.wantKSA, gotKSA)
			assert.Equal(t, tt.wantOK, gotOK)
		})
	}
}

// TestValidateKubernetesServiceAccountMappings covers the three GSA email
// shapes GCP actually issues (user-managed, App Engine default, Compute
// Engine default) plus rejection of malformed emails and KSA names.
func TestValidateKubernetesServiceAccountMappings(t *testing.T) {
	tests := []struct {
		name     string
		mappings map[string]string
		wantErr  bool
	}{
		{
			name:     "user-managed GSA (iam.gserviceaccount.com)",
			mappings: map[string]string{"agent-worker@my-project.iam.gserviceaccount.com": "agent-worker-ksa"},
			wantErr:  false,
		},
		{
			name:     "App Engine default GSA (appspot.gserviceaccount.com)",
			mappings: map[string]string{"my-project@appspot.gserviceaccount.com": "appengine-ksa"},
			wantErr:  false,
		},
		{
			name:     "Compute Engine default GSA (developer.gserviceaccount.com)",
			mappings: map[string]string{"123456789012-compute@developer.gserviceaccount.com": "compute-default-ksa"},
			wantErr:  false,
		},
		{
			name:     "malformed GSA email",
			mappings: map[string]string{"not-an-email": "agent-worker-ksa"},
			wantErr:  true,
		},
		{
			name:     "GSA email not ending in gserviceaccount.com",
			mappings: map[string]string{"agent-worker@my-project.iam.example.com": "agent-worker-ksa"},
			wantErr:  true,
		},
		{
			name:     "KSA name with uppercase characters",
			mappings: map[string]string{"agent-worker@my-project.iam.gserviceaccount.com": "Agent-Worker-KSA"},
			wantErr:  true,
		},
		{
			name:     "KSA name with underscore",
			mappings: map[string]string{"agent-worker@my-project.iam.gserviceaccount.com": "agent_worker_ksa"},
			wantErr:  true,
		},
		{
			name:     "empty KSA name",
			mappings: map[string]string{"agent-worker@my-project.iam.gserviceaccount.com": ""},
			wantErr:  true,
		},
		{
			name:     "empty mapping set",
			mappings: map[string]string{},
			wantErr:  false,
		},
		{
			name:     "KSA name with a dot (DNS-1123 subdomain, not just a label)",
			mappings: map[string]string{"agent-worker@my-project.iam.gserviceaccount.com": "team.agent-worker"},
			wantErr:  false,
		},
		{
			name:     "uppercase GSA email is rejected, not silently lowercased",
			mappings: map[string]string{"Agent-Worker@my-project.iam.gserviceaccount.com": "agent-worker-ksa"},
			wantErr:  true,
		},
		{
			name:     "GSA email with an empty domain label",
			mappings: map[string]string{"agent-worker@.gserviceaccount.com": "agent-worker-ksa"},
			wantErr:  true,
		},
		{
			name:     "GSA email with a domain label starting with a hyphen",
			mappings: map[string]string{"agent-worker@-project.iam.gserviceaccount.com": "agent-worker-ksa"},
			wantErr:  true,
		},
		{
			name:     "GSA email with a disallowed local-part character",
			mappings: map[string]string{"agent!worker@my-project.iam.gserviceaccount.com": "agent-worker-ksa"},
			wantErr:  true,
		},
		{
			name:     "empty GSA email",
			mappings: map[string]string{"": "agent-worker-ksa"},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateKubernetesServiceAccountMappings(tt.mappings)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestResolveKubernetesServiceAccountMapping_CaseInsensitiveLookup proves the
// lookup lower-cases the requested GSA email before matching, so a caller
// that received the email in mixed case (e.g. from an upstream API) still
// resolves the mapping against a validated, lowercase settings key.
func TestResolveKubernetesServiceAccountMapping_CaseInsensitiveLookup(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "prod",
		Profiles: map[string]V1ProfileConfig{
			"prod": {Runtime: "gke"},
		},
		Runtimes: map[string]V1RuntimeConfig{
			"gke": {
				Type: "kubernetes",
				KubernetesServiceAccountMappings: map[string]string{
					"agent-worker@my-project.iam.gserviceaccount.com": "agent-worker-ksa",
				},
			},
		},
	}
	ksa, ok := vs.ResolveKubernetesServiceAccountMapping("prod", "Agent-Worker@My-Project.IAM.GServiceAccount.com")
	assert.True(t, ok)
	assert.Equal(t, "agent-worker-ksa", ksa)
}

// TestResolveKubernetesServiceAccountMapping_ProfileEmptyValueFallsThrough
// proves that an explicit but empty-string profile-level entry for a GSA
// does not shadow the runtime-level entry for the same GSA — it is treated
// as unset, per the resolver's documented fall-through behavior.
func TestResolveKubernetesServiceAccountMapping_ProfileEmptyValueFallsThrough(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "prod",
		Profiles: map[string]V1ProfileConfig{
			"prod": {
				Runtime: "gke",
				KubernetesServiceAccountMappings: map[string]string{
					"agent-worker@my-project.iam.gserviceaccount.com": "",
				},
			},
		},
		Runtimes: map[string]V1RuntimeConfig{
			"gke": {
				Type: "kubernetes",
				KubernetesServiceAccountMappings: map[string]string{
					"agent-worker@my-project.iam.gserviceaccount.com": "runtime-ksa",
				},
			},
		},
	}
	ksa, ok := vs.ResolveKubernetesServiceAccountMapping("prod", "agent-worker@my-project.iam.gserviceaccount.com")
	assert.True(t, ok)
	assert.Equal(t, "runtime-ksa", ksa)
}

// TestResolveKubernetesNamespace covers the namespace lookup used by GCP
// identity mode "assign": only the selected runtime entry's namespace is
// consulted, and an entry without one (or no entry) reports false so the
// caller falls back to the runtime's default.
func TestResolveKubernetesNamespace(t *testing.T) {
	vs := &VersionedSettings{
		Profiles: map[string]V1ProfileConfig{
			"prod": {Runtime: "gke"},
		},
		Runtimes: map[string]V1RuntimeConfig{
			"gke":      {Namespace: "scion-agents"},
			"gke-team": {Namespace: "team-agents"},
			"gke-bare": {},
		},
	}
	tests := []struct {
		name          string
		runtimeEntry  string
		wantNamespace string
		wantOK        bool
	}{
		{name: "runtime entry namespace", runtimeEntry: "gke", wantNamespace: "scion-agents", wantOK: true},
		{name: "second runtime entry namespace", runtimeEntry: "gke-team", wantNamespace: "team-agents", wantOK: true},
		{name: "runtime entry without namespace", runtimeEntry: "gke-bare"},
		{name: "unknown runtime entry", runtimeEntry: "missing"},
		{name: "no runtime entry", runtimeEntry: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotNamespace, gotOK := vs.ResolveKubernetesNamespace(tt.runtimeEntry)
			assert.Equal(t, tt.wantNamespace, gotNamespace)
			assert.Equal(t, tt.wantOK, gotOK)
		})
	}
}

// TestProjectSettingsHasKubernetesServiceAccountMappings covers the
// project-root-vs-.scion-dir resolution this function must get right (via
// GetResolvedProjectDir) and confirms it reads the project's settings.yaml
// directly rather than through LoadEffectiveSettings/LoadVersionedSettings —
// which merge the global settings in underneath the project's own, making a
// key the project never set indistinguishable from one it did (see the
// function's own doc comment). Each subtest uses a fresh HOME and project
// dir so a mapping written to one is never visible by accident from the
// other.
func TestProjectSettingsHasKubernetesServiceAccountMappings(t *testing.T) {
	writeProjectSettings := func(t *testing.T, yaml string) string {
		t.Helper()
		projectDir := t.TempDir()
		dotScion := filepath.Join(projectDir, ".scion")
		if err := os.MkdirAll(dotScion, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(yaml), 0644); err != nil {
			t.Fatal(err)
		}
		return projectDir
	}
	writeGlobalSettingsWithMapping := func(t *testing.T) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		globalDir := filepath.Join(home, ".scion")
		if err := os.MkdirAll(globalDir, 0755); err != nil {
			t.Fatal(err)
		}
		yaml := `schema_version: "1"
runtimes:
    kubernetes:
        type: kubernetes
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: global-ksa
`
		if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(yaml), 0644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("project runtime entry sets the mapping", func(t *testing.T) {
		writeGlobalSettingsWithMapping(t)
		projectDir := writeProjectSettings(t, `schema_version: "1"
runtimes:
    kubernetes:
        type: kubernetes
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: project-ksa
`)
		if !ProjectSettingsHasKubernetesServiceAccountMappings(projectDir, "kubernetes", "") {
			t.Error("expected true: the project's own settings.yaml sets this runtime entry's mapping")
		}
	})

	t.Run("project profile sets the mapping", func(t *testing.T) {
		writeGlobalSettingsWithMapping(t)
		projectDir := writeProjectSettings(t, `schema_version: "1"
profiles:
    prod:
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: project-ksa
`)
		if !ProjectSettingsHasKubernetesServiceAccountMappings(projectDir, "", "prod") {
			t.Error("expected true: the project's own settings.yaml sets this profile's mapping")
		}
	})

	// The regression case this function exists for: a project settings.yaml
	// that never mentions kubernetes_service_account_mappings at all must
	// read as false here, even though the broker's global settings (merged
	// in underneath by LoadEffectiveSettings/LoadVersionedSettings for every
	// other caller) do have one for the same runtime entry name. A naive
	// LoadEffectiveSettings(projectDir)-based check cannot tell these two
	// cases apart.
	t.Run("project settings silent, global has the mapping", func(t *testing.T) {
		writeGlobalSettingsWithMapping(t)
		projectDir := writeProjectSettings(t, `schema_version: "1"
runtimes:
    kubernetes:
        type: kubernetes
`)
		if ProjectSettingsHasKubernetesServiceAccountMappings(projectDir, "kubernetes", "") {
			t.Error("expected false: the project's own settings.yaml never mentions this mapping, even though the merged/global view would show one")
		}
	})

	t.Run("no project settings file at all", func(t *testing.T) {
		writeGlobalSettingsWithMapping(t)
		emptyDir := t.TempDir()
		if ProjectSettingsHasKubernetesServiceAccountMappings(emptyDir, "kubernetes", "") {
			t.Error("expected false when there is no project settings file")
		}
	})

	t.Run("project root path (not yet resolved to its .scion dir)", func(t *testing.T) {
		writeGlobalSettingsWithMapping(t)
		projectDir := writeProjectSettings(t, `schema_version: "1"
runtimes:
    kubernetes:
        type: kubernetes
        kubernetes_service_account_mappings:
            agent-worker@my-project.iam.gserviceaccount.com: project-ksa
`)
		// projectDir itself is the project root, not projectDir/.scion —
		// GetResolvedProjectDir must resolve it before the file is read.
		if !ProjectSettingsHasKubernetesServiceAccountMappings(projectDir, "kubernetes", "") {
			t.Error("expected true: a project-root path must resolve to its .scion settings file")
		}
	})
}

// TestResolveKubernetesBlockServiceAccountForSelection covers the block
// ServiceAccount precedence (ptone/scion#4034): profile over runtime entry,
// an empty value treated as unset, and the Kubernetes profiles reported as
// having none.
func TestResolveKubernetesBlockServiceAccountForSelection(t *testing.T) {
	vs := &VersionedSettings{
		Profiles: map[string]V1ProfileConfig{
			"team":   {Runtime: "gke", KubernetesBlockServiceAccount: "team-block"},
			"shared": {Runtime: "gke"},
			"bare":   {Runtime: "k8s-bare"},
			"empty":  {Runtime: "k8s-bare", KubernetesBlockServiceAccount: ""},
			"local":  {Runtime: "docker"},
		},
		Runtimes: map[string]V1RuntimeConfig{
			"gke":      {Type: "kubernetes", KubernetesBlockServiceAccount: "entry-block"},
			"k8s-bare": {Type: "kubernetes"},
			"docker":   {Type: "docker"},
		},
	}
	cases := []struct {
		profile, entry string
		want           string
		wantOK         bool
	}{
		{"team", "gke", "team-block", true},
		{"shared", "gke", "entry-block", true},
		{"", "gke", "entry-block", true},
		{"bare", "k8s-bare", "", false},
		{"empty", "k8s-bare", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		got, ok := vs.ResolveKubernetesBlockServiceAccountForSelection(tc.profile, tc.entry)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("(%q, %q) = (%q, %v), want (%q, %v)", tc.profile, tc.entry, got, ok, tc.want, tc.wantOK)
		}
	}
	missing := vs.KubernetesProfilesWithoutBlockServiceAccount()
	if strings.Join(missing, ",") != "bare,empty" {
		t.Errorf("KubernetesProfilesWithoutBlockServiceAccount = %v, want [bare empty]", missing)
	}
	var nilVS *VersionedSettings
	if _, ok := nilVS.ResolveKubernetesBlockServiceAccountForSelection("team", "gke"); ok {
		t.Error("expected nil settings to resolve nothing")
	}
	if err := ValidateKubernetesBlockServiceAccount("scion-block"); err != nil {
		t.Errorf("valid name refused: %v", err)
	}
	if err := ValidateKubernetesBlockServiceAccount("Not_Valid"); err == nil {
		t.Error("expected an invalid name to be refused")
	}
}
