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

package celengine

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/overloads"
	"cel.dev/cel-go/common/types"
)

// Go regexp/syntax case-folding rune range [minFold, maxFold]. Under (?i),
// regexp/syntax's appendFoldedRange iterates rune-by-rune across character
// class ranges intersecting [re2MinFoldRune, re2MaxFoldRune] at parse time,
// producing a 3-instruction RE2 program that RegexProgramSizeLimit cannot see.
const (
	re2MinFoldRune rune = 0x0041
	re2MaxFoldRune rune = 0x1E943
)

var errRegexLiteralRequired = errors.New("matches requires a string literal pattern")

// precompiledRegex holds an immutable compiled RE2 pattern and its instruction
// count, built once during Engine.Compile and kept with the cached Program so
// evaluation never parses a regular expression.
type precompiledRegex struct {
	re       *regexp.Regexp
	progSize int
}

// validateAndCompileMatchesAST enforces Option A at the post-macro AST level
// before celEnv.Check runs:
//  1. Every receiver-style s.matches(pat) and global matches(s, pat) (including
//     leading-dot .matches(s, pat)) must pass a constant string literal pattern
//     after macro expansion. Any non-literal pattern expression fails with
//     CategoryRegexLiteralRequired at the pattern argument's 1-based source
//     position.
//  2. Every string literal pattern passes the linear O(len(pat)) pre-scan
//     (MaxRegexPatternBytes and MaxRegexFoldedRangeRunes, bounded both per
//     pattern and cumulatively across the expression) before any regexp/syntax
//     parse runs.
//  3. Every valid string literal pattern is compiled once into a *regexp.Regexp
//     bounded by maxRegexProgramSize and returned for storage on Program.
func validateAndCompileMatchesAST(parsed *cel.Ast, kind Kind, maxRegexProgramSize int) (map[string]precompiledRegex, *CompileError) {
	if parsed == nil {
		return nil, nil
	}
	rep := parsed.NativeRep()
	if rep == nil {
		return nil, nil
	}
	srcInfo := rep.SourceInfo()

	calls := celast.MatchDescendants(celast.NavigateAST(rep), func(e celast.NavigableExpr) bool {
		if e.Kind() != celast.CallKind {
			return false
		}
		return strings.TrimPrefix(e.AsCall().FunctionName(), ".") == overloads.Matches
	})
	if len(calls) == 0 {
		return nil, nil
	}

	type callPos struct {
		expr celast.NavigableExpr
		line int
		col  int
	}
	ordered := make([]callPos, 0, len(calls))
	for _, c := range calls {
		line, col := exprStartLocation(srcInfo, c.ID())
		ordered = append(ordered, callPos{expr: c, line: line, col: col})
	}
	slices.SortFunc(ordered, func(a, b callPos) int {
		if c := cmp.Compare(a.line, b.line); c != 0 {
			return c
		}
		if c := cmp.Compare(a.col, b.col); c != 0 {
			return c
		}
		return cmp.Compare(a.expr.ID(), b.expr.ID())
	})

	if maxRegexProgramSize <= 0 {
		maxRegexProgramSize = DefaultMaxRegexProgramSize
	}
	compiled := make(map[string]precompiledRegex, len(ordered))
	totalFoldedRunes := 0
	for _, item := range ordered {
		call := item.expr.AsCall()
		args := call.Args()
		var patExpr celast.Expr
		switch {
		case call.IsMemberFunction() && len(args) == 1:
			patExpr = args[0]
		case !call.IsMemberFunction() && len(args) == 2:
			patExpr = args[1]
		default:
			// Wrong arity has no matching CEL matches overload; celEnv.Check
			// rejects it as CategoryType without invoking regexp/syntax.
			continue
		}

		line, col := exprStartLocation(srcInfo, patExpr.ID())
		if patExpr.Kind() != celast.LiteralKind {
			return nil, &CompileError{
				Line:     line,
				Column:   col,
				Message:  errRegexLiteralRequired.Error(),
				Kind:     kind.Name,
				Category: CategoryRegexLiteralRequired,
			}
		}

		strLit, ok := patExpr.AsLiteral().(types.String)
		if !ok {
			// Non-string literal (e.g. int/bool/null/bytes): celEnv.Check reports
			// overload mismatch as CategoryType without invoking regexp/syntax.
			continue
		}
		pat := string(strLit)

		var err error
		totalFoldedRunes, err = preScanRegexLiteral(pat, totalFoldedRunes)
		if err != nil {
			return nil, &CompileError{
				Line:     line,
				Column:   col,
				Message:  err.Error(),
				Kind:     kind.Name,
				Category: CategorySize,
			}
		}

		if _, exists := compiled[pat]; !exists {
			progSize, err := types.RegexProgramSize(pat)
			if err != nil {
				return nil, &CompileError{
					Line:     line,
					Column:   col,
					Message:  err.Error(),
					Kind:     kind.Name,
					Category: CategoryType,
				}
			}
			if progSize > maxRegexProgramSize {
				return nil, &CompileError{
					Line:     line,
					Column:   col,
					Message:  fmt.Sprintf("regex program size %d exceeds limit of %d", progSize, maxRegexProgramSize),
					Kind:     kind.Name,
					Category: CategorySize,
				}
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, &CompileError{
					Line:     line,
					Column:   col,
					Message:  err.Error(),
					Kind:     kind.Name,
					Category: CategoryType,
				}
			}
			compiled[pat] = precompiledRegex{re: re, progSize: progSize}
		}
	}
	return compiled, nil
}

