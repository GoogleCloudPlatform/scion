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

package cmd

// Contract: no Cloud Build ignore file may drop a file that a //go:embed
// directive compiles into a binary.
//
// gcloud filters the Cloud Build source upload with gitignore semantics, so a
// slash-less pattern such as `agents.md` or `.gemini/` matches at any depth.
// When such a pattern hits a file under a //go:embed root, the build still
// succeeds, but the file is missing from the binary. See ptone/scion#3509.
//
// How it works:
//   - Embedded files are found from the source: every tracked, non-test .go
//     file in the root module is scanned for //go:embed directives, and the
//     patterns are resolved against `git ls-files` with go:embed rules (a
//     matched directory embeds its tree; without the `all:` prefix, names
//     starting with "." or "_" below it are skipped). Roots are not hardcoded.
//   - Ignore files are `.gcloudignore` and every tracked
//     `image-build/gcloudignore-*`.
//   - Matching is done by `git check-ignore --no-index` in an empty scratch
//     repository, with the expanded ignore file as the only exclude source.
//     This gives real gitignore semantics (negation, `**`, directory-only
//     patterns, and "a file inside an excluded directory cannot be
//     re-included"), the same rules gcloud documents and implements in
//     googlecloudsdk/command_lib/util/gcloudignore.py.
//   - `#!include:<file>` lines are expanded in place, as gcloud does: the file
//     is read from the directory that contains the ignore file (not the repo
//     root), names containing "/" are rejected, and include lines inside an
//     included file are not followed (gcloud uses one level of recursion).
//     The included patterns keep their meaning relative to the upload root.
//     So `image-build/gcloudignore-*` pull in `image-build/.gitignore`, and
//     only the root `.gcloudignore` pulls in the root `.gitignore`.

import (
	"bytes"
	"fmt"
	"go/build/constraint"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// gcloudIgnoreBuildTags lists the extra Go build tags used by the builds that
// upload with each ignore file. The scion-base build that uses the default
// .gcloudignore compiles with -tags no_embed_web, so web/embed.go (and its
// //go:embed all:dist/client) is not compiled there; the omni and hub-gke
// builds (both upload with image-build/gcloudignore-omni) compile without that
// tag and build web/dist/client in the image.
// Ignore files not listed here are checked with no extra tags (the strictest
// choice).
var gcloudIgnoreBuildTags = map[string][]string{
	".gcloudignore": {"no_embed_web"},
}

// embeddedFile is a tracked file compiled into a binary by a //go:embed
// directive.
type embeddedFile struct {
	path      string // repo-relative, slash-separated
	directive string // "<go file>:<line>: //go:embed ..." that embeds it
}

// repoTrackedFiles returns `git ls-files` for the repo root (one level above
// this package).
func repoTrackedFiles(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found; the gcloudignore contract needs git ls-files and git check-ignore")
	}
	cmd := exec.Command("git", "-C", "..", "ls-files", "-z")
	// Without GIT_* (e.g. GIT_DIR or GIT_INDEX_FILE leaked from a hook or a
	// worktree wrapper), git finds the repository from the directory alone.
	cmd.Env = envWithoutGit()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v: %s", err, stderr.String())
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		t.Fatal("git ls-files returned no files")
	}
	return files
}

// envWithoutGit returns the process environment minus every GIT_* variable,
// so a git subprocess sees neither a caller-chosen repository, index or work
// tree nor config overrides passed through the environment.
func envWithoutGit() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return env
}

// goFileBuildConstraint returns the //go:build expression of a Go source
// file, or nil if it has none.
func goFileBuildConstraint(src string) (constraint.Expr, error) {
	for _, line := range strings.Split(src, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "package ") {
			return nil, nil
		}
		if constraint.IsGoBuild(l) {
			return constraint.Parse(l)
		}
	}
	return nil, nil
}

