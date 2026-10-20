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

// Package unitdrift compares the installed starter-hub systemd unit with the
// unit template in a source checkout. The Rebuild from git maintenance task
// uses it to tell an admin when the hub needs re-provisioning: the task
// itself only replaces the binary and never rewrites the unit.
//
// The check reads only: it needs no privilege beyond reading the unit file
// and asking systemd (unprivileged) where that file is.
package unitdrift

import (
	"fmt"
	"regexp"
	"strings"
)

// TemplatePath is the unit template's path relative to the repository root.
const TemplatePath = "scripts/starter-hub/scion-hub.service.tmpl"

// knownStarterHubDescriptions lists every Description= value the starter-hub
// template has used. A unit whose Description is not in this list (or equal
// to the template's current one) is not treated as a starter-hub unit, and
// the check is skipped. Append, never remove: when the template's
// Description changes, add the old value here so older hubs report drift
// instead of being skipped.
var knownStarterHubDescriptions = []string{
	"Scion Hub API Server",
}

// Status is the outcome of a comparison.
type Status int

const (
	// Skipped means no verdict: the unit is not a starter-hub unit or an
	// input is missing or unusable.
	Skipped Status = iota
	// Match means the installed unit matches the template.
	Match
	// Drift means the installed unit differs from the template.
	Drift
)

// Result is the outcome of Compare.
type Result struct {
	Status Status
	// Reason explains a Skipped result.
	Reason string
	// Structural marks a skip that will not go away on its own (see
	// Report.Structural).
	Structural bool
	// Diff lists differing lines for a Drift result, "template: ..." or
	// "installed: ...", with credential-like values elided.
	Diff []string
}

// maxLines bounds the inputs so a huge file cannot make the line alignment
// expensive. Real units are a few dozen lines.
const maxLines = 1000

// maxDiffLines bounds the diff shown in the run log.
const maxDiffLines = 10

var placeholderRE = regexp.MustCompile(`@[A-Z_]+@`)

// placeholderPattern is what a placeholder matches: one non-empty run of
// non-whitespace characters, so a flag added next to a per-host value is
// not absorbed into it.
const placeholderPattern = `(\S+)`

