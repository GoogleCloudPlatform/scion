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
	"reflect"
	"testing"
)

func TestExecDetachKeysArgs(t *testing.T) {
	tests := []struct {
		cmd  string
		want []string
	}{
		{"docker", []string{`--detach-keys=ctrl-\,ctrl-^`}},
		{"/usr/local/bin/docker", []string{`--detach-keys=ctrl-\,ctrl-^`}},
		{"podman", []string{"--detach-keys="}},
		{"/usr/bin/podman", []string{"--detach-keys="}},
		{"container", nil},
		{"kubernetes", nil},
		{"/tmp/runtime-exec", nil},
		{"", nil},
	}
	for _, tc := range tests {
		t.Run(tc.cmd, func(t *testing.T) {
			if got := ExecDetachKeysArgs(tc.cmd); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ExecDetachKeysArgs(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}
