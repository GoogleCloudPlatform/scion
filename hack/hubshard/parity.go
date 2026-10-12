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
	"bytes"
	"encoding/json"
	"fmt"
	"go/build"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The overlay, the shard queries (-list, -skip, -locate) and the
// parity check.
//
// Which files a build compiles is decided by go/build itself
// (build.Context.MatchFile, reading the overlay contents), so the build
// lines, the file-name GOOS/GOARCH suffixes and the implied tags (unix,
// the go1.N release tags, cgo) are judged exactly as go test judges them
// on this machine.

// parityConfigs are the build configurations -parity checks: the tag sets
// CI's shard jobs compile pkg/hub with (hack/hubshard/shard.sh).
var parityConfigs = []struct {
	name string
	tags []string
}{
	{"default", nil},
	{"no_sqlite", []string{"no_sqlite"}},
	{"integration", []string{"integration"}},
	{"vet-integration", vetIntegrationTags},
}

// The two tag sets pkg/hub is vetted with. Before the shards, CI ran
// go vet -tags no_sqlite ./... (make lint) and go vet -tags 'integration
// volume_test tzcontract' ./... (make vet-integration) over pkg/hub; each
// shard job now vets its shard with the same two sets (shard.sh vet).
var (
	vetNoSQLiteTags    = []string{"no_sqlite"}
	vetIntegrationTags = []string{"integration", "volume_test", "tzcontract"}
)

// sharding holds the computed shards and the overlay contents.
type sharding struct {
	absDir     string
	files      map[string]*fileInfo // the test files as on disk (no hubshard tags)
	an         *analysis
	assign     map[string]int // file -> shard, 0 = common
	n          int
	pkg        string
	tagged     map[string][]byte    // sharded file -> its contents with the hubshard clause
	imports    map[string][]byte    // generated imports file -> contents
	genFiles   map[string]*fileInfo // every test file as the overlay build sees it
	genImports map[string]*fileInfo // the generated imports files, parsed
}

func newSharding(dir string, files map[string]*fileInfo, an *analysis, assign map[string]int, n int) (*sharding, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	s := &sharding{
		absDir: absDir, files: files, an: an, assign: assign, n: n,
		pkg:      testPackage(files),
		tagged:   map[string][]byte{},
		genFiles: map[string]*fileInfo{},
	}
	for _, name := range sortedNames(files) {
		f := files[name]
		out, err := rewrite(f, assign[name])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if bytes.Equal(out, f.src) {
			s.genFiles[name] = f
			continue
		}
		s.tagged[name] = out
		g, err := parseFile(filepath.Join(dir, name), out)
		if err != nil {
			return nil, fmt.Errorf("generated %s: %w", name, err)
		}
		s.genFiles[name] = g
	}
	s.imports = importsFiles(files, s.pkg)
	s.genImports = map[string]*fileInfo{}
	for _, name := range sortedKeys(s.imports) {
		g, err := parseFile(filepath.Join(dir, name), s.imports[name])
		if err != nil {
			return nil, fmt.Errorf("generated %s: %w", name, err)
		}
		s.genImports[name] = g
	}
	return s, nil
}

// matchSrc reports whether go/build compiles the file name, with contents
// src, under tags (on this machine's GOOS/GOARCH).
func matchSrc(name string, src []byte, tags []string) (bool, error) {
	ctx := build.Default
	ctx.BuildTags = append([]string(nil), tags...)
	ctx.OpenFile = func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(src)), nil
	}
	return ctx.MatchFile(".", name)
}

func shardTags(k int, tags []string) []string {
	return append(append([]string(nil), tags...), shardTag, fmt.Sprintf("%s_%d", shardTag, k))
}

// testsIn returns the tests of the files m that go/build compiles under
// tags, as test name -> files declaring it.
func testsIn(m map[string]*fileInfo, tags []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, name := range sortedNames(m) {
		f := m[name]
		if len(f.tests) == 0 {
			continue
		}
		ok, err := matchSrc(name, f.src, tags)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if !ok {
			continue
		}
		for _, t := range f.tests {
			out[t] = append(out[t], name)
		}
	}
	return out, nil
}

// shardTests returns the sorted tests that shard k's binary compiles under
// tags: the common files' tests and shard k's. go test -list (or the test
// binary's -test.list) prints the same set.
func (s *sharding) shardTests(k int, tags []string) ([]string, error) {
	m, err := testsIn(s.genFiles, shardTags(k, tags))
	if err != nil {
		return nil, err
	}
	return sortedKeys(m), nil
}

// commonTests returns the sorted tests of the common files under tags.
// Every shard compiles them; shard.sh runs them only in shard 1 (the other
// shards pass skipPattern to go test -skip), so each runs once.
func (s *sharding) commonTests(tags []string) ([]string, error) {
	common := map[string]*fileInfo{}
	for name, f := range s.genFiles {
		if s.an.common[name] {
			common[name] = f
		}
	}
	m, err := testsIn(common, tags)
	if err != nil {
		return nil, err
	}
	return sortedKeys(m), nil
}

