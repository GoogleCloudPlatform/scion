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

package runtime

import (
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// AgentOperationID returns the identifier to pass to Stop, Delete, GetLogs or
// Exec for an agent entry returned by a runtime's List.
//
// For a Kubernetes entry (one carrying Kubernetes metadata with a namespace)
// it is "<namespace>/<pod>", which KubernetesRuntime resolves without looking
// the pod up in its default namespace. List itself keeps reporting the bare
// pod name as ContainerID. For every other runtime it is ContainerID,
// unchanged.
func AgentOperationID(a api.AgentInfo) string {
	if a.ContainerID == "" || a.Kubernetes == nil {
		return a.ContainerID
	}
	ns := strings.TrimSpace(a.Kubernetes.Namespace)
	if ns == "" || strings.Contains(a.ContainerID, "/") {
		return a.ContainerID
	}
	return ns + "/" + a.ContainerID
}
