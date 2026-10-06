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

package hubclient

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The per-profile default map must round-trip the PUT semantics: an empty
// map clears ({}), a nil map keeps (null).
func TestProjectSettings_ProfileDefaultSAMapMarshalling(t *testing.T) {
	b, err := json.Marshal(ProjectSettings{DefaultGCPIdentityServiceAccountIDByProfile: map[string]string{}})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"defaultGCPIdentityServiceAccountIDByProfile":{}`, "an empty map must reach the hub to clear the setting")

	b, err = json.Marshal(ProjectSettings{})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"defaultGCPIdentityServiceAccountIDByProfile":null`, "a nil map keeps the stored setting")

	b, err = json.Marshal(ProjectSettings{DefaultGCPIdentityServiceAccountIDByProfile: map[string]string{"k8s": "sa"}})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"defaultGCPIdentityServiceAccountIDByProfile":{"k8s":"sa"}`)
}
