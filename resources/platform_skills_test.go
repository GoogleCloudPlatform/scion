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

package resources

import (
	"io/fs"
	"strings"
	"testing"
)

func TestIndependentReviewPlatformSkillContract(t *testing.T) {
	data, err := fs.ReadFile(PlatformSkillsFS(), "independent-review/SKILL.md")
	if err != nil {
		t.Fatalf("independent-review platform skill is not embedded: %v", err)
	}

	content := string(data)
	for _, required := range []string{
		"create a separate reviewer agent",
		"Do not ask it to judge only from the producer's summary",
		"findings go to the orchestrator",
		"every material finding has an explicit disposition",
		"run a focused re-review before acceptance",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("independent-review skill missing required contract text %q", required)
		}
	}
}
