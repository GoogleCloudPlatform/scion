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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// TestBuildCommonRunArgs_FUSEWrapperMatchesSharedSuffix ties
// pkg/runtime/common.go's FUSE start wrapper to state.FUSEStartCmdExecSuffix,
// which sciontool init's resolveShellStartCmdArgs matches when resolving
// SCION_START_CMD from PID 1's environment under shell workload isolation.
func TestBuildCommonRunArgs_FUSEWrapperMatchesSharedSuffix(t *testing.T) {
	const img = "scion-agent:latest"
	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Shell{},
		Name:         "shell-fuse-agent",
		UnixUsername: "scion",
		Image:        img,
		Volumes: []api.VolumeMount{
			{Type: "gcs", Bucket: "my-bucket", Target: "/data"},
		},
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}

	imgIdx := slices.Index(args, img)
	if imgIdx < 0 {
		t.Fatalf("image %q not found in args: %v", img, args)
	}
	childArgs := args[imgIdx+1:]
	if len(childArgs) != 3 || childArgs[0] != "sh" || childArgs[1] != "-c" {
		t.Fatalf("FUSE child argv shape = %q, want 3-element [sh -c <script>] for sciontool resolveShellStartCmdArgs", childArgs)
	}
	if !strings.HasSuffix(childArgs[2], " && "+state.FUSEStartCmdExecSuffix) {
		t.Errorf("FUSE wrapper script %q does not end with %q", childArgs[2], " && "+state.FUSEStartCmdExecSuffix)
	}
}
