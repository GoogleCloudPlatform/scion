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

package authzop

import "testing"

// The GitHub installation bind operation admits a user token on the hub
// boundary only: binding also requires hub.github_app.update, which no
// project token can carry, and the operation names users only (no agent
// or broker credential). Revert check: with the project boundary listed
// again, or another principal or credential added, this fails.
func TestCatalog_GitHubInstallationBindHubBoundaryOnly(t *testing.T) {
	var spec *OperationSpec
	for i := range Catalog {
		if Catalog[i].ID == "project.github.installation.bind" {
			spec = &Catalog[i]
			break
		}
	}
	if spec == nil {
		t.Fatal("project.github.installation.bind is not catalogued")
	}

	if spec.Bearer.Kind != BearerAdmit {
		t.Fatalf("bearer kind = %q, want %q", spec.Bearer.Kind, BearerAdmit)
	}
	if len(spec.Bearer.Boundaries) != 1 || spec.Bearer.Boundaries[0] != BearerBoundaryHub {
		t.Errorf("bearer boundaries = %v, want only %q", spec.Bearer.Boundaries, BearerBoundaryHub)
	}
	if len(spec.Principals) != 1 || spec.Principals[0] != PrincipalUser {
		t.Errorf("principals = %v, want only %q", spec.Principals, PrincipalUser)
	}
	for _, c := range spec.Credentials {
		if c != CredentialSessionJWT && c != CredentialScopedUAT {
			t.Errorf("credential %q admitted, want only %q and %q", c, CredentialSessionJWT, CredentialScopedUAT)
		}
	}

	// The PUT entry point belongs to the bind operation alone; the generic
	// GitHub write operation keeps only DELETE.
	for _, s := range Catalog {
		for _, ep := range s.EntryPoints {
			if ep.Kind == EntryPointHTTPRoute && ep.Method == "PUT" &&
				ep.Pattern == "/api/v1/projects/{id}/github-installation" && s.ID != spec.ID {
				t.Errorf("PUT /api/v1/projects/{id}/github-installation is also catalogued under %s", s.ID)
			}
		}
	}
}
