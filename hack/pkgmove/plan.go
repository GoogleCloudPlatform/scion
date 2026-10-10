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
	"fmt"
	"io"
	"sort"
	"strings"
)

// Config holds the inputs of one move.
type Config struct {
	SrcDir           string   // source package directory
	DstDir           string   // target package directory (must hold no Go files yet)
	PkgName          string   // target package name (default: base of DstDir)
	Area             string   // alias file name stem: zz_alias_<Area>.go
	Files            []string // files to move (base names or paths inside SrcDir)
	Tags             []string // build tags for the analysis
	DryRun           bool
	Vet              bool // run go vet on both packages afterwards (off by default)
	NoGit            bool // use os.Rename instead of git mv
	Typecheck        bool // re-type-check both packages after the rewrite
	AllowFieldExport bool // allow exporting struct fields (changes reflection/encoding visibility)
	ReportPath       string
	Stdout           io.Writer
}

// Severity levels of safety findings, most severe first.
const (
	levelError = "ERROR"
	levelHigh  = "HIGH"
	levelWarn  = "WARN"
	levelInfo  = "INFO"
)

var levelRank = map[string]int{levelError: 0, levelHigh: 1, levelWarn: 2, levelInfo: 3}

type finding struct {
	Level    string
	Category string
	Pos      string // module-relative file:line, or ""
	Msg      string
}

type fileMove struct {
	From, To string // module-relative paths
}

type renameEntry struct {
	Kind     string // type, func, const, var, field, method
	Owner    string // receiver/owner for members, "" for package-level
	Old, New string
	Pos      string
}

type aliasEntry struct {
	Kind       string // type, const, func, var
	Old, New   string
	Test       bool
	Constraint string
	Decl       string // rendered declaration (filled in by the renderer)
}

type varRewrite struct {
	Pos      string
	Old, New string
}

// Plan is the deterministic outcome of the analysis.
type Plan struct {
	SrcImport, DstImport string
	PkgName              string
	Moves                []fileMove
	Renames              []renameEntry
	Aliases              []aliasEntry
	AliasFiles           []generatedFile
	VarRewrites          []varRewrite
	TouchedFiles         []string // remaining source files edited (module-relative)
	Findings             []finding
	Errors               []string
}

type generatedFile struct {
	Path    string // module-relative
	Content []byte
}

func (p *Plan) errorf(format string, args ...any) {
	p.Errors = append(p.Errors, fmt.Sprintf(format, args...))
}

func (p *Plan) add(level, category, pos, format string, args ...any) {
	p.Findings = append(p.Findings, finding{Level: level, Category: category, Pos: pos, Msg: fmt.Sprintf(format, args...)})
}

// normalize sorts and de-duplicates every list so output is deterministic.
func (p *Plan) normalize() {
	sort.Slice(p.Moves, func(i, j int) bool { return p.Moves[i].From < p.Moves[j].From })
	sort.Slice(p.Renames, func(i, j int) bool {
		a, b := p.Renames[i], p.Renames[j]
		if a.Owner != b.Owner {
			return a.Owner < b.Owner
		}
		if a.Old != b.Old {
			return a.Old < b.Old
		}
		return a.Kind < b.Kind
	})
	sort.Slice(p.Aliases, func(i, j int) bool {
		a, b := p.Aliases[i], p.Aliases[j]
		if a.Test != b.Test {
			return !a.Test
		}
		if a.Constraint != b.Constraint {
			return a.Constraint < b.Constraint
		}
		return a.Old < b.Old
	})
	sort.Slice(p.VarRewrites, func(i, j int) bool { return lessPos(p.VarRewrites[i].Pos, p.VarRewrites[j].Pos) })
	sort.Strings(p.TouchedFiles)
	p.TouchedFiles = dedupStrings(p.TouchedFiles)
	sort.Slice(p.Findings, func(i, j int) bool {
		a, b := p.Findings[i], p.Findings[j]
		if levelRank[a.Level] != levelRank[b.Level] {
			return levelRank[a.Level] < levelRank[b.Level]
		}
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		if a.Pos != b.Pos {
			return lessPos(a.Pos, b.Pos)
		}
		return a.Msg < b.Msg
	})
	p.Findings = dedupFindings(p.Findings)
	sort.Strings(p.Errors)
	p.Errors = dedupStrings(p.Errors)
}

// lessPos orders "file:line" strings by file, then numerically by line.
func lessPos(a, b string) bool {
	fa, la := splitPos(a)
	fb, lb := splitPos(b)
	if fa != fb {
		return fa < fb
	}
	return la < lb
}

