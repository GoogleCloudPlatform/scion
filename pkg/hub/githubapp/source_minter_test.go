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

package githubapp

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/source"
)

func TestTokenPermissionsFromScope(t *testing.T) {
	perms, err := TokenPermissionsFromScope(source.CredentialScope{
		Permissions: map[string]string{
			"issues":   "write",
			"metadata": "read",
		},
	})
	if err != nil {
		t.Fatalf("TokenPermissionsFromScope failed: %v", err)
	}
	if perms.Issues != "write" || perms.Metadata != "read" {
		t.Fatalf("unexpected permissions: %+v", perms)
	}

	m := TokenPermissionsToMap(perms)
	if m["issues"] != "write" || m["metadata"] != "read" || len(m) != 2 {
		t.Fatalf("unexpected map: %+v", m)
	}

	if _, err := TokenPermissionsFromScope(source.CredentialScope{
		Permissions: map[string]string{"bogus": "read"},
	}); err == nil {
		t.Error("expected error for unknown permission")
	}

	if _, err := TokenPermissionsFromScope(source.CredentialScope{
		Permissions: map[string]string{"issues": "admin"},
	}); err == nil {
		t.Error("expected error for invalid permission level")
	}

	if _, err := TokenPermissionsFromScope(source.CredentialScope{}); err == nil {
		t.Error("expected error for empty permission scope")
	}
}
