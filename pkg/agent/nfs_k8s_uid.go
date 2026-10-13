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
	"fmt"
	"log/slog"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// k8sNFSUIDIgnoredWarning returns the warning to log when the Kubernetes
// runtime is used with an nfs.uid other than the uid its pods run as, or
// "" when there is nothing to say: another runtime, no NFS block, or a uid
// that is unset (0, meaning the default 1000) or already 1000. It never
// rejects the config (#2608). See the heads-up comment at
// runtime.K8sContainerUID for why nfs.uid is not applied on Kubernetes.
func k8sNFSUIDIgnoredWarning(runtimeName string, nfsCfg *config.V1NFSConfig) string {
	if !isKubernetesRuntime(runtimeName) || nfsCfg == nil {
		return ""
	}
	if nfsCfg.UID == 0 || int64(nfsCfg.UID) == runtime.K8sContainerUID {
		return ""
	}
	return fmt.Sprintf("%s is %d, but it is not applied on the kubernetes runtime: "+
		"agents on kubernetes run as uid %d and their NFS workspaces are owned by uid %d",
		config.NFSUIDKey, nfsCfg.UID, runtime.K8sContainerUID, runtime.K8sContainerUID)
}

// k8sNFSUIDWarned records the nfs.uid values already warned about, so the
// broker log carries the warning once per process per value instead of on
// every agent start. Reset it with resetK8sNFSUIDWarned in tests.
var k8sNFSUIDWarned sync.Map

// resetK8sNFSUIDWarned forgets which nfs.uid values were warned about.
func resetK8sNFSUIDWarned() {
	k8sNFSUIDWarned.Clear()
}

// warnK8sNFSUIDIgnored logs k8sNFSUIDIgnoredWarning through logf, once per
// process for each distinct nfs.uid value. It reports whether it logged.
func warnK8sNFSUIDIgnored(runtimeName string, nfsCfg *config.V1NFSConfig, logf func(msg string, args ...any)) bool {
	msg := k8sNFSUIDIgnoredWarning(runtimeName, nfsCfg)
	if msg == "" {
		return false
	}
	if _, already := k8sNFSUIDWarned.LoadOrStore(nfsCfg.UID, struct{}{}); already {
		return false
	}
	if logf == nil {
		logf = slog.Warn
	}
	logf(msg)
	return true
}
