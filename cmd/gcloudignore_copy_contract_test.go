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

// Contract: every build step of every in-repo Cloud Build config can COPY its
// sources after the ignore file that build uploads with has filtered the
// source. A Dockerfile that COPYs a path the ignore file drops builds with
// `docker build` but fails under `gcloud builds submit` (ptone/scion#3555: the
// default .gcloudignore drops web/, which the repo-root Dockerfile's frontend
// stage copies). This complements gcloudignore_contract_test.go, which checks
// embedded files.
//
// How it works:
//   - Build steps are read from every tracked image-build/cloudbuild*.yaml: a
//     step whose args run `build` (docker build or buildx build). Its
//     Dockerfile (-f, default Dockerfile) and context (the last arg) are
//     resolved against the step's dir.
//   - The ignore file of a config is the one its header documents with
//     `--ignore-file=<file>`; a config that documents none uploads with the
//     default .gcloudignore. image-build/scripts/builders/cloud-build.sh passes
//     image-build/gcloudignore-<target> when that file exists, so for each
//     such file the matching cloudbuild-<target>.yaml must document it.
//   - For each COPY/ADD source in the build context (not --from=, not a URL),
//     the tracked files it names must not all be dropped by the ignore file.
//     Matching reuses gitIgnoredBy (real gitignore semantics, see
//     gcloudignore_contract_test.go).
//
// Not covered: the per-harness config that cloud-build.sh generates on the
// fly (it mirrors cloudbuild-harnesses.yaml), and builds started by hand from
// docs; the repo-root Dockerfile's header documents its ignore file.

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// cbContractStep is the part of a Cloud Build step this contract reads.
type cbContractStep struct {
	ID   string   `yaml:"id"`
	Name string   `yaml:"name"`
	Dir  string   `yaml:"dir"`
	Args []string `yaml:"args"`
}

// dockerBuild is one Dockerfile build: which file, which context, which
// ignore file filters the upload, and where it came from.
type dockerBuild struct {
	origin     string // "<config> step <id>"
	dockerfile string // repo-relative
	context    string // repo-relative, "." for the repo root
	ignoreFile string // repo-relative
}

var ignoreFileFlagRE = regexp.MustCompile(`--ignore-file=(\S+)`)

// cloudBuildIgnoreFile returns the ignore file a Cloud Build config uploads
// with: the one its comments document, else the default .gcloudignore.
func cloudBuildIgnoreFile(content string) string {
	for _, line := range strings.Split(content, "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, "#") {
			continue
		}
		if m := ignoreFileFlagRE.FindStringSubmatch(l); m != nil {
			return strings.TrimSuffix(m[1], "\\")
		}
	}
	return ".gcloudignore"
}

// cloudBuildDockerBuilds returns the Dockerfile builds of a Cloud Build config.
func cloudBuildDockerBuilds(name, content string) ([]dockerBuild, error) {
	var cfg struct {
		Steps []cbContractStep `yaml:"steps"`
	}
	if err := yaml.Unmarshal([]byte(content), &cfg); err != nil {
		return nil, fmt.Errorf("%s: %v", name, err)
	}
	ignore := cloudBuildIgnoreFile(content)
	var out []dockerBuild
	for i, st := range cfg.Steps {
		isBuild := false
		for _, a := range st.Args {
			if a == "build" {
				isBuild = true
				break
			}
		}
		if !isBuild || !strings.Contains(st.Name, "docker") || len(st.Args) == 0 {
			continue
		}
		id := st.ID
		if id == "" {
			id = fmt.Sprintf("#%d", i)
		}
		dir := st.Dir
		if dir == "" {
			dir = "."
		}
		dockerfile, err := dockerfileFlag(st.Args)
		if err != nil {
			return nil, fmt.Errorf("%s step %s: %v", name, id, err)
		}
		if dockerfile == "" {
			dockerfile = "Dockerfile"
		}
		ctx := st.Args[len(st.Args)-1]
		if strings.HasPrefix(ctx, "-") {
			return nil, fmt.Errorf("%s step %s: last arg %q is not a build context", name, id, ctx)
		}
		out = append(out, dockerBuild{
			origin:     name + " step " + id,
			dockerfile: path.Clean(path.Join(dir, dockerfile)),
			context:    path.Clean(path.Join(dir, ctx)),
			ignoreFile: ignore,
		})
	}
	return out, nil
}

