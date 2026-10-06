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

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// dockerfileInstr is one logical Dockerfile instruction (continuations joined).
type dockerfileInstr struct {
	cmd  string // upper-cased instruction keyword, e.g. "FROM"
	args string // remainder of the instruction, whitespace-trimmed
}

// dockerfileStage is a build stage: its FROM base, optional name, and body.
// base and name are lower-cased, because BuildKit matches stage names
// case-insensitively.
type dockerfileStage struct {
	base   string
	name   string
	instrs []dockerfileInstr
}

// parseDockerfileStages is a minimal Dockerfile parser: it drops comment and
// blank lines (including those inside a backslash continuation, as Docker
// does), joins backslash continuations, and splits on FROM. It is enough to
// check stage structure; it is not a general Dockerfile parser. In particular
// it does not handle parser directives (`# escape=`) or heredocs (`RUN <<EOF`);
// the root Dockerfile uses neither.
func parseDockerfileStages(t *testing.T, content string) []dockerfileStage {
	t.Helper()
	var logical []string
	var cur strings.Builder
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasSuffix(trimmed, "\\") {
			cur.WriteString(strings.TrimSuffix(trimmed, "\\"))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(trimmed)
		logical = append(logical, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		logical = append(logical, cur.String())
	}

	var stages []dockerfileStage
	for _, l := range logical {
		fields := strings.Fields(l)
		in := dockerfileInstr{cmd: strings.ToUpper(fields[0]), args: strings.TrimSpace(strings.TrimPrefix(l, fields[0]))}
		if in.cmd == "FROM" {
			st := dockerfileStage{}
			var parts []string
			for _, f := range strings.Fields(in.args) {
				if !strings.HasPrefix(f, "--") { // skip --platform=...
					parts = append(parts, f)
				}
			}
			if len(parts) == 0 {
				t.Fatalf("FROM with no image: %q", l)
			}
			st.base = strings.ToLower(parts[0])
			if len(parts) == 3 && strings.EqualFold(parts[1], "AS") {
				st.name = strings.ToLower(parts[2])
			}
			stages = append(stages, st)
			continue
		}
		if len(stages) == 0 {
			if in.cmd == "ARG" {
				continue // global ARG before the first FROM
			}
			t.Fatalf("instruction before first FROM: %q", l)
		}
		stages[len(stages)-1].instrs = append(stages[len(stages)-1].instrs, in)
	}
	return stages
}

func findStage(stages []dockerfileStage, name string) *dockerfileStage {
	for i := range stages {
		if stages[i].name == strings.ToLower(name) {
			return &stages[i]
		}
	}
	return nil
}

// stageChain returns the named stage followed by every stage it inherits from
// through FROM <stage>, stopping at the first external base image.
func stageChain(stages []dockerfileStage, name string) []*dockerfileStage {
	var chain []*dockerfileStage
	seen := map[string]bool{}
	for st := findStage(stages, name); st != nil && !seen[st.name]; st = findStage(stages, st.base) {
		seen[st.name] = true
		chain = append(chain, st)
	}
	return chain
}

// envOrArgKeys returns the variable names an ENV or ARG instruction sets. It
// handles `K=V [K2=V2 ...]` and the legacy `ENV K V` form; values with
// embedded spaces may yield extra tokens, which only makes it stricter.
func envOrArgKeys(args string) []string {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return nil
	}
	if !strings.Contains(fields[0], "=") {
		return []string{fields[0]} // ENV K V, or ARG K
	}
	var keys []string
	for _, f := range fields {
		k, _, _ := strings.Cut(f, "=")
		keys = append(keys, k)
	}
	return keys
}

// envValue returns the value the last ENV in the stage assigns to key.
func envValue(st *dockerfileStage, key string) (string, bool) {
	var val string
	var found bool
	for _, in := range st.instrs {
		if in.cmd != "ENV" {
			continue
		}
		fields := strings.Fields(in.args)
		if len(fields) >= 2 && !strings.Contains(fields[0], "=") && fields[0] == key {
			val, found = strings.Join(fields[1:], " "), true
			continue
		}
		for _, f := range fields {
			if k, v, ok := strings.Cut(f, "="); ok && k == key {
				val, found = strings.Trim(v, `"'`), true
			}
		}
	}
	return val, found
}

