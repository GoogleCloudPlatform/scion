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

package labels

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		labels  map[string]string
		wantErr string
	}{
		{
			name:   "nil labels",
			labels: nil,
		},
		{
			name:   "empty labels",
			labels: map[string]string{},
		},
		{
			name:   "valid single label",
			labels: map[string]string{"env": "prod"},
		},
		{
			name:   "valid multiple labels",
			labels: map[string]string{"env": "prod", "team": "platform", "role": "worker"},
		},
		{
			name:   "value can be empty",
			labels: map[string]string{"marker": ""},
		},
		{
			name:    "empty key",
			labels:  map[string]string{"": "value"},
			wantErr: "label key must not be empty",
		},
		{
			name:    "key too long",
			labels:  map[string]string{strings.Repeat("a", 64): "value"},
			wantErr: "exceeds maximum length of 63 characters",
		},
		{
			name:   "key at max length",
			labels: map[string]string{strings.Repeat("a", 63): "value"},
		},
		{
			name:    "value too long",
			labels:  map[string]string{"key": strings.Repeat("b", 64)},
			wantErr: "exceeds maximum length of 63 characters",
		},
		{
			name:   "value at max length",
			labels: map[string]string{"key": strings.Repeat("b", 63)},
		},
		{
			name:    "non-printable ASCII in key",
			labels:  map[string]string{"key\x00": "value"},
			wantErr: "non-printable ASCII character",
		},
		{
			name:    "non-printable ASCII in value",
			labels:  map[string]string{"key": "val\x01ue"},
			wantErr: "non-printable ASCII character",
		},
		{
			name:    "non-ASCII unicode in key",
			labels:  map[string]string{"ké y": "value"},
			wantErr: "non-printable ASCII character",
		},
		{
			name:    "tab in value",
			labels:  map[string]string{"key": "val\tue"},
			wantErr: "non-printable ASCII character",
		},
		{
			name:   "printable special characters",
			labels: map[string]string{"key-1.0/test": "value_with spaces & symbols!"},
		},
		{
			name: "too many labels",
			labels: func() map[string]string {
				m := make(map[string]string)
				for i := 0; i < 17; i++ {
					m[string(rune('a'+i))] = "v"
				}
				return m
			}(),
			wantErr: "too many labels: 17 exceeds maximum of 16",
		},
		{
			name: "exactly 16 labels",
			labels: func() map[string]string {
				m := make(map[string]string)
				for i := 0; i < 16; i++ {
					m[string(rune('a'+i))] = "v"
				}
				return m
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.labels)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("Validate() expected error containing %q, got nil", tt.wantErr)
				return
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}
