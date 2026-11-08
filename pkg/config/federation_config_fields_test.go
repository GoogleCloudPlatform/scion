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

package config

import (
	"encoding/json"
	"testing"
)

// TestFederationValidateFields_NamesEachField checks that every failure
// names its field, and that Validate returns the same messages in the same
// order (ptone/scion#4450).
func TestFederationValidateFields_NamesEachField(t *testing.T) {
	cfg := &FederationConfig{
		Enabled:    true,
		Algorithms: []string{"HS256"},
		TrustedIssuers: []TrustedIssuerConfig{
			{IssuerURL: "ftp://bad.example", IssuerType: "hub"},
			{IssuerURL: "https://ok.example", IssuerType: "nope", AllowedDomains: []string{"*.example.com"}},
		},
	}
	fields := cfg.ValidateFields()
	want := map[string]bool{
		"algorithms":                            false,
		"trusted_issuers[0].issuer_url":         false,
		"trusted_issuers[1].issuer_type":        false,
		"trusted_issuers[1].allowed_domains[0]": false,
	}
	for _, f := range fields {
		if f.Path == "" || f.Message == "" {
			t.Errorf("entry without path or message: %+v", f)
		}
		if _, ok := want[f.Path]; ok {
			want[f.Path] = true
		}
	}
	for p, seen := range want {
		if !seen {
			t.Errorf("no entry for %s in %+v", p, fields)
		}
	}

	errs := cfg.Validate()
	if len(errs) != len(fields) {
		t.Fatalf("Validate returned %d errors, ValidateFields %d", len(errs), len(fields))
	}
	for i := range errs {
		if errs[i].Error() != fields[i].Message {
			t.Errorf("errs[%d] = %q, want %q", i, errs[i].Error(), fields[i].Message)
		}
	}
}

func TestFederationValidateFields_DisabledOrValid(t *testing.T) {
	if got := (&FederationConfig{}).ValidateFields(); len(got) != 0 {
		t.Errorf("disabled config: got %+v", got)
	}
	if got := (&FederationConfig{}).Validate(); got != nil {
		t.Errorf("disabled config Validate: got %v, want nil", got)
	}
	ok := &FederationConfig{Enabled: true, TrustedIssuers: []TrustedIssuerConfig{{IssuerURL: "https://hub.example", IssuerType: "hub"}}}
	if got := ok.ValidateFields(); len(got) != 0 {
		t.Errorf("valid config: got %+v", got)
	}
}

// TestValidationError_JSONTags pins the lowercase wire names.
func TestValidationError_JSONTags(t *testing.T) {
	b, err := json.Marshal(ValidationError{Path: "hub/endpoint", Message: "bad"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"path":"hub/endpoint","message":"bad"}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	b, _ = json.Marshal(ValidationError{Path: "p", Message: "m", StorageField: true})
	if got, want := string(b), `{"path":"p","message":"m","storage_field":true}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
