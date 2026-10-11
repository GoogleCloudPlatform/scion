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
	"go/ast"
	"io"
	"path"
	"strings"
)

// helperSuffix marks a helper file; such files are never bulk origins.
const helperSuffix = "_helpers_test.go"

// bulkDest returns the helper file that receives the cross-file
// declarations of origin (whose build constraint is constraint).
//
//   - by "origin": x_test.go -> x_helpers_test.go.
//   - by "area": the area is the file name up to its first '_' (for
//     handlers_agent_test.go, "handlers"). Unconstrained origins go to
//     handlers_helpers_test.go, !no_sqlite origins to
//     handlers_sqlite_helpers_test.go, and any other constraint to
//     handlers_<tag>_helpers_test.go, so each helper file has one constraint.
func bulkDest(origin, constraint, by string) string {
	stem := strings.TrimSuffix(origin, "_test.go")
	if by == "origin" {
		return stem + helperSuffix
	}
	area, _, _ := strings.Cut(stem, "_")
	if tag := constraintTag(constraint); tag != "" {
		area += "_" + tag
	}
	return area + helperSuffix
}

// constraintTag turns a build constraint into a file-name fragment: "" for
// none, "sqlite" for !no_sqlite, otherwise its letters and digits with "!"
// spelled "not" (for example "linux", "notwindows").
func constraintTag(c string) string {
	switch c {
	case "":
		return ""
	case "!no_sqlite":
		return "sqlite"
	}
	var b strings.Builder
	for _, r := range strings.ReplaceAll(c, "!", "not") {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if r >= 'A' && r <= 'Z' {
			b.WriteRune(r - 'A' + 'a')
		}
	}
	return b.String()
}

// isBulkOrigin reports whether f may give up declarations in a bulk run.
func isBulkOrigin(f *srcFile, glob string, skip map[string]bool) bool {
	if !f.isTest || strings.HasSuffix(f.name, helperSuffix) || skip[f.name] {
		return false
	}
	if glob == "" {
		return true
	}
	ok, _ := path.Match(glob, f.name)
	return ok
}

// bulkFamilies returns one family per helper file. From each origin test
// file it takes the top-level non-method declarations that a declaration in
// another test file refers to (methods follow their type); a declaration that
// shares a grouped var/const/type block takes the whole group. They go to
// bulkDest(origin), which carries the origin's own build constraint (so all
// origins of one dest share a constraint). origins[i] lists the origin files
// of fams[i]. Declarations that cannot be requested by name are reported in
// notes and left alone.
func bulkFamilies(p *pkgInfo, glob, by string, skip, keep map[string]bool) (fams []family, origins [][]string, notes []string) {
	users := p.users()
	index := map[string]int{} // dest -> position in fams
	for _, f := range p.files {
		if !isBulkOrigin(f, glob, skip) {
			continue
		}
		var names []string
		seen := map[string]bool{}
		blocked := ""
		for _, d := range p.decls {
			if d.file != f || d.recv != "" || seen[d.key] || !crossUsed(d, users[d.key]) {
				continue
			}
			if keep[d.key] {
				continue // pinned to its file (bulk -keep)
			}
			if isTestFunc(d.key) || d.key == "init" || d.key == "TestMain" {
				notes = append(notes, fmt.Sprintf("%s: %s is used by another test file but is a test entry point; left in place", f.name, d.key))
				continue
			}
			if len(p.byKey[d.key]) != 1 {
				notes = append(notes, fmt.Sprintf("%s: %s is declared %d times (per-platform files?); left in place", f.name, d.key, len(p.byKey[d.key])))
				continue
			}
			for _, m := range groupMates(p, d) {
				if strings.HasPrefix(m.key, "_@") || keep[m.key] {
					blocked = d.key
				}
				if !seen[m.key] {
					seen[m.key] = true
					names = append(names, m.key)
				}
			}
		}
		if blocked != "" {
			notes = append(notes, fmt.Sprintf("%s: %s shares a block with a blank or kept declaration; origin skipped", f.name, blocked))
			continue
		}
		if len(names) == 0 {
			continue
		}
		dest := bulkDest(f.name, f.constraint, by)
		i, ok := index[dest]
		if !ok {
			i = len(fams)
			index[dest] = i
			fams = append(fams, family{dest: dest, build: f.constraint, localClosure: true, keep: keep})
			origins = append(origins, nil)
		}
		fams[i].names = append(fams[i].names, names...)
		origins[i] = append(origins[i], f.name)
	}
	return fams, origins, notes
}

