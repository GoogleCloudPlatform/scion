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
)

// TestBuildCommonRunArgs_MetadataInterceptionMarkerIsRuntimeOwned checks
// that the interception marker the container sees follows
// RunConfig.MetadataInterception (the field that grants NET_ADMIN) and that
// no other env source can set or clear it: not config.Env (user, template
// config and harness-config env reach the runtime there), not an
// environment-type secret, and not the template env fallback.
func TestBuildCommonRunArgs_MetadataInterceptionMarkerIsRuntimeOwned(t *testing.T) {
	for _, tc := range []struct {
		name         string
		interception bool
		injected     string
		want         string
		wantNetAdmin bool
	}{
		{name: "granted, others try to clear", interception: true, injected: MetadataInterceptionOff, want: MetadataInterceptionRequired, wantNetAdmin: true},
		{name: "granted, others try an empty value", interception: true, injected: "", want: MetadataInterceptionRequired, wantNetAdmin: true},
		{name: "not granted, others try to set", interception: false, injected: MetadataInterceptionRequired, want: MetadataInterceptionOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalRunConfig()
			cfg.MetadataInterception = tc.interception
			cfg.Env = []string{"SCION_METADATA_MODE=block", MetadataInterceptionEnvVar + "=" + tc.injected}
			cfg.ResolvedSecrets = []api.ResolvedSecret{
				{Name: "MARKER", Type: "environment", Target: MetadataInterceptionEnvVar, Value: tc.injected, Source: "user"},
			}
			cfg.FallbackEnv = map[string]string{MetadataInterceptionEnvVar: tc.injected}

			args, err := buildCommonRunArgs(cfg)
			if err != nil {
				t.Fatalf("buildCommonRunArgs: %v", err)
			}
			env := effectiveArgsEnv(args, cfg.Image)
			if got := env[MetadataInterceptionEnvVar]; got != tc.want {
				t.Errorf("%s = %q, want %q (args %v)", MetadataInterceptionEnvVar, got, tc.want, args)
			}
			n := 0
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-e" && strings.HasPrefix(args[i+1], MetadataInterceptionEnvVar+"=") {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%s emitted %d times, want exactly once (args %v)", MetadataInterceptionEnvVar, n, args)
			}
			if got := containsArg(args, "--cap-add", "NET_ADMIN"); got != tc.wantNetAdmin {
				t.Errorf("NET_ADMIN granted = %v, want %v", got, tc.wantNetAdmin)
			}
		})
	}
}
