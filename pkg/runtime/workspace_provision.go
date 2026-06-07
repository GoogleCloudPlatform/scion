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
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// Backward-compatible re-exports from pkg/provision.
// The provisioning logic was extracted to the config-free pkg/provision leaf
// package so that lean binaries (e.g. sciontool) can invoke provisioning
// without pulling in pkg/config.

// ProvisionSentinelFile is re-exported from pkg/provision.
const ProvisionSentinelFile = provision.ProvisionSentinelFile

// ProvisionShared delegates to provision.ProvisionShared.
func ProvisionShared(in ProvisionInput) error {
	return provision.ProvisionShared(in)
}
