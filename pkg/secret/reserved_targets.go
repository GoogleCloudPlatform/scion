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

package secret

import "strings"

// ReservedEnvTargetPrefixes lists the environment-variable name prefixes that
// are reserved for scion's own control-plane variables. These names are
// always set by the runtime itself, so an environment-type secret may not
// target one of them. This mirrors the existing SCION_* persistence
// allowlist in the hub dispatcher (shouldPersistResolvedEnvKey), which
// already treats SCION_* as a reserved system namespace.
var ReservedEnvTargetPrefixes = []string{
	"SCION_",
	"GCE_METADATA_",
}

// IsReservedEnvTarget reports whether target falls under a prefix reserved
// for scion's own control-plane environment variables (see
// ReservedEnvTargetPrefixes). It is meaningful only for environment-type
// secret targets; other secret types are not projected into the container
// environment.
func IsReservedEnvTarget(target string) bool {
	for _, prefix := range ReservedEnvTargetPrefixes {
		if strings.HasPrefix(target, prefix) {
			return true
		}
	}
	return false
}