// skipPattern returns the go test -skip pattern for shard k under tags:
// ^(A|B|...)$ over the common files' tests, or "" for shard 1 (or when
// there are none). go test matches -skip against each test name with the
// pattern split at slashes, so the anchored top-level alternatives skip
// exactly these tests and no subtest of another test.
func (s *sharding) skipPattern(k int, tags []string) (string, error) {
	if k == 1 {
		return "", nil
	}
	names, err := s.commonTests(tags)
	if err != nil || len(names) == 0 {
		return "", err
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexp.QuoteMeta(n)
	}
	return "^(" + strings.Join(quoted, "|") + ")$", nil
}

// runShard returns the shard that runs the test declared in file name: 1
// for a common file (see commonTests), else the file's shard.
func (s *sharding) runShard(name string) int {
	if s.an.common[name] {
		return 1
	}
	return s.assign[name]
}

// locate prints "<test> <K>" for each test name: the shard that runs it
// under tags. It fails if a name is in no file compiled under tags, so a
// renamed or deleted test cannot drop out of a hand-maintained list
// silently.
func (s *sharding) locate(w io.Writer, tags []string, names []string) error {
	m, err := testsIn(s.files, tags)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	var missing []string
	for _, t := range names {
		decl := m[t]
		if len(decl) == 0 {
			missing = append(missing, t)
			continue
		}
		fmt.Fprintf(&b, "%s %d\n", t, s.runShard(decl[0]))
	}
	if len(missing) > 0 {
		return fmt.Errorf("no pkg/hub test file compiled with -tags %q declares: %s", strings.Join(tags, ","), strings.Join(missing, " "))
	}
	_, err = w.Write(b.Bytes())
	return err
}

// writeOverlay writes the tagged files and the generated imports files to
// out, plus out/overlay.json mapping each of them to its path in the
// package directory, for go build -overlay. Nothing in the tree changes.
func (s *sharding) writeOverlay(out string, w io.Writer) error {
	absOut, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(absOut, 0o755); err != nil {
		return err
	}
	replace := map[string]string{}
	for _, m := range []map[string][]byte{s.tagged, s.imports} {
		for _, name := range sortedKeys(m) {
			p := filepath.Join(absOut, name)
			if err := os.WriteFile(p, m[name], 0o644); err != nil {
				return err
			}
			replace[filepath.Join(s.absDir, name)] = p
		}
	}
	data, err := json.MarshalIndent(struct{ Replace map[string]string }{replace}, "", "\t")
	if err != nil {
		return err
	}
	cfg := filepath.Join(absOut, "overlay.json")
	if err := os.WriteFile(cfg, append(data, '\n'), 0o644); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "overlay: %d tagged test files and %d generated imports files: %s\n", len(s.tagged), len(s.imports), cfg)
	return err
}

