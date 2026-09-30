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

package opsettings

import (
	"encoding/json"
	"testing"
)

// TestParseExperimentsDocMatchesRefreshUpdatePredicate feeds the same table
// of documents to ParseExperimentsDoc and to the exact malformed predicate
// Refresh/Update apply generically (json.Valid + json.Unmarshal into
// sec.New()), and asserts the two never disagree (ptone/scion#2217).
func TestParseExperimentsDocMatchesRefreshUpdatePredicate(t *testing.T) {
	sec := SectionByName("experiments")
	if sec == nil {
		t.Fatal("experiments section not registered")
	}

	tests := []struct {
		name string
		raw  json.RawMessage
	}{
		{"valid with overrides", json.RawMessage(`{"overrides":{"web.terminal_workspace":false}}`)},
		{"valid empty object", json.RawMessage(`{}`)},
		{"invalid json", json.RawMessage(`not json`)},
		{"non-boolean override value", json.RawMessage(`{"overrides":{"web.terminal_workspace":"nope"}}`)},
		{"parseable doc with extra top-level key", json.RawMessage(`{"overrides":{},"unexpected":true}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, gotMalformed := ParseExperimentsDoc(tt.raw)

			wantMalformed := !json.Valid(tt.raw)
			if !wantMalformed {
				target := sec.New()
				if err := json.Unmarshal(tt.raw, target); err != nil {
					wantMalformed = true
				}
			}

			if gotMalformed != wantMalformed {
				t.Errorf("ParseExperimentsDoc malformed=%v, Refresh/Update predicate malformed=%v", gotMalformed, wantMalformed)
			}
		})
	}
}

func TestParseExperimentsDoc_ValidOverridesRoundTrip(t *testing.T) {
	doc, malformed := ParseExperimentsDoc(json.RawMessage(`{"overrides":{"web.terminal_workspace":false}}`))
	if malformed {
		t.Fatal("expected a valid document, got malformed=true")
	}
	if v, ok := doc.Overrides["web.terminal_workspace"]; !ok || v != false {
		t.Errorf("doc.Overrides = %v, want web.terminal_workspace=false", doc.Overrides)
	}
}

// TestExperimentsSchemaValidation checks the hand-written schema accepts
// valid documents and rejects bad names, non-bool values, and extra
// top-level keys.
func TestExperimentsSchemaValidation(t *testing.T) {
	tests := []struct {
		name    string
		doc     json.RawMessage
		wantErr bool
	}{
		{"valid overrides", json.RawMessage(`{"overrides":{"web.terminal_workspace":false}}`), false},
		{"empty doc", json.RawMessage(`{}`), false},
		{"empty overrides", json.RawMessage(`{"overrides":{}}`), false},
		{"bad name: no dot", json.RawMessage(`{"overrides":{"notaname":true}}`), true},
		{"bad name: uppercase", json.RawMessage(`{"overrides":{"Web.Terminal":true}}`), true},
		{"non-bool value", json.RawMessage(`{"overrides":{"web.terminal_workspace":"nope"}}`), true},
		{"extra top-level key", json.RawMessage(`{"overrides":{},"unexpected":true}`), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := Validate("experiments", tt.doc)
			if tt.wantErr && len(errs) == 0 {
				t.Error("expected validation errors, got none")
			}
			if !tt.wantErr && len(errs) > 0 {
				t.Errorf("expected no validation errors, got %v", errs)
			}
		})
	}
}
