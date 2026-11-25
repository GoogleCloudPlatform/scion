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

package filterquery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type classifyMatchVectorFile struct {
	Cases []struct {
		Name  string `json:"name"`
		Query string `json:"query"`
		Field string `json:"field"`
		Tier  *int   `json:"tier"`
	} `json:"cases"`
}

func TestClassifyMatch_SharedVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "classify_match_vectors.json"))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var vf classifyMatchVectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatalf("unmarshal vectors: %v", err)
	}
	if len(vf.Cases) == 0 {
		t.Fatal("expected non-empty classify_match_vectors.json")
	}
	for _, tc := range vf.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			q := NormalizeQuery(tc.Query)
			tier, ok := ClassifyMatch(q, tc.Field)
			if tc.Tier == nil {
				if ok {
					t.Fatalf("ClassifyMatch(%q, %q) = (%d, true), want no match", q, tc.Field, tier)
				}
				return
			}
			if !ok || tier != *tc.Tier {
				t.Fatalf("ClassifyMatch(%q, %q) = (%d, %v), want (%d, true)", q, tc.Field, tier, ok, *tc.Tier)
			}
		})
	}
}