// normalize splits a unit into the lines systemd acts on: CRLF folded,
// leading and trailing whitespace trimmed, blank lines and full-line
// comments (# or ;) dropped.
func normalize(b []byte) []string {
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// templateLine is one template line compiled to an anchored pattern. Each
// @TOKEN@ is a per-host value that matches one non-empty run of
// non-whitespace characters.
type templateLine struct {
	text  string
	re    *regexp.Regexp
	names []string
}

func compileLine(line string) templateLine {
	var pat strings.Builder
	var names []string
	pat.WriteString("^")
	last := 0
	for _, loc := range placeholderRE.FindAllStringIndex(line, -1) {
		pat.WriteString(regexp.QuoteMeta(line[last:loc[0]]))
		pat.WriteString(placeholderPattern)
		names = append(names, line[loc[0]:loc[1]])
		last = loc[1]
	}
	pat.WriteString(regexp.QuoteMeta(line[last:]))
	pat.WriteString("$")
	return templateLine{text: line, re: regexp.MustCompile(pat.String()), names: names}
}

// description returns the value of the first Description= line.
func description(lines []string) (string, bool) {
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, "Description="); ok {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// Compare compares an installed unit with the template text.
func Compare(template, installed []byte) Result {
	tmpl := normalize(template)
	inst := normalize(installed)
	if len(tmpl) == 0 {
		return Result{Status: Skipped, Reason: "the unit template is empty", Structural: true}
	}
	if len(inst) == 0 {
		return Result{Status: Skipped, Reason: "the installed unit is empty"}
	}
	if len(tmpl) > maxLines || len(inst) > maxLines {
		return Result{Status: Skipped, Reason: fmt.Sprintf("a unit file has more than %d lines", maxLines)}
	}
	tmplDesc, ok := description(tmpl)
	if !ok {
		return Result{Status: Skipped, Reason: "the unit template has no Description= line", Structural: true}
	}
	instDesc, _ := description(inst)
	if !isKnownDescription(instDesc, tmplDesc) {
		return Result{Status: Skipped, Reason: fmt.Sprintf("the installed unit is not a starter-hub unit (Description=%q)", instDesc), Structural: true}
	}

	compiled := make([]templateLine, len(tmpl))
	for i, l := range tmpl {
		compiled[i] = compileLine(l)
	}

	pairs := align(compiled, inst)

	// Walk the alignment; unmatched lines on either side are drift, and so
	// is a placeholder that captures different values on different lines.
	var tmplOnly, instOnly []string
	values := map[string]string{}
	ti, ii := 0, 0
	for _, p := range append(pairs, [2]int{len(tmpl), len(inst)}) {
		for ; ti < p[0]; ti++ {
			tmplOnly = append(tmplOnly, tmpl[ti])
		}
		for ; ii < p[1]; ii++ {
			instOnly = append(instOnly, inst[ii])
		}
		if p[0] == len(tmpl) {
			break
		}
		tl := compiled[p[0]]
		m := tl.re.FindStringSubmatch(inst[p[1]])
		consistent := true
		for k, name := range tl.names {
			if prev, seen := values[name]; seen && prev != m[k+1] {
				consistent = false
			} else if !seen {
				values[name] = m[k+1]
			}
		}
		if !consistent {
			tmplOnly = append(tmplOnly, tmpl[p[0]])
			instOnly = append(instOnly, inst[p[1]])
		}
		ti, ii = p[0]+1, p[1]+1
	}

	if len(tmplOnly) == 0 && len(instOnly) == 0 {
		return Result{Status: Match}
	}
	return Result{Status: Drift, Diff: diffLines(tmplOnly, instOnly)}
}

func isKnownDescription(desc, templateDesc string) bool {
	if desc == templateDesc {
		return true
	}
	for _, d := range knownStarterHubDescriptions {
		if desc == d {
			return true
		}
	}
	return false
}

// align returns the index pairs (template, installed) of a longest common
// subsequence where a template line's pattern matches the installed line.
func align(tmpl []templateLine, inst []string) [][2]int {
	n, m := len(tmpl), len(inst)
	match := make([][]bool, n)
	for i := range tmpl {
		match[i] = make([]bool, m)
		for j := range inst {
			match[i][j] = tmpl[i].re.MatchString(inst[j])
		}
	}
	// lcs[i][j] = LCS length of tmpl[i:] and inst[j:].
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case match[i][j]:
				lcs[i][j] = lcs[i+1][j+1] + 1
			case lcs[i+1][j] >= lcs[i][j+1]:
				lcs[i][j] = lcs[i+1][j]
			default:
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var pairs [][2]int
	for i, j := 0, 0; i < n && j < m; {
		switch {
		case match[i][j] && lcs[i][j] == lcs[i+1][j+1]+1:
			pairs = append(pairs, [2]int{i, j})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			i++
		default:
			j++
		}
	}
	return pairs
}

var credentialKeyRE = regexp.MustCompile(`(?i)(token|secret|key|password|credential)`)

// elide hides the value of Environment=, EnvironmentFile= and any line whose
// key looks credential-like, so the run log (stored in the hub database and
// shown in the web UI) never carries a value copied from the unit.
func elide(line string) string {
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		return line
	}
	// systemd allows spaces around "=".
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	switch {
	case key == "Environment":
		v := strings.Trim(value, `"'`)
		name, _, hasEq := strings.Cut(v, "=")
		if !hasEq {
			return key + "=…"
		}
		return key + `="` + name + `=…"`
	case key == "EnvironmentFile", credentialKeyRE.MatchString(key):
		return key + "=…"
	}
	return line
}

func diffLines(tmplOnly, instOnly []string) []string {
	te := make([]string, len(tmplOnly))
	ie := make([]string, len(instOnly))
	for i, l := range tmplOnly {
		te[i] = elide(l)
	}
	for i, l := range instOnly {
		ie[i] = elide(l)
	}
	// An elided line that reads the same on both sides only differs in its
	// hidden value; say so, or the diff looks like two identical lines.
	differs := func(s string, other []string, orig string) string {
		if s == orig {
			return s
		}
		for _, o := range other {
			if o == s {
				return s + " (value differs)"
			}
		}
		return s
	}
	var out []string
	for i, s := range te {
		out = append(out, "template:  "+differs(s, ie, tmplOnly[i]))
	}
	for i, s := range ie {
		out = append(out, "installed: "+differs(s, te, instOnly[i]))
	}
	if len(out) > maxDiffLines {
		more := len(out) - maxDiffLines
		out = append(out[:maxDiffLines], fmt.Sprintf("... and %d more differing lines", more))
	}
	return out
}
