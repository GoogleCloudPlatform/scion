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

package hub

import (
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestMergeEnvironmentSecretMetadata(t *testing.T) {
	envVars := []store.EnvVar{
		{ID: "plain-shadowed", Key: "SHARED", Value: "stale"},
		{ID: "plain-kept", Key: "PLAIN", Value: "visible"},
		{ID: "secret-kept", Key: "SHARED", Value: "********", Secret: true},
	}
	metas := []secret.SecretMeta{
		{ID: "secret-new", Name: "SHARED", Scope: store.ScopeProject, ScopeID: "project-1"},
		{ID: "secret-other", Name: "SECRET", Scope: store.ScopeProject, ScopeID: "project-1"},
	}

	got := mergeEnvironmentSecretMetadata(envVars, metas)
	want := []store.EnvVar{
		envVars[1],
		envVars[2],
		secretMetaToEnvVar(metas[0]),
		secretMetaToEnvVar(metas[1]),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeEnvironmentSecretMetadata() = %#v, want %#v", got, want)
	}
}

func TestMergeEnvironmentSecretMetadataWithoutSecrets(t *testing.T) {
	envVars := []store.EnvVar{{ID: "plain", Key: "PLAIN", Value: "visible"}}

	got := mergeEnvironmentSecretMetadata(envVars, nil)
	if !reflect.DeepEqual(got, envVars) {
		t.Fatalf("mergeEnvironmentSecretMetadata() = %#v, want %#v", got, envVars)
	}
}
