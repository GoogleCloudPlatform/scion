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

package stagedsecrets

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileSecrets(t *testing.T) {
	homeDir := t.TempDir()
	targetDir := t.TempDir()
	staged := &Staged{
		FileSecrets: []FileSecret{
			{
				Name:   "TLS_CERT",
				Target: filepath.Join(targetDir, "ssl", "cert.pem"),
				Value:  base64.StdEncoding.EncodeToString([]byte("cert-content")),
			},
			{
				Name:   "SSH_KEY",
				Target: filepath.Join(targetDir, "ssh", "id_rsa"),
				Value:  base64.StdEncoding.EncodeToString([]byte("ssh-key")),
			},
		},
	}

	if err := Write(homeDir, staged); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	for path, want := range map[string]string{
		filepath.Join(targetDir, "ssl", "cert.pem"): "cert-content",
		filepath.Join(targetDir, "ssh", "id_rsa"):   "ssh-key",
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(content) != want {
			t.Errorf("%s content = %q, want %q", path, content, want)
		}
	}

	info, err := os.Stat(filepath.Join(targetDir, "ssl", "cert.pem"))
	if err != nil {
		t.Fatalf("stat cert.pem: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestWriteVariableSecrets(t *testing.T) {
	homeDir := t.TempDir()
	staged := &Staged{VariableSecrets: map[string]string{
		"config": `{"a":"b"}`,
		"token":  "abc123",
	}}

	if err := Write(homeDir, staged); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	secretsPath := filepath.Join(homeDir, ".scion", "secrets.json")
	data, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatalf("read secrets.json: %v", err)
	}
	var vars map[string]string
	if err := json.Unmarshal(data, &vars); err != nil {
		t.Fatalf("unmarshal secrets.json: %v", err)
	}
	if len(vars) != 2 || vars["config"] != `{"a":"b"}` || vars["token"] != "abc123" {
		t.Errorf("variable secrets = %#v", vars)
	}

	info, err := os.Stat(secretsPath)
	if err != nil {
		t.Fatalf("stat secrets.json: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestWriteNoVariables(t *testing.T) {
	homeDir := t.TempDir()
	if err := Write(homeDir, &Staged{}); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homeDir, ".scion", "secrets.json")); !os.IsNotExist(err) {
		t.Error("secrets.json was created without variable secrets")
	}
}

// TestWrite_RefusesSymlinkAtVariableSecretsPath proves a symlink planted at
// <homeDir>/.scion/secrets.json (e.g. left over from a previous run on a
// persisted home, or planted ahead of a restart) is refused rather than
// written or chowned through: Write must return an error, and the symlink's
// target file must be untouched.
func TestWrite_RefusesSymlinkAtVariableSecretsPath(t *testing.T) {
	homeDir := t.TempDir()
	scionDir := filepath.Join(homeDir, ".scion")
	if err := os.MkdirAll(scionDir, 0700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(homeDir, "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	secretsPath := filepath.Join(scionDir, "secrets.json")
	if err := os.Symlink(victim, secretsPath); err != nil {
		t.Fatal(err)
	}

	staged := &Staged{VariableSecrets: map[string]string{"token": "abc123"}}
	if err := Write(homeDir, staged); err == nil {
		t.Fatal("Write() = nil error, want a refusal for a symlinked secrets.json")
	}

	link, err := os.Readlink(secretsPath)
	if err != nil {
		t.Fatalf("secrets.json is no longer a symlink after the refused write: %v", err)
	}
	if link != victim {
		t.Errorf("secrets.json symlink target = %q, want %q (unchanged)", link, victim)
	}
	content, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(content) != "untouched" {
		t.Errorf("victim content = %q, want %q (unchanged)", content, "untouched")
	}
}

// TestWrite_RefusesSymlinkAtFileSecretTarget proves the same for a file
// secret's own Target: a planted symlink there is refused rather than
// written or chowned through, and the symlink's target file is untouched.
func TestWrite_RefusesSymlinkAtFileSecretTarget(t *testing.T) {
	homeDir := t.TempDir()
	targetDir := t.TempDir()
	victim := filepath.Join(targetDir, "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetDir, "ssl", "cert.pem")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, target); err != nil {
		t.Fatal(err)
	}

	staged := &Staged{FileSecrets: []FileSecret{
		{Name: "TLS_CERT", Target: target, Value: base64.StdEncoding.EncodeToString([]byte("cert-content"))},
	}}
	if err := Write(homeDir, staged); err == nil {
		t.Fatal("Write() = nil error, want a refusal for a symlinked file-secret target")
	}

	link, err := os.Readlink(target)
	if err != nil {
		t.Fatalf("target is no longer a symlink after the refused write: %v", err)
	}
	if link != victim {
		t.Errorf("target symlink = %q, want %q (unchanged)", link, victim)
	}
	content, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(content) != "untouched" {
		t.Errorf("victim content = %q, want %q (unchanged)", content, "untouched")
	}
}

func TestDecodeErrors(t *testing.T) {
	for name, encoded := range map[string]string{
		"invalid base64": "not-valid-base64!!!",
		"invalid JSON":   base64.StdEncoding.EncodeToString([]byte("not json")),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(encoded); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
