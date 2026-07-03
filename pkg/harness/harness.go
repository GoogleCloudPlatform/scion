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

package harness

import (
	"io/fs"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"

	harnessesEmbed "github.com/GoogleCloudPlatform/scion/harnesses"
)

func New(harnessName string) api.Harness {
	if h := newFromEmbedFS(harnessName); h != nil {
		return h
	}
	return &Generic{}
}

func newFromEmbedFS(name string) api.Harness {
	data, err := fs.ReadFile(harnessesEmbed.FS, name+"/config.yaml")
	if err != nil {
		return nil
	}
	entry, err := config.ParseHarnessConfigYAML(data)
	if err != nil {
		return nil
	}
	entry.Harness = name
	return NewDeclarativeGenericHarness(entry)
}

// EmbedOnlyHarnesses returns harnesses that use compiled-in Go embeds for
// seeding. All harnesses have migrated to the harnesses/ directory, so this
// returns an empty slice.
func EmbedOnlyHarnesses() []api.Harness {
	return nil
}

// HarnessesFS returns the embedded harnesses/ filesystem.
func HarnessesFS() fs.FS {
	return harnessesEmbed.FS
}

// AllHarnessNames returns the list of harness names from the embedded
// harnesses/ filesystem.
func AllHarnessNames() []string {
	entries, _ := fs.ReadDir(harnessesEmbed.FS, ".")
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}
