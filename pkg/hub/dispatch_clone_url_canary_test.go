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
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// checkDispatchCloneURLs reads a project's clone URLs only to refuse them:
// the error it returns for a refused URL, on either leg, carries no part of
// the URL, so a caller that logs, wraps or returns it cannot leak the URL.
// Each refused form embeds a canary that must not appear in the error text.
func TestCheckDispatchCloneURLs_ErrorCarriesNoURL(t *testing.T) {
	const canary = "c4n4ry-clone-url"
	refused := []string{
		"file:///srv/" + canary + "/repo.git",
		"/srv/" + canary + "/repo",
		"ext::sh -c " + canary,
		"http://" + canary + ".example/org/repo.git",
		"https://-" + canary + ".example/org/repo.git",
		" https://" + canary + ".example/org/repo.git",
		"C:/" + canary + "/repo",
	}
	for _, raw := range refused {
		for _, leg := range []string{"clone-per-agent", "shared-workspace"} {
			t.Run(leg+"/"+raw, func(t *testing.T) {
				if provision.CheckHostCloneURL(raw) == nil {
					t.Fatalf("fixture check: %q is not refused by CheckHostCloneURL", raw)
				}
				gc := &api.GitCloneConfig{URL: raw}
				spec := WorkspaceDispatchSpec{GitClone: gc}
				if leg == "shared-workspace" {
					spec = WorkspaceDispatchSpec{SharedWorkspaceClone: gc}
				}
				err := checkDispatchCloneURLs(spec)
				if !errors.Is(err, provision.ErrHostCloneURLRefused) {
					t.Fatalf("checkDispatchCloneURLs = %v, want ErrHostCloneURLRefused", err)
				}
				if strings.Contains(err.Error(), canary) {
					t.Errorf("the refusal error carries the clone URL: %q", err.Error())
				}
				// The callers (buildCreateRequestFor, buildStartEnv) wrap it
				// with their own name only.
				for _, caller := range []string{"DispatchAgentCreate", "DispatchAgentStart", "DispatchAgentRestart"} {
					if wrapped := fmt.Errorf("%s: %w", caller, err).Error(); strings.Contains(wrapped, canary) {
						t.Errorf("the caller-wrapped refusal error carries the clone URL: %q", wrapped)
					}
				}
			})
		}
	}
}
