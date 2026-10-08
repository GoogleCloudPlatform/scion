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

package critic

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestProjections(t *testing.T) {
	cases := []struct {
		name, in, clean, accept string
	}{
		{"plain", "no marks here", "no marks here", "no marks here"},
		{"empty", "", "", ""},
		{"insertion", "a {++big ++}dog", "a dog", "a big dog"},
		{"deletion", "a {--big --}dog", "a big dog", "a dog"},
		{"substitution", "a {~~cat~>dog~~}!", "a cat!", "a dog!"},
		{"comment", "text{>>why?<<} more", "text more", "text more"},
		{"highlight", "{==key==} point", "key point", "key point"},
		{"highlight comment pair", "a {==b==}{>>note<<} c", "a b c", "a b c"},
		{"adjacent marks", "{++a++}{--b--}{~~c~>d~~}", "bc", "ad"},
		{"empty insertion", "x{++++}y", "xy", "xy"},
		{"empty deletion", "x{----}y", "xy", "xy"},
		{"empty comment", "x{>><<}y", "xy", "xy"},
		{"substitution empty old", "x{~~~>new~~}y", "xy", "xnewy"},
		{"substitution empty new", "x{~~old~>~~}y", "xoldy", "xy"},
		{"substitution both empty", "x{~~~>~~}y", "xy", "xy"},
		{"substitution first separator wins", "{~~a~>b~>c~~}", "a", "b~>c"},
		{"substitution without separator is literal", "{~~old~~}", "{~~old~~}", "{~~old~~}"},
		{"malformed substitution then valid one", "{~~a~~}{~~b~>c~~}", "{~~a~~}b", "{~~a~~}c"},
		{"opener needs doubled char", "{+-a++}", "{+-a++}", "{+-a++}"},
		{"substitution separator after closer is literal", "{~~a~~} b~>c", "{~~a~~} b~>c", "{~~a~~} b~>c"},
		{"unterminated insertion is literal", "a {++b c", "a {++b c", "a {++b c"},
		{"unterminated then valid", "{++a {--b--}", "{++a b", "{++a "},
		{"unterminated comment", "x {>>note", "x {>>note", "x {>>note"},
		{"wrong closer is literal", "{++a--}", "{++a--}", "{++a--}"},
		{"nested opener is literal content", "{++a {++b++} c++}", " c++}", "a {++b c++}"},
		{"other opener inside is content", "{++a {--b--} c++}", "", "a {--b--} c"},
		{"braces inside marks", "{++f(){ return {} }++}", "", "f(){ return {} }"},
		{"lone braces", "{ } {{ }} {+ +} {-}", "{ } {{ }} {+ +} {-}", "{ } {{ }} {+ +} {-}"},
		{"mixed opener chars", "{+-a-+}", "{+-a-+}", "{+-a-+}"},
		{"spans newlines", "a{++b\n\nc++}d", "ad", "ab\n\ncd"},
		{"deletion spans newline", "a{--b\nc--}d", "ab\ncd", "ad"},
		{"crlf preserved", "a\r\n{++b\r\n++}c\r\n", "a\r\nc\r\n", "a\r\nb\r\nc\r\n"},
		{"code span not special", "`{++x++}`", "``", "`x`"},
		{"fence not special", "```\n{--x--}\n```", "```\nx\n```", "```\n\n```"},
		{"unicode around marks", "café {~~naïve~>naive~~} 日本", "café naïve 日本", "café naive 日本"},
		{"trailing opener", "abc{++", "abc{++", "abc{++"},
		{"trailing brace", "abc{", "abc{", "abc{"},
		{"closer without opener", "a ++} b <<}", "a ++} b <<}", "a ++} b <<}"},
		{"overlapping closer chars", "{+++}", "{+++}", "{+++}"},
		{"four plus empty insertion", "{++++}", "", ""},
		{"comment contains closer of other kind", "{>>a ++} b<<}", "", ""},
		{"spec example", "Lorem {++ipsum ++}dolor{-- sit--} amet {~~is~>are~~} {==here==}{>>yes<<}.",
			"Lorem dolor sit amet is here.", "Lorem ipsum dolor amet are here."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(CleanText([]byte(c.in))); got != c.clean {
				t.Errorf("Clean(%q) = %q, want %q", c.in, got, c.clean)
			}
			if got := string(AcceptText([]byte(c.in))); got != c.accept {
				t.Errorf("Accept(%q) = %q, want %q", c.in, got, c.accept)
			}
			if got := string(Project([]byte(c.in), Raw)); got != c.in {
				t.Errorf("Raw(%q) = %q", c.in, got)
			}
			assertCovers(t, []byte(c.in))
		})
	}
}

