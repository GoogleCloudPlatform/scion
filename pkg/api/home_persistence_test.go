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

package api

import "testing"

func TestEncodeAndParseHomePersistentRuntimeState(t *testing.T) {
	tests := []struct {
		name           string
		existing       string
		persistent     bool
		wantEncoded    string
		wantPersistent bool
		wantKnown      bool
	}{
		{
			name:           "empty existing true",
			existing:       "",
			persistent:     true,
			wantEncoded:    "home_persistent=true",
			wantPersistent: true,
			wantKnown:      true,
		},
		{
			name:           "empty existing false",
			existing:       "",
			persistent:     false,
			wantEncoded:    "home_persistent=false",
			wantPersistent: false,
			wantKnown:      true,
		},
		{
			name:           "preserves container prefix",
			existing:       "container:abc-123",
			persistent:     true,
			wantEncoded:    "container:abc-123;home_persistent=true",
			wantPersistent: true,
			wantKnown:      true,
		},
		{
			name:           "replaces prior token",
			existing:       "container:abc-123;home_persistent=true",
			persistent:     false,
			wantEncoded:    "container:abc-123;home_persistent=false",
			wantPersistent: false,
			wantKnown:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EncodeHomePersistentRuntimeState(tc.existing, tc.persistent)
			if got != tc.wantEncoded {
				t.Fatalf("EncodeHomePersistentRuntimeState(%q, %v) = %q, want %q", tc.existing, tc.persistent, got, tc.wantEncoded)
			}
			p, known := ParseHomePersistentRuntimeState(got)
			if p != tc.wantPersistent || known != tc.wantKnown {
				t.Fatalf("ParseHomePersistentRuntimeState(%q) = (%v, %v), want (%v, %v)", got, p, known, tc.wantPersistent, tc.wantKnown)
			}
		})
	}
}

func TestParseHomePersistentRuntimeState_Unknown(t *testing.T) {
	for _, raw := range []string{"", "container:abc", "home_persistent=maybe", "home_persistent="} {
		p, known := ParseHomePersistentRuntimeState(raw)
		if p || known {
			t.Errorf("ParseHomePersistentRuntimeState(%q) = (%v, %v), want (false, false)", raw, p, known)
		}
	}
}
