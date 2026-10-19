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

package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unreachableHubURL returns a loopback URL nothing listens on, so a hub
// status run against it fails fast with "connection refused".
func unreachableHubURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	return url
}

// TestResolveCLIHub_ValueAndSourceAgree: in every combination of the hub
// sources (--hub as a URL or a nickname, SCION_HUB_ENDPOINT,
// SCION_HUB_URL or both, a directory link, a current hub), the endpoint and the
// source come from one resolver, so hub status never prints the endpoint of
// one source with the label of another. A directory project's settings
// hub.endpoint is present in every row and is never a source.
func TestResolveCLIHub_ValueAndSourceAgree(t *testing.T) {
	type envCase struct{ name, varName string }
	// "both" sets SCION_HUB_ENDPOINT and SCION_HUB_URL to different
	// values: SCION_HUB_ENDPOINT wins, value and label.
	envCases := []envCase{{"no env", ""}, {"SCION_HUB_ENDPOINT", "SCION_HUB_ENDPOINT"}, {"SCION_HUB_URL", "SCION_HUB_URL"}, {"both", "SCION_HUB_ENDPOINT"}}
	flagCases := []string{"no flag", "--hub URL", "--hub nickname"}

	for _, flag := range flagCases {
		for _, env := range envCases {
			for _, link := range []bool{false, true} {
				for _, current := range []bool{false, true} {
					name := fmt.Sprintf("%s/%s/link=%v/current=%v", flag, env.name, link, current)
					t.Run(name, func(t *testing.T) {
						setupContextTest(t)
						origFormat := outputFormat
						t.Cleanup(func() { outputFormat = origFormat })

						settingsURL := unreachableHubURL(t)
						flagURL := unreachableHubURL(t)
						envURL := unreachableHubURL(t)
						linkURL := unreachableHubURL(t)
						currentURL := unreachableHubURL(t)

						t.Setenv("SCION_HUB_ENDPOINT", "")
						t.Setenv("SCION_HUB_URL", "")
						if env.varName != "" {
							t.Setenv(env.varName, envURL)
						}
						if env.name == "both" {
							t.Setenv("SCION_HUB_URL", unreachableHubURL(t))
						}
						if current {
							saveTestContext(t, currentURL, "")
						}
						if link {
							dotScion := makeLinkedDirProject(t, settingsURL)
							saveLinkEntry(t, dotScion, linkURL, "linked")
						} else {
							makeDirProject(t, settingsURL)
						}
						switch flag {
						case "--hub URL":
							hubEndpoint = flagURL
						case "--hub nickname":
							require.NoError(t, config.UpdateCLIContext(func(c *config.CLIContext) error {
								_, _ = c.EnsureHub(flagURL, "flagged")
								return nil
							}))
							hubEndpoint = "flagged"
						}

						// Expected: --hub > environment > directory link >
						// current hub > none.
						var wantURL, wantSource, wantLabel string
						switch {
						case flag != "no flag":
							wantURL, wantSource, wantLabel = flagURL, useSourceFlag, "--hub flag"
						case env.varName != "":
							wantURL, wantSource, wantLabel = envURL, useSourceEnv, env.varName
						case link:
							wantURL, wantSource, wantLabel = linkURL, useSourceDirectory, "directory link"
						case current:
							wantURL, wantSource, wantLabel = currentURL, useSourceContext, "current context"
						default:
							wantSource, wantLabel = useSourceNone, "none"
						}
						wantInherited := wantSource == useSourceEnv || wantSource == useSourceContext

						status := findCommand(t, "hub", "status")
						require.NoError(t, resolveInvocationTarget(status, ModeHuman))

						got := resolveCLIHub()
						assert.Equal(t, cliHub{Endpoint: wantURL, Source: wantSource, Label: wantLabel}, got)
						ep, src := invocationHub()
						assert.Equal(t, wantURL, ep)
						assert.Equal(t, wantSource, src)
						assert.NotEqual(t, settingsURL, ep, "settings hub.endpoint must never be a source")

						target := describeUseTarget(&config.CLIContext{})
						assert.Equal(t, wantURL, target.HubEndpoint)
						assert.Equal(t, wantSource, target.HubSource)
						assert.Equal(t, wantLabel, target.HubLabel)

						scope := getHubEndpointScope(false, target)
						assert.Equal(t, hubEndpointScope{Endpoint: wantURL, Source: wantSource, Label: wantLabel, Inherited: wantInherited}, scope)

						// hub status itself: the endpoint it prints and the
						// source it reports belong together.
						outputFormat = "json"
						out := captureStdout(t, func() { require.NoError(t, runHubStatus(status, nil)) })
						var js map[string]interface{}
						require.NoError(t, json.Unmarshal([]byte(out), &js), out)
						assert.Equal(t, wantURL, js["endpoint"])
						assert.Equal(t, wantSource, js["endpointSource"])
						assert.Equal(t, wantInherited, js["endpointInherited"])
					})
				}
			}
		}
	}
}