// parseEmbedPatterns splits the argument of a //go:embed line into patterns.
// Patterns are space-separated and may be Go string literals.
func parseEmbedPatterns(args string) ([]string, error) {
	var pats []string
	s := strings.TrimSpace(args)
	for s != "" {
		var p string
		switch s[0] {
		case '"', '`':
			end := strings.IndexByte(s[1:], s[0])
			if s[0] == '"' {
				// Find the closing quote, skipping escapes.
				end = -1
				for i := 1; i < len(s); i++ {
					if s[i] == '\\' {
						i++
						continue
					}
					if s[i] == '"' {
						end = i - 1
						break
					}
				}
			}
			if end < 0 {
				return nil, fmt.Errorf("unterminated string in %q", args)
			}
			lit := s[:end+2]
			var err error
			if p, err = strconv.Unquote(lit); err != nil {
				return nil, fmt.Errorf("bad string %s: %v", lit, err)
			}
			s = s[end+2:]
		default:
			end := strings.IndexAny(s, " \t")
			if end < 0 {
				end = len(s)
			}
			p, s = s[:end], s[end:]
		}
		pats = append(pats, p)
		s = strings.TrimSpace(s)
	}
	return pats, nil
}

// listGoEmbeddedFiles returns every tracked file that a //go:embed directive
// in the root module embeds when compiled with the given extra build tags
// (on linux/amd64).
//
// Not modeled: GOOS/GOARCH file-name suffixes, and the //go:embed rule that
// rejects irregular files. _test.go files are not scanned (test-only embeds
// never reach a shipped binary).
func listGoEmbeddedFiles(t *testing.T, tracked []string, tags []string) []embeddedFile {
	t.Helper()
	tagSet := map[string]bool{"linux": true, "amd64": true, "unix": true, "gc": true}
	for _, tg := range tags {
		tagSet[tg] = true
	}
	hasTag := func(tg string) bool { return tagSet[tg] || strings.HasPrefix(tg, "go1.") }

	// Nested modules (e.g. extras/*) are separate builds; their files are
	// neither scanned nor embeddable from the root module.
	var nestedModules []string
	for _, f := range tracked {
		if path.Base(f) == "go.mod" && f != "go.mod" {
			nestedModules = append(nestedModules, path.Dir(f)+"/")
		}
	}
	inRootModule := func(f string) bool {
		for _, m := range nestedModules {
			if strings.HasPrefix(f, m) {
				return false
			}
		}
		return true
	}

	var out []embeddedFile
	for _, gf := range tracked {
		if !strings.HasSuffix(gf, ".go") || strings.HasSuffix(gf, "_test.go") || !inRootModule(gf) {
			continue
		}
		b, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(gf)))
		if err != nil {
			t.Fatalf("read %s: %v", gf, err)
		}
		src := string(b)
		if !strings.Contains(src, "//go:embed") {
			continue
		}
		expr, err := goFileBuildConstraint(src)
		if err != nil {
			t.Fatalf("%s: bad //go:build line: %v", gf, err)
		}
		if expr != nil && !expr.Eval(hasTag) {
			continue
		}
		pkgDir := path.Dir(gf)
		for i, line := range strings.Split(src, "\n") {
			rest, ok := strings.CutPrefix(strings.TrimSpace(line), "//go:embed")
			if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
				continue
			}
			directive := fmt.Sprintf("%s:%d: //go:embed%s", gf, i+1, rest)
			pats, err := parseEmbedPatterns(rest)
			if err != nil || len(pats) == 0 {
				t.Fatalf("%s: cannot parse directive: %v", directive, err)
			}
			for _, pat := range pats {
				all := strings.HasPrefix(pat, "all:")
				pat = strings.TrimPrefix(pat, "all:")
				for _, f := range tracked {
					if pkgDir != "." && !strings.HasPrefix(f, pkgDir+"/") {
						continue
					}
					if !inRootModule(f) {
						continue
					}
					rel := strings.TrimPrefix(f, pkgDir+"/")
					if goEmbedPatternMatches(pat, rel, all) {
						out = append(out, embeddedFile{path: f, directive: directive})
					}
				}
			}
		}
	}
	return out
}

