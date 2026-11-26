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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/filterquery"
	"github.com/GoogleCloudPlatform/scion/pkg/filterquery/schemas"
)

func TestDecisionsSchema(t *testing.T) {
	if schemas.Decisions.View != "decisions" {
		t.Fatalf("View = %q, want decisions", schemas.Decisions.View)
	}
	wantKeys := []string{"agent", "conversation", "by", "provenance"}
	if len(schemas.Decisions.Keys) != len(wantKeys) {
		t.Fatalf("len(Keys) = %d, want %d", len(schemas.Decisions.Keys), len(wantKeys))
	}
	for i, name := range wantKeys {
		k := schemas.Decisions.Keys[i]
		if k.Name != name {
			t.Errorf("Keys[%d].Name = %q, want %q", i, k.Name, name)
		}
		if k.Exec != filterquery.ExecServer {
			t.Errorf("Keys[%d] (%s) Exec = %v, want ExecServer", i, k.Name, k.Exec)
		}
		if name == "by" || name == "provenance" {
			if !strings.Contains(k.Help, "Made tab only") {
				t.Errorf("Keys[%d] (%s) Help = %q, want to contain 'Made tab only'", i, k.Name, k.Help)
			}
		}
	}

	validCases := []string{
		"",
		"deploy prod",
		"agent:deploy-frontend",
		"agent:a,b -agent:c",
		`conversation:"DM with deploy" by:me`,
		`by:"Ada Lovelace" provenance:direct,confirmed`,
		"-provenance:dismissed",
		`agent:deploy -conversation:"#general" by:ada@example.com rollover`,
	}
	for _, input := range validCases {
		q, err := filterquery.Parse(input, schemas.Decisions.HasKey)
		if err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", input, err)
			continue
		}
		if vErr := schemas.Decisions.Validate(q); vErr != nil {
			t.Errorf("Validate(%q) unexpected error: %v", input, vErr)
		}
	}

	invalidCases := []struct {
		input    string
		wantCode string
	}{
		{"provenance:bogus", filterquery.ErrCodeValue},
		{"agent:", filterquery.ErrCodeSyntax},
		{"agent:a OR agent:b", filterquery.ErrCodeSyntax},
	}
	for _, tc := range invalidCases {
		q, err := filterquery.Parse(tc.input, schemas.Decisions.HasKey)
		if err != nil {
			if err.Code != tc.wantCode {
				t.Errorf("Parse(%q) error code = %q, want %q", tc.input, err.Code, tc.wantCode)
			}
			continue
		}
		vErr := schemas.Decisions.Validate(q)
		if vErr == nil {
			t.Errorf("Validate(%q) succeeded, want error code %q", tc.input, tc.wantCode)
		} else if vErr.Code != tc.wantCode {
			t.Errorf("Validate(%q) error code = %q, want %q", tc.input, vErr.Code, tc.wantCode)
		}
	}
}

func TestProfileDecisionsSchema(t *testing.T) {
	if schemas.ProfileDecisions.View != "profile-decisions" {
		t.Fatalf("View = %q, want profile-decisions", schemas.ProfileDecisions.View)
	}
	wantKeys := []string{"agent", "conversation", "by", "provenance", "project"}
	if len(schemas.ProfileDecisions.Keys) != len(wantKeys) {
		t.Fatalf("len(Keys) = %d, want %d", len(schemas.ProfileDecisions.Keys), len(wantKeys))
	}
	for i, name := range wantKeys {
		k := schemas.ProfileDecisions.Keys[i]
		if k.Name != name {
			t.Errorf("Keys[%d].Name = %q, want %q", i, k.Name, name)
		}
		if k.Exec != filterquery.ExecServer {
			t.Errorf("Keys[%d] (%s) Exec = %v, want ExecServer", i, k.Name, k.Exec)
		}
		if name == "by" || name == "provenance" {
			if !strings.Contains(k.Help, "Made tab only") {
				t.Errorf("Keys[%d] (%s) Help = %q, want to contain 'Made tab only'", i, k.Name, k.Help)
			}
		}
	}

	validCases := []string{
		"",
		"deploy prod",
		"project:web-app",
		"project:alpha,beta -project:gamma",
		`project:demo agent:deploy-frontend -conversation:"#general" by:me provenance:direct,confirmed rollout`,
	}
	for _, input := range validCases {
		q, err := filterquery.Parse(input, schemas.ProfileDecisions.HasKey)
		if err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", input, err)
			continue
		}
		if vErr := schemas.ProfileDecisions.Validate(q); vErr != nil {
			t.Errorf("Validate(%q) unexpected error: %v", input, vErr)
		}
	}

	invalidCases := []struct {
		input    string
		wantCode string
	}{
		{"provenance:bogus", filterquery.ErrCodeValue},
		{"project:", filterquery.ErrCodeSyntax},
		{"project:a OR project:b", filterquery.ErrCodeSyntax},
	}
	for _, tc := range invalidCases {
		q, err := filterquery.Parse(tc.input, schemas.ProfileDecisions.HasKey)
		if err != nil {
			if err.Code != tc.wantCode {
				t.Errorf("Parse(%q) error code = %q, want %q", tc.input, err.Code, tc.wantCode)
			}
			continue
		}
		vErr := schemas.ProfileDecisions.Validate(q)
		if vErr == nil {
			t.Errorf("Validate(%q) succeeded, want error code %q", tc.input, tc.wantCode)
		} else if vErr.Code != tc.wantCode {
			t.Errorf("Validate(%q) error code = %q, want %q", tc.input, vErr.Code, tc.wantCode)
		}
	}
}
