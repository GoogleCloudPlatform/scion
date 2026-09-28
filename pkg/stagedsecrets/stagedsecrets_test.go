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