// goEmbedPatternMatches reports whether the go:embed pattern pat (relative to
// the package directory) embeds the package-relative file rel. A pattern that
// names the file embeds it unconditionally; a pattern that names a parent
// directory embeds the file unless, without all:, a path element below that
// directory starts with "." or "_".
func goEmbedPatternMatches(pat, rel string, all bool) bool {
	comps := strings.Split(rel, "/")
	for i := 1; i <= len(comps); i++ {
		if ok, _ := path.Match(pat, strings.Join(comps[:i], "/")); !ok {
			continue
		}
		if i == len(comps) || all {
			return true
		}
		hidden := false
		for _, c := range comps[i:] {
			if strings.HasPrefix(c, ".") || strings.HasPrefix(c, "_") {
				hidden = true
			}
		}
		return !hidden
	}
	return false
}

// ignoreLine is one line of an expanded ignore file, with where it came from.
type ignoreLine struct {
	text   string
	origin string // "<file>:<line>"
}

// expandGcloudIgnore expands `#!include:` directives the way gcloud does (see
// the file comment). name is the repo-relative path of the ignore file;
// readFile reads a repo-relative path.
func expandGcloudIgnore(name, content string, readFile func(string) (string, error)) ([]ignoreLine, error) {
	return expandGcloudIgnoreDepth(name, content, readFile, 1)
}

func expandGcloudIgnoreDepth(name, content string, readFile func(string) (string, error), recurse int) ([]ignoreLine, error) {
	var out []ignoreLine
	for i, line := range strings.Split(content, "\n") {
		origin := fmt.Sprintf("%s:%d", name, i+1)
		if strings.HasPrefix(line, "#") {
			body := strings.TrimLeft(line[1:], " \t")
			inc, ok := strings.CutPrefix(body, "!include:")
			if !ok || recurse == 0 {
				continue // a comment, or an include gcloud does not follow
			}
			if strings.Contains(inc, "/") {
				return nil, fmt.Errorf("%s: gcloud only includes files from the same directory: %q", origin, line)
			}
			incPath := path.Join(path.Dir(name), inc)
			incContent, err := readFile(incPath)
			if err != nil {
				return nil, fmt.Errorf("%s: %s: gcloud fails on a missing included file: %v", origin, line, err)
			}
			lines, err := expandGcloudIgnoreDepth(incPath, incContent, readFile, recurse-1)
			if err != nil {
				return nil, err
			}
			for _, l := range lines {
				l.origin = l.origin + " (via " + origin + " " + strings.TrimSpace(line) + ")"
				out = append(out, l)
			}
			continue
		}
		out = append(out, ignoreLine{text: line, origin: origin})
	}
	return out, nil
}

