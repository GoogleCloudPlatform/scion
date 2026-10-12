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
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// scrubbedClientEnv maps each environment variable constant this package
// declares or references that scrubHubEnv must clear to its value.
var scrubbedClientEnv = map[string]string{
	"EnvHubEndpoint":                      EnvHubEndpoint,
	"EnvHubURL":                           EnvHubURL,
	"EnvHubToken":                         EnvHubToken,
	"EnvAgentID":                          EnvAgentID,
	"EnvAgentMode":                        EnvAgentMode,
	"EnvTransportToken":                   EnvTransportToken,
	"transportauth.EnvTransportToken":     transportauth.EnvTransportToken,
	"transportauth.EnvTransportTokenFile": transportauth.EnvTransportTokenFile,
}

// unscrubbedClientEnv lists the environment variable constants this package
// declares or references that scrubHubEnv deliberately leaves alone, with
// the reason.
var unscrubbedClientEnv = map[string]string{
	"EnvLaunchID":                        "run id header value only; does not locate or authenticate to a Hub",
	"EnvHubOIDCAudience":                 "token request setting; used only once a Hub endpoint is set",
	"transportauth.EnvHubOIDCAudience":   "token request setting; used only once a Hub endpoint is set",
	"EnvTransportAudience":               "token request setting; used only once a Hub endpoint is set",
	"transportauth.EnvTransportAudience": "token request setting; used only once a Hub endpoint is set",
	"transportauth.EnvTransportMode":     "token request setting; used only once a Hub endpoint is set",
	"transportauth.EnvMetadataMode":      "token request setting; used only once a Hub endpoint is set",
	"EnvGitHubAppEnabled":                "GitHub token setting; unrelated to reaching a Hub",
	"EnvGitHubTokenExpiry":               "GitHub token setting; unrelated to reaching a Hub",
	"EnvGitHubTokenPath":                 "GitHub token setting; unrelated to reaching a Hub",
	"EnvUserGitHubToken":                 "GitHub token setting; unrelated to reaching a Hub",
}

// clientEnvConstantNames returns the names of every Env* constant declared in
// this package's non-test sources, plus every transportauth.Env* identifier
// those sources reference (as "transportauth.EnvX").
func clientEnvConstantNames(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing package sources: %v", err)
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.GenDecl:
				if n.Tok != token.CONST {
					return true
				}
				for _, spec := range n.Specs {
					for _, id := range spec.(*ast.ValueSpec).Names {
						if strings.HasPrefix(id.Name, "Env") {
							seen[id.Name] = true
						}
					}
				}
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "transportauth" && strings.HasPrefix(n.Sel.Name, "Env") {
					seen["transportauth."+n.Sel.Name] = true
				}
			}
			return true
		})
	}
	var names []string
	for name := range seen {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Every Env* constant declared in this package's non-test sources, and every
// transportauth.Env* identifier those sources reference, must be either
// cleared by the shared scrub list or listed, with a reason, as deliberately
// left alone. A new constant of either kind fails this test until it is
// classified. The test does not see environment reads through string
// literals, constants from other packages, or helpers in other packages
// that read the environment themselves.
func TestScrubHubEnv_CoversClientEnvKeys(t *testing.T) {
	names := clientEnvConstantNames(t)
	if len(names) == 0 {
		t.Fatal("found no Env* constants in the package sources")
	}
	shared := testutil.HubEnvKeys()
	for _, name := range names {
		value, scrubbed := scrubbedClientEnv[name]
		_, unscrubbed := unscrubbedClientEnv[name]
		switch {
		case scrubbed && unscrubbed:
			t.Errorf("%s is listed as both scrubbed and unscrubbed", name)
		case scrubbed:
			if !slices.Contains(shared, value) {
				t.Errorf("testutil.HubEnvKeys() is missing %s (%s)", value, name)
			}
		case !unscrubbed:
			t.Errorf("%s is not classified: add it to the testutil hubEnvKeys list "+
				"and scrubbedClientEnv, or to unscrubbedClientEnv with a reason", name)
		}
	}
	for name := range scrubbedClientEnv {
		if !slices.Contains(names, name) {
			t.Errorf("scrubbedClientEnv lists %s, which the package no longer declares or references", name)
		}
	}
	for name := range unscrubbedClientEnv {
		if !slices.Contains(names, name) {
			t.Errorf("unscrubbedClientEnv lists %s, which the package no longer declares or references", name)
		}
	}
}
