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

package projectcompat

import "strings"

func ProjectIDFromLabels(labels map[string]string) string {
	if labels == nil {
		return ""
	}
	if projectID := labels[LabelProjectID]; projectID != "" {
		return projectID
	}
	return labels[LabelGroveID]
}

func ProjectNameFromLabels(labels map[string]string) string {
	if labels == nil {
		return ""
	}
	if projectName := labels[LabelProject]; projectName != "" {
		return projectName
	}
	return labels[LabelGrove]
}

func ProjectPathFromLabels(labels map[string]string) string {
	if labels == nil {
		return ""
	}
	if projectPath := labels[LabelProjectPath]; projectPath != "" {
		return projectPath
	}
	return labels[LabelGrovePath]
}

func ProjectIDLabels(projectID string, includeLegacy bool) map[string]string {
	labels := map[string]string{
		LabelProjectID: projectID,
	}
	if includeLegacy {
		labels[LabelGroveID] = projectID
	}
	return labels
}

func ProjectNameLabels(projectName string, includeLegacy bool) map[string]string {
	labels := map[string]string{
		LabelProject: projectName,
	}
	if includeLegacy {
		labels[LabelGrove] = projectName
	}
	return labels
}

func ProjectPathLabels(projectPath string, includeLegacy bool) map[string]string {
	labels := map[string]string{
		LabelProjectPath: projectPath,
	}
	if includeLegacy {
		labels[LabelGrovePath] = projectPath
	}
	return labels
}

func CanonicalFieldAliases(key string) (canonical string, legacy bool) {
	switch key {
	case "project", "projects", "projectId", "project_id":
		return key, false
	case "grove":
		return "project", true
	case "groves":
		return "projects", true
	case "groveId":
		return "projectId", true
	case "grove_id":
		return "project_id", true
	case "hub.grove_id":
		return "hub.project_id", true
	case "hub.groveId":
		return "hub.projectId", true
	default:
		return key, false
	}
}

func DeprecatedGroveRoute(path string) bool {
	return path == "/api/v1/groves" || strings.HasPrefix(path, "/api/v1/groves/")
}