// checkRootDockerfileContract returns the list of contract violations for the
// repo-root Dockerfile. See the stage comments in that file for the reasons.
// The runtime stage's body is deliberately not pinned instruction by
// instruction; only the properties below are.
func checkRootDockerfileContract(t *testing.T, content string) []string {
	t.Helper()
	stages := parseDockerfileStages(t, content)
	var errs []string
	if len(stages) == 0 {
		return []string{"no stages found"}
	}

	// Default target (last stage) must be exactly `FROM runtime`, empty.
	last := stages[len(stages)-1]
	if last.base != "runtime" || last.name != "" || len(last.instrs) != 0 {
		errs = append(errs, "final stage must be a bare, empty `FROM runtime` so the default build target is the runtime image; got FROM "+last.base+" AS "+last.name)
	}

	runtime := findStage(stages, "runtime")
	if runtime == nil {
		errs = append(errs, "no stage named runtime")
	} else {
		var entrypoint string
		for _, in := range runtime.instrs {
			if in.cmd == "ENTRYPOINT" {
				entrypoint = in.args
			}
			if in.cmd == "USER" {
				errs = append(errs, "runtime stage must not set USER (it would change the default image): USER "+in.args)
			}
		}
		if entrypoint != `["/usr/local/bin/scion"]` {
			errs = append(errs, `runtime stage ENTRYPOINT must be ["/usr/local/bin/scion"]; got `+entrypoint)
		}
	}

	gke := findStage(stages, "hub-gke")
	if gke == nil {
		errs = append(errs, "no stage named hub-gke")
		return errs
	}
	if gke.base != "runtime" {
		errs = append(errs, "hub-gke must be FROM runtime; got FROM "+gke.base)
	}
	var user string
	var runs []string
	for _, in := range gke.instrs {
		switch in.cmd {
		case "USER":
			user = in.args
		case "CMD":
			errs = append(errs, "hub-gke must not set CMD: CMD "+in.args)
		case "ENTRYPOINT":
			errs = append(errs, "hub-gke must inherit ENTRYPOINT from runtime: ENTRYPOINT "+in.args)
		case "RUN":
			runs = append(runs, strings.Join(strings.Fields(in.args), " "))
		}
	}
	if user != "1000:1000" {
		errs = append(errs, "hub-gke final USER must be 1000:1000; got "+user)
	}
	if home, ok := envValue(gke, "HOME"); !ok || home != "/home/scion" {
		errs = append(errs, "hub-gke must set ENV HOME=/home/scion; got "+home)
	}
	allRuns := strings.Join(runs, "\n")
	for _, want := range []string{
		"useradd -u 1000 ",            // the uid the chart's runAsUser assumes, and the passwd entry ssh needs
		"mkdir -p /home/scion/.scion", // ~/.scion exists and is created before the chown
		"chown -R 1000:1000 /home/scion",
	} {
		if !strings.Contains(allRuns, want) {
			errs = append(errs, "hub-gke must RUN `"+strings.TrimSpace(want)+"`")
		}
	}

	// No KUBECONFIG in hub-gke or in any stage it inherits from: a baked
	// KUBECONFIG pointing at a loadable file beats in-cluster auth.
	for _, st := range stageChain(stages, "hub-gke") {
		for _, in := range st.instrs {
			if in.cmd != "ENV" && in.cmd != "ARG" {
				continue
			}
			for _, k := range envOrArgKeys(in.args) {
				if strings.EqualFold(k, "KUBECONFIG") {
					errs = append(errs, "stage "+st.name+" (hub-gke or a stage it inherits from) must not set "+in.cmd+" KUBECONFIG: "+in.cmd+" "+in.args)
				}
			}
		}
	}
	return errs
}

// cloudBuildFile is the subset of a Cloud Build config the checker reads.
type cloudBuildFile struct {
	Steps []struct {
		Name       string   `yaml:"name"`
		ID         string   `yaml:"id"`
		Entrypoint string   `yaml:"entrypoint"`
		Args       []string `yaml:"args"`
	} `yaml:"steps"`
	Substitutions map[string]string `yaml:"substitutions"`
}

const (
	hubGKEImageTag   = "$_REGISTRY/scion-hub-gke:$_SHORT_SHA"
	hubGKEIgnoreFile = "image-build/gcloudignore-hub-gke"
)

