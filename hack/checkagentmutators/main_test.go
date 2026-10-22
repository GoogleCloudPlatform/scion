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
	"io"
	"strings"
	"testing"
)

// TestFindingsReportsAgentMutations is the proof test: the analyzer reports
// every agent mutation in testdata/mutates (client, typed parameter,
// entity Update) and nothing in testdata/clean or for read-only calls.
func TestFindingsReportsAgentMutations(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks testdata packages")
	}
	pkgs, err := load(".", "./testdata/mutates", "./testdata/clean")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(findings(pkgs), "\n")
	for _, want := range []string{"AgentClient.UpdateOneID", "AgentClient.DeleteOneID", "Agent.Update"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	if n := len(findings(pkgs)); n != 3 {
		t.Errorf("got %d findings, want 3:\n%s", n, got)
	}
	if strings.Contains(got, "testdata/clean") {
		t.Errorf("reported the clean package:\n%s", got)
	}
}

func TestAllowed(t *testing.T) {
	for p, want := range map[string]bool{
		modulePath + "/pkg/store":            true,
		modulePath + "/pkg/store/entadapter": true,
		modulePath + "/pkg/ent/agent":        true,
		modulePath + "/pkg/hub":              false,
		modulePath + "/pkg/storex":           false,
		modulePath + "/cmd":                  false,
	} {
		if got := allowed(p); got != want {
			t.Errorf("allowed(%q) = %v, want %v", p, got, want)
		}
	}
}

// TestRunExitCodes is review r2 O4: findings give 1, an empty selection
// (no package importing pkg/ent) gives 2, a clean package gives 0, and the
// self-test passes from the module root.
func TestRunExitCodes(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks testdata packages")
	}
	for _, c := range []struct {
		pattern string
		want    int
	}{
		{"./testdata/mutates", 1},
		{"./testdata/clean", 0},
		{".", 2}, // this package does not import pkg/ent
	} {
		if got := run(".", []string{c.pattern}, io.Discard, io.Discard); got != c.want {
			t.Errorf("run(%s) = %d, want %d", c.pattern, got, c.want)
		}
	}
	if got := selfTest("../..", io.Discard); got != 0 {
		t.Errorf("selfTest = %d, want 0", got)
	}
}
