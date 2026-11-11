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
	"strings"
	"testing"
)

// TestBrokerProfile_AttachPointerDecode pins the tri-state wire contract of
// BrokerProfile.Attach: absent => nil (supported), false => &false,
// true => &true, and nil re-encodes with no "attach" key.
func TestBrokerProfile_AttachPointerDecode(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantNil bool
		want    bool
	}{
		{"key absent decodes nil", `{"name":"p"}`, true, false},
		{"explicit false decodes non-nil false", `{"name":"p","attach":false}`, false, false},
		{"explicit true decodes non-nil true", `{"name":"p","attach":true}`, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p BrokerProfile
			if err := json.Unmarshal([]byte(tc.in), &p); err != nil {
				t.Fatal(err)
			}
			if tc.wantNil {
				if p.Attach != nil {
					t.Fatalf("Attach = %v, want nil", *p.Attach)
				}
				return
			}
			if p.Attach == nil || *p.Attach != tc.want {
				t.Fatalf("Attach = %v, want &%v", p.Attach, tc.want)
			}
		})
	}
	out, err := json.Marshal(BrokerProfile{Name: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"attach"`) {
		t.Fatalf("nil Attach must be omitted, got %s", out)
	}
}
