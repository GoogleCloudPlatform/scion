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
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
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

	builder := findStage(stages, "builder")
	if builder == nil {
		errs = append(errs, "no stage named builder")
	} else {
		builderArgs := map[string]bool{}
		foundGoBuild := false
		for _, in := range builder.instrs {
			switch in.cmd {
			case "ARG":
				for _, k := range envOrArgKeys(in.args) {
					builderArgs[k] = true
				}
			case "RUN":
				norm := strings.Join(strings.Fields(in.args), " ")
				if !strings.Contains(norm, "go build") {
					continue
				}
				foundGoBuild = true
				for _, argName := range []string{"GIT_COMMIT", "VERSION", "BUILD_TIME"} {
					if !builderArgs[argName] {
						errs = append(errs, "builder stage must declare ARG "+argName+" before `go build`")
					}
				}
				if !strings.Contains(norm, "-buildvcs=false") {
					errs = append(errs, "builder stage `go build` must pass -buildvcs=false (.git is excluded from build context)")
				}
				for _, sym := range []string{
					"-X github.com/GoogleCloudPlatform/scion/pkg/version.Version=${VERSION}",
					"-X github.com/GoogleCloudPlatform/scion/pkg/version.Commit=${GIT_COMMIT}",
					"-X github.com/GoogleCloudPlatform/scion/pkg/version.BuildTime=${BUILD_TIME}",
				} {
					if !strings.Contains(norm, sym) {
						errs = append(errs, "builder stage `go build` must pass ldflags `"+sym+"`")
					}
				}
			}
		}
		if !foundGoBuild {
			errs = append(errs, "builder stage must run `go build`")
		}
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
	labelArg := map[string]string{
		"org.opencontainers.image.revision": "GIT_COMMIT",
		"org.opencontainers.image.version":  "VERSION",
	}
	var user string
	var runs []string
	gkeArgs := map[string]bool{}
	gkeLabels := map[string]string{}
	gkeLabelArgInScope := map[string]bool{}
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
		case "ARG":
			for _, k := range envOrArgKeys(in.args) {
				gkeArgs[k] = true
			}
		case "LABEL":
			recordLabel := func(k, raw string) {
				gkeLabels[k] = strings.Trim(raw, `"'`)
				if arg, ok := labelArg[k]; ok {
					gkeLabelArgInScope[k] = gkeArgs[arg]
				}
			}
			fields := strings.Fields(in.args)
			if len(fields) >= 2 && !strings.Contains(fields[0], "=") {
				recordLabel(fields[0], strings.Join(fields[1:], " "))
			} else {
				for _, f := range fields {
					if k, v, ok := strings.Cut(f, "="); ok {
						recordLabel(k, v)
					}
				}
			}
		}
	}
	if user != "1000:1000" {
		errs = append(errs, "hub-gke final USER must be 1000:1000; got "+user)
	}
	if home, ok := envValue(gke, "HOME"); !ok || home != "/home/scion" {
		errs = append(errs, "hub-gke must set ENV HOME=/home/scion; got "+home)
	}
	for _, k := range []string{"org.opencontainers.image.revision", "org.opencontainers.image.version"} {
		arg := labelArg[k]
		want := "${" + arg + "}"
		if got := gkeLabels[k]; got != want {
			errs = append(errs, "hub-gke must set LABEL "+k+`="`+want+`"; got `+got)
		} else if !gkeLabelArgInScope[k] {
			errs = append(errs, "hub-gke must declare ARG "+arg+" before LABEL "+k)
		}
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
	hubGKEIgnoreFile = "image-build/gcloudignore-omni"
	hubGKEHelmDoc    = "docs-site/src/content/docs/hosted/ha/helm.md"
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
	for _, sub := range []string{"_VERSION", "_COMMIT_SHA", "_BUILD_TIME"} {
		v, ok := cb.Substitutions[sub]
		if !ok {
			errs = append(errs, "cloudbuild-hub-gke.yaml must define substitution "+sub)
		} else if v != "" {
			errs = append(errs, "cloudbuild-hub-gke.yaml substitution "+sub+" must default to empty string; got "+v)
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
		case "-t", "--tag", "-f", "--file", "--platform", "--target", "--build-arg":
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
	buildArgs := map[string]int{}
	for _, ba := range flags["--build-arg"] {
		buildArgs[ba]++
	}
	for _, want := range []string{"VERSION=$_VERSION", "GIT_COMMIT=$_COMMIT_SHA", "BUILD_TIME=$_BUILD_TIME"} {
		if buildArgs[want] != 1 {
			errs = append(errs, "cloudbuild-hub-gke.yaml build step must pass --build-arg "+want+" once; got ["+strings.Join(flags["--build-arg"], ", ")+"]")
		}
	}

	// The ignore file is only named in the usage comment; check it textually.
	if !strings.Contains(content, "--ignore-file="+hubGKEIgnoreFile) {
		errs = append(errs, "cloudbuild-hub-gke.yaml usage must pass --ignore-file="+hubGKEIgnoreFile)
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

// checkHubGKEHelmDocIgnoreFiles checks that the hosted HA Helm doc names at
// least one --ignore-file= flag and that every --ignore-file= value in the doc
// matches hubGKEIgnoreFile.
func checkHubGKEHelmDocIgnoreFiles(doc string) []string {
	refs := docIgnoreFileRefs(doc)
	if len(refs) == 0 {
		return []string{hubGKEHelmDoc + " must pass --ignore-file=" + hubGKEIgnoreFile}
	}
	var errs []string
	for _, ref := range refs {
		if ref.path == "" || path.Clean(ref.path) != hubGKEIgnoreFile {
			errs = append(errs, fmt.Sprintf("%s:%d: --ignore-file=%s differs from expected %s", hubGKEHelmDoc, ref.line, ref.path, hubGKEIgnoreFile))
		}
	}
	return errs
}

// TestDockerfileContractHubGKEIgnoreFile checks the hub-gke ignore file exists
// and that image-build/README.md and the hosted HA Helm doc point at it. That
// no ignore file drops embedded files is checked by
// TestGcloudIgnoreKeepsEmbeddedFiles (gcloudignore_contract_test.go), and that
// every --ignore-file= path anywhere in docs-site/ exists is checked by
// TestDocsSiteIgnoreFilesExist (gcloudignore_copy_contract_test.go).
func TestDockerfileContractHubGKEIgnoreFile(t *testing.T) {
	_ = readRepoFile(t, hubGKEIgnoreFile)
	readme := readRepoFile(t, "image-build/README.md")
	if !strings.Contains(readme, "--ignore-file="+hubGKEIgnoreFile) {
		t.Error("image-build/README.md must pass --ignore-file=" + hubGKEIgnoreFile + " for the hub-gke build")
	}
	for _, e := range checkHubGKEHelmDocIgnoreFiles(readRepoFile(t, hubGKEHelmDoc)) {
		t.Error(e)
	}
}

// TestDockerfileContractHubGKEDocs checks that the operator-facing build
// instructions in image-build/README.md and docs-site helm.md pass _COMMIT_SHA
// in their gcloud builds submit invocation and document _VERSION for release
// builds, so following the published commands produces a version-stamped image.
func TestDockerfileContractHubGKEDocs(t *testing.T) {
	cases := []struct {
		file    string
		heading string
	}{
		{"image-build/README.md", "## GKE Hub Image (`cloudbuild-hub-gke.yaml`)"},
		{hubGKEHelmDoc, "## 2. Build the hub image"},
	}
	for _, tc := range cases {
		doc := readRepoFile(t, tc.file)
		_, rest, ok := strings.Cut(doc, tc.heading)
		if !ok {
			t.Fatalf("%s: section %q not found", tc.file, tc.heading)
		}
		section, _, _ := strings.Cut(rest, "\n## ")
		for _, want := range []string{
			"--config=image-build/cloudbuild-hub-gke.yaml",
			"--ignore-file=" + hubGKEIgnoreFile,
			"_SHORT_SHA=$(git rev-parse --short HEAD)",
			"_COMMIT_SHA=$(git rev-parse HEAD)",
			",_VERSION=",
		} {
			if !strings.Contains(section, want) {
				t.Errorf("%s (%s): hub-gke build instructions must include %q", tc.file, tc.heading, want)
			}
		}
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
		{"builder missing ARG VERSION", mustReplace(t, good, "ARG VERSION=\"\"\n", ""), "builder stage must declare ARG VERSION before `go build`"},
		{"builder missing ARG GIT_COMMIT", mustReplace(t, good, "ARG GIT_COMMIT=\"\"\n", ""), "builder stage must declare ARG GIT_COMMIT before `go build`"},
		{"builder missing ARG BUILD_TIME", mustReplace(t, good, "ARG BUILD_TIME=\"\"\n", ""), "builder stage must declare ARG BUILD_TIME before `go build`"},
		{"builder missing -buildvcs=false", mustReplace(t, good, "-buildvcs=false ", ""), "must pass -buildvcs=false"},
		{"builder missing Version ldflag", mustReplace(t, good, "pkg/version.Version=${VERSION}", "pkg/version.Ver=${VERSION}"), "pkg/version.Version=${VERSION}"},
		{"builder missing Commit ldflag", mustReplace(t, good, "pkg/version.Commit=${GIT_COMMIT}", "pkg/version.GitCommit=${GIT_COMMIT}"), "pkg/version.Commit=${GIT_COMMIT}"},
		{"builder missing BuildTime ldflag", mustReplace(t, good, "pkg/version.BuildTime=${BUILD_TIME}", "pkg/version.Time=${BUILD_TIME}"), "pkg/version.BuildTime=${BUILD_TIME}"},
		{"hub-gke missing OCI revision label", mustReplace(t, good, "LABEL org.opencontainers.image.revision=\"${GIT_COMMIT}\"\n", ""), "hub-gke must set LABEL org.opencontainers.image.revision"},
		{"hub-gke missing OCI version label", mustReplace(t, good, "LABEL org.opencontainers.image.version=\"${VERSION}\"\n", ""), "hub-gke must set LABEL org.opencontainers.image.version"},
		{"hub-gke missing stage ARG GIT_COMMIT", mustReplace(t, good, "ARG GIT_COMMIT=\"\"\nARG VERSION=\"\"\nLABEL org.opencontainers.image.revision", "ARG VERSION=\"\"\nLABEL org.opencontainers.image.revision"), "hub-gke must declare ARG GIT_COMMIT before LABEL org.opencontainers.image.revision"},
		{"hub-gke missing stage ARG VERSION", mustReplace(t, good, "ARG GIT_COMMIT=\"\"\nARG VERSION=\"\"\nLABEL org.opencontainers.image.revision", "ARG GIT_COMMIT=\"\"\nLABEL org.opencontainers.image.revision"), "hub-gke must declare ARG VERSION before LABEL org.opencontainers.image.version"},
		{"hub-gke OCI label before stage ARG", mustReplace(t, good, "ARG GIT_COMMIT=\"\"\nARG VERSION=\"\"\nLABEL org.opencontainers.image.revision=\"${GIT_COMMIT}\"\nLABEL org.opencontainers.image.version=\"${VERSION}\"\n", "LABEL org.opencontainers.image.revision=\"${GIT_COMMIT}\"\nLABEL org.opencontainers.image.version=\"${VERSION}\"\nARG GIT_COMMIT=\"\"\nARG VERSION=\"\"\n"), "hub-gke must declare ARG GIT_COMMIT before LABEL org.opencontainers.image.revision"},
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
		{"ignore file reverted to default", mustReplace(t, cb, "--ignore-file="+hubGKEIgnoreFile, "--ignore-file=.gcloudignore"), "--ignore-file=" + hubGKEIgnoreFile},
		{"missing _VERSION substitution", mustReplace(t, cb, "  _VERSION: ''\n", ""), "must define substitution _VERSION"},
		{"non-empty _VERSION substitution", mustReplace(t, cb, "  _VERSION: ''\n", "  _VERSION: 'dev'\n"), "substitution _VERSION must default to empty string"},
		{"missing _COMMIT_SHA substitution", mustReplace(t, cb, "  _COMMIT_SHA: ''\n", ""), "must define substitution _COMMIT_SHA"},
		{"missing _BUILD_TIME substitution", mustReplace(t, cb, "  _BUILD_TIME: ''\n", ""), "must define substitution _BUILD_TIME"},
		{"missing VERSION build-arg", mustReplace(t, cb, "      - '--build-arg'\n      - 'VERSION=$_VERSION'\n", ""), "--build-arg VERSION=$_VERSION once"},
		{"missing GIT_COMMIT build-arg", mustReplace(t, cb, "      - '--build-arg'\n      - 'GIT_COMMIT=$_COMMIT_SHA'\n", ""), "--build-arg GIT_COMMIT=$_COMMIT_SHA once"},
		{"missing BUILD_TIME build-arg", mustReplace(t, cb, "      - '--build-arg'\n      - 'BUILD_TIME=$_BUILD_TIME'\n", ""), "--build-arg BUILD_TIME=$_BUILD_TIME once"},
	}
	for _, m := range cbMutations {
		expectViolation(t, m.name, checkHubGKECloudBuildContract(m.src), m.want)
	}
	if errs := checkHubGKECloudBuildContract("# never latest\n" + cb); len(errs) != 0 {
		t.Errorf("latest in a comment: valid file reported violations: %q", errs)
	}

	helmDoc := readRepoFile(t, hubGKEHelmDoc)
	if errs := checkHubGKEHelmDocIgnoreFiles(helmDoc); len(errs) != 0 {
		t.Errorf("valid %s reported violations: %q", hubGKEHelmDoc, errs)
	}
	helmMutations := []struct{ name, src, want string }{
		{"helm doc non-existent ignore file", mustReplace(t, helmDoc, "--ignore-file="+hubGKEIgnoreFile, "--ignore-file=image-build/gcloudignore-hub-gke"), "differs from expected " + hubGKEIgnoreFile},
		{"helm doc extra drifted ignore file", helmDoc + "\n  --ignore-file=.gcloudignore \\\n", "differs from expected " + hubGKEIgnoreFile},
		{"helm doc missing ignore file flag", strings.ReplaceAll(helmDoc, "--ignore-file="+hubGKEIgnoreFile, ""), "must pass --ignore-file=" + hubGKEIgnoreFile},
	}
	for _, m := range helmMutations {
		expectViolation(t, m.name, checkHubGKEHelmDocIgnoreFiles(m.src), m.want)
	}
}

// agentImageDockerfiles returns the tracked Dockerfiles of agent images: the
// image-build/ chain (core-base, thick-prep, scion-base, hub, omni) and the
// harness images under harnesses/.
func agentImageDockerfiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, f := range repoTrackedFiles(t) {
		if !strings.HasPrefix(filepath.Base(f), "Dockerfile") {
			continue
		}
		if strings.HasPrefix(f, "image-build/") || strings.HasPrefix(f, "harnesses/") {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		t.Fatal("no agent image Dockerfiles found under image-build/ or harnesses/")
	}
	return out
}

// checkNoImageTZ returns a violation for each ENV or ARG TZ in a Dockerfile's
// stages. Agent images must not pin a timezone: the agent zone is set per
// agent when the container starts (the hub is the TZ authority for
// hub-dispatched agents), and an image-level TZ, even an empty one baked from
// an unset build arg, sits outside that chain. A non-empty build arg would
// silently pin every container built from the image. verify-base-contract.sh
// asserts the same for the base images at build time; this static check also
// covers the harness images, which do not run that script.
func checkNoImageTZ(t *testing.T, name, content string) []string {
	t.Helper()
	var errs []string
	for _, st := range parseDockerfileStages(t, content) {
		for _, in := range st.instrs {
			if in.cmd != "ENV" && in.cmd != "ARG" {
				continue
			}
			for _, k := range envOrArgKeys(in.args) {
				if k == "TZ" {
					errs = append(errs, name+": "+in.cmd+" "+in.args+": agent images must not set TZ; the agent zone is set per agent at container start")
				}
			}
		}
	}
	return errs
}

func TestAgentImageDockerfilesDoNotSetTZ(t *testing.T) {
	for _, f := range agentImageDockerfiles(t) {
		for _, e := range checkNoImageTZ(t, f, readRepoFile(t, f)) {
			t.Error(e)
		}
	}
}

func TestNoImageTZCheckDetectsViolations(t *testing.T) {
	base := "ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\nRUN true\n"
	for name, src := range map[string]string{
		"ARG TZ":          base + "ARG TZ\n",
		"ENV TZ quoted":   base + "ENV TZ=\"$TZ\"\n",
		"ENV TZ legacy":   base + "ENV TZ UTC\n",
		"ENV TZ in a set": base + "ENV LANG=C.UTF-8 TZ=UTC\n",
		"ARG TZ default":  base + "ARG TZ=Etc/UTC\n",
	} {
		expectViolation(t, name, checkNoImageTZ(t, "x/Dockerfile", src), "must not set TZ")
	}
	if errs := checkNoImageTZ(t, "x/Dockerfile", base+"ENV TZDIR=/usr/share/zoneinfo\nARG TZ_NOTE\n"); len(errs) != 0 {
		t.Errorf("TZ-prefixed names reported as TZ: %q", errs)
	}
}

const antigravityInstallerURL = "https://antigravity.google/cli/install.sh"

func hasShellPipe(s string) bool {
	return strings.Contains(strings.ReplaceAll(s, "||", ""), "|")
}

// checkAntigravityDockerfileContract checks that harnesses/antigravity/Dockerfile
// installs agy via the upstream installer script without a manual AGY_VERSION
// pin, downloads the script to a file instead of piping curl into bash under
// Docker's non-pipefail /bin/sh, installs into /usr/local/bin, and verifies the
// binary with `agy --version`.
func checkAntigravityDockerfileContract(t *testing.T, content string) []string {
	t.Helper()
	stages := parseDockerfileStages(t, content)
	if len(stages) == 0 {
		return []string{"no stages found"}
	}
	var errs []string

	// Check global ARGs before FROM as well as stage-level ARG/ENV instructions.
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		kw := strings.ToUpper(fields[0])
		if kw != "ARG" && kw != "ENV" {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0]))
		for _, k := range envOrArgKeys(rest) {
			if strings.EqualFold(k, "AGY_VERSION") {
				errs = append(errs, "harnesses/antigravity/Dockerfile must not set "+kw+" AGY_VERSION (version comes from upstream installer)")
			}
		}
	}

	var runs []string
	for _, st := range stages {
		for _, in := range st.instrs {
			if in.cmd == "RUN" {
				runs = append(runs, strings.Join(strings.Fields(in.args), " "))
			}
		}
	}
	allRuns := strings.Join(runs, "\n")

	if strings.Contains(allRuns, "github.com/google-antigravity/antigravity-cli/releases") {
		errs = append(errs, "harnesses/antigravity/Dockerfile must not download pinned GitHub release tarballs directly")
	}
	if !strings.Contains(allRuns, antigravityInstallerURL) {
		errs = append(errs, "harnesses/antigravity/Dockerfile must fetch "+antigravityInstallerURL)
	}

	var hasBashDirInstall, hasVersionCheck bool
	for _, run := range runs {
		for _, andClause := range strings.Split(run, "&&") {
			for _, clause := range strings.Split(andClause, ";") {
				c := strings.TrimSpace(clause)
				if (strings.Contains(c, "curl ") || strings.Contains(c, antigravityInstallerURL)) && hasShellPipe(c) {
					errs = append(errs, "harnesses/antigravity/Dockerfile must download install.sh to a file rather than piping curl into bash/sh (Docker /bin/sh lacks pipefail)")
				}
				if !hasShellPipe(c) && strings.Contains(c, "bash ") &&
					(strings.Contains(c, "--dir /usr/local/bin") || strings.Contains(c, "--dir=/usr/local/bin") || strings.Contains(c, "-d /usr/local/bin")) {
					hasBashDirInstall = true
				}
				if c == "agy --version" || c == "/usr/local/bin/agy --version" {
					hasVersionCheck = true
				}
			}
		}
	}
	if !hasBashDirInstall {
		errs = append(errs, "harnesses/antigravity/Dockerfile must invoke install.sh via bash with --dir /usr/local/bin")
	}
	if !hasVersionCheck {
		errs = append(errs, "harnesses/antigravity/Dockerfile must run `agy --version` (without `|| true`) to log the installed version and fail on a broken install")
	}
	return errs
}

func TestAntigravityDockerfileUsesInstaller(t *testing.T) {
	for _, e := range checkAntigravityDockerfileContract(t, readRepoFile(t, "harnesses/antigravity/Dockerfile")) {
		t.Error(e)
	}
}

func TestAntigravityDockerfileContractDetectsViolations(t *testing.T) {
	good := "ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\nUSER root\n" +
		"RUN rm -f /usr/local/bin/agy \\\n" +
		"  && tmp_home=\"$(mktemp -d)\" \\\n" +
		"  && curl -fsSL -o /tmp/agy-install.sh https://antigravity.google/cli/install.sh \\\n" +
		"  && HOME=\"${tmp_home}\" bash /tmp/agy-install.sh --dir /usr/local/bin \\\n" +
		"  && rm -rf /tmp/agy-install.sh \"${tmp_home}\" \\\n" +
		"  && /usr/local/bin/agy --version\n"

	if errs := checkAntigravityDockerfileContract(t, good); len(errs) != 0 {
		t.Fatalf("valid Dockerfile reported violations: %q", errs)
	}

	mutations := []struct{ name, src, want string }{
		{"stage ARG AGY_VERSION", good + "ARG AGY_VERSION=1.5.8\n", "must not set ARG AGY_VERSION"},
		{"global ARG AGY_VERSION", "ARG AGY_VERSION=1.5.8\n" + good, "must not set ARG AGY_VERSION"},
		{"ENV AGY_VERSION", good + "ENV AGY_VERSION=1.5.8\n", "must not set ENV AGY_VERSION"},
		{"curl piped to bash", "ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\nRUN curl -fsSL https://antigravity.google/cli/install.sh | bash -s -- --dir /usr/local/bin && /usr/local/bin/agy --version\n", "rather than piping curl"},
		{"missing --dir /usr/local/bin", mustReplace(t, good, " --dir /usr/local/bin", ""), "with --dir /usr/local/bin"},
		{"missing agy --version", mustReplace(t, good, "  && /usr/local/bin/agy --version\n", "\n"), "must run `agy --version`"},
		{"agy --version masked with || true", mustReplace(t, good, "/usr/local/bin/agy --version", "/usr/local/bin/agy --version || true"), "must run `agy --version`"},
	}
	for _, m := range mutations {
		expectViolation(t, m.name, checkAntigravityDockerfileContract(t, m.src), m.want)
	}
}

// checkNoImagePlaywrightBrowsersPath returns a violation for each ENV or ARG
// PLAYWRIGHT_BROWSERS_PATH in an agent image Dockerfile. Setting an image-wide
// PLAYWRIGHT_BROWSERS_PATH relocates runtime browser downloads (`npx playwright
// install webkit firefox`) out of the persistent agent home (~/.cache/ms-playwright)
// into the ephemeral container layer and orphans existing home caches; system
// ffmpeg is seeded into the persistent home registry by sciontool init instead.
func checkNoImagePlaywrightBrowsersPath(t *testing.T, name, content string) []string {
	t.Helper()
	var errs []string
	for _, st := range parseDockerfileStages(t, content) {
		for _, in := range st.instrs {
			if in.cmd != "ENV" && in.cmd != "ARG" {
				continue
			}
			for _, k := range envOrArgKeys(in.args) {
				if k == "PLAYWRIGHT_BROWSERS_PATH" {
					errs = append(errs, name+": "+in.cmd+" "+in.args+": agent images must not set PLAYWRIGHT_BROWSERS_PATH; keep Playwright's persistent ~/.cache/ms-playwright registry")
				}
			}
		}
	}
	return errs
}

func TestAgentImageDockerfilesDoNotSetPlaywrightBrowsersPath(t *testing.T) {
	for _, f := range agentImageDockerfiles(t) {
		for _, e := range checkNoImagePlaywrightBrowsersPath(t, f, readRepoFile(t, f)) {
			t.Error(e)
		}
	}
}

func TestNoImagePlaywrightBrowsersPathCheckDetectsViolations(t *testing.T) {
	base := "ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\nRUN true\n"
	for name, src := range map[string]string{
		"ARG PLAYWRIGHT_BROWSERS_PATH":         base + "ARG PLAYWRIGHT_BROWSERS_PATH\n",
		"ENV PLAYWRIGHT_BROWSERS_PATH":         base + "ENV PLAYWRIGHT_BROWSERS_PATH=/usr/local/share/ms-playwright\n",
		"ENV PLAYWRIGHT_BROWSERS_PATH legacy":  base + "ENV PLAYWRIGHT_BROWSERS_PATH /usr/local/share/ms-playwright\n",
		"ENV PLAYWRIGHT_BROWSERS_PATH in list": base + "ENV NPM_CONFIG_PREFIX=/usr/local/share/npm-global PLAYWRIGHT_BROWSERS_PATH=/opt/pw\n",
	} {
		expectViolation(t, name, checkNoImagePlaywrightBrowsersPath(t, "x/Dockerfile", src), "must not set PLAYWRIGHT_BROWSERS_PATH")
	}
	if errs := checkNoImagePlaywrightBrowsersPath(t, "x/Dockerfile", base+"ENV PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1\n"); len(errs) != 0 {
		t.Errorf("other PLAYWRIGHT_* env vars reported as PLAYWRIGHT_BROWSERS_PATH: %q", errs)
	}
}

// checkBaseImagePlaywrightFfmpegContract verifies that a foundation Dockerfile
// (core-base or thick-prep) leaves PLAYWRIGHT_BROWSERS_PATH unset and runs
// install-core-toolchain.sh playwright-ffmpeg after npm-tools and before
// verify-base-contract.sh.
func checkBaseImagePlaywrightFfmpegContract(t *testing.T, name, content string) []string {
	t.Helper()
	errs := checkNoImagePlaywrightBrowsersPath(t, name, content)
	stages := parseDockerfileStages(t, content)
	if len(stages) == 0 {
		return append(errs, name+": no stages found")
	}
	final := &stages[len(stages)-1]

	npmToolsStep, ffmpegStep, verifyStep := -1, -1, -1
	seq := 0
	for _, in := range final.instrs {
		if in.cmd != "RUN" {
			continue
		}
		for _, andClause := range strings.Split(in.args, "&&") {
			for _, clause := range strings.Split(andClause, ";") {
				c := strings.Join(strings.Fields(clause), " ")
				switch {
				case strings.Contains(c, "install-core-toolchain.sh npm-tools"):
					seq++
					npmToolsStep = seq
				case strings.Contains(c, "install-core-toolchain.sh playwright-ffmpeg"):
					seq++
					ffmpegStep = seq
				case strings.Contains(c, "verify-base-contract.sh"):
					seq++
					verifyStep = seq
				}
			}
		}
	}

	if ffmpegStep < 0 {
		errs = append(errs, name+": final stage must run `install-core-toolchain.sh playwright-ffmpeg`")
		return errs
	}
	if npmToolsStep >= 0 && ffmpegStep < npmToolsStep {
		errs = append(errs, name+": `install-core-toolchain.sh playwright-ffmpeg` must run after `install-core-toolchain.sh npm-tools`")
	}
	if verifyStep >= 0 && ffmpegStep > verifyStep {
		errs = append(errs, name+": `install-core-toolchain.sh playwright-ffmpeg` must run before `verify-base-contract.sh`")
	}
	return errs
}

func TestBaseImageDockerfilesConfigurePlaywrightFfmpeg(t *testing.T) {
	for _, f := range []string{
		"image-build/core-base/Dockerfile",
		"image-build/thick-prep/Dockerfile",
	} {
		for _, e := range checkBaseImagePlaywrightFfmpegContract(t, f, readRepoFile(t, f)) {
			t.Error(e)
		}
	}
}

func TestBaseImagePlaywrightFfmpegContractDetectsViolations(t *testing.T) {
	good := "FROM node:24-trixie-slim\n" +
		"ENV NPM_CONFIG_PREFIX=/usr/local/share/npm-global\n" +
		"RUN /opt/scion-image-build/lib/install-core-toolchain.sh npm-tools \\\n" +
		"    && /opt/scion-image-build/lib/install-core-toolchain.sh playwright-ffmpeg\n" +
		"RUN /opt/scion-image-build/lib/verify-base-contract.sh\n"

	if errs := checkBaseImagePlaywrightFfmpegContract(t, "test/Dockerfile", good); len(errs) != 0 {
		t.Fatalf("valid Dockerfile reported violations: %q", errs)
	}

	mutations := []struct{ name, src, want string }{
		{
			"sets PLAYWRIGHT_BROWSERS_PATH",
			mustReplace(t, good,
				"ENV NPM_CONFIG_PREFIX=/usr/local/share/npm-global\n",
				"ENV NPM_CONFIG_PREFIX=/usr/local/share/npm-global\nENV PLAYWRIGHT_BROWSERS_PATH=/usr/local/share/ms-playwright\n",
			),
			"must not set PLAYWRIGHT_BROWSERS_PATH",
		},
		{
			"missing playwright-ffmpeg step",
			mustReplace(t, good, " \\\n    && /opt/scion-image-build/lib/install-core-toolchain.sh playwright-ffmpeg", ""),
			"must run `install-core-toolchain.sh playwright-ffmpeg`",
		},
		{
			"playwright-ffmpeg before npm-tools",
			"FROM node:24-trixie-slim\n" +
				"RUN /opt/scion-image-build/lib/install-core-toolchain.sh playwright-ffmpeg\n" +
				"RUN /opt/scion-image-build/lib/install-core-toolchain.sh npm-tools\n" +
				"RUN /opt/scion-image-build/lib/verify-base-contract.sh\n",
			"must run after `install-core-toolchain.sh npm-tools`",
		},
		{
			"playwright-ffmpeg after verify-base-contract.sh",
			mustReplace(
				t,
				mustReplace(t, good, " \\\n    && /opt/scion-image-build/lib/install-core-toolchain.sh playwright-ffmpeg", ""),
				"RUN /opt/scion-image-build/lib/verify-base-contract.sh\n",
				"RUN /opt/scion-image-build/lib/verify-base-contract.sh\nRUN /opt/scion-image-build/lib/install-core-toolchain.sh playwright-ffmpeg\n",
			),
			"must run before `verify-base-contract.sh`",
		},
	}
	for _, m := range mutations {
		expectViolation(t, m.name, checkBaseImagePlaywrightFfmpegContract(t, "test/Dockerfile", m.src), m.want)
	}
}

// pinnedCLIContract describes the build-arg pinning invariants for a harness
// Dockerfile whose usage decoder is verified on specific CLI versions.
type pinnedCLIContract struct {
	dockerfilePath   string
	argName          string
	installerURL     string
	binaryName       string
	verifiedVersions []string
}

// checkPinnedCLIDockerfileContract verifies that a harness Dockerfile:
//   - declares `ARG <argName>=<version>` with a non-empty, non-"latest" default
//   - pins a version listed in verifiedVersions (matching either exact entry or
//     a `<entry>-<suffix>` build tag such as "1.4.25-R9361.1")
//   - references ${<argName>} or $<argName> in a RUN step (so the ARG is wired)
//   - downloads its installer to a file instead of piping curl to bash/sh
//   - runs `<binaryName> --version` (without `|| true`) to log and verify install
func checkPinnedCLIDockerfileContract(t *testing.T, spec pinnedCLIContract, content string) []string {
	t.Helper()
	stages := parseDockerfileStages(t, content)
	if len(stages) == 0 {
		return []string{spec.dockerfilePath + ": no stages found"}
	}

	var errs []string
	var argValue string
	argDeclared := false

	for _, st := range stages {
		for _, in := range st.instrs {
			if in.cmd != "ARG" {
				continue
			}
			for _, tok := range strings.Fields(in.args) {
				k, v, hasEq := strings.Cut(tok, "=")
				if k != spec.argName {
					continue
				}
				argDeclared = true
				if hasEq {
					argValue = strings.Trim(v, `"'`)
				} else {
					argValue = ""
				}
			}
		}
	}

	if !argDeclared {
		errs = append(errs, fmt.Sprintf("%s must declare ARG %s=<verified-version>", spec.dockerfilePath, spec.argName))
	} else if argValue == "" || strings.EqualFold(argValue, "latest") {
		errs = append(errs, fmt.Sprintf("%s must pin ARG %s to a verified version (got %q, must not be empty or latest)", spec.dockerfilePath, spec.argName, argValue))
	} else {
		matched := slices.ContainsFunc(spec.verifiedVersions, func(v string) bool {
			return argValue == v || strings.HasPrefix(argValue, v+"-")
		})
		if !matched {
			errs = append(errs, fmt.Sprintf("%s pins ARG %s=%s, which is not in verified versions %v", spec.dockerfilePath, spec.argName, argValue, spec.verifiedVersions))
		}
	}

	var runs []string
	for _, st := range stages {
		for _, in := range st.instrs {
			if in.cmd == "RUN" {
				runs = append(runs, strings.Join(strings.Fields(in.args), " "))
			}
		}
	}
	allRuns := strings.Join(runs, "\n")

	if !strings.Contains(allRuns, "${"+spec.argName+"}") && !strings.Contains(allRuns, "$"+spec.argName) {
		errs = append(errs, fmt.Sprintf("%s must reference ${%s} in a RUN instruction", spec.dockerfilePath, spec.argName))
	}
	if spec.installerURL != "" && !strings.Contains(allRuns, spec.installerURL) {
		errs = append(errs, fmt.Sprintf("%s must fetch %s", spec.dockerfilePath, spec.installerURL))
	}

	var hasVersionCheck bool
	wantBin := spec.binaryName + " --version"
	wantUsrBin := "/usr/local/bin/" + spec.binaryName + " --version"
	for _, run := range runs {
		for _, andClause := range strings.Split(run, "&&") {
			for _, clause := range strings.Split(andClause, ";") {
				c := strings.TrimSpace(clause)
				if (strings.Contains(c, "curl ") || (spec.installerURL != "" && strings.Contains(c, spec.installerURL))) && hasShellPipe(c) {
					errs = append(errs, fmt.Sprintf("%s must download installer to a file rather than piping curl into bash/sh (Docker /bin/sh lacks pipefail)", spec.dockerfilePath))
				}
				if c == wantBin || c == wantUsrBin {
					hasVersionCheck = true
				}
			}
		}
	}
	if !hasVersionCheck {
		errs = append(errs, fmt.Sprintf("%s must run `%s --version` (without `|| true`) to log the installed version and fail on a broken install", spec.dockerfilePath, spec.binaryName))
	}
	return errs
}

// grokBuildCLIContract enforces the pinned CLI policy for harnesses/grok-build/Dockerfile.
// Antigravity and muse-code intentionally float via their upstream installers
// (see GoogleCloudPlatform/scion#4820 and TestAntigravityDockerfileUsesInstaller)
// and rely on runtime version checks as their safety net, so they are
// deliberately not enrolled in checkPinnedCLIDockerfileContract.
var grokBuildCLIContract = pinnedCLIContract{
	dockerfilePath:   "harnesses/grok-build/Dockerfile",
	argName:          "GROK_VERSION",
	installerURL:     "https://x.ai/cli/install.sh",
	binaryName:       "grok",
	verifiedVersions: telemetry.GrokBuildVerifiedVersions,
}

func TestGrokBuildDockerfilePinsVerifiedVersion(t *testing.T) {
	for _, e := range checkPinnedCLIDockerfileContract(t, grokBuildCLIContract, readRepoFile(t, grokBuildCLIContract.dockerfilePath)) {
		t.Error(e)
	}
}

func TestGrokBuildDockerfileContractDetectsViolations(t *testing.T) {
	good := "ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\n" +
		"ARG GROK_VERSION=1.0.92\n" +
		"USER root\n" +
		"RUN curl -fsSL -o /tmp/grok-install.sh https://x.ai/cli/install.sh \\\n" +
		"    && bash /tmp/grok-install.sh \"${GROK_VERSION}\" \\\n" +
		"    && rm -f /tmp/grok-install.sh \\\n" +
		"    && /usr/local/bin/grok --version\n"

	if errs := checkPinnedCLIDockerfileContract(t, grokBuildCLIContract, good); len(errs) != 0 {
		t.Fatalf("valid Dockerfile reported violations: %q", errs)
	}

	mutations := []struct{ name, src, want string }{
		{"latest pin", mustReplace(t, good, "ARG GROK_VERSION=1.0.92", "ARG GROK_VERSION=latest"), "must not be empty or latest"},
		{"empty pin", mustReplace(t, good, "ARG GROK_VERSION=1.0.92", "ARG GROK_VERSION="), "must not be empty or latest"},
		{"unverified version", mustReplace(t, good, "ARG GROK_VERSION=1.0.92", "ARG GROK_VERSION=9.9.9"), "is not in verified versions"},
		{"missing ARG", mustReplace(t, good, "ARG GROK_VERSION=1.0.92\n", ""), "must declare ARG GROK_VERSION"},
		{"unused ARG", mustReplace(t, good, `bash /tmp/grok-install.sh "${GROK_VERSION}"`, "bash /tmp/grok-install.sh"), "must reference ${GROK_VERSION}"},
		{"curl piped to bash", mustReplace(t, good, "curl -fsSL -o /tmp/grok-install.sh https://x.ai/cli/install.sh \\\n    && bash /tmp/grok-install.sh \"${GROK_VERSION}\"", "curl -fsSL https://x.ai/cli/install.sh | bash -s \"${GROK_VERSION}\""), "rather than piping curl"},
		{"missing grok --version", mustReplace(t, good, " \\\n    && /usr/local/bin/grok --version\n", "\n"), "must run `grok --version`"},
		{"grok --version masked with || true", mustReplace(t, good, "/usr/local/bin/grok --version", "/usr/local/bin/grok --version || true"), "must run `grok --version`"},
	}
	for _, m := range mutations {
		expectViolation(t, m.name, checkPinnedCLIDockerfileContract(t, grokBuildCLIContract, m.src), m.want)
	}
}