// dockerfileFlag returns the Dockerfile a docker build step names with -f or
// --file, in every form the docker CLI accepts: "-f X", "--file X",
// "--file=X", "-f=X" and "-fX". It returns "" when the step names none (the
// build then uses the Dockerfile in its directory). An empty or missing value,
// an unrecognised -f or --file token, more than one Dockerfile flag, or a flag
// whose value would be the step's build context (its last arg) is an error:
// a flag read wrongly would make the contract check the wrong Dockerfile.
func dockerfileFlag(args []string) (string, error) {
	file, found := "", false
	set := func(v, flag string) error {
		if found {
			return fmt.Errorf("more than one Dockerfile flag (%s)", flag)
		}
		if v == "" {
			return fmt.Errorf("%s with an empty value", flag)
		}
		file, found = v, true
		return nil
	}
	last := len(args) - 1
	for j := 0; j < len(args); j++ {
		a := args[j]
		var err error
		switch {
		case a == "-f" || a == "--file":
			if j+1 >= last {
				return "", fmt.Errorf("%s with no value before the build context", a)
			}
			j++
			err = set(args[j], a)
		case strings.HasPrefix(a, "--file="):
			err = set(strings.TrimPrefix(a, "--file="), "--file=")
		case strings.HasPrefix(a, "--file"):
			return "", fmt.Errorf("unrecognised Dockerfile flag %q", a)
		case strings.HasPrefix(a, "-f="):
			err = set(strings.TrimPrefix(a, "-f="), "-f=")
		case strings.HasPrefix(a, "-f") && !strings.HasPrefix(a, "--"):
			err = set(strings.TrimPrefix(a, "-f"), "-f")
		}
		if err != nil {
			return "", err
		}
	}
	return file, nil
}

// dockerCopySources returns the build-context sources of every COPY and ADD
// instruction in a Dockerfile: not --from= (another stage or image), not a URL.
func dockerCopySources(t *testing.T, content string) []string {
	t.Helper()
	var srcs []string
	for _, st := range parseDockerfileStages(t, content) {
		for _, in := range st.instrs {
			if in.cmd != "COPY" && in.cmd != "ADD" {
				continue
			}
			var args []string
			if strings.HasPrefix(in.args, "[") {
				if err := json.Unmarshal([]byte(in.args), &args); err != nil {
					t.Fatalf("%s %s: bad JSON form: %v", in.cmd, in.args, err)
				}
			} else {
				args = strings.Fields(in.args)
			}
			fromStage := false
			var paths []string
			for _, a := range args {
				if strings.HasPrefix(a, "--") {
					if strings.HasPrefix(a, "--from=") {
						fromStage = true
					}
					continue
				}
				paths = append(paths, a)
			}
			if fromStage || len(paths) < 2 {
				continue
			}
			for _, s := range paths[:len(paths)-1] { // the last path is the destination
				if strings.Contains(s, "://") {
					continue
				}
				srcs = append(srcs, s)
			}
		}
	}
	return srcs
}

