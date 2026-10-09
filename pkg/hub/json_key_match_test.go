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
	"encoding/json"
	"iter"
	"reflect"
	"testing"
)

// orderedNames yields names in slice order, each paired with its index,
// so a test can tell which candidate was matched.
func orderedNames(names ...string) iter.Seq2[string, int] {
	return func(yield func(string, int) bool) {
		for i, n := range names {
			if !yield(n, i) {
				return
			}
		}
	}
}

func TestMatchJSONKey(t *testing.T) {
	tests := []struct {
		name   string
		names  []string
		key    string
		want   int
		wantOK bool
	}{
		{"exact only", []string{"a", "github_app", "b"}, "github_app", 1, true},
		{"exact wins over earlier fold", []string{"GitHub_App", "github_app"}, "github_app", 1, true},
		{"exact wins over later fold", []string{"github_app", "GITHUB_APP"}, "github_app", 0, true},
		{"fold when no exact", []string{"x", "GitHub_App"}, "github_app", 1, true},
		{"first fold in order", []string{"GitHub_App", "GITHUB_APP"}, "github_app", 0, true},
		{"fold of request key", []string{"server"}, "SERVER", 0, true},
		{"no match", []string{"github", "app"}, "github_app", 0, false},
		{"no candidates", nil, "github_app", 0, false},
		{"empty key exact", []string{"", "a"}, "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := matchJSONKey(orderedNames(tt.names...), tt.key)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("matchJSONKey(%q, %q) = %d, %v; want %d, %v",
					tt.names, tt.key, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

type jsonKeyMatchEmbedded struct {
	Inner string `json:"inner"`
}

type jsonKeyMatchFixture struct {
	jsonKeyMatchEmbedded
	Exact      string `json:"github_app"`
	Folded     string `json:"GitHub_App"`
	Omit       string `json:"omit_me,omitempty"`
	Untagged   string
	Dash       string `json:"-"`
	DashComma  string `json:"-,"` //nolint:staticcheck // pins how each helper reads a "-," tag
	unexported string //nolint:unused // pins that unexported fields are not matched
}

func TestStructFieldByJSONNameMatching(t *testing.T) {
	typ := reflect.TypeOf(&jsonKeyMatchFixture{})
	tests := []struct {
		key       string
		wantField string
	}{
		{"github_app", "Exact"},
		{"GitHub_App", "Folded"},
		{"GITHUB_APP", "Exact"}, // first fold in field order
		{"omit_me", "Omit"},
		{"OMIT_ME", "Omit"},
		{"Untagged", "Untagged"},
		{"untagged", "Untagged"},
		{"Dash", ""},
		{"-", ""}, // a "-," tag is not a candidate here
		{"DashComma", ""},
		{"unexported", ""},
		{"inner", ""}, // embedded struct fields are not flattened here
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			f, ok := structFieldByJSONName(typ, tt.key)
			if tt.wantField == "" {
				if ok {
					t.Fatalf("matched %s, want no match", f.Name)
				}
				return
			}
			if !ok || f.Name != tt.wantField {
				t.Fatalf("got %q, %v; want %q", f.Name, ok, tt.wantField)
			}
		})
	}
	if _, ok := structFieldByJSONName(reflect.TypeOf(""), "x"); ok {
		t.Error("non-struct type matched")
	}
}

func TestFieldFoldMatching(t *testing.T) {
	fields := jsonFields(reflect.TypeOf(jsonKeyMatchFixture{}))
	strType := reflect.TypeOf("")
	tests := []struct {
		key    string
		wantOK bool
	}{
		{"github_app", true},
		{"GitHub_App", true},
		{"omit_me", true},
		{"untagged", true},
		{"inner", true}, // jsonFields flattens embedded structs
		{"INNER", true},
		{"-", true}, // jsonFields names a "-," field "-"
		{"Dash", false},
		{"unexported", false},
		{"missing", false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			ft, ok := fieldFold(fields, tt.key)
			if ok != tt.wantOK || (ok && ft != strType) {
				t.Fatalf("fieldFold(%q) = %v, %v; want ok %v", tt.key, ft, ok, tt.wantOK)
			}
		})
	}
}

func TestLookupFoldMatching(t *testing.T) {
	m := map[string]json.RawMessage{
		"github_app": json.RawMessage(`1`),
		"GitHub_App": json.RawMessage(`2`),
		"server":     json.RawMessage(`3`),
	}
	tests := []struct {
		key    string
		want   string
		wantOK bool
	}{
		{"github_app", "1", true},
		{"GitHub_App", "2", true},
		{"Server", "3", true},
		{"missing", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			v, ok := lookupFold(m, tt.key)
			if ok != tt.wantOK || string(v) != tt.want {
				t.Fatalf("lookupFold(%q) = %s, %v; want %s, %v", tt.key, v, ok, tt.want, tt.wantOK)
			}
		})
	}
}
