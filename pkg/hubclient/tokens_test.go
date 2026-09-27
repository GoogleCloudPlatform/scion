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

package hubclient

import (
	"encoding/json"
	"testing"
)

func TestTokenInfo_JSON(t *testing.T) {
	t.Run("unmarshal legacy groveId field is not honored", func(t *testing.T) {
		jsonData := `{"id": "t1", "groveId": "p1"}`
		var i TokenInfo
		if err := json.Unmarshal([]byte(jsonData), &i); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if i.ProjectID != "" {
			t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", i.ProjectID)
		}
	})

	t.Run("marshal emits only canonical fields", func(t *testing.T) {
		i := TokenInfo{ID: "t1", ProjectID: "p1"}
		data, err := json.Marshal(i)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Unmarshal back failed: %v", err)
		}

		if m["projectId"] != "p1" {
			t.Errorf("projectId = %v, want %q", m["projectId"], "p1")
		}
		if _, ok := m["groveId"]; ok {
			t.Errorf("legacy 'groveId' key present in marshal output, want absent: %v", m["groveId"])
		}
	})
}