// gitIgnoredBy runs `git check-ignore --no-index` in an empty scratch repo
// with lines as the only exclude source. It returns, for each ignored path,
// the index into lines of the pattern that decided it.
func gitIgnoredBy(t *testing.T, lines []ignoreLine, paths []string) map[string]int {
	t.Helper()
	if len(paths) == 0 {
		return nil
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	excludes := filepath.Join(dir, "excludes")
	var buf strings.Builder
	for _, l := range lines {
		buf.WriteString(l.text)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(excludes, []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	// git runs a git command in the scratch repo. check-ignore exits 1 when
	// no path is ignored; okExit1 accepts that.
	git := func(stdin string, okExit1 bool, args ...string) ([]byte, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Stdin = strings.NewReader(stdin)
		// Isolate from the caller's git config and repository.
		cmd.Env = append(envWithoutGit(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_CEILING_DIRECTORIES="+dir)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 && okExit1 {
				return out, nil
			}
			return out, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
		}
		return out, nil
	}
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := git("", false, "init", "-q", "."); err != nil {
		t.Fatal(err)
	}
	// The scratch repo's own .git/info/exclude would be a second source.
	_ = os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), nil, 0o644)

	out, err := git(strings.Join(paths, "\x00")+"\x00", true,
		"-c", "core.excludesFile="+excludes, "check-ignore", "--no-index", "--stdin", "-z", "-v", "-n")
	if err != nil {
		t.Fatal(err)
	}
	// -z -v output: <source> NUL <linenum> NUL <pattern> NUL <path> NUL;
	// with -n, non-matching paths have empty source, linenum and pattern.
	fields := strings.Split(string(out), "\x00")
	ignored := map[string]int{}
	for i := 0; i+3 < len(fields); i += 4 {
		lineNum, pattern, p := fields[i+1], fields[i+2], fields[i+3]
		if pattern == "" || strings.HasPrefix(pattern, "!") {
			continue
		}
		n, err := strconv.Atoi(lineNum)
		if err != nil || n < 1 || n > len(lines) {
			t.Fatalf("unexpected check-ignore line number %q for %s", lineNum, p)
		}
		ignored[p] = n - 1
	}
	return ignored
}

// checkGcloudIgnoreAgainstEmbeds returns one violation per embedded file that
// the ignore file (name, content) would drop from the Cloud Build upload.
func checkGcloudIgnoreAgainstEmbeds(t *testing.T, name, content string, embedded []embeddedFile) []string {
	t.Helper()
	lines, err := expandGcloudIgnore(name, content, func(rel string) (string, error) {
		b, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(rel)))
		return string(b), err
	})
	if err != nil {
		return []string{err.Error()}
	}
	var paths []string
	byPath := map[string]embeddedFile{}
	for _, e := range embedded {
		// A package's own *_test.go files are swept in by a bare wildcard
		// (harnesses/embed.go: `all:*`) but are Go test sources, never read
		// through the embedded FS; every ignore file drops *_test.go on
		// purpose because Cloud Build does not run tests.
		//
		// The exemption is deliberately limited to the directive's own
		// directory. go:embed does embed *_test.go files at any depth, and
		// deeper files can be read: resources/catalog.go ships each
		// harnesses/<name>/ subtree whole (fs.Sub) as a harness-config
		// bundle, so a harnesses/<name>/foo_test.go would be part of that
		// bundle, and dropping it from the upload would make the Cloud Build
		// binary differ from a local build. Such a file should fail here.
		if strings.HasSuffix(e.path, "_test.go") && path.Dir(e.path) == path.Dir(strings.SplitN(e.directive, ":", 2)[0]) {
			continue
		}
		if _, dup := byPath[e.path]; !dup {
			paths = append(paths, e.path)
		}
		byPath[e.path] = e
	}
	sort.Strings(paths)
	var errs []string
	ignored := gitIgnoredBy(t, lines, paths)
	for _, p := range paths {
		if idx, ok := ignored[p]; ok {
			errs = append(errs, fmt.Sprintf("%s drops embedded file %s: pattern %q at %s; embedded by %s",
				name, p, lines[idx].text, lines[idx].origin, byPath[p].directive))
		}
	}
	return errs
}

// gcloudIgnoreFiles returns the Cloud Build ignore files: .gcloudignore and
// every tracked image-build/gcloudignore-*.
func gcloudIgnoreFiles(t *testing.T, tracked []string) []string {
	t.Helper()
	files := []string{".gcloudignore"}
	for _, f := range tracked {
		if ok, _ := path.Match("image-build/gcloudignore-*", f); ok {
			files = append(files, f)
		}
	}
	if len(files) < 2 {
		t.Fatalf("expected .gcloudignore and image-build/gcloudignore-* files; found %v", files)
	}
	return files
}

func TestGcloudIgnoreKeepsEmbeddedFiles(t *testing.T) {
	tracked := repoTrackedFiles(t)
	for _, name := range gcloudIgnoreFiles(t, tracked) {
		embedded := listGoEmbeddedFiles(t, tracked, gcloudIgnoreBuildTags[name])
		for _, e := range checkGcloudIgnoreAgainstEmbeds(t, name, readRepoFile(t, name), embedded) {
			t.Error(e)
		}
	}
}

func embeddedPaths(files []embeddedFile) map[string]bool {
	m := map[string]bool{}
	for _, e := range files {
		m[e.path] = true
	}
	return m
}