// checkHubGKECloudBuildContract returns violations for the hub-gke Cloud Build
// file. It parses the YAML and inspects step args and substitutions, so
// comments (e.g. "never latest") do not trip it.
func checkHubGKECloudBuildContract(content string) []string {
	var cb cloudBuildFile
	if err := yaml.Unmarshal([]byte(content), &cb); err != nil {
		return []string{"cloudbuild-hub-gke.yaml does not parse: " + err.Error()}
	}
	var errs []string

	for k, v := range cb.Substitutions {
		if strings.Contains(k, "_TAG") {
			errs = append(errs, "cloudbuild-hub-gke.yaml must not define a _TAG substitution: "+k)
		}
		if strings.Contains(strings.ToLower(v), "latest") {
			errs = append(errs, "cloudbuild-hub-gke.yaml substitution "+k+" must not mention latest: "+v)
		}
	}

	buildIdx, shaIdx := -1, -1
	for i, s := range cb.Steps {
		joined := strings.Join(s.Args, " ")
		if strings.Contains(strings.ToLower(joined), "latest") {
			errs = append(errs, "cloudbuild-hub-gke.yaml step "+s.ID+" args must not mention latest (only the $_SHORT_SHA tag is pushed)")
		}
		if strings.Contains(joined, "_TAG") {
			errs = append(errs, "cloudbuild-hub-gke.yaml step "+s.ID+" args must not use a _TAG substitution")
		}
		if len(s.Args) >= 2 && s.Args[0] == "buildx" && s.Args[1] == "build" {
			if buildIdx >= 0 {
				errs = append(errs, "cloudbuild-hub-gke.yaml must have exactly one buildx build step")
			}
			buildIdx = i
		}
		if s.ID == "require-short-sha" && strings.Contains(joined, `test -n "$_SHORT_SHA"`) {
			shaIdx = i
		}
	}
	if shaIdx < 0 {
		errs = append(errs, "cloudbuild-hub-gke.yaml must have a require-short-sha step that runs `test -n \"$_SHORT_SHA\"`")
	}
	if buildIdx < 0 {
		return append(errs, "cloudbuild-hub-gke.yaml has no buildx build step")
	}
	if shaIdx >= 0 && shaIdx > buildIdx {
		errs = append(errs, "cloudbuild-hub-gke.yaml require-short-sha step must precede the build step")
	}

	// Collect the build step's flags, accepting both "--flag value" and
	// "--flag=value".
	var tags []string
	flags := map[string][]string{}
	push := false
	args := cb.Steps[buildIdx].Args[2:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasEq := strings.Cut(a, "=")
		switch name {
		case "--push":
			push = true
			continue
		case "-t", "--tag", "-f", "--file", "--platform", "--target":
		default:
			continue
		}
		if !hasEq {
			if i+1 >= len(args) {
				errs = append(errs, "cloudbuild-hub-gke.yaml build flag "+name+" has no value")
				continue
			}
			i++
			val = args[i]
		}
		switch name {
		case "-t", "--tag":
			tags = append(tags, val)
		case "-f", "--file":
			flags["-f"] = append(flags["-f"], val)
		default:
			flags[name] = append(flags[name], val)
		}
	}
	if len(tags) != 1 || tags[0] != hubGKEImageTag {
		errs = append(errs, "cloudbuild-hub-gke.yaml must pass exactly one -t, "+hubGKEImageTag+"; got "+strings.Join(tags, ", "))
	}
	if !push {
		errs = append(errs, "cloudbuild-hub-gke.yaml build step must pass --push")
	}
	for flag, want := range map[string]string{"-f": "Dockerfile", "--platform": "linux/amd64", "--target": "hub-gke"} {
		if got := flags[flag]; len(got) != 1 || got[0] != want {
			errs = append(errs, "cloudbuild-hub-gke.yaml build step must pass "+flag+" "+want+"; got "+strings.Join(got, ", "))
		}
	}

	// The ignore file is only named in the usage comment; check it textually.
	if !strings.Contains(content, "--ignore-file="+hubGKEIgnoreFile) {
		errs = append(errs, "cloudbuild-hub-gke.yaml usage must pass --ignore-file="+hubGKEIgnoreFile)
	}
	if strings.Contains(content, "--ignore-file=image-build/gcloudignore-omni") {
		errs = append(errs, "cloudbuild-hub-gke.yaml must not point at gcloudignore-omni as its ignore file")
	}
	return errs
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func TestRootDockerfileHubGKEContract(t *testing.T) {
	for _, e := range checkRootDockerfileContract(t, readRepoFile(t, "Dockerfile")) {
		t.Error(e)
	}
}

func TestDockerfileContractHubGKECloudBuild(t *testing.T) {
	for _, e := range checkHubGKECloudBuildContract(readRepoFile(t, "image-build/cloudbuild-hub-gke.yaml")) {
		t.Error(e)
	}
}

// TestDockerfileContractHubGKEIgnoreFile checks the README points at the
// hub-gke ignore file. That no ignore file drops embedded files is checked by
// TestGcloudIgnoreKeepsEmbeddedFiles (gcloudignore_contract_test.go).
func TestDockerfileContractHubGKEIgnoreFile(t *testing.T) {
	readme := readRepoFile(t, "image-build/README.md")
	if !strings.Contains(readme, "--ignore-file="+hubGKEIgnoreFile) {
		t.Error("image-build/README.md must pass --ignore-file=" + hubGKEIgnoreFile + " for the hub-gke build")
	}
}

// mustReplace applies a mutation and fails if it did not change the input.
func mustReplace(t *testing.T, s, old, new string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("mutation anchor not found: %q", old)
	}
	return strings.Replace(s, old, new, 1)
}

