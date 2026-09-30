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

package hub

import (
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
)

// offloadPolicy returns the current messaging.OffloadPolicy derived from
// operational settings (ptone/scion#2257). When OperationalSettings is
// unavailable (e.g. file/SQLite mode init failure), the threshold is 0
// (disabled) — the same fail-safe default as every other opsettings-backed
// feature. PreviewBudgetBytes is left at its zero value, so
// OffloadForDelivery falls back to messaging.DefaultPreviewBudgetBytes; the
// preview budget is not an operator-configurable setting.
func (s *Server) offloadPolicy() messaging.OffloadPolicy {
	threshold := 0
	if ops := s.GetOperationalSettings(); ops != nil {
		threshold = ops.OffloadThresholdRunes()
	}
	return messaging.OffloadPolicy{ThresholdRunes: threshold}
}
