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

// Contract: the hub Dockerfiles do not drift apart again.
//
//   - Dockerfile.hub was a byte-identical copy of the repo-root Dockerfile that
//     went stale once the root file gained stages. It is removed; build with
//     `-f Dockerfile` (the default target is the same image).
//   - scripts/cloudrun/Dockerfile (built by scripts/cloudrun/deploy.sh) uses
//     the same Node major and the same exact Go version as the root Dockerfile,
//     so the scripted Cloud Run path and the root build compile with the same
//     toolchain.
//   - Its final stage is named, so the default build target can be pinned with
//     --target.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cloudRunDockerfile = "scripts/cloudrun/Dockerfile"

// imageNameTag splits a lower-cased FROM base such as "node:22-alpine" into
// its repository name and tag. A digest suffix is dropped.
func imageNameTag(base string) (name, tag string) {
	base, _, _ = strings.Cut(base, "@")
	i := strings.LastIndex(base, ":")
	if i < 0 || strings.Contains(base[i:], "/") {
		return base, ""
	}
	return base[:i], base[i+1:]
}

// stageImageVersion returns the version part of the tag (the tag up to the
// first "-", so "1.26.1-alpine" gives "1.26.1") of the first stage whose base
// image repository is image, or "" if no stage uses it.
func stageImageVersion(stages []dockerfileStage, image string) string {
	for _, st := range stages {
		name, tag := imageNameTag(st.base)
		if name == image || name == "docker.io/library/"+image {
			v, _, _ := strings.Cut(tag, "-")
			return v
		}
	}
	return ""
}

// majorOf returns the leading dotted component of a version ("22.1" -> "22").
func majorOf(v string) string {
	m, _, _ := strings.Cut(v, ".")
	return m
}

// checkCloudRunDockerfileContract returns the contract violations of the
// Cloud Run Dockerfile against the repo-root Dockerfile.
func checkCloudRunDockerfileContract(t *testing.T, root, cloudRun string) []string {
	t.Helper()
	rootStages := parseDockerfileStages(t, root)
	crStages := parseDockerfileStages(t, cloudRun)
	var errs []string

	rootNode, crNode := stageImageVersion(rootStages, "node"), stageImageVersion(crStages, "node")
	switch {
	case rootNode == "" || crNode == "":
		errs = append(errs, "both Dockerfiles must have a node:<version> frontend stage (root "+rootNode+", cloudrun "+crNode+")")
	case majorOf(rootNode) != majorOf(crNode):
		errs = append(errs, cloudRunDockerfile+" uses Node "+majorOf(crNode)+" but the repo-root Dockerfile uses Node "+majorOf(rootNode)+"; keep the majors equal")
	}

	rootGo, crGo := stageImageVersion(rootStages, "golang"), stageImageVersion(crStages, "golang")
	switch {
	case rootGo == "" || crGo == "":
		errs = append(errs, "both Dockerfiles must have a golang:<version> builder stage (root "+rootGo+", cloudrun "+crGo+")")
	case rootGo != crGo:
		errs = append(errs, cloudRunDockerfile+" uses golang:"+crGo+" but the repo-root Dockerfile uses golang:"+rootGo+"; pin the same exact Go version")
	}

	if len(crStages) > 0 {
		last := crStages[len(crStages)-1]
		if last.name == "" && findStage(crStages[:len(crStages)-1], last.base) == nil {
			errs = append(errs, cloudRunDockerfile+": the final stage must be named (FROM <image> AS <name>), or be FROM a named stage, so the default target can be pinned with --target")
		}
	}
	return errs
}

func TestHubDockerfileDuplicateRemoved(t *testing.T) {
	_, err := os.Stat(filepath.Join("..", "Dockerfile.hub"))
	if err == nil {
		t.Error("Dockerfile.hub must not exist: it duplicated the repo-root Dockerfile and went stale; build with -f Dockerfile")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat Dockerfile.hub: %v", err)
	}
}

func TestCloudRunDockerfileTracksRootToolchain(t *testing.T) {
	for _, e := range checkCloudRunDockerfileContract(t, readRepoFile(t, "Dockerfile"), readRepoFile(t, cloudRunDockerfile)) {
		t.Error(e)
	}
}

func TestCloudRunDockerfileContractDetectsViolations(t *testing.T) {
	root := readRepoFile(t, "Dockerfile")
	cr := readRepoFile(t, cloudRunDockerfile)
	mutations := []struct{ name, src, want string }{
		{"older node major", mustReplace(t, cr, "FROM node:22-slim", "FROM node:20-slim"), "uses Node 20"},
		{"floating go minor", mustReplace(t, cr, "FROM golang:1.26.1 ", "FROM golang:1.26 "), "uses golang:1.26 "},
		{"older go", mustReplace(t, cr, "FROM golang:1.26.1 ", "FROM golang:1.25 "), "uses golang:1.25 "},
		{"unnamed final stage", mustReplace(t, cr, "FROM debian:bookworm-slim AS runtime", "FROM debian:bookworm-slim"), "final stage must be named"},
		{"no node stage", mustReplace(t, cr, "FROM node:22-slim", "FROM debian:bookworm-slim"), "node:<version> frontend stage"},
	}
	for _, m := range mutations {
		expectViolation(t, m.name, checkCloudRunDockerfileContract(t, root, m.src), m.want)
	}

	valid := map[string]string{
		"trailing FROM of a named stage": cr + "\nFROM runtime\n",
		"node with a minor version":      mustReplace(t, cr, "FROM node:22-slim", "FROM node:22.11-slim"),
	}
	for name, src := range valid {
		if errs := checkCloudRunDockerfileContract(t, root, src); len(errs) != 0 {
			t.Errorf("%s: valid Dockerfile reported violations: %q", name, errs)
		}
	}
}
