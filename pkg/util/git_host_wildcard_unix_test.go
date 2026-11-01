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

//go:build unix

package util

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// NoCredentialHelperEnv also clears credential helpers configured for a
// wildcard host pattern (credential.https://*.example.test.helper), in the
// global and the system config: git then offers no credential for a host the
// pattern matches. Without the reset the same helper answers (control).
func TestNoCredentialHelperEnv_ClearsWildcardScopedHelper(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is required: %v", err)
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "helper-ran")
	cfg := filepath.Join(dir, "gitconfig")
	helper := "!f() { touch '" + marker + "'; echo username=op; echo password=wildcard-secret; }; f"
	body := "[credential \"https://*.example.test\"]\n\thelper = \"" + helper + "\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	const cloneURL = "https://repo.example.test/org/repo.git"
	fill := func(t *testing.T, configScope string, extra []string) (bool, string) {
		t.Helper()
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		env := []string{"HOME=" + dir, "PATH=" + os.Getenv("PATH"), "GIT_TERMINAL_PROMPT=0"}
		if configScope == "global" {
			env = append(env, "GIT_CONFIG_GLOBAL="+cfg, "GIT_CONFIG_NOSYSTEM=1")
		} else {
			env = append(env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+cfg)
		}
		cmd := exec.Command(gitPath, "credential", "fill")
		cmd.Env = append(env, extra...)
		cmd.Stdin = strings.NewReader("protocol=https\nhost=repo.example.test\npath=org/repo.git\n\n")
		out, _ := cmd.CombinedOutput()
		_, statErr := os.Stat(marker)
		return statErr == nil, string(out)
	}
	for _, scope := range []string{"global", "system"} {
		t.Run(scope, func(t *testing.T) {
			ran, out := fill(t, scope, nil)
			if !ran || !strings.Contains(out, "password=wildcard-secret") {
				t.Fatalf("fixture check: the wildcard helper did not answer without the reset: %q", out)
			}
			ran, out = fill(t, scope, NoCredentialHelperEnv(nil, cloneURL))
			if ran {
				t.Error("the wildcard-scoped helper ran despite NoCredentialHelperEnv")
			}
			if strings.Contains(out, "password=wildcard-secret") {
				t.Error("git returned the wildcard helper's credential despite NoCredentialHelperEnv")
			}
		})
	}
}
