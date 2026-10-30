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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
)

// KubeConnectionOf converts a Kubernetes client's connection mode to the
// config package's KubeConnection, which the placement matcher takes
// (pkg/k8s imports no other scion package, so it cannot use that type).
func KubeConnectionOf(m k8s.ConnectionMode) config.KubeConnection {
	switch m {
	case k8s.ConnKubeconfig:
		return config.KubeConnKubeconfig
	case k8s.ConnInCluster:
		return config.KubeConnInCluster
	default:
		return config.KubeConnUnknown
	}
}
