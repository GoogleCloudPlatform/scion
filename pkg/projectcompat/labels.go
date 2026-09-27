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

func ProjectIDFromLabels(labels map[string]string) string {
	if labels == nil {
		return ""
	}
	return labels[LabelProjectID]
}

func ProjectNameFromLabels(labels map[string]string) string {
	if labels == nil {
		return ""
	}
	return labels[LabelProject]
}

func ProjectPathFromLabels(labels map[string]string) string {
	if labels == nil {
		return ""
	}
	return labels[LabelProjectPath]
}

func ProjectIDLabels(projectID string) map[string]string {
	return map[string]string{
		LabelProjectID: projectID,
	}
}

func ProjectNameLabels(projectName string) map[string]string {
	return map[string]string{
		LabelProject: projectName,
	}
}

func ProjectPathLabels(projectPath string) map[string]string {
	return map[string]string{
		LabelProjectPath: projectPath,
	}
}
