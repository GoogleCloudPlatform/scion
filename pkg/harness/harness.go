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
	switch harnessName {
	case "gemini":
		return &GeminiCLI{}
	default:
		if h := newFromEmbedFS(harnessName); h != nil {
			return h
		}
		return &Generic{}
	}
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

// EmbedOnlyHarnesses returns harnesses that still use compiled-in Go embeds
// for seeding (i.e., those not yet migrated to the harnesses/ directory).
func EmbedOnlyHarnesses() []api.Harness {
	return []api.Harness{
		&GeminiCLI{},
	}
}

// HarnessesFS returns the embedded harnesses/ filesystem.
func HarnessesFS() fs.FS {
	return harnessesEmbed.FS
}

// AllHarnessNames returns the complete list of harness names by combining
// directory-based harnesses (from the embedded harnesses/ FS) with
// embed-only harnesses.
func AllHarnessNames() []string {
	seen := make(map[string]bool)

	entries, _ := fs.ReadDir(harnessesEmbed.FS, ".")
	for _, e := range entries {
		if e.IsDir() {
			seen[e.Name()] = true
		}
	}

	for _, h := range EmbedOnlyHarnesses() {
		seen[h.Name()] = true
	}

	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
