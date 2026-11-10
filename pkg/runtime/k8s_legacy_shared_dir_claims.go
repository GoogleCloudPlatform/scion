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
	"context"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LegacySharedDirClaimChecker is implemented by runtimes that kept shared
// dirs on per-dir claims before server.project_storage placed them on a
// shared volume. LegacySharedDirClaimWarnings returns one dispatch warning
// per shared dir of config that config.SharedDirStorage now places on the
// shared tree while its old per-dir claim still exists: that claim's
// contents are not on the volume and are not moved automatically
// (ptone/scion#5932). The claims are never changed or deleted here.
type LegacySharedDirClaimChecker interface {
	LegacySharedDirClaimWarnings(ctx context.Context, config RunConfig) ([]string, error)
}

// LegacySharedDirClaimWarnings implements LegacySharedDirClaimChecker.
func (r *KubernetesRuntime) LegacySharedDirClaimWarnings(ctx context.Context, config RunConfig) ([]string, error) {
	if len(config.SharedDirs) == 0 || !config.SharedDirStorage.Shared() {
		return nil, nil
	}
	projectName := projectkeys.ProjectNameFromLabels(config.Labels)
	if projectName == "" {
		return nil, nil
	}
	namespace := r.runNamespace(config.Labels)
	var warnings []string
	for _, sd := range config.SharedDirs {
		name := sharedDirPVCName(projectName, sd.Name)
		_, err := r.Client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			warnings = append(warnings, fmt.Sprintf(
				"Warning: shared dir %q is now on the shared volume, but its previous claim %s/%s still exists; "+
					"its contents are not on the volume and are not moved automatically (the claim is kept)",
				sd.Name, namespace, name))
		case apierrors.IsNotFound(err):
		default:
			return warnings, fmt.Errorf("checking previous shared-dir claim %s/%s: %w", namespace, name, err)
		}
	}
	return warnings, nil
}
