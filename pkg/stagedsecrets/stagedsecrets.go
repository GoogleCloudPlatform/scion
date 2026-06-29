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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// EnvVar is the environment variable used to pass serialized
// file and variable secrets from the broker to the container. The value is
// a base64-encoded JSON blob decoded by sciontool init.
const EnvVar = "SCION_STAGED_SECRETS"

// FileSecret describes a single file-type secret in the staged blob.
type FileSecret struct {
	Name   string `json:"name"`
	Target string `json:"target"` // container-side path (tilde already expanded)
	Value  string `json:"value"`  // base64-encoded file content
}

// Staged is the top-level structure serialized into SCION_STAGED_SECRETS.
type Staged struct {
	FileSecrets     []FileSecret      `json:"file_secrets,omitempty"`
	VariableSecrets map[string]string `json:"variable_secrets,omitempty"`
}

// Decode decodes the SCION_STAGED_SECRETS env var value
// (base64 → JSON) and returns the structured secrets. This is called
// inside the container by sciontool init.
func Decode(encoded string) (*Staged, error) {
	jsonData, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("failed to base64-decode staged secrets: %w", err)
	}
	var staged Staged
	if err := json.Unmarshal(jsonData, &staged); err != nil {
		return nil, fmt.Errorf("failed to unmarshal staged secrets JSON: %w", err)
	}
	return &staged, nil
}

// Write writes decoded staged secrets to the filesystem inside
// the container. File secrets are written to their target paths with 0600
// permissions. Variable secrets are written to <homeDir>/.scion/secrets.json.
func Write(homeDir string, staged *Staged) error {
	var uid, gid int
	if os.Getuid() == 0 {
		if uidStr := os.Getenv("SCION_HOST_UID"); uidStr != "" {
			if id, err := strconv.Atoi(uidStr); err == nil {
				uid = id
			}
		}
		if gidStr := os.Getenv("SCION_HOST_GID"); gidStr != "" {
			if id, err := strconv.Atoi(gidStr); err == nil {
				gid = id
			}
		}
	}

	for _, fs := range staged.FileSecrets {
		data, err := base64.StdEncoding.DecodeString(fs.Value)
		if err != nil {
			return fmt.Errorf("failed to base64-decode secret %s: %w", fs.Name, err)
		}

		dir := filepath.Dir(fs.Target)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory for secret %s: %w", fs.Name, err)
		}
		if (uid > 0 || gid > 0) && strings.HasPrefix(dir, homeDir) {
			_ = os.Chown(dir, uid, gid)
		}
		if err := os.WriteFile(fs.Target, data, 0600); err != nil {
			return fmt.Errorf("failed to write secret file %s: %w", fs.Name, err)
		}
		if (uid > 0 || gid > 0) && strings.HasPrefix(fs.Target, homeDir) {
			_ = os.Chown(fs.Target, uid, gid)
		}
	}

	if len(staged.VariableSecrets) > 0 {
		scionDir := filepath.Join(homeDir, ".scion")
		if err := os.MkdirAll(scionDir, 0700); err != nil {
			return fmt.Errorf("failed to create .scion directory: %w", err)
		}
		if uid > 0 || gid > 0 {
			_ = os.Chown(scionDir, uid, gid)
		}
		data, err := json.Marshal(staged.VariableSecrets)
		if err != nil {
			return fmt.Errorf("failed to marshal secrets.json: %w", err)
		}
		secretsPath := filepath.Join(scionDir, "secrets.json")
		if err := os.WriteFile(secretsPath, data, 0600); err != nil {
			return fmt.Errorf("failed to write secrets.json: %w", err)
		}
		if uid > 0 || gid > 0 {
			_ = os.Chown(secretsPath, uid, gid)
		}
	}

	return nil
}