// assertCovers checks that segments tile src exactly, in order.
func assertCovers(t *testing.T, src []byte) {
	t.Helper()
	pos := 0
	for _, s := range Parse(src) {
		if s.Start != pos || s.End <= s.Start || s.End > len(src) {
			t.Fatalf("segment %+v does not continue at %d (len %d)", s, pos, len(src))
		}
		pos = s.End
	}
	if pos != len(src) {
		t.Fatalf("segments end at %d, want %d", pos, len(src))
	}
}

func TestParseSegments(t *testing.T) {
	segs := Parse([]byte("a{~~x~>y~~}{>>c<<}"))
	if len(segs) != 3 {
		t.Fatalf("got %d segments: %+v", len(segs), segs)
	}
	if segs[0].Kind != Text || string(segs[0].Text) != "a" {
		t.Errorf("seg0 = %+v", segs[0])
	}
	if segs[1].Kind != Substitution || string(segs[1].Text) != "x" || string(segs[1].New) != "y" || segs[1].Start != 1 || segs[1].End != 11 {
		t.Errorf("seg1 = %+v", segs[1])
	}
	if segs[2].Kind != Comment || string(segs[2].Text) != "c" {
		t.Errorf("seg2 = %+v", segs[2])
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": Raw, "raw": Raw, "clean": Clean, "accept": Accept} {
		if got, ok := ParseMode(in); !ok || got != want {
			t.Errorf("ParseMode(%q) = %v, %v", in, got, ok)
		}
	}
	if _, ok := ParseMode("Clean"); ok {
		t.Error("ParseMode is case-sensitive")
	}
}

// TestParseLinearWork defends the linear cost bound: inputs full of
// unterminated or malformed marks must not make Parse rescan the tail once
// per opener. A per-opener rescan examines O(n^2) bytes and fails here.
func TestParseLinearWork(t *testing.T) {
	const n = 1 << 14
	inputs := map[string]string{
		"unterminated insertions":  strings.Repeat("{++", n),
		"unterminated each kind":   strings.Repeat("{++{--{~~{>>{==", n/5),
		"substitutions no sep":     strings.Repeat("{~~a", n) + "~~}",
		"sep only far away":        strings.Repeat("{~~a~~}", n) + "~>",
		"many closers few openers": strings.Repeat("++}", n),
		"openers then one closer":  strings.Repeat("{++a", n) + "++}",
		"interleaved malformed":    strings.Repeat("{~~x~~}{++", n/2),
		"long text":                strings.Repeat("abcdefgh", n),
		"brace soup":               strings.Repeat("{{{{+-~>=", n),
		"valid marks":              strings.Repeat("{++a++}{--b--}{~~c~>d~~}{>>e<<}{==f==}", n/16),
	}
	for name, in := range inputs {
		t.Run(name, func(t *testing.T) {
			src := []byte(in)
			_, steps := parse(src)
			// Each byte is examined by the '{' scan and by at most each of
			// the six cursors, plus a token length of overlap per search.
			if limit := 16 * (len(src) + 1); steps > limit {
				t.Fatalf("parse examined %d bytes for input of %d (limit %d)", steps, len(src), limit)
			}
		})
	}
}

