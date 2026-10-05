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
)

// dockerfileInstr is one logical Dockerfile instruction (continuations joined).
type dockerfileInstr struct {
	cmd  string // upper-cased instruction keyword, e.g. "FROM"
	args string // remainder of the instruction, whitespace-trimmed
}

// dockerfileStage is a build stage: its FROM base, optional name, and body.
type dockerfileStage struct {
	base   string
	name   string
	instrs []dockerfileInstr
}

// parseDockerfileStages is a minimal Dockerfile parser: it drops comments and
// blank lines, joins backslash continuations, and splits on FROM. It is enough
// to check stage structure; it is not a general Dockerfile parser.
func parseDockerfileStages(t *testing.T, content string) []dockerfileStage {
	t.Helper()
	var logical []string
	var cur strings.Builder
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if cur.Len() == 0 && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
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
			st.base = parts[0]
			if len(parts) == 3 && strings.EqualFold(parts[1], "AS") {
				st.name = parts[2]
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
		if stages[i].name == name {
			return &stages[i]
		}
	}
	return nil
}

// checkRootDockerfileContract returns the list of contract violations for the
// repo-root Dockerfile. See the stage comments in that file for the reasons.
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
	} else {
		if gke.base != "runtime" {
			errs = append(errs, "hub-gke must be FROM runtime; got FROM "+gke.base)
		}
		var user string
		for _, in := range gke.instrs {
			switch in.cmd {
			case "USER":
				user = in.args
			case "CMD":
				errs = append(errs, "hub-gke must not set CMD: CMD "+in.args)
			case "ENTRYPOINT":
				errs = append(errs, "hub-gke must inherit ENTRYPOINT from runtime: ENTRYPOINT "+in.args)
			case "ENV":
				if strings.Contains(strings.ToUpper(in.args), "KUBECONFIG") {
					errs = append(errs, "hub-gke must not set ENV KUBECONFIG: ENV "+in.args)
				}
			}
		}
		if user != "1000:1000" {
			errs = append(errs, "hub-gke final USER must be 1000:1000; got "+user)
		}
	}
	return errs
}

// checkHubGKECloudBuildContract returns violations for the hub-gke Cloud Build
// file: it must build --target hub-gke and must not push a moving tag.
func checkHubGKECloudBuildContract(content string) []string {
	var errs []string
	if strings.Contains(strings.ToLower(content), "latest") {
		errs = append(errs, "cloudbuild-hub-gke.yaml must not mention `latest` (only the per-commit $_SHORT_SHA tag is pushed)")
	}
	if strings.Contains(content, "_TAG") {
		errs = append(errs, "cloudbuild-hub-gke.yaml must not use a _TAG substitution")
	}
	if !strings.Contains(content, "'hub-gke'") || !strings.Contains(content, "'--target'") {
		errs = append(errs, "cloudbuild-hub-gke.yaml must build with --target hub-gke")
	}
	if !strings.Contains(content, "'$_REGISTRY/scion-hub-gke:$_SHORT_SHA'") {
		errs = append(errs, "cloudbuild-hub-gke.yaml must tag $_REGISTRY/scion-hub-gke:$_SHORT_SHA")
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

func TestHubGKECloudBuildContract(t *testing.T) {
	for _, e := range checkHubGKECloudBuildContract(readRepoFile(t, "image-build/cloudbuild-hub-gke.yaml")) {
		t.Error(e)
	}
}

// TestDockerfileContractCheckerDetectsViolations proves the checkers fail on
// the mutations they exist to catch, so a passing contract test means something.
func TestDockerfileContractCheckerDetectsViolations(t *testing.T) {
	good := readRepoFile(t, "Dockerfile")
	idx := strings.LastIndex(good, "\nFROM runtime\n")
	if idx < 0 {
		t.Fatal("trailing FROM runtime not found")
	}
	dockerMutations := map[string]string{
		"trailing stage removed":   good[:idx+1],
		"stage appended after it":  good + "\nFROM runtime AS extra\nRUN true\n",
		"hub-gke CMD added":        strings.Replace(good, "USER 1000:1000\n", "USER 1000:1000\nCMD [\"server\"]\n", 1),
		"hub-gke KUBECONFIG added": strings.Replace(good, "ENV HOME=/home/scion\n", "ENV HOME=/home/scion KUBECONFIG=/k\n", 1),
		"hub-gke USER root":        strings.Replace(good, "USER 1000:1000\n", "USER 0:0\n", 1),
		"runtime ENTRYPOINT moved": strings.Replace(good, `ENTRYPOINT ["/usr/local/bin/scion"]`, `ENTRYPOINT ["/bin/sh"]`, 1),
		"hub-gke from debian":      strings.Replace(good, "FROM runtime AS hub-gke", "FROM debian:bookworm-slim AS hub-gke", 1),
	}
	for name, m := range dockerMutations {
		if m == good {
			t.Errorf("%s: mutation did not apply", name)
			continue
		}
		if errs := checkRootDockerfileContract(t, m); len(errs) == 0 {
			t.Errorf("%s: checker did not report a violation", name)
		}
	}

	cb := readRepoFile(t, "image-build/cloudbuild-hub-gke.yaml")
	cbMutations := map[string]string{
		"latest tag added": strings.Replace(cb, "      - '$_REGISTRY/scion-hub-gke:$_SHORT_SHA'\n",
			"      - '$_REGISTRY/scion-hub-gke:$_SHORT_SHA'\n      - '-t'\n      - '$_REGISTRY/scion-hub-gke:latest'\n", 1),
		"target dropped": strings.Replace(cb, "'hub-gke'", "'runtime'", 1),
	}
	for name, m := range cbMutations {
		if m == cb {
			t.Errorf("%s: mutation did not apply", name)
			continue
		}
		if errs := checkHubGKECloudBuildContract(m); len(errs) == 0 {
			t.Errorf("%s: checker did not report a violation", name)
		}
	}
}
