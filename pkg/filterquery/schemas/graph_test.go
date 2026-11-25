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

package schemas_test

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/filterquery"
	"github.com/GoogleCloudPlatform/scion/pkg/filterquery/schemas"
)

func TestGraphSchemas(t *testing.T) {
	wantGlobal := []string{"phase", "activity", "label", "project", "template", "harness", "mode"}
	if schemas.Graph.View != "graph" {
		t.Fatalf("Graph.View = %q, want %q", schemas.Graph.View, "graph")
	}
	if len(schemas.Graph.Keys) != len(wantGlobal) {
		t.Fatalf("Graph has %d keys, want %d", len(schemas.Graph.Keys), len(wantGlobal))
	}
	for i, name := range wantGlobal {
		k := schemas.Graph.Keys[i]
		if k.Name != name {
			t.Errorf("Graph.Keys[%d].Name = %q, want %q", i, k.Name, name)
		}
		if k.Exec != filterquery.ExecClient {
			t.Errorf("Graph key %q has Exec=%v, want ExecClient", k.Name, k.Exec)
		}
	}
}

func TestGraphSchemaParse(t *testing.T) {
	for _, input := range []string{
		"phase:running",
		"harness:antigravity",
		"activity:working,blocked",
		"mode:direct",
		"project:demo template:developer -label:tmp",
	} {
		q, err := filterquery.Parse(input, schemas.Graph.HasKey)
		if err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", input, err)
			continue
		}
		if q.Mode != filterquery.ModeStructured {
			t.Errorf("Parse(%q) mode = %v, want structured", input, q.Mode)
		}
	}
}
