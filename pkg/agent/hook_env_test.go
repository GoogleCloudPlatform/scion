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
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestComputeHookWithheldEnv_Provenance(t *testing.T) {
	bundleEnv := map[string]string{
		"BUNDLE_ONLY":    "from-bundle",
		"GITHUB_TOKEN":   "",
		"INLINE_WIN":     "from-bundle",
		"AUTH_WIN":       "from-bundle",
		"SECRET_WIN":     "from-bundle",
		"FILE_SECRET_NO": "from-bundle",
		"UNEMITTED":      "",
	}
	agentEnv := []string{
		"BUNDLE_ONLY=from-bundle",
		"GITHUB_TOKEN=ghp_from_hub",
		"INLINE_WIN=from-inline",
		"AUTH_WIN=from-auth",
		"SECRET_WIN=from-secret",
		"FILE_SECRET_NO=from-bundle",
		"OPERATOR_ONLY=keep",
	}
	callerEnv := map[string]string{
		"GITHUB_TOKEN": "ghp_from_hub",
	}
	inlineEnv := map[string]string{
		"INLINE_WIN": "from-inline",
	}
	resolvedAuth := &api.ResolvedAuth{
		EnvVars: map[string]string{
			"AUTH_WIN": "from-auth",
		},
	}
	resolvedSecrets := []api.ResolvedSecret{
		{Type: "environment", Target: "SECRET_WIN", Value: "from-secret"},
		{Type: "file", Target: "FILE_SECRET_NO", Value: "file-content"},
	}

	got := computeHookWithheldEnv(bundleEnv, agentEnv, callerEnv, inlineEnv, resolvedAuth, resolvedSecrets)
	want := []string{"BUNDLE_ONLY", "FILE_SECRET_NO"}
	if !slices.Equal(got, want) {
		t.Errorf("computeHookWithheldEnv() = %v, want %v", got, want)
	}
}

func TestComputeHookWithheldEnv_EmptyBundle(t *testing.T) {
	got := computeHookWithheldEnv(nil, []string{"FOO=bar"}, nil, nil, nil, nil)
	if len(got) != 0 {
		t.Errorf("computeHookWithheldEnv(nil, ...) = %v, want empty", got)
	}
}