func splitPos(s string) (string, int) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, 0
	}
	var n int
	if _, err := fmt.Sscanf(s[i+1:], "%d", &n); err != nil {
		return s, 0
	}
	return s[:i], n
}

func dedupStrings(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func dedupFindings(in []finding) []finding {
	var out []finding
	for i, f := range in {
		if i == 0 || f != in[i-1] {
			out = append(out, f)
		}
	}
	return out
}

// writePlan prints the move plan (files, renames, alias entries, rewrites).
func writePlan(out io.Writer, p *Plan) {
	var w strings.Builder
	defer func() { printf(out, "%s", w.String()) }()
	fmt.Fprintf(&w, "pkgmove plan: %s -> %s (package %s)\n", p.SrcImport, p.DstImport, p.PkgName)
	fmt.Fprintf(&w, "\nFiles (%d):\n", len(p.Moves))
	for _, m := range p.Moves {
		fmt.Fprintf(&w, "  git mv %s %s\n", m.From, m.To)
	}
	fmt.Fprintf(&w, "\nRenames (%d):\n", len(p.Renames))
	for _, r := range p.Renames {
		owner := ""
		if r.Owner != "" {
			owner = r.Owner + "."
		}
		fmt.Fprintf(&w, "  %-6s %s%s -> %s  (%s)\n", r.Kind, owner, r.Old, r.New, r.Pos)
	}
	fmt.Fprintf(&w, "\nAlias entries (%d):\n", len(p.Aliases))
	for _, a := range p.Aliases {
		tags := ""
		if a.Test {
			tags += " [test]"
		}
		if a.Constraint != "" {
			tags += " [" + a.Constraint + "]"
		}
		fmt.Fprintf(&w, "  %-5s %s = %s.%s%s\n", a.Kind, a.Old, p.PkgName, a.New, tags)
	}
	fmt.Fprintf(&w, "\nAlias files (%d):\n", len(p.AliasFiles))
	for _, f := range p.AliasFiles {
		fmt.Fprintf(&w, "  %s (%d lines)\n", f.Path, strings.Count(string(f.Content), "\n"))
	}
	fmt.Fprintf(&w, "\nVar reference rewrites in remaining files (%d):\n", len(p.VarRewrites))
	for _, v := range p.VarRewrites {
		fmt.Fprintf(&w, "  %s: %s -> %s\n", v.Pos, v.Old, v.New)
	}
	fmt.Fprintf(&w, "\nRemaining source files edited (%d):\n", len(p.TouchedFiles))
	for _, f := range p.TouchedFiles {
		fmt.Fprintf(&w, "  %s\n", f)
	}
	if len(p.Errors) > 0 {
		fmt.Fprintf(&w, "\nERRORS (%d) - the move cannot be generated; see the safety report below.\n", len(p.Errors))
	}
}

// safetyReport renders the safety report. It contains no timestamps or
// absolute paths, so it is byte-identical across runs.
func safetyReport(p *Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pkgmove safety report: %s -> %s\n", p.SrcImport, p.DstImport)
	fmt.Fprintf(&b, "Files moved: %d. Findings: %d. Errors: %d.\n", len(p.Moves), len(p.Findings), len(p.Errors))
	b.WriteString("\nWhy this matters: a moved package is initialised before the package that\n")
	b.WriteString("imports it, so init() functions and package-level var initialisers in the moved\n")
	b.WriteString("files now run before every initialiser of the source package. Review each\n")
	b.WriteString("ERROR/HIGH/WARN item below before merging.\n")
	if len(p.Errors) > 0 {
		b.WriteString("\n== ERRORS (the move was not generated) ==\n")
		for _, e := range p.Errors {
			fmt.Fprintf(&b, "  - %s\n", e)
		}
	}
	var lastLevel, lastCat string
	for _, f := range p.Findings {
		if f.Level != lastLevel || f.Category != lastCat {
			fmt.Fprintf(&b, "\n== %s: %s ==\n", f.Level, f.Category)
			lastLevel, lastCat = f.Level, f.Category
		}
		if f.Pos != "" {
			fmt.Fprintf(&b, "  %s: %s\n", f.Pos, f.Msg)
		} else {
			fmt.Fprintf(&b, "  %s\n", f.Msg)
		}
	}
	if len(p.Findings) == 0 && len(p.Errors) == 0 {
		b.WriteString("\nNo findings.\n")
	}
	return b.String()
}

// printf writes to an output stream; write errors on the tool's own output
// are deliberately ignored.
func printf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}
