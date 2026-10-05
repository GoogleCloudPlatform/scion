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

package fsutil

import (
	"strings"
	"testing"
)

func TestValidateOwnerID(t *testing.T) {
	tests := []struct {
		name    string
		id      int64
		wantErr bool
	}{
		{"zero", 0, false},
		{"default", 1000, false},
		{"maximum", MaxOwnerID, false},
		{"negative one sentinel", -1, true},
		{"negative", -1000, true},
		{"unsigned sentinel", 4294967295, true},
		{"above 32 bits", 1 << 40, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOwnerID("--uid", int(tt.id))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ValidateOwnerID(%d) = nil, want an error", tt.id)
				}
				if !strings.Contains(err.Error(), "--uid") {
					t.Errorf("error %q does not name the field", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateOwnerID(%d) = %v, want nil", tt.id, err)
			}
		})
	}
}
