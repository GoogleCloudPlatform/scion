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

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
)

// TestWriteOwnerMaterialChain: the owner material chain section prints the
// counts, the reasons, the remedy and one row per listed agent with its
// depth and row names.
func TestWriteOwnerMaterialChain(t *testing.T) {
	var buf bytes.Buffer
	o := &textOut{w: &buf}
	writeOwnerMaterialChain(o, hub.OwnerMaterialChainCensus{
		Withheld: 2, Affected: 1,
		ByReason: map[string]int{"terminal_user_not_owner": 1, "legacy_no_edge": 1},
		Remedy:   "reincarnate",
		Rows: []hub.OwnerMaterialChainCensusRow{
			{
				AgentID: "agent-1", ProjectID: "project-1", OwnerID: "user-1",
				Reason: "terminal_user_not_owner", Depth: 1,
				Secrets: []string{"API_KEY", "TOKEN"}, EnvVars: []string{"MY_ENV"},
				SharedSecrets: []string{"SHARED_KEY"},
			},
			{
				AgentID: "agent-2", ProjectID: "project-1", OwnerID: "user-2",
				Reason: "legacy_no_edge", Secrets: []string{}, EnvVars: []string{},
			},
		},
	})
	if o.err != nil {
		t.Fatalf("write: %v", o.err)
	}
	out := buf.String()
	for _, want := range []string{
		"# owner material chain\n", "withheld\t2\n", "affected\t1\n",
		"reason\tlegacy_no_edge\t1\n", "reason\tterminal_user_not_owner\t1\n",
		"remedy\treincarnate\n",
		"agent_id\tproject_id\towner_id\treason\tdepth\tsecrets\tenv_vars\trows_error\tshared_secrets\n",
		"agent-1\tproject-1\tuser-1\tterminal_user_not_owner\t1\tAPI_KEY,TOKEN\tMY_ENV\t\tSHARED_KEY\n",
		"agent-2\tproject-1\tuser-2\tlegacy_no_edge\t0\t\t\t\t\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// TestWriteRuntimeRead: the runtime read section prints the counts by
// cohort and by shape and one row per listed agent with its shape, depth,
// ancestor, root user, shared secret names and remedy.
func TestWriteRuntimeRead(t *testing.T) {
	var buf bytes.Buffer
	o := &textOut{w: &buf}
	writeRuntimeRead(o, hub.RuntimeReadCensus{
		ByCohort: map[string]int{"runtime_read_refused": 1},
		ByShape:  map[string]int{"unrecorded_above": 1},
		Rows: []hub.RuntimeReadCensusRow{{
			AgentID: "agent-1", ProjectID: "project-1", OwnerID: "user-1", Cohort: "runtime_read_refused",
			Shape: "unrecorded_above", Depth: 1, AncestorAgentID: "agent-0", RootUserID: "user-1",
			SharedSecrets: []string{"API_KEY", "TOKEN"}, Remedy: "reincarnate",
		}},
	})
	if o.err != nil {
		t.Fatalf("write: %v", o.err)
	}
	out := buf.String()
	for _, want := range []string{
		"# runtime read\n", "cohort\truntime_read_refused\t1\n", "shape\tunrecorded_above\t1\n",
		"agent_id\tproject_id\towner_id\tcohort\tshape\tdepth\tancestor_agent_id\troot_user_id\tshared_secrets\tsecrets_error\tremedy\n",
		"agent-1\tproject-1\tuser-1\truntime_read_refused\tunrecorded_above\t1\tagent-0\tuser-1\tAPI_KEY,TOKEN\t\treincarnate\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
