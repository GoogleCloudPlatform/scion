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

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasTopLevelOR(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want bool
	}{
		{
			name: "pre-8531 projectLabelNotContains postgres (violation)",
			sql:  `NOT ( __expr__ @> __arg__ ::jsonb) OR __expr__ IS NULL`,
			want: true,
		},
		{
			name: "post-8531 projectLabelNotContains postgres (parenthesized)",
			sql:  `(NOT ( __expr__ @> __arg__ ::jsonb) OR __expr__ IS NULL)`,
			want: false,
		},
		{
			name: "projectLabelNotContains sqlite (parenthesized)",
			sql:  `(json_extract( __expr__ , __arg__ ) IS NULL OR json_extract( __expr__ , __arg__ ) != __arg__ )`,
			want: false,
		},
		{
			name: "mixed AND and OR at top level",
			sql:  `a = 1 AND b = 2 OR c = 3`,
			want: true,
		},
		{
			name: "parenthesized branches joined by top-level OR",
			sql:  `(a = 1) OR (b = 2)`,
			want: true,
		},
		{
			name: "lowercase or at top level",
			sql:  `a = 1 or b = 2`,
			want: true,
		},
		{
			name: "keyset cursor with outer parens",
			sql:  `(( __expr__ < __arg__ AND __expr__ != __arg__ ) OR ( __expr__ = __arg__ AND __expr__ < __arg__ ))`,
			want: false,
		},
		{
			name: "OR inside parenthesized CASE WHEN",
			sql:  `(CASE WHEN __expr__ IS NULL OR __expr__ = '' THEN NULL ELSE __expr__ ::jsonb #>> '{a,b}' END) = __arg__`,
			want: false,
		},
		{
			name: "OR inside unparenthesized CASE WHEN",
			sql:  `CASE WHEN a = 1 OR b = 2 THEN 1 ELSE 0 END = 1`,
			want: false,
		},
		{
			name: "OR after CASE END at top level",
			sql:  `CASE WHEN a = 1 THEN 1 ELSE 0 END = 1 OR b = 2`,
			want: true,
		},
		{
			name: "pure top-level AND without OR",
			sql:  `__expr__ IS NOT NULL AND EXISTS (SELECT 1 FROM json_each( __expr__ ) WHERE value = __arg__ )`,
			want: false,
		},
		{
			name: "OR inside EXISTS subquery",
			sql:  `EXISTS (SELECT 1 FROM t WHERE a = 1 OR b = 2)`,
			want: false,
		},
		{
			name: "OR inside single-quoted string literal",
			sql:  `status = 'pending OR active'`,
			want: false,
		},
		{
			name: "OR inside single-quoted string with escaped quote",
			sql:  `status = 'it''s OR nothing'`,
			want: false,
		},
		{
			name: "OR inside double-quoted identifier",
			sql:  `"or" = 1`,
			want: false,
		},
		{
			name: "OR inside line comment",
			sql:  "a = 1 -- or b = 2\n",
			want: false,
		},
		{
			name: "OR inside block comment",
			sql:  `a = 1 /* OR b = 2 */`,
			want: false,
		},
		{
			name: "identifier containing or substring",
			sql:  `order_id = 1 AND for_update = 0`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HasTopLevelOR(tc.sql)
			if got != tc.want {
				t.Errorf("HasTopLevelOR(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}

func TestAnalyzeFiles_PositiveAndNegative(t *testing.T) {
	dir := t.TempDir()
	src := `package sample

import (
	"fmt"

	entsql "entgo.io/ent/dialect/sql"
)

func badChained(col, key, value string) func(*entsql.Selector) {
	return func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("NOT (").
				WriteString(col).
				WriteString(" @> ").
				Arg(fmt.Sprintf("{%q:%q}", key, value)).
				WriteString("::jsonb)").
				WriteString(" OR ").
				WriteString(col).
				WriteString(" IS NULL")
		}))
	}
}

func goodParenthesized(col, key, value string) func(*entsql.Selector) {
	return func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("(NOT (").
				WriteString(col).
				WriteString(" @> ").
				Arg(fmt.Sprintf("{%q:%q}", key, value)).
				WriteString("::jsonb) OR ").
				WriteString(col).
				WriteString(" IS NULL)")
		}))
	}
}

func badExprP(col string) func(*entsql.Selector) {
	return func(s *entsql.Selector) {
		s.Where(entsql.ExprP(col + " = 'a' OR " + col + " = 'b'"))
	}
}

func goodExprP(col string) func(*entsql.Selector) {
	return func(s *entsql.Selector) {
		s.Where(entsql.ExprP("(" + col + " = 'a' OR " + col + " = 'b')"))
	}
}

func badWriteOp(col string) func(*entsql.Selector) {
	return func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.Ident(col).WriteOp(entsql.OpIsNull)
			b.WriteOp(entsql.OpOr)
			b.Ident(col).WriteOp(entsql.OpEQ).Arg("x")
		}))
	}
}

func writeBadHelper(b *entsql.Builder) {
	b.WriteString("a = 1 OR b = 2")
}

func badViaHelper() func(*entsql.Selector) {
	return func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			writeBadHelper(b)
		}))
	}
}

func goodPureAnd(col string) func(*entsql.Selector) {
	return func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString(col).WriteString(" IS NOT NULL AND EXISTS (SELECT 1 FROM t WHERE a = 1 OR b = 2)")
		}))
	}
}

type S struct{}

func (s *S) BadMethod(col string) func(*entsql.Selector) {
	return func(sel *entsql.Selector) {
		sel.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString(col).WriteString(" = 1 OR ").WriteString(col).WriteString(" = 2")
		}))
	}
}
`
	filePath := filepath.Join(dir, "sample.go")
	if err := os.WriteFile(filePath, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	findings, analysed, err := AnalyzeFiles([]string{filePath})
	if err != nil {
		t.Fatalf("AnalyzeFiles error: %v", err)
	}
	if analysed != 1 {
		t.Fatalf("analysed = %d, want 1", analysed)
	}

	wantFuncs := map[string]bool{
		"badChained":   true,
		"badExprP":     true,
		"badWriteOp":   true,
		"badViaHelper": true,
		"S.BadMethod":  true,
	}
	if len(findings) != len(wantFuncs) {
		for _, f := range findings {
			t.Logf("finding: %s", f)
		}
		t.Fatalf("got %d findings, want %d", len(findings), len(wantFuncs))
	}
	for _, f := range findings {
		if !wantFuncs[f.Func] {
			t.Errorf("unexpected finding in func %q: %s", f.Func, f)
		}
		if f.Rule != ruleTopLevelOR {
			t.Errorf("unexpected rule %q in %s", f.Rule, f)
		}
		delete(wantFuncs, f.Func)
	}
	for fn := range wantFuncs {
		t.Errorf("expected finding in func %q was not reported", fn)
	}
}

func TestAllowlist_ValidAndStale(t *testing.T) {
	dir := t.TempDir()
	alPath := filepath.Join(dir, "allowlist.txt")
	content := `# test allowlist
pkg/foo/bar.go | badFunc | top-level-or | intentional test exemption
pkg/foo/bar.go | staleFunc | top-level-or | no longer present
`
	if err := os.WriteFile(alPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := ParseAllowlist(alPath)
	if err != nil {
		t.Fatalf("ParseAllowlist: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	findings := []Finding{
		{File: "pkg/foo/bar.go", Func: "badFunc", Rule: ruleTopLevelOR},
		{File: "pkg/foo/bar.go", Func: "unlistedFunc", Rule: ruleTopLevelOR},
	}
	violations, stale := FilterFindings(findings, entries)
	if len(violations) != 1 || violations[0].Func != "unlistedFunc" {
		t.Fatalf("unexpected violations: %+v", violations)
	}
	if len(stale) != 1 || stale[0].Func != "staleFunc" {
		t.Fatalf("unexpected stale entries: %+v", stale)
	}
}

func TestAllowlist_Malformed(t *testing.T) {
	dir := t.TempDir()
	alPath := filepath.Join(dir, "bad.txt")
	if err := os.WriteFile(alPath, []byte("pkg/a.go | fn | top-level-or\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseAllowlist(alPath); err == nil {
		t.Fatal("expected error for malformed allowlist entry, got nil")
	}
}

func TestRun_NoCandidateFilesExits4(t *testing.T) {
	dir := t.TempDir()
	rc := run([]string{"-allowlist", "", dir})
	if rc != 4 {
		t.Fatalf("run on empty dir = %d, want 4", rc)
	}
}