func exprStartLocation(srcInfo *celast.SourceInfo, id int64) (int, int) {
	if srcInfo == nil || id == 0 {
		return 1, 1
	}
	return oneBasedPos(srcInfo.GetStartLocation(id))
}

// preScanRegexLiteral bounds regexp/syntax parse cost in strictly O(len(pat))
// time before any regexp/syntax.Parse or regexp.Compile call:
//   - rejects len(pat) > MaxRegexPatternBytes;
//   - whenever (?i) can be active anywhere in pat, sums the codepoint span of
//     every character-class [lo-hi] range intersecting [0x41, 0x1E943] and
//     rejects once cumulative folded runes exceed MaxRegexFoldedRangeRunes.
func preScanRegexLiteral(pat string, priorFoldedRunes int) (int, error) {
	if len(pat) > MaxRegexPatternBytes {
		return priorFoldedRunes, fmt.Errorf(
			"regex pattern length %d exceeds maximum %d bytes",
			len(pat), MaxRegexPatternBytes,
		)
	}
	if !hasCaseFoldDirective(pat) {
		return priorFoldedRunes, nil
	}

	foldedRunes := priorFoldedRunes
	t := pat
	for t != "" {
		switch t[0] {
		case '\\':
			if len(t) >= 2 && t[1] == 'Q' {
				// \Q ... \E treats everything up to \E (or end of pattern) as literals.
				_, after, found := strings.Cut(t[2:], `\E`)
				if !found {
					return foldedRunes, nil
				}
				t = after
				continue
			}
			t = skipOutsideRegexEscape(t)
		case '[':
			var err error
			t, foldedRunes, err = scanCharClassFoldedRunes(t, foldedRunes)
			if err != nil {
				return foldedRunes, err
			}
		default:
			_, sz := utf8.DecodeRuneInString(t)
			if sz <= 0 {
				sz = 1
			}
			t = t[sz:]
		}
	}
	return foldedRunes, nil
}

