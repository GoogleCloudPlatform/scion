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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKubernetesServiceAccountMappingGSAs(t *testing.T) {
	vs := &VersionedSettings{
		Profiles: map[string]V1ProfileConfig{
			"gke": {Runtime: "k8s", KubernetesServiceAccountMappings: map[string]string{
				"p@x.iam.gserviceaccount.com":     "ksa-p",
				"blank@x.iam.gserviceaccount.com": "", // no entry, like the resolver
			}},
		},
		Runtimes: map[string]V1RuntimeConfig{
			"k8s": {KubernetesServiceAccountMappings: map[string]string{
				"r@x.iam.gserviceaccount.com":     "ksa-r",
				"p@x.iam.gserviceaccount.com":     "ksa-dup",
				"Upper@x.iam.gserviceaccount.com": "ksa-u", // never matched by the resolver
			}},
		},
	}

	got := vs.KubernetesServiceAccountMappingGSAs("gke", "k8s")
	assert.Equal(t, []string{"p@x.iam.gserviceaccount.com", "r@x.iam.gserviceaccount.com"}, got,
		"union of profile and runtime-entry keys, sorted, deduplicated")

	// Every reported GSA resolves through the dispatch-time resolver, and the
	// excluded ones do not.
	for _, gsa := range got {
		_, ok := vs.ResolveKubernetesServiceAccountMappingForSelection("gke", "k8s", gsa)
		assert.True(t, ok, gsa)
	}
	for _, gsa := range []string{"blank@x.iam.gserviceaccount.com"} {
		_, ok := vs.ResolveKubernetesServiceAccountMappingForSelection("gke", "k8s", gsa)
		assert.False(t, ok, gsa)
	}

	assert.Equal(t, []string{"p@x.iam.gserviceaccount.com"}, vs.KubernetesServiceAccountMappingGSAs("gke", ""),
		"profile level alone")
	assert.Equal(t, []string{"p@x.iam.gserviceaccount.com", "r@x.iam.gserviceaccount.com"},
		vs.KubernetesServiceAccountMappingGSAs("", "k8s"), "runtime entry alone (ForceRuntime shape)")
	assert.Nil(t, vs.KubernetesServiceAccountMappingGSAs("missing", "missing"))
	assert.Nil(t, (*VersionedSettings)(nil).KubernetesServiceAccountMappingGSAs("gke", "k8s"))
}
