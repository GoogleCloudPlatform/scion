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

// Aliases for symbols moved to
// github.com/GoogleCloudPlatform/scion/pkg/hub/internal/principal
// by hack/pkgmove, so existing references in package hub keep compiling.

import (
	"github.com/GoogleCloudPlatform/scion/pkg/hub/internal/principal"
)

func sameIdentityObject(p0 principal.Identity, p1 principal.Identity) bool {
	return principal.SameIdentityObject(p0, p1)
}