// hasCaseFoldDirective conservatively reports whether pat contains any (?...)
// Perl flag specification that can enable FoldCase ('i' before '-', ':', or ')').
// In Go's regexp/syntax, FoldCase is off initially and can only be enabled by a
// raw "(?" sequence parsed by parsePerlFlags.
func hasCaseFoldDirective(pat string) bool {
	s := pat
	for {
		idx := strings.Index(s, "(?")
		if idx < 0 {
			return false
		}
		rest := s[idx+2:]
		s = rest
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case 'i':
				return true
			case 'm', 's', 'U':
				continue
			default:
				// Any '-', ':', ')', 'P', '<', or invalid flag ends the positive-flag
				// section of this "(?" group ('i' after '-' clears FoldCase).
				i = len(rest)
			}
		}
	}
}

func skipOutsideRegexEscape(s string) string {
	// Caller guarantees s != "" && s[0] == '\\' && not \Q.
	if len(s) < 2 {
		return ""
	}
	switch s[1] {
	case 'A', 'b', 'B', 'C', 'z', 'd', 'D', 's', 'S', 'w', 'W':
		return s[2:]
	case 'p', 'P':
		return skipUnicodePropEscape(s)
	default:
		if _, rest, ok := scanRegexEscape(s); ok {
			return rest
		}
		// Malformed escape: advance past '\' and one rune so scanning stays O(N).
		_, sz := utf8.DecodeRuneInString(s[1:])
		if sz <= 0 {
			sz = 1
		}
		return s[1+sz:]
	}
}

func skipUnicodePropEscape(s string) string {
	// Caller guarantees len(s) >= 2 && s[0] == '\\' && (s[1] == 'p' || s[1] == 'P').
	t := s[2:]
	if t == "" {
		return ""
	}
	c, rest := nextScanRune(t)
	if c != '{' {
		return rest
	}
	if end := strings.IndexByte(rest, '}'); end >= 0 {
		return rest[end+1:]
	}
	return ""
}

// scanCharClassFoldedRunes mirrors regexp/syntax.(*parser).parseClass on s
// (where s[0] == '[') and adds the intersection of every [lo-hi] range with
// [re2MinFoldRune, re2MaxFoldRune] to foldedRunes.
func scanCharClassFoldedRunes(s string, foldedRunes int) (string, int, error) {
	t := s[1:] // chop '['
	if t != "" && t[0] == '^' {
		t = t[1:]
	}
	first := true
	for t != "" && (t[0] != ']' || first) {
		first = false

		// POSIX named class [:alnum:], [:^alpha:], etc. (fixed small ASCII tables).
		// Just like regexp/syntax.parseNamedClass, this only triggers when closing ":]" exists.
		if len(t) > 2 && t[0] == '[' && t[1] == ':' {
			if end := strings.Index(t[2:], ":]"); end >= 0 {
				t = t[end+4:]
				continue
			}
		}

		// Unicode property group \pL or \p{Greek} (uses RangeTable, never appendFoldedRange).
		if len(t) >= 2 && t[0] == '\\' && (t[1] == 'p' || t[1] == 'P') {
			t = skipUnicodePropEscape(t)
			continue
		}

		// Perl character class escape \d, \D, \s, \S, \w, \W (fixed small ASCII tables).
		if len(t) >= 2 && t[0] == '\\' {
			switch t[1] {
			case 'd', 'D', 's', 'S', 'w', 'W':
				t = t[2:]
				continue
			}
		}

		lo, rest, ok := scanClassChar(t)
		if !ok {
			// Invalid escape inside class: regexp/syntax aborts parse immediately.
			return rest, foldedRunes, nil
		}
		t = rest
		hi := lo
		if len(t) >= 2 && t[0] == '-' && t[1] != ']' {
			t = t[1:]
			hi, rest, ok = scanClassChar(t)
			if !ok {
				return rest, foldedRunes, nil
			}
			t = rest
			if hi < lo {
				return t, foldedRunes, nil
			}
		}

		// Single-rune literals (lo == hi) do 1 SimpleFold loop; ranges (lo < hi)
		// intersecting [0x41, 0x1E943] iterate across every rune in the overlap.
		if hi > lo {
			overlapLo := max(lo, re2MinFoldRune)
			overlapHi := min(hi, re2MaxFoldRune)
			if overlapLo <= overlapHi {
				span := int(overlapHi-overlapLo) + 1
				foldedRunes += span
				if foldedRunes > MaxRegexFoldedRangeRunes {
					return "", foldedRunes, fmt.Errorf(
						"case-folded regex character class range span %d exceeds maximum %d runes",
						foldedRunes, MaxRegexFoldedRangeRunes,
					)
				}
			}
		}
	}
	if t != "" && t[0] == ']' {
		t = t[1:]
	}
	return t, foldedRunes, nil
}

