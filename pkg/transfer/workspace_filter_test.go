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

package transfer

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer/transfertest"
)

func TestExcludeWorkspaceObject_RuleTable(t *testing.T) {
	cases := transfertest.WorkspaceObjectCases()
	if len(cases) == 0 {
		t.Fatal("the shared case table is empty")
	}
	for _, tc := range cases {
		if got := ExcludeWorkspaceObject(tc.Name); string(got) != tc.Excluded {
			t.Errorf("ExcludeWorkspaceObject(%q) = %q, want %q", tc.Name, got, tc.Excluded)
		}
	}
}
