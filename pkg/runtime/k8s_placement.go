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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// validateKubernetesPlacement checks the merged node selector, tolerations
// and runtime class that buildPod puts on the pod spec, with the same rules
// the API server applies. It accepts every value the API server accepts.
func validateKubernetesPlacement(k *api.KubernetesConfig) error {
	if k == nil {
		return nil
	}
	if err := config.ValidateNodeSelector(k.NodeSelector); err != nil {
		return fmt.Errorf("invalid kubernetes.nodeSelector: %w", err)
	}
	if err := config.ValidateTolerations(k.Tolerations); err != nil {
		return fmt.Errorf("invalid kubernetes.tolerations: %w", err)
	}
	if err := config.ValidateRuntimeClassName(k.RuntimeClassName); err != nil {
		return fmt.Errorf("invalid kubernetes.runtimeClassName: %w", err)
	}
	return nil
}
