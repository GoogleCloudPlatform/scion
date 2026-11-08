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

package runtime

import (
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// homeTransferParityConfigs covers the staged-file classification: file
// secrets inside and outside the home (tilde and absolute targets),
// variable secrets, auth files with and without a source, and external
// references (the GKE CSI path).
func homeTransferParityConfigs() map[string]RunConfig {
	base := func() RunConfig {
		return RunConfig{UnixUsername: "scion", HomeStorageBackend: HomeStorageNFS}
	}
	cfgs := map[string]RunConfig{"none": base()}

	c := base()
	c.ResolvedSecrets = []api.ResolvedSecret{
		{Name: "a", Type: "file", Target: "~/.config/a.json", Value: "x"},
		{Name: "b", Type: "file", Target: "/home/scion/.b", Value: "x"},
		{Name: "outside", Type: "file", Target: "/etc/ssl/key.pem", Value: "x"},
		{Name: "v", Type: "variable", Target: "V", Value: "x"},
		{Name: "e", Type: "environment", Target: "E", Value: "x"},
	}
	cfgs["file, variable and environment secrets"] = c

	c = base()
	c.ResolvedSecrets = []api.ResolvedSecret{
		{Name: "ext", Type: "file", Target: "~/.ext.json", Value: "x", Ref: "projects/p/secrets/s"},
		{Name: "v", Type: "variable", Target: "V", Value: "x"},
	}
	cfgs["external reference"] = c

	c = base()
	c.ResolvedAuth = &api.ResolvedAuth{Files: []api.FileMapping{
		{SourcePath: "/src/creds", ContainerPath: "~/.claude/.credentials.json"},
		{SourcePath: "", ContainerPath: "~/.skipped"},
		{SourcePath: "/src/adc", ContainerPath: "/var/run/adc.json"},
	}}
	cfgs["auth files"] = c

	c = base()
	c.ResolvedSecrets = []api.ResolvedSecret{{Name: "dup", Type: "file", Target: "~/.d", Value: "x"}}
	c.ResolvedAuth = &api.ResolvedAuth{Files: []api.FileMapping{{SourcePath: "/s", ContainerPath: "~/.e"}}}
	cfgs["secrets and auth"] = c
	return cfgs
}

// The Kubernetes NFS-home transfer and the Cloud Run home copy leave out
// the same classification (one homeTransferExcludes): Kubernetes leaves out
// exactly its link targets plus the harness bundle's secrets and outputs,
// and Cloud Run leaves out the same set (secrets.json delivered) plus the
// hub credential file, its staging file and secrets.json.
func TestHomeTransferExcludes_ParityWithKubernetes(t *testing.T) {
	harness := []string{harnessBundleSecretsRel, harnessBundleOutputsRel}
	for name, cfg := range homeTransferParityConfigs() {
		for _, gke := range []bool{false, true} {
			r := &KubernetesRuntime{GKEMode: gke}
			k8s := r.nfsHomeSyncExcludes(cfg)

			want := append(slices.Clone(r.k8sHomeLinkTargets(cfg)), harness...)
			if !slices.Equal(k8s, want) {
				t.Errorf("%s (gke=%v): kubernetes excludes %v, want link targets plus harness dirs %v", name, gke, k8s, want)
			}
			if shared := homeTransferExcludes(cfg, !r.useGKESecretsPath(cfg)); !slices.Equal(k8s, shared) {
				t.Errorf("%s (gke=%v): kubernetes excludes %v differ from homeTransferExcludes %v", name, gke, k8s, shared)
			}
		}

		cr := cloudRunHomeExcludes(cfg)
		want := append(homeTransferExcludes(cfg, true), cloudRunHubTokenRel, cloudRunHubTokenTmpHomeRel, cloudRunSecretsJSONHomeRel)
		if !slices.Equal(cr, want) {
			t.Errorf("%s: cloudrun excludes %v, want %v", name, cr, want)
		}
		// Everything Kubernetes leaves out (without the CSI path) Cloud Run
		// leaves out too.
		for _, ex := range (&KubernetesRuntime{}).nfsHomeSyncExcludes(cfg) {
			if !homeRelExcluded(ex, cr) {
				t.Errorf("%s: kubernetes leaves out %s but cloudrun copies it", name, ex)
			}
		}
	}
}

// The classification itself: in-home targets only, tilde expanded,
// secrets.json only with a variable secret and withVariables, auth files
// only with a source.
func TestHomeTransferExcludes_Classification(t *testing.T) {
	cfgs := homeTransferParityConfigs()
	got := homeTransferExcludes(cfgs["file, variable and environment secrets"], true)
	want := []string{".config/a.json", ".b", ".scion/secrets.json", harnessBundleSecretsRel, harnessBundleOutputsRel}
	if !slices.Equal(got, want) {
		t.Errorf("excludes = %v, want %v", got, want)
	}
	got = homeTransferExcludes(cfgs["file, variable and environment secrets"], false)
	want = []string{".config/a.json", ".b", harnessBundleSecretsRel, harnessBundleOutputsRel}
	if !slices.Equal(got, want) {
		t.Errorf("excludes without variables = %v, want %v", got, want)
	}
	got = homeTransferExcludes(cfgs["auth files"], true)
	want = []string{".claude/.credentials.json", harnessBundleSecretsRel, harnessBundleOutputsRel}
	if !slices.Equal(got, want) {
		t.Errorf("auth excludes = %v, want %v", got, want)
	}
}

func TestHomeRelExcluded(t *testing.T) {
	ex := []string{".scion/harness/secrets", ".b"}
	for rel, want := range map[string]bool{
		".scion/harness/secrets":       true,
		".scion/harness/secrets/KEY":   true,
		".scion/harness/secretsX":      false,
		".scion/harness":               false,
		".b":                           true,
		".bb":                          false,
		".b/c":                         true,
		".scion/harness/outputs/x.env": false,
	} {
		if got := homeRelExcluded(rel, ex); got != want {
			t.Errorf("homeRelExcluded(%q) = %v, want %v", rel, got, want)
		}
	}
}

// Temporary leftovers of the broker's guarded writers (agenthome and dirfd
// forms) are left out of the Cloud Run copy wherever they are; ordinary
// names that merely contain ".tmp" are copied.
func TestCloudRunHomeCopyExcluded_TempNames(t *testing.T) {
	ex := cloudRunHomeExcludes(RunConfig{UnixUsername: "scion"})
	for rel, want := range map[string]bool{
		".scion/.scion-token.tmp.tmp-0123456789abcdef": true,
		".scion/.agent-info.json.tmp-123-1730000000":   true,
		".x.tmp-fedcba9876543210":                      true,
		".scion/scion-token.tmp":                       true,
		".scion/scion-token":                           true,
		"notes.tmp":                                    false,
		".x.tmp-0123":                                  false,
		".x.tmp-0123456789abcdeG":                      false,
		"x.tmp-0123456789abcdef":                       false,
		".bashrc":                                      false,
	} {
		if got := cloudRunHomeCopyExcluded(rel, ex); got != want {
			t.Errorf("cloudRunHomeCopyExcluded(%q) = %v, want %v", rel, got, want)
		}
	}
}
