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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func TestReservedAgentEnvName_IncludesHookWithheldMarker(t *testing.T) {
	if !reservedAgentEnvName(envpolicy.HookWithheldEnvVar) {
		t.Fatalf("reservedAgentEnvName(%q) = false, want true", envpolicy.HookWithheldEnvVar)
	}
}

func TestBuildCommonRunArgs_HookWithheldEnvIsReservedAndEmittedLast(t *testing.T) {
	config := RunConfig{
		Harness:         &harness.Generic{},
		Name:            "test-agent",
		UnixUsername:    "scion",
		Image:           "scion-agent:latest",
		RuntimeName:     "docker",
		Env:             []string{"BUNDLE_KEY=v", envpolicy.HookWithheldEnvVar + "=FORGED_FROM_ENV"},
		FallbackEnv:     map[string]string{envpolicy.HookWithheldEnvVar: "FORGED_FROM_FALLBACK", "FALLBACK_KEY": "fb"},
		ResolvedSecrets: []api.ResolvedSecret{{Type: "environment", Target: envpolicy.HookWithheldEnvVar, Value: "FORGED_FROM_SECRET"}},
		HookWithheldEnv: []string{"Z_KEY", "BUNDLE_KEY", "BUNDLE_KEY"},
	}

	args, err := buildCommonRunArgs(config)
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	var envArgs []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-e" {
			envArgs = append(envArgs, args[i+1])
			i++
		}
	}

	wantEntry := envpolicy.HookWithheldEnvVar + "=BUNDLE_KEY,Z_KEY"
	var markerEntries []string
	for _, e := range envArgs {
		if k, _, _ := strings.Cut(e, "="); k == envpolicy.HookWithheldEnvVar {
			markerEntries = append(markerEntries, e)
		}
	}
	if len(markerEntries) != 1 || markerEntries[0] != wantEntry {
		t.Fatalf("marker -e entries = %v, want [%s]", markerEntries, wantEntry)
	}
	if envArgs[len(envArgs)-1] != wantEntry {
		t.Errorf("last -e entry = %q, want %q (must be emitted last)", envArgs[len(envArgs)-1], wantEntry)
	}
}

func TestBuildCommonRunArgs_HookWithheldEnvOmittedWhenEmptyEvenIfForged(t *testing.T) {
	config := RunConfig{
		Harness:         &harness.Generic{},
		Name:            "test-agent",
		UnixUsername:    "scion",
		Image:           "scion-agent:latest",
		RuntimeName:     "docker",
		Env:             []string{envpolicy.HookWithheldEnvVar + "=FORGED"},
		FallbackEnv:     map[string]string{envpolicy.HookWithheldEnvVar: "FORGED_FALLBACK"},
		ResolvedSecrets: []api.ResolvedSecret{{Type: "environment", Target: envpolicy.HookWithheldEnvVar, Value: "FORGED_SECRET"}},
	}

	args, err := buildCommonRunArgs(config)
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-e" {
			if k, _, _ := strings.Cut(args[i+1], "="); k == envpolicy.HookWithheldEnvVar {
				t.Errorf("unexpected %s emitted when HookWithheldEnv is empty: %q", envpolicy.HookWithheldEnvVar, args[i+1])
			}
			i++
		}
	}
}
