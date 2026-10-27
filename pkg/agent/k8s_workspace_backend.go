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

package agent

import (
	"log/slog"
	"sync/atomic"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// k8sGKESharedVolumeBackend is the workspace backend (server.project_storage,
// or the deprecated server.workspace_storage) that the Kubernetes runtime
// does not mount into agent pods yet (ptone/scion#1075).
const k8sGKESharedVolumeBackend = "gke-shared-volume"

// k8sWorkspaceBackendDroppedMsg is the warning logged when a Kubernetes
// agent starts with a workspace backend the runtime does not mount. It names
// both keys: the backend comes from the effective workspace storage, which
// server.project_storage sets, or the deprecated server.workspace_storage.
const k8sWorkspaceBackendDroppedMsg = "the workspace storage backend (server.project_storage.backend, " +
	"or the deprecated server.workspace_storage.backend) is gke-shared-volume, " +
	"but the kubernetes runtime does not mount it for agents: agent workspaces use an " +
	"emptyDir volume and are lost when the pod is deleted; use backend nfs for persistent " +
	"agent workspaces"

// k8sWorkspaceBackendDropped reports whether an agent started on runtimeName
// with workspace backend gets an emptyDir workspace instead: true only
// for the Kubernetes runtime with backend gke-shared-volume. Agent start only
// applies the nfs backend there, in every workspace mode.
func k8sWorkspaceBackendDropped(runtimeName, backend string) bool {
	return isKubernetesRuntime(runtimeName) && backend == k8sGKESharedVolumeBackend
}

// k8sWorkspaceBackendWarned records that the warning was logged, so the
// agent runtime log carries it once per process instead of on every agent
// start. Agent starts can run concurrently, hence the atomic flag.
var k8sWorkspaceBackendWarned atomic.Bool

// warnK8sWorkspaceBackendDropped logs k8sWorkspaceBackendDroppedMsg through
// logf (slog.Warn when nil), once per process, when k8sWorkspaceBackendDropped
// holds for cfg. It reports whether it logged. It only logs: workspace
// selection is unchanged.
func warnK8sWorkspaceBackendDropped(runtimeName string, cfg *config.V1WorkspaceStorageConfig, logf func(msg string, args ...any)) bool {
	if cfg == nil || !k8sWorkspaceBackendDropped(runtimeName, cfg.Backend) {
		return false
	}
	if !k8sWorkspaceBackendWarned.CompareAndSwap(false, true) {
		return false
	}
	if logf == nil {
		logf = slog.Warn
	}
	logf(k8sWorkspaceBackendDroppedMsg, "runtime", runtimeName, "backend", cfg.Backend)
	return true
}
