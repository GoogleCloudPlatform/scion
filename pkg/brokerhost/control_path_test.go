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

package brokerhost

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestCheckInstanceDefaultVolumes(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	outside := filepath.Join(root, "data")
	for _, d := range []string{filepath.Join(home, ".scion"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	ok := []config.V1RuntimeBrokerInstanceConfig{{Key: "a", AgentDefaults: &config.V1InstanceAgentDefaults{
		Volumes: []api.VolumeMount{{Source: outside, Target: "/x"}},
	}}, {Key: "b"}}
	if err := checkInstanceDefaultVolumes(ok); err != nil {
		t.Fatalf("clean defaults: %v", err)
	}

	bad := []config.V1RuntimeBrokerInstanceConfig{{Key: "a"}, {Key: "b", AgentDefaults: &config.V1InstanceAgentDefaults{
		Volumes: []api.VolumeMount{{Source: "~/.scion", Target: "/x", ReadOnly: true}},
	}}}
	err = checkInstanceDefaultVolumes(bad)
	if !errors.Is(err, brokercontrol.ErrControlPathMount) || !strings.Contains(err.Error(), "server.broker.instances[1].agent_defaults.volumes[0]") {
		t.Fatalf("err = %v", err)
	}

	override := []config.V1RuntimeBrokerInstanceConfig{{Key: "a", AgentDefaults: &config.V1InstanceAgentDefaults{
		HarnessOverrides: map[string]config.V1HarnessOverride{"claude": {Volumes: []api.VolumeMount{{Source: home, Target: "/h"}}}},
	}}}
	err = checkInstanceDefaultVolumes(override)
	if !errors.Is(err, brokercontrol.ErrControlPathMount) || !strings.Contains(err.Error(), "harness_overrides.claude") {
		t.Fatalf("err = %v", err)
	}
}
