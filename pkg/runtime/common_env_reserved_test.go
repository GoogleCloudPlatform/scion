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
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func TestBuildCommonRunArgs_BareReservedEnvNameIsNotEmitted(t *testing.T) {
	config := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Env:          []string{"SCION_SERVER_SESSION_SECRET", "SESSION_SECRET", "SCION_DEV_TOKEN", "SCION_AUTH_TOKEN", "ORDINARY_BARE", "WITH_VALUE=v"},
	}

	args, err := buildCommonRunArgs(config)
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	emitted := map[string]bool{}
	for i, arg := range args {
		if arg == "-e" && i+1 < len(args) {
			emitted[args[i+1]] = true
		}
	}
	for _, name := range []string{"SCION_SERVER_SESSION_SECRET", "SESSION_SECRET", "SCION_DEV_TOKEN", "SCION_AUTH_TOKEN"} {
		if emitted[name] {
			t.Errorf("bare -e %s emitted; the container CLI would copy the value from this process", name)
		}
	}
	for _, want := range []string{"ORDINARY_BARE", "WITH_VALUE=v"} {
		if !emitted[want] {
			t.Errorf("-e %s not emitted", want)
		}
	}
}

func TestBuildCommonRunArgs_VolumePathsDoNotExpandRefusedHostEnv(t *testing.T) {
	const sentinel = "refused-host-value"
	for _, name := range []string{"SCION_SERVER_SESSION_SECRET", "SESSION_SECRET", "SCION_DEV_TOKEN", "SCION_AUTH_TOKEN"} {
		t.Setenv(name, sentinel)
	}
	t.Setenv("ORDINARY_VOL_ROOT", "/ordinary")

	config := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Volumes: []api.VolumeMount{
			{Source: "/src/${SCION_SERVER_SESSION_SECRET}", Target: "/dst/${SESSION_SECRET}"},
			{Source: "/src2/$SCION_DEV_TOKEN", Target: "/dst2/$SCION_AUTH_TOKEN"},
			{Source: "${ORDINARY_VOL_ROOT}/pkg", Target: "/container/pkg"},
		},
	}

	args, err := buildCommonRunArgs(config)
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, sentinel) {
		t.Error("run args carry a refused host value in a mount path")
	}
	if !strings.Contains(joined, "-v /ordinary/pkg:/container/pkg") {
		t.Errorf("ordinary volume expansion missing from args: %s", joined)
	}
}