// TestRandomComposition composes random fragments and checks invariants
// that hold for every input: segments tile the input, projections never
// grow it, Clean of Accept-free text is identity, and the projections agree
// with a reference built from the segment list.
func TestRandomComposition(t *testing.T) {
	frags := []string{"{++", "++}", "{--", "--}", "{~~", "~>", "~~}", "{>>", "<<}", "{==", "==}",
		"{", "}", "+", "-", "~", ">", "<", "=", "a", "é", "\n", "\r\n", " ", "x y"}
	r := rand.New(rand.NewSource(1))
	for iter := 0; iter < 3000; iter++ {
		var b strings.Builder
		for k := r.Intn(40); k > 0; k-- {
			b.WriteString(frags[r.Intn(len(frags))])
		}
		src := []byte(b.String())
		assertCovers(t, src)
		c, a := CleanText(src), AcceptText(src)
		if len(c) > len(src) || len(a) > len(src) {
			t.Fatalf("projection grew %q", src)
		}
		var rc, ra []byte
		for _, s := range Parse(src) {
			whole := src[s.Start:s.End]
			switch s.Kind {
			case Text:
				if !bytes.Equal(whole, s.Text) {
					t.Fatalf("text segment content differs from source in %q", src)
				}
				rc, ra = append(rc, whole...), append(ra, whole...)
			case Insertion:
				ra = append(ra, s.Text...)
			case Deletion:
				rc = append(rc, s.Text...)
			case Substitution:
				rc, ra = append(rc, s.Text...), append(ra, s.New...)
			case Highlight:
				rc, ra = append(rc, s.Text...), append(ra, s.Text...)
			}
		}
		if !bytes.Equal(c, rc) || !bytes.Equal(a, ra) {
			t.Fatalf("projection mismatch for %q", src)
		}
		// Text with no '{' has no marks: every projection is identity.
		if !bytes.ContainsRune(src, '{') && (!bytes.Equal(c, src) || !bytes.Equal(a, src)) {
			t.Fatalf("mark-free text changed: %q", src)
		}
		// Re-projecting a clean text with no remaining marks is stable.
		if cc := CleanText(c); len(Parse(c)) <= 1 && !bytes.Equal(cc, c) {
			t.Fatalf("clean not stable for %q", src)
		}
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"a\r\nb":     "a\nb",
		"a\rb":       "a\nb",
		"a\r\r\nb":   "a\n\nb",
		"cafe\u0301": "café",
		"café\r\n":   "café\n",
		"":           "",
		"plain\n":    "plain\n",
		"\r":         "\n",
	}
	for in, want := range cases {
		if got := string(Normalize([]byte(in))); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiff(t *testing.T) {
	if h := Diff([]byte("same\n"), []byte("same\n")); h != nil {
		t.Fatalf("equal texts gave %+v", h)
	}
	h := Diff([]byte("a\nb\nc\nd\n"), []byte("a\nB\nc\nD\n"))
	if len(h) != 2 || h[0].Line != 2 || h[0].Parent != "b\n" || h[0].Clean != "B\n" || h[1].Line != 4 {
		t.Fatalf("in-place edits: %+v", h)
	}
	h = Diff([]byte("a\nb\nc\n"), []byte("a\nx\ny\nc\n"))
	if len(h) != 1 || h[0].Line != 2 || h[0].ParentLines != 1 || h[0].CleanLines != 2 || h[0].Clean != "x\ny\n" {
		t.Fatalf("insertion: %+v", h)
	}
	h = Diff([]byte("a\n"), []byte("a"))
	if len(h) != 1 || h[0].Line != 1 {
		t.Fatalf("missing final newline: %+v", h)
	}
	h = Diff(nil, []byte("new\n"))
	if len(h) != 1 || h[0].ParentLines != 0 || h[0].Clean != "new\n" {
		t.Fatalf("from empty: %+v", h)
	}
}

// TestDiffBounds defends MaxHunks and MaxExcerptBytes: removing either
// check makes this fail.
func TestDiffBounds(t *testing.T) {
	var p, c strings.Builder
	for i := 0; i < 3*MaxHunks; i++ {
		p.WriteString("same\nold\n")
		c.WriteString("same\nnew\n")
	}
	h := Diff([]byte(p.String()), []byte(c.String()))
	if len(h) != MaxHunks || !h[len(h)-1].Truncated {
		t.Fatalf("got %d hunks (truncated=%v), want %d truncated", len(h), len(h) > 0 && h[len(h)-1].Truncated, MaxHunks)
	}
	// A one-byte prefix puts every rune boundary on an odd offset, so the
	// byte cap falls inside a two-byte rune and must back off by one.
	long := "x" + strings.Repeat("é", MaxExcerptBytes)
	h = Diff([]byte("y\n"), []byte(long+"\n"))
	if len(h) != 1 || len(h[0].Clean) > MaxExcerptBytes || !h[0].Truncated {
		t.Fatalf("excerpt %d bytes, truncated=%v", len(h[0].Clean), h[0].Truncated)
	}
	if !strings.HasPrefix(long, h[0].Clean) || !utf8.ValidString(h[0].Clean) || len(h[0].Clean) != MaxExcerptBytes-1 {
		t.Fatalf("excerpt cut inside a rune: %d bytes", len(h[0].Clean))
	}
	many := strings.Repeat("line\n", 1000)
	h = Diff(nil, []byte(many))
	if len(h) != 1 || len(h[0].Clean) > MaxExcerptBytes || !h[0].Truncated {
		t.Fatalf("multi-line excerpt %d bytes, truncated=%v", len(h[0].Clean), h[0].Truncated)
	}
}

// TestReviewInvariant: for a text made only of marks added to a parent,
// Clean reproduces the parent; an edit outside marks is detected.
func TestReviewInvariant(t *testing.T) {
	parent := "The plan ships in Q3.\nOwners: docs team.\n"
	review := "The plan {~~ships~>launches~~} in Q3.{>>confirm date<<}\nOwners: {==docs team==}{>>which one?<<}.{++\nRisks: none.++}\n"
	if got := Normalize(CleanText([]byte(review))); !bytes.Equal(got, Normalize([]byte(parent))) {
		t.Fatalf("clean(review) = %q", got)
	}
	edited := strings.Replace(review, "Owners", "Owner", 1)
	h := Diff(Normalize([]byte(parent)), Normalize(CleanText([]byte(edited))))
	if len(h) != 1 || h[0].Line != 2 {
		t.Fatalf("unmarked edit: %+v", h)
	}
	crlf := strings.ReplaceAll(review, "\n", "\r\n")
	if got := Normalize(CleanText([]byte(crlf))); !bytes.Equal(got, Normalize([]byte(parent))) {
		t.Fatalf("CRLF review not equal after normalisation: %q", got)
	}
}
