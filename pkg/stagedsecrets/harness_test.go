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

package stagedsecrets

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const harnessCanary = "fake-harness-secret-canary-7242-not-a-credential"

func harnessSecret(name, value string) HarnessSecret {
	return HarnessSecret{Name: name, Value: base64.StdEncoding.EncodeToString([]byte(value))}
}

// The harness secrets are written 0600 into a 0700 directory below the
// in-memory root.
func TestWriteHarnessSecrets_WritesPrivateFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "harness-secrets")
	err := writeHarnessSecretsAs(root, dir, []HarnessSecret{harnessSecret("API_KEY", harnessCanary), harnessSecret("OTHER", "x")}, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatalf("writeHarnessSecretsAs: %v", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("harness secrets dir: %v, err %v; want a 0700 directory", fi, err)
	}
	for name, want := range map[string]string{"API_KEY": harnessCanary, "OTHER": "x"} {
		p := filepath.Join(dir, name)
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode() != 0o600 {
			t.Errorf("%s: %v, err %v; want a 0600 file", name, fi, err)
		}
		if data, _ := os.ReadFile(p); string(data) != want {
			t.Errorf("%s content differs", name)
		}
	}
}

// Nothing to write writes nothing, even with no directory configured.
func TestWriteHarnessSecrets_NoneIsNoop(t *testing.T) {
	if err := writeHarnessSecretsAs("/run/scion/mem", "", nil, 0, 0); err != nil {
		t.Errorf("no secrets: %v", err)
	}
}

// Harness secrets with no in-memory directory are refused, never written
// anywhere else; a directory outside the root, a bad name, or a link at a
// file name or the directory is refused too. Errors carry no value.
func TestWriteHarnessSecrets_Refusals(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := []HarnessSecret{harnessSecret("API_KEY", harnessCanary)}
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T) string // returns dir
		secrets []HarnessSecret
		is      error
	}{
		{"no directory", func(t *testing.T) string { return "" }, good, ErrNoHarnessSecretsDir},
		{"outside the root", func(t *testing.T) string { return filepath.Join(outside, "hs") }, good, nil},
		{"the root itself", func(t *testing.T) string { return root }, good, nil},
		{"relative", func(t *testing.T) string { return "harness-secrets" }, good, nil},
		{"unclean", func(t *testing.T) string { return root + "/x/../hs" }, good, nil},
		{"name with a slash", func(t *testing.T) string { return filepath.Join(root, "a") }, []HarnessSecret{harnessSecret("../x", harnessCanary)}, nil},
		{"dot name", func(t *testing.T) string { return filepath.Join(root, "b") }, []HarnessSecret{harnessSecret("..", harnessCanary)}, nil},
		{"link at the file name", func(t *testing.T) string {
			dir := filepath.Join(root, "c")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(victim, filepath.Join(dir, "API_KEY")); err != nil {
				t.Fatal(err)
			}
			return dir
		}, good, nil},
		{"link at the directory", func(t *testing.T) string {
			dir := filepath.Join(root, "d")
			if err := os.Symlink(outside, dir); err != nil {
				t.Fatal(err)
			}
			return dir
		}, good, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := writeHarnessSecretsAs(root, tc.setup(t), tc.secrets, os.Getuid(), os.Getgid())
			if err == nil {
				t.Fatal("no error")
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("err = %v, want %v", err, tc.is)
			}
			if strings.Contains(err.Error(), harnessCanary) {
				t.Error("the error carries the secret value")
			}
			if data, _ := os.ReadFile(victim); string(data) != "original" {
				t.Error("a write went through a link")
			}
			if _, err := os.Lstat(filepath.Join(outside, "API_KEY")); !errors.Is(err, os.ErrNotExist) {
				t.Error("a secret was written outside the root")
			}
		})
	}
}

// Without a workload uid (an unprivileged init, or SCION_HOST_UID unset)
// nothing is created.
func TestPrepareHarnessDir_NoWorkloadUIDIsNoop(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	t.Setenv("SCION_HOST_GID", "")
	root := t.TempDir()
	dir := filepath.Join(root, "outputs")
	if err := PrepareHarnessDir(root, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("an unprivileged init created the directory")
	}
}

// Formatting a Staged never prints a harness secret value.
func TestStagedStringWithholdsHarnessSecrets(t *testing.T) {
	s := Staged{HarnessSecrets: []HarnessSecret{harnessSecret("API_KEY", harnessCanary)}}
	for _, out := range []string{s.String(), fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s)} {
		if strings.Contains(out, harnessCanary) || strings.Contains(out, s.HarnessSecrets[0].Value) {
			t.Errorf("formatted Staged carries the value: %q", out)
		}
	}
}

// Write leaves harness secrets alone: they never reach the home.
func TestWriteIgnoresHarnessSecrets(t *testing.T) {
	home := t.TempDir()
	if err := writeAs(home, &Staged{HarnessSecrets: []HarnessSecret{harnessSecret("API_KEY", harnessCanary)}}, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Errorf("Write put %d entries in the home", len(entries))
	}
}

func TestValidHarnessSecretName(t *testing.T) {
	for name, want := range map[string]bool{
		"API_KEY": true, ".hidden": true, "": false, ".": false, "..": false,
		"a/b": false, `a\b`: false, "a\x00b": false,
	} {
		if got := ValidHarnessSecretName(name); got != want {
			t.Errorf("ValidHarnessSecretName(%q) = %v, want %v", name, got, want)
		}
	}
}
