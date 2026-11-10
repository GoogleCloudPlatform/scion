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

import corev1 "k8s.io/api/core/v1"

// podPlacementLogAttrs returns log attributes naming a pod's effective
// node selector and the keys of its tolerations, for the unschedulable
// log lines (placement defaults, ptone/scion#5085). Toleration values are
// left out.
func podPlacementLogAttrs(pod *corev1.Pod) []any {
	if pod == nil {
		return nil
	}
	keys := make([]string, 0, len(pod.Spec.Tolerations))
	for _, t := range pod.Spec.Tolerations {
		keys = append(keys, t.Key)
	}
	return []any{"nodeSelector", pod.Spec.NodeSelector, "tolerationKeys", keys}
}