// trackedUnder returns the tracked files a COPY source (relative to the build
// context) names: the file itself, everything under a directory, or the glob
// matches (and everything under matched directories).
func trackedUnder(tracked []string, context, src string) []string {
	rel := path.Clean(strings.TrimPrefix(src, "/"))
	full := rel
	if context != "." {
		full = path.Join(context, rel)
	}
	var out []string
	for _, f := range tracked {
		if rel == "." {
			if context == "." || strings.HasPrefix(f, context+"/") {
				out = append(out, f)
			}
			continue
		}
		if f == full || strings.HasPrefix(f, full+"/") {
			out = append(out, f)
			continue
		}
		comps := strings.Split(f, "/")
		for i := 1; i <= len(comps); i++ {
			if ok, _ := path.Match(full, strings.Join(comps[:i], "/")); ok {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// checkDockerBuildCopySources returns one violation per COPY/ADD source of the
// build that names no tracked file, or whose tracked files the build's ignore
// file all drops.
func checkDockerBuildCopySources(t *testing.T, tracked []string, b dockerBuild, dockerfileContent string) []string {
	t.Helper()
	lines, err := expandGcloudIgnore(b.ignoreFile, readRepoFile(t, b.ignoreFile), func(rel string) (string, error) {
		return readRepoFile(t, rel), nil
	})
	if err != nil {
		return []string{fmt.Sprintf("%s: %v", b.origin, err)}
	}
	var errs []string
	for _, src := range dockerCopySources(t, dockerfileContent) {
		if strings.Contains(src, "$") {
			errs = append(errs, fmt.Sprintf("%s: %s copies %q, which uses a variable this contract cannot resolve", b.origin, b.dockerfile, src))
			continue
		}
		files := trackedUnder(tracked, b.context, src)
		if len(files) == 0 {
			errs = append(errs, fmt.Sprintf("%s: %s copies %q, which names no tracked file in context %s", b.origin, b.dockerfile, src, b.context))
			continue
		}
		ignored := gitIgnoredBy(t, lines, files)
		if len(ignored) < len(files) {
			continue
		}
		sample := files[0]
		l := lines[ignored[sample]]
		errs = append(errs, fmt.Sprintf("%s: %s copies %q (context %s), but %s drops all of it (e.g. %s, pattern %q at %s); upload with an ignore file that keeps it",
			b.origin, b.dockerfile, src, b.context, b.ignoreFile, sample, l.text, l.origin))
	}
	return errs
}

// cloudBuildConfigs returns the tracked image-build/cloudbuild*.yaml files.
func cloudBuildConfigs(t *testing.T, tracked []string) []string {
	t.Helper()
	var out []string
	for _, f := range tracked {
		if ok, _ := path.Match("image-build/cloudbuild*.yaml", f); ok {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("no image-build/cloudbuild*.yaml found")
	}
	return out
}

func TestCloudBuildCopySourcesSurviveIgnoreFile(t *testing.T) {
	tracked := repoTrackedFiles(t)
	total := 0
	for _, cfg := range cloudBuildConfigs(t, tracked) {
		builds, err := cloudBuildDockerBuilds(cfg, readRepoFile(t, cfg))
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range builds {
			total++
			for _, e := range checkDockerBuildCopySources(t, tracked, b, readRepoFile(t, b.dockerfile)) {
				t.Error(e)
			}
		}
	}
	if total == 0 {
		t.Fatal("found no Dockerfile build steps in image-build/cloudbuild*.yaml")
	}
	t.Logf("checked %d Dockerfile build steps", total)
}

// TestCloudBuildConfigsDocumentTargetIgnoreFile: cloud-build.sh uploads target
// <t> with image-build/gcloudignore-<t> when it exists, so cloudbuild-<t>.yaml
// must document that same file, or the check above would use the wrong one.
func TestCloudBuildConfigsDocumentTargetIgnoreFile(t *testing.T) {
	tracked := repoTrackedFiles(t)
	trackedSet := map[string]bool{}
	for _, f := range tracked {
		trackedSet[f] = true
	}
	for _, ig := range gcloudIgnoreFiles(t, tracked) {
		target, ok := strings.CutPrefix(ig, "image-build/gcloudignore-")
		if !ok {
			continue
		}
		cfg := "image-build/cloudbuild-" + target + ".yaml"
		if !trackedSet[cfg] {
			continue
		}
		if got := cloudBuildIgnoreFile(readRepoFile(t, cfg)); got != ig {
			t.Errorf("%s documents --ignore-file=%s, but cloud-build.sh uploads target %s with %s", cfg, got, target, ig)
		}
	}
}

// TestCloudBuildCopyContractDetectsViolations proves the checker catches the
// #3555 failure and resolves steps the way Cloud Build does.
func TestCloudBuildCopyContractDetectsViolations(t *testing.T) {
	tracked := repoTrackedFiles(t)
	root := readRepoFile(t, "Dockerfile")

	// The repo-root Dockerfile with the default .gcloudignore: its frontend
	// stage copies web/package.json, which that file drops.
	bad := dockerBuild{origin: "synthetic", dockerfile: "Dockerfile", context: ".", ignoreFile: ".gcloudignore"}
	expectViolation(t, "root Dockerfile with default ignore", checkDockerBuildCopySources(t, tracked, bad, root), `copies "web/package.json"`)

	// The same Dockerfile with the hub-gke ignore file is fine.
	good := bad
	good.ignoreFile = hubGKEIgnoreFile
	if errs := checkDockerBuildCopySources(t, tracked, good, root); len(errs) != 0 {
		t.Errorf("root Dockerfile with %s reported violations: %q", hubGKEIgnoreFile, errs)
	}

	// A source that names nothing tracked, and an unresolvable variable.
	missing := root + "\nFROM runtime AS x\nCOPY no/such/file /x\nCOPY ${SRC} /y\n"
	errs := checkDockerBuildCopySources(t, tracked, good, missing)
	expectViolation(t, "missing source", errs, `"no/such/file", which names no tracked file`)
	expectViolation(t, "variable source", errs, "uses a variable")

	// Step resolution: dir, -f relative to dir, context, documented ignore file.
	cfg := "# gcloud builds submit --ignore-file=image-build/gcloudignore-omni \\\nsteps:\n" +
		"  - name: 'gcr.io/cloud-builders/docker'\n    id: 'a'\n    dir: 'image-build'\n" +
		"    args: ['buildx', 'build', '-f', 'core-base/Dockerfile', '--push', '.']\n" +
		"  - name: 'gcr.io/cloud-builders/docker'\n    id: 'b'\n    args: ['buildx', 'create', '--use']\n" +
		"  - name: 'gcr.io/cloud-builders/docker'\n    id: 'c'\n    dir: 'harnesses/claude'\n    args: ['build', '.']\n"
	builds, err := cloudBuildDockerBuilds("cfg.yaml", cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []dockerBuild{
		{origin: "cfg.yaml step a", dockerfile: "image-build/core-base/Dockerfile", context: "image-build", ignoreFile: "image-build/gcloudignore-omni"},
		{origin: "cfg.yaml step c", dockerfile: "harnesses/claude/Dockerfile", context: "harnesses/claude", ignoreFile: "image-build/gcloudignore-omni"},
	}
	if fmt.Sprint(builds) != fmt.Sprint(want) {
		t.Errorf("step resolution:\n got %+v\nwant %+v", builds, want)
	}
	if got := cloudBuildIgnoreFile("steps: []\n"); got != ".gcloudignore" {
		t.Errorf("undocumented ignore file resolved to %q, want .gcloudignore", got)
	}

	// COPY parsing: --from, --chown, JSON form, URL, multiple sources.
	srcs := dockerCopySources(t, "FROM a\nCOPY --from=b /x /y\nCOPY --chown=1:1 a b /dst/\nCOPY [\"c d\", \"/e\"]\nADD https://h/f /g\n")
	if fmt.Sprint(srcs) != fmt.Sprint([]string{"a", "b", "c d"}) {
		t.Errorf("COPY sources: got %q", srcs)
	}
}

// TestCloudBuildDockerfileFlagForms: every form of the docker -f flag names
// the same Dockerfile, and a flag that does not resolve to exactly one
// Dockerfile fails the config instead of falling back to <dir>/Dockerfile.
func TestCloudBuildDockerfileFlagForms(t *testing.T) {
	step := func(args ...string) string {
		parts := append([]string{"build"}, args...)
		parts = append(parts, ".")
		quoted := make([]string, len(parts))
		for i, a := range parts {
			quoted[i] = "'" + a + "'"
		}
		return "steps:\n  - name: 'gcr.io/cloud-builders/docker'\n    id: 's'\n    dir: 'image-build'\n" +
			"    args: [" + strings.Join(quoted, ", ") + "]\n"
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"-f X", []string{"-f", "sub/Dockerfile"}, "image-build/sub/Dockerfile"},
		{"--file X", []string{"--file", "sub/Dockerfile"}, "image-build/sub/Dockerfile"},
		{"--file=X", []string{"--file=sub/Dockerfile"}, "image-build/sub/Dockerfile"},
		{"-f=X", []string{"-f=sub/Dockerfile"}, "image-build/sub/Dockerfile"},
		{"-fX", []string{"-fsub/Dockerfile"}, "image-build/sub/Dockerfile"},
		{"no flag", nil, "image-build/Dockerfile"},
	} {
		builds, err := cloudBuildDockerBuilds("cfg.yaml", step(tc.args...))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(builds) != 1 || builds[0].dockerfile != tc.want || builds[0].context != "image-build" {
			t.Errorf("%s: got %+v, want dockerfile %s with context image-build", tc.name, builds, tc.want)
		}
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"-f= empty", []string{"-f="}, "-f= with an empty value"},
		{"--file= empty", []string{"--file="}, "--file= with an empty value"},
		{"-f empty arg", []string{"-f", ""}, "-f with an empty value"},
		{"two flags", []string{"-f", "a/Dockerfile", "--file=b/Dockerfile"}, "more than one Dockerfile flag"},
		{"unrecognised --file form", []string{"--filex=a"}, "unrecognised Dockerfile flag"},
		{"-f takes the context", []string{"-f"}, "-f with no value before the build context"},
	} {
		_, err := cloudBuildDockerBuilds("cfg.yaml", step(tc.args...))
		if err == nil || !strings.Contains(err.Error(), "cfg.yaml step s: ") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want an error for cfg.yaml step s containing %q", tc.name, err, tc.want)
		}
	}
}
