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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/require"
)

// namedPullConsumers pull a Hub resource with the given Name and no --to,
// and report the directory such a pull writes below.
var namedPullConsumers = []struct {
	name       string
	collection string
	id         string
	parentDir  func(t *testing.T) string
	pull       func(hubCtx *HubContext, name string) error
}{
	{
		name: "harness-config", collection: "harness-configs", id: "hc-id",
		parentDir: func(t *testing.T) string {
			g, err := config.GetGlobalDir()
			require.NoError(t, err)
			return filepath.Join(g, "harness-configs")
		},
		pull: func(hubCtx *HubContext, name string) error {
			return pullHarnessConfigFromHub(hubCtx, &hubclient.HarnessConfig{ID: "hc-id", Name: name, Harness: "claude"}, "")
		},
	},
	{
		name: "template", collection: "templates", id: "tpl-id",
		parentDir: func(t *testing.T) string {
			d, err := config.GetProjectTemplatesDir()
			require.NoError(t, err)
			return d
		},
		pull: func(hubCtx *HubContext, name string) error {
			match := &TemplateMatch{Name: name, Location: LocationHubGlobal, HubTemplate: &hubclient.Template{ID: "tpl-id", Name: name}}
			return pullTemplateFromHubMatch(hubCtx, match, "")
		},
	},
}

// treeEntries lists every path below dir, relative to it.
func treeEntries(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, err)
	sort.Strings(out)
	return out
}

// pullNameHome makes an isolated HOME (also the working directory, so no
// project is found) and returns it.
func pullNameHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, config.GlobalDir), 0755))
	return home
}

func TestPullRefusesNonLocalHubName(t *testing.T) {
	for _, consumer := range namedPullConsumers {
		t.Run(consumer.name, func(t *testing.T) {
			for _, name := range []string{"../x", "../../x", "a/b", "..", "ABSOLUTE"} {
				t.Run(name, func(t *testing.T) {
					home := pullNameHome(t)
					if name == "ABSOLUTE" {
						name = filepath.Join(home, "outside")
					}
					before := treeEntries(t, home)
					server := newPullDestServer(t, consumer.collection, consumer.id, []string{"ok.txt"})

					err := consumer.pull(newPullHubCtx(t, server), name)
					require.Error(t, err)
					require.True(t, errors.Is(err, errPullNameNotLocal), "err = %v", err)
					require.NotContains(t, err.Error(), name)
					require.Equal(t, before, treeEntries(t, home), "nothing may be created")
				})
			}
		})
	}
}

func TestPullRefusesLinkedDestination(t *testing.T) {
	for _, consumer := range namedPullConsumers {
		t.Run(consumer.name, func(t *testing.T) {
			home := pullNameHome(t)
			outside := filepath.Join(home, "outside")
			require.NoError(t, os.MkdirAll(outside, 0755))
			parent := consumer.parentDir(t)
			require.NoError(t, os.MkdirAll(parent, 0755))
			if err := os.Symlink(outside, filepath.Join(parent, "linked")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			server := newPullDestServer(t, consumer.collection, consumer.id, []string{"ok.txt"})

			require.Error(t, consumer.pull(newPullHubCtx(t, server), "linked"))
			entries, err := os.ReadDir(outside)
			require.NoError(t, err)
			require.Empty(t, entries, "nothing may be written through a linked destination")
		})
	}
}

func TestPullSlugNameWritesBelowParent(t *testing.T) {
	for _, consumer := range namedPullConsumers {
		t.Run(consumer.name, func(t *testing.T) {
			pullNameHome(t)
			server := newPullDestServer(t, consumer.collection, consumer.id, []string{"ok.txt"})

			require.NoError(t, consumer.pull(newPullHubCtx(t, server), "res-one"))
			data, err := os.ReadFile(filepath.Join(consumer.parentDir(t), "res-one", "ok.txt"))
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(string(data), "content-"))
		})
	}
}
