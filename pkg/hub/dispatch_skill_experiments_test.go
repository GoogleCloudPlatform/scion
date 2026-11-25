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

package hub

import (
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/resources"
)

var (
	skillFrontmatterExperimentRe = regexp.MustCompile(`(?m)^experiment:\s*["']?([^"'\s]+)["']?\s*$`)
	skillBlockExperimentRe       = regexp.MustCompile(`(?m)^\s*<!--\s*scion:experiment\s+(\S+)\s*-->\s*$`)
)

// TestDispatchExperimentNames_CoverPlatformSkillGates pins that the hub
// dispatches the state of every experiment an embedded platform skill gates
// on (ptone/scion#7830). The broker evaluates skill gates from the
// dispatched HubAgentDefaults.Experiments only, so a gate on an experiment
// missing from dispatchExperimentNames would always be off.
func TestDispatchExperimentNames_CoverPlatformSkillGates(t *testing.T) {
	refs := map[string][]string{} // experiment -> files naming it
	err := fs.WalkDir(resources.PlatformSkillsFS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		data, err := fs.ReadFile(resources.PlatformSkillsFS(), p)
		if err != nil {
			return err
		}
		content := strings.ReplaceAll(string(data), "\r\n", "\n")
		if front, ok := strings.CutPrefix(content, "---\n"); ok {
			if end := strings.Index(front, "\n---"); end >= 0 {
				for _, m := range skillFrontmatterExperimentRe.FindAllStringSubmatch(front[:end+1], -1) {
					refs[m[1]] = append(refs[m[1]], p)
				}
			}
		}
		for _, m := range skillBlockExperimentRe.FindAllStringSubmatch(content, -1) {
			refs[m[1]] = append(refs[m[1]], p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) == 0 {
		t.Fatal("no embedded platform skill names an experiment; the scan is broken or the Choices content was removed")
	}
	for id, files := range refs {
		if !slices.Contains(dispatchExperimentNames, id) {
			t.Errorf("platform skill file(s) %v gate on experiment %q, which is not in dispatchExperimentNames (pkg/hub/experiments.go); add it so the broker sees its state", files, id)
		}
	}
	for id, owner := range map[string]string{
		experiments.Decisions:       "the scion-messaging Choices content",
		experiments.Artifacts:       "the scion-artifacts skill",
		experiments.MessagePolicies: "the scion-messaging message policy guidance",
		experiments.AgentSecrets:    "the scion-messaging and scion-cli-operations agent secrets guidance",
	} {
		if _, ok := refs[id]; !ok {
			t.Errorf("no platform skill gates on %s; %s is expected to", id, owner)
		}
	}
}

// web.decisions, hub.artifacts, hub.message_policies and hub.agent_secrets are
// dispatched (for the scion-messaging, scion-cli-operations and scion-artifacts
// skill gates) and are off by default, so a fresh hub still dispatches no
// experiments.
func TestSkillGateExperiments_Dispatched(t *testing.T) {
	for _, id := range []string{experiments.Decisions, experiments.Artifacts, experiments.MessagePolicies, experiments.AgentSecrets} {
		exp, ok := experiments.Default().Lookup(id)
		if !ok {
			t.Fatalf("%s is not registered", id)
		}
		if exp.Default {
			t.Errorf("%s must stay off by default, or a fresh hub dispatches experiments", id)
		}
		if !exp.HasLayer(experiments.LayerServer) {
			t.Errorf("%s must be a server-layer experiment to be dispatched", id)
		}
		if !slices.Contains(dispatchExperimentNames, id) {
			t.Errorf("%s must be sent with dispatches", id)
		}
	}
}
