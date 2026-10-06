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

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Workspace records name which side populated a hub-managed project
// workspace (~/.scion/projects/<slug>). Each record is a file named after the
// project slug, holding exactly the hub project ID. The record directories
// sit directly in the global directory, outside every workspace tree.
const (
	// HubWorkspacesDir holds records written by a hub for the workspaces
	// it keeps as its own.
	HubWorkspacesDir = "hub-workspaces"
	// BrokerWorkspacesDir holds records written by a runtime broker for
	// the workspaces it populated from a hub workspace upload.
	BrokerWorkspacesDir = "broker-workspaces"
)

// HubWorkspaceRecordPath returns <globalDir>/hub-workspaces/<slug>.
func HubWorkspaceRecordPath(slug string) (string, error) {
	return workspaceRecordPath(HubWorkspacesDir, slug)
}

// BrokerWorkspaceRecordPath returns <globalDir>/broker-workspaces/<slug>.
func BrokerWorkspaceRecordPath(slug string) (string, error) {
	return workspaceRecordPath(BrokerWorkspacesDir, slug)
}

func workspaceRecordPath(dir, slug string) (string, error) {
	if !isSinglePathElement(slug) {
		return "", fmt.Errorf("invalid project slug for workspace record")
	}
	globalDir, err := GetGlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(globalDir, dir, slug), nil
}

// WriteWorkspaceRecord writes projectID, exactly, as the record at path.
func WriteWorkspaceRecord(path, projectID string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(projectID), 0644)
}

// ReadWorkspaceRecord returns the exact content of the record at path.
func ReadWorkspaceRecord(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ConfinedProjectConfigRoot returns the project config directory
// ~/.scion/project-configs/<slug>__<id8> for slug and projectID. ok is false
// unless slug and projectID are single path elements and the result is a
// direct child of the project-configs directory named exactly <slug>__<id8>.
// Callers apply the project ID grammar themselves.
func ConfinedProjectConfigRoot(slug, projectID string) (root string, ok bool) {
	if !isSinglePathElement(slug) || !isSinglePathElement(projectID) {
		return "", false
	}
	globalDir, err := GetGlobalDir()
	if err != nil {
		return "", false
	}
	marker := ProjectMarker{ProjectID: projectID, ProjectSlug: slug}
	configsDir := filepath.Join(globalDir, ProjectConfigsDir)
	root = filepath.Join(configsDir, marker.DirName())
	if filepath.Dir(root) != configsDir || filepath.Base(root) != marker.DirName() {
		return "", false
	}
	return root, true
}

// isSinglePathElement reports whether s is usable as one path element.
func isSinglePathElement(s string) bool {
	return s != "" && s != "." && !strings.Contains(s, "..") && !strings.ContainsAny(s, `/\`)
}
