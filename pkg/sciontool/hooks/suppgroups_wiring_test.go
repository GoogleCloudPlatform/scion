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

//go:build linux

package hooks

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
)

// grantedTestGroup stubs this process's supplementary groups so that
// suppgroups.FromEnv treats the returned gid as runtime-granted.
func grantedTestGroup(t *testing.T) uint32 {
	t.Helper()
	const g = 4242
	t.Cleanup(suppgroups.SetGetgroupsForTest(func() ([]int, error) { return []int{0, g}, nil }))
	return g
}

// Dropped hooks and the harness-provision wrapper keep the runtime-granted
// nfs shared-dir groups, like the harness (ptone/scion#3155).
func TestBuildEnforcedCmd_DroppedKeepsSharedDirGroups(t *testing.T) {
	skipUnlessFdExecSupported(t)
	g := grantedTestGroup(t)
	t.Setenv(suppgroups.EnvVar, strconv.FormatUint(uint64(g), 10))

	for name, tc := range map[string]struct {
		file   string
		event  string
		asRoot bool
	}{
		"dropped hook":      {"session-end", EventSessionEnd, false},
		"provision wrapper": {harness.HarnessProvisionHookFilename, EventPreStart, true},
	} {
		t.Run(name, func(t *testing.T) {
			script := filepath.Join(t.TempDir(), tc.file)
			mustWriteExecutableScript(t, script, "#!/bin/sh\nexit 0\n")
			f, _ := openScriptForTest(t, script)
			m := &LifecycleManager{
				EnforcePrivilegeDrop: true,
				AgentHome:            "/home/scion",
				WorkloadUID:          1000,
				WorkloadGID:          1000,
				WorkloadUsername:     "scion",
			}
			cmd, err := m.buildEnforcedCmd(f, script, tc.event, tc.asRoot)
			if err != nil {
				t.Fatalf("buildEnforcedCmd: %v", err)
			}
			cred := cmd.SysProcAttr.Credential
			if cred == nil || len(cred.Groups) != 1 || cred.Groups[0] != g {
				t.Fatalf("Credential = %+v, want Groups [%d]", cred, g)
			}
		})
	}
}
