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

package filterquery_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/filterquery"
	"github.com/GoogleCloudPlatform/scion/pkg/filterquery/schemas"
)

type conformanceFile struct {
	Parse []parseCase `json:"parse"`
	Eval  evalSuite   `json:"eval"`
}

type parseCase struct {
	Name      string         `json:"name"`
	Input     string         `json:"input"`
	KnownKeys []string       `json:"knownKeys"`
	Expect    *expectedQuery `json:"expect,omitempty"`
	Canonical *string        `json:"canonical,omitempty"`
	Error     *expectedError `json:"error,omitempty"`
}

type expectedQuery struct {
	Mode        string             `json:"mode"`
	Text        string             `json:"text,omitempty"`
	Terms       []filterquery.Term `json:"terms,omitempty"`
	ExcludeText []string           `json:"excludeText,omitempty"`
}

type expectedError struct {
	Code   string `json:"code"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

type evalSuite struct {
	KnownKeys []string   `json:"knownKeys"`
	Rows      []evalRow  `json:"rows"`
	Cases     []evalCase `json:"cases"`
}

type evalRow struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Slug   string            `json:"slug"`
	Phase  string            `json:"phase"`
	Labels map[string]string `json:"labels"`
}

func (r evalRow) Values(key string) []string {
	switch key {
	case "phase":
		if r.Phase == "" {
			return nil
		}
		return []string{r.Phase}
	case "label":
		if len(r.Labels) == 0 {
			return nil
		}
		out := make([]string, 0, len(r.Labels)*2)
		for k, v := range r.Labels {
			out = append(out, k, k+"="+v)
		}
		return out
	default:
		return nil
	}
}

func (r evalRow) Text() []string {
	return []string{r.Name, r.Slug}
}

type evalCase struct {
	Name      string   `json:"name"`
	Query     string   `json:"query"`
	ExpectIDs []string `json:"expectIds"`
}

func loadConformance(t *testing.T) conformanceFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "conformance.json"))
	if err != nil {
		t.Fatalf("read conformance.json: %v", err)
	}
	var cf conformanceFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		t.Fatalf("unmarshal conformance.json: %v", err)
	}
	return cf
}

func keySetFunc(keys []string) func(string) bool {
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[strings.ToLower(k)] = true
	}
	return func(k string) bool { return set[strings.ToLower(k)] }
}

func TestConformance_Parse(t *testing.T) {
	cf := loadConformance(t)
	for _, tc := range cf.Parse {
		t.Run(tc.Name, func(t *testing.T) {
			q, fqErr := filterquery.Parse(tc.Input, keySetFunc(tc.KnownKeys))
			if tc.Error != nil {
				if fqErr == nil {
					t.Fatalf("Parse(%q) succeeded (%+v), want error %+v", tc.Input, q, *tc.Error)
				}
				if fqErr.Code != tc.Error.Code {
					t.Errorf("error code = %q, want %q (msg: %s)", fqErr.Code, tc.Error.Code, fqErr.Message)
				}
				if fqErr.Offset != tc.Error.Offset || fqErr.Length != tc.Error.Length {
					t.Errorf("error span = (%d, %d), want (%d, %d) (msg: %s)",
						fqErr.Offset, fqErr.Length, tc.Error.Offset, tc.Error.Length, fqErr.Message)
				}
				return
			}
			if fqErr != nil {
				t.Fatalf("Parse(%q) returned unexpected error: %v", tc.Input, fqErr)
			}
			if tc.Expect == nil {
				t.Fatalf("test case %q has neither expect nor error", tc.Name)
			}
			if q.Mode.String() != tc.Expect.Mode {
				t.Errorf("mode = %q, want %q", q.Mode.String(), tc.Expect.Mode)
			}
			if q.Text != tc.Expect.Text {
				t.Errorf("text = %q, want %q", q.Text, tc.Expect.Text)
			}
			if !reflect.DeepEqual(q.Terms, tc.Expect.Terms) {
				t.Errorf("terms = %+v, want %+v", q.Terms, tc.Expect.Terms)
			}
			if !reflect.DeepEqual(q.ExcludeText, tc.Expect.ExcludeText) {
				t.Errorf("excludeText = %+v, want %+v", q.ExcludeText, tc.Expect.ExcludeText)
			}
			if tc.Canonical != nil {
				gotCanon := filterquery.Canonical(q)
				if gotCanon != *tc.Canonical {
					t.Errorf("Canonical = %q, want %q", gotCanon, *tc.Canonical)
				}
				// Round-trip stability: parsing canonical produces the same canonical output.
				q2, err2 := filterquery.Parse(gotCanon, keySetFunc(tc.KnownKeys))
				if err2 != nil {
					t.Errorf("Parse(Canonical(%q) = %q) failed: %v", tc.Input, gotCanon, err2)
				} else if got2 := filterquery.Canonical(q2); got2 != gotCanon {
					t.Errorf("Canonical not idempotent: %q -> %q", gotCanon, got2)
				}
			}
		})
	}
}

func TestConformance_Eval(t *testing.T) {
	cf := loadConformance(t)
	known := keySetFunc(cf.Eval.KnownKeys)
	for _, tc := range cf.Eval.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			q, fqErr := filterquery.Parse(tc.Query, known)
			if fqErr != nil {
				t.Fatalf("Parse(%q) failed: %v", tc.Query, fqErr)
			}
			var gotIDs []string
			for _, row := range cf.Eval.Rows {
				if filterquery.Match(q, row) {
					gotIDs = append(gotIDs, row.ID)
				}
			}
			if !slices.Equal(gotIDs, tc.ExpectIDs) {
				t.Errorf("Match(%q) IDs = %v, want %v", tc.Query, gotIDs, tc.ExpectIDs)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	schemaWithClientAndFlags := filterquery.Schema{
		View:     "test",
		TextHelp: "test",
		Keys: []filterquery.Key{
			{
				Name: "phase",
				Kind: filterquery.KindEnum,
				Exec: filterquery.ExecServer,
				Enum: []string{"running", "stopped", "error"},
			},
			{
				Name:        "label",
				Kind:        filterquery.KindLabel,
				Exec:        filterquery.ExecServer,
				MultiValued: true,
			},
			{
				Name:      "is",
				Kind:      filterquery.KindFlagSet,
				Exec:      filterquery.ExecServer,
				Enum:      []string{"mine", "shared"},
				Exclusive: [][]string{{"mine", "shared"}},
			},
			{
				Name: "mode",
				Kind: filterquery.KindEnum,
				Exec: filterquery.ExecClient,
				Enum: []string{"lineage", "branch"},
			},
		},
	}

	tests := []struct {
		name      string
		input     string
		wantCode  string
		wantInMsg string
	}{
		{name: "valid enum and label", input: "phase:running,ERROR label:a=b label:c"},
		{name: "invalid enum with suggestion", input: "phase:runing", wantCode: filterquery.ErrCodeValue, wantInMsg: "running"},
		{name: "mutually exclusive flags", input: "is:mine is:shared", wantCode: filterquery.ErrCodeValue, wantInMsg: "mutually exclusive"},
		{name: "client-only key rejected on server", input: "mode:lineage", wantCode: filterquery.ErrCodeUnsupportedKey, wantInMsg: "not supported"},
		{name: "empty label key rejected", input: "label:=foo", wantCode: filterquery.ErrCodeValue, wantInMsg: "must not be empty"},
		{name: "pure text valid", input: "just plain text"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, fqErr := filterquery.Parse(tc.input, schemaWithClientAndFlags.HasKey)
			if fqErr != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", tc.input, fqErr)
			}
			vErr := schemaWithClientAndFlags.Validate(q)
			if tc.wantCode == "" {
				if vErr != nil {
					t.Fatalf("Validate(%q) unexpected error: %v", tc.input, vErr)
				}
				return
			}
			if vErr == nil {
				t.Fatalf("Validate(%q) succeeded, want error code %q", tc.input, tc.wantCode)
			}
			if vErr.Code != tc.wantCode {
				t.Errorf("error code = %q, want %q (msg: %s)", vErr.Code, tc.wantCode, vErr.Message)
			}
			if tc.wantInMsg != "" && !strings.Contains(vErr.Message, tc.wantInMsg) {
				t.Errorf("error message = %q, want substring %q", vErr.Message, tc.wantInMsg)
			}
		})
	}
}

func TestLimits(t *testing.T) {
	known := schemas.Agents.HasKey

	// > 512 bytes
	longInput := strings.Repeat("a", filterquery.MaxInputBytes+1)
	if _, err := filterquery.Parse(longInput, known); err == nil || err.Code != filterquery.ErrCodeLimit {
		t.Errorf("over MaxInputBytes: got %v, want limit error", err)
	}

	// > 200 runes in pure text
	longText := strings.Repeat("x", filterquery.MaxTextRunes+1)
	if _, err := filterquery.Parse(longText, known); err == nil || err.Code != filterquery.ErrCodeLimit {
		t.Errorf("over MaxTextRunes: got %v, want limit error", err)
	}

	// > 20 values per key
	vals := make([]string, filterquery.MaxValuesPerKey+1)
	for i := range vals {
		vals[i] = "a"
	}
	if _, err := filterquery.Parse("label:"+strings.Join(vals, ","), known); err == nil || err.Code != filterquery.ErrCodeLimit {
		t.Errorf("over MaxValuesPerKey: got %v, want limit error", err)
	}

	// > 32 terms
	terms := make([]string, filterquery.MaxTerms+1)
	for i := range terms {
		terms[i] = "label:a"
	}
	if _, err := filterquery.Parse(strings.Join(terms, " "), known); err == nil || err.Code != filterquery.ErrCodeLimit {
		t.Errorf("over MaxTerms: got %v, want limit error", err)
	}
}

func FuzzParse(f *testing.F) {
	f.Add("")
	f.Add("phase:running")
	f.Add("phase:running,error -label:tmp foo")
	f.Add("label:\"team=web ui\" -\"draft notes\"")
	f.Add("src/(foo|bar)\\.go")
	f.Add("phase:running OR phase:stopped")
	f.Add("phase:")
	f.Add("label:\"unterminated")

	known := schemas.Agents.HasKey
	f.Fuzz(func(t *testing.T, input string) {
		q, err := filterquery.Parse(input, known)
		if err != nil {
			return
		}
		canon := filterquery.Canonical(q)
		if q.Mode == filterquery.ModeStructured {
			q2, err2 := filterquery.Parse(canon, known)
			if err2 != nil {
				t.Fatalf("Parse(Canonical(%q) = %q) failed: %v", input, canon, err2)
			}
			if canon2 := filterquery.Canonical(q2); canon2 != canon {
				t.Fatalf("Canonical not idempotent for %q: %q != %q", input, canon, canon2)
			}
		}
	})
}