// parity checks, for every configuration in parityConfigs, that the union
// of the shards runs exactly the tests the plain build runs, each exactly
// once (a common file's tests in shard 1), and that every shard build
// compiles: no file uses a file of another shard, and every blank import
// reaches every shard. It also checks that the per-shard vets cover every
// test file the two module-wide vets covered. It prints a per-shard table.
func (s *sharding) parity(w io.Writer) error {
	var problems []string
	for _, name := range sortedNames(s.files) {
		if s.genFiles[name] == nil {
			problems = append(problems, fmt.Sprintf("%s: in the tree but missing from the overlay build", name))
		}
	}
	for _, name := range sortedNames(s.genFiles) {
		if s.files[name] == nil {
			problems = append(problems, fmt.Sprintf("%s: in the overlay build but not in the tree", name))
		}
	}
	problems = append(problems, breakage(s.an, s.genFiles, s.assign, s.n)...)
	problems = append(problems, importBreakage(s.genFiles, s.genImports, s.pkg)...)
	var b bytes.Buffer
	for _, warn := range sideEffectWarnings(s.genFiles, s.pkg) {
		fmt.Fprintln(&b, warn)
	}

	tagProblems, err := s.tagPlacement()
	if err != nil {
		return err
	}
	problems = append(problems, tagProblems...)

	for _, cfg := range parityConfigs {
		plain, err := testsIn(s.files, cfg.tags)
		if err != nil {
			return err
		}
		runs := map[string]int{}
		compiled := make([]int, s.n+1)
		ran := make([]int, s.n+1)
		for k := 1; k <= s.n; k++ {
			m, err := testsIn(s.genFiles, shardTags(k, cfg.tags))
			if err != nil {
				return err
			}
			pattern, err := s.skipPattern(k, cfg.tags)
			if err != nil {
				return err
			}
			var skipRE *regexp.Regexp
			if pattern != "" {
				if skipRE, err = regexp.Compile(pattern); err != nil {
					return err
				}
			}
			for _, t := range sortedKeys(m) {
				compiled[k]++
				if len(m[t]) > 1 {
					problems = append(problems, fmt.Sprintf("%s: shard %d declares %s in %s", cfg.name, k, t, strings.Join(m[t], ", ")))
				}
				if plain[t] == nil {
					problems = append(problems, fmt.Sprintf("%s: shard %d compiles %s (%s), which the plain build does not", cfg.name, k, t, m[t][0]))
					continue
				}
				// What go test -skip does with the pattern shard.sh passes.
				skipped := skipRE != nil && skipRE.MatchString(t)
				if skipped != (s.runShard(m[t][0]) != k) {
					problems = append(problems, fmt.Sprintf("%s: shard %d: -skip pattern skipped=%v for %s (%s)", cfg.name, k, skipped, t, m[t][0]))
				}
				if !skipped {
					runs[t]++
					ran[k]++
				}
			}
		}
		for _, t := range sortedKeys(plain) {
			if c := runs[t]; c != 1 {
				problems = append(problems, fmt.Sprintf("%s: %s (%s) runs in %d shards, want exactly 1", cfg.name, t, plain[t][0], c))
			}
		}
		fmt.Fprintf(&b, "parity %s: %d tests in the plain build;", cfg.name, len(plain))
		for k := 1; k <= s.n; k++ {
			fmt.Fprintf(&b, " shard %d compiles %d, runs %d;", k, compiled[k], ran[k])
		}
		b.Truncate(b.Len() - 1)
		b.WriteString("\n")
	}

	vetted := 0
	for _, name := range sortedNames(s.files) {
		before, err := vettedBy(name, s.files[name].src, nil)
		if err != nil {
			return err
		}
		if !before {
			continue
		}
		vetted++
		if s.genFiles[name] == nil {
			continue // reported above
		}
		now := false
		for k := 1; k <= s.n && !now; k++ {
			if now, err = vettedBy(name, s.genFiles[name].src, []int{k}); err != nil {
				return err
			}
		}
		if !now {
			problems = append(problems, fmt.Sprintf("vet: %s was vetted by go vet -tags no_sqlite or -tags '%s', and no shard's vet covers it", name, strings.Join(vetIntegrationTags, " ")))
		}
	}
	fmt.Fprintf(&b, "parity vet: all %d test files the module-wide vets covered are covered by a shard's vet\n", vetted)

	if len(problems) > 0 {
		if _, err := w.Write(b.Bytes()); err != nil {
			return err
		}
		sort.Strings(problems)
		return fmt.Errorf("parity: %d problem(s):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	b.WriteString("parity ok\n")
	_, err = w.Write(b.Bytes())
	return err
}

// vettedBy reports whether one of the two vet tag sets compiles the file:
// as is (shards nil), or with the hubshard tags of one of shards added.
func vettedBy(name string, src []byte, shards []int) (bool, error) {
	for _, tags := range [][]string{vetNoSQLiteTags, vetIntegrationTags} {
		sets := [][]string{tags}
		if shards != nil {
			sets = nil
			for _, k := range shards {
				sets = append(sets, shardTags(k, tags))
			}
		}
		for _, t := range sets {
			ok, err := matchSrc(name, src, t)
			if err != nil || ok {
				return ok, err
			}
		}
	}
	return false, nil
}

// tagPlacement checks every test file of the overlay with go/build: under
// each parity configuration in which the plain build compiles it, a
// sharded file must compile in its own shard and in no other, and a common
// file in every shard. A generated //go:build line that go/build ignored
// (misplaced in the header) would put a sharded file in every shard; this
// makes that fail loudly.
func (s *sharding) tagPlacement() ([]string, error) {
	var problems []string
	for _, name := range sortedNames(s.genFiles) {
		g := s.genFiles[name]
		if s.files[name] == nil {
			continue // reported by parity
		}
		for _, cfg := range parityConfigs {
			ok, err := matchSrc(name, s.files[name].src, cfg.tags)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if !ok {
				continue
			}
			want := s.assign[name]
			if s.an.common[name] {
				want = 0
			}
			var in []int
			for k := 1; k <= s.n; k++ {
				ok, err := matchSrc(name, g.src, shardTags(k, cfg.tags))
				if err != nil {
					return nil, fmt.Errorf("%s: %w", name, err)
				}
				if ok {
					in = append(in, k)
				}
			}
			switch {
			case want == 0 && len(in) != s.n:
				problems = append(problems, fmt.Sprintf("%s: %s: common file compiles in shards %v, want all %d", cfg.name, name, in, s.n))
			case want != 0 && (len(in) != 1 || in[0] != want):
				problems = append(problems, fmt.Sprintf("%s: %s: assigned to shard %d, go/build compiles it in shards %v (is its //go:build line ignored?)", cfg.name, name, want, in))
			}
		}
	}
	return problems, nil
}
