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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/filterquery"
	"github.com/GoogleCloudPlatform/scion/pkg/filterquery/schemas"
)

func TestFilesSchemaRegistration(t *testing.T) {
	found := false
	for _, s := range schemas.All() {
		if s.View == "files" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("schemas.All() missing files schema")
	}

	extKey, ok := schemas.Files.LookupKey("ext")
	if !ok {
		t.Fatal("Files schema missing ext key")
	}
	if extKey.Exec != filterquery.ExecClient {
		t.Errorf("ext key Exec = %v, want ExecClient", extKey.Exec)
	}
	if extKey.Kind != filterquery.KindString {
		t.Errorf("ext key Kind = %v, want KindString", extKey.Kind)
	}
	if extKey.MultiValued {
		t.Errorf("ext key MultiValued = true, want false")
	}
}

func TestFilesSchemaParseAndValidate(t *testing.T) {
	t.Run("pure-text regex stays verbatim", func(t *testing.T) {
		q, err := filterquery.Parse(`src/(foo|bar)\.go`, schemas.Files.HasKey)
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if q.Mode != filterquery.ModeText {
			t.Fatalf("mode = %v, want ModeText", q.Mode)
		}
		if q.Text != `src/(foo|bar)\.go` {
			t.Errorf("text = %q, want verbatim regex", q.Text)
		}
		if verr := schemas.Files.Validate(q); verr != nil {
			t.Errorf("unexpected validate error: %v", verr)
		}
	})

	t.Run("structured ext comma OR and text", func(t *testing.T) {
		q, err := filterquery.Parse(`ext:ts,tsx button -test`, schemas.Files.HasKey)
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if q.Mode != filterquery.ModeStructured {
			t.Fatalf("mode = %v, want ModeStructured", q.Mode)
		}
		if len(q.Terms) != 1 || q.Terms[0].Key != "ext" || q.Terms[0].Negate {
			t.Fatalf("unexpected terms: %+v", q.Terms)
		}
		if len(q.Terms[0].Values) != 2 || q.Terms[0].Values[0] != "ts" || q.Terms[0].Values[1] != "tsx" {
			t.Errorf("ext values = %v, want [ts tsx]", q.Terms[0].Values)
		}
		if q.Text != "button" {
			t.Errorf("text = %q, want %q", q.Text, "button")
		}
		if len(q.ExcludeText) != 1 || q.ExcludeText[0] != "test" {
			t.Errorf("excludeText = %v, want [test]", q.ExcludeText)
		}
		// Server-side Validate rejects client-only key ext.
		verr := schemas.Files.Validate(q)
		if verr == nil || verr.Code != filterquery.ErrCodeUnsupportedKey {
			t.Errorf("Validate(ext:) = %v, want ErrCodeUnsupportedKey", verr)
		}
	})

	t.Run("negated ext", func(t *testing.T) {
		q, err := filterquery.Parse(`-ext:md,txt`, schemas.Files.HasKey)
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if q.Mode != filterquery.ModeStructured || len(q.Terms) != 1 || !q.Terms[0].Negate {
			t.Fatalf("unexpected query: %+v", q)
		}
	})
}