// TestGcloudIgnoreContractCheckerDetectsViolations proves the discovery and
// matching parts of the contract catch what they exist to catch.
func TestGcloudIgnoreContractCheckerDetectsViolations(t *testing.T) {
	tracked := repoTrackedFiles(t)

	// Embed discovery: files issue #3509 lists must be found from the
	// directives, and build tags must be honored.
	plain := embeddedPaths(listGoEmbeddedFiles(t, tracked, nil))
	for _, want := range []string{
		"pkg/config/embeds/templates/default/agents.md",                  // pkg/config/init.go all:embeds/*
		"resources/templates/default/home/.gemini/.geminiignore",         // resources/embed.go all:templates/*
		"harnesses/claude/home/.claude/settings.json",                    // harnesses/embed.go all:*
		"harnesses/scion_harness.py",                                     // harnesses/embed.go scion_harness.py
		"pkg/config/schemas/settings-v1.schema.json",                     // pkg/config/schema.go, no all:
		"web/dist/client/.gitkeep",                                       // web/embed.go, //go:build !no_embed_web
		"resources/platform_skills/scion-cli-operations/SKILL.md",        // resources/embed.go all:platform_skills/*
		"pkg/config/embeds/templates/default/home/.gemini/.geminiignore", // dot-dir under an all: root
	} {
		if !plain[want] {
			t.Errorf("embed discovery missed %s", want)
		}
	}
	if tagged := embeddedPaths(listGoEmbeddedFiles(t, tracked, []string{"no_embed_web"})); tagged["web/dist/client/.gitkeep"] {
		t.Error("embed discovery ignored //go:build !no_embed_web on web/embed.go")
	}
	for f := range plain {
		if strings.HasPrefix(f, "extras/") {
			t.Errorf("embed discovery included nested-module file %s", f)
		}
	}

	// go:embed rules without all:.
	for _, c := range []struct {
		pat, rel  string
		all, want bool
	}{
		{"dir", "dir/a", false, true},
		{"dir", "dir/.hidden", false, false},
		{"dir", "dir/_x/a", false, false},
		{"all:dir", "dir/.hidden", true, true},
		{"dir/.hidden", "dir/.hidden", false, true},
		{"*", "x/y", false, true},
		{"x/*", "y/z", false, false},
	} {
		if got := goEmbedPatternMatches(strings.TrimPrefix(c.pat, "all:"), c.rel, c.all); got != c.want {
			t.Errorf("goEmbedPatternMatches(%q, %q) = %v, want %v", c.pat, c.rel, got, c.want)
		}
	}

	// Matching: gitignore semantics via git check-ignore.
	emb := []embeddedFile{
		{path: "pkg/a/agents.md", directive: "pkg/a/x.go:1: //go:embed all:*"},
		{path: "pkg/a/.gemini/s.json", directive: "pkg/a/x.go:1: //go:embed all:*"},
		{path: "pkg/a/keep/f", directive: "pkg/a/x.go:1: //go:embed all:*"},
		{path: "pkg/a/x_test.go", directive: "pkg/a/x.go:1: //go:embed all:*"},
	}
	matchCases := []struct {
		name, content string
		want          []string // substrings, one violation each; nil = clean
	}{
		{"anchored root patterns", "/agents.md\n/.gemini/\n*_test.go\n", nil},
		{"unanchored file", "agents.md\n", []string{"drops embedded file pkg/a/agents.md"}},
		{"unanchored dir", ".gemini/\n", []string{"drops embedded file pkg/a/.gemini/s.json"}},
		{"glob", "pkg/*/agents.md\n", []string{"pkg/a/agents.md"}},
		{"double star", "**/keep/\n", []string{"pkg/a/keep/f"}},
		{"negation re-includes", ".gemini/\n!pkg/a/.gemini/\n", nil},
		{"excluded parent wins over file negation", "keep/\n!pkg/a/keep/f\n", []string{"pkg/a/keep/f"}},
		{"comment is not a pattern", "# agents.md\n", nil},
	}
	for _, c := range matchCases {
		errs := checkGcloudIgnoreAgainstEmbeds(t, "image-build/gcloudignore-mut", c.content, emb)
		if len(errs) != len(c.want) {
			t.Errorf("%s: got %d violations %q, want %d", c.name, len(errs), errs, len(c.want))
			continue
		}
		for i, w := range c.want {
			if !strings.Contains(errs[i], w) {
				t.Errorf("%s: violation %q does not contain %q", c.name, errs[i], w)
			}
		}
	}

	// The *_test.go exemption covers only the directive's own directory: a
	// test file deeper under an embed root is shipped with its subtree.
	nested := []embeddedFile{{path: "pkg/a/sub/y_test.go", directive: "pkg/a/x.go:1: //go:embed all:*"}}
	expectViolation(t, "nested *_test.go under an embed root",
		checkGcloudIgnoreAgainstEmbeds(t, "image-build/gcloudignore-mut", "*_test.go\n", nested),
		"drops embedded file pkg/a/sub/y_test.go")

	// #!include: expansion follows gcloud: relative to the ignore file's
	// directory, one level deep, no "/" in the name.
	files := map[string]string{
		"sub/.inc":    "agents.md\n#!include:.deeper\n",
		"sub/.deeper": ".gemini/\n",
		".inc":        "/never\n",
	}
	read := func(p string) (string, error) {
		if s, ok := files[p]; ok {
			return s, nil
		}
		return "", os.ErrNotExist
	}
	lines, err := expandGcloudIgnore("sub/ign", "#!include:.inc\n# !include:.inc\n", read)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, l := range lines {
		texts = append(texts, l.text)
	}
	if got := strings.Join(texts, "|"); !strings.Contains(got, "agents.md") || strings.Contains(got, ".gemini/") || strings.Contains(got, "/never") {
		t.Errorf("include expansion = %q; want sub/.inc twice (gcloud allows a space after #), not sub/.deeper or the root .inc", got)
	}
	if _, err := expandGcloudIgnore("sub/ign", "#!include:../.inc\n", read); err == nil {
		t.Error("include with a path separator should be rejected")
	}
	if _, err := expandGcloudIgnore("sub/ign", "#!include:.missing\n", read); err == nil {
		t.Error("include of a missing file should be an error")
	}

	// Against the real files: reverting a fix reproduces issue #3509.
	omni := readRepoFile(t, "image-build/gcloudignore-omni")
	omniEmb := listGoEmbeddedFiles(t, tracked, nil)
	expectViolation(t, "omni agents.md unanchored",
		checkGcloudIgnoreAgainstEmbeds(t, "image-build/gcloudignore-omni", mustReplace(t, omni, "\n/agents.md\n", "\nagents.md\n"), omniEmb),
		"drops embedded file pkg/config/embeds/templates/default/agents.md")
	expectViolation(t, "omni .claude/ unanchored",
		checkGcloudIgnoreAgainstEmbeds(t, "image-build/gcloudignore-omni", mustReplace(t, omni, "\n/.claude/\n", "\n.claude/\n"), omniEmb),
		"drops embedded file harnesses/claude/home/.claude/settings.json")
	expectViolation(t, "omni .gemini/ unanchored",
		checkGcloudIgnoreAgainstEmbeds(t, "image-build/gcloudignore-omni", mustReplace(t, omni, "\n/.gemini/\n", "\n.gemini/\n"), omniEmb),
		"drops embedded file resources/templates/default/home/.gemini/.geminiignore")
	def := readRepoFile(t, ".gcloudignore")
	defEmb := listGoEmbeddedFiles(t, tracked, gcloudIgnoreBuildTags[".gcloudignore"])
	expectViolation(t, ".gcloudignore README.md unanchored",
		checkGcloudIgnoreAgainstEmbeds(t, ".gcloudignore", mustReplace(t, def, "\n/README.md\n", "\nREADME.md\n"), defEmb),
		"drops embedded file harnesses/claude/README.md")
	// The root .gitignore (pulled in by #!include) has an any-depth .claude/.
	expectViolation(t, ".gcloudignore without the .claude re-include",
		checkGcloudIgnoreAgainstEmbeds(t, ".gcloudignore", mustReplace(t, def, "\n!/harnesses/*/home/.claude/\n", "\n"), defEmb),
		"(via .gcloudignore:")
}