func scanClassChar(s string) (rune, string, bool) {
	if s == "" {
		return 0, "", false
	}
	if s[0] == '\\' {
		return scanRegexEscape(s)
	}
	r, sz := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && sz == 1 {
		return 0, s[1:], false
	}
	return r, s[sz:], true
}

// scanRegexEscape mirrors regexp/syntax.(*parser).parseEscape.
func scanRegexEscape(s string) (rune, string, bool) {
	t := s[1:]
	if t == "" {
		return 0, "", false
	}
	c, sz := utf8.DecodeRuneInString(t)
	if c == utf8.RuneError && sz == 1 {
		return 0, t[1:], false
	}
	t = t[sz:]

	switch c {
	case '1', '2', '3', '4', '5', '6', '7':
		if t == "" || t[0] < '0' || t[0] > '7' {
			return 0, t, false
		}
		fallthrough
	case '0':
		r := c - '0'
		for i := 1; i < 3; i++ {
			if t == "" || t[0] < '0' || t[0] > '7' {
				break
			}
			r = r*8 + rune(t[0]-'0')
			t = t[1:]
		}
		return r, t, true

	case 'x':
		if t == "" {
			return 0, "", false
		}
		ch, csz := utf8.DecodeRuneInString(t)
		if ch == utf8.RuneError && csz == 1 {
			return 0, t[1:], false
		}
		t = t[csz:]
		if ch == '{' {
			nhex := 0
			var r rune
			for {
				if t == "" {
					return 0, "", false
				}
				hc, hsz := utf8.DecodeRuneInString(t)
				if hc == utf8.RuneError && hsz == 1 {
					return 0, t[1:], false
				}
				t = t[hsz:]
				if hc == '}' {
					break
				}
				v := unhexRune(hc)
				if v < 0 {
					return 0, t, false
				}
				r = r*16 + v
				if r > unicode.MaxRune {
					return 0, t, false
				}
				nhex++
			}
			if nhex == 0 {
				return 0, t, false
			}
			return r, t, true
		}
		x := unhexRune(ch)
		if t == "" {
			return 0, "", false
		}
		ch2, csz2 := utf8.DecodeRuneInString(t)
		if ch2 == utf8.RuneError && csz2 == 1 {
			return 0, t[1:], false
		}
		t = t[csz2:]
		y := unhexRune(ch2)
		if x < 0 || y < 0 {
			return 0, t, false
		}
		return x*16 + y, t, true

	case 'a':
		return '\a', t, true
	case 'f':
		return '\f', t, true
	case 'n':
		return '\n', t, true
	case 'r':
		return '\r', t, true
	case 't':
		return '\t', t, true
	case 'v':
		return '\v', t, true
	default:
		if c < utf8.RuneSelf && !isASCIIAlnum(c) {
			return c, t, true
		}
		return 0, t, false
	}
}

func nextScanRune(s string) (rune, string) {
	c, sz := utf8.DecodeRuneInString(s)
	if sz <= 0 {
		return c, s[1:]
	}
	return c, s[sz:]
}

func unhexRune(c rune) rune {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10
	default:
		return -1
	}
}

func isASCIIAlnum(c rune) bool {
	return ('0' <= c && c <= '9') || ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z')
}