// expectViolation fails unless errs contains a message containing want.
func expectViolation(t *testing.T, name string, errs []string, want string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e, want) {
			return
		}
	}
	t.Errorf("%s: expected a violation containing %q; got %q", name, want, errs)
}

// TestDockerfileContractCheckerDetectsViolations proves each checker assertion
// fails on the mutation it exists to catch (by its specific message), and that
// the parser does not report violations on valid inputs it used to misread.
func TestDockerfileContractCheckerDetectsViolations(t *testing.T) {
	good := readRepoFile(t, "Dockerfile")
	idx := strings.LastIndex(good, "\nFROM runtime\n")
	if idx < 0 {
		t.Fatal("trailing FROM runtime not found")
	}
	const homeLine = "ENV HOME=/home/scion\n"
	dockerMutations := []struct{ name, src, want string }{
		{"trailing stage removed", good[:idx+1], "final stage must be"},
		{"stage appended after it", good + "\nFROM runtime AS extra\nRUN true\n", "final stage must be"},
		{"hub-gke CMD added", mustReplace(t, good, "USER 1000:1000\n", "USER 1000:1000\nCMD [\"server\"]\n"), "must not set CMD"},
		{"hub-gke USER root", mustReplace(t, good, "USER 1000:1000\n", "USER 0:0\n"), "final USER must be 1000:1000"},
		{"runtime ENTRYPOINT moved", mustReplace(t, good, `ENTRYPOINT ["/usr/local/bin/scion"]`, `ENTRYPOINT ["/bin/sh"]`), "ENTRYPOINT must be"},
		{"hub-gke from debian", mustReplace(t, good, "FROM runtime AS hub-gke", "FROM debian:bookworm-slim AS hub-gke"), "must be FROM runtime"},
		{"hub-gke HOME removed", mustReplace(t, good, homeLine, ""), "ENV HOME=/home/scion"},
		{"hub-gke HOME=/root", mustReplace(t, good, homeLine, "ENV HOME=/root\n"), "ENV HOME=/home/scion"},
		{"hub-gke HOME overridden later", mustReplace(t, good, homeLine, homeLine+"ENV HOME /root\n"), "ENV HOME=/home/scion"},
		{"useradd uid changed", mustReplace(t, good, "useradd -u 1000 ", "useradd -u 1001 "), "useradd -u 1000"},
		{"mkdir ~/.scion removed", mustReplace(t, good, " && mkdir -p /home/scion/.scion \\\n", ""), "mkdir -p /home/scion/.scion"},
		{"chown changed", mustReplace(t, good, "chown -R 1000:1000 /home/scion", "chown -R 1000 /home/scion"), "chown -R 1000:1000 /home/scion"},
		{"hub-gke ENV KUBECONFIG", mustReplace(t, good, homeLine, "ENV HOME=/home/scion KUBECONFIG=/k\n"), "must not set ENV KUBECONFIG"},
		{"hub-gke ARG KUBECONFIG", mustReplace(t, good, homeLine, homeLine+"ARG KUBECONFIG\n"), "must not set ARG KUBECONFIG"},
		{"runtime ENV KUBECONFIG", mustReplace(t, good, "EXPOSE 8080\n", "EXPOSE 8080\nENV KUBECONFIG=/k\n"), "stage runtime (hub-gke or a stage it inherits from) must not set ENV KUBECONFIG"},
		{"runtime ENV KUBECONFIG legacy form", mustReplace(t, good, "EXPOSE 8080\n", "EXPOSE 8080\nENV KUBECONFIG /k\n"), "must not set ENV KUBECONFIG"},
		{"runtime ARG KUBECONFIG", mustReplace(t, good, "EXPOSE 8080\n", "EXPOSE 8080\nARG KUBECONFIG=/k\n"), "must not set ARG KUBECONFIG"},
		{"KUBECONFIG after a comment in a continuation", mustReplace(t, good, homeLine, "ENV HOME=/home/scion \\\n# note\n    KUBECONFIG=/k\n"), "must not set ENV KUBECONFIG"},
	}
	for _, m := range dockerMutations {
		expectViolation(t, m.name, checkRootDockerfileContract(t, m.src), m.want)
	}

	// Valid variants the parser must not misreport.
	dockerValid := map[string]string{
		"comment inside a continuation": mustReplace(t, good, homeLine, "ENV HOME=/home/scion \\\n# KUBECONFIG is deliberately not set\n    FOO=1\n"),
		"stage names in other case":     mustReplace(t, mustReplace(t, good, "FROM runtime AS hub-gke", "from RUNTIME as HUB-GKE"), "\nFROM runtime\n", "\nfrom Runtime\n"),
	}
	for name, src := range dockerValid {
		if errs := checkRootDockerfileContract(t, src); len(errs) != 0 {
			t.Errorf("%s: valid Dockerfile reported violations: %q", name, errs)
		}
	}

	cb := readRepoFile(t, "image-build/cloudbuild-hub-gke.yaml")
	const tagLine = "      - '$_REGISTRY/scion-hub-gke:$_SHORT_SHA'\n"
	cbMutations := []struct{ name, src, want string }{
		{"latest tag added", mustReplace(t, cb, tagLine, tagLine+"      - '-t'\n      - '$_REGISTRY/scion-hub-gke:latest'\n"), "must not mention latest"},
		{"second moving tag", mustReplace(t, cb, tagLine, tagLine+"      - '-t'\n      - '$_REGISTRY/scion-hub-gke:main'\n"), "exactly one -t"},
		{"second tag in --tag= form", mustReplace(t, cb, tagLine, tagLine+"      - '--tag=$_REGISTRY/scion-hub-gke:main'\n"), "exactly one -t"},
		{"tag not the short sha", mustReplace(t, cb, tagLine, "      - '$_REGISTRY/scion-hub-gke:dev'\n"), "exactly one -t"},
		{"_TAG substitution", mustReplace(t, cb, "  _SHORT_SHA: ''\n", "  _SHORT_SHA: ''\n  _TAG: 'main'\n"), "_TAG substitution"},
		{"target dropped", mustReplace(t, cb, "'hub-gke'", "'runtime'"), "--target hub-gke"},
		{"multi-arch platform", mustReplace(t, cb, "'linux/amd64'", "'linux/amd64,linux/arm64'"), "--platform linux/amd64"},
		{"--push dropped", mustReplace(t, cb, "      - '--push'\n", ""), "must pass --push"},
		{"-f swapped", mustReplace(t, cb, "      - 'Dockerfile'\n", "      - 'image-build/hub/Dockerfile'\n"), "-f Dockerfile"},
		{"require-short-sha neutered", mustReplace(t, cb, `'test -n "$_SHORT_SHA" || { echo "ERROR: pass --substitutions=_SHORT_SHA=<git short sha>" >&2; exit 1; }'`, "'true'"), "require-short-sha step"},
		{"ignore file reverted to omni", strings.ReplaceAll(cb, "gcloudignore-hub-gke", "gcloudignore-omni"), "--ignore-file=" + hubGKEIgnoreFile},
	}
	for _, m := range cbMutations {
		expectViolation(t, m.name, checkHubGKECloudBuildContract(m.src), m.want)
	}
	if errs := checkHubGKECloudBuildContract("# never latest\n" + cb); len(errs) != 0 {
		t.Errorf("latest in a comment: valid file reported violations: %q", errs)
	}
}
