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

package harnesses_test

import (
	"io/fs"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/harnesses"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestEmbeddedHarnessThinkingBlocks loads every embedded harness config.yaml
// that declares a `thinking:` block and checks that it passes the schema and
// the load-time ordering check, then pins the codex, antigravity and claude
// tables to their agreed literals (ptone/scion#2673, ptone/scion#3011). The
// codex table must equal the pre-migration hard-coded buckets in provision.py
// byte for byte; the provision_test.py characterization tables restate the
// same literals.
func TestEmbeddedHarnessThinkingBlocks(t *testing.T) {
	want := map[string]*config.HarnessThinkingConfig{
		"codex": {
			Levels: []config.HarnessThinkingLevel{
				{Max: 25, Value: "low"},
				{Max: 50, Value: "medium"},
				{Max: 75, Value: "high"},
				{Max: 100, Value: "xhigh"},
			},
			Default: "medium",
		},
		"antigravity": {
			Levels: []config.HarnessThinkingLevel{
				{Max: 25, Value: "low"},
				{Max: 50, Value: "medium"},
				{Max: 100, Value: "high"},
			},
		},
		// No Default: an unset level leaves Claude Code's per-model default
		// effort in place (ptone/scion#3011).
		"claude": {
			Levels: []config.HarnessThinkingLevel{
				{Max: 25, Value: "low"},
				{Max: 50, Value: "medium"},
				{Max: 75, Value: "high"},
				{Max: 100, Value: "xhigh"},
			},
		},
	}

	entries, err := fs.ReadDir(harnesses.FS, ".")
	if err != nil {
		t.Fatalf("read harnesses FS root: %v", err)
	}

	got := map[string]*config.HarnessThinkingConfig{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		data, err := fs.ReadFile(harnesses.FS, name+"/config.yaml")
		if err != nil {
			continue
		}
		entry, err := config.ParseHarnessConfigYAML(data)
		if err != nil {
			t.Errorf("%s/config.yaml: parse: %v", name, err)
			continue
		}
		if entry.Thinking == nil {
			continue
		}
		got[name] = entry.Thinking
		if verrs, err := config.ValidateHarnessConfig(data); err != nil {
			t.Errorf("%s/config.yaml: schema validation error: %v", name, err)
		} else if len(verrs) > 0 {
			t.Errorf("%s/config.yaml: schema violations: %v", name, verrs)
		}
		if err := entry.Thinking.Validate(); err != nil {
			t.Errorf("%s/config.yaml: thinking block: %v", name, err)
		}
	}

	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%s/config.yaml: missing thinking block", name)
			continue
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s/config.yaml: thinking block = %+v, want %+v", name, *g, *w)
		}
	}
}