// crossUsed reports whether a declaration in a test file other than d's own
// refers to d.
func crossUsed(d *declInfo, users map[*declInfo]bool) bool {
	for u := range users {
		if u.file != d.file && u.file.isTest {
			return true
		}
	}
	return false
}

// groupMates returns every declaration that shares d's AST node (a grouped
// GenDecl moves as a whole), in source order; for a lone declaration, d.
func groupMates(p *pkgInfo, d *declInfo) []*declInfo {
	if _, ok := d.node.(*ast.GenDecl); !ok {
		return []*declInfo{d}
	}
	var out []*declInfo
	for _, o := range p.decls {
		if o.node == d.node {
			out = append(out, o)
		}
	}
	return out
}

// stillIn returns the names that are still declared (once) in one of the
// files origins, keeping their order.
func stillIn(p *pkgInfo, names, origins []string) []string {
	in := map[string]bool{}
	for _, o := range origins {
		in[o] = true
	}
	var out []string
	for _, n := range names {
		if ds := p.byKey[n]; len(ds) == 1 && in[ds[0].file.name] {
			out = append(out, n)
		}
	}
	return out
}

// notPlanned returns the names not in planned, keeping their order.
func notPlanned(names []string, planned map[string]bool) []string {
	var out []string
	for _, n := range names {
		if !planned[n] {
			out = append(out, n)
		}
	}
	return out
}

// bulkStats summarises a bulk run.
type bulkStats struct {
	iterations int // rounds in which something moved
	moved      int // declarations moved, closure and methods included
	origins    map[string]bool
	skipped    map[string]string // origin -> reason it was dropped
}

// runBulkOn moves every cross-file-used declaration of each origin into the
// origin's helper file (see bulkDest) and repeats until no origin has any
// left: a moved helper that uses a declaration its origin keeps makes that
// declaration cross-file used in the next round. If a helper file's family
// cannot be moved, its origins are dropped (reported) for the rest of the
// run. With dry set, only the first round is planned and nothing is written.
func runBulkOn(dir, glob, by string, keep map[string]bool, dry bool, maxIter int, w io.Writer) (*bulkStats, error) {
	st := &bulkStats{origins: map[string]bool{}, skipped: map[string]string{}}
	skip := map[string]bool{}
	reported := map[string]bool{}
	for round := 0; round < maxIter; round++ {
		p, err := loadPackage(dir)
		if err != nil {
			return st, err
		}
		fams, origins, notes := bulkFamilies(p, glob, by, skip, keep)
		for _, n := range notes {
			if !reported[n] {
				reported[n] = true
				_, _ = fmt.Fprintln(w, "note", n)
			}
		}
		if len(fams) == 0 {
			return st, nil
		}
		movedBefore := st.moved
		// Keys planned by earlier families of this round. A real run sees
		// them gone from their origins (stillIn); a dry run writes nothing,
		// so it drops them here to print the same plan.
		planned := map[string]bool{}
		for i, fam := range fams {
			if i > 0 {
				// Every apply rewrites files, so plan each family on a fresh load.
				if p, err = loadPackage(dir); err != nil {
					return st, err
				}
				// An earlier family of this round may already have taken some
				// of these names (as its own closure); request only the ones
				// still declared in this family's origins, so nothing is
				// pulled back out of another helper file.
				fam.names = stillIn(p, fam.names, origins[i])
				fam.names = notPlanned(fam.names, planned)
				if len(fam.names) == 0 {
					continue
				}
			}
			res, err := plan(p, fam)
			if err != nil {
				for _, o := range origins[i] {
					skip[o] = true
					st.skipped[o] = err.Error()
				}
				_, _ = fmt.Fprintf(w, "skip %s (from %s): %v\n", fam.dest, strings.Join(origins[i], " "), err)
				continue
			}
			for _, m := range res.moves {
				_, _ = fmt.Fprintf(w, "move %-40s %s -> %s (%s)\n", m.d.key, m.d.file.name, fam.dest, m.reason)
			}
			for _, name := range sortedKeys(res.changed) {
				if res.changed[name] == nil {
					_, _ = fmt.Fprintf(w, "delete %s (no declarations left)\n", name)
				}
			}
			st.moved += len(res.moves)
			for _, m := range res.moves {
				st.origins[m.d.file.name] = true
				planned[m.d.key] = true
			}
			if dry {
				continue
			}
			if err := apply(p, res); err != nil {
				return st, err
			}
		}
		if st.moved > movedBefore {
			st.iterations++
		}
		if dry {
			return st, nil
		}
		if st.moved == movedBefore {
			return st, nil // nothing movable is left (the rest was skipped)
		}
	}
	return st, fmt.Errorf("no fixpoint after %d rounds", maxIter)
}
