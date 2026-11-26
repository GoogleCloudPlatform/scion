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
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/filterquery"
	"github.com/GoogleCloudPlatform/scion/pkg/filterquery/schemas"
)

func TestArtifactsSchema_IsDraftRegisteredAndValidated(t *testing.T) {
	isKey, ok := schemas.Artifacts.LookupKey("is")
	if !ok {
		t.Fatal("Artifacts schema missing `is` key")
	}
	if !slices.Contains(isKey.Enum, "draft") {
		t.Fatalf("Artifacts `is` enum = %v, want it to include \"draft\"", isKey.Enum)
	}
	if !isKey.MultiValued {
		t.Errorf("Artifacts `is` key MultiValued = false, want true")
	}

	valid := []string{
		"is:draft",
		"-is:draft",
		"is:draft,review",
		"-is:draft,shared",
		"is:draft is:mine",
		"is:draft is:shared",
		"is:draft is:review project:demo report",
		"IS:DRAFT",
	}
	for _, raw := range valid {
		q, err := filterquery.Parse(raw, schemas.Artifacts.HasKey)
		if err != nil {
			t.Errorf("Parse(%q) error = %v", raw, err)
			continue
		}
		if verr := schemas.Artifacts.Validate(q); verr != nil {
			t.Errorf("Validate(%q) error = %v", raw, verr)
		}
	}

	// Project-scoped artifacts view does not expose is:draft.
	q, err := filterquery.Parse("is:draft", schemas.ProjectArtifacts.HasKey)
	if err != nil {
		t.Fatalf("Parse(is:draft) on ProjectArtifacts: %v", err)
	}
	if verr := schemas.ProjectArtifacts.Validate(q); verr == nil || verr.Code != filterquery.ErrCodeValue {
		t.Errorf("ProjectArtifacts.Validate(is:draft) = %v, want %s", verr, filterquery.ErrCodeValue)
	}
}
